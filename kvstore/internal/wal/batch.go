package wal

import (
	"encoding/binary"
	"hash/crc32"
	"log"
	"sync"
	"time"
)

const (
	// batchSize — максимальное количество записей в одном batch.
	//
	// При 12 workers × ~100K SET/sec = ~1.2M entries/sec.
	// Batch 256 → ~4700 flush/sec (вместо 1.2M mutex locks).
	//
	// Компромисс:
	//   Больше batch → выше throughput, но выше latency (ждём заполнения)
	//   Меньше batch → ниже latency, но больше mutex locks
	//   256 — sweet spot для in-memory KV store.
	batchSize = 256

	// flushInterval — максимальное время ожидания перед flush.
	//
	// Даже если batch не заполнен — flush через 1ms.
	// Это гарантирует bounded latency: запись попадёт на диск ≤ 1ms.
	//
	// Без таймера: если RPS = 10 ops/sec, batch из 256 ждал бы 25 секунд!
	flushInterval = 1 * time.Millisecond

	// channelSize — размер буферизованного канала.
	//
	// Канал = очередь между workers и flusher.
	// 8192 записей = ~1MB памяти (при средних 128B/entry).
	// При burst'ах workers не блокируются, пока канал не заполнен.
	//
	// Если канал полон → worker блокируется (backpressure).
	// Это ХОРОШО: лучше замедлить worker, чем потерять данные.
	channelSize = 8192
)

// BatchWAL — обёртка над WAL с batch-записью.
//
// Архитектура:
//
//	  ┌─── Worker 0 ───┐
//	  │  ch <- entry   │ ← lock-free send (~50ns)
//	  └────────────────┘
//	  ┌─── Worker 1 ───┐
//	  │  ch <- entry   │
//	  └────────────────┘
//	         ...
//	  ┌─── Worker N ───┐
//	  │  ch <- entry   │
//	  └────────────────┘
//	          │
//	          ▼
//	┌──── ch (буфер 8192) ────┐
//	└─────────────────────────┘
//	          │
//	          ▼
//	┌──── Flusher goroutine ──┐
//	│  batch = read до 256    │
//	│  encodeBatch(batch)     │ ← pre-allocated buffer
//	│  wal.WriteBatch(buf)    │ ← 1 mutex lock
//	│  timer reset            │
//	└─────────────────────────┘
//
// Вместо N mutex locks → 1 mutex lock на batch.
// При batch=256: contention снижается в 256 раз.
//
// Durability: fire-and-forget (аналог Redis AOF everysec).
// Write() возвращает управление сразу после отправки в канал.
// Данные попадают на диск асинхронно. Окно потери ≤100ms при crash.
// Это осознанный trade-off: throughput > strict durability.
// См. WAL doc для деталей.
type BatchWAL struct {
	wal *WAL           // нижележащий WAL (запись на диск)
	ch  chan Entry     // канал: workers → flusher
	wg  sync.WaitGroup // ожидание завершения flusher

	// Pre-allocated буфер для кодирования batch.
	// Переиспользуется между flush'ами (zero-alloc на горячем пути).
	// Размер: 256 entries × ~128B = ~32KB.
	encodeBuf []byte
}

// NewBatchWAL создаёт BatchWAL поверх существующего WAL.
//
// Запускает фоновую goroutine (flusher), которая:
//   - Читает entries из канала
//   - Накапливает batch
//   - Пишет batch одним вызовом WriteBatch
func NewBatchWAL(w *WAL) *BatchWAL {
	bw := &BatchWAL{
		wal:       w,
		ch:        make(chan Entry, channelSize),
		encodeBuf: make([]byte, 0, 64*1024), // 64KB начальный буфер
	}

	bw.wg.Add(1)
	go bw.flusher()

	return bw
}

