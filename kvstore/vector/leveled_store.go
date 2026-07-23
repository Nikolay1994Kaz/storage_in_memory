package vector

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"kvstore/kvstore/internal/monitoring"
	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// LeveledVectorStore — LSM-style векторное хранилище с Delta + Compaction.
//
// Архитектура:
//
//	Write Path (все размерности):
//	  Add(key, vec) → DeltaSegment (O(1) append)
//	  Когда delta.Full() → trigger background compaction
//
//	Read Path:
//	  Search(q, K) → параллельно по delta + все сегменты уровней → merge top-K
//
//	Compaction (background, не блокирует reads):
//	  L0: buildHNSW(deltaMax vectors) → [CSR если dim≤256]
//	  L1: merge 4×L0 → buildHNSW(4*deltaMax) → [CSR если dim≤256]
//	  L2: merge 4×L1 → buildHNSW(16*deltaMax) → ...
//
// Параметры по умолчанию:
//
//	deltaMax = min(50_000, 100MB / (dim * 4))
//	fanout   = 4
//	CSR threshold = dim ≤ 256
//
// =============================================================================

const (
	// csrDimThreshold — граница: dim ≤ N → freeze в CSR (+14% QPS),
	// dim > N → оставляем plain HNSW (distance compute доминирует).
	csrDimThreshold = 256

	// defaultFanout — сколько сегментов одного уровня сливается в следующий.
	defaultFanout = 4

	// flushingBacklogCap — swap дельты откладывается, пока flushing держит ≥ N
	// дельт: каждая flushing-дельта — nShards fp32-графов с ef-полом на каждый
	// запрос (brownout 09.07: 5-6 flushing × 12 шардов ≈ 70 графов, 70% CPU).
	// Дельта копит дальше (Append растёт за maxSize) → флаши реже и крупнее.
	// Повтор swap гарантирован: applyResult каждого build ре-триггерит compact,
	// пока delta.Full(); FlushDeltaSync ре-триггерит на каждом пробуждении.
	flushingBacklogCap = 2

	// deltaHardGrowthFactor — предел роста дельты сверх maxSize: при N×maxSize
	// свапаем несмотря на flushingBacklogCap, чтобы один build (и пиковая память
	// его аллокатора) не разрастался безгранично при перегрузе вставками.
	deltaHardGrowthFactor = 4

	// maxLevels — максимальное число уровней LSM-иерархии.
	maxLevels = 8

	// Типы сегментов в бинарном снапшоте.
	segTypeFrozen    byte = 0
	segTypeHNSWFlat  byte = 1
	segTypeFrozenSQ8 byte = 2 // SQ8-compressed frozen (uint8 codes)
)

// leveledMagic — магические байты заголовка бинарного снапшота LeveledVectorStore.
// Версия в байте [5]: v1 — без тенант-меты; v2 — после каждого frozen/SQ-сегмента
// сериализованы TenantCatalog + segmentAttrs (durability колоночного слоя); v3 —
// hnswSegment (flat, dim>256) тоже персистит атрибуты по вектору (writeAttrs),
// колонки+каталог регенерируются buildSegment на load. v1/v2 читаются как прежде.
// leveledMagic: "LVLV" + версия формата. v6 добавил 8B snapshotLSN-watermark
// в заголовок (после snapshotTime) — recovery-идемпотентность векторов. v7 —
// текстовый BM25-слой: frozen/SQ-сегменты сериализуют segmentText (writeSegText)
// после segMeta, hnswSegment персистит термы по вектору (writeTerms) после
// attrs — слой регенерируется buildSegment на load. Снапшоты v<7 читаются как
// прежде (текста в них нет → слой nil).
var leveledMagic = [8]byte{'L', 'V', 'L', 'V', 0, 7, 0, 0}

// leveledMagicPrefix — общая часть ('LVLV') для проверки формата без версии.
var leveledMagicPrefix = [4]byte{'L', 'V', 'L', 'V'}

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
// DEL»), затем бинпоиском по sorted-пермутации (TTL-жнец, порог 3 шага 6),
// теперь map: строгое затенение П3 сделало HasKey горячим путём чтения.
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
	// памяти, платится только если сегмент вообще спрашивают (жнец, строгое
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

// =============================================================================
// LeveledVectorStore
// =============================================================================

// LeveledConfig — параметры хранилища.
type LeveledConfig struct {
	// DeltaMax — максимальное число векторов в дельте до flush.
	// 0 = автоматически: min(10_000, 40MB / (dim * 4)).
	DeltaMax int

	// Fanout — кол-во сегментов одного уровня до merge в следующий.
	Fanout int

	// MergeBatchMax — макс сегментов, забираемых ОДНИМ merge с уровня
	// (batch-merge). Freeze-per-shard публикует nShards мини-сегментов за флаш;
	// каскад ровно по Fanout штук проигрывает продюсеру гонку (e2e 10.07:
	// 111 мини в L0 к концу bulk-load) и пере-rebuild'ит каждый вектор на
	// каждом уровне пирамиды. Батч-заход забирает весь накопившийся префикс
	// уровня до этого капа: та же линейная стоимость на вектор, но одним
	// rebuild'ом — заходов кратно меньше, fan-out поиска падает скачком.
	// Кап ограничивает пик памяти/длительность одного merge (fp32-копии всех
	// входных векторов + построение графа). 0 = 8×Fanout; значения < Fanout
	// поднимаются до Fanout.
	MergeBatchMax int

	// M — параметр M HNSW (число соседей). 0 = default из NewGraph.
	M int

	// EfConstruction — ширина поиска при построении HNSW.
	EfConstruction int

	// EfSearch — ширина поиска при запросах.
	EfSearch int

	// Distance — функция расстояния.
	Distance DistanceFunc

	// Metric — first-class метрика для SQ8-обхода (ADC-режим). Источник истины:
	// при MetricAuto (дефолт) выводится из Distance через inferMetric один раз
	// при инициализации (legacy-мост). Новый код задаёт явно (напр. MetricCosine),
	// чтобы не полагаться на хрупкий вывод по указателю. См. distance.go.
	Metric Metric

	// Allocator — менеджер памяти для HNSW.
	Allocator *tcmalloc.TCMallocStore

	// UseSQ — включить Scalar Quantization (int8) для frozen-сегментов (любая dim).
	// Сжимает рабочий набор векторов 4× (float32→uint8), поднимает QPS за счёт
	// cache-locality. Recall держится ≥96% (vs 97% float32) благодаря ADC+rerank.
	// Применяется только к новым сегментам после buildSegment; существующие
	// сегменты сохраняют свой формат (segType различает в сериализации).
	UseSQ bool

	// NumBuilders — число параллельных build workers для компакции.
	// 0 = auto (NumCPU/2, clamped 2..8). Build Pool: пока один worker строит
	// тяжёлый L2-сегмент (~100s), остальные параллельно флашат дельту и строят
	// L0/L1 → вставка не блокируется при burst-write.
	NumBuilders int

	// DeltaShards — число шардов дельты для конкурентных вставок (шаг 5).
	// 0/1 = один граф (прежнее поведение). >1 = ключи роутятся hash%N в
	// независимые шарды со своими локами → Add масштабируется по числу
	// писателей. Flush чистых векторов идёт через freeze-per-shard (каждый
	// шард → свой мини-сегмент L0, без rebuild); attrs/tenant/fp32 dim>256
	// флашатся rebuild'ом из slab.
	DeltaShards int

	// TenantOf — если задана, включает тенант-локальную раскладку (Вещь 1):
	// при build/merge entries сортируются по коду тенанта (#5 ведущий ключ) →
	// каждый тенант ложится непрерывным блоком, на сегмент вешается TenantCatalog
	// с типом индекса по порогу #6. nil = выключено (прежнее поведение).
	// В тенант-режиме freeze-on-flush отключается (нужен rebuild для сортировки).
	TenantOf func(key string) uint64

	// PartitionAttr — имя категориального атрибута (колоночный слой), по которому
	// идёт тенант-локальная раскладка (заменяет TenantOf продуктовым вводом через
	// AddWithAttrs). Его dict-код служит ведущим ключом сортировки/каталога. "" =
	// слой выключен. Имеет приоритет над TenantOf, если задан.
	PartitionAttr string

	// BruteThreshold — порог #6 (абсолютный размер блока тенанта): блок ≤ порога →
	// IndexBrute, иначе IndexGraph. 0 = DefaultBruteThreshold. Действует только
	// при TenantOf != nil или PartitionAttr != "".
	BruteThreshold int

	// IdleConsolidateAfter — затишье мутаций (Add/Delete), после которого фон
	// консолидирует индекс: остаток дельты флашится, все сегменты мержатся в один.
	// Закрывает сценарий bulk-load→read: суб-fanout остаток (<Fanout сегментов на
	// уровне) обычным каскадом не мержится никогда, а search платит за каждый
	// сегмент отдельным полным обходом графа. Guard от рестройки-шторма: если
	// крупнейший сегмент уже держит ≥90% векторов, полный rebuild ради хвоста не
	// запускается (мелочь дочистит fanout-каскад). 0 = выключено.
	IdleConsolidateAfter time.Duration

	// StrictSegShadow — строгое затенение сегмент-vs-сегмент в публичных путях
	// поиска (Search*/SearchTextFilter): хит сегмента гасится, если БОЛЕЕ СВЕЖИЙ
	// сегмент содержит этот ключ, даже когда свежая копия не попала в свои хиты
	// (отфильтрована атрибутами или не матчится запросом). Закрывает стейл-
	// семантику переходных мульти-сегментных состояний (та же дыра, что чинил
	// пере-суд VMEM.Recall, репро TestVMEMSupersedeTwoSegmentLeak) ценой
	// HasKey-лукапов по более свежим сегментам на каждый хит: O(log n) во
	// frozen/SQ, O(N) в hnsw. false = прежняя семантика («затенение живёт
	// только среди хитов», компакция нормализует). Экспериментальный флаг
	// П3-прототипа: судьба решается A/B-замером цены (порог ≥0.9× QPS).
	StrictSegShadow bool
}

// compactionResult — результат build/merge goroutine, передаётся через resultChan.
// Значение (не указатель) — channel receive создаёт happens-before для всех полей.
type compactionResult struct {
	kind      taskKind
	sourceLvl int     // для merge: уровень источник
	targetLvl int     // куда добавлен результат
	epoch     uint64  // эпоха на момент GRAB
	result    segment // построенный сегмент (nil = failure)
	// results — мини-сегменты freeze-per-shard (taskFlushDelta, шардированная
	// дельта): каждый шард заморожен отдельным сегментом, applyResult публикует
	// ВСЕ атомарно под одним Lock вместе со снятием дельты с flushing. Взаимно
	// исключен с result (обычный flush/freeze кладёт один сегмент в result).
	// Пустой при result==nil = failure (all-or-nothing, как у result).
	results []segment
	oldSegs []segment // для merge: сегменты для удаления из sourceLvl
	// appliedTombstones — ключи, исключённые из merge-сегмента по tombstone.
	// applyResult чистит их tombstones ПОСЛЕ атомарной публикации и лишь если
	// ключа больше нет в других сегментах. nil для flush.
	appliedTombstones map[string]struct{}
	// flushDelta — сфлашенная дельта (taskFlushDelta): держалась searchable в
	// lvs.flushing до этого момента. applyResult удаляет её из flushing и Close'ит
	// ПОСЛЕ публикации сегмента (единственный owner Close на нормальном пути).
	flushDelta *DeltaSegment
}

// LeveledVectorStore — основное хранилище с leveled compaction.
type LeveledVectorStore struct {
	cfg LeveledConfig

	mu    sync.RWMutex
	delta *DeltaSegment
	dim   int // устанавливается при первой вставке или LoadBinary

	// flushing — дельты, сфлашиваемые в фоне: swap уже произошёл (новые Add идут в
	// lvs.delta), но сегмент ещё НЕ опубликован в levels. Держим их searchable
	// (immutable memtable á-la LSM), иначе между swap и публикацией сегмента
	// сфлашиваемые векторы невидимы — flush-visibility gap (transient false-negative).
	// Заполняется под mu.Lock при swap в handleCompactSignal; элемент удаляется и
	// Close'ится в applyResult ПОСЛЕ публикации сегмента (единственный owner Close на
	// нормальном пути — эксклюзивно к search-RLock, нет UAF). После swap дельта
	// immutable (Add в неё не пишет) → конкурентный Search по ней безопасен под RLock.
	flushing []*DeltaSegment

	// tombstoneMu сериализует COW-мутации tombstones на горячем пути Add,
	// который теперь держит lvs.mu лишь как RLock (несколько писателей сразу).
	// Чтения tombstones остаются lock-free через atomic.Pointer.
	tombstoneMu sync.Mutex

	// levels[i] — слой i, содержит []segment, упорядоченных по времени создания.
	// levels[0] — самые свежие малые сегменты (каждый ≈ deltaMax векторов).
	levels [maxLevels][]segment

	// tombstones — COW-множество удалённых ключей (atomic.Pointer для lock-free чтения).
	// nil = нет удалений (zero-overhead).
	tombstones atomic.Pointer[map[string]struct{}]

	// compactionEpoch — поколение данных. Bump'ится под write-lock в Clear.
	// Compactor захватывает epoch перед долгой работой (build/merge вне lock) и
	// проверяет под lock перед записью в levels — иначе остаётся TOCTOU-окно,
	// в котором Clear() зануляет levels, а compactor публикует устаревший сегмент.
	compactionEpoch atomic.Uint64

	// compacting — атомарный флаг: идёт ли фоновая/синхронная компакция.
	compacting atomic.Bool

	// compactCh — сигнальный канал для запуска фоновой компакции.
	compactCh chan struct{}

	// done — канал для остановки фонового воркера.
	done chan struct{}

	// ─── Build Pool (producer-consumer компакция) ───
	//
	// coordinateCompaction() = producer: GRAB (delta flush + overflow merges)
	// под lvs.mu, кладёт tasks в buildQueue. Никаких тяжёлых BUILD внутри.
	//
	// buildWorker() × numBuilders = consumers: BUILD (buildSegment/mergeSegments
	// вне lock) + PUBLISH (append в levels под lock + epoch check).
	//
	// Это устраняет блокировку вставки: пока один worker строит L2 (~100s),
	// остальные флашат дельту и строят L0/L1 параллельно.

	// buildQueue — очередь задач для build workers. Bounded (32) — backpressure:
	// когда полна, coordinator блокируется → insert не опережает build (анти-OOM).
	buildQueue chan *compactionTask

	// buildWG — WaitGroup для graceful shutdown build workers.
	buildWG sync.WaitGroup

	// coordWG — WaitGroup для coordinator (compactWorker).
	coordWG sync.WaitGroup

	// numBuilders — фактическое число build workers (из cfg.NumBuilders, auto).
	numBuilders int

	// closeOnce — защита от двойного close(done) в Close().
	closeOnce sync.Once

	// flushCond — механизм ожидания FlushDeltaSync. FlushDeltaSync форсит swap
	// pre-call дельты и ждёт по членству в flushing; applyResult/empty-path будят
	// cond безусловно при снятии дельты с flushing (см. FlushDeltaSync).
	flushCond *sync.Cond

	// buildWorkerIDCounter — раздаёт уникальные workerID для параллельных buildSegment.
	// TCMallocStore.Alloc(workerID, ...) шардирует по workerID; конкурентный доступ
	// к одному caches[workerID] — data race. Каждый buildWorker берёт свой ID при старте.
	buildWorkerIDCounter atomic.Int32

	// buildAllocators — pool per-worker аллокаторов. TCMallocStore НЕ потокобезопасен
	// для конкурентных Alloc через общий MCentral/MHeap, поэтому каждый buildWorker
	// использует свой изолированный аллокатор. Создаются один раз при старте.
	buildAllocators []*tcmalloc.TCMallocStore

	// mergeAllocator — изолированный аллокатор для merge goroutines.
	// Может быть несколько параллельных merge (разные уровни) → нужен pool.
	// mergeAllocatorsCounter раздаёт round-robin.
	mergeAllocators        []*tcmalloc.TCMallocStore
	mergeAllocatorsCounter atomic.Int32

	// inFlightBuilds — счётчик delta-flush задач в работе.
	// Increment перед запуском горутины, decrement в applyResult после publish/drop.
	inFlightBuilds atomic.Int32

	// buildSem — семафор: не более numBuilders builds одновременно.
	// Горутины ждут на семафоре; coordinator никогда не блокируется.
	// Это обеспечивает изоляцию allocator (только numBuilders concurrent builds)
	// и предотвращает TCMallocStore race без ограничения количества горутин.
	buildSem chan struct{}

	// ─── Event Loop Coordinator ───
	//
	// Coordinator — чистый select{} event loop. Никогда не строит сам.
	//   compactCh   → grab delta → запустить goroutine build → продолжить
	//   resultChan  → атомарно применить результат в levels[] → продолжить
	//
	// Старые сегменты НЕ убираются из levels[] пока merge не завершён (нет дыр для search).
	// mergeInFlight[L] = true → merge уровня L в работе (новый не запускаем).

	// resultChan — канал результатов от build/merge goroutines → coordinator.
	// Передаёт значение (не указатель) — happens-before через channel receive.
	resultChan chan compactionResult

	// mergeInFlight — флаги "идёт merge на уровне" (по одному merge на уровень одновременно).
	mergeInFlight [maxLevels]bool

	// lastMutation — UnixNano последней мутации данных (Add/Delete/Clear/LoadBinary).
	// Источник истины для idle-детекта консолидации (IdleConsolidateAfter).
	lastMutation atomic.Int64

	// consolidateInFlight — идёт idle-консолидация (merge ВСЕХ сегментов в один).
	// Взаимоисключение с обычными merge: maybeScheduleMerges пропускается, пока
	// флаг взведён — иначе одни и те же сегменты уехали бы в два параллельных
	// merge и оба результата опубликовались (дубли данных). Под lvs.mu.
	consolidateInFlight bool

	// deltaFlushInFlight — true пока delta-flush goroutine не вернула результат.
	// Только один delta-flush одновременно (один buildAllocators[0] → нет TCMalloc race).
	deltaFlushInFlight bool

	// snapshotTime — UnixNano момента последнего успешного LoadBinary (для диагностики).
	snapshotTime int64

	// snapshotLSN — LSN-watermark снапшота: «graph_leveled.bin покрывает все
	// векторные операции с LSN ≤ snapshotLSN». Пишется в заголовок при SaveBinary
	// (значение выставляется SetSnapshotWatermark перед сохранением), читается при
	// LoadBinary. Recovery пропускает реплей векторных WAL-записей с LSN ≤ этого
	// watermark — иначе операции из окна «ротация WAL → запись снапшота» попали бы
	// и в снапшот, и в новый WAL, дублируя векторы при каждом рестарте после
	// компакции. Дедуп поиска/merge (#1) добивает async-хвост (см. SetSnapshotWatermark).
	snapshotLSN uint64
}

