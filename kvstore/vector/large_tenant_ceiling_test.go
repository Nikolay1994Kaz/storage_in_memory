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
// ЗАМЕР ПОТОЛКА: крупный + СЕЛЕКТИВНЫЙ тенант.
//
// Контекст ([[vector-large-tenant-filter-plan-20260630]]): когда тенант КРУПНЫЙ
// (graph-маршрут, B>порога) и предикат СЕЛЕКТИВНЫЙ, но matched всё же больше
// residual-бюджета (ef*32), боевой SearchFilter падает на пол ~230-274 QPS:
// filtered-HNSW переисследует граф, residual-brute (#2) не срабатывает. Этот пол —
// число дистанций, а не маршрут. Данные alem.ai крупные → угол важен.
//
// Вопрос: стоит ли filterable-HNSW (~1-2 нед, теор. 5-10×)? Чтобы ответить ДО
// разработки — меряем ИДЕАЛИЗИРОВАННЫЙ потолок: на ОДНОМ matched-множестве (предикат
// фиксирован по всем запросам → строим суб-индекс ОДИН раз) сравниваем при равном
// recall три точки:
//
//   floor  — SearchFilter по боевому консолидированному SQ8-стору (что есть сегодня).
//   brute  — линейный скан ровно по matched-векторам (дешёвая опция: поднять residual-
//            бюджет; теория [[vector-dimaware-residual-brute-20260630]]: линеен по matched).
//   graph  — бросовый HNSW над ровно matched-множеством (float32/euclidean, ОПТИМИСТИЧНО —
//            это лучший случай per-filter суб-индекса = идеализированный filterable-HNSW).
//
// Решающее сравнение — graph/brute на канонических ~5000 matched: если граф над
// matched бьёт brute над matched лишь в ~1.5-2×, а brute включается тривиально
// (residual-бюджет), то filterable-HNSW НЕ окупает 1-2 недели. Если ≥5× — окупает.
//
//   go test ./kvstore/vector -run TestLargeTenant_FilterCeiling -v -timeout 3600s
//
// Требует /tmp/dbpedia100k.bin (та же геометрия, что тюнили 3fbfd78: 100k×1536 cosine).
// matched-свип по price-диапазону на крупнейшем тенанте t4 (B=40000, graph-маршрут).
// =============================================================================

