package zset

import (
	"encoding/binary"
	"math"
	"sync"

	"kvstore/kvstore/internal/btree"
	"kvstore/kvstore/internal/store/tcmalloc"
)

// ============================================================================
// ZSetRegistry — реестр sorted sets на базе B+Tree
// ============================================================================
//
// Каждый sorted set — это BPTree (score → member), создаётся лениво при ZADD.
// Обратный индекс (member → score) хранится в KV store:
//   key = "__zidx:<setName>:<member>"  →  value = 8 bytes float64 (LE)
//
// Lock-free чтение (zero mutex на горячем пути):
//   sets.Load("prices")              ← sync.Map atomic.Pointer (lock-free)
//   tree.RangeSearch(min, max)       ← RLock (batch read)
//   tree.Search(score)              ← seqlock (lock-free)
//   kvStore.Get("__zidx:prices:X")   ← lock-free HashTable
//
// Единый аллокатор:
//   B+Tree nodes, member strings, KV data, vectors — всё в TCMalloc.
//   Один MemoryCounter, один DeferFree, один HeapStats.

// zsetEntry — один sorted set: дерево + writer-мьютекс.
//
// S3: ZAdd/ZRem — это check-then-act (Get(__zidx) → DeleteMember → Insert →
// Set(__zidx)). Без сериализации два конкурентных ZAdd одного member оба
// читают «не существует» → оба Insert → ДУБЛЬ в дереве + сирота в обратном
// индексе навсегда (ZCARD раздут, призрак в range). mu делает RMW атомарным
// per-set. Readers (ZScore/ZRange/ZCard/ForEach) mu НЕ берут — их защищают
// собственные локи BPTree (RLock/seqlock) и lock-free __zidx.
type zsetEntry struct {
	tree *btree.BPTree
	mu   sync.Mutex // сериализует writers одного set
}

// ZSetRegistry — реестр sorted sets.
type ZSetRegistry struct {
	sets  sync.Map // map[string]*zsetEntry — lock-free Load для существующих
	store *tcmalloc.TCMallocStore
}

// New создаёт реестр sorted sets.
func New(store *tcmalloc.TCMallocStore) *ZSetRegistry {
	return &ZSetRegistry{
		store: store,
	}
}

// reverseKey — ключ обратного индекса в KV store.
// Формат: "__zidx:<setName>:<member>" → 8 bytes float64 (LE).
func reverseKey(setName, member string) string {
	// Прямая конкатенация — быстрее fmt.Sprintf.
	return "__zidx:" + setName + ":" + member
}

// encodeScore кодирует float64 в 8 байт (little-endian).
func encodeScore(score float64) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], math.Float64bits(score))
	return buf[:]
}

// DecodeScore декодирует float64 из 8 байт (little-endian).
func DecodeScore(data []byte) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(data))
}

// EncodeZAddValue кодирует score + member для WAL Entry.Value.
// Формат: [8 bytes score LE float64][member string]
func EncodeZAddValue(score float64, member string) []byte {
	buf := make([]byte, 8+len(member))
	binary.LittleEndian.PutUint64(buf[:8], math.Float64bits(score))
	copy(buf[8:], member)
	return buf
}

// DecodeZAddValue декодирует score + member из WAL Entry.Value.
func DecodeZAddValue(data []byte) (float64, string) {
	score := math.Float64frombits(binary.LittleEndian.Uint64(data[:8]))
	member := string(data[8:])
	return score, member
}

// ── Public API ──────────────────────────────────────────────

// getOrCreate возвращает существующее дерево или создаёт новое.
// sync.Map.LoadOrStore — lock-free для существующих, мьютекс только при первом создании.
//
// workerID вызывающего воркера прокидывается в btree.New (аллокация корня из
// его MCache) — иначе все деревья аллоцировали бы из общего caches[0] из
// произвольных epoll-горутин (data race, см. [[zset alloc race]]).
func (r *ZSetRegistry) getOrCreate(workerID int, setName string) *zsetEntry {
	if v, ok := r.sets.Load(setName); ok {
		return v.(*zsetEntry)
	}
	// Lazy create — sync.Map корректно обрабатывает concurrent LoadOrStore.
	// Если два ZADD одновременно создают "prices" — один entry победит,
	// второй будет GC'd (один лишний узел, не страшно).
	e := &zsetEntry{tree: btree.New(r.store, workerID)}
	actual, _ := r.sets.LoadOrStore(setName, e)
	return actual.(*zsetEntry)
}

// get возвращает entry или nil если set не существует.
func (r *ZSetRegistry) get(setName string) *zsetEntry {
	v, ok := r.sets.Load(setName)
	if !ok {
		return nil
	}
	return v.(*zsetEntry)
}