// NewLeveledVectorStore создаёт хранилище и запускает Build Pool.
func NewLeveledVectorStore(cfg LeveledConfig) *LeveledVectorStore {
	if cfg.Fanout <= 0 {
		cfg.Fanout = defaultFanout
	}
	if cfg.MergeBatchMax <= 0 {
		cfg.MergeBatchMax = 8 * cfg.Fanout
	} else if cfg.MergeBatchMax < cfg.Fanout {
		cfg.MergeBatchMax = cfg.Fanout
	}
	if cfg.EfConstruction <= 0 {
		cfg.EfConstruction = 200
	}
	if cfg.EfSearch <= 0 {
		cfg.EfSearch = 100
	}
	if cfg.Distance == nil {
		cfg.Distance = EuclideanDistance
	}
	// Резолвим метрику ОДИН раз: при MetricAuto выводим из Distance (legacy-мост),
	// иначе синхронизируем Distance с явно заданной метрикой. После этого весь
	// SQ8-путь оперирует cfg.Metric как данными — без сравнения указателей.
	if cfg.Metric == MetricAuto {
		cfg.Metric = inferMetric(cfg.Distance)
	} else {
		cfg.Distance = cfg.Metric.DistanceFunc()
	}
	if cfg.Allocator == nil {
		cfg.Allocator = tcmalloc.NewTCMallocStore(1)
	}

	// NumBuilders: auto = NumCPU/2, clamped [2, 8].
	numBuilders := cfg.NumBuilders
	if numBuilders <= 0 {
		numBuilders = runtime.NumCPU() / 2
		if numBuilders < 2 {
			numBuilders = 2
		}
		if numBuilders > 8 {
			numBuilders = 8
		}
	}

	lvs := &LeveledVectorStore{
		cfg:         cfg,
		compactCh:   make(chan struct{}, 1),
		done:        make(chan struct{}),
		buildQueue:  make(chan *compactionTask, 32),
		resultChan:  make(chan compactionResult, 32),
		numBuilders: numBuilders,
		buildSem:    make(chan struct{}, numBuilders),
	}
	lvs.flushCond = sync.NewCond(&lvs.mu)
	// Точка отсчёта затишья — старт стора: после рестарта/LoadBinary консолидация
	// не стартует раньше полного окна IdleConsolidateAfter.
	lvs.lastMutation.Store(time.Now().UnixNano())

	// Per-worker изолированные аллокаторы для параллельных buildSegment.
	// TCMallocStore НЕ потокобезопасен через общий MCentral/MHeap — каждый
	// buildWorker получает свой аллокатор (свой MHeap/MCentral/MCache).
	lvs.buildAllocators = make([]*tcmalloc.TCMallocStore, numBuilders)
	for i := range lvs.buildAllocators {
		lvs.buildAllocators[i] = tcmalloc.NewTCMallocStore(1)
	}
	// Изолированный аллокатор для coordinator-driven merge (не пересекается с build workers).
	lvs.mergeAllocators = make([]*tcmalloc.TCMallocStore, maxLevels)
	for i := range lvs.mergeAllocators {
		lvs.mergeAllocators[i] = tcmalloc.NewTCMallocStore(1)
	}

	// Coordinator: 1 горутина, GRAB-only, кладёт tasks в buildQueue.
	lvs.coordWG.Add(1)
	go func() {
		defer lvs.coordWG.Done()
		lvs.compactWorker()
	}()

	// Build workers: N горутин, BUILD + PUBLISH параллельно.
	for i := 0; i < numBuilders; i++ {
		lvs.buildWG.Add(1)
		go func() {
			defer lvs.buildWG.Done()
			lvs.buildWorker()
		}()
	}

	return lvs
}

// deltaMax вычисляет максимальный размер дельты для заданной размерности.
// min(50_000, 100MB / (dim * 4 bytes)).
//
// 50 000 выбрано как баланс:
// - 500k векторов → всего 10 L0 сегментов (быстрый merge, быстрый search).
// - L1 merge = 4×50k = 200k (остаётся frozen: uint32 neighbor IDs, потолка нет).
// - Delta flush занимает ~5-10s (build HNSW на 50k при M=32/efC=400).
func deltaMax(dim, cfgMax int) int {
	if cfgMax > 0 {
		return cfgMax
	}
	if dim <= 0 {
		// Defense-in-depth: ValidateVector отсекает пустой вектор до сюда, но
		// deltaMax не должен паниковать делением на ноль ни при каком входе
		// (напр. реплей отравленного до фикса WAL). Безопасное значение.
		return 1
	}
	const maxBytes = 100 * 1024 * 1024 // 100MB лимит на дельту
	bySize := maxBytes / (dim * 4)
	if bySize > 50_000 {
		return 50_000
	}
	return bySize
}

// Add добавляет вектор с заданным ключом.
// O(1) на горячем пути (append в flat slab).
func (lvs *LeveledVectorStore) Add(key string, vec []float32) error {
	return lvs.AddWithAttrs(key, vec, Attributes{})
}

// AddWithAttrs вставляет вектор с атрибутами (колоночный слой uint/multi-attr).
// Атрибуты едут с данными через дельту и переживают flush/merge. Партиционный
// атрибут (cfg.PartitionAttr) задаёт тенант-раскладку. Add — обёртка без атрибутов.
func (lvs *LeveledVectorStore) AddWithAttrs(key string, vec []float32, attrs Attributes) error {
	return lvs.addEntry(key, vec, attrs, nil)
}

// addEntry — общий путь вставки для Add/AddWithAttrs/AddDoc: полная замена
// состояния дока (вектор + attrs + термы текста; terms=nil ⇒ текста нет).
func (lvs *LeveledVectorStore) addEntry(key string, vec []float32, attrs Attributes, terms []TermTF) error {
	// Choke-point персистентности: и Add, и реплей WAL (OpVSimAdd/OpVSimAddAttrs)
	// идут сюда. Санитизируем недоверенный вход до любых сайд-эффектов — ошибка
	// вместо паники (deltaMax dim=0), тихой потери (пустой ключ = tombstone) или
	// порчи снапшота (ключ > uint16) / SQ8-калибровки (NaN/Inf).
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ValidateVector(vec); err != nil {
		return err
	}
	// Горячий путь держит lvs.mu лишь как RLock: несколько писателей идут
	// параллельно, безопасность данных — на пер-шардовых локах дельты (шаг 5).
	// Эксклюзивный Lock берут только swap дельты (compaction) и Delete/Clear.
	lvs.mu.RLock()
	if lvs.delta == nil {
		// Первая вставка: инициализируем дельту под эксклюзивным локом.
		lvs.mu.RUnlock()
		if err := lvs.initDelta(len(vec)); err != nil {
			return err
		}
		lvs.mu.RLock()
	}
	if len(vec) != lvs.dim {
		lvs.mu.RUnlock()
		return &dimMismatchError{expected: lvs.dim, got: len(vec)}
	}

	becameFull := lvs.delta.AppendDoc(key, vec, attrs, terms)
	lvs.touchMutation()

	// Если ключ ранее был удалён (есть в tombstones) — снимаем tombstone,
	// иначе Delete(key) после Add(key,...) вновь удалит живой вектор.
	lvs.removeTombstone(key)
	lvs.mu.RUnlock()

	// Если дельта ТОЛЬКО ЧТО переполнилась (ровно maxSize слотов) — сигнализируем
	// coordinator. becameFull истинен ровно у одного писателя → один сигнал, без
	// busy-loop. Coordinator swap'нет delta при первой возможности.
	if becameFull {
		lvs.triggerCompact()
	}

	return nil
}

// initDelta лениво создаёт дельту при первой вставке (под эксклюзивным локом).
// Double-check: при гонке первых вставок второй вызов застанет delta != nil.
func (lvs *LeveledVectorStore) initDelta(dim int) error {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	return lvs.initDeltaLocked(dim)
}

// initDeltaLocked — тело initDelta для вызова под уже удерживаемым lvs.mu.Lock
// (supersedes-путь VMEM держит замок на весь read-modify-write).
func (lvs *LeveledVectorStore) initDeltaLocked(dim int) error {
	if lvs.delta != nil {
		if dim != lvs.dim {
			return &dimMismatchError{expected: lvs.dim, got: dim}
		}
		return nil
	}
	lvs.dim = dim
	max := deltaMax(dim, lvs.cfg.DeltaMax)
	lvs.delta = NewDeltaSegmentSharded(dim, max, lvs.cfg.Distance, lvs.cfg.M, lvs.cfg.EfConstruction, lvs.deltaShardCount())
	return nil
}

// deltaShardCount возвращает число шардов дельты (cfg.DeltaShards, >=1).
func (lvs *LeveledVectorStore) deltaShardCount() int {
	if lvs.cfg.DeltaShards > 1 {
		return lvs.cfg.DeltaShards
	}
	return 1
}

// removeTombstone снимает tombstone с ключа на горячем пути Add под RLock.
// COW-мутация сериализуется tombstoneMu (несколько писателей под RLock).
// Fast path: если tombstones пусто — ничего не делаем (zero-overhead).
func (lvs *LeveledVectorStore) removeTombstone(key string) {
	if t := lvs.tombstones.Load(); t == nil || len(*t) == 0 {
		return
	}
	lvs.tombstoneMu.Lock()
	deleteTombstone(&lvs.tombstones, key)
	lvs.tombstoneMu.Unlock()
}

// Delete помечает ключ как удалённый.
func (lvs *LeveledVectorStore) Delete(key string) bool {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	return lvs.deleteLocked(key)
}

// deleteLocked — тело Delete под уже взятым lvs.mu.Lock: жнец TTL (шаг 6 VMEM)
// обязан перепроверить приговор и удалить атомарно под одним замком, иначе
// между перепроверкой и Delete вклинился бы параллельный Remember.
func (lvs *LeveledVectorStore) deleteLocked(key string) bool {
	deleted, needTomb := lvs.deleteClassifyLocked(key)
	if needTomb {
		addTombstone(&lvs.tombstones, key)
	}
	return deleted
}

// deleteClassifyLocked — логика Delete БЕЗ вставки в tombstone-карту: решает
// судьбу ключа, а нужный tombstone отдаёт флагом. Единственная причина
// расщепления — COW карты: одиночный Delete платит копию за ключ (редкая
// операция), а массовая жатва (reapKeys) обязана батчировать — одна копия на
// батч, иначе O(n²) под write-замком (порог 3 шага 6 ловил именно это).
func (lvs *LeveledVectorStore) deleteClassifyLocked(key string) (deleted, needTomb bool) {
	lvs.touchMutation()

	// Если в дельте — hard delete (вектор физически исчезает из дельты).
	if lvs.delta != nil && lvs.delta.Delete(key) {
		// Но тот же ключ мог жить и в сегменте, и в сфлашиваемой дельте (upsert
		// через flush: Add→flush→Add оставляет старую копию, новую — в дельте).
		// Тогда ставим tombstone на неё, иначе она воскреснет при чтении/merge.
		// Иначе — снимаем возможный прежний tombstone.
		if lvs.keyInFlushingLocked(key) || lvs.keyPhysicallyInSegmentsLocked(key) {
			return true, true
		}
		deleteTombstone(&lvs.tombstones, key)
		return true, false
	}

	// Не в активной дельте. Уже tombstoned → уже удалён.
	if t := lvs.tombstones.Load(); t != nil {
		if _, ok := (*t)[key]; ok {
			return false, false
		}
	}
	// Ключ мог быть в сфлашиваемой дельте (flush-окно) ИЛИ в сегменте. В обоих
	// случаях ставим tombstone: сфлашиваемую дельту мутировать НЕЛЬЗЯ (её читает
	// build-горутина), а tombstone замаскирует ключ и в получившемся сегменте.
	if !lvs.keyInFlushingLocked(key) && !lvs.keyPhysicallyInSegmentsLocked(key) {
		return false, false
	}
	monitoring.VectorTombstoneTotal.Inc()
	return true, true
}

// keyInFlushingLocked — есть ли живой ключ в какой-либо сфлашиваемой дельте
// (flush-окно между swap дельты и публикацией сегмента). Только чтение
// (Contains); мутировать flushing-дельту нельзя — её читает build-горутина.
// Под lvs.mu.
func (lvs *LeveledVectorStore) keyInFlushingLocked(key string) bool {
	for _, fd := range lvs.flushing {
		if fd.Contains(key) {
			return true
		}
	}
	return false
}

// keyPhysicallyInSegmentsLocked — есть ли ФИЗИЧЕСКАЯ копия ключа в каком-либо
// сегменте, ИГНОРИРУЯ tombstones. Нужна для решений о самом tombstone (ставить/
// снимать) — там учёт tombstone был бы циклом. Под lvs.mu.
func (lvs *LeveledVectorStore) keyPhysicallyInSegmentsLocked(key string) bool {
	for _, level := range lvs.levels {
		for _, seg := range level {
			if seg.HasKey(key) {
				return true
			}
		}
	}
	return false
}

// keyPhysicallyInOtherSegmentsLocked — как keyPhysicallyInSegmentsLocked, но
// пропускает exclude (только что построенный merge-сегмент: он по определению
// исключил применённые tombstone-ключи, скан по нему бесполезен и дорог). Под mu.
func (lvs *LeveledVectorStore) keyPhysicallyInOtherSegmentsLocked(key string, exclude segment) bool {
	for _, level := range lvs.levels {
		for _, seg := range level {
			if seg == exclude {
				continue
			}
			if seg.HasKey(key) {
				return true
			}
		}
	}
	return false
}

