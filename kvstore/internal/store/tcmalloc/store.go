package tcmalloc

import (
	"encoding/binary"
	"hash/fnv"
	"sync"
)

const numStoreShards = 256

// indexShard — шард индекса (как в ArenaStore).
// map[uint64]Handle — ноль указателей, GC не сканирует.
type indexShard struct {
	mu    sync.RWMutex
	index map[uint64]Handle // hash(key) → Handle
}

// TCMallocStore — Key-Value хранилище на базе TCMalloc-style аллокатора.
//
// Тот же KV-интерфейс что ArenaStore, но с иерархической моделью памяти:
//
//	Level 1 (mcache):   lock-free, per-worker        → 99% аллокаций
//	Level 2 (mcentral): per-size-class mutex          → ~1% аллокаций
//	Level 3 (mheap):    global mutex, chunk allocation → ~0.01%
//
// Данные хранятся в том же формате что ArenaStore:
//
//	[4B keyLen][key bytes][4B valLen][val bytes]
//
// Разница: вместо append в непрерывный буфер — аллокация из span
// через mcache. Это устраняет contention при высокой конкурентности.
type TCMallocStore struct {
	heap     *MHeap
	centrals [numSizeClasses]*MCentral
	caches   []*MCache // по одному на worker

	// Шардированный индекс (как в ArenaStore).
	// hash(key) → Handle.
	// Handle = uint64 → map[uint64]uint64 → 0 указателей → GC не сканирует.
	shards [numStoreShards]indexShard
}

// NewTCMallocStore создаёт хранилище.
//
// numWorkers — количество epoll workers (обычно = GOMAXPROCS или 12).
// Каждый worker получает свой MCache.
func NewTCMallocStore(numWorkers int) *TCMallocStore {
	heap := NewMHeap()

	var centrals [numSizeClasses]*MCentral
	for i := 0; i < numSizeClasses; i++ {
		centrals[i] = NewMCentral(i, heap)
	}

	caches := make([]*MCache, numWorkers)
	for i := 0; i < numWorkers; i++ {
		caches[i] = NewMCache(centrals)
	}

	s := &TCMallocStore{
		heap:     heap,
		centrals: centrals,
		caches:   caches,
	}

	for i := 0; i < numStoreShards; i++ {
		s.shards[i].index = make(map[uint64]Handle)
	}

	return s
}

// ─── Кодирование данных ─────────────────────────────────────
//
// Формат записи в блоке (тот же что ArenaStore):
//   [4 byte keyLen][key bytes][4 byte valLen][val bytes]
//
// Пример: SET "abc" "hello"
//   [03 00 00 00][61 62 63][05 00 00 00][68 65 6C 6C 6F]
//    keyLen=3     "abc"     valLen=5     "hello"

func encodeSize(key string, value []byte) int {
	return 4 + len(key) + 4 + len(value)
}

func encodeInto(buf []byte, key string, value []byte) {
	offset := 0

	// Key length
	binary.LittleEndian.PutUint32(buf[offset:], uint32(len(key)))
	offset += 4

	// Key
	copy(buf[offset:], key)
	offset += len(key)

	// Value length
	binary.LittleEndian.PutUint32(buf[offset:], uint32(len(value)))
	offset += 4

	// Value
	copy(buf[offset:], value)
}

func decodeFrom(buf []byte) (string, []byte) {
	offset := 0

	keyLen := binary.LittleEndian.Uint32(buf[offset:])
	offset += 4

	key := string(buf[offset : offset+int(keyLen)])
	offset += int(keyLen)

	valLen := binary.LittleEndian.Uint32(buf[offset:])
	offset += 4

	value := make([]byte, valLen)
	copy(value, buf[offset:offset+int(valLen)])

	return key, value
}

// ─── Store Operations ───────────────────────────────────────

func hashStoreKey(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64()
}

