package vector

import (
	"hash/maphash"
	"math"
	"slices"
	"sync"
	"sync/atomic"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// DeltaSegment — буфер вставки (Write Path) с инкрементальным HNSW-индексом.
//
// Все Add-запросы попадают сюда первым делом. Хранение — плоский slab (data),
// он остаётся источником истины для flush (ExtractAll). ДОПОЛНИТЕЛЬНО каждый
// вектор инкрементально вставляется в живой HNSW-граф `idx` (модель Qdrant):
// поиск по дельте идёт через idx.Search() за log(n), а не брутфорсом O(n).
// Это снимает просадку QPS во время записи при крупной дельте.
//
// ─── Шардинг (шаг 5: конкурентные вставки) ───────────────────────────────
// Дельта разбита на nShards независимых под-сегментов (shard). Ключ роутится
// hash(key)%nShards в свой shard со СВОИМ локом. Вставки в разные шарды идут
// конкурентно → insert/s масштабируется с числом писателей (раньше всё
// сериализовалось глобальным lvs.mu, потолок ~800/s). Поиск фан-аутит по всем
// шардам и мержит top-K. nShards==1 (дефолт) = прежнее поведение, один граф.
//
// Лок-контракт: внешний lvs.mu держится как RLock на горячем пути (Add/Search)
// и Lock при swap дельты (compaction). Безопасность ДАННЫХ обеспечивается
// пер-шардовым shard.mu (Append/Delete → Lock, Search/Get → RLock). Порядок
// захвата всегда lvs.mu → shard.mu, поэтому дедлоков нет.
//
// idx шарда — только для поиска: при flush он отбрасывается (Close), сегмент
// строится из slab. Freeze-on-flush (заморозка графа без rebuild) применим
// только при nShards==1 — при шардинге flush всегда rebuild из merged slab.
// =============================================================================

// DeltaEntry — ключ + вектор в дельте.
type DeltaEntry struct {
	Key   string
	Vec   []float32  // ссылка в data-слайсе, не копия
	Attrs Attributes // колоночный слой (uint/multi-attr); zero-value = без атрибутов
}

// deltaResult — внутренний тип результата Search/BruteForce.
type deltaResult struct {
	key  string
	dist float32
}

// deltaShard — независимый под-сегмент дельты со своим slab, HNSW-индексом и локом.
type deltaShard struct {
	mu      sync.RWMutex
	data    []float32      // flat: data[i*dim : (i+1)*dim] = вектор i (источник истины для flush)
	keys    []string       // keys[i] = ключ вектора i ("" = tombstone)
	attrs   []Attributes   // attrs[i] = атрибуты вектора i (nil-поля = без атрибутов)
	keyIdx  map[string]int // ключ → индекс в data (для быстрого upsert/delete)
	idx     *Graph         // инкрементальный HNSW-индекс для поиска (Qdrant-style)
	alloc   *tcmalloc.TCMallocStore
	nodeKey map[uint32]string // id узла графа → ключ (маппинг результатов поиска)
	keyNode map[string]uint32 // ключ → текущий id узла (для удаления при upsert/delete)
}

// DeltaSegment — буфер вставки, разбитый на nShards шардов.
type DeltaSegment struct {
	dim     int
	maxSize int // максимальное число слотов (по всем шардам) до flush
	distFn  DistanceFunc

	shards  []*deltaShard
	nShards int
	seed    maphash.Seed // для роутинга ключ→shard

	live  atomic.Int64 // число ЖИВЫХ векторов (len keyIdx по всем шардам)
	slots atomic.Int64 // число занятых слотов (len keys по всем шардам, вкл. tombstones)
}

// newDeltaShard создаёт один шард с собственным графом и аллокатором.
func newDeltaShard(dim, capHint int, distFn DistanceFunc, m, efC int) *deltaShard {
	alloc := tcmalloc.NewTCMallocStore(1)
	g := NewGraph(distFn, alloc)
	if efC > 0 {
		g.EfConstruction = efC
	}
	if m > 0 {
		g.M = m
		g.M0 = 2 * m
		g.Ml = 1.0 / math.Log(float64(m))
		g.pruneBufItems = make([]item, 0, g.M0+1)
		g.pruneBufIDs = make([]uint64, 0, g.M0+1)
		g.insertBuf = make([]uint64, 0, g.M0+1)
	}
	return &deltaShard{
		data:    make([]float32, 0, capHint*dim),
		keys:    make([]string, 0, capHint),
		keyIdx:  make(map[string]int, capHint),
		idx:     g,
		alloc:   alloc,
		nodeKey: make(map[uint32]string, capHint),
		keyNode: make(map[string]uint32, capHint),
	}
}

// NewDeltaSegment создаёт дельту с одним шардом (прежнее поведение).
// distFn нужна для инкрементального HNSW-индекса поиска; m/efC — параметры графа
// (0 = дефолты NewGraph). m/efC задают, чтобы граф дельты совпадал с сегментами
// — тогда freeze-on-flush даёт эквивалентный сегмент.
func NewDeltaSegment(dim, maxSize int, distFn DistanceFunc, m, efC int) *DeltaSegment {
	return NewDeltaSegmentSharded(dim, maxSize, distFn, m, efC, 1)
}

// NewDeltaSegmentSharded создаёт дельту, разбитую на nShards независимых шардов.
// nShards<=1 → один шард (= NewDeltaSegment, freeze-on-flush применим).
// nShards>1 → конкурентные вставки в разные шарды; flush через rebuild из slab.
func NewDeltaSegmentSharded(dim, maxSize int, distFn DistanceFunc, m, efC, nShards int) *DeltaSegment {
	if nShards < 1 {
		nShards = 1
	}
	// Ёмкость шарда: maxSize/nShards с запасом (роутинг хэшем не идеально ровный).
	capHint := maxSize/nShards + maxSize/nShards/4 + 1
	shards := make([]*deltaShard, nShards)
	for i := range shards {
		shards[i] = newDeltaShard(dim, capHint, distFn, m, efC)
	}
	return &DeltaSegment{
		dim:     dim,
		maxSize: maxSize,
		distFn:  distFn,
		shards:  shards,
		nShards: nShards,
		seed:    maphash.MakeSeed(),
	}
}

// Sharded возвращает true, если дельта разбита более чем на один шард.
// При шардинге freeze-on-flush не применим (нет единого графа) → rebuild из slab.
func (d *DeltaSegment) Sharded() bool { return d.nShards > 1 }

// shardFor роутит ключ в шард по хэшу. Детерминированно для жизни DeltaSegment
// (seed фиксирован), поэтому upsert/delete всегда попадают в тот же шард.
func (d *DeltaSegment) shardFor(key string) *deltaShard {
	if d.nShards == 1 {
		return d.shards[0]
	}
	h := maphash.String(d.seed, key)
	return d.shards[h%uint64(d.nShards)]
}

// Close освобождает аллокаторы всех шардов (останавливает их goroutine).
// Вызывать после swap дельты, когда она больше не используется для поиска
// И графы больше не нужны (freeze скопировал данные / rebuild завершён).
func (d *DeltaSegment) Close() {
	for _, s := range d.shards {
		if s.alloc != nil {
			s.alloc.Close()
			s.alloc = nil
		}
	}
}

// Graph возвращает HNSW-граф для freeze-on-flush. Валидно только при nShards==1.
func (d *DeltaSegment) Graph() *Graph { return d.shards[0].idx }

// FreezeKeys возвращает map[node_id]→key для FreezeGraph/FreezeGraphSQ.
// Валидно только при nShards==1.
func (d *DeltaSegment) FreezeKeys() map[uint64]string {
	s := d.shards[0]
	m := make(map[uint64]string, len(s.nodeKey))
	for gid, key := range s.nodeKey {
		m[uint64(gid)] = key
	}
	return m
}

// Len возвращает число ЖИВЫХ векторов в дельте (без tombstones).
func (d *DeltaSegment) Len() int { return int(d.live.Load()) }

// Full возвращает true, если дельта заполнена и нужен flush.
// Считаем по числу занятых слотов (вкл. tombstones) — отражает заполненность памяти.
func (d *DeltaSegment) Full() bool { return d.slots.Load() >= int64(d.maxSize) }

// MaxSize возвращает максимальное число слотов до flush.
func (d *DeltaSegment) MaxSize() int { return d.maxSize }

// Append добавляет или обновляет вектор по ключу.
// Берёт лок шарда (конкурентно с другими шардами). Возвращает becameFull=true,
// если ЭТА вставка довела число занятых слотов ровно до maxSize — сигнал для
// единственного триггера компакции (без busy-loop, как прежний `==maxSize`).
func (d *DeltaSegment) Append(key string, vec []float32) (becameFull bool) {
	return d.AppendWithAttrs(key, vec, Attributes{})
}

// AppendWithAttrs — вставка с атрибутами (колоночный слой). attrs хранятся параллельно
// keys/data в шарде и переживают flush через ExtractAll → buildSegmentAttrs.
func (d *DeltaSegment) AppendWithAttrs(key string, vec []float32, attrs Attributes) (becameFull bool) {
	s := d.shardFor(key)
	s.mu.Lock()
	if slot, exists := s.keyIdx[key]; exists {
		// Upsert: перезаписываем slab in-place, число слотов/живых не меняется.
		copy(s.data[slot*d.dim:(slot+1)*d.dim], vec)
		if slot < len(s.attrs) {
			s.attrs[slot] = attrs
		}
		if oldGid, ok := s.keyNode[key]; ok {
			s.idx.Delete(uint64(oldGid))
			delete(s.nodeKey, oldGid)
		}
		gid := s.idx.Insert(vec)
		s.keyNode[key] = gid
		s.nodeKey[gid] = key
		s.mu.Unlock()
		return false
	}
	slot := len(s.keys)
	s.data = append(s.data, vec...)
	s.keys = append(s.keys, key)
	s.attrs = append(s.attrs, attrs)
	s.keyIdx[key] = slot
	gid := s.idx.Insert(vec)
	s.keyNode[key] = gid
	s.nodeKey[gid] = key
	s.mu.Unlock()

	d.live.Add(1)
	newSlots := d.slots.Add(1)
	return newSlots == int64(d.maxSize)
}

// Delete помечает вектор как удалённый (tombstone — пустая строка ключа).
// Не освобождает память — compaction уберёт при следующем flush.
func (d *DeltaSegment) Delete(key string) bool {
	s := d.shardFor(key)
	s.mu.Lock()
	idx, exists := s.keyIdx[key]
	if !exists {
		s.mu.Unlock()
		return false
	}
	delete(s.keyIdx, key)
	s.keys[idx] = "" // tombstone
	if gid, ok := s.keyNode[key]; ok {
		s.idx.Delete(uint64(gid))
		delete(s.nodeKey, gid)
		delete(s.keyNode, key)
	}
	s.mu.Unlock()
	d.live.Add(-1)
	return true
}

// Get возвращает копию живого вектора по ключу (под shard RLock).
// found=false, если ключ отсутствует или tombstoned в дельте.
func (d *DeltaSegment) Get(key string) ([]float32, bool) {
	s := d.shardFor(key)
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx, ok := s.keyIdx[key]
	if !ok {
		return nil, false
	}
	if s.keys[idx] == "" {
		return nil, false
	}
	out := make([]float32, d.dim)
	copy(out, s.data[idx*d.dim:(idx+1)*d.dim])
	return out, true
}

// ForEachLive вызывает fn(key, vec) для каждого живого вектора во всех шардах.
// vec — ссылка во внутренний slab (не копия): не сохранять между вызовами.
// Под shard RLock каждого шарда поочерёдно.
func (d *DeltaSegment) ForEachLive(fn func(key string, vec []float32)) {
	for _, s := range d.shards {
		s.mu.RLock()
		for i, key := range s.keys {
			if key == "" {
				continue
			}
			fn(key, s.data[i*d.dim:(i+1)*d.dim])
		}
		s.mu.RUnlock()
	}
}

// Search ищет K ближайших через инкрементальные HNSW-индексы шардов (за log(n)).
// Фан-аут по всем шардам (каждый под RLock), затем merge top-K — контракт ≤K
// результатов сохранён (как у одного графа). filterFn применяется к РЕЗУЛЬТАТАМ.
func (d *DeltaSegment) Search(query []float32, K, efSearch int, filterFn func(string) bool) []deltaResult {
	if d.live.Load() == 0 {
		return nil
	}
	// Быстрый путь для одного шарда — без merge.
	if d.nShards == 1 {
		return d.shards[0].search(query, K, efSearch, filterFn, d.dim, d.distFn)
	}
	var merged []deltaResult
	for _, s := range d.shards {
		merged = append(merged, s.search(query, K, efSearch, filterFn, d.dim, d.distFn)...)
	}
	if len(merged) <= K {
		slices.SortFunc(merged, cmpDeltaResult)
		return merged
	}
	slices.SortFunc(merged, cmpDeltaResult)
	return merged[:K]
}

// search — поиск в одном шарде под RLock.
func (s *deltaShard) search(query []float32, K, efSearch int, filterFn func(string) bool, dim int, distFn DistanceFunc) []deltaResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Фильтрованный путь — точный brute по шарду (recall=1.0). Индексный Graph.Search
	// фильтрует POST-HOC: обходит граф БЕЗ фильтра, обрезает до топ-K по расстоянию, и
	// лишь ПОТОМ выкидывает непрошедших → при селективном фильтре recall обваливается
	// почти до selectivity (см. delta_filter_bug_test.go: 0.128 vs 1.0). Дельта
	// ограничена (bounded ~maxSize), brute по совпавшим дёшев и точен; фильтр
	// применяется к КАЖДОМУ вектору ДО отбора top-K. Без фильтра — быстрый граф-путь.
	if filterFn != nil {
		return s.bruteFiltered(query, K, dim, distFn, filterFn)
	}

	if s.idx == nil || len(s.nodeKey) == 0 {
		return nil
	}
	raw := s.idx.Search(query, K, efSearch)
	out := make([]deltaResult, 0, len(raw))
	for _, r := range raw {
		key, ok := s.nodeKey[uint32(r.ID)]
		if !ok {
			continue // узел удалён — пропускаем
		}
		out = append(out, deltaResult{key: key, dist: r.Distance})
	}
	return out
}

