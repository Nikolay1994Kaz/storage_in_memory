package tcmalloc

import (
	"encoding/binary"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"
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
	mu    sync.RWMutex              // защищает writers (Lock) и readers (RLock)
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

	// ── Deferred Free (QSBR — quiescent-state reclamation) ──
	//
	// Проблема (баг T2): lock-free Get читает handle из таблицы, затем
	// Resolve+decode из span-памяти. Если Del/overwrite освободит слот, а
	// Alloc переиспользует его ПОКА Get ещё читает — Get прочитает чужие
	// данные (UAF/torn-read/ложный not-found).
	//
	// Старое решение — освобождать по таймеру (100ms) — НЕВЕРНО: время ≠
	// гарантия завершения читателей. Длинная async-преемпция или STW-пауза
	// GC переживают 100ms, а storedKey-чек ловит не все случаи (склейка
	// старого ключа с новым значением).
	//
	// Новое решение — QSBR (как RCU в ядре Linux): слот освобождается не по
	// таймеру, а когда КАЖДЫЙ воркер-читатель гарантированно прошёл «тихое»
	// состояние после ретайра. Детали и доказательство — в reclaim.go.
	//
	// epoch        — глобальное поколение (монотонно растёт).
	// workerEpoch  — на воркера: наблюдаемое поколение (offlineEpoch=не в кворуме).
	// pending      — отложенные хендлы, партиями по поколению ретайра (под deferMu).
	epoch       atomic.Uint64
	workerEpoch []atomic.Uint64
	deferMu     sync.Mutex
	pending     []retireBatch
	stopCh      chan struct{} // закрыть для остановки runDeferredFree
	closeOnce   sync.Once     // T4: Close идемпотентен (двойной close(stopCh) = паника)
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
		stopCh:   make(chan struct{}),
	}

	// QSBR: поколение стартует с 1, слоты воркеров — «онлайн на поколении 1».
	// Онлайн-старт (а не offlineEpoch=0) критичен для WAL-replay: во время
	// восстановления воркеры ещё не запущены и не двигают свои слоты, поэтому
	// min наблюдаемого поколения застревает на 1 → reclaimer НИЧЕГО не
	// освобождает, пока сервер не поднялся. Это защищает lock-free чтения
	// одиночной replay-горутины (ZAdd → store.Get), которая не рапортует QS.
	s.epoch.Store(1)
	s.workerEpoch = make([]atomic.Uint64, numWorkers)
	for i := range s.workerEpoch {
		s.workerEpoch[i].Store(1)
	}

	for i := 0; i < numStoreShards; i++ {
		s.shards[i].initShard()
	}

	// Аллокатор сам управляет своим deferred free —
	// никакой зависимости от WAL/Syncer/внешних таймеров.
	go s.runDeferredFree()

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
	if len(buf) < 4 {
		return "", nil
	}
	offset := 0

	keyLen := binary.LittleEndian.Uint32(buf[offset:])
	offset += 4

	if int(keyLen) < 0 || offset+int(keyLen) > len(buf) {
		return "", nil
	}
	key := string(buf[offset : offset+int(keyLen)])
	offset += int(keyLen)

	if offset+4 > len(buf) {
		return "", nil
	}
	valLen := binary.LittleEndian.Uint32(buf[offset:])
	offset += 4

	if int(valLen) < 0 || offset+int(valLen) > len(buf) {
		return "", nil
	}
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

	oldHandle, overwritten := t.Put(hash, uint64(handle))

	sh.mu.Unlock()

	if overwritten {
		s.DeferFree(Handle(oldHandle))
	}
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

	// ★ LOCK-FREE — ни одного mutex ★
	//
	// Безопасно потому что:
	//   1. table.Load() — atomic (если Set сделал Grow, мы прочитаем
	//      либо старую, либо новую таблицу — обе валидны)
	//   2. t.Get(hash) — atomic Load слотов
	//   3. heap.Resolve(handle) — lock-free (chunks append-only)
	//   4. Del НЕ вызывает Free(handle) сразу → данные в span
	//      НЕ перезаписываются пока мы читаем (deferred free)
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

	// ★ НЕ освобождаем сразу! ★
	// Handle кладётся в очередь отложенного Free.
	// Это гарантирует что in-flight Get (который уже прочитал
	// этот handle из таблицы, но ещё не вызвал Resolve)
	// прочитает валидные данные.
	s.DeferFree(Handle(rawHandle))
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
		sh := &s.shards[i]
		sh.mu.RLock()
		t := sh.table.Load()
		for j := uint64(0); j < t.size; j++ {
			h := t.slots[j].hash.Load()
			if h != emptyHash && h != tombstoneHash {
				rawHandle := t.slots[j].handle.Load()
				buf := s.heap.Resolve(Handle(rawHandle))
				key, value := decodeFrom(buf)
				if key != "" {
					fn(key, value)
				}
			}
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

// ══════════════════════════════════════════════════
// MEMORY LIMITS
// ══════════════════════════════════════════════════

// IsOOM проверяет, превышен ли лимит памяти.
// Вызывается перед каждым SET в обработчике команд.
func (s *TCMallocStore) IsOOM() bool {
	return s.heap.IsOOM()
}

// SetMaxMemory устанавливает лимит памяти в байтах.
// 0 = без лимита (по умолчанию).
func (s *TCMallocStore) SetMaxMemory(bytes int64) {
	s.heap.SetMaxMemory(bytes)
}

// UsedMemory возвращает текущее потребление памяти.
func (s *TCMallocStore) UsedMemory() int64 {
	return s.heap.UsedMemory()
}

// Clear удаляет ВСЕ данные из store.
//
// Используется при Full Sync репликации: реплика получает +FULLSYNC,
// очищает все данные и принимает свежий слепок от мастера.
// Аналог FLUSHALL в Redis.
//
// Реализация: проходим по всем шардам и пересоздаём индексы.
// Под мьютексом каждого шарда — безопасно для параллельных GET.
func (s *TCMallocStore) Clear() {
	for i := range s.shards {
		s.shards[i].mu.Lock()
		s.shards[i].table.Store(NewHashTable(defaultInitialCap))
		s.shards[i].mu.Unlock()
	}
}

// Alloc выделяет сырой блок памяти через per-worker mcache
// NumShards возвращает число worker-shard'ов (len(caches)). Используется для
// корректного распределения build workers по shard'ам (workerID % NumShards).
func (s *TCMallocStore) NumShards() int {
	return len(s.caches)
}

func (s *TCMallocStore) Alloc(workerID int, size int) ([]byte, Handle) {
	return s.caches[workerID].Alloc(size)
}

// Free освобождает ранее выделенный Handle
func (s *TCMallocStore) Free(workerID int, handle Handle) {
	s.caches[workerID].Free(s.heap, handle)
}

// Resolve возвращает слайс байт, на который указывает Handle (zero-copy)
func (s *TCMallocStore) Resolve(handle Handle) []byte {
	return s.heap.Resolve(handle)
}

// HeapStats возвращает статистику аллокатора кучи.
func (s *TCMallocStore) HeapStats() (numChunks int, totalBytes int, usedBytes int, numSpans int) {
	return s.heap.Stats()
}

// MaxMemory возвращает лимит памяти в байтах.
func (s *TCMallocStore) MaxMemory() int64 {
	return s.heap.MaxMemory()
}

// ══════════════════════════════════════════════════
// DEFERRED FREE (QSBR — see reclaim.go)
// ══════════════════════════════════════════════════

// runDeferredFree — внутренняя горутина аллокатора.
//
// Аллокатор САМ управляет своим lifecycle —
// не зависит от WAL/Syncer/внешних таймеров.
// Это Single Responsibility: память управляется аллокатором,
// WAL управляется syncer’ом — каждый отвечает за своё.
func (s *TCMallocStore) runDeferredFree() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// QSBR-gated: освобождаем только то, что прошло кворум quiescence.
			s.reclaim()
		case <-s.stopCh:
			// Остановка вызывается ПОСЛЕ того, как воркеры остановлены
			// (defer store.Close выполняется после srv.Stop) — читателей уже
			// нет, поэтому безопасно освободить всё накопленное безусловно.
			s.FlushDeferred()
			return
		}
	}
}