// Write отправляет entry в канал (вызывается workers).
//
// Это ГОРЯЧИЙ ПУТЬ. Стоимость: ~50ns (channel send).
// Сравни с WAL.Write: ~200ns (mutex + encode + CRC + bufio).
//
// Блокируется ТОЛЬКО если канал полон (backpressure).
// При channelSize=8192 это происходит при burst > 8K entries.
//
// ВАЖНО: fire-and-forget. Ошибки записи на диск логируются,
// но НЕ возвращаются вызывающему. Это осознанный trade-off
// (аналог Redis AOF): throughput важнее strict durability.
func (bw *BatchWAL) Write(entry Entry) {
	bw.ch <- entry
}

// Close останавливает flusher и закрывает WAL.
//
// Порядок:
//  1. Закрываем канал → flusher видит close и дочитывает остатки
//  2. wg.Wait() → ждём пока flusher допишет последний batch
//  3. WAL.Close() → flush bufio + close file
func (bw *BatchWAL) Close() error {
	close(bw.ch)
	bw.wg.Wait()
	return bw.wal.Close()
}

// Failed возвращает залатченную фатальную ошибку персистентности нижележащего
// WAL (nil если здоров). Write-путь сервера обязан отклонять мутации, когда
// Failed()!=nil — иначе клиент получает OK на запись, которая уже потеряна.
func (bw *BatchWAL) Failed() error {
	return bw.wal.Failed()
}

// RawWAL возвращает нижележащий WAL.
//
// Используется для операций, которые BatchWAL НЕ оборачивает:
//   - Syncer: вызывает WAL.Sync() (fsync на диск)
//   - COMPACT: вызывает WAL.Rotate() (переключение файлов)
//   - Recovery: ReadEntries читает файл напрямую
//
// BatchWAL оборачивает только Write (горячий путь).
// Sync/Rotate — холодный путь, им батчинг не нужен.
func (bw *BatchWAL) RawWAL() *WAL {
	return bw.wal
}

// flusher — фоновая goroutine, читающая entries и пишущая батчами.
//
// Два триггера для flush:
//  1. Набралось batchSize (256) записей → flush немедленно
//  2. Прошло flushInterval (1ms) → flush то что есть
//
// Триггер 1 — для высокой нагрузки (throughput).
// Триггер 2 — для низкой нагрузки (latency).
func (bw *BatchWAL) flusher() {
	defer bw.wg.Done()

	// batch — слайс для накопления entries.
	// Переиспользуется (capacity сохраняется после [:0]).
	batch := make([]Entry, 0, batchSize)

	// Timer для flush по таймауту.
	// При низком RPS (10 ops/sec) без таймера batch из 256
	// ждал бы 25 секунд — неприемлемо для durability.
	timer := time.NewTimer(flushInterval)
	defer timer.Stop()

	for {
		// ─── Фаза 1: ждём первый entry ───
		//
		// select блокируется пока не придёт entry или таймер.
		// Это НЕ busy-wait: goroutine паркуется и не тратит CPU.
		select {
		case entry, ok := <-bw.ch:
			if !ok {
				// Канал закрыт → flush остатки и выход
				if len(batch) > 0 {
					bw.flushBatch(batch)
				}
				return
			}
			batch = append(batch, entry)

		case <-timer.C:
			// Таймер сработал, но batch пуст → reset и ждём дальше
			if len(batch) == 0 {
				timer.Reset(flushInterval)
				continue
			}
			// Есть записи → flush по таймауту
			bw.flushBatch(batch)
			batch = batch[:0] // reuse capacity
			timer.Reset(flushInterval)
			continue
		}

		// ─── Фаза 2: жадное чтение (drain) ───
		//
		// Первый entry получен. Теперь читаем ещё столько,
		// сколько ЕСТЬ в канале, до batchSize.
		//
		// Ключевое слово: default в select.
		// Если канал пуст → default срабатывает мгновенно → идём flush.
		// Если канал не пуст → читаем ещё один entry.
		//
		// Это «жадное» чтение: забираем всё что накопилось.
	drain:
		for len(batch) < batchSize {
			select {
			case entry, ok := <-bw.ch:
				if !ok {
					// Канал закрыт в процессе drain
					bw.flushBatch(batch)
					return
				}
				batch = append(batch, entry)
			default:
				// Канал пуст → всё забрали, идём flush
				break drain
			}
		}

		// ─── Фаза 3: flush ───
		bw.flushBatch(batch)
		batch = batch[:0] // reuse capacity (batch[:0] НЕ освобождает память)
		timer.Reset(flushInterval)
	}
}