// bruteFiltered — точный top-K среди ВЕКТОРОВ ШАРДА, прошедших filterFn. Вызывается
// под s.mu.RLock. recall=1.0: дистанция считается для каждого совпавшего, top-K
// берётся уже после фильтра (в отличие от post-filter в индексном пути). Стоимость
// O(matched) дистанций — приемлемо, дельта ограничена. Семантика совпадает с
// DeltaSegment.BruteForce (эталон тестов), но по одному шарду.
func (s *deltaShard) bruteFiltered(query []float32, K, dim int, distFn DistanceFunc, filterFn func(string) bool) []deltaResult {
	if K <= 0 || len(s.keys) == 0 {
		return nil
	}
	var matched []deltaResult
	for i := 0; i < len(s.keys); i++ {
		key := s.keys[i]
		if key == "" || !filterFn(key) { // "" = tombstone
			continue
		}
		matched = append(matched, deltaResult{key: key, dist: distFn(query, s.data[i*dim:(i+1)*dim])})
	}
	slices.SortFunc(matched, cmpDeltaResult)
	if len(matched) > K {
		matched = matched[:K]
	}
	return matched
}

func cmpDeltaResult(a, b deltaResult) int {
	if a.dist < b.dist {
		return -1
	}
	if a.dist > b.dist {
		return 1
	}
	return 0
}