// clearAppliedTombstonesLocked чистит tombstones ключей, физически исчезнувших из
// индекса после merge. Кандидаты — только ключи, исключённые (применённые) в ЭТОМ
// merge; ключ чистится ⟺ его больше нет ни в одном другом живом сегменте (newSeg
// его исключил, oldSegs уже удалены свапом; копия могла остаться в не-мёрдженном
// сегменте другого уровня — тогда tombstone обязан жить). Под lvs.mu (write).
//
// Исправляет два бага прежнего in-merge-clearing: (#1) чистились ВСЕ ключи
// глобального снапшота ts, включая живущие вне oldSegs → воскрешение; (#3) чистка
// шла в merge-горутине ДО публикации — при дропе результата (epoch-mismatch/
// rollback) свапа нет, этот метод не зовётся, tombstone сохраняется.
func (lvs *LeveledVectorStore) clearAppliedTombstonesLocked(applied map[string]struct{}, newSeg segment) {
	if len(applied) == 0 {
		return
	}
	if lvs.tombstones.Load() == nil {
		return
	}
	var toClear []string
	for key := range applied {
		if !lvs.keyPhysicallyInOtherSegmentsLocked(key, newSeg) {
			toClear = append(toClear, key)
		}
	}
	deleteTombstonesBatch(&lvs.tombstones, toClear)
}

// addTombstone: COW-добавление ключа. Вызывать под lvs.mu (write).
// Дедуп: если ключ уже есть — no-op (без аллокации).
func addTombstone(p *atomic.Pointer[map[string]struct{}], key string) {
	old := p.Load()
	if old != nil {
		if _, exists := (*old)[key]; exists {
			return
		}
		nw := make(map[string]struct{}, len(*old)+1)
		for k, v := range *old {
			nw[k] = v
		}
		nw[key] = struct{}{}
		p.Store(&nw)
		return
	}
	nw := map[string]struct{}{key: {}}
	p.Store(&nw)
}

// addTombstones: батчевое COW-добавление — ОДНА копия карты на весь батч.
// Вызывать под lvs.mu (write). Массовое удаление (TTL-жнец) с per-key COW
// стоило бы O(n²) переносов записей под write-замком.
func addTombstones(p *atomic.Pointer[map[string]struct{}], keys []string) {
	if len(keys) == 0 {
		return
	}
	old := p.Load()
	nOld := 0
	if old != nil {
		nOld = len(*old)
	}
	nw := make(map[string]struct{}, nOld+len(keys))
	if old != nil {
		for k, v := range *old {
			nw[k] = v
		}
	}
	for _, k := range keys {
		nw[k] = struct{}{}
	}
	p.Store(&nw)
}

// deleteTombstone: COW-удаление ключа. Вызывать под lvs.mu (write).
// Когда множество пустеет — Store(nil), возвращая zero-overhead-состояние.
func deleteTombstone(p *atomic.Pointer[map[string]struct{}], key string) {
	cur := p.Load()
	if cur == nil {
		return
	}
	if _, ok := (*cur)[key]; !ok {
		return
	}
	if len(*cur) == 1 {
		p.Store(nil)
		return
	}
	nw := make(map[string]struct{}, len(*cur)-1)
	for k, v := range *cur {
		if k != key {
			nw[k] = v
		}
	}
	p.Store(&nw)
}

// deleteTombstonesBatch — COW-удаление набора ключей одним rebuild (без O(k²)
// пересборки на каждый ключ). Удаляет лишь присутствующие. Вызывать под lvs.mu.
func deleteTombstonesBatch(p *atomic.Pointer[map[string]struct{}], keys []string) {
	if len(keys) == 0 {
		return
	}
	cur := p.Load()
	if cur == nil {
		return
	}
	rm := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if _, ok := (*cur)[k]; ok {
			rm[k] = struct{}{}
		}
	}
	if len(rm) == 0 {
		return
	}
	if len(rm) == len(*cur) {
		p.Store(nil)
		return
	}
	nw := make(map[string]struct{}, len(*cur)-len(rm))
	for k, v := range *cur {
		if _, drop := rm[k]; !drop {
			nw[k] = v
		}
	}
	p.Store(&nw)
}

// Clear сбрасывает всё хранилище в пустое состояние.
func (lvs *LeveledVectorStore) Clear() {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()

	// Bump эпохи ПОД локом: инвариант «epoch сменился ⇒ levels уже занулён».
	// Compactor, державший старую эпоху в долгом merge вне lock, увидит новый
	// epoch при последующем Lock и выбросит устаревший сегмент.
	lvs.compactionEpoch.Add(1)

	lvs.delta = nil
	lvs.dim = 0
	for i := range lvs.levels {
		lvs.levels[i] = nil
	}
	// Снимаем сфлашиваемые дельты с searchable-списка: их эпоха устарела. Сам Close
	// НЕ здесь — in-flight build/freeze-горутины ещё читают эти дельты; их закроет
	// applyResult по epoch-mismatch, когда горутина завершится (нет UAF, нет утечки).
	lvs.flushing = nil
	lvs.tombstones.Store(nil)
	lvs.touchMutation()
}

// touchMutation отмечает момент последней мутации данных (idle-детект консолидации).
func (lvs *LeveledVectorStore) touchMutation() {
	lvs.lastMutation.Store(time.Now().UnixNano())
}

// FlushDelta принудительно флашит дельту (async — для graceful shutdown).
func (lvs *LeveledVectorStore) FlushDelta() {
	lvs.mu.Lock()
	if lvs.delta == nil || lvs.delta.Len() == 0 {
		lvs.mu.Unlock()
		return
	}
	lvs.triggerCompact()
	lvs.mu.Unlock()
}

// FlushDeltaSync — см. реализацию ниже (Build Pool версия с cond-ожиданием).

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
	// вскрытая VMEM-оракулом). История: здесь был ПОЛНЫЙ снимок AttrsSnapshot
	// всех дельт на КАЖДЫЙ запрос — O(|delta|) map-аллокация и конвой на
	// runtime-локах аллокатора под конкуренцией (E2E vmemload 23.07,
	// mutex-профиль: 88% в maps.newTable); затем freshest-лукап GetAttrs —
	// дедлок рекурсивного RLock из-под шардового замка. Стейл-копию, прошедшую
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

// =============================================================================
// Дополнительные методы VectorIndex
// =============================================================================

// ForEach итерирует по всем живым векторам (delta + все сегменты).
// Tombstoned ключи пропускаются. fn вызывается без блокировки lvs.mu.
func (lvs *LeveledVectorStore) ForEach(fn func(key string, vec []float32)) {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	// Snapshot tombstones под локом
	var ts map[string]struct{}
	if t := lvs.tombstones.Load(); t != nil {
		ts = *t
	}

	// Дедуп upsert-копий: ключ, переписанный после flush, живёт в 2+ источниках
	// (delta + сегмент, либо два сегмента). Обходим источники СВЕЖЕЕ-ПЕРВЫМ
	// (зеркально провенанс-рангу search) и отдаём только первую встреченную
	// копию — иначе потребитель (репликация full-sync) применил бы новую версию,
	// а затем перезатёр её устаревшей. seen под RLock: view-строки frozen-арен
	// живы, пока живы сегменты. Память O(живых ключей) — ForEach и так O(N).
	seen := make(map[string]struct{}, 256)
	shadowed := func(key string) bool {
		if _, dup := seen[key]; dup {
			return true
		}
		seen[key] = struct{}{}
		return false
	}

	// Delta (tombstones дельты обработаны внутри; пер-шардовые RLock)
	if lvs.delta != nil {
		lvs.delta.ForEachLive(func(key string, vec []float32) {
			if !shadowed(key) {
				fn(key, vec)
			}
		})
	}

	// Сфлашиваемые дельты (flush-окно): в обратном порядке (позже добавленная
	// свежее); пропускаем tombstoned (Delete в окне ставит tombstone, не мутируя
	// дельту → живая копия там ещё есть).
	for fi := len(lvs.flushing) - 1; fi >= 0; fi-- {
		lvs.flushing[fi].ForEachLive(func(key string, vec []float32) {
			if ts != nil {
				if _, deleted := ts[key]; deleted {
					return
				}
			}
			if !shadowed(key) {
				fn(key, vec)
			}
		})
	}

	// Segments свежее-первым (уровень 0 → max, внутри уровня с конца):
	// пропускаем tombstoned и затенённые ключи.
	for _, level := range lvs.levels {
		for pos := len(level) - 1; pos >= 0; pos-- {
			switch s := level[pos].(type) {
			case *frozenSegment:
				fg := s.fg
				for i := 0; i < fg.n; i++ {
					if fg.keys.view(i) == "" {
						continue
					}
					if ts != nil {
						if _, deleted := ts[fg.keys.view(i)]; deleted {
							continue
						}
					}
					if shadowed(fg.keys.view(i)) {
						continue
					}
					fn(fg.keys.clone(i), fg.data[i*fg.dim:(i+1)*fg.dim])
				}
			case *frozenSQSegment:
				fg := s.fg
				dim := fg.dim
				vecBuf := make([]float32, dim)
				for i := 0; i < fg.n; i++ {
					if fg.keys.view(i) == "" {
						continue
					}
					if ts != nil {
						if _, deleted := ts[fg.keys.view(i)]; deleted {
							continue
						}
					}
					if shadowed(fg.keys.view(i)) {
						continue
					}
					// Деквантование int8 → float32
					base := i * dim
					for d := 0; d < dim; d++ {
						vecBuf[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]
					}
					fn(fg.keys.clone(i), vecBuf)
				}
			case *hnswSegment:
				s.mu.RLock()
				for id, key := range s.keys {
					if key == "" {
						continue
					}
					if ts != nil {
						if _, deleted := ts[key]; deleted {
							continue
						}
					}
					if shadowed(key) {
						continue
					}
					node := &s.g.nodes[id]
					if node.Alive {
						fn(key, s.g.arena.Get(node.VectorOffset))
					}
				}
				s.mu.RUnlock()
			}
		}
	}
}

// Get возвращает копию вектора по ключу.
// Сначала ищет в дельте (O(1)), затем проверяет tombstones, затем сканирует сегменты.
func (lvs *LeveledVectorStore) Get(key string) ([]float32, bool) {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	// Дельта: O(1) через keyIdx map шарда — tombstones дельты обработаны внутри.
	if lvs.delta != nil {
		if out, ok := lvs.delta.Get(key); ok {
			return out, true
		}
	}

	// Tombstone check: ключ мог быть удалён из сегмента после последнего merge.
	// Проверяем ДО flushing/сегментов: Delete в flush-окне ставит tombstone, не
	// мутируя сфлашиваемую дельту (её читает build), поэтому живая копия там ещё
	// есть — tombstone обязан её замаскировать.
	if t := lvs.tombstones.Load(); t != nil {
		if _, deleted := (*t)[key]; deleted {
			return nil, false
		}
	}

	// Сфлашиваемые дельты (flush-окно между swap и публикацией сегмента): новее
	// сегментов, только чтение. Иначе Get промахивается по ключу в этом окне.
	// В обратном порядке — позже добавленная flushing-дельта свежее (провенанс,
	// зеркально memtable-порядку search): upsert-копии затеняются правильно.
	for i := len(lvs.flushing) - 1; i >= 0; i-- {
		if out, ok := lvs.flushing[i].Get(key); ok {
			return out, true
		}
	}

	// Сканирование сегментов СВЕЖЕЕ-ПЕРВЫМ, зеркально провенанс-рангу search
	// (rank = lvl<<40 - pos): уровень 0 свежее уровня 1, внутри уровня больший
	// индекс свежее. Иначе после upsert-через-flush (две frozen-копии ключа)
	// Get возвращал бы устаревшую копию из более раннего сегмента.
	for _, level := range lvs.levels {
		for pos := len(level) - 1; pos >= 0; pos-- {
			seg := level[pos]
			switch s := seg.(type) {
			case *frozenSegment:
				fg := s.fg
				if i, ok := fg.keys.find(key); ok {
					out := make([]float32, fg.dim)
					copy(out, fg.data[i*fg.dim:(i+1)*fg.dim])
					return out, true
				}
			case *frozenSQSegment:
				fg := s.fg
				dim := fg.dim
				if i, ok := fg.keys.find(key); ok {
					out := make([]float32, dim)
					base := i * dim
					for d := 0; d < dim; d++ {
						out[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]
					}
					return out, true
				}
			case *hnswSegment:
				s.mu.RLock()
				for id, k := range s.keys {
					if k == key {
						node := &s.g.nodes[id]
						if node.Alive {
							raw := s.g.arena.Get(node.VectorOffset)
							out := make([]float32, len(raw))
							copy(out, raw)
							s.mu.RUnlock()
							return out, true
						}
					}
				}
				s.mu.RUnlock()
			}
		}
	}
	return nil, false
}

// Info возвращает общее кол-во векторов, размерность и номер максимального заполненного уровня.
func (lvs *LeveledVectorStore) Info() (count, dim, maxLevel int) {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	count = 0
	if lvs.delta != nil {
		count += lvs.delta.Len()
	}
	// Сфлашиваемые дельты (flush-окно): их векторы ещё не в сегментах — без них
	// count недосчитывал бы в этом окне (тот же фикс, что в Len).
	for _, fd := range lvs.flushing {
		count += fd.Len()
	}
	maxLevel = 0
	for i, level := range lvs.levels {
		for _, seg := range level {
			count += seg.Len()
			if i > maxLevel {
				maxLevel = i
			}
		}
	}
	// Tombstones маскируют удалённые ключи, физически ещё живущие в сегментах:
	// без вычета Delete не уменьшал бы count до компакции (юзер-видимо через
	// VSIM.INFO). Вычет точен для ключей с одной физической копией; ключ,
	// апсерченный через flush (2+ копии) и затем удалённый, до merge даёт
	// временный overcount — count никогда не ЗАнижается. Clamp — страховка
	// инварианта «tombstone ⇒ есть физическая копия».
	if t := lvs.tombstones.Load(); t != nil {
		count -= len(*t)
		if count < 0 {
			count = 0
		}
	}
	return count, lvs.dim, maxLevel
}

// SetHNSWParams обновляет параметры HNSW — применяются к следующим buildSegment.
func (lvs *LeveledVectorStore) SetHNSWParams(M, efConstruction, efSearch int) {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	if M > 0 {
		lvs.cfg.M = M
	}
	if efConstruction > 0 {
		lvs.cfg.EfConstruction = efConstruction
	}
	if efSearch > 0 {
		lvs.cfg.EfSearch = efSearch
	}
}

// SetUseLSH — no-op для LeveledVectorStore (LSH не используется в LSM-архитектуре).
func (lvs *LeveledVectorStore) SetUseLSH(_ bool) {}

// Len возвращает общее число векторов (delta + все сегменты).
func (lvs *LeveledVectorStore) Len() int {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	total := 0
	if lvs.delta != nil {
		total += lvs.delta.Len()
	}
	// Сфлашиваемые дельты (flush-окно): их векторы ещё не в сегментах — без учёта
	// Len недосчитывал бы в этом окне.
	for _, fd := range lvs.flushing {
		total += fd.Len()
	}
	for _, level := range lvs.levels {
		for _, seg := range level {
			total += seg.Len()
		}
	}
	return total
}

// SnapshotTime возвращает UnixNano времени последнего успешного LoadBinary (для диагностики).
func (lvs *LeveledVectorStore) SnapshotTime() int64 { return lvs.snapshotTime }

// SnapshotLSN возвращает LSN-watermark, прочитанный последним LoadBinary (0 если
// снапшот без watermark / не загружался). Recovery пропускает векторные WAL-записи
// с LSN ≤ этого значения — они уже в снапшоте.
func (lvs *LeveledVectorStore) SnapshotLSN() uint64 { return lvs.snapshotLSN }

