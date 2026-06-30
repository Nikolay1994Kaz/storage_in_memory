package vector

import (
	"fmt"
	"slices"
	"sort"
)

// =============================================================================
// Tenant layout — «Вещь 1»: тенант-локальная раскладка внутри сегмента.
//
// Идея (пункт #5/#7/#6 из бенчей фильтрации 2026-06-28):
//
//   #5  Та же компакция, что и так пересобирает сегмент, ЗАОДНО раскладывает
//       векторы по тенантам непрерывными диапазонами. Консолидация не разрушает
//       тенант-локальность — она её СОЗДАЁТ, если ведущий ключ сортировки = tenant.
//   #7  Маленький тенант → линейный перебор (brute) ТОЛЬКО по его диапазону, не
//       трогая чужие 99% корпуса. Это главный рычаг (15–50× для малого тенанта).
//   #6  Выбор brute vs HNSW делается по АБСОЛЮТНОМУ размеру блока, не по проценту.
//
// Этот файл — переиспользуемое ядро, НЕ зашитое в горячий путь стора. Оно даёт:
//   sortEntriesByTenant  — «поменять comparator» при merge (создаёт контигуозность);
//   buildTenantCatalog   — каталог тенант → (диапазон, тип индекса);
//   verifyContiguous     — исполняемый инвариант #5 (тест ломается, если раскладка
//                          разъехалась);
//   blockBruteSearch     — нижний режим (#7): точный top-K по диапазону тенанта.
//
// Интеграция в mergeSegments/Search — следующий инкремент (за конфиг-гейтом),
// после того как ядро доказано тестами и кроссовер перемерян на консолидированном
// сторе (открытый шаг #1).
// =============================================================================

// IndexKind — как обслуживается блок тенанта.
type IndexKind uint8

const (
	// IndexBrute — маленький блок: линейный перебор по непрерывному диапазону (#7).
	IndexBrute IndexKind = iota
	// IndexGraph — большой блок: HNSW-обход (свой граф на блок/сегмент).
	IndexGraph
)

func (k IndexKind) String() string {
	if k == IndexBrute {
		return "brute"
	}
	return "graph"
}

// DefaultBruteThreshold — порог #6 по умолчанию (размер блока) НА ЭТАЛОННОЙ
// размерности bruteDimRef. Калибровано на реальном SIFT-128/N=200k
// (TestTenant_CalibrateBruteThreshold): кроссовер brute/graph ≈32k при recall=1.0
// у обоих, но штраф асимметричен (graph на малом блоке ×300 vs brute на крупном
// ×5.5) → дефолт держим ниже кроссовера. 16384 — безопасное значение в зоне
// уверенной победы brute ПРИ dim=128.
const DefaultBruteThreshold = 16384

// bruteDimRef — эталонная размерность, при которой калибровался DefaultBruteThreshold
// (SIFT-128). Порог масштабируется относительно неё (см. effectiveBruteThreshold).
const bruteDimRef = 128

// residualBruteFactor — бюджет residual-brute как множитель efSearch. Filtered-HNSW
// тратит ~efSearch×(структурный множитель) дистанций независимо от селективности
// остаточного предиката. Brute выигрывает при matched ≤ efSearch×residualBruteFactor.
// В отличие от блок-порога #1 — НЕ dim-aware: кроссовер по ЧИСЛУ дистанций, множитель
// dim сокращается (и у brute, и у графа дистанция дорожает одинаково). Калибровано на
// dbpedia-1536 (эмпирич. кроссовер ≈45×ef); 32 (=4096 при ef=128) — консервативно.
//
// ⚠ ВАЖНО (TestDBpedia_FilterTenant сквозной, 30.06): bruteRangeAttr идёт по ВСЕМУ
// блоку тенанта [start,end) — distFn только на matched, но проход и cache-gather —
// O(block), а matched РАЗБРОСАНЫ по блоку (сортировка лишь по тенанту, не по
// region/price). Поэтому поднятие бюджета НЕ даёт ускорения уровня «компактного»
// brute по matched: B40k matched 5000 включил block-brute = 288 QPS (vs graph ~234,
// лишь ~1.2×), а НЕ 2530, как показывал ceiling над КОМПАКТНЫМ срезом
// (TestLargeTenant_FilterCeiling). Тот 2530 = потолок ВТОРИЧНОЙ contiguous-раскладки
// (sort tenant→region→price → matched в подотрезке → brute O(matched)) — это рычаг
// ~9×, но он про layout, НЕ про эту константу. Бюджет держим на 32.
// См. [[vector-large-tenant-ceiling-20260630]].
const residualBruteFactor = 32