// BruteForce выполняет линейный поиск K ближайших во всех шардах дельты.
// filterFn != nil — включаются только векторы, для которых filterFn(key) == true.
// Используется тестами/бенчами как эталон против индексированного Search.
func (d *DeltaSegment) BruteForce(query []float32, K int, distFn DistanceFunc, filterFn func(string) bool) []deltaResult {
	if d.live.Load() == 0 {
		return nil
	}
	heap := make([]deltaResult, 0, K+1)

	heapPush := func(r deltaResult) {
		heap = append(heap, r)
		i := len(heap) - 1
		for i > 0 {
			p := (i - 1) / 2
			if heap[p].dist >= heap[i].dist {
				break
			}
			heap[p], heap[i] = heap[i], heap[p]
			i = p
		}
	}
	heapPop := func() {
		l := len(heap) - 1
		heap[0] = heap[l]
		heap = heap[:l]
		i := 0
		for {
			ll, r, s := 2*i+1, 2*i+2, i
			if ll < len(heap) && heap[ll].dist > heap[s].dist {
				s = ll
			}
			if r < len(heap) && heap[r].dist > heap[s].dist {
				s = r
			}
			if s == i {
				break
			}
			heap[i], heap[s] = heap[s], heap[i]
			i = s
		}
	}

	dim := d.dim
	for _, s := range d.shards {
		s.mu.RLock()
		for i := 0; i < len(s.keys); i++ {
			if s.keys[i] == "" {
				continue // tombstone
			}
			if filterFn != nil && !filterFn(s.keys[i]) {
				continue
			}
			vec := s.data[i*dim : (i+1)*dim]
			dist := distFn(query, vec)
			if len(heap) < K {
				heapPush(deltaResult{key: s.keys[i], dist: dist})
			} else if dist < heap[0].dist {
				heapPop()
				heapPush(deltaResult{key: s.keys[i], dist: dist})
			}
		}
		s.mu.RUnlock()
	}

	slices.SortFunc(heap, cmpDeltaResult)
	return heap
}