// SetSnapshotWatermark выставляет LSN-watermark, записываемый следующим SaveBinary.
//
// Вызывать ПЕРЕД SaveBinary со значением LastLSN() на момент НАЧАЛА сохранения
// (до FlushDeltaSync). Контракт безопасности: любая векторная операция с
// LSN ≤ watermark получила свой LSN до этого момента, значит вектор уже был в
// дельте/сегментах и попадёт в снапшот через FlushDeltaSync → пропуск такой
// записи при реплее не теряет данных. Векторы, чей WAL-LSN присвоился позже
// (async batch-flusher), но которые всё же попали в снапшот, реплеятся повторно
// и схлопываются дедупом поиска/merge (#1) — корректность сохраняется, лишь
// возможен временный дубль в памяти до ближайшего merge.
func (lvs *LeveledVectorStore) SetSnapshotWatermark(lsn uint64) {
	lvs.mu.Lock()
	lvs.snapshotLSN = lsn
	lvs.mu.Unlock()
}

// Close останавливает coordinator и build workers, дожидаясь завершения.
// Idempotent (sync.Once) — безопасен для многократного вызова.
func (lvs *LeveledVectorStore) Close() {
	lvs.closeOnce.Do(func() {
		close(lvs.done)
		close(lvs.buildQueue) // build workers дорабатывают in-flight tasks и выходят
		lvs.coordWG.Wait()    // coordinator вышел
		lvs.buildWG.Wait()    // все build workers вышли

		// Пробудить зависший в FlushDeltaSync cond.Wait, если он ждёт.
		lvs.mu.Lock()
		lvs.flushCond.Broadcast()
		lvs.mu.Unlock()

		// Останавливаем фоновые горутины runDeferredFree в per-worker аллокаторах.
		for _, a := range lvs.buildAllocators {
			a.Close()
		}
		for _, a := range lvs.mergeAllocators {
			a.Close()
		}
	})
}

// =============================================================================
// Persistence: SaveBinary / LoadBinary
//
// Формат (little-endian):
//   [8B magic]
//   [8B snapshotTime int64 UnixNano]
//   [4B dim uint32]
//   [1B maxLevels uint8 — всегда 8]
//   для каждого уровня [0..maxLevels-1]:
//     [4B nSegs uint32]
//     для каждого сегмента:
//       [1B segType: 0=FROZEN, 1=HNSW_FLAT]
//       if FROZEN: fg.WriteTo(w)
//       if HNSW_FLAT:
//         [4B nVecs uint32]
//         для каждого вектора:
//           [2B keyLen uint16][keyLen B key][dim*4 B vec float32]
//
// Delta НЕ сохраняется (она пуста после FlushDeltaSync).
// Новые Add() после FlushDeltaSync попадают в WAL и восстанавливаются при старте.
// =============================================================================

// SaveBinary сохраняет все скомпакченные сегменты.
// ВАЖНО: вызывать после FlushDeltaSync() для гарантии полного снапшота.
func (lvs *LeveledVectorStore) SaveBinary(w io.Writer) error {
	// Снапшотируем указатели сегментов под RLock (дёшево — только указатели).
	// Сами данные сегментов иммутабельны (frozenSegment) или защищены своим мьютексом (hnswSegment).
	lvs.mu.RLock()
	dim := lvs.dim
	var snapLevels [maxLevels][]segment
	for i, level := range lvs.levels {
		if len(level) > 0 {
			snapLevels[i] = make([]segment, len(level))
			copy(snapLevels[i], level)
		}
	}
	// Снимаем tombstone-срез под тем же RLock — он консистентен с указателями
	// сегментов (множество удалённых ⊆ актуальному набору сегментов).
	tombSnap := lvs.tombstones.Load()
	lvs.mu.RUnlock()

	// CRC32 (v5+): весь payload пишется и в w, и в hasher; 4-байтный чексум
	// дописывается в КОНЕЦ (в out, не в hasher) и проверяется при загрузке.
	// Защита от тихого bit-rot в graph_leveled.bin (P0-3).
	out := w
	hasher := crc32.NewIEEE()
	w = io.MultiWriter(out, hasher)

	// Пишем без блокировки (frozenSegment иммутабелен, hnswSegment использует свой мьютекс)
	if _, err := w.Write(leveledMagic[:]); err != nil {
		return fmt.Errorf("leveled: write magic: %w", err)
	}

	var u8 [8]byte
	binary.LittleEndian.PutUint64(u8[:], uint64(time.Now().UnixNano()))
	if _, err := w.Write(u8[:]); err != nil {
		return fmt.Errorf("leveled: write snapshotTime: %w", err)
	}

	// LSN-watermark (v6): «снапшот покрывает векторные операции до этого LSN».
	// Значение выставлено SetSnapshotWatermark перед вызовом (под RLock снимаем).
	lvs.mu.RLock()
	wmLSN := lvs.snapshotLSN
	lvs.mu.RUnlock()
	binary.LittleEndian.PutUint64(u8[:], wmLSN)
	if _, err := w.Write(u8[:]); err != nil {
		return fmt.Errorf("leveled: write snapshotLSN: %w", err)
	}

	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], uint32(dim))
	if _, err := w.Write(u4[:]); err != nil {
		return fmt.Errorf("leveled: write dim: %w", err)
	}

	if _, err := w.Write([]byte{maxLevels}); err != nil {
		return fmt.Errorf("leveled: write nLevels: %w", err)
	}

	for lyr, level := range snapLevels {
		binary.LittleEndian.PutUint32(u4[:], uint32(len(level)))
		if _, err := w.Write(u4[:]); err != nil {
			return fmt.Errorf("leveled: write nSegs[%d]: %w", lyr, err)
		}
		for si, seg := range level {
			switch s := seg.(type) {
			case *frozenSegment:
				if _, err := w.Write([]byte{segTypeFrozen}); err != nil {
					return fmt.Errorf("leveled: write segType[%d][%d]: %w", lyr, si, err)
				}
				if err := s.fg.WriteGraphTo(w); err != nil {
					return fmt.Errorf("leveled: write frozen[%d][%d]: %w", lyr, si, err)
				}
				if err := writeSegMeta(w, s.cat, s.attrs); err != nil {
					return fmt.Errorf("leveled: write frozenMeta[%d][%d]: %w", lyr, si, err)
				}
				if err := writeSegText(w, s.text); err != nil {
					return fmt.Errorf("leveled: write frozenText[%d][%d]: %w", lyr, si, err)
				}

			case *frozenSQSegment:
				if _, err := w.Write([]byte{segTypeFrozenSQ8}); err != nil {
					return fmt.Errorf("leveled: write segType[%d][%d]: %w", lyr, si, err)
				}
				if err := s.fg.WriteGraphToSQ(w); err != nil {
					return fmt.Errorf("leveled: write frozenSQ[%d][%d]: %w", lyr, si, err)
				}
				if err := writeSegMeta(w, s.cat, s.attrs); err != nil {
					return fmt.Errorf("leveled: write frozenSQMeta[%d][%d]: %w", lyr, si, err)
				}
				if err := writeSegText(w, s.text); err != nil {
					return fmt.Errorf("leveled: write frozenSQText[%d][%d]: %w", lyr, si, err)
				}

			case *hnswSegment:
				if _, err := w.Write([]byte{segTypeHNSWFlat}); err != nil {
					return fmt.Errorf("leveled: write segType[%d][%d]: %w", lyr, si, err)
				}
				s.mu.RLock()
				// Считаем живые векторы
				nVecs := 0
				for id, key := range s.keys {
					if key != "" && s.g.nodes[id].Alive {
						nVecs++
					}
				}
				binary.LittleEndian.PutUint32(u4[:], uint32(nVecs))
				if _, err := w.Write(u4[:]); err != nil {
					s.mu.RUnlock()
					return fmt.Errorf("leveled: write nVecs[%d][%d]: %w", lyr, si, err)
				}
				// Термы по вектору (v7): node id = frozen id текстового слоя →
				// декодим постинги в пер-доковые списки один раз на сегмент.
				// На load buildSegment регенерирует segmentText из entries[].Terms
				// (как колонки из Attrs). decTerms==nil (текстов нет) → nTerms=0.
				decTerms := s.text.decodeTerms()
				var klen [2]byte
				var writeErr error
				for id, key := range s.keys {
					if key == "" || !s.g.nodes[id].Alive {
						continue
					}
					binary.LittleEndian.PutUint16(klen[:], uint16(len(key)))
					if _, err := w.Write(klen[:]); err != nil {
						writeErr = err
						break
					}
					if _, err := io.WriteString(w, key); err != nil {
						writeErr = err
						break
					}
					vec := s.g.arena.Get(s.g.nodes[id].VectorOffset)
					if len(vec) > 0 {
						b := unsafe.Slice((*byte)(unsafe.Pointer(&vec[0])), len(vec)*4)
						if _, err := w.Write(b); err != nil {
							writeErr = err
							break
						}
					}
					// Атрибуты вектора (v3): node id = индекс колонки → decodeAt(id).
					// hnsw-сегмент на load перестраивается buildSegment(entries), который
					// регенерирует колонки+каталог из entries[].Attrs (см. readAttrs ниже).
					if err := writeAttrs(w, s.attrs.decodeAt(int(id))); err != nil {
						writeErr = err
						break
					}
					var entryTerms []TermTF
					if int(id) < len(decTerms) {
						entryTerms = decTerms[id]
					}
					if err := writeTerms(w, entryTerms); err != nil {
						writeErr = err
						break
					}
				}
				s.mu.RUnlock()
				if writeErr != nil {
					return fmt.Errorf("leveled: write hnswFlat[%d][%d]: %w", lyr, si, writeErr)
				}
			}
		}
	}

	// Tombstones (v4+): ключи, удалённые из иммутабельных сегментов, но ещё не
	// вырезанные физически (это делает merge). Без их персиста Delete не переживает
	// рестарт — вектор остаётся в данных сегмента и «воскресает» (см. P0-1).
	var tcount uint32
	if tombSnap != nil {
		tcount = uint32(len(*tombSnap))
	}
	binary.LittleEndian.PutUint32(u4[:], tcount)
	if _, err := w.Write(u4[:]); err != nil {
		return fmt.Errorf("leveled: write nTombstones: %w", err)
	}
	if tombSnap != nil {
		var tklen [2]byte
		for key := range *tombSnap {
			binary.LittleEndian.PutUint16(tklen[:], uint16(len(key)))
			if _, err := w.Write(tklen[:]); err != nil {
				return fmt.Errorf("leveled: write tombstoneKeyLen: %w", err)
			}
			if _, err := io.WriteString(w, key); err != nil {
				return fmt.Errorf("leveled: write tombstoneKey: %w", err)
			}
		}
	}

	// CRC32-трейлер (v5+): чексум всего payload выше. Пишется в out (НЕ в hasher).
	binary.LittleEndian.PutUint32(u4[:], hasher.Sum32())
	if _, err := out.Write(u4[:]); err != nil {
		return fmt.Errorf("leveled: write crc: %w", err)
	}

	return nil
}

// LoadBinary восстанавливает состояние из ридера.
// После загрузки delta пуста — новые Add() идут туда, WAL replay добавит недостающие.
//
// Всё-или-ничего: payload целиком читается в локальные переменные и сверяется
// CRC, и только потом коммитится в lvs. При любой ошибке (bit-rot, усечённый
// файл) стор остаётся ровно в прежнем состоянии — иначе recovery реплеил бы WAL
// поверх наполовину загруженных сегментов (тихо-неконсистентная база).
func (lvs *LeveledVectorStore) LoadBinary(r io.Reader) error {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()

	// CRC32 (v5+): весь payload читаем через tee → hasher; 4-байтный трейлер
	// читаем отдельно из crcSource (НЕ через tee) и сверяем. crcSource держит
	// оригинальный ридер, r переключаем на tee — все ReadFull ниже идут через него.
	crcSource := r
	hasher := crc32.NewIEEE()
	r = io.TeeReader(r, hasher)

	// Проверяем магию: префикс 'LVLV' обязателен, версия в байте [5].
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("leveled: read magic: %w", err)
	}
	if [4]byte(magic[:4]) != leveledMagicPrefix {
		return fmt.Errorf("leveled: invalid magic header (not a graph_leveled.bin?)")
	}
	version := magic[5]
	if version < 1 || version > 7 {
		return fmt.Errorf("leveled: unsupported snapshot version %d", version)
	}

	// ── Фаза чтения: только локальные переменные, lvs не трогаем до коммита. ──

	// Читаем snapshotTime
	var u8 [8]byte
	if _, err := io.ReadFull(r, u8[:]); err != nil {
		return fmt.Errorf("leveled: read snapshotTime: %w", err)
	}
	snapTime := int64(binary.LittleEndian.Uint64(u8[:]))

	// LSN-watermark (v6+): recovery пропустит векторные WAL-записи с LSN ≤ этого.
	// Старые снапшоты (v≤5) без поля → watermark 0 (реплей всех записей, как раньше;
	// дедуп #1 удержит корректность).
	var snapLSN uint64
	if version >= 6 {
		if _, err := io.ReadFull(r, u8[:]); err != nil {
			return fmt.Errorf("leveled: read snapshotLSN: %w", err)
		}
		snapLSN = binary.LittleEndian.Uint64(u8[:])
	}

	// Читаем dim
	var u4 [4]byte
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return fmt.Errorf("leveled: read dim: %w", err)
	}
	dim := int(binary.LittleEndian.Uint32(u4[:]))

	// Читаем nLevels
	var nLevelsBuf [1]byte
	if _, err := io.ReadFull(r, nLevelsBuf[:]); err != nil {
		return fmt.Errorf("leveled: read nLevels: %w", err)
	}
	nLevels := int(nLevelsBuf[0])
	if nLevels > maxLevels {
		return fmt.Errorf("leveled: nLevels=%d превышает maxLevels=%d (повреждён заголовок?)", nLevels, maxLevels)
	}

	// stagedSeg — сегмент, прочитанный, но ещё не закоммиченный в lvs.levels.
	// frozen/frozenSQ читаются сразу (обычная Go-память — при abort подберёт GC);
	// hnsw откладывается сырыми entries: buildSegment аллоцирует из tcmalloc-арен,
	// строим его только после успешного CRC (заодно не жжём CPU на битом файле).
	type stagedSeg struct {
		seg     segment
		entries []DeltaEntry
	}
	var staged [maxLevels][]stagedSeg

	// Восстанавливаем каждый уровень
	for lyr := 0; lyr < nLevels; lyr++ {
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return fmt.Errorf("leveled: read nSegs[%d]: %w", lyr, err)
		}
		nSegs := int(binary.LittleEndian.Uint32(u4[:]))
		if nSegs == 0 {
			continue
		}

		for si := 0; si < nSegs; si++ {
			var stBuf [1]byte
			if _, err := io.ReadFull(r, stBuf[:]); err != nil {
				return fmt.Errorf("leveled: read segType[%d][%d]: %w", lyr, si, err)
			}
			segType := stBuf[0]

			switch segType {
			case segTypeFrozen:
				fg, err := ReadFrozenGraph(r, lvs.cfg.Distance)
				if err != nil {
					return fmt.Errorf("leveled: read frozen[%d][%d]: %w", lyr, si, err)
				}
				seg := &frozenSegment{fg: fg}
				if version >= 2 {
					if seg.cat, seg.attrs, err = readSegMeta(r); err != nil {
						return fmt.Errorf("leveled: read frozenMeta[%d][%d]: %w", lyr, si, err)
					}
				}
				if version >= 7 {
					if seg.text, err = readSegText(r); err != nil {
						return fmt.Errorf("leveled: read frozenText[%d][%d]: %w", lyr, si, err)
					}
				}
				staged[lyr] = append(staged[lyr], stagedSeg{seg: seg})

			case segTypeFrozenSQ8:
				fg, err := ReadFrozenGraphSQ(r, lvs.cfg.Metric)
				if err != nil {
					return fmt.Errorf("leveled: read frozenSQ[%d][%d]: %w", lyr, si, err)
				}
				seg := &frozenSQSegment{fg: fg}
				if version >= 2 {
					if seg.cat, seg.attrs, err = readSegMeta(r); err != nil {
						return fmt.Errorf("leveled: read frozenSQMeta[%d][%d]: %w", lyr, si, err)
					}
				}
				if version >= 7 {
					if seg.text, err = readSegText(r); err != nil {
						return fmt.Errorf("leveled: read frozenSQText[%d][%d]: %w", lyr, si, err)
					}
				}
				staged[lyr] = append(staged[lyr], stagedSeg{seg: seg})

			case segTypeHNSWFlat:
				if _, err := io.ReadFull(r, u4[:]); err != nil {
					return fmt.Errorf("leveled: read nVecs[%d][%d]: %w", lyr, si, err)
				}
				nVecs := int(binary.LittleEndian.Uint32(u4[:]))

				entries := make([]DeltaEntry, 0, nVecs)
				var klen [2]byte
				for vi := 0; vi < nVecs; vi++ {
					if _, err := io.ReadFull(r, klen[:]); err != nil {
						return fmt.Errorf("leveled: read keyLen[%d][%d][%d]: %w", lyr, si, vi, err)
					}
					kl := int(binary.LittleEndian.Uint16(klen[:]))
					key := ""
					if kl > 0 {
						kb := make([]byte, kl)
						if _, err := io.ReadFull(r, kb); err != nil {
							return fmt.Errorf("leveled: read key[%d][%d][%d]: %w", lyr, si, vi, err)
						}
						key = string(kb)
					}
					vec := make([]float32, dim)
					if dim > 0 {
						vb := unsafe.Slice((*byte)(unsafe.Pointer(&vec[0])), dim*4)
						if _, err := io.ReadFull(r, vb); err != nil {
							return fmt.Errorf("leveled: read vec[%d][%d][%d]: %w", lyr, si, vi, err)
						}
					}
					var attrs Attributes
					if version >= 3 {
						a, err := readAttrs(r)
						if err != nil {
							return fmt.Errorf("leveled: read hnswAttrs[%d][%d][%d]: %w", lyr, si, vi, err)
						}
						attrs = a
					}
					var terms []TermTF
					if version >= 7 {
						t, err := readTerms(r)
						if err != nil {
							return fmt.Errorf("leveled: read hnswTerms[%d][%d][%d]: %w", lyr, si, vi, err)
						}
						terms = t
					}
					entries = append(entries, DeltaEntry{Key: key, Vec: vec, Attrs: attrs, Terms: terms})
				}

				if len(entries) > 0 {
					staged[lyr] = append(staged[lyr], stagedSeg{entries: entries})
				}

			default:
				return fmt.Errorf("leveled: unknown segType=%d at [%d][%d]", segType, lyr, si)
			}
		}
	}

	// Tombstones (v4+): восстанавливаем множество удалённых-но-не-вырезанных ключей.
	// Без этого удаления, сделанные до снапшота, «воскресают» после рестарта (P0-1).
	// Старые снапшоты (v<4) секции не имеют → мапа остаётся пустой (прежнее поведение).
	var tomb map[string]struct{}
	if version >= 4 {
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return fmt.Errorf("leveled: read nTombstones: %w", err)
		}
		tcount := int(binary.LittleEndian.Uint32(u4[:]))
		if tcount > 0 {
			tomb = make(map[string]struct{}, tcount)
			var tklen [2]byte
			for i := 0; i < tcount; i++ {
				if _, err := io.ReadFull(r, tklen[:]); err != nil {
					return fmt.Errorf("leveled: read tombstoneKeyLen[%d]: %w", i, err)
				}
				kl := int(binary.LittleEndian.Uint16(tklen[:]))
				kb := make([]byte, kl)
				if _, err := io.ReadFull(r, kb); err != nil {
					return fmt.Errorf("leveled: read tombstoneKey[%d]: %w", i, err)
				}
				tomb[string(kb)] = struct{}{}
			}
		}
	}

	// CRC32-трейлер (v5+): сверяем чексум всего прочитанного payload. Ловит тихий
	// bit-rot graph_leveled.bin до того, как битые данные попадут в индекс (P0-3).
	if version >= 5 {
		var crcBuf [4]byte
		if _, err := io.ReadFull(crcSource, crcBuf[:]); err != nil {
			return fmt.Errorf("leveled: read crc: %w", err)
		}
		want := binary.LittleEndian.Uint32(crcBuf[:])
		if got := hasher.Sum32(); got != want {
			return fmt.Errorf("leveled: snapshot CRC mismatch: got %08x want %08x (повреждён graph_leveled.bin)", got, want)
		}
	}

	// ── Фаза коммита: payload целиком валиден, ошибок ниже нет. ──

	lvs.snapshotTime = snapTime
	lvs.snapshotLSN = snapLSN
	lvs.dim = dim

	for i := range lvs.levels {
		lvs.levels[i] = nil
	}
	for lyr := range staged {
		if len(staged[lyr]) == 0 {
			continue
		}
		lvs.levels[lyr] = make([]segment, 0, len(staged[lyr]))
		for _, ss := range staged[lyr] {
			seg := ss.seg
			if seg == nil {
				seg = lvs.buildSegment(ss.entries, dim, 0)
			}
			if seg != nil {
				lvs.levels[lyr] = append(lvs.levels[lyr], seg)
			}
		}
	}

	if tomb != nil {
		lvs.tombstones.Store(&tomb)
	}

	// Инициализируем пустую дельту (новые Add() пойдут сюда)
	if lvs.delta != nil {
		lvs.delta.Close() // освободить аллокатор старой дельты
	}
	if dim > 0 {
		max := deltaMax(dim, lvs.cfg.DeltaMax)
		lvs.delta = NewDeltaSegmentSharded(dim, max, lvs.cfg.Distance, lvs.cfg.M, lvs.cfg.EfConstruction, lvs.deltaShardCount())
	}

	lvs.touchMutation()
	return nil
}

