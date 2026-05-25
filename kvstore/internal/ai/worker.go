package ai

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Worker — фоновый процессор AI-задач.
//
// Зачем он нужен?
// Embedding через Ollama занимает ~10-50ms. Если делать синхронно
// в обработчике команд — клиент будет ждать 50ms на каждый INGEST.
// При 1000 документов — 50 секунд ожидания!
//
// Worker решает это: клиент получает "QUEUED" мгновенно (~μs),
// embedding вычисляется в фоне. По готовности → PubSub оповещает.
//
// Это тот же паттерн, что wal.Syncer — фоновая горутина,
// которая делает тяжёлую работу, не блокируя основной поток.
type Worker struct {
	client *Client
	tasks  chan Task // буферизованный канал задач (как sub.ch в PubSub!)

	// Callback-мостики к основному движку.
	// Тот же паттерн, что в compute.Engine:
	//   StoreGet, StoreSet, Publish — функции, а не прямые зависимости.
	// Это позволяет пакету ai/ не знать о store/ и pubsub/.
	VecStoreAdd func(key string, vec []float32) error
	KVStoreSet  func(key string, value []byte)
	Publish     func(channel, message string)

	done sync.WaitGroup // ожидание завершения горутин
	stop chan struct{}  // сигнал остановки (как sub.done в PubSub!)
}

// Task — одна задача на embedding.
type Task struct {
	Key  string // "doc:cats"
	Text string // "Кошки спят 16 часов в день"
}

// NewWorker создаёт фоновый AI-воркер.
//
// bufferSize — размер буфера канала задач.
// Аналогия: maxBuffer=256 в PubSub.
// Если буфер заполнен — Submit() вернёт ошибку (back-pressure),
// как disconnectSlow в PubSub отключает медленных подписчиков.
func NewWorker(client *Client, bufferSize int) *Worker {
	return &Worker{
		client: client,
		tasks:  make(chan Task, bufferSize),
		stop:   make(chan struct{}),
	}
}

// Start запускает горутины-воркеры.
//
// concurrency — сколько горутин обрабатывают задачи параллельно.
// Для Ollama на одной машине 2-4 достаточно:
// Ollama сама использует все CPU/GPU для одного запроса,
// больше параллельных запросов только создадут contention.
//
// Сравни с writePump в PubSub — там 1 горутина на подписчика.
// Здесь N горутин на все задачи (fan-out worker pool).
func (w *Worker) Start(concurrency int) {
	for i := 0; i < concurrency; i++ {
		w.done.Add(1)
		go w.loop(i)
	}
	log.Printf("[ai] Worker started: %d goroutines, buffer=%d", concurrency, cap(w.tasks))
}

// Submit отправляет задачу на embedding.
//
// Non-blocking: если буфер полон — возвращаем ошибку.
// Тот же подход, что в PubSub.Publish:
//
//	select { case sub.ch <- msg: ... default: disconnectSlow(sub) }
//
// Клиент получает либо "QUEUED", либо "ERR queue full".
func (w *Worker) Submit(key, text string) error {
	select {
	case w.tasks <- Task{Key: key, Text: text}:
		return nil
	default:
		return fmt.Errorf("ai worker queue full (%d/%d)", len(w.tasks), cap(w.tasks))
	}
}

// Stop останавливает воркер (graceful shutdown).
//
// 1. Закрываем канал stop → горутины выходят из select
// 2. done.Wait() — ждём завершения всех горутин
//
// Тот же паттерн, что syncer.Stop() в WAL.
func (w *Worker) Stop() {
	close(w.stop)
	w.done.Wait()
	log.Printf("[ai] Worker stopped")
}

// loop — рабочий цикл одной горутины.
//
// Структура select идентична writePump в PubSub:
//
//	case task := <-w.tasks  → обработать задачу
//	case <-w.stop           → выйти
func (w *Worker) loop(id int) {
	defer w.done.Done()

	for {
		select {
		case task := <-w.tasks:
			w.processTask(id, task)
		case <-w.stop:
			return
		}
	}
}

// processTask обрабатывает одну задачу.
//
// Это сердце воркера. Здесь происходит:
//  1. Текст → Ollama → embedding ([]float32)
//  2. Embedding → VectorStore (HNSW-граф)
//  3. Текст → KV Store (для RAG: потом достанем оригинал)
//  4. PubSub → уведомление "ai:indexed"
//
// Если ошибка → PubSub → уведомление "ai:errors"
func (w *Worker) processTask(workerID int, task Task) {
	start := time.Now()

	// 1. Генерируем embedding через Ollama (с 1 retry при транзиентных ошибках).
	// Ollama может вернуть 503 при перегрузке — один retry с паузой 500ms
	// достаточен для восстановления. Без retry задача терялась навсегда.
	var embedding []float32
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		embedding, err = w.client.Embed(ctx, task.Text)
		cancel()
		if err == nil {
			break
		}
		if attempt == 0 {
			log.Printf("[ai] Worker %d: embed retry for key '%s': %v", workerID, task.Key, err)
			time.Sleep(500 * time.Millisecond)
		}
	}
	if err != nil {
		log.Printf("[ai] Worker %d: embed failed for key '%s': %v", workerID, task.Key, err)
		if w.Publish != nil {
			w.Publish("ai:errors", fmt.Sprintf("embed_failed:%s:%v", task.Key, err))
		}
		return
	}

	// 2. Сохраняем вектор в HNSW
	if w.VecStoreAdd != nil {
		if err := w.VecStoreAdd(task.Key, embedding); err != nil {
			log.Printf("[ai] Worker %d: vecstore add error for key '%s': %v", workerID, task.Key, err)
			if w.Publish != nil {
				w.Publish("ai:errors", fmt.Sprintf("vecstore_failed:%s:%v", task.Key, err))
			}
			return
		}
	}

	// 3. Сохраняем оригинальный текст в KV Store
	//    Зачем? Для RAG: когда AI.ASK найдёт этот вектор,
	//    нужно достать ТЕКСТ, чтобы подставить в промпт LLM.
	if w.KVStoreSet != nil {
		w.KVStoreSet(task.Key, []byte(task.Text))
	}

	elapsed := time.Since(start)

	// 4. Оповещаем через PubSub: "документ проиндексирован!"
	if w.Publish != nil {
		w.Publish("ai:indexed", task.Key)
		w.Publish("ai:metrics", fmt.Sprintf("embed_time:%s:%.1fms", task.Key, float64(elapsed.Microseconds())/1000.0))
	}

	log.Printf("[ai] Worker %d: indexed '%s' (dim=%d, %.1fms)",
		workerID, task.Key, len(embedding), float64(elapsed.Microseconds())/1000.0)
}
