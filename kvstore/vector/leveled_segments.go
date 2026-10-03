package vector

import "sync"

// =============================================================================
// Segment interface — единица хранения (либо FrozenGraph, либо plain HNSW)
// =============================================================================

// segment — внутренний интерфейс. filterFn != nil → фильтровать результаты.
type segment interface {
	Search(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult
	Len() int
	// HasKey проверяет, содержит ли сегмент живой вектор с этим ключом.
	// Используется Delete для подтверждения, что tombstone нужно поставить.
	HasKey(key string) bool
	// Catalog возвращает тенант-каталог сегмента (Вещь 1) или nil, если
	// тенант-режим выключен (cfg.TenantOf == nil). Диапазоны каталога индексируют
	// flat-раскладку сегмента в порядке frozen id.
	Catalog() *TenantCatalog
	// Attrs возвращает колоночный слой атрибутов сегмента (uint/multi-attr) или nil.
	// Колонки выровнены по frozen id (как Catalog). Используется SearchFilter.
	Attrs() *segmentAttrs
	// SearchTenant ищет K ближайших ТОЛЬКО в блоке тенанта. Маршрутизация по
	// собственному каталогу: brute-блок (#6) → перебор по диапазону (#7); graph-блок
	// или каталог==nil → HNSW-обход с filterFn (включает тенант-предикат). Если
	// каталог есть, но тенанта нет в сегменте → пустой результат (сегмент пропущен).
	SearchTenant(query []float32, K, efSearch int, tenant uint64, dst []FrozenResult, filterFn func(string) bool) []FrozenResult
	// SearchFilter — мульти-атрибутный фильтр (колоночный слой). partitionAttr из f.Eq
	// (если есть) задаёт блок тенанта через каталог; остальные Eq/Range — idx-предикат
	// по uint-колонкам. filterFn — tombstone-фильтр, применяется наравне.
	SearchFilter(query []float32, K, efSearch int, partitionAttr string, f Filter, dst []FrozenResult, filterFn func(string) bool) []FrozenResult
	// Text возвращает текстовый BM25-слой сегмента (постинги по frozen id) или
	// nil, если текстов в сегменте нет. Слой выровнен по frozen id (как Attrs).
	Text() *segmentText
	// TextKey маппит frozen id дока (bm25Hit.doc из Text().search) в
	// пользовательский ключ. Валиден при Text() != nil.
	TextKey(doc uint32) string
}

type leveledSearchState struct {
	workerBufs [][]FrozenResult // буфер результатов для каждой горутины-воркера
	combined   []VSearchResult  // общий буфер для слияния (был []LeveledResult)

	// dedupPos: key → индекс в combined. Дедуп по ключу на merge (один ключ может
	// жить и в дельте, и во frozen после upsert). Пулится, чистится clear() каждый
	// поиск → амортизированно zero-alloc.
	dedupPos map[string]int
	// rank[i] — провенанс-ранг combined[i] (меньше = свежее). Хранится параллельно
	// combined, чтобы при коллизии ключа оставить копию из более свежего источника.
	rank []int64
}

var leveledSearchPool = sync.Pool{
	New: func() any {
		return &leveledSearchState{
			workerBufs: make([][]FrozenResult, 0, 8),
			combined:   make([]VSearchResult, 0, 256),
			dedupPos:   make(map[string]int, 256),
			rank:       make([]int64, 0, 256),
		}
	},
}

// -----------------------------------------------------------------------------
// frozenSegment — обёртка вокруг *FrozenGraph, реализует segment.
// -----------------------------------------------------------------------------

type frozenSegment struct {
	fg    *FrozenGraph
	cat   *TenantCatalog // тенант-каталог (Вещь 1); nil если тенант-режим выключен
	attrs *segmentAttrs  // колоночный слой атрибутов; nil если атрибутов нет
	text  *segmentText   // текстовый BM25-слой; nil если текстов нет
	// keySet — ленивый индекс ключей для HasKey (строится первым вызовом,
	// string-ключи — views в blob сегмента, живут не дольше него). Мотив:
	// строгое затенение (StrictSegShadow) зовёт HasKey на каждый кандидат
	// top-K × каждый более свежий сегмент — бинпоиск find стал заметен в
	// профиле (замер №2 П3: 0.865×). Цена: +~(30-50Б)/ключ, платится лениво.
	keySetOnce sync.Once
	keySet     map[string]struct{}
}

func (s *frozenSegment) Len() int                  { return s.fg.Len() }
func (s *frozenSegment) Catalog() *TenantCatalog   { return s.cat }
func (s *frozenSegment) Attrs() *segmentAttrs      { return s.attrs }
func (s *frozenSegment) Text() *segmentText        { return s.text }
func (s *frozenSegment) TextKey(doc uint32) string { return s.fg.viewKey(int(doc)) }

// HasKey — O(1) по ленивому keySet. Исторически был линейным («редкие ручные
// DEL»), затем бинпоиском по sorted-пермутации, теперь map: строгое затенение
// П3 сделало HasKey горячим путём чтения.
func (s *frozenSegment) HasKey(key string) bool {
	s.keySetOnce.Do(func() {
		n := s.fg.keys.count()
		ks := make(map[string]struct{}, n)
		for i := 0; i < n; i++ {
			if k := s.fg.keys.view(i); k != "" {
				ks[k] = struct{}{}
			}
		}
		s.keySet = ks
	})
	_, ok := s.keySet[key]
	return ok
}

func (s *frozenSegment) Search(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	return s.fg.Search(query, K, efSearch, dst, filterFn)
}

func (s *frozenSegment) SearchTenant(query []float32, K, efSearch int, tenant uint64, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	if s.cat == nil {
		return s.fg.Search(query, K, efSearch, dst, filterFn) // нет каталога → фильтрованный обход
	}
	r, ok := s.cat.Lookup(tenant)
	if !ok {
		return dst[:0] // тенанта нет в этом сегменте
	}
	if r.Kind == IndexBrute {
		return s.fg.bruteRange(query, K, r.Start, r.End, dst, filterFn)
	}
	return s.fg.Search(query, K, efSearch, dst, filterFn) // graph-блок: обход с тенант-фильтром
}

func (s *frozenSegment) SearchFilter(query []float32, K, efSearch int, partitionAttr string, f Filter, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	return searchFilterFrozen(s.fg, s.cat, s.attrs, query, K, efSearch, partitionAttr, f, dst, filterFn)
}

// frozenFilterable — общий контракт frozen-сегментов (float32 и SQ8) для роутинга
// фильтра: оба умеют brute по диапазону с idx-предикатом и graph-обход с idx-предикатом.
type frozenFilterable interface {
	bruteRangeAttr(query []float32, K, start, end int, dst []FrozenResult, predIdx func(int) bool) []FrozenResult
	searchWithIdx(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool, predIdx func(int) bool) []FrozenResult
	viewKey(i int) string
	// residualBruteBudget — макс. число совпавших, при котором residual-brute по
	// блоку выгоднее обхода filtered-HNSW (см. searchFilterFrozen). 0 — режим
	// выключен для этого типа сегмента (float32 на high-dim: brute неконкурентен).
	residualBruteBudget(efSearch int) int
}

// searchFilterFrozen — общий роутинг мульти-атрибутного фильтра для frozen-сегментов.
// Партиционный атрибут (если задан в f.Eq) резолвится в код через dict сегмента → блок
// тенанта из каталога: brute-блок → bruteRangeAttr (рычаг #2), graph-блок → обход,
// ограниченный диапазоном блока + остаточный idx-предикат. Остальные атрибуты (Eq/Range)
// компилируются в idx-предикат по uint-колонкам. filterFn (tombstone) применяется наравне.
func searchFilterFrozen(fg frozenFilterable, cat *TenantCatalog, sa *segmentAttrs,
	query []float32, K, efSearch int, partitionAttr string, f Filter,
	dst []FrozenResult, filterFn func(string) bool) []FrozenResult {

	predIdx, matchable := sa.compile(f, partitionAttr)
	if !matchable {
		return dst[:0] // фильтр заведомо никого не пропустит в этом сегменте
	}

	// Партиционный роутинг по каталогу.
	if pv, ok := f.Eq[partitionAttr]; ok && partitionAttr != "" {
		// Партиционный фильтр задан — сегмент обязан выполнить его через каталог+dict.
		// Если каталога/атрибутов нет (сегмент из смешанной истории ингеста без
		// колоночного слоя, sa==nil), тенанта в нём нет → пусто. НЕ падать в полный
		// обход ниже: иначе безатрибутный сегмент вернул бы ВСЕ векторы = утечка
		// между тенантами (compile пропускает партиц-атрибут → predIdx=always-true).
		if cat == nil || sa == nil {
			return dst[:0]
		}
		dict := sa.dict[partitionAttr]
		if dict == nil {
			return dst[:0]
		}
		code, ok := dict.lookup(pv)
		if !ok {
			return dst[:0] // тенанта нет в этом сегменте
		}
		r, ok := cat.Lookup(uint64(code))
		if !ok {
			return dst[:0]
		}
		if r.Kind == IndexBrute {
			return fg.bruteRangeAttr(query, K, r.Start, r.End, dst, composeIdxKey(predIdx, filterFn, fg))
		}
		start, end := r.Start, r.End
		// Residual-aware brute: крупный тенант-блок (Kind=graph), но остаточный предикат
		// (region/price/…) может оставить мало совпавших. Filtered-HNSW в этом случае
		// переисследует граф (тратит фиксированный ef-бюджет дистанций НЕЗАВИСИМО от
		// селективности — фильтр применяется к результатам, не к обходу), а brute
		// считает дистанции ТОЛЬКО по совпавшим. Кроссовер тут — по числу совпавших
		// (≈ef-бюджет графа), он ВЫШЕ и не привязан к dim-aware блок-порогу (#1).
		//
		// Считаем совпавших дёшево (целочисленный predIdx по uint-колонкам, без
		// дистанций, с ранним выходом за бюджетом). Брутим ⟺ остаток СЕЛЕКТИВЕН:
		//   1. абсолютно мало:  matched ≤ budget (=ef×residualBruteFactor);
		//   2. мало как доля:   matched·denom ≤ block — иначе плотный остаток ≈
		//      single-attr, и должен идти block-роутингом (#1), не сюда (иначе
		//      regression на не-селективном среднем блоке);
		//   3. SQ8-сегмент: budget>0 (float32-brute на high-dim неконкурентен —
		//      FrozenGraph.residualBruteBudget возвращает 0).
		budget := fg.residualBruteBudget(efSearch)
		if predIdx != nil && budget > 0 {
			block := end - start
			matched := 0
			for i := start; i < end && matched <= budget; i++ {
				if predIdx(i) {
					matched++
				}
			}
			if matched <= budget && matched*residualBruteSelDenom <= block {
				return fg.bruteRangeAttr(query, K, start, end, dst, composeIdxKey(predIdx, filterFn, fg))
			}
		}
		// graph-блок: обход всего сегмента, но в результаты — только узлы блока тенанта
		// (i ∈ [Start,End)) + остаточный предикат.
		rangePred := func(i int) bool {
			if i < start || i >= end {
				return false
			}
			return predIdx == nil || predIdx(i)
		}
		return fg.searchWithIdx(query, K, efSearch, dst, filterFn, rangePred)
	}

	// Без партиционного атрибута — обход всего сегмента с остаточным предикатом.
	return fg.searchWithIdx(query, K, efSearch, dst, filterFn, predIdx)
}

// composeIdxKey объединяет idx-предикат (атрибуты по колонкам) и строковый filterFn
// (tombstone) в один idx-предикат для brute-пути.
func composeIdxKey(predIdx func(int) bool, filterFn func(string) bool, fg frozenFilterable) func(int) bool {
	if filterFn == nil {
		return predIdx
	}
	return func(i int) bool {
		if predIdx != nil && !predIdx(i) {
			return false
		}
		return filterFn(fg.viewKey(i))
	}
}

// -----------------------------------------------------------------------------
// frozenSQSegment — обёртка вокруг *FrozenGraphSQ (SQ8-compressed), реализует segment.
// -----------------------------------------------------------------------------

type frozenSQSegment struct {
	fg    *FrozenGraphSQ
	cat   *TenantCatalog // тенант-каталог (Вещь 1); nil если тенант-режим выключен
	attrs *segmentAttrs  // колоночный слой атрибутов; nil если атрибутов нет
	text  *segmentText   // текстовый BM25-слой; nil если текстов нет
	// keySet — ленивый индекс ключей для HasKey (см. frozenSegment.keySet).
	keySetOnce sync.Once
	keySet     map[string]struct{}
}

func (s *frozenSQSegment) Len() int                  { return s.fg.Len() }
func (s *frozenSQSegment) Catalog() *TenantCatalog   { return s.cat }
func (s *frozenSQSegment) Attrs() *segmentAttrs      { return s.attrs }
func (s *frozenSQSegment) Text() *segmentText        { return s.text }
func (s *frozenSQSegment) TextKey(doc uint32) string { return s.fg.viewKey(int(doc)) }

// HasKey — O(1) по ленивому keySet (аналог frozenSegment).
func (s *frozenSQSegment) HasKey(key string) bool {
	s.keySetOnce.Do(func() {
		n := s.fg.keys.count()
		ks := make(map[string]struct{}, n)
		for i := 0; i < n; i++ {
			if k := s.fg.keys.view(i); k != "" {
				ks[k] = struct{}{}
			}
		}
		s.keySet = ks
	})
	_, ok := s.keySet[key]
	return ok
}

func (s *frozenSQSegment) Search(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	return s.fg.Search(query, K, efSearch, dst, filterFn)
}

func (s *frozenSQSegment) SearchTenant(query []float32, K, efSearch int, tenant uint64, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	if s.cat == nil {
		return s.fg.Search(query, K, efSearch, dst, filterFn)
	}
	r, ok := s.cat.Lookup(tenant)
	if !ok {
		return dst[:0]
	}
	if r.Kind == IndexBrute {
		return s.fg.bruteRange(query, K, r.Start, r.End, dst, filterFn)
	}
	return s.fg.Search(query, K, efSearch, dst, filterFn)
}

func (s *frozenSQSegment) SearchFilter(query []float32, K, efSearch int, partitionAttr string, f Filter, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	return searchFilterFrozen(s.fg, s.cat, s.attrs, query, K, efSearch, partitionAttr, f, dst, filterFn)
}

// -----------------------------------------------------------------------------
// hnswSegment — обёртка вокруг *Graph (plain HNSW), реализует segment.
// -----------------------------------------------------------------------------

type hnswSegment struct {
	g    *Graph
	keys map[uint64]string // internal_id → user_key
	// keySet — ленивый обратный индекс user_key → ∃ для HasKey: keys иммутабелен
	// после build (мутаций по месту нет, удаления идут tombstone'ами store-уровня),
	// строится один раз при первом HasKey под mu.Lock. Цена: +~(16-48Б)/ключ
	// памяти, платится только если сегмент вообще спрашивают (Delete, строгое
	// затенение П3). До индекса HasKey был O(N)-сканом на каждый вызов.
	keySet map[string]struct{}
	mu     sync.RWMutex   // защита от чтения пока граф строится
	cat    *TenantCatalog // тенант-каталог (Вещь 1); nil если тенант-режим выключен
	attrs  *segmentAttrs  // колоночный слой атрибутов; nil если атрибутов нет
	text   *segmentText   // текстовый BM25-слой; nil если текстов нет
}

func (s *hnswSegment) Catalog() *TenantCatalog { return s.cat }
func (s *hnswSegment) Attrs() *segmentAttrs    { return s.attrs }
func (s *hnswSegment) Text() *segmentText      { return s.text }

// TextKey — для hnswSegment frozen id текстового слоя = graph id: build без
// freeze вставляет entries по порядку (без шафла), id идут последовательно.
func (s *hnswSegment) TextKey(doc uint32) string { return s.keys[uint64(doc)] }

// SearchTenant — для hnswSegment brute по arena не реализован (редкий случай:
// dim>csr И !UseSQ). Всегда фильтрованный обход графа с тенант-предикатом — корректно,
// но без brute-fast-path. Если тенанта нет в каталоге → пустой результат.
func (s *hnswSegment) SearchTenant(query []float32, K, efSearch int, tenant uint64, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	if s.cat != nil {
		if _, ok := s.cat.Lookup(tenant); !ok {
			return dst[:0]
		}
	}
	return s.Search(query, K, efSearch, dst, filterFn)
}

// SearchFilter — мульти-атрибутный фильтр для hnswSegment (редкий путь dim>csr И !UseSQ).
// Без brute-fast-path: graph-обход с idx-фильтром (node id = frozen id = индекс колонки),
// ограниченным диапазоном блока тенанта при заданном партиционном атрибуте.
func (s *hnswSegment) SearchFilter(query []float32, K, efSearch int, partitionAttr string, f Filter, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	predIdx, matchable := s.attrs.compile(f, partitionAttr)
	if !matchable {
		return dst[:0]
	}
	lo, hi := -1, -1 // диапазон блока тенанта (-1 = без ограничения)
	if pv, ok := f.Eq[partitionAttr]; ok && partitionAttr != "" {
		// см. searchFilterFrozen: партиционный фильтр на сегменте без каталога/
		// атрибутов → пусто, НЕ полный обход (утечка между тенантами).
		if s.cat == nil || s.attrs == nil {
			return dst[:0]
		}
		dict := s.attrs.dict[partitionAttr]
		if dict == nil {
			return dst[:0]
		}
		code, ok := dict.lookup(pv)
		if !ok {
			return dst[:0]
		}
		r, ok := s.cat.Lookup(uint64(code))
		if !ok {
			return dst[:0]
		}
		lo, hi = r.Start, r.End
	}
	internalFilter := func(nodeID uint64) bool {
		i := int(nodeID)
		if lo >= 0 && (i < lo || i >= hi) {
			return false
		}
		if predIdx != nil && !predIdx(i) {
			return false
		}
		key, ok := s.keys[nodeID]
		if !ok || key == "" {
			return false
		}
		return filterFn == nil || filterFn(key)
	}
	rawSearch := s.g.SearchFiltered(query, K, efSearch, internalFilter)
	if cap(dst) < len(rawSearch) {
		dst = make([]FrozenResult, 0, len(rawSearch))
	} else {
		dst = dst[:0]
	}
	for _, r := range rawSearch {
		key, ok := s.keys[r.ID]
		if !ok || key == "" {
			continue
		}
		dst = append(dst, FrozenResult{Key: key, Dist: r.Distance})
	}
	return dst
}

func (s *hnswSegment) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.g.nodeCount
}