// =============================================================================
// Внутренние методы
// =============================================================================

// ─── Build Pool: producer-consumer типы ───────────────────────────────────────

// taskKind — тип задачи компакции.
type taskKind int

const (
	taskFlushDelta  taskKind = iota // flush delta → новый L0 сегмент
	taskMergeLevel                  // merge N сегментов уровня L → сегмент уровня L+1
	taskConsolidate                 // idle-консолидация: merge ВСЕХ сегментов всех уровней в один
)

// compactionTask — единица работы. Создаётся coordinator'ом (GRAB под lock),
// обрабатывается build/merge goroutine (BUILD вне lock), результат возвращается
// через resultChan → coordinator атомарно применяет (PUBLISH под lock).
type compactionTask struct {
	kind      taskKind
	entries   []DeltaEntry // для taskFlushDelta: векторы из дельты
	segs      []segment    // для taskMergeLevel: сегменты для слияния (oldSegs для замены)
	targetLvl int          // куда публиковать результат (0 для flush, L+1 для merge)
	sourceLvl int          // для merge: уровень источника (L) — для cleanup oldSegs
	epoch     uint64       // эпоха на момент GRAB (для PUBLISH-time check)
	dim       int          // dim, захваченный под локом (build читает его вне лока)
	// flushDelta — сфлашиваемая дельта (taskFlushDelta): остаётся searchable в
	// lvs.flushing, пока applyResult не опубликует построенный из неё сегмент.
	flushDelta *DeltaSegment
	// result — заполнается goroutine после BUILD, читается coordinator при PUBLISH.
	result segment
}

// collectSegments возвращает flat срез всех активных сегментов (под RLock).
func (lvs *LeveledVectorStore) collectSegments() []segment {
	var segs []segment
	for _, level := range lvs.levels {
		segs = append(segs, level...)
	}
	return segs
}

// triggerCompact посылает сигнал в non-blocking канал.
// Вызывать под write lock.
func (lvs *LeveledVectorStore) triggerCompact() {
	select {
	case lvs.compactCh <- struct{}{}:
	default: // уже есть сигнал, не блокируем
	}
}

// compactWorker (coordinator) — ЧИСТЫЙ event loop. Никогда не строит сам.
//
//	select {
//	  compactCh  → grab delta → запустить goroutine build → продолжить
//	  resultChan → атомарно применить результат в levels[] → продолжить
//	}
//
// Старые сегменты НЕ убираются из levels[] пока merge не завершён (нет дыр для search).
// mergeInFlight[L] = true → merge уровня L в работе (новый не запускаем).
func (lvs *LeveledVectorStore) compactWorker() {
	// Idle-детект: тик = четверть окна затишья → консолидация стартует с
	// запаздыванием не более 25% от IdleConsolidateAfter. nil-канал (фича
	// выключена) в select блокируется вечно — ветка мертва без оверхеда.
	var idleC <-chan time.Time
	if lvs.cfg.IdleConsolidateAfter > 0 {
		t := time.NewTicker(lvs.cfg.IdleConsolidateAfter / 4)
		defer t.Stop()
		idleC = t.C
	}
	for {
		select {
		case <-lvs.done:
			return

		case <-lvs.compactCh:
			// Сигнал: возможно нужно что-то сделать (delta fill или merge overflow).
			lvs.handleCompactSignal()

		case r := <-lvs.resultChan:
			// Результат от build/merge goroutine → атомарно применить.
			lvs.applyResult(r)

		case <-idleC:
			// Затишье мутаций → флаш остатка дельты, затем консолидация сегментов.
			lvs.handleIdleTick()
		}
	}
}

// handleIdleTick — idle-консолидация (IdleConsolidateAfter). Вызывается только
// из compactWorker (тот же goroutine, что и handleCompactSignal/applyResult —
// вся координация однопоточна, флаги in-flight гоняться не могут).
//
// Последовательность (по тику за шаг, без ожиданий внутри):
//  1. Есть остаток дельты (не Full — сам не сфлашится никогда) → штатный flush
//     через triggerCompact; консолидация подождёт следующего тика.
//  2. Дельта пуста, работ в полёте нет, сегментов >1 и крупнейший держит <90%
//     данных → merge всех сегментов всех уровней в один.
func (lvs *LeveledVectorStore) handleIdleTick() {
	if time.Now().UnixNano()-lvs.lastMutation.Load() < int64(lvs.cfg.IdleConsolidateAfter) {
		return
	}

	// Жатва TTL (шаг 6 VMEM) — ДО консолидации: Delete истёкших ставит
	// tombstones, и консолидация ниже выметает их физически тем же циклом
	// простоя (стор становится и чистым, и плотным). Пожатые удаления трогают
	// lastMutation → следующий тик отсчитает окно затишья заново; это принято.
	// Часы легальны (дверь 1): решение жнеца локально, в WAL не едет — после
	// реплея истёкшие воскресают физически, невидимы фильтром чтения и будут
	// пожаты заново.
	lvs.ReapExpired(time.Now().Unix(), vmemReapTickLimit)

	lvs.mu.Lock()
	epoch := lvs.compactionEpoch.Load()

	if lvs.delta != nil && lvs.delta.Len() > 0 {
		lvs.mu.Unlock()
		lvs.triggerCompact() // handleCompactSignal сфлашит непустую дельту
		return
	}
	// Ждём тишины и в компакционной машинерии: параллельная консолидация с
	// flush/merge либо дублировала бы данные, либо мержила снапшот без хвоста.
	if lvs.consolidateInFlight || lvs.deltaFlushInFlight || lvs.inFlightBuilds.Load() > 0 || lvs.anyMergeInFlightLocked() {
		lvs.mu.Unlock()
		return
	}

	// Собираем сегменты СТАРШИЕ→МЛАДШИЕ (уровни сверху вниз, внутри уровня — по
	// созданию): дедуп в mergeSegments оставляет ПОСЛЕДНЮЮ копию ключа, порядок
	// oldest→newest обязателен для корректного upsert (как в maybeScheduleMerges).
	var toMerge []segment
	highest := 0
	for lyr := maxLevels - 1; lyr >= 0; lyr-- {
		if len(lvs.levels[lyr]) > 0 && highest == 0 {
			highest = lyr
		}
		toMerge = append(toMerge, lvs.levels[lyr]...)
	}
	if len(toMerge) <= 1 {
		lvs.mu.Unlock()
		return
	}
	// Guard от рестройки-шторма: почти всё уже в одном сегменте → полный rebuild
	// ради хвоста не окупается (капля в 1k векторов после паузы пересобирала бы
	// миллионный индекс). Хвост мелких L0 дочистит обычный fanout-каскад.
	total, largest := 0, 0
	for _, s := range toMerge {
		n := s.Len()
		total += n
		if n > largest {
			largest = n
		}
	}
	if total == 0 || largest*10 >= total*9 {
		lvs.mu.Unlock()
		return
	}

	lvs.consolidateInFlight = true
	targetLvl := highest + 1
	if targetLvl >= maxLevels {
		targetLvl = maxLevels - 1
	}
	lvs.mu.Unlock()

	// Merge-горутина — зеркало maybeScheduleMerges. mergeAllocators[0] свободен:
	// консолидация стартует только при полном отсутствии merge в полёте, а новые
	// merge блокируются consolidateInFlight.
	go func(segs []segment, tgtLvl int, e uint64) {
		mergeStart := time.Now()
		var seg segment
		var applied map[string]struct{}
		func() {
			defer func() {
				if r := recover(); r != nil {
					seg = nil
					applied = nil
				}
			}()
			seg, applied = lvs.mergeSegmentsWithAllocator(segs, lvs.mergeAllocators[0])
		}()
		monitoring.VectorMergeDuration.Update(time.Since(mergeStart).Seconds())
		res := compactionResult{
			kind:              taskConsolidate,
			targetLvl:         tgtLvl,
			epoch:             e,
			result:            seg,
			oldSegs:           segs,
			appliedTombstones: applied,
		}
		select {
		case lvs.resultChan <- res:
		case <-lvs.done:
		}
	}(toMerge, targetLvl, epoch)
}

// anyMergeInFlightLocked — как anyMergeInFlight, но caller уже держит lvs.mu.
func (lvs *LeveledVectorStore) anyMergeInFlightLocked() bool {
	for i := 0; i < maxLevels; i++ {
		if lvs.mergeInFlight[i] {
			return true
		}
	}
	return false
}

