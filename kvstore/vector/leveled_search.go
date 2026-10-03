package vector

import (
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
)

// =============================================================================
// Поиск
// =============================================================================

// segSearchFn — как искать в одном сегменте. Подменяется для tenant-aware поиска
// (роутинг через TenantCatalog: brute по диапазону / граф) без дублирования всей
// merge-машинерии search(). nil → defaultSegSearch (обычный seg.Search).
type segSearchFn func(seg segment, query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult

func defaultSegSearch(seg segment, query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	return seg.Search(query, K, efSearch, dst, filterFn)
}

// Search выполняет поиск K ближайших. dst — неиспользуется (для совместимости с VectorIndex).
func (lvs *LeveledVectorStore) Search(query []float32, K int, dst []VSearchResult) ([]VSearchResult, error) {
	return lvs.search(query, K, nil, nil, defaultSegSearch)
}

// SearchFiltered выполняет поиск K ближайших с фильтром по ключу. dst — неиспользуется.
func (lvs *LeveledVectorStore) SearchFiltered(query []float32, K int, filterFn func(string) bool, dst []VSearchResult) ([]VSearchResult, error) {
	return lvs.search(query, K, filterFn, nil, defaultSegSearch)
}

// SearchTenant — tenant-aware поиск (Вещь 1): результаты только из блока тенанта.
// Маршрутизация через TenantCatalog сегмента: блок brute-типа (#6) → точный перебор
// по непрерывному диапазону (#7, чужие векторы не трогаются); блок graph-типа →
// HNSW-обход сегмента с тенант-предикатом. Сегменты без каталога (после LoadBinary)
// и дельта — фильтрованный поиск с тем же предикатом. Требует cfg.TenantOf != nil.
func (lvs *LeveledVectorStore) SearchTenant(query []float32, K int, tenant uint64, dst []VSearchResult) ([]VSearchResult, error) {
	if lvs.cfg.TenantOf == nil {
		return nil, errTenantModeOff
	}
	// Footgun-гард: при PartitionAttr каталог адресуется dict-кодом атрибута,
	// ЛОКАЛЬНЫМ для каждого сегмента (per-segment dict) — глобальный uint64 не может
	// консистентно адресовать тенанта. SearchTenant(tenant) тихо попал бы в чужой
	// блок и вернул мусор (вскрыто валидацией dbpedia). Правильный API — SearchFilter,
	// который резолвит строку через dict каждого сегмента. См. TestTenant_PartitionAttr_*.
	if lvs.cfg.PartitionAttr != "" {
		return nil, errTenantWithPartition
	}
	tenantOf := lvs.cfg.TenantOf
	// Тенант-предикат для дельты и graph/fallback-путей. В brute-пути по чистому
	// диапазону он избыточен (там только свой тенант), но дёшев (#2: ~10-15%).
	tenantPred := func(key string) bool { return tenantOf(key) == tenant }
	segSearch := func(seg segment, q []float32, k, efS int, d []FrozenResult, f func(string) bool) []FrozenResult {
		return seg.SearchTenant(q, k, efS, tenant, d, f)
	}
	return lvs.search(query, K, tenantPred, nil, segSearch)
}

// SearchFilter — мульти-атрибутный фильтр (колоночный слой uint/multi-attr).
// Партиционный атрибут (cfg.PartitionAttr) из f.Eq задаёт блок тенанта (роутинг brute/
// graph через каталог); остальные Eq (категориальное равенство) и Range (числовой
// диапазон) фильтруют по uint-колонкам (рычаг #2). Дельта фильтруется по снимку её
// атрибутов (matchAttrs); сегменты — колонками внутри seg.SearchFilter.
func (lvs *LeveledVectorStore) SearchFilter(query []float32, K int, f Filter) ([]VSearchResult, error) {
	partitionAttr := lvs.cfg.PartitionAttr

	// Атрибут-суд memtable-кандидатов — инлайн по СОБСТВЕННЫМ атрибутам копии,
	// которые подаёт шард из-под своего RLock (см. deltaFilterFn): ни снапшота,
	// ни лукапов, ни замков. Flushing-дельты покрыты устройством search()
	// (memtable-набор = активная + flushing, грабля окна flush-visibility,
	// вскрытая оракульным тестом). История: здесь был ПОЛНЫЙ снимок атрибутов
	// всех дельт на КАЖДЫЙ запрос — O(|delta|) map-аллокация и конвой на
	// runtime-локах аллокатора под конкуренцией (нагрузочный прогон 23.07,
	// mutex-профиль: 88% в maps.newTable); затем лукап атрибутов свежайшей
	// копии — дедлок рекурсивного RLock из-под шардового замка. Стейл-копию, прошедшую
	// суд по своим атрибутам, гасит Contains/дедуп-затенение merge.
	var memAttrFilter deltaFilterFn
	if !f.empty() {
		memAttrFilter = func(_ string, a Attributes) bool { return matchAttrs(a, f, "") }
	}

	segSearch := func(seg segment, q []float32, k, efS int, d []FrozenResult, filterFn func(string) bool) []FrozenResult {
		return seg.SearchFilter(q, k, efS, partitionAttr, f, d, filterFn)
	}
	return lvs.search(query, K, nil, memAttrFilter, segSearch)
}

var errTenantModeOff = errorString("SearchTenant требует cfg.TenantOf != nil (тенант-режим выключен)")
var errTenantWithPartition = errorString("SearchTenant несовместим с cfg.PartitionAttr (каталог keyed по per-segment dict-коду атрибута, глобальный uint64 не адресует тенанта); используйте SearchFilter(Filter{Eq:{<attr>:<value>}})")

type errorString string

func (e errorString) Error() string { return string(e) }

// search — внутренняя реализация с опциональным filterFn.
//
// Быстрые пути (0 горутин):
//   - Только delta: прямой BruteForce, без merge
//   - Один сегмент (+ пустая delta): прямой Search, без merge
//
// Параллельный путь: N сегментов + delta, горутины только при N > 3.
//
// Tombstones: snapshot захватывается ПОД lock вместе с указателями сегментов
// и компонуется с filterFn в composedFilter. Если tombstones==nil (нет удалений)
// — ни одной аллокации, overhead нулевой.
func (lvs *LeveledVectorStore) search(query []float32, K int, filterFn func(string) bool, memAttrFilter deltaFilterFn, segSearch segSearchFn) ([]VSearchResult, error) {
	if segSearch == nil {
		segSearch = defaultSegSearch
	}
	lvs.mu.RLock()

	if lvs.dim == 0 {
		lvs.mu.RUnlock()
		return nil, nil
	}
	if len(query) != lvs.dim {
		lvs.mu.RUnlock()
		return nil, &dimMismatchError{expected: lvs.dim, got: len(query)}
	}

	efSearch := lvs.cfg.EfSearch
	if efSearch < K {
		efSearch = K
	}
	// При фильтрации расширяем окно поиска ×4 для компенсации recall-потерь.
	if filterFn != nil || memAttrFilter != nil {
		if efSearch*4 <= 2000 {
			efSearch *= 4
		} else {
			efSearch = 2000
		}
	}

	// Считаем сегменты и delta ПОД lock
	nSegs := 0
	for i := range lvs.levels {
		nSegs += len(lvs.levels[i])
	}
	hasDelta := lvs.delta != nil && lvs.delta.Len() > 0
	// hasFlushing — есть дельты, сфлашиваемые в фоне (swap уже был, сегмент ещё не
	// опубликован). Их надо искать наравне с активной дельтой (flush-visibility gap).
	// При наличии flushing идём общим путём — fast-path 2/3 их не учитывают.
	hasFlushing := len(lvs.flushing) > 0

	// Fast path 1: ничего нет — возвращаем nil
	if !hasDelta && !hasFlushing && nSegs == 0 {
		lvs.mu.RUnlock()
		return nil, nil
	}

	// Захватываем snapshot tombstones ПОД lock — атомарно вместе с указателями
	// сегментов. После RUnlock compaction может сбросить tombstones, но наш
	// snapshot останется консистентным: мы фильтруем по «не менее» актуальному
	// множеству (старые tombstones ⊆ актуальных → не покажем лишнего).
	tombSnap := lvs.tombstones.Load()

	// composedFilter = tombstone-check ∘ filterFn.
	// Передаётся во все сегменты и дельту. Если tombstones==nil и filterFn==nil
	// — composedFilter==nil → zero overhead на всех путях.
	var composedFilter func(string) bool
	if tombSnap != nil {
		ts := *tombSnap // immutable snapshot (COW-гарантия)
		if filterFn != nil {
			composedFilter = func(key string) bool {
				_, deleted := ts[key]
				return !deleted && filterFn(key)
			}
		} else {
			composedFilter = func(key string) bool {
				_, deleted := ts[key]
				return !deleted
			}
		}
	} else {
		composedFilter = filterFn // nil или caller-provided, без обёртки
	}

	// deltaFn — фильтр memtable-путей: key-only composedFilter, поднятый в
	// attrs-aware сигнатуру, + опциональный атрибутный суд memAttrFilter
	// (SearchFilter). Атрибуты кандидата подаёт сам шард из-под своего RLock
	// (см. deltaFilterFn) — внешних лукапов/замков в фильтре нет.
	var deltaFn deltaFilterFn
	if composedFilter != nil || memAttrFilter != nil {
		cf, af := composedFilter, memAttrFilter
		deltaFn = func(key string, attrs Attributes) bool {
			if cf != nil && !cf(key) {
				return false
			}
			return af == nil || af(key, attrs)
		}
	}

	// Активная delta — MUTABLE (Add делает append/copy в неё). Все обращения
	// к delta ниже идут ПОД lvs.mu.RLock(), иначе data race с параллельным Add.
	// BruteForce по ~10k векторов ~200μs — RLock на это время блокирует Add,
	// что приемлемо (delta bounded, flush регулярно освобождает lock).
	delta := lvs.delta

	// Fast path 2: только активная delta, нет сегментов и нет flushing — BruteForce
	// под RLock. 1 alloc/op (выходной heap). BruteForce по ~10k векторов ~200μs —
	// RLock на это время блокирует Add, что приемлемо (delta bounded).
	if nSegs == 0 && hasDelta && !hasFlushing {
		raw := delta.Search(query, K, efSearch, deltaFn)
		lvs.mu.RUnlock()
		if len(raw) == 0 {
			return nil, nil
		}
		result := make([]VSearchResult, len(raw))
		for i, r := range raw {
			result[i] = VSearchResult{Key: r.key, Distance: r.dist}
		}
		return result, nil
	}

	// Fast path 3: один сегмент, дельта пуста и нет flushing — прямой Search без merge
	if nSegs == 1 && !hasDelta && !hasFlushing {
		var seg segment
		for i := range lvs.levels {
			if len(lvs.levels[i]) > 0 {
				seg = lvs.levels[i][0]
				break
			}
		}
		lvs.mu.RUnlock()
		raw := segSearch(seg, query, K, efSearch, nil, composedFilter)
		if len(raw) > K {
			raw = raw[:K]
		}
		if len(raw) == 0 {
			return nil, nil
		}
		// Внешняя граница: r.Key — zero-copy view в blob сегмента; клонируем.
		result := make([]VSearchResult, len(raw))
		for i, r := range raw {
			result[i] = VSearchResult{Key: strings.Clone(r.Key), Distance: r.Dist}
		}
		return result, nil
	}

	// General path: N сегментов + memtable-набор (активная дельта + flushing-дельты).
	//
	// Корректность concurrency:
	//   - Сегменты immutable после публикации → поиск по ним идёт БЕЗ lvs.mu.
	//   - Memtable-дельты MUTABLE/в аллокаторе → Search по ним ОБЯЗАН идти ПОД
	//     lvs.mu.RLock(): активная — из-за конкурентного Add; flushing — из-за Close
	//     в applyResult (только под mu.Lock → эксклюзивно к RLock, нет UAF по slab).
	//     Обрабатываем синхронно под lock (~200μs/10k), затем отпускаем lock и
	//     параллельно обходим immutable-сегменты.
	//
	// memtables — дельты в порядке СВЕЖЕСТИ (freshest first): активная дельта, затем
	// flushing от новейшей к старейшей. Порядок = провенанс: memtable[i] свежее любого
	// сегмента и свежее memtable[j>i]. Contains ниже (после RUnlock) безопасен и на
	// закрытой дельте (keyIdx — Go-map, переживает swap+Close).
	memtables := make([]*DeltaSegment, 0, 1+len(lvs.flushing))
	if hasDelta {
		memtables = append(memtables, delta)
	}
	for i := len(lvs.flushing) - 1; i >= 0; i-- {
		memtables = append(memtables, lvs.flushing[i])
	}
	nMem := len(memtables)

	// 1 alloc/op на memtable (выходной heap), сегменты — через sync.Pool.
	memRes := make([][]deltaResult, nMem)
	for i, mt := range memtables {
		memRes[i] = mt.Search(query, K, efSearch, deltaFn)
	}

	// Копируем указатели сегментов (дешёво: ~8 байт на сегмент) вместе с
	// провенанс-рангом (меньше = свежее): levels[0] — свежайшие, внутри уровня
	// позже добавленный (больший индекс) свежее. Ранг нужен дедупу в merge, чтобы
	// при коллизии ключа (delta+frozen после upsert, либо frozen+frozen после
	// повторного flush) оставить копию из САМОГО свежего источника, а не по дистанции.
	segs := make([]segment, 0, nSegs)
	segRank := make([]int64, 0, nSegs)
	for lvl := range lvs.levels {
		for pos := range lvs.levels[lvl] {
			segs = append(segs, lvs.levels[lvl][pos])
			segRank = append(segRank, int64(lvl)<<40-int64(pos))
		}
	}
	lvs.mu.RUnlock()

	nWorkers := nSegs + nMem

	st := leveledSearchPool.Get().(*leveledSearchState)
	defer leveledSearchPool.Put(st)

	if cap(st.workerBufs) < nWorkers {
		st.workerBufs = make([][]FrozenResult, nWorkers)
	} else {
		st.workerBufs = st.workerBufs[:nWorkers]
	}
	for i := range st.workerBufs {
		if cap(st.workerBufs[i]) < efSearch {
			st.workerBufs[i] = make([]FrozenResult, 0, efSearch)
		} else {
			st.workerBufs[i] = st.workerBufs[i][:0]
		}
	}

	// memtable-результаты вычислены под RLock ([]deltaResult). Копируем в
	// workerBufs[0..nMem-1] (freshest first) как FrozenResult (struct conversion —
	// zero alloc, копирование string-header + float32 по значению).
	for i := 0; i < nMem; i++ {
		for _, r := range memRes[i] {
			st.workerBufs[i] = append(st.workerBufs[i], FrozenResult{Key: r.key, Dist: r.dist})
		}
	}
	idx := nMem

	// Сегменты — immutable, можно параллельно без lvs.mu.
	remainSegs := segs
	// Запускаем параллельный обход через фиксированный Worker Pool (Milvus-style).
	// Ранее здесь запускалась goroutine на КАЖДЫЙ сегмент (до 35 штук), что вызывало
	// огромный overhead на планировщик Go и context-switches на 12 ядрах.
	// Теперь Worker Pool распределяет сегменты на горутины равномерно, без аллокаций.
	if len(remainSegs) > 1 {
		lvs.searchSegmentsParallel(query, efSearch, st, remainSegs, idx, composedFilter, segSearch)
	} else {
		// Fast-path: всего 1 сегмент, работаем в текущей горутине (0 overhead).
		for _, seg := range remainSegs {
			st.workerBufs[idx] = segSearch(seg, query, efSearch, efSearch, st.workerBufs[idx], composedFilter)
			idx++
		}
	}

	// Flat merge с ДЕДУПОМ по ключу: один ключ может присутствовать сразу в
	// нескольких воркер-буферах (в дельте и во frozen после upsert; в двух frozen
	// после повторного flush). Оставляем копию из самого свежего источника
	// (минимальный ранг): delta (workerBufs[0]) свежее любого сегмента, среди
	// сегментов — по segRank. Без дедупа stale-копия занимала слот в top-K и
	// вытесняла легитимного соседа. Выбор ПО ПРОВЕНАНСУ, а не по дистанции — иначе
	// более близкий, но устаревший вектор победил бы свежий.
	// Ранг: memtable[i] (i<nMem) свежее любого сегмента; среди memtable меньший
	// индекс = свежее (активная дельта = MinInt64). Сегменты — по segRank (>= ~-nSegs,
	// т.е. всегда > MinInt64+nMem → memtable гарантированно побеждает).
	rankFor := func(wb int) int64 {
		if wb < nMem {
			return math.MinInt64 + int64(wb)
		}
		return segRank[wb-nMem]
	}

	total := 0
	for i := range st.workerBufs {
		total += len(st.workerBufs[i])
	}

	if cap(st.combined) < total {
		st.combined = make([]VSearchResult, 0, total)
	} else {
		st.combined = st.combined[:0]
	}
	if cap(st.rank) < total {
		st.rank = make([]int64, 0, total)
	} else {
		st.rank = st.rank[:0]
	}
	clear(st.dedupPos)

	strictShadow := lvs.cfg.StrictSegShadow
	for i := range st.workerBufs {
		r := rankFor(i)
		// Затенение: более свежий источник гасит stale-копию того же ключа, даже когда
		// свежая копия далеко от запроса и не попала в top-K (провенанс-дедуп тогда
		// бессилен — stale заняла бы слот и вытеснила легитимного соседа).
		//   - сегмент (i>=nMem) затеняется ЛЮБОЙ memtable-дельтой;
		//   - memtable[i] затеняется лишь БОЛЕЕ СВЕЖЕЙ memtable (индекс < i);
		//   - при StrictSegShadow сегмент затеняется и БОЛЕЕ СВЕЖИМ сегментом
		//     (segRank меньше): инвариант LSM — копия в более свежем сегменте
		//     всегда новее (merge дедупит, сохраняя свежайшую).
		// Contains race-free и O(1), безопасен и на сфлашенной (закрытой) дельте.
		shadowLimit := i
		if i >= nMem {
			shadowLimit = nMem
		}
		for _, res := range st.workerBufs[i] {
			shadowed := false
			for j := 0; j < shadowLimit; j++ {
				if memtables[j].Contains(res.Key) {
					shadowed = true
					break
				}
			}
			if shadowed {
				continue
			}
			if pos, ok := st.dedupPos[res.Key]; ok {
				if r < st.rank[pos] {
					st.combined[pos] = VSearchResult{Key: res.Key, Distance: res.Dist}
					st.rank[pos] = r
				}
				continue
			}
			st.dedupPos[res.Key] = len(st.combined)
			st.combined = append(st.combined, VSearchResult{Key: res.Key, Distance: res.Dist})
			st.rank = append(st.rank, r)
		}
	}

	if strictShadow {
		// Ленивый пере-суд топа (StrictSegShadow): хиты сортируются вместе с
		// рангом (плоская структура — без двойной индирекции пермутации),
		// спуск по дистанции гасит сегментные хиты, чей ключ живёт в более
		// свежем сегменте. Судятся только кандидаты до наполнения top-K.
		type rankedRes struct {
			res  VSearchResult
			rank int64
		}
		hits := make([]rankedRes, len(st.combined))
		for i := range st.combined {
			hits[i] = rankedRes{st.combined[i], st.rank[i]}
		}
		slices.SortFunc(hits, func(a, b rankedRes) int {
			if a.res.Distance < b.res.Distance {
				return -1
			}
			if a.res.Distance > b.res.Distance {
				return 1
			}
			return 0
		})
		memBound := int64(math.MinInt64) + int64(nMem)
		result := make([]VSearchResult, 0, min(K, len(hits)))
		for i := range hits {
			if len(result) == K {
				break
			}
			if r := hits[i].rank; r >= memBound { // хит сегмента
				stale := false
				for j := range segs {
					if segRank[j] < r && segs[j].HasKey(hits[i].res.Key) {
						stale = true
						break
					}
				}
				if stale {
					continue
				}
			}
			result = append(result, VSearchResult{Key: strings.Clone(hits[i].res.Key), Distance: hits[i].res.Distance})
		}
		return result, nil
	}

	slices.SortFunc(st.combined, func(a, b VSearchResult) int {
		if a.Distance < b.Distance {
			return -1
		}
		if a.Distance > b.Distance {
			return 1
		}
		return 0
	})

	if len(st.combined) > K {
		st.combined = st.combined[:K]
	}

	// Внешняя граница: клонируем ключи K выживших. Ключи из frozen-сегментов —
	// zero-copy views в blob сегмента; клон делает результат независимым от времени
	// жизни сегмента и не удерживает весь blob через указатель внутрь него. O(K)
	// клонов на запрос вместо efSearch×N (которые раньше делались внутри Search).
	result := make([]VSearchResult, len(st.combined))
	for i := range st.combined {
		result[i] = VSearchResult{Key: strings.Clone(st.combined[i].Key), Distance: st.combined[i].Distance}
	}
	return result, nil
}

// searchSegmentsParallel запускает горутину на каждый сегмент.
// searchSegmentsParallel распределяет обход сегментов между воркерами (горутинами).
// Вместо 1 горутины на сегмент (до 35+ горутин на запрос), мы создаём ровно
// `numWorkers` горутин. Каждая горутина последовательно обходит свою пачку сегментов.
// Это убирает overhead планировщика Go и максимально утилизирует L1/L2 кэши CPU.
//
// Только immutable-сегменты — вызывается ПОСЛЕ lvs.mu.RUnlock().
// startIdx — начальный индекс в st.workerBufs (0 если delta пуста, иначе 1,
// т.к. delta-результат уже в workerBufs[0]).
func (lvs *LeveledVectorStore) searchSegmentsParallel(query []float32, efSearch int, st *leveledSearchState, segs []segment, startIdx int, filterFn func(string) bool, segSearch segSearchFn) {
	numWorkers := runtime.NumCPU()
	if numWorkers > len(segs) {
		numWorkers = len(segs)
	}
	if numWorkers < 1 {
		numWorkers = 1
	}
	// Если воркер всего один — нет смысла использовать горутины, делаем последовательно.
	if numWorkers == 1 {
		idx := startIdx
		for _, seg := range segs {
			st.workerBufs[idx] = segSearch(seg, query, efSearch, efSearch, st.workerBufs[idx], filterFn)
			idx++
		}
		return
	}

	segsPerWorker := (len(segs) + numWorkers - 1) / numWorkers

	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		// Разбиваем срез сегментов на пачки [w * segsPerWorker : ...].
		start := w * segsPerWorker
		end := start + segsPerWorker
		if start > len(segs) {
			start = len(segs)
		}
		if end > len(segs) {
			end = len(segs)
		}
		if start >= end {
			wg.Done() // нет работы для этого воркера (остаток меньше numWorkers)
			continue
		}

		// Копируем срез сегментов и начальный индекс для безопасной записи в workerBufs.
		workerSegs := segs[start:end]
		bufStartIdx := startIdx + start

		go func(workerBufs [][]FrozenResult) {
			defer wg.Done()
			idx := bufStartIdx
			for _, seg := range workerSegs {
				workerBufs[idx] = segSearch(seg, query, efSearch, efSearch, workerBufs[idx], filterFn)
				idx++
			}
		}(st.workerBufs)
	}

	wg.Wait()
}
