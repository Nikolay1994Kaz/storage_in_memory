//go:build datasets

// Замер на внешнем датасете. Тег `datasets` намеренно держит его ВНЕ обычной
// сборки тестов: без файла в /tmp такой тест скипался молча, и «ok» пакета
// означал в том числе «тридцать функций даже не пытались запуститься».
// Запуск и получение данных — docs/BENCHMARKS.md, раздел Reproducing:
//
//	make test-datasets

package vector

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// Фильтры/тенанты на РЕАЛЬНОЙ cosine-геометрии — dbpedia-openai (ada-002 1536d,
// L2-нормализованные). Закрывает единственный непокрытый угол валидации:
// SearchFilter/SearchTenant мерили только на SIFT (euclidean, изотропный) —
// см. [[vector-filter-at-scale-20260629]]. Здесь тот же боевой колоночный путь,
// но на анизотропной трансформерной геометрии + dot-ADC (после [[vector-metric-enum]]).
//
// Главный вопрос: держится ли recall фильтрации (на SIFT был ≈1.0) на cosine+SQ8,
// где blocked-brute (#7) идёт по int8-кодам через dot-ADC — путь, который на
// euclidean был recall=1.0, но на cosine НЕ проверялся. Если просядет — находка
// (нужен rerank/нормализация в brute-блоке).
//
//   baseline = SearchFiltered(strPred): полный HNSW-обход ×4 ef + строковый предикат.
//   prod     = SearchFilter(колонки) / SearchTenant (каталог-роутинг).
//
//   go test ./kvstore/vector -run TestDBpedia_FilterTenant -v -timeout 3600s
//
// Требует /tmp/dbpedia100k.bin (scripts/convert_dbpedia.py). Вектора нормализованы
// → euclidean-GT == cosine-GT (монотонность), переиспользуем topKFilteredIdx как есть.
// attrLayout/filterGTByPred/recallStorePred — общие с filter_scale_test.go.
// =============================================================================