// handleCompactSignal — обработка сигнала compactCh.
// GRAB delta → запустить goroutine build → проверить merges.
func (lvs *LeveledVectorStore) handleCompactSignal() {
	monitoring.VectorCompactionTotal.Inc()
	epoch := lvs.compactionEpoch.Load()

	// GRAB delta: атомарный swap под write-lock (микросекунды).
	lvs.mu.Lock()
	if lvs.delta == nil || lvs.delta.Len() == 0 {
		lvs.mu.Unlock()
		lvs.maybeScheduleMerges(epoch)
		return
	}
	// Кап flushing-бэклога: очередной swap при полном бэклоге лишь плодит
	// fp32-графы, которые каждый поиск обходит с ef-полом. Дельта копит дальше;
	// swap повторится с applyResult ближайшего build (re-check delta.Full()) или
	// со следующего пробуждения FlushDeltaSync. Жёсткий предел роста — гарантия,
	// что при перегрузе вставками build остаётся ограниченного размера.
	if len(lvs.flushing) >= flushingBacklogCap &&
		lvs.delta.Len() < deltaHardGrowthFactor*lvs.delta.MaxSize() {
		lvs.mu.Unlock()
		monitoring.VectorFlushSwapDeferredTotal.Inc()
		lvs.maybeScheduleMerges(epoch)
		return
	}
	oldDelta := lvs.delta
	// dim захватываем ПОД локом: build-горутина иначе читает lvs.dim без синхронизации,
	// а Clear() пишет lvs.dim=0 (pre-existing data race, обнажён Clear-during-build).
	dim := lvs.dim
	max := deltaMax(lvs.dim, lvs.cfg.DeltaMax)
	lvs.delta = NewDeltaSegmentSharded(lvs.dim, max, lvs.cfg.Distance, lvs.cfg.M, lvs.cfg.EfConstruction, lvs.deltaShardCount())
	// Держим сфлашиваемую дельту searchable до публикации сегмента (закрывает
	// flush-visibility gap). Close/удаление — в applyResult под mu.Lock.
	lvs.flushing = append(lvs.flushing, oldDelta)
	lvs.mu.Unlock()

	monitoring.VectorFlushDeltaTotal.Inc()
	if oldDelta.Len() > 0 {
		// Freeze-on-flush: графы дельты УЖЕ построены инкрементально при Add —
		// замораживаем их напрямую (SQ8 при UseSQ — любой dim; иначе CSR float32
		// при dim ≤ порога), без повторной сборки HNSW (нет double-build).
		// Тенант-режим (TenantOf/PartitionAttr) и атрибуты требуют rebuild для
		// сортировки/колонок → freeze (заморозка инкрементального графа как есть)
		// несовместим: он сохранил бы временной порядок вставки, а не тенант-порядок.
		// HasText — как HasAttrs: freeze заморозил бы граф БЕЗ segmentText, и
		// SEARCHTEXT, работавший по дельте, молча слеп бы после flush.
		canFreeze := lvs.cfg.TenantOf == nil && lvs.cfg.PartitionAttr == "" &&
			!oldDelta.HasAttrs() && !oldDelta.HasText() &&
			(lvs.cfg.UseSQ || lvs.dim <= csrDimThreshold)
		switch {
		case canFreeze && !oldDelta.Sharded():
			// Один шард → один граф → один сегмент. Close аллокатора — внутри
			// горутины ПОСЛЕ freeze (freeze копирует в Go-память).
			lvs.startFreezeDeltaGoroutine(oldDelta, epoch)
		case canFreeze:
			// Freeze-per-shard: каждый из nShards графов замораживается СВОИМ
			// сегментом → nShards мини-сегментов публикуются атомарно в L0, а
			// merge-каскад склеивает их фоном. Это схлопывает flushing-состояние
			// за миллисекунды вместо десятков секунд rebuild'а (563-2738×, см.
			// freeze_pershard_profit_test.go) → дорогие fp32-графы исчезают из
			// переходного состояния, бэклог не копится, кап swap'ов не срабатывает.
			// RepairReachability тут НЕ зовём: verify стоит ~полсборки (см.
			// graph.go), а достижимость мини = достижимость тех же графов, по
			// которым поиск ходил в дельте только что; первый же merge пере-
			// строит с шафлом+repair (buildSegmentWithAllocator).
			lvs.startFreezeShardsGoroutine(oldDelta, epoch)
		default:
			// Freeze неприменим (attrs/tenant: нужна пересортировка entries;
			// fp32 dim>порога: frozen-CSR не применим) → rebuild из slab.
			// entries ссылаются на slab oldDelta (не на граф) → oldDelta должна жить до
			// конца build → Close откладывается в applyResult (после публикации сегмента),
			// а не сразу после ExtractAll (иначе UAF по slab при конкурентном Search).
			entries := oldDelta.ExtractAll()
			lvs.startBuildGoroutine(&compactionTask{
				kind:       taskFlushDelta,
				entries:    entries,
				targetLvl:  0,
				epoch:      epoch,
				dim:        dim,
				flushDelta: oldDelta,
			})
		}
	} else {
		// Пустая дельта (defensive; early-return выше отсекает Len==0): нечего строить.
		// Убираем из flushing и закрываем сразу.
		lvs.removeFlushing(oldDelta)
		oldDelta.Close()
		// Дельта снята с flushing → будим FlushDeltaSync (ждёт по членству).
		lvs.mu.Lock()
		lvs.flushCond.Broadcast()
		lvs.mu.Unlock()
	}

	// Проверить: нужно ли запустить merge (без блокировки coordinator).
	lvs.maybeScheduleMerges(epoch)
}

// removeFlushing удаляет сфлашенную дельту из lvs.flushing (берёт mu.Lock).
// Вызывается из build/freeze-горутин на shutdown-пути (lock не удержан).
func (lvs *LeveledVectorStore) removeFlushing(d *DeltaSegment) {
	if d == nil {
		return
	}
	lvs.mu.Lock()
	lvs.removeFlushingLocked(d)
	lvs.mu.Unlock()
}

// removeFlushingLocked удаляет d из lvs.flushing С СОХРАНЕНИЕМ ПОРЯДКА (порядок =
// свежесть для провенанс-дедупа). Вызывать под mu.Lock. Сдвиг in-place безопасен:
// search копирует срез flushing под RLock, а мутация идёт под эксклюзивным Lock.
func (lvs *LeveledVectorStore) removeFlushingLocked(d *DeltaSegment) {
	for i, f := range lvs.flushing {
		if f == d {
			lvs.flushing = append(lvs.flushing[:i], lvs.flushing[i+1:]...)
			return
		}
	}
}

// startBuildGoroutine запускает goroutine для BUILD delta-flush сегмента.
// inFlightBuilds.Add(1) выполняется СИНХРОННО до запуска горутины —
// FlushDeltaSync никогда не увидит ложный 0.
//
// Архитектура:
//   - buildSem семафор ограничивает CPU-нагрузку: ≤ numBuilders горутин одновременно.
//   - Каждая горутина создаёт СВОЙ свежий TCMallocStore → нет TCMalloc race (корень бага).
//   - FrozenGraph копирует всё в Go memory → alloc можно закрыть после build.
//   - defer alloc.Close() останавливает внутреннюю goroutine runDeferredFree (нет утечек).
func (lvs *LeveledVectorStore) startBuildGoroutine(task *compactionTask) {
	// Синхронный инкремент ДО запуска горутины — критично для FlushDeltaSync.
	lvs.inFlightBuilds.Add(1)
	epoch := task.epoch
	flushDelta := task.flushDelta
	go func() {
		// Ожидаем свободный слот (семафор). Горутина блокируется, coordinator — нет.
		select {
		case lvs.buildSem <- struct{}{}:
			defer func() { <-lvs.buildSem }()
		case <-lvs.done:
			// Shutdown: coordinator уже вышел → applyResult не отработает.
			// Закрываем сфлашиваемую дельту сами (единственный owner Close на этом пути).
			lvs.removeFlushing(flushDelta)
			flushDelta.Close()
			res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: nil}
			select {
			case lvs.resultChan <- res:
			default:
				lvs.inFlightBuilds.Add(-1)
			}
			return
		}

		// Свежий аллокатор на каждый build: нет shared state, нет TCMalloc race.
		// FrozenGraph не держит ссылку на аллокатор → можно закрыть после build.
		alloc := tcmalloc.NewTCMallocStore(1)
		defer alloc.Close() // остановить runDeferredFree goroutine

		var seg segment
		func() {
			defer func() {
				if r := recover(); r != nil {
					seg = nil // паника → rollback
				}
			}()
			seg = lvs.buildSegmentWithAllocator(task.entries, task.dim, alloc)
		}()

		// flushDelta передаётся в результат — applyResult удалит её из flushing и
		// Close'ит ПОСЛЕ публикации сегмента (держали searchable весь build).
		res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: seg, flushDelta: flushDelta}
		select {
		case lvs.resultChan <- res:
		case <-lvs.done:
			// Shutdown до применения результата: coordinator не отработает — Close сами.
			lvs.removeFlushing(flushDelta)
			flushDelta.Close()
			lvs.inFlightBuilds.Add(-1)
		}
	}()
}

// startFreezeDeltaGoroutine замораживает УЖЕ построенный граф дельты напрямую
// в frozen-CSR (без rebuild HNSW). Применяется при dim ≤ csrDimThreshold.
// inFlightBuilds.Add(1) синхронно ДО запуска горутины (как startBuildGoroutine).
func (lvs *LeveledVectorStore) startFreezeDeltaGoroutine(oldDelta *DeltaSegment, epoch uint64) {
	lvs.inFlightBuilds.Add(1)
	go func() {
		select {
		case lvs.buildSem <- struct{}{}:
			defer func() { <-lvs.buildSem }()
		case <-lvs.done:
			// Shutdown: coordinator уже вышел → Close сами (единственный owner).
			lvs.removeFlushing(oldDelta)
			oldDelta.Close()
			res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: nil}
			select {
			case lvs.resultChan <- res:
			default:
				lvs.inFlightBuilds.Add(-1)
			}
			return
		}

		var seg segment
		func() {
			defer func() {
				if r := recover(); r != nil {
					seg = nil // паника → rollback
				}
			}()
			keys := oldDelta.FreezeKeys()
			g := oldDelta.Graph()
			if lvs.cfg.UseSQ {
				if fg := FreezeGraphSQ(g, lvs.cfg.Metric, keys); fg != nil {
					seg = &frozenSQSegment{fg: fg}
				}
			} else {
				if fg := FreezeGraph(g, lvs.cfg.Distance, keys); fg != nil {
					seg = &frozenSegment{fg: fg}
				}
			}
		}()
		// freeze скопировал данные в Go-память, НО oldDelta остаётся searchable в
		// flushing до публикации сегмента → Close откладывается в applyResult
		// (единственный owner Close на нормальном пути; эксклюзивно к search-RLock).
		res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: seg, flushDelta: oldDelta}
		select {
		case lvs.resultChan <- res:
		case <-lvs.done:
			lvs.inFlightBuilds.Add(-1)
		}
	}()
}

// startFreezeShardsGoroutine — freeze-per-shard: замораживает КАЖДЫЙ шард
// шардированной дельты его собственным FreezeGraphSQ/FreezeGraph → nShards
// мини-сегментов (миллисекунды vs десятки секунд rebuild). Мини публикуются
// applyResult'ом атомарно (results) вместе со снятием дельты с flushing;
// merge-каскад склеивает их фоном (mergeSegments перестраивает с шафлом+repair).
//
// Семантика отказа — all-or-nothing, как у build: если хоть один непустой шард
// не заморозился (panic/nil), НИЧЕГО не публикуем (results=nil → rollback,
// частичная публикация потеряла бы данные шарда молча; WAL реплеит на рестарте).
// Пустые шарды (хэш-дисбаланс на маленькой дельте) пропускаются — это не отказ.
//
// Контракты как у startFreezeDeltaGoroutine: inFlightBuilds.Add(1) синхронно ДО
// горутины; buildSem занимаем ОДИН слот на весь freeze (предохранитель при
// перегрузе — freeze конкурирует за CPU наравне с builds, см. 17663b6); после
// swap дельта immutable → чтение графов/nodeKey без shard.mu безопасно; freeze
// копирует в Go-память → Close дельты (аллокаторов) в applyResult после публикации.
func (lvs *LeveledVectorStore) startFreezeShardsGoroutine(oldDelta *DeltaSegment, epoch uint64) {
	lvs.inFlightBuilds.Add(1)
	go func() {
		select {
		case lvs.buildSem <- struct{}{}:
			defer func() { <-lvs.buildSem }()
		case <-lvs.done:
			// Shutdown: coordinator уже вышел → Close сами (единственный owner).
			lvs.removeFlushing(oldDelta)
			oldDelta.Close()
			res := compactionResult{kind: taskFlushDelta, epoch: epoch, result: nil}
			select {
			case lvs.resultChan <- res:
			default:
				lvs.inFlightBuilds.Add(-1)
			}
			return
		}

		nShards := oldDelta.ShardCount()
		minis := make([]segment, nShards) // [i] = мини шарда i; nil = пустой шард
		failed := atomic.Bool{}
		var wg sync.WaitGroup
		for i := 0; i < nShards; i++ {
			if oldDelta.ShardLiveLen(i) == 0 {
				continue // пустой шард — нечего замораживать (не отказ)
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						failed.Store(true) // паника шарда → rollback всего flush
					}
				}()
				keys := oldDelta.ShardFreezeKeys(i)
				g := oldDelta.ShardGraph(i)
				if lvs.cfg.UseSQ {
					if fg := FreezeGraphSQ(g, lvs.cfg.Metric, keys); fg != nil {
						minis[i] = &frozenSQSegment{fg: fg}
						return
					}
				} else {
					if fg := FreezeGraph(g, lvs.cfg.Distance, keys); fg != nil {
						minis[i] = &frozenSegment{fg: fg}
						return
					}
				}
				failed.Store(true) // непустой шард дал nil-freeze → rollback
			}(i)
		}
		wg.Wait()

		var results []segment
		if !failed.Load() {
			for _, m := range minis {
				if m != nil {
					results = append(results, m)
				}
			}
		}
		res := compactionResult{kind: taskFlushDelta, epoch: epoch, results: results, flushDelta: oldDelta}
		select {
		case lvs.resultChan <- res:
		case <-lvs.done:
			lvs.removeFlushing(oldDelta)
			oldDelta.Close()
			lvs.inFlightBuilds.Add(-1)
		}
	}()
}

// startMergeGoroutine запускает goroutine для BUILD merge-сегмента.
// Результат возвращается через resultChan. Сегменты НЕ удаляются из levels
// до получения результата (applyResult).
func (lvs *LeveledVectorStore) startMergeGoroutine(task *compactionTask) {
	sourceLvl := task.sourceLvl
	targetLvl := task.targetLvl
	epoch := task.epoch
	segs := task.segs
	go func() {
		mergeStart := time.Now()
		seg, applied := lvs.mergeSegmentsWithAllocator(segs, lvs.pickMergeAllocator(sourceLvl))
		monitoring.VectorMergeDuration.Update(time.Since(mergeStart).Seconds())
		res := compactionResult{
			kind:              taskMergeLevel,
			sourceLvl:         sourceLvl,
			targetLvl:         targetLvl,
			epoch:             epoch,
			result:            seg,
			oldSegs:           segs,
			appliedTombstones: applied,
		}
		select {
		case lvs.resultChan <- res:
		case <-lvs.done:
		}
	}()
}

// maybeScheduleMerges проверяет переполненные уровни и запускает merge goroutines.
// По одному merge на уровень одновременно (mergeInFlight[L]). Сегменты НЕ убираются
// из levels[] — старые остаются видимы для search до замены.
func (lvs *LeveledVectorStore) maybeScheduleMerges(epoch uint64) {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	lvs.maybeScheduleMergesLocked(epoch)
}

// maybeScheduleMergesLocked — тело maybeScheduleMerges; caller держит lvs.mu.
// Вызывается напрямую из applyResult: каскад merge пинается БЕЗ triggerCompact,
// т.к. сигнал заставил бы handleCompactSignal сначала забрать текущую дельту,
// какой бы маленькой она ни была → при живом потоке вставок это
// самоподдерживающийся флаш-шторм (e2e 09.07: 424 микросегмента по ~140
// векторов на 60k вставок, пока долгий merge держал mergeInFlight[0]).
func (lvs *LeveledVectorStore) maybeScheduleMergesLocked(epoch uint64) {
	fanout := lvs.cfg.Fanout

	// Идёт консолидация — она уже мержит ВСЕ сегменты; параллельный merge тех же
	// сегментов опубликовал бы данные дважды (дубли в памяти под дедупом поиска).
	if lvs.consolidateInFlight {
		return
	}

	// Приоритет флаш-builds над merge (brownout после bulk-load, e2e 09.07):
	// каждая flushing-дельта — это nShards fp32-графов, которые КАЖДЫЙ поиск
	// обходит с ef-полом (70% CPU окна brownout — именно этот поиск). Merge
	// работает над frozen-сегментами, уже дешёвыми для поиска, и может подождать —
	// CPU отдаём builds, ликвидирующим дорогое состояние. Отложенный merge не
	// теряется: applyResult последнего build вызывает нас же, когда flushing пуст.
	deferMerges := len(lvs.flushing) > 0 || lvs.inFlightBuilds.Load() > 0

	for lyr := 0; lyr < maxLevels-1; lyr++ {
		if lvs.mergeInFlight[lyr] {
			continue // merge этого уровня уже в работе
		}
		if len(lvs.levels[lyr]) < fanout {
			continue // уровень не переполнен — продолжаем проверять выше (L1 может быть переполнен без L0)
		}
		// Предохранитель от голодания: под непрерывным insert-потоком flushing не
		// пустеет никогда — уровень, разросшийся до 2×fanout, мержим несмотря на
		// builds, иначе fanout поиска по уровню растёт без границы.
		if deferMerges && len(lvs.levels[lyr]) < 2*fanout {
			monitoring.VectorMergeDeferredTotal.Inc()
			continue
		}
		if lvs.compactionEpoch.Load() != epoch {
			break // Clear() — стоп
		}
		// Batch-merge: берём ВЕСЬ накопившийся префикс уровня до капа, а не ровно
		// fanout (см. LeveledConfig.MergeBatchMax — почему каскад по fanout штук
		// проигрывает freeze-per-shard гонку). Префикс = oldest→newest порядок,
		// обязательный для дедупа upsert в mergeSegmentsWithAllocator. Сегменты НЕ
		// удаляются из levels (copy slice header) — старые видимы для search до
		// applyResult.
		batch := len(lvs.levels[lyr])
		if batch > lvs.cfg.MergeBatchMax {
			batch = lvs.cfg.MergeBatchMax
		}
		toMerge := make([]segment, batch)
		copy(toMerge, lvs.levels[lyr][:batch])

		lvs.mergeInFlight[lyr] = true
		sourceLvl := lyr
		targetLvl := lyr + 1
		taskEpoch := epoch
		// Запускаем merge goroutine (coordinator не блокируется).
		go func(segs []segment, srcLvl, tgtLvl int, e uint64) {
			mergeStart := time.Now()
			var seg segment
			var applied map[string]struct{}
			func() {
				defer func() {
					if r := recover(); r != nil {
						seg = nil
						applied = nil
					}
				}()
				seg, applied = lvs.mergeSegmentsWithAllocator(segs, lvs.pickMergeAllocator(srcLvl))
			}()
			monitoring.VectorMergeDuration.Update(time.Since(mergeStart).Seconds())
			res := compactionResult{
				kind:              taskMergeLevel,
				sourceLvl:         srcLvl,
				targetLvl:         tgtLvl,
				epoch:             e,
				result:            seg,
				oldSegs:           segs,
				appliedTombstones: applied,
			}
			select {
			case lvs.resultChan <- res:
			case <-lvs.done:
			}
		}(toMerge, sourceLvl, targetLvl, taskEpoch)
	}
}