// flushBatch кодирует batch entries в один буфер и пишет за 1 вызов WAL.
//
// Формат каждой записи в буфере (тот же что WAL.Write):
//
//	[CRC32 4B][PayloadLen 4B][LSN 8B][Op 1B][KeyLen 4B][Key][Value]
//
// Весь batch кодируется в bw.encodeBuf (pre-allocated, zero-alloc).
func (bw *BatchWAL) flushBatch(batch []Entry) {
	// Сбрасываем буфер (переиспользуем underlying array)
	bw.encodeBuf = bw.encodeBuf[:0]

	// Резервируем LSN на весь batch одним atomic Add. Присваиваем по возрастанию
	// startLSN, startLSN+1, ... в порядке кодирования (== порядок в файле).
	// flusher — единственный писатель, поэтому пачка непрерывна в файле.
	startLSN := bw.wal.reserveLSN(uint64(len(batch)))

	for i := range batch {
		e := &batch[i] // pointer, не копия (avoid copy overhead)

		// Вычисляем размер payload: [LSN 8B][Op 1B][KeyLen 4B][Key][Value]
		payloadSize := 8 + 1 + 4 + len(e.Key) + len(e.Value)

		// Запоминаем позицию начала header (для записи CRC и длины)
		headerStart := len(bw.encodeBuf)

		// Расширяем буфер на header (8B) + payload
		needed := 8 + payloadSize
		bw.encodeBuf = grow(bw.encodeBuf, needed)

		// Заполняем payload прямо в буфер (после header)
		payloadStart := headerStart + 8
		off := payloadStart

		binary.LittleEndian.PutUint64(bw.encodeBuf[off:], startLSN+uint64(i))
		off += 8

		bw.encodeBuf[off] = e.Op
		off++

		binary.LittleEndian.PutUint32(bw.encodeBuf[off:], uint32(len(e.Key)))
		off += 4

		copy(bw.encodeBuf[off:], e.Key)
		off += len(e.Key)

		if len(e.Value) > 0 {
			copy(bw.encodeBuf[off:], e.Value)
		}

		// CRC32 считаем по payload
		payload := bw.encodeBuf[payloadStart : headerStart+8+payloadSize]
		checksum := crc32.ChecksumIEEE(payload)

		// Записываем header: [CRC32][PayloadLen]
		binary.LittleEndian.PutUint32(bw.encodeBuf[headerStart:], checksum)
		binary.LittleEndian.PutUint32(bw.encodeBuf[headerStart+4:], uint32(payloadSize))
	}

	// Одна запись в WAL — один mutex lock на весь batch!
	// WriteBatch латчит фатальную ошибку внутри (bw.wal.Failed() станет !=nil),
	// после чего write-путь сервера начнёт отклонять новые мутации (fail-stop).
	// Этот батч уже потерян — окно fire-and-forget закрыть нельзя, но следующие
	// записи клиенту больше не подтверждаются как durable.
	if err := bw.wal.WriteBatch(bw.encodeBuf); err != nil {
		log.Printf("WAL batch write failed (%d entries lost) — engaging durability fail-stop, writes will be rejected: %v", len(batch), err)
	}
}

// grow расширяет buf на n байт, возвращая новый slice.
// Если capacity хватает — zero-alloc (просто сдвигаем len).
func grow(buf []byte, n int) []byte {
	l := len(buf)
	if cap(buf) >= l+n {
		return buf[:l+n]
	}
	// Capacity не хватает — grow x2
	newCap := cap(buf) * 2
	if newCap < l+n {
		newCap = l + n
	}
	newBuf := make([]byte, l+n, newCap)
	copy(newBuf, buf)
	return newBuf
}