// AttrsSnapshot возвращает снимок атрибутов живых записей (key → Attributes).
// Используется SearchFilter: дельта не имеет колонок, поэтому её атрибуты судятся
// по этому снимку (matchAttrs). Bounded (delta ограничена), копия безопасна от гонок.
func (d *DeltaSegment) AttrsSnapshot() map[string]Attributes {
	out := make(map[string]Attributes, int(d.live.Load()))
	for _, s := range d.shards {
		s.mu.RLock()
		for i, key := range s.keys {
			if key == "" || i >= len(s.attrs) {
				continue
			}
			out[key] = s.attrs[i]
		}
		s.mu.RUnlock()
	}
	return out
}

// ExtractAll возвращает все живые записи (key, vec-slice) для compaction.
// Возвращает срезы на внутренний data-буфер шардов — не копирует.
// Вызывать только когда дельта больше не используется для записи (после swap).
func (d *DeltaSegment) ExtractAll() []DeltaEntry {
	out := make([]DeltaEntry, 0, int(d.live.Load()))
	for _, s := range d.shards {
		s.mu.RLock()
		for i, key := range s.keys {
			if key == "" {
				continue // tombstone
			}
			var attrs Attributes
			if i < len(s.attrs) {
				attrs = s.attrs[i]
			}
			out = append(out, DeltaEntry{
				Key:   key,
				Vec:   s.data[i*d.dim : (i+1)*d.dim],
				Attrs: attrs,
			})
		}
		s.mu.RUnlock()
	}
	return out
}