// residualBruteSelDenom — порог селективности остатка: residual-brute включается
// лишь если matched ≤ block/residualBruteSelDenom (остаток разрежен в блоке). При
// плотном остатке (matched≈block) случай вырождается в single-attr и должен идти
// блок-роутингом (#1); brute там проиграл бы графу на среднем блоке.
const residualBruteSelDenom = 4

// effectiveBruteThreshold масштабирует размер-блок-порог к размерности вектора.
// Стоимость линейного перебора ∝ block×dim, поэтому кроссовер brute/graph по числу
// векторов ∝ 1/dim: на high-dim дистанция дороже, brute выгоден на МЕНЬШЕМ блоке.
// Якорь — bruteDimRef (порог калибровался на dim=128). Примеры при threshold=16384:
// dim=128 → 16384 (без изменений); dim=1536 → ~1365; dim=64 → 32768.
//
// Закрывает яму, найденную на dbpedia-1536 (B≈16k brute проигрывал графу 0.2×):
// фиксированный 16384 не был dim-aware и слал крупный high-dim блок в brute.
func effectiveBruteThreshold(threshold, dim int) int {
	if threshold <= 0 {
		threshold = DefaultBruteThreshold
	}
	if dim <= 0 {
		return threshold
	}
	t := threshold * bruteDimRef / dim
	if t < 1 {
		t = 1
	}
	return t
}

// TenantRange — непрерывный диапазон одного тенанта внутри сегмента.
// [Start, End) — полуинтервал по индексам векторов в раскладке сегмента.
type TenantRange struct {
	Tenant uint64
	Start  int // включительно
	End    int // исключительно
	Kind   IndexKind
}

// Len — число векторов в блоке тенанта.
func (r TenantRange) Len() int { return r.End - r.Start }

// TenantCatalog — тенант → его непрерывный диапазон в сегменте + тип индекса.
// Это новая метадата, которую компакция обновляет, выводя тенанта в непрерывный
// диапазон и проставляя тип индекса по порогу #6.
type TenantCatalog struct {
	ranges map[uint64]TenantRange
	// bruteThreshold — граница #6: Len ≤ threshold → brute, иначе graph.
	bruteThreshold int
}

// Lookup возвращает диапазон тенанта. ok=false — тенанта нет в сегменте.
func (c *TenantCatalog) Lookup(tenant uint64) (TenantRange, bool) {
	r, ok := c.ranges[tenant]
	return r, ok
}

// Tenants — число тенантов в каталоге.
func (c *TenantCatalog) Tenants() int { return len(c.ranges) }

// BruteThreshold — порог классификации brute/graph (#6).
func (c *TenantCatalog) BruteThreshold() int { return c.bruteThreshold }