// Set записывает ключ-значение.
//
// workerID определяет какой MCache использовать (lock-free путь).
// Шард индекса определяется хешем ключа (RWMutex на запись).
//
// Горячий путь:
//  1. mcache.Alloc() → 0 locks (span bump pointer)
//  2. encodeInto(buf) → запись в pre-allocated блок
//  3. shard.mu.Lock() → запись handle в index
//
// Итого: 1 mutex lock (на index shard), 0 locks на аллокацию.
// ArenaStore: 1 mutex lock (на arena shard) включая аллокацию.
//
// Разница: при hot spot (все ключи в одном shard) ArenaStore
// блокирует и аллокацию и запись в index. TCMallocStore
// блокирует только index, аллокация lock-free.
func (s *TCMallocStore) Set(workerID int, key string, value []byte) {
	size := encodeSize(key, value)
	cache := s.caches[workerID]

	// 1. Аллоцируем блок (LOCK-FREE через mcache!)
	buf, handle := cache.Alloc(size)

	// 2. Записываем данные в блок
	encodeInto(buf, key, value)

	// 3. Сохраняем handle в шардированный индекс
	hash := hashStoreKey(key)
	sh := &s.shards[hash%numStoreShards]
	sh.mu.Lock()
	sh.index[hash] = handle
	sh.mu.Unlock()
}

// Get читает значение по ключу.
//
// Не использует workerID — чтение через heap.Resolve (lock-free).
//
// Путь:
//  1. hash → shard → RLock → index lookup → handle → RUnlock
//  2. heap.Resolve(handle) → buf (lock-free: chunks append-only)
//  3. decodeFrom(buf) → (key, value)
func (s *TCMallocStore) Get(key string) ([]byte, bool) {
	hash := hashStoreKey(key)
	sh := &s.shards[hash%numStoreShards]

	sh.mu.RLock()
	handle, ok := sh.index[hash]
	sh.mu.RUnlock()

	if !ok {
		return nil, false
	}

	// Resolve — lock-free чтение из span
	buf := s.heap.Resolve(handle)
	storedKey, value := decodeFrom(buf)

	// Проверка коллизии хешей
	if storedKey != key {
		return nil, false
	}
	return value, true
}

// Del удаляет ключ.
//
// workerID — чтобы вызвать Free через правильный mcache.
//
// Путь:
//  1. hash → shard → Lock → удалить из index → Unlock
//  2. mcache.Free(handle) → span.Free(objIndex) (возврат блока)
func (s *TCMallocStore) Del(workerID int, key string) bool {
	hash := hashStoreKey(key)
	sh := &s.shards[hash%numStoreShards]

	sh.mu.Lock()
	handle, ok := sh.index[hash]
	if ok {
		delete(sh.index, hash)
	}
	sh.mu.Unlock()

	if !ok {
		return false
	}

	// Освобождаем блок обратно в аллокатор
	s.caches[workerID].Free(s.heap, handle)
	return true
}

// Len возвращает количество ключей.
func (s *TCMallocStore) Len() int {
	total := 0
	for i := 0; i < numStoreShards; i++ {
		s.shards[i].mu.RLock()
		total += len(s.shards[i].index)
		s.shards[i].mu.RUnlock()
	}
	return total
}

// ForEach итерирует по всем ключам.
func (s *TCMallocStore) ForEach(fn func(key string, value []byte)) {
	for i := 0; i < numStoreShards; i++ {
		sh := &s.shards[i]
		sh.mu.RLock()
		for _, handle := range sh.index {
			buf := s.heap.Resolve(handle)
			key, value := decodeFrom(buf)
			fn(key, value)
		}
		sh.mu.RUnlock()
	}
}

// Stats возвращает статистику аллокатора.
func (s *TCMallocStore) Stats() map[string]interface{} {
	numChunks, totalBytes, usedBytes, numSpans := s.heap.Stats()

	totalAllocs := uint64(0)
	totalRefills := uint64(0)
	for _, c := range s.caches {
		totalAllocs += c.AllocCount
		totalRefills += c.RefillCount
	}

	return map[string]interface{}{
		"chunks":        numChunks,
		"total_bytes":   totalBytes,
		"used_bytes":    usedBytes,
		"spans":         numSpans,
		"total_allocs":  totalAllocs,
		"total_refills": totalRefills,
		"refill_ratio":  float64(totalRefills) / float64(max(totalAllocs, 1)),
	}
}

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