// Close останавливает внутренние горутины аллокатора.
// Вызывай при завершении работы (defer s.Close()).
//
// T4: идемпотентен — повторный Close (или двойной defer) не паникует
// на close уже закрытого канала.
func (s *TCMallocStore) Close() {
	s.closeOnce.Do(func() {
		close(s.stopCh)
	})
}

// DeferFree ставит handle в очередь отложенного освобождения (QSBR).
//
// Handle тегируется ТЕКУЩИМ поколением и будет фактически освобождён только
// когда каждый онлайн-воркер пройдёт «тихое» состояние на более позднем
// поколении (reclaim, см. reclaim.go). До тех пор ни один in-flight читатель,
// наблюдавший этот handle, не может прочитать освобождённую память.
//
// Используется:
//   - Del (KV store): lock-free Get может держать handle в момент удаления
//   - B+Tree Delete: lock-free Search может держать member handle
//   - B+Tree Insert (duplicate): старый member ещё может читаться Search'ем
func (s *TCMallocStore) DeferFree(h Handle) {
	g := s.epoch.Load()
	s.deferMu.Lock()
	// Копим в партию текущего поколения (последняя, если её тег совпал).
	if n := len(s.pending); n > 0 && s.pending[n-1].gen == g {
		s.pending[n-1].handles = append(s.pending[n-1].handles, h)
	} else {
		s.pending = append(s.pending, retireBatch{gen: g, handles: []Handle{h}})
	}
	s.deferMu.Unlock()
}

// FlushDeferred БЕЗУСЛОВНО освобождает все отложенные handle'ы, игнорируя
// кворум QSBR.
//
// Вызывающий ОБЯЗАН гарантировать, что параллельных lock-free читателей нет:
//   - graceful shutdown (воркеры уже остановлены, см. runDeferredFree/Close),
//   - тесты, детерминированно форсящие путь освобождения.
//
// Для штатного продакшн-пути используется reclaim() (QSBR-gated), а НЕ этот
// метод. Возвращает количество освобождённых handle'ов.
func (s *TCMallocStore) FlushDeferred() int {
	s.deferMu.Lock()
	pending := s.pending
	s.pending = nil
	s.deferMu.Unlock()

	// Освобождаем за пределами мьютекса — не блокируем Del/DeferFree.
	// pending теперь эксклюзивно наш: s.pending сброшен в nil.
	n := 0
	for _, b := range pending {
		for _, h := range b.handles {
			s.caches[0].Free(s.heap, h)
			n++
		}
	}
	return n
}

// DeferredQueueLen возвращает количество handle'ов, ожидающих освобождения.
// Для мониторинга / метрик.
func (s *TCMallocStore) DeferredQueueLen() int {
	s.deferMu.Lock()
	n := 0
	for _, b := range s.pending {
		n += len(b.handles)
	}
	s.deferMu.Unlock()
	return n
}

// MemoryCounter возвращает указатель на общий атомарный счётчик памяти.
// Используется для интеграции B+Tree / Arena с общим учётом IsOOM.
func (s *TCMallocStore) MemoryCounter() *atomic.Int64 {
	return s.heap.MemoryCounter()
}
