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
// Фильтр НА МАСШТАБЕ — боевой колоночный путь SearchFilter на SIFT-1M.
//
// Прошлые filter_profit-бенчи мерили ЭМУЛЯЦИЮ предикатов (bitset/map) на 200k и
// отвечали на дизайн-вопросы #2/#3/#6/#7. Здесь мы прогоняем РЕАЛЬНЫЙ
// закоммиченный API на масштабе 1М:
//
//   AddWithAttrs(key, vec, Attributes{Cat:{tenant,region}, Num:{price}})
//   SearchFilter(q, K, Filter{Eq:{tenant:X}})                     ← single-attr
//   SearchFilter(q, K, Filter{Eq:{tenant:X,region:r0}, Range:{price}})  ← multi-attr
//
// cfg.PartitionAttr="tenant" → колоночный код tenant ведёт сортировку/каталог
// (Вещь 1) → blocked-brute (малый тенант) / graph (крупный). region+price —
// остаточный idx-предикат по uint/float-колонкам (рычаг #2, multi-attr).
//
//   baseline = SearchFiltered(q, K, strPred): полный HNSW-обход +×4 ef + строковый
//              предикат, воспроизводящий тот же фильтр. Ровно то, что делает движок
//              БЕЗ колоночного слоя (string→atoi на каждый посещённый узел).
//   prod     = SearchFilter(...): колоночный роутинг.
//
// Вопрос: окупается ли колоночный SearchFilter на масштабе 1М (а не на 200k/dim128,
// где рычаг #2 вышел скромным 1.23×)? И сколько стоит multi-attr остаток?
//
//   go test ./kvstore/vector -run TestFilter_AttrScaleQPSGain -v -timeout 3600s
//
// Требует /tmp/sift1m.bin (scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift1m.bin --test 1000).
// =============================================================================

// attrTenantOf раскладывает глобальный id по тенант-блокам разного размера.
// Возвращает строковый код тенанта (категориальный атрибут "tenant").
func attrLayout(i int, blocks []int) (tenant string, region string, price float64) {
	off := 0
	tid := 0 // 0 — фоновый тенант
	for b, sz := range blocks {
		if i >= off && i < off+sz {
			tid = b + 1
			break
		}
		off += sz
	}
	tenant = "t" + strconv.Itoa(tid)
	region = "r" + strconv.Itoa(i%4) // 4 региона равномерно
	price = float64(i % 1000)        // [0,999]
	return
}

func TestFilter_AttrScaleQPSGain(t *testing.T) {
	if testing.Short() {
		t.Skip("long: строит 1M HNSW + многопоточный QPS sweep")
	}
	train, test, err := loadSIFTRaw("/tmp/sift1m.bin")
	if err != nil {
		t.Skipf("нет данных: %v (scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift1m.bin --test 1000)", err)
	}

	const (
		dim = 128
		K   = 10
		ef  = 128
		W   = 12              // эталонная многопоточная нагрузка
		dur = 3 * time.Second // на режим/тенант
	)

	// Тенант-блоки: два ниже порога 16384 (brute), два выше (graph) — оба регима.
	blocks := []int{1000, 16000, 50000, 200000}
	totalTargets := 0
	for _, b := range blocks {
		totalTargets += b
	}
	if totalTargets >= len(train) {
		t.Fatalf("сумма блоков %d ≥ корпуса %d", totalTargets, len(train))
	}

	// ── Построение: один консолидированный сегмент (DeltaMax > N → один flush) ──
	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(8),
		DeltaMax:       len(train) + 1,
		Fanout:         4,
		EfConstruction: arEfConstruction,
		EfSearch:       ef,
		PartitionAttr:  "tenant", // колоночный тенант ведёт раскладку/каталог
		BruteThreshold: DefaultBruteThreshold,
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
	// Перемешать порядок вставки (как в проде приходят вперемешку) — сортировка по
	// тенанту при компакции обязана восстановить контигуозность.
	rng := rand.New(rand.NewSource(20260629))
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
	fmt.Printf("FILTER@SCALE — РЕАЛЬНЫЙ SIFT-128, N=%d, segs=%d, ef=%d, K=%d, W=%d, build=%.1fs\n",
		st.TotalVectors, nseg, ef, K, W, buildDur.Seconds())
	fmt.Printf("PartitionAttr=tenant, BruteThreshold=%d  (baseline=SearchFiltered str-pred, prod=SearchFilter колонки)\n",
		cfg.BruteThreshold)
	if nseg != 1 {
		fmt.Printf("ВНИМАНИЕ: сегментов %d, не 1 — выигрыш brute занижен фрагментацией (segs=%v)\n",
			nseg, st.SegmentsByLevel)
	}

	// Индекс глобального id для строковых предикатов baseline.
	tenantOfId := func(i int) string { tn, _, _ := attrLayout(i, blocks); return tn }
	regionOfId := func(i int) string { _, rg, _ := attrLayout(i, blocks); return rg }
	priceOfId := func(i int) float64 { _, _, pr := attrLayout(i, blocks); return pr }

	// ── РЕЖИМ A: single-attr (только tenant) ──
	fmt.Println()
	fmt.Println("=== A) SINGLE-ATTR: Filter{Eq:{tenant:X}} ===")
	fmt.Printf("%-9s %-7s %-12s %-12s %-9s %-10s %-10s\n",
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

		fmt.Printf("%-9d %-7s %-12.0f %-12.0f %-9.2f %-10.3f %-10.3f\n",
			B, route, baseQPS, filtQPS, filtQPS/baseQPS, baseRec, filtRec)
	}

	// ── РЕЖИМ B: multi-attr (tenant ∧ region=r0 ∧ price∈[0,499]) ──
	fmt.Println()
	fmt.Println("=== B) MULTI-ATTR: Filter{Eq:{tenant:X,region:r0}, Range:{price:[0,499]}} ===")
	fmt.Printf("%-9s %-7s %-9s %-12s %-12s %-9s %-10s %-10s\n",
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

		fmt.Printf("%-9d %-7s %-9d %-12.0f %-12.0f %-9.2f %-10.3f %-10.3f\n",
			B, route, nPass, baseQPS, filtQPS, filtQPS/baseQPS, baseRec, filtRec)
	}
	fmt.Println("---")
	fmt.Println("Выигрыш = filt_QPS/base_QPS при filt_rec ≥ base_rec. brute-тенанты (B≤16384) —")
	fmt.Println("кратный отрыв (#7); graph-тенанты — паритет/умеренно (#2 на остаточном предикате).")
}