// ZAdd добавляет member с score в sorted set.
// Если member уже существует — обновляет score.
// Возвращает true если новый member добавлен (не обновление).
func (r *ZSetRegistry) ZAdd(workerID int, setName string, score float64, member string) bool {
	e := r.getOrCreate(workerID, setName)
	rk := reverseKey(setName, member)

	// S3: весь check-then-act под per-set мьютексом — иначе конкурентные ZAdd
	// одного member дают дубль в дереве + сироту в обратном индексе.
	e.mu.Lock()
	defer e.mu.Unlock()

	// Проверяем обратный индекс: есть ли уже этот member?
	oldScoreBytes, existed := r.store.Get(rk)

	if existed {
		oldScore := DecodeScore(oldScoreBytes)
		if oldScore == score {
			// Тот же score — ничего не делаем
			return false
		}
		// Удаляем старую запись из дерева (по old score + member)
		e.tree.DeleteMember(oldScore, member)
	}

	// Вставляем новую запись (аллокация из кэша этого воркера)
	e.tree.Insert(workerID, score, member)

	// Обновляем обратный индекс
	r.store.Set(workerID, rk, encodeScore(score))

	return !existed
}

// ZScore возвращает score для member в sorted set.
// Lock-free: читает из KV store (atomic HashTable).
func (r *ZSetRegistry) ZScore(setName, member string) (float64, bool) {
	rk := reverseKey(setName, member)
	data, ok := r.store.Get(rk)
	if !ok {
		return 0, false
	}
	return DecodeScore(data), true
}

// ZRem удаляет member из sorted set.
// Возвращает true если member существовал.
func (r *ZSetRegistry) ZRem(workerID int, setName, member string) bool {
	e := r.get(setName)
	if e == nil {
		return false
	}

	rk := reverseKey(setName, member)

	// S3: check-then-act под тем же per-set мьютексом, что и ZAdd.
	e.mu.Lock()
	defer e.mu.Unlock()

	oldScoreBytes, existed := r.store.Get(rk)
	if !existed {
		return false
	}

	oldScore := DecodeScore(oldScoreBytes)
	e.tree.DeleteMember(oldScore, member)
	r.store.Del(workerID, rk)
	return true
}

// RangeResult — результат диапазонного поиска.
type RangeResult struct {
	Score  float64
	Member string
}

// ZRangeByScore возвращает все members в диапазоне [minScore, maxScore].
// Результаты отсортированы по score (B+Tree гарантирует порядок).
func (r *ZSetRegistry) ZRangeByScore(setName string, minScore, maxScore float64) []RangeResult {
	e := r.get(setName)
	if e == nil {
		return nil
	}

	raw := e.tree.RangeSearch(minScore, maxScore)
	results := make([]RangeResult, len(raw))
	for i, item := range raw {
		results[i] = RangeResult{Score: item.Score, Member: item.Member}
	}
	return results
}

// ZCard возвращает количество элементов в sorted set.
// Lock-free: atomic.Int64.Load().
func (r *ZSetRegistry) ZCard(setName string) int {
	e := r.get(setName)
	if e == nil {
		return 0
	}
	return e.tree.Len()
}

// MembersInRange возвращает множество member'ов в диапазоне score.
// Оптимизирован для VSIM.SEARCHRANGE: B+Tree RangeSearch → map[string]bool.
func (r *ZSetRegistry) MembersInRange(setName string, minScore, maxScore float64) map[string]bool {
	e := r.get(setName)
	if e == nil {
		return nil
	}

	raw := e.tree.RangeSearch(minScore, maxScore)
	if len(raw) == 0 {
		return nil
	}

	result := make(map[string]bool, len(raw))
	for _, item := range raw {
		result[item.Member] = true
	}
	return result
}

// MembersInRangeHashed — ★ ОПТИМИЗИРОВАННАЯ ★ версия MembersInRange.
//
// Вместо N аллокаций строк (resolveMember × N) возвращает множество
// FNV-1a хешей member'ов. Фильтрация HNSW кандидата:
//
//	allowed := zsetReg.MembersInRangeHashed("prices", 100, 500)
//	filterFn := func(key string) bool {
//	    _, ok := allowed[btree.HashMember(key)]
//	    return ok
//	}
//
// Аллокации: 1 (map) вместо 113 (map + 101 строк + slices).
// Latency: ~1.5 µs вместо ~10 µs на 100 элементов.
//
// Caveat: FNV-1a коллизия теоретически возможна (~1/2^64).
// Для mission-critical фильтрации можно верифицировать через ZSCORE.
func (r *ZSetRegistry) MembersInRangeHashed(setName string, minScore, maxScore float64) map[uint64]struct{} {
	e := r.get(setName)
	if e == nil {
		return nil
	}
	return e.tree.RangeCollectHashes(minScore, maxScore)
}

// ForEach итерирует по sorted set в порядке score.
// Для snapshot/WAL compaction.
func (r *ZSetRegistry) ForEach(setName string, fn func(score float64, member string)) {
	e := r.get(setName)
	if e == nil {
		return
	}
	e.tree.ForEach(fn)
}

// ForEachSet итерирует по всем sorted sets.
// Для snapshot (сохранение всех ZADD при compaction).
func (r *ZSetRegistry) ForEachSet(fn func(setName string)) {
	r.sets.Range(func(key, value any) bool {
		fn(key.(string))
		return true
	})
}