func TestLargeTenant_FilterCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("long: строит 100k×1536 SQ8 HNSW + per-matched бросовые графы")
	}
	train, test, _, err := loadDBpediaRaw("/tmp/dbpedia100k.bin")
	if err != nil {
		t.Skipf("нет данных: %v (python3 convert_dbpedia.py)", err)
	}
	dim := len(train[0])

	const (
		K   = 10
		ef  = 128
		W   = 12
		dur = 2 * time.Second
	)

	// Тенант-блоки как в боевом dbpedia_filter_test: t4=40000 — крупнейший, graph-маршрут.
	blocks := []int{1000, 16000, 30000, 40000}
	totalTargets := 0
	for _, b := range blocks {
		totalTargets += b
	}
	if totalTargets >= len(train) {
		t.Fatalf("сумма блоков %d ≥ корпуса %d", totalTargets, len(train))
	}

	tenantOfId := func(i int) string { tn, _, _ := attrLayout(i, blocks); return tn }
	regionOfId := func(i int) string { _, rg, _ := attrLayout(i, blocks); return rg }
	priceOfId := func(i int) float64 { _, _, pr := attrLayout(i, blocks); return pr }

	// ── Боевой консолидированный SQ8-стор (для пола SearchFilter) ──
	cfg := LeveledConfig{
		Metric:         MetricCosine,
		Allocator:      tcmalloc.NewTCMallocStore(8),
		DeltaMax:       len(train) + 1,
		Fanout:         4,
		EfConstruction: arEfConstruction,
		EfSearch:       ef,
		PartitionAttr:  "tenant",
		BruteThreshold: DefaultBruteThreshold,
		UseSQ:          true,
	}
	lvs := NewLeveledVectorStore(cfg)

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
	rng := rand.New(rand.NewSource(20260630))
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })

	tBuild := time.Now()
	for _, e := range all {
		if err := lvs.AddWithAttrs(e.key, e.vec, e.attrs); err != nil {
			t.Fatalf("AddWithAttrs: %v", err)
		}
	}
	settle(lvs)
	buildDur := time.Since(tBuild)

	st := lvs.Stats()
	nseg := 0
	for _, n := range st.SegmentsByLevel {
		nseg += n
	}

	nQ := min(200, len(test))
	queries := test[:nQ]

	fmt.Println()
	fmt.Printf("LARGE-TENANT CEILING — ada-002 %d-dim cosine, N=%d, segs=%d, build=%.1fs, q=%d, ef=%d, K=%d, W=%d\n",
		dim, len(train), nseg, buildDur.Seconds(), nQ, ef, K, W)
	fmt.Printf("tenant t4 (B=40000, graph-маршрут, порог brute=%d), свип matched по price-диапазону\n", DefaultBruteThreshold)
	if nseg != 1 {
		fmt.Printf("⚠ segs=%d≠1 — пол занижен фрагментацией\n", nseg)
	}
	fmt.Printf("residual-бюджет (ef*%d) = %d matched; пол держится пока matched > бюджета\n", residualBruteFactor, ef*residualBruteFactor)

	// Сценарии: крупнейший тенант t4, селективность через price-диапазон.
	type scen struct {
		name     string
		priceHi  float64 // -1 => без price-фильтра
		useReg   bool    // region=r0
	}
	scens := []scen{
		{"r0∧price[0,124]", 124, true},
		{"r0∧price[0,249]", 249, true},
		{"r0∧price[0,499]", 499, true}, // КАНОН ~5000
		{"r0 (all price)", -1, true},   // ~10000
		{"вся t4", -1, false},          // 40000 — весь крупный тенант
	}

	fmt.Println()
	fmt.Printf("%-18s %-8s | %-9s %-8s | %-9s %-8s | %-9s %-8s %-9s | %-9s %-9s\n",
		"predicate(t4∧)", "matched",
		"floor_QPS", "f_rec", "brute_QPS", "b_rec", "graph_QPS", "g_rec", "g_build",
		"graph/bru", "graph/flr")
	fmt.Println("─────────────────────────────────────────────────────────────────────────────────────────────────────────────────")

	const tn = "t4"
	for _, sc := range scens {
		// Предикат (по глобальному id) и матчинг-множество.
		matchId := func(i int) bool {
			if tenantOfId(i) != tn {
				return false
			}
			if sc.useReg && regionOfId(i) != "r0" {
				return false
			}
			if sc.priceHi >= 0 && (priceOfId(i) < 0 || priceOfId(i) > sc.priceHi) {
				return false
			}
			return true
		}

		// Собрать matched-векторы в компактный слайс (float32).
		var matchedVecs [][]float32
		var matchedIds []int
		for i := range train {
			if matchId(i) {
				matchedVecs = append(matchedVecs, train[i])
				matchedIds = append(matchedIds, i)
			}
		}
		matched := len(matchedVecs)

		// Точный GT среди matched (euclidean == cosine на нормализованных).
		gt := filterGTByPred(train, queries, K, matchId)

		// ── floor: боевой SearchFilter (SQ8 graph-маршрут) ──
		filt := Filter{Eq: map[string]string{"tenant": tn}}
		if sc.useReg {
			filt.Eq["region"] = "r0"
		}
		if sc.priceHi >= 0 {
			filt.Range = map[string][2]float64{"price": {0, sc.priceHi}}
		}
		floorRec := recallStorePred(queries, gt, K, func(q []float32) []VSearchResult {
			r, _ := lvs.SearchFilter(q, K, filt)
			return r
		})
		floorQPS := runQPS(W, dur, queries, func(q []float32) { _, _ = lvs.SearchFilter(q, K, filt) })

		// ── brute: линейный скан ровно по matched ──
		bruteRec := recallPred(queries, gt, K, func(q []float32) []int {
			return bruteTopKCompact(matchedVecs, matchedIds, q, K)
		})
		bruteQPS := runQPS(W, dur, queries, func(q []float32) { _ = bruteTopKCompact(matchedVecs, matchedIds, q, K) })

		// ── graph: бросовый HNSW над ровно matched (оптимистичный потолок) ──
		alloc := tcmalloc.NewTCMallocStore(2)
		g := NewGraph(EuclideanDistance, alloc)
		g.EfConstruction = arEfConstruction
		node2id := make(map[uint64]int, matched)
		tg := time.Now()
		for j, v := range matchedVecs {
			nid := g.Insert(v)
			node2id[uint64(nid)] = matchedIds[j]
		}
		gBuild := time.Since(tg)
		graphRec := recallPred(queries, gt, K, func(q []float32) []int {
			res := g.Search(q, K, ef)
			ids := make([]int, len(res))
			for i, r := range res {
				ids[i] = node2id[r.ID]
			}
			return ids
		})
		graphQPS := runQPS(W, dur, queries, func(q []float32) { _ = g.Search(q, K, ef) })

		fmt.Printf("%-18s %-8d | %-9.0f %-8.3f | %-9.0f %-8.3f | %-9.0f %-8.3f %-9s | %-9.2f %-9.2f\n",
			sc.name, matched,
			floorQPS, floorRec, bruteQPS, bruteRec, graphQPS, graphRec,
			fmt.Sprintf("%.0fms", gBuild.Seconds()*1000),
			graphQPS/bruteQPS, graphQPS/floorQPS)
	}

	fmt.Println()
	fmt.Println("ЧТЕНИЕ: floor=сегодня (SQ8 graph-маршрут). brute=поднять residual-бюджет (тривиально).")
	fmt.Println("graph=идеализированный filterable-HNSW (float32, лучший случай). graph/bru — главный")
	fmt.Println("критерий: если на каноне ~5000 он ~1.5-2× → filterable-HNSW НЕ окупает 1-2 недели")
	fmt.Println("(хватит residual-бюджета); если ≥5× → окупает. g_build — цена построить суб-индекс на лету.")
}

// recallPred — recall@K результатов (срез глобальных индексов) против GT-индексов.
func recallPred(queries [][]float32, gt [][]int, K int, search func([]float32) []int) float64 {
	var sum float64
	for qi, q := range queries {
		sum += recallVsGT(search(q), gt[qi], K)
	}
	return sum / float64(len(queries))
}

// bruteTopKCompact — точный top-K (euclidean) по компактному matched-слайсу.
// Возвращает глобальные id. Сортированная вставка, K мал (~10).
func bruteTopKCompact(vecs [][]float32, ids []int, q []float32, K int) []int {
	type de struct {
		d  float32
		id int
	}
	best := make([]de, 0, K+1)
	for j, v := range vecs {
		d := EuclideanDistance(q, v)
		if len(best) < K {
			best = append(best, de{d, ids[j]})
			for k := len(best) - 1; k > 0 && best[k].d < best[k-1].d; k-- {
				best[k], best[k-1] = best[k-1], best[k]
			}
		} else if d < best[K-1].d {
			best[K-1] = de{d, ids[j]}
			for k := K - 1; k > 0 && best[k].d < best[k-1].d; k-- {
				best[k], best[k-1] = best[k-1], best[k]
			}
		}
	}
	out := make([]int, len(best))
	for i := range best {
		out[i] = best[i].id
	}
	return out
}
