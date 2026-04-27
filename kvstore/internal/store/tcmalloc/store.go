package tcmalloc

import (
	"encoding/binary"
	"hash/maphash"
	"sync"
	"sync/atomic"
)

const numStoreShards = 256

// ─── Hash Function ──────────────────────────────────────────
//
// Замена FNV → maphash (AES-NI аппаратное ускорение).
//
// Старый код:
//
//	h := fnv.New64a()       // interface alloc (~50ns, heap escape)
//	h.Write([]byte(key))    // string→[]byte conversion
//	return h.Sum64()        // программный хеш (~30ns)
//	ИТОГО: ~80ns + возможная heap аллокация
//
// Новый код:
//
//	maphash.String(seed, key)  // inline, AES-NI, zero-alloc (~1-2ns)
//
// Прирост: ~40-80x на хешировании.
var hashSeed = maphash.MakeSeed()

func hashStoreKey(key string) uint64 {
	return maphash.String(hashSeed, key)
}

// ─── Index Shard ────────────────────────────────────────────
//
// Гибридная модель конкурентности:
//
//	GET:  lock-free через atomic.Pointer → table.Get() (atomic Load)
//	SET:  mu.Lock → table.Put() → auto grow/rebuild → mu.Unlock
//	DEL:  mu.Lock → table.Delete() → mu.Unlock
//
// Readers (GET) НИКОГДА не блокируются writers (SET/DEL).
// Writers блокируют только ДРУГИХ writers в ТОМ ЖЕ шарде.
//
// Сравнение с прежней моделью (sync.RWMutex + map):
//
//	Прежде: SET блокирует GET (RWMutex writer → readerCount<0 → RLock blocked)
//	Теперь: SET НЕ блокирует GET (readers идут через atomic.Pointer)
//
// ─── Cache Line Padding ─────────────────────────────────────
//
// Без padding: sizeof(indexShard) ≈ 16 байт → 4 шарда в cache line.
// Worker 0 пишет в shard[0] → CPU инвалидирует cache line →
// Worker 1 получает cache miss на shard[1] (false sharing).
//
// С padding: каждый шард = своя cache line → нет false sharing.
type indexShard struct {
	_     [64]byte                  // ── padding: начало cache line
	mu    sync.Mutex                // защищает ТОЛЬКО writers (Set, Del, Resize)
	table atomic.Pointer[HashTable] // lock-free для readers (Get)
	_     [64]byte                  // ── padding: конец cache line
}

// initShard создаёт начальную таблицу для шарда.
func (sh *indexShard) initShard() {
	t := NewHashTable(defaultInitialCap)
	sh.table.Store(t)
}

// ─── TCMallocStore ──────────────────────────────────────────
//
// Key-Value хранилище на базе TCMalloc-style аллокатора
// с lock-free индексом для чтения.
//
// Уровни аллокации:
//
//	Level 1 (mcache):   lock-free, per-worker        → 99% аллокаций
//	Level 2 (mcentral): per-size-class mutex          → ~1%
//	Level 3 (mheap):    global mutex, chunk allocation → ~0.01%
//
// Уровни индексации:
//
//	GET: atomic.Pointer → HashTable.Get() → 0 locks (lock-free)
//	SET: shard.mu.Lock → HashTable.Put()  → 1 mutex (writers only)
//
// Данные хранятся в формате:
//
//	[4B keyLen][key bytes][4B valLen][val bytes]
type TCMallocStore struct {
	heap     *MHeap
	centrals [numSizeClasses]*MCentral
	caches   []*MCache // по одному на worker

	// Шардированный индекс.
	// hash(key) → Handle.
	// Каждый шард с padding до 64 байт (anti false sharing).
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
		s.shards[i].initShard()
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

// Set записывает ключ-значение.
//
// workerID определяет какой MCache использовать (lock-free путь).
//
// Горячий путь:
//  1. mcache.Alloc() → 0 locks (span bump pointer)
//  2. encodeInto(buf) → запись в pre-allocated блок
//  3. shard.mu.Lock() → table.Put() → mu.Unlock() (writers only mutex)
//
// КЛЮЧЕВОЕ ОТЛИЧИЕ от прежней версии:
//
//	Прежде: sh.mu.Lock() блокировал и readers (GET), и writers (SET).
//	Теперь: sh.mu.Lock() блокирует ТОЛЬКО writers.
//	        GET идёт через atomic.Pointer — полностью lock-free.
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

	t := sh.table.Load()

	// Auto-grow: если таблица заполнена >70% — увеличиваем в 2x.
	// Auto-rebuild: если tombstones >25% — пересобираем.
	// Grow/Rebuild создают новую таблицу, atomic.Store подменяет указатель.
	// Readers (Get) подхватят новую таблицу при следующем Load().
	// Старая таблица будет собрана GC когда все readers уйдут.
	if t.NeedsGrow() {
		t = t.Grow()
		sh.table.Store(t)
	} else if t.NeedsRebuild() {
		t = t.Rebuild()
		sh.table.Store(t)
	}

	t.Put(hash, uint64(handle))

	sh.mu.Unlock()
}