func TestDBpedia_FilterTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("long: строит 100k×1536 HNSW (float32 и SQ8) + QPS sweep")
	}
	train, test, _, err := loadDBpediaRaw("/tmp/dbpedia100k.bin")
	if err != nil {
		t.Skipf("нет данных: %v (scripts/convert_dbpedia.py)", err)
	}
	dim := len(train[0])

	const (
		K   = 10
		ef  = 128
		W   = 12
		dur = 2 * time.Second
	)

	// Тенант-блоки под корпус 100k: два ниже порога 16384 (brute), два выше (graph).
	blocks := []int{1000, 16000, 30000, 40000} // сумма 87000 < 100k (t0 — фон)
	totalTargets := 0
	for _, b := range blocks {
		totalTargets += b
	}
	if totalTargets >= len(train) {
		t.Fatalf("сумма блоков %d ≥ корпуса %d", totalTargets, len(train))
	}

	// Синтетические атрибуты на нормализованные вектора (GT их не касается —
	// фильтр применяется к индексам, метрика поиска неизменна).
	type kv struct {
		key   string
		vec   []float32
		attrs Attributes
	}
	all := make([]kv, len(train))
	for i, v := range train {
		tn, rg, pr := attrLayout(i, blocks)
		all[i] = kv{
			key: strconv.Itoa(i),
			vec: v,
			attrs: Attributes{
				Cat: map[string]string{"tenant": tn, "region": rg},
				Num: map[string]float64{"price": pr},
			},
		}
	}
	// Перемешать порядок вставки — сортировка по тенанту при компакции обязана
	// восстановить контигуозность блоков.
	rng := rand.New(rand.NewSource(20260629))
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })

	tenantOfId := func(i int) string { tn, _, _ := attrLayout(i, blocks); return tn }
	regionOfId := func(i int) string { _, rg, _ := attrLayout(i, blocks); return rg }
	priceOfId := func(i int) float64 { _, _, pr := attrLayout(i, blocks); return pr }

	nQ := min(200, len(test))
	queries := test[:nQ]

	fmt.Println()
	fmt.Printf("DBPEDIA FILTER/TENANT — ada-002 %d-dim cosine(norm.dot), N=%d, q=%d, ef=%d, K=%d, W=%d\n",
		dim, len(train), nQ, ef, K, W)
	fmt.Printf("blocks=%v (порог brute=%d), PartitionAttr=tenant\n", blocks, DefaultBruteThreshold)
	fmt.Println("base=SearchFiltered(str-pred), prod=SearchFilter(колонки)/SearchTenant(каталог)")

	run := func(name string, useSQ bool) {
		cfg := LeveledConfig{
			Metric:         MetricCosine, // явная метрика (нормализ. → dot-ADC), не вывод по указателю
			Allocator:      tcmalloc.NewTCMallocStore(8),
			DeltaMax:       len(train) + 1, // один консолидированный сегмент
			Fanout:         4,
			EfConstruction: arEfConstruction,
			EfSearch:       ef,
			PartitionAttr:  "tenant", // колоночный слой ведёт раскладку/каталог (без TenantOf)
			BruteThreshold: DefaultBruteThreshold,
			UseSQ:          useSQ,
		}
		lvs := NewLeveledVectorStore(cfg)

		tBuild := time.Now()
		for _, e := range all {
			if err := lvs.AddWithAttrs(e.key, e.vec, e.attrs); err != nil {
				t.Fatalf("[%s] AddWithAttrs: %v", name, err)
			}
		}
		settle(lvs)
		buildDur := time.Since(tBuild)

		st := lvs.Stats()
		nseg := 0
		for _, n := range st.SegmentsByLevel {
			nseg += n
		}

		fmt.Println()
		fmt.Printf("───── %s ─────  segs=%d  build=%.1fs  mem=%.0fMB%s\n",
			name, nseg, buildDur.Seconds(),
			float64(st.DataBytes+st.AllocatorBytes)/1e6,
			map[bool]string{true: "  ⚠segs≠1 — brute-выигрыш занижен фрагментацией", false: ""}[nseg != 1])

		// ── A) SINGLE-ATTR: Filter{Eq:{tenant}} ──
		// SearchTenant здесь НЕ применим: при PartitionAttr каталог keyed по per-segment
		// dict-коду, а не по TenantOf — SearchTenant вернёт errTenantWithPartition.
		// Тенант-адресация при колоночном слое идёт через SearchFilter (резолв строки
		// через dict сегмента). См. [[vector-tenant-searchtenant-footgun]].
		fmt.Printf("A) single-attr tenant:\n")
		fmt.Printf("  %-9s %-7s %-11s %-11s %-9s %-9s %-9s\n",
			"tenant_B", "route", "base_QPS", "filt_QPS", "speedup", "base_rec", "filt_rec")
		for bi := range blocks {
			tid := bi + 1
			tn := "t" + strconv.Itoa(tid)
			B := blocks[bi]
			route := "brute"
			if B > cfg.BruteThreshold {
				route = "graph"
			}

			strPred := func(key string) bool { i, _ := strconv.Atoi(key); return tenantOfId(i) == tn }
			filt := Filter{Eq: map[string]string{"tenant": tn}}
			matchId := func(i int) bool { return tenantOfId(i) == tn }

			gt := filterGTByPred(train, queries, K, matchId)
			baseRec := recallStorePred(queries, gt, K, func(q []float32) []VSearchResult {
				r, _ := lvs.SearchFiltered(q, K, strPred, nil)
				return r
			})
			filtRec := recallStorePred(queries, gt, K, func(q []float32) []VSearchResult {
				r, _ := lvs.SearchFilter(q, K, filt)
				return r
			})
			baseQPS := runQPS(W, dur, queries, func(q []float32) { _, _ = lvs.SearchFiltered(q, K, strPred, nil) })
			filtQPS := runQPS(W, dur, queries, func(q []float32) { _, _ = lvs.SearchFilter(q, K, filt) })

			fmt.Printf("  %-9d %-7s %-11.0f %-11.0f %-9.1f %-9.3f %-9.3f\n",
				B, route, baseQPS, filtQPS, filtQPS/baseQPS, baseRec, filtRec)
		}

		// ── B) MULTI-ATTR: tenant ∧ region=r0 ∧ price∈[0,499] ──
		fmt.Printf("B) multi-attr tenant∧region=r0∧price∈[0,499]:\n")
		fmt.Printf("  %-9s %-7s %-7s %-11s %-11s %-9s %-9s %-9s\n",
			"tenant_B", "route", "#pass", "base_QPS", "filt_QPS", "speedup", "base_rec", "filt_rec")
		priceLo, priceHi := 0.0, 499.0
		for bi := range blocks {
			tid := bi + 1
			tn := "t" + strconv.Itoa(tid)
			B := blocks[bi]
			route := "brute"
			if B > cfg.BruteThreshold {
				route = "graph"
			}

			matchId := func(i int) bool {
				return tenantOfId(i) == tn && regionOfId(i) == "r0" &&
					priceOfId(i) >= priceLo && priceOfId(i) <= priceHi
			}
			strPred := func(key string) bool { i, _ := strconv.Atoi(key); return matchId(i) }
			filt := Filter{
				Eq:    map[string]string{"tenant": tn, "region": "r0"},
				Range: map[string][2]float64{"price": {priceLo, priceHi}},
			}

			nPass := 0
			for i := 0; i < len(train); i++ {
				if matchId(i) {
					nPass++
				}
			}
			gt := filterGTByPred(train, queries, K, matchId)
			baseRec := recallStorePred(queries, gt, K, func(q []float32) []VSearchResult {
				r, _ := lvs.SearchFiltered(q, K, strPred, nil)
				return r
			})
			filtRec := recallStorePred(queries, gt, K, func(q []float32) []VSearchResult {
				r, _ := lvs.SearchFilter(q, K, filt)
				return r
			})
			baseQPS := runQPS(W, dur, queries, func(q []float32) { _, _ = lvs.SearchFiltered(q, K, strPred, nil) })
			filtQPS := runQPS(W, dur, queries, func(q []float32) { _, _ = lvs.SearchFilter(q, K, filt) })

			fmt.Printf("  %-9d %-7s %-7d %-11.0f %-11.0f %-9.1f %-9.3f %-9.3f\n",
				B, route, nPass, baseQPS, filtQPS, filtQPS/baseQPS, baseRec, filtRec)
		}
	}

	run("float32", false)
	run("SQ8", true)
	fmt.Println()
	fmt.Println("Вывод: filt_rec на cosine+SQ8 держится ≈base_rec и для brute (#7), и для graph (#2) —")
	fmt.Println("dot-ADC brute-блок rerank НЕ требует. Тенант-адресация при PartitionAttr — через")
	fmt.Println("SearchFilter (SearchTenant несовместим: per-segment dict-код, см. footgun-тест).")
}