// applyResult — атомарное применение результата build/merge под write lock.
// Для flushDelta: просто append в levels[0].
// Для mergeLevel: убрать oldSegs из levels[source], добавить newSeg в levels[target].
// После применения: сброс mergeInFlight, re-check overflow, signal FlushDeltaSync.
func (lvs *LeveledVectorStore) applyResult(r compactionResult) {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()

	// Epoch check: если Clear() сработал во время build — результат устарел.
	if lvs.compactionEpoch.Load() != r.epoch {
		monitoring.VectorCompactionEpochMismatchTotal.Inc()
		if r.kind == taskFlushDelta {
			// Сегмент устарел (Clear) → не публикуем. Дельту всё равно снимаем с
			// searchable-списка и закрываем (owner Close на этом пути — здесь).
			lvs.removeFlushingLocked(r.flushDelta)
			if r.flushDelta != nil {
				r.flushDelta.Close()
			}
			lvs.inFlightBuilds.Add(-1)
			// Дельта снята с flushing → будим FlushDeltaSync (ждёт по членству).
			lvs.flushCond.Broadcast()
		} else if r.kind == taskConsolidate {
			lvs.consolidateInFlight = false
		} else {
			lvs.mergeInFlight[r.sourceLvl] = false
		}
		return
	}

	switch r.kind {
	case taskFlushDelta:
		lvs.inFlightBuilds.Add(-1)
		if len(r.results) > 0 {
			// Freeze-per-shard: все мини-сегменты шардов публикуются ОДНИМ append
			// под этим же Lock — search никогда не видит «половину дельты».
			lvs.levels[0] = append(lvs.levels[0], r.results...)
		} else if r.result != nil {
			lvs.levels[0] = append(lvs.levels[0], r.result)
		} else {
			monitoring.VectorCompactionRollbackTotal.Inc()
		}
		// Публикация сегмента и снятие дельты с searchable-списка — под одним Lock:
		// для любого Search переход delta→segment атомарен (видит либо flushing-дельту,
		// либо уже сегмент — но не «ни там, ни там» = закрытый flush-visibility gap).
		// Close ПОСЛЕ снятия из flushing и публикации: с этого момента дельту никто не
		// ищет (search берёт RLock, эксклюзивно к нашему Lock) → нет UAF по slab/графу.
		lvs.removeFlushingLocked(r.flushDelta)
		if r.flushDelta != nil {
			r.flushDelta.Close()
		}
		// Дельта снята с flushing и сегмент опубликован → будим FlushDeltaSync
		// (он перепроверит членство pre-call набора в flushing). Безусловно: waiter
		// сам решает, все ли его дельты опубликованы — здесь гейта быть не должно.
		lvs.flushCond.Broadcast()

	case taskMergeLevel:
		lvs.mergeInFlight[r.sourceLvl] = false
		if r.result == nil {
			// Merge не удался — сегменты остаются в levels (не трогаем), просто выходим.
			monitoring.VectorCompactionRollbackTotal.Inc()
			return
		}
		// Убираем oldSegs из levels[source] — они заменены новым сегментом.
		lvs.removeSegments(r.sourceLvl, r.oldSegs)
		// Добавляем новый сегмент в levels[target].
		lvs.levels[r.targetLvl] = append(lvs.levels[r.targetLvl], r.result)
		// Чистим tombstones применённых ключей — АТОМАРНО со свапом и лишь тех,
		// кого больше нет в других сегментах (см. clearAppliedTombstonesLocked).
		lvs.clearAppliedTombstonesLocked(r.appliedTombstones, r.result)

	case taskConsolidate:
		lvs.consolidateInFlight = false
		if r.result == nil {
			monitoring.VectorCompactionRollbackTotal.Inc()
			return
		}
		// oldSegs пришли со ВСЕХ уровней — чистим каждый (removeSegments фильтрует
		// по identity, лишние элементы в toRemove безвредны).
		for lyr := range lvs.levels {
			lvs.removeSegments(lyr, r.oldSegs)
		}
		lvs.levels[r.targetLvl] = append(lvs.levels[r.targetLvl], r.result)
		lvs.clearAppliedTombstonesLocked(r.appliedTombstones, r.result)
	}

	// Re-check каскада — ПРЯМЫМ вызовом под уже удержанным mu, НЕ через
	// triggerCompact: любой сигнал заставляет handleCompactSignal сначала
	// забрать текущую дельту (какой бы маленькой она ни была) → при живом
	// потоке вставок post-flush сигнал становится самоподдерживающимся
	// флаш-штормом (микросегменты по ~сотне векторов, e2e 09.07). Прямой вызов
	// планирует только merge переполненных уровней и дельту не трогает.
	// Закрывает и bulk-load→тишина: последний flush, публикующий fanout-ный L0,
	// сам ставит merge (раньше каскад пинался только СЛЕДУЮЩИМ flush).
	lvs.maybeScheduleMergesLocked(r.epoch)

	// Сигнал (с забором дельты) — ТОЛЬКО когда она реально ПОЛНА (safety на
	// случай пропущенного Add-триггера). Прежде здесь была ветка
	// `flushPending && deltaNonEmpty` — она заставляла applyResult гнать flush
	// остатка дельты после КАЖДОГО build во время FlushDeltaSync → flush-storm
	// (тысячи flush по 1-2 вектора, каждый — свежий дельта-сегмент, сотни ГБ
	// churn). FlushDeltaSync теперь сам форсит нужный swap и ждёт по членству в
	// flushing, поэтому eager-re-flush остатка здесь больше не нужен.
	if lvs.delta != nil && lvs.delta.Full() {
		lvs.triggerCompact()
	}
}

// removeSegments удаляет указанные сегменты из levels[lyr].
// O(N) но N маленькое (fanout сегментов на уровне).
func (lvs *LeveledVectorStore) removeSegments(lyr int, toRemove []segment) {
	if len(toRemove) == 0 {
		return
	}
	// Build set of pointers to remove (identity comparison).
	level := lvs.levels[lyr]
	kept := level[:0] // in-place filter
	for _, s := range level {
		found := false
		for _, r := range toRemove {
			if s == r { // pointer identity
				found = true
				break
			}
		}
		if !found {
			kept = append(kept, s)
		}
	}
	lvs.levels[lyr] = kept
}

// pickBuildWorkerID — round-robin по buildAllocators.
// Каждый вызов возвращает следующий ID (atomic). Обеспечивает изоляцию
// TCMallocStore при параллельных delta-flush goroutines (каждый — свой аллокатор).
func (lvs *LeveledVectorStore) pickBuildWorkerID() int {
	n := len(lvs.buildAllocators)
	if n == 0 {
		return 0
	}
	return int(lvs.buildWorkerIDCounter.Add(1)-1) % n
}

// pickMergeAllocator возвращает изолированный аллокатор для merge уровня sourceLvl.
// Каждый уровень имеет свой allocator → параллельные merge разных уровней
// не конкурируют. mergeAllocators имеет maxLevels элементов.
func (lvs *LeveledVectorStore) pickMergeAllocator(sourceLvl int) *tcmalloc.TCMallocStore {
	if sourceLvl >= 0 && sourceLvl < len(lvs.mergeAllocators) {
		return lvs.mergeAllocators[sourceLvl]
	}
	return lvs.mergeAllocators[0]
}

// enqueueTask кладёт task в buildQueue, уважая shutdown.
// Может блокироваться при полном буфере (backpressure).
// Increment inFlightBuilds — buildWorker декремтит после publish.
func (lvs *LeveledVectorStore) enqueueTask(task *compactionTask) {
	lvs.inFlightBuilds.Add(1)
	select {
	case lvs.buildQueue <- task:
	case <-lvs.done:
		lvs.inFlightBuilds.Add(-1)
	}
}

// buildWorker — пуловый воркер delta-flush задач. Получает задачу из buildQueue,
// строит сегмент, применяет результат напрямую в levels[0].
// Каждый buildWorker имеет свой дедикированный allocator — без TCMalloc race.
func (lvs *LeveledVectorStore) buildWorker() {
	workerID := int(lvs.buildWorkerIDCounter.Add(1)-1) % len(lvs.buildAllocators)
	for {
		select {
		case <-lvs.done:
			return
		case task, ok := <-lvs.buildQueue:
			if !ok {
				return
			}
			// Build: паника перехватывается, seg=nil = rollback.
			var seg segment
			func() {
				defer func() {
					if r := recover(); r != nil {
						seg = nil
					}
				}()
				seg = lvs.buildSegment(task.entries, lvs.dim, workerID)
			}()

			// Применяем результат под write-lock и декрементируем счётчик DOPO применения.
			// Важно: декремент DOPO применения, чтобы FlushDeltaSync не вернулся раньше записи данных.
			lvs.mu.Lock()
			if lvs.compactionEpoch.Load() == task.epoch && seg != nil {
				lvs.levels[0] = append(lvs.levels[0], seg)
			} else if seg == nil {
				monitoring.VectorCompactionRollbackTotal.Inc()
			}
			// Декремент счётчика DOPO применения результата.
			lvs.inFlightBuilds.Add(-1)
			// Дельта снята с flushing → будим FlushDeltaSync (ждёт по членству).
			lvs.flushCond.Broadcast()
			overflow := len(lvs.levels[0]) >= lvs.cfg.Fanout
			// Re-flush ТОЛЬКО когда дельта реально полна (без flushPending-шторма,
			// как в applyResult). Примечание: этот путь (buildQueue/buildWorker)
			// сейчас не активен — enqueueTask не вызывается; держим консистентным на
			// случай будущего включения пула.
			needsFlush := !overflow && lvs.delta != nil && lvs.delta.Full()
			lvs.mu.Unlock()
			if overflow || needsFlush {
				lvs.triggerCompact()
			}
		}
	}
}

// FlushDeltaSync гарантирует: после возврата все векторы, добавленные ДО вызова,
// находятся в опубликованных сегментах (levels[]). Merge goroutines (L1→L2 и т.д.)
// НЕ ждём — они работают в фоне.
//
// ВАЖНО (fix flush-storm): ждём публикации лишь PRE-CALL набора дельт, а НЕ полного
// опустошения дельты. Прежняя реализация крутилась пока delta не станет ПУСТОЙ И
// inFlightBuilds==0 в один момент — под непрерывной конкурентной записью это
// никогда не сходилось: applyResult пере-триггерил flush на каждом build'е, дельта
// флашилась тысячи раз по 1-2 вектора, каждый flush аллоцировал свежий
// дельта-сегмент → сотни ГБ churn (см. soak-профиль 06.07). Контракт требует лишь
// «pre-call векторы в сегментах»; их держат:
//   - target: дельта на момент входа (форсим один swap, ждём её сегмент);
//   - pending: дельты, уже находящиеся в flushing на входе (их builds в полёте).
//
// Векторы, добавленные писателями ПОСЛЕ swap, попадают в НОВУЮ дельту — в набор не
// входят, их не ждём (они durable в WAL, уедут в следующий снапшот). Это и убивает
// шторм: конечный набор ожидания вместо гонки за движущейся пустотой.
//
// flushCond.Broadcast() вызывается из applyResult() при снятии дельты с flushing и
// из Close() при graceful shutdown; FlushDeltaSync пробуждается и перепроверяет.
func (lvs *LeveledVectorStore) FlushDeltaSync() {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()

	// Pre-call набор: текущая непустая дельта + уже флашащиеся дельты.
	var target *DeltaSegment
	if lvs.delta != nil && lvs.delta.Len() > 0 {
		target = lvs.delta
	}
	pending := append([]*DeltaSegment(nil), lvs.flushing...)

	inFlushing := func(d *DeltaSegment) bool {
		for _, f := range lvs.flushing {
			if f == d {
				return true
			}
		}
		return false
	}

	for {
		// Shutdown: coordinator уже вышел, ждать некому.
		select {
		case <-lvs.done:
			return
		default:
		}

		// target опубликован, когда он больше не активная дельта И снят с flushing.
		targetDone := target == nil || (lvs.delta != target && !inFlushing(target))
		pendingDone := true
		for _, d := range pending {
			if inFlushing(d) {
				pendingDone = false
				break
			}
		}
		if targetDone && pendingDone {
			// Всё pre-call — в сегментах. Толкнём каскад merge (L0→L1→L2).
			lvs.triggerCompact()
			return
		}

		// Форсим swap target'а — но ТОЛЬКО пока он ещё активная дельта. После swap
		// повторный триггер флашил бы уже НОВЫЕ дельты = прежний шторм.
		if target != nil && lvs.delta == target {
			lvs.triggerCompact()
		}
		lvs.flushCond.Wait()
	}
}

// anyMergeInFlight проверяет, есть ли активный merge на любом уровне.
func (lvs *LeveledVectorStore) anyMergeInFlight() bool {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	return lvs.anyMergeInFlightLocked()
}

// waitForBuildCompletion — не используется, оставлен для совместимости.
func (lvs *LeveledVectorStore) waitForBuildCompletion() {
	for lvs.inFlightBuilds.Load() > 0 {
		select {
		case <-lvs.done:
			return
		default:
			runtime.Gosched()
		}
	}
}

// flushDeltaToSegment удалён — логика delta-flush интегрирована в coordinateCompaction
// (Build Pool). Coordinator забирает delta и создаёт task{taskFlushDelta}, buildWorker
// строит сегмент через buildSegment.

// mergeSegments сливает несколько сегментов в один новый.
// Ключи из tombstone-слоя физически пропускаются — это единственный момент,
// когда tombstones «применяются» и после merge перестают быть нужны.
func (lvs *LeveledVectorStore) mergeSegments(segs []segment, workerID int) segment {
	seg, _ := lvs.mergeSegmentsWithAllocator(segs, lvs.buildAllocators[workerID%len(lvs.buildAllocators)])
	return seg
}