// Get читает значение по ключу.
//
// ★ LOCK-FREE ★ — не берёт никаких mutex/RWMutex.
//
// Путь:
//  1. hash → shard → atomic.Pointer.Load() → table
//  2. table.Get(hash) → atomic.Load слотов (lock-free)
//  3. heap.Resolve(handle) → buf (lock-free: chunks append-only)
//  4. decodeFrom(buf) → (key, value)
//
// Стоимость: ~5-10ns (вместо ~50ns с RLock/RUnlock).
func (s *TCMallocStore) Get(key string) ([]byte, bool) {
	hash := hashStoreKey(key)
	sh := &s.shards[hash%numStoreShards]

	// LOCK-FREE: просто atomic Load, никакого mutex.
	t := sh.table.Load()
	rawHandle, ok := t.Get(hash)
	if !ok {
		return nil, false
	}

	handle := Handle(rawHandle)

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
//  1. hash → shard → mu.Lock → table.Delete() → mu.Unlock
//  2. mcache.Free(handle) → span.Free(objIndex) (возврат блока)
func (s *TCMallocStore) Del(workerID int, key string) bool {
	hash := hashStoreKey(key)
	sh := &s.shards[hash%numStoreShards]

	sh.mu.Lock()

	t := sh.table.Load()
	rawHandle, ok := t.Delete(hash)

	// Rebuild если накопилось слишком много tombstones.
	if t.NeedsRebuild() {
		newT := t.Rebuild()
		sh.table.Store(newT)
	}

	sh.mu.Unlock()

	if !ok {
		return false
	}

	// Освобождаем блок обратно в аллокатор
	s.caches[workerID].Free(s.heap, Handle(rawHandle))
	return true
}

// Len возвращает количество ключей.
func (s *TCMallocStore) Len() int {
	total := 0
	for i := 0; i < numStoreShards; i++ {
		t := s.shards[i].table.Load()
		total += t.Len()
	}
	return total
}

// ForEach итерирует по всем ключам.
// Используется для snapshot/compaction — не на горячем пути.
func (s *TCMallocStore) ForEach(fn func(key string, value []byte)) {
	for i := 0; i < numStoreShards; i++ {
		t := s.shards[i].table.Load()
		for j := uint64(0); j < t.size; j++ {
			h := t.slots[j].hash.Load()
			if h != emptyHash && h != tombstoneHash {
				rawHandle := t.slots[j].handle.Load()
				buf := s.heap.Resolve(Handle(rawHandle))
				key, value := decodeFrom(buf)
				fn(key, value)
			}
		}
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

// GetKeysInSlot возвращает ключи, принадлежащие конкретному слоту кластера.
// Используется для миграции ключей между нодами кластера.
func (s *TCMallocStore) GetKeysInSlot(slot uint16, count int, slotFunc func(string) uint16) []string {
	var keys []string
	s.ForEach(func(key string, value []byte) {
		if len(keys) >= count {
			return
		}
		if slotFunc(key) == slot {
			keys = append(keys, key)
		}
	})
	return keys
}

// DelSimple удаляет ключ без указания workerID.
// Использует worker 0 для Free (допустимо для cold path: TTL eviction, migration).
//
// Реализует интерфейс store.Evictor для TTLManager.
func (s *TCMallocStore) DelSimple(key string) bool {
	return s.Del(0, key)
}
