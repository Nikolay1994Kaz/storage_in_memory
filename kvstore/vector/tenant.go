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

// DefaultBruteThreshold — порог #6 по умолчанию (абсолютный размер блока).
// Калибровано на реальном SIFT-128/N=200k (TestTenant_CalibrateBruteThreshold):
// кроссовер brute/graph ≈32k при recall=1.0 у обоих, но штраф асимметричен
// (graph на малом блоке ×300 vs brute на крупном ×5.5) → дефолт держим ниже
// кроссовера. 16384 — безопасное значение в зоне уверенной победы brute.
const DefaultBruteThreshold = 16384

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
func buildTenantCatalog(entries []DeltaEntry, tenantOf func(DeltaEntry) uint64, bruteThreshold int) (*TenantCatalog, error) {
	if bruteThreshold <= 0 {
		bruteThreshold = DefaultBruteThreshold
	}
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