// mergeSegmentsWithAllocator — merge с явным аллокатором (для coordinator-driven merge,
// использует lvs.mergeAllocator, изолированный от build workers).
func (lvs *LeveledVectorStore) mergeSegmentsWithAllocator(segs []segment, allocator *tcmalloc.TCMallocStore) (segment, map[string]struct{}) {
	lvs.mu.RLock()
	dim := lvs.dim
	// Snapshot tombstones под RLock — tombstones актуальны на момент начала merge.
	// Ключи, удалённые до merge, не попадают в новый сегмент.
	// Ключи, удалённые после snapshot, останутся в новом сегменте — они будут
	// в tombstones и отфильтруются при следующем Search/Get. Корректно.
	var ts map[string]struct{}
	if t := lvs.tombstones.Load(); t != nil {
		ts = *t
	}
	lvs.mu.RUnlock()

	if dim == 0 {
		return nil, nil
	}

	// applied — ключи, исключённые из нового сегмента по tombstone. Кандидаты на
	// очистку tombstone, но чистим их НЕ здесь, а в applyResult после атомарной
	// публикации сегмента и только если ключа больше нет в других сегментах.
	applied := make(map[string]struct{})

	// Извлекаем векторы, пропуская tombstoned ключи, и ДЕДУПЛИЦИРУЕМ по ключу.
	// Один ключ может жить в двух merge-входах (upsert после flush: свежая копия
	// уехала в новый сегмент, старая осталась в прежнем; оба попадают в один merge).
	// segs идут oldest→newest (caller берёт префикс levels[lyr][:batch] в порядке создания),
	// поэтому более поздняя запись СВЕЖЕЕ и перезаписывает раннюю — так stale-копия
	// физически выметается из индекса (иначе она осталась бы дублем ВНУТРИ сегмента,
	// который провенанс-дедуп поиска уже не различит — одинаковый ранг).
	var entries []DeltaEntry
	seen := make(map[string]int)
	addEntry := func(key string, vec []float32, attrs Attributes, terms []TermTF) {
		if pos, ok := seen[key]; ok {
			entries[pos] = DeltaEntry{Key: key, Vec: vec, Attrs: attrs, Terms: terms}
			return
		}
		seen[key] = len(entries)
		entries = append(entries, DeltaEntry{Key: key, Vec: vec, Attrs: attrs, Terms: terms})
	}
	// Текст (шаг 6): постинги сегмента декодируются в пер-доковые термы ОДИН раз
	// на входной сегмент (decodeTerms — обратный проход buildSegmentText), едут в
	// entries и пересобираются build'ом заново. Слияние листов напрямую невозможно:
	// frozen id перенумеровываются, tombstoned/затенённые копии выпадают, тенант-
	// сорт меняет порядок. Побочный эффект пересборки — нормализация статистики:
	// df/N/avgdl завышенные затенёнными копиями возвращаются к правде (конец
	// «принятой семантики» шага 4). Все входы без текста → termsAt даёт nil →
	// buildSegmentText вернёт nil (zero overhead сохраняется).
	termsAt := func(dec [][]TermTF, i int) []TermTF {
		if i < len(dec) {
			return dec[i]
		}
		return nil
	}
	for _, seg := range segs {
		switch s := seg.(type) {
		case *frozenSegment:
			fg := s.fg
			decTerms := s.text.decodeTerms()
			for i := 0; i < fg.n; i++ {
				if fg.keys.view(i) == "" {
					continue
				}
				if ts != nil {
					if _, deleted := ts[fg.keys.view(i)]; deleted {
						applied[fg.keys.clone(i)] = struct{}{}
						continue // tombstone — физически удаляем
					}
				}
				vec := fg.data[i*fg.dim : (i+1)*fg.dim]
				vecCopy := make([]float32, dim)
				copy(vecCopy, vec)
				addEntry(fg.keys.clone(i), vecCopy, s.attrs.decodeAt(i), termsAt(decTerms, i))
			}
		case *frozenSQSegment:
			// Деквантуем SQ8-векторы обратно в float32 для rebuild.
			// Это единственная операция, теряющая точность: деквантованный вектор
			// ≈ исходный с точностью SQ8 (error ≤ scale/2 per dimension).
			// При rebuild новый сегмент квантуется заново — накопления ошибки нет.
			fg := s.fg
			decTerms := s.text.decodeTerms()
			for i := 0; i < fg.n; i++ {
				if fg.keys.view(i) == "" {
					continue
				}
				if ts != nil {
					if _, deleted := ts[fg.keys.view(i)]; deleted {
						applied[fg.keys.clone(i)] = struct{}{}
						continue
					}
				}
				vecCopy := make([]float32, dim)
				base := i * dim
				for d := 0; d < dim; d++ {
					vecCopy[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]
				}
				addEntry(fg.keys.clone(i), vecCopy, s.attrs.decodeAt(i), termsAt(decTerms, i))
			}
		case *hnswSegment:
			s.mu.RLock()
			g := s.g
			// node id = frozen id текстового слоя (build без шафла) — тот же
			// индекс, что у attrs.decodeAt.
			decTerms := s.text.decodeTerms()
			for i := range g.nodes {
				if !g.nodes[i].Alive {
					continue
				}
				key, ok := s.keys[uint64(i)]
				if !ok || key == "" {
					continue
				}
				if ts != nil {
					if _, deleted := ts[key]; deleted {
						applied[key] = struct{}{}
						continue // tombstone — физически удаляем
					}
				}
				raw := g.arena.Get(g.nodes[i].VectorOffset)
				vecCopy := make([]float32, dim)
				copy(vecCopy, raw)
				addEntry(key, vecCopy, s.attrs.decodeAt(i), termsAt(decTerms, i))
			}
			s.mu.RUnlock()
		}
	}

	if len(entries) == 0 {
		// Все ключи входа tombstoned. Сегмент не публикуется (applyResult сделает
		// rollback: oldSegs остаются в levels) → ключи физически живы → tombstones
		// НЕ чистим. Возвращаем applied=nil, чтобы clearAppliedTombstonesLocked не
		// сработал (он и так гейтован r.result != nil).
		return nil, nil
	}

	newSeg := lvs.buildSegmentWithAllocator(entries, dim, allocator)

	// Очистку применённых tombstones делает applyResult ПОСЛЕ атомарной публикации
	// сегмента (свап под mu.Lock) и только для ключей, отсутствующих в остальных
	// сегментах. Внутри merge-горутины НЕ чистим: (#3) при дропе результата (epoch-
	// mismatch/rollback) tombstone обязан сохраниться.
	return newSeg, applied
}

// buildSegment строит сегмент из набора (key, vec) записей.
// Dim-adaptive: dim ≤ csrDimThreshold → FrozenGraph, иначе → hnswSegment.
func (lvs *LeveledVectorStore) buildSegment(entries []DeltaEntry, dim, workerID int) segment {
	n := len(entries)
	if n == 0 {
		return nil
	}
	// Выбираем per-worker изолированный аллокатор.
	allocator := lvs.cfg.Allocator
	if workerID >= 0 && workerID < len(lvs.buildAllocators) {
		allocator = lvs.buildAllocators[workerID]
	}
	if allocator == nil {
		allocator = tcmalloc.NewTCMallocStore(1)
	}
	return lvs.buildSegmentWithAllocator(entries, dim, allocator)
}

// buildSegmentWithAllocator — ядро построения сегмента с явным аллокатором.
// Используется build workers (per-worker allocators) и coordinator-merge (mergeAllocator).
func (lvs *LeveledVectorStore) buildSegmentWithAllocator(entries []DeltaEntry, dim int, allocator *tcmalloc.TCMallocStore) segment {
	n := len(entries)
	if n == 0 {
		return nil
	}

	buildStart := time.Now()
	defer func() {
		monitoring.VectorSegmentBuildDuration.Update(time.Since(buildStart).Seconds())
	}()

	// Тенант-локальная раскладка (Вещь 1): #5 ведущий ключ компакции.
	// Сортируем entries по коду тенанта ДО вставки в граф → node id (= frozen id)
	// идут в тенант-порядке → каждый тенант непрерывным блоком. Затем строим
	// каталог диапазонов (#6 тип индекса по размеру) и вешаем на сегмент.
	var cat *TenantCatalog
	var attrs *segmentAttrs
	// presets переиспользует партиционный dict в колонке, если тенант-раскладка
	// строится (коды каталога обязаны совпасть с кодами колонки — иначе SearchFilter
	// не найдёт блок). Без партиции остаётся nil → колонки строят свои dict.
	var presets map[string]*attrDict
	if lvs.cfg.TenantOf != nil || lvs.cfg.PartitionAttr != "" {
		// Тенант-раскладка (Вещь 1): сортируем entries по коду тенанта и строим
		// каталог диапазонов. Источник кода: партиционный атрибут либо legacy-ключ.
		var tenantCode func(DeltaEntry) uint64
		if lvs.cfg.PartitionAttr != "" {
			pa := lvs.cfg.PartitionAttr
			pd := newAttrDict()
			tenantCode = func(e DeltaEntry) uint64 { return uint64(pd.intern(e.Attrs.Cat[pa])) }
			presets = map[string]*attrDict{pa: pd}
		} else {
			tenantCode = tenantByKey(lvs.cfg.TenantOf)
		}
		sortEntriesByTenant(entries, tenantCode)
		c, err := buildTenantCatalog(entries, tenantCode, lvs.cfg.BruteThreshold, dim)
		if err != nil {
			// Инвариант #5 нарушен (entries не сгруппированы после сортировки) —
			// это баг сортировки/деривации тенанта, не данных. Фейлим build:
			// coordinator откатит (seg=nil), сегмент не публикуется битым.
			monitoring.VectorSegmentBuildDuration.Update(0)
			return nil
		}
		cat = c
	}
	// Колоночный слой строим ВСЕГДА, когда в entries есть атрибуты — НЕЗАВИСИМО от
	// партиции. Иначе SearchFilter, работавший на свежих данных в дельте, молча
	// выпадал бы после flush (attrs жили только в дельте). buildSegmentAttrs
	// возвращает nil, если атрибутов нет (zero-overhead для чистых векторов).
	// entries к этому моменту уже отсортированы (если была тенант-раскладка) →
	// frozen id = позиция, колонки выровнены.
	attrs = buildSegmentAttrs(entries, presets)

	// Текстовый BM25-слой — тем же правилом, что attrs: всегда, когда в entries
	// есть термы (nil иначе — zero overhead). entries уже в финальном порядке →
	// постинги лягут по frozen id. Merge-путь кладёт Terms декодом постингов
	// входных сегментов (mergeSegmentsWithAllocator → decodeTerms).
	text := buildSegmentText(entries)

	// Строим HNSW из всех векторов.
	g := NewGraph(lvs.cfg.Distance, allocator)
	g.workerID = 0 // изолированный allocator: всегда shard 0
	g.EfConstruction = lvs.cfg.EfConstruction

	// Применяем параметр M если задан
	if lvs.cfg.M > 0 {
		g.M = lvs.cfg.M
		g.M0 = 2 * lvs.cfg.M
		g.Ml = 1.0 / math.Log(float64(lvs.cfg.M))
		g.pruneBufItems = make([]item, 0, g.M0+1)
		g.pruneBufIDs = make([]uint64, 0, g.M0+1)
		g.insertBuf = make([]uint64, 0, g.M0+1)
	}

	// Порядок ВСТАВКИ в граф перемешиваем (детерминированно по n): entries к этому
	// моменту блочно отсортированы (тенант-сорт выше; у клиента — батчи похожих
	// данных), а HNSW-построение деградирует на блочном порядке мультимодальных
	// данных: блоки разных распределений не образуют кросс-рёбер, и у части вершин
	// раннего блока beam-поиск позже не находит даже точное совпадение (soak
	// searchmiss 7/400 детерминированно; форензика 07.2026: miss 8/1007 при
	// блочном порядке vs 0/1007 при перемешанном на том же наборе). Раскладка
	// данных ОСТАЁТСЯ отсортированной — каталог тенантов и колоночные атрибуты
	// адресуют позицию в entries; граф переупорядочивается при freeze через pos.
	// Для hnswSegment (без freeze) порядок не трогаем: там graph id — источник
	// адресации, перестановка ломала бы каталог.
	willFreeze := lvs.cfg.UseSQ || dim <= csrDimThreshold
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	var pos []uint32
	if willFreeze && n > 1 {
		rand.New(rand.NewSource(0x5eed<<32|int64(n))).Shuffle(n, func(a, b int) {
			order[a], order[b] = order[b], order[a]
		})
		pos = make([]uint32, n)
	}

	keys := make(map[uint64]string, n)
	for _, ei := range order {
		idx := g.Insert(entries[ei].Vec)
		if pos != nil {
			pos[idx] = uint32(ei)
		}
		keys[uint64(idx)] = entries[ei].Key
	}

	// Repair-pass: детерминированная гарантия self-findability при рабочем
	// efSearch. Шафл выше снимает систематическую порядкозависимость, repair
	// добивает вероятностный хвост мультимодальной геометрии (см.
	// Graph.RepairReachability). Фоновый билд — бюджет CPU есть.
	g.RepairReachability(lvs.cfg.EfSearch)

	// Выбор формата сегмента (frozen uint32 neighbor IDs — потолка n нет):
	//   UseSQ=true → SQ8-frozen для ЛЮБОЙ размерности. На dim>256 это и есть решение
	//     памяти для 768 (4× сжатие: 3072→768 B/vec). SQ-гейт по ПОЛЬЗЕ, не по dim.
	//   UseSQ=false → float32-frozen только при dim ≤ csrDimThreshold (там CSR даёт
	//     +QPS за счёт cache-locality); иначе обычный hnswSegment.
	if lvs.cfg.UseSQ {
		fg := FreezeGraphSQOrdered(g, lvs.cfg.Metric, keys, pos)
		if fg == nil {
			return nil
		}
		return &frozenSQSegment{fg: fg, cat: cat, attrs: attrs, text: text}
	}
	if dim <= csrDimThreshold {
		fg := FreezeGraphOrdered(g, lvs.cfg.Distance, keys, pos)
		if fg == nil {
			return nil
		}
		return &frozenSegment{fg: fg, cat: cat, attrs: attrs, text: text}
	}

	return &hnswSegment{g: g, keys: keys, cat: cat, attrs: attrs, text: text}
}

// =============================================================================
// Утилиты
// =============================================================================

// LeveledStats — снимок состояния LeveledVectorStore для метрик/диагностики.
// Возвращается методом Stats(). Не зависит от пакета monitoring (нет import cycle).
type LeveledStats struct {
	TotalVectors    int   // живых векторов (delta + все сегменты)
	DeltaLen        int   // векторов в активной дельте
	DeltaMax        int   // ёмкость дельты до flush
	Dim             int   // размерность векторов (0 если пусто)
	MaxLevel        int   // максимальный заполненный уровень LSM (0-based)
	SegmentsByLevel []int // [maxLevels] — число сегментов на каждом уровне
	Tombstones      int   // активных tombstones (не применённых merge'ем)
	AllocatorBytes  int64 // память под neighbors-блоки (tcmalloc)
	DataBytes       int64 // память под векторы (frozen.data + hnsw arena)
}

// Stats возвращает snapshot состояния для метрик.
// Потокобезопасен: берёт RLock на короткое время. Не делает аллокаций вне
// возвращаемого среза SegmentsByLevel (фиксированной длины maxLevels).
func (lvs *LeveledVectorStore) Stats() LeveledStats {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	segsByLevel := make([]int, maxLevels)
	totalVectors := 0
	maxLevel := 0
	var dataBytes int64
	for i, level := range lvs.levels {
		segsByLevel[i] = len(level)
		if len(level) > 0 && i > maxLevel {
			maxLevel = i
		}
		for _, seg := range level {
			totalVectors += seg.Len()
			dataBytes += segMemoryBytes(seg)
		}
	}

	deltaLen := 0
	deltaMax := 0
	if lvs.delta != nil {
		deltaLen = lvs.delta.Len()
		deltaMax = lvs.delta.MaxSize()
		totalVectors += deltaLen
	}

	tombstones := 0
	if t := lvs.tombstones.Load(); t != nil {
		tombstones = len(*t)
	}

	return LeveledStats{
		TotalVectors:    totalVectors,
		DeltaLen:        deltaLen,
		DeltaMax:        deltaMax,
		Dim:             lvs.dim,
		MaxLevel:        maxLevel,
		SegmentsByLevel: segsByLevel,
		Tombstones:      tombstones,
		DataBytes:       dataBytes,
	}
}

// segMemoryBytes возвращает приближённый размер данных векторов в сегменте.
// Не учитывает overhead структуры (keys []string, метаданные) — только payload.
func segMemoryBytes(seg segment) int64 {
	switch s := seg.(type) {
	case *frozenSegment:
		// data slab (n*dim*4) + CSR layers
		return int64(s.fg.MemoryBytes())
	case *frozenSQSegment:
		// codes (n*dim*1) + sqMin/sqScale + CSR layers
		return int64(s.fg.MemoryBytes())
	case *hnswSegment:
		s.mu.RLock()
		defer s.mu.RUnlock()
		// arena.data (float32) + tcmalloc neighbors (через allocator, не здесь)
		return int64(len(s.g.arena.data)) * 4
	}
	return 0
}

type dimMismatchError struct {
	expected, got int
}

func (e *dimMismatchError) Error() string {
	return "vector dimension mismatch: expected " +
		itoa(e.expected) + ", got " + itoa(e.got)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := 20
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
