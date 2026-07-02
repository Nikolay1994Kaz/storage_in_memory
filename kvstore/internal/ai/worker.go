package ai

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"kvstore/kvstore/internal/monitoring"
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

	// embedCtx / cancelEmbed — общий контекст in-flight Embed. Отменяется на Stop
	// по истечении DrainTimeout: без этого залипший запрос к Ollama держал бы
	// shutdown до ~30с (15с × 2 попытки). Хранение контекста в структуре здесь
	// оправдано — его время жизни совпадает со временем жизни воркера.
	embedCtx    context.Context
	cancelEmbed context.CancelFunc

	// DrainTimeout — бюджет на дренаж очереди при Stop. Оставшиеся QUEUED-задачи
	// дообрабатываются в его пределах (WAL у самой очереди нет — иначе они тихо
	// теряются, хотя клиенту вернули "QUEUED"). По истечении бюджета embedCtx
	// отменяется, in-flight Embed прерывается, остаток очереди считаем потерянным.
	DrainTimeout time.Duration
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
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		client:       client,
		tasks:        make(chan Task, bufferSize),
		stop:         make(chan struct{}),
		embedCtx:     ctx,
		cancelEmbed:  cancel,
		DrainTimeout: 5 * time.Second,
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
		monitoring.AiQueueLen.Set(uint64(len(w.tasks)))
		return nil
	default:
		return fmt.Errorf("ai worker queue full (%d/%d)", len(w.tasks), cap(w.tasks))
	}
}

// Stop останавливает воркер (graceful shutdown).
//
//  1. Закрываем канал stop → горутины дренируют остаток очереди и выходят.
//  2. Таймер на DrainTimeout: если дренаж/in-flight Embed не уложился в бюджет —
//     отменяем embedCtx, залипший запрос к Ollama возвращается сразу (иначе
//     shutdown ждал бы до ~30с на одной застрявшей задаче).
//  3. done.Wait() — ждём завершения всех горутин.
//
// Тот же паттерн, что syncer.Stop() в WAL.
func (w *Worker) Stop() {
	close(w.stop)
	timer := time.AfterFunc(w.DrainTimeout, w.cancelEmbed)
	w.done.Wait()
	timer.Stop()
	w.cancelEmbed() // освобождаем контекст (идемпотентно), если вышли раньше бюджета
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
			monitoring.AiQueueLen.Set(uint64(len(w.tasks)))
			w.processTaskSafe(id, task)
		case <-w.stop:
			w.drain(id)
			return
		}
	}
}

// drain дообрабатывает задачи, оставшиеся в буфере на момент Stop. Забирает лишь
// то, что уже в очереди (неблокирующий default), поэтому N горутин совместно
// разгребают буфер без гонок. Прерывается, как только исчерпан бюджет DrainTimeout
// (embedCtx отменён) — тянуть остаток бессмысленно, каждый Embed упал бы сразу.
func (w *Worker) drain(id int) {
	for {
		select {
		case task := <-w.tasks:
			if w.embedCtx.Err() != nil {
				return // бюджет дренажа исчерпан — остаток очереди теряем
			}
			w.processTaskSafe(id, task)
		default:
			return
		}
	}
}

// processTaskSafe оборачивает processTask recover'ом. Паника в одной задаче
// (например, dim-mismatch в HNSW или nil в колбэке) не должна убивать горутину —
// и тем более весь процесс: падает только эта задача, воркер живёт дальше.
func (w *Worker) processTaskSafe(workerID int, task Task) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ai] Worker %d: recovered panic on key '%s': %v", workerID, task.Key, r)
			if w.Publish != nil {
				w.Publish("ai:errors", fmt.Sprintf("panic:%s", task.Key))
			}
		}
	}()
	w.processTask(workerID, task)
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
		// Контекст производный от w.embedCtx: на Stop он отменяется по истечении
		// DrainTimeout, и залипший запрос к Ollama прерывается вместо ожидания 15с.
		ctx, cancel := context.WithTimeout(w.embedCtx, 15*time.Second)
		embedding, err = w.client.Embed(ctx, task.Text)
		cancel()
		if err == nil {
			break
		}
		// Воркер останавливается — не ретраим и не спим, чтобы не тормозить shutdown.
		if w.embedCtx.Err() != nil {
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