// HasKey — O(1) по ленивому keySet (первый вызов строит индекс за O(N)).
func (s *hnswSegment) HasKey(key string) bool {
	s.mu.RLock()
	if s.keySet != nil {
		_, ok := s.keySet[key]
		s.mu.RUnlock()
		return ok
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keySet == nil {
		ks := make(map[string]struct{}, len(s.keys))
		for _, k := range s.keys {
			if k != "" {
				ks[k] = struct{}{}
			}
		}
		s.keySet = ks
	}
	_, ok := s.keySet[key]
	return ok
}

func (s *hnswSegment) Search(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if filterFn != nil {
		internalFilter := func(nodeID uint64) bool {
			key, ok := s.keys[nodeID]
			return ok && key != "" && filterFn(key)
		}
		rawSearch := s.g.SearchFiltered(query, K, efSearch, internalFilter)
		if cap(dst) < len(rawSearch) {
			dst = make([]FrozenResult, 0, len(rawSearch))
		} else {
			dst = dst[:0]
		}
		for _, r := range rawSearch {
			key, ok := s.keys[r.ID]
			if !ok || key == "" {
				continue
			}
			dst = append(dst, FrozenResult{Key: key, Dist: r.Distance})
		}
		return dst
	}

	rawSearch := s.g.Search(query, K, efSearch)
	if cap(dst) < len(rawSearch) {
		dst = make([]FrozenResult, 0, len(rawSearch))
	} else {
		dst = dst[:0]
	}
	for _, r := range rawSearch {
		key, ok := s.keys[r.ID]
		if !ok || key == "" {
			continue
		}
		dst = append(dst, FrozenResult{Key: key, Dist: r.Distance})
	}
	return dst
}