// All возвращает диапазоны, отсортированные по Start (детерминированный обход).
func (c *TenantCatalog) All() []TenantRange {
	out := make([]TenantRange, 0, len(c.ranges))
	for _, r := range c.ranges {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// sortEntriesByTenant — ВЕДУЩИЙ КЛЮЧ компакции (#5). Стабильная сортировка записей
// по коду тенанта: после неё все записи одного тенанта лежат непрерывным блоком,
// а внутри блока сохраняется исходный относительный порядок (важно для повторяемости
// и для того, чтобы граф/SQ строились из стабильной раскладки).
//
// Это и есть «поменять comparator при merge». Вызывать на entries ДО buildSegment.
func sortEntriesByTenant(entries []DeltaEntry, tenantOf func(DeltaEntry) uint64) {
	slices.SortStableFunc(entries, func(a, b DeltaEntry) int {
		ta, tb := tenantOf(a), tenantOf(b)
		switch {
		case ta < tb:
			return -1
		case ta > tb:
			return 1
		default:
			return 0
		}
	})
}

// tenantByKey адаптирует key-based деривацию тенанта к entry-based сигнатуре
// (legacy cfg.TenantOf и тесты, где тенант закодирован в ключе).
func tenantByKey(f func(string) uint64) func(DeltaEntry) uint64 {
	return func(e DeltaEntry) uint64 { return f(e.Key) }
}

// buildTenantCatalog строит каталог по записям, СГРУППИРОВАННЫМ по тенанту
// (т.е. после sortEntriesByTenant). Каждому тенанту проставляется Kind по порогу
// bruteThreshold (#6): блок ≤ порога → brute, иначе graph.
//
// Если записи НЕ сгруппированы (тенант встречается двумя несмежными кусками) —
// возвращает ошибку: это нарушение инварианта #5, которое нельзя молча проглотить
// (иначе каталог укажет на неверный диапазон). Перед вызовом гарантируй сортировку.
func buildTenantCatalog(entries []DeltaEntry, tenantOf func(DeltaEntry) uint64, bruteThreshold, dim int) (*TenantCatalog, error) {
	// dim-aware: храним УЖЕ масштабированный к размерности порог, чтобы и build-time
	// классификация Kind, и query-time residual-brute (searchFilterFrozen) пользовались
	// одним эффективным значением.
	bruteThreshold = effectiveBruteThreshold(bruteThreshold, dim)
	cat := &TenantCatalog{
		ranges:         make(map[uint64]TenantRange),
		bruteThreshold: bruteThreshold,
	}
	if len(entries) == 0 {
		return cat, nil
	}

	start := 0
	cur := tenantOf(entries[0])
	flush := func(end int) error {
		if _, dup := cat.ranges[cur]; dup {
			return fmt.Errorf("tenant %d встречается несмежными блоками — entries не сгруппированы (нужен sortEntriesByTenant)", cur)
		}
		kind := IndexBrute
		if end-start > bruteThreshold {
			kind = IndexGraph
		}
		cat.ranges[cur] = TenantRange{Tenant: cur, Start: start, End: end, Kind: kind}
		return nil
	}

	for i := 1; i < len(entries); i++ {
		t := tenantOf(entries[i])
		if t != cur {
			if err := flush(i); err != nil {
				return nil, err
			}
			start = i
			cur = t
		}
	}
	if err := flush(len(entries)); err != nil {
		return nil, err
	}
	return cat, nil
}

// verifyContiguous — ИСПОЛНЯЕМЫЙ ИНВАРИАНТ #5: каждый тенант занимает ровно один
// непрерывный диапазон. Возвращает ошибку, если тенант появляется двумя несмежными
// кусками. Используется тестами компакции как защёлка: если кто-то поменяет порядок
// раскладки и разорвёт блок тенанта — тест падает.
func verifyContiguous(entries []DeltaEntry, tenantOf func(DeltaEntry) uint64) error {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{})
	cur := tenantOf(entries[0])
	seen[cur] = struct{}{}
	for i := 1; i < len(entries); i++ {
		t := tenantOf(entries[i])
		if t == cur {
			continue
		}
		if _, dup := seen[t]; dup {
			return fmt.Errorf("инвариант #5 нарушен: тенант %d разорван (повторно встречен на позиции %d)", t, i)
		}
		seen[t] = struct{}{}
		cur = t
	}
	return nil
}

// blockBruteSearch — нижний режим (#7): точный top-K ПО ДИАПАЗОНУ тенанта.
// data — flat-раскладка сегмента (data[i*dim:(i+1)*dim] = вектор i, в том же порядке,
// что и entries после sortEntriesByTenant); keys[i] — ключ вектора i. r — диапазон
// тенанта из каталога. Перебирается только [r.Start, r.End) — чужие векторы не
// трогаются вообще (в отличие от фильтрованного скана с предикатом на каждый вектор).
//
// recall = 1.0 по построению (точный перебор). Результаты отсортированы по дистанции.
func blockBruteSearch(data []float32, keys []string, dim int, r TenantRange, query []float32, K int, distFn DistanceFunc) []FrozenResult {
	if K <= 0 || r.Len() == 0 {
		return nil
	}
	// top-K через простую вставку (K мал; блок мал по построению режима brute).
	top := make([]FrozenResult, 0, K+1)
	for i := r.Start; i < r.End; i++ {
		if keys[i] == "" {
			continue // tombstone
		}
		d := distFn(query, data[i*dim:(i+1)*dim])
		top = insertTopK(top, K, keys[i], d)
	}
	return top
}

// insertTopK вставляет кандидата (key,d) в отсортированный по дистанции срез top
// длины ≤ K, поддерживая инвариант сортировки. O(K) на вставку. Используется
// всеми brute-путями (blockBruteSearch и FrozenGraph(SQ).bruteRange).
func insertTopK(top []FrozenResult, K int, key string, d float32) []FrozenResult {
	if len(top) < K {
		top = append(top, FrozenResult{Key: key, Dist: d})
		for j := len(top) - 1; j > 0 && top[j].Dist < top[j-1].Dist; j-- {
			top[j], top[j-1] = top[j-1], top[j]
		}
		return top
	}
	if d >= top[K-1].Dist {
		return top
	}
	pos := K - 1
	for pos > 0 && top[pos-1].Dist > d {
		top[pos] = top[pos-1]
		pos--
	}
	top[pos] = FrozenResult{Key: key, Dist: d}
	return top
}
