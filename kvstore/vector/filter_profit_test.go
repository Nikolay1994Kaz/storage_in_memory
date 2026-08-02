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
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestFilterProfit_SelectivityCrossover доказывает (или опровергает) ДВА тезиса
// дизайна фильтрации на РЕАЛЬНЫХ данных (SIFT-200k, dim128), без реализации фич:
//
//	#3  frozen post-result фильтрация: при селективном фильтре recall фильтрованного
//	    HNSW-поиска проседает (доходим до K по дистанции, ПОТОМ фильтруем → <<K попаданий).
//	#6  brute-force по отфильтрованному блоку при высокой селективности БЫСТРЕЕ
//	    фильтрованного HNSW И точен (recall=1.0). Ищем точку кроссовера.
//
// Запуск:
//
//	go test ./kvstore/vector/ -run TestFilterProfit_SelectivityCrossover -v -timeout 1200s
//
// Требует /tmp/sift200k.bin (scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift200k.bin --train 200000 --test 500).
func TestFilterProfit_SelectivityCrossover(t *testing.T) {
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("нет данных: %v (scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift200k.bin --train 200000 --test 500)", err)
	}
	const N = 200_000
	train = train[:N]
	if len(test) > 200 {
		test = test[:200] // 200 запросов хватает для стабильного recall, экономим время на GT
	}
	K := 10

	// ── Построить стор как в arch P2 (steady-state frozen-SQ сегменты) ──
	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(8),
		DeltaMax:  20_000,
		Fanout:    4,
		EfSearch:  128,
		UseSQ:     true,
	}
	lvs := NewLeveledVectorStore(cfg)
	t0 := time.Now()
	for i, v := range train {
		_ = lvs.Add(strconv.Itoa(i), v)
	}
	settleStart := time.Now()
	for time.Since(settleStart) < 180*time.Second {
		lvs.FlushDeltaSync()
		if !lvs.anyMergeInFlight() {
			time.Sleep(500 * time.Millisecond)
			if !lvs.anyMergeInFlight() {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	st := lvs.Stats()
	t.Logf("built %d за %v, segs=%v", N, time.Since(t0).Round(time.Second), st.SegmentsByLevel)

	// Селективность: доля векторов, проходящих фильтр (имитация одного tenant/категории).
	selectivities := []float64{1.0, 0.5, 0.2, 0.05, 0.01, 0.002}

	fmt.Println()
	fmt.Printf("FILTER PROFIT — SIFT-%d dim128, K=%d, queries=%d, efSearch=%d\n", N, K, len(test), cfg.EfSearch)
	fmt.Printf("%-8s %9s | %10s %10s | %10s %10s | %s\n",
		"select", "#pass", "HNSW_rec", "HNSW_QPS", "BRUTE_rec", "BRUTE_QPS", "winner")
	fmt.Println("-------------------------------------------------------------------------------------")

	for _, p := range selectivities {
		// Детерминированный разброс «проходящих» id (не зависит от порядка вставки).
		thr := uint64(p * 1_000_000)
		passSet := make([]bool, N)
		passIdx := make([]int, 0, int(p*float64(N))+1)
		for i := 0; i < N; i++ {
			if (uint64(i)*2654435761)%1_000_000 < thr {
				passSet[i] = true
				passIdx = append(passIdx, i)
			}
		}
		nPass := len(passIdx)
		if nPass == 0 {
			continue
		}

		// Фильтрованный ground-truth: точный top-K СРЕДИ проходящих фильтр.
		fgt := filteredBruteGT(train, test, K, passSet)

		// Предикат движка: строковый ключ → int → bitset (строковый «налог» как в реальном API).
		filterFn := func(key string) bool {
			i, _ := strconv.Atoi(key)
			return passSet[i]
		}

		// HNSW-фильтрованный: recall + QPS_1thr.
		hnswRec := recallFiltered(lvs, test, fgt, K, filterFn)
		hnswQPS := qpsFilteredStore(lvs, test, K, filterFn, 1, 3*time.Second)

		// Brute по блоку: точен (recall=1.0), измеряем QPS_1thr.
		bruteRec := recallBruteFiltered(train, test, fgt, K, passIdx)
		bruteQPS := qpsBruteFiltered(train, test, K, passIdx, 1, 3*time.Second)

		winner := "HNSW"
		if bruteQPS > hnswQPS {
			winner = "BRUTE"
		}
		fmt.Printf("%-8.3f %9d | %10.3f %10.0f | %10.3f %10.0f | %s\n",
			p, nPass, hnswRec, hnswQPS, bruteRec, bruteQPS, winner)
	}
	fmt.Println("-------------------------------------------------------------------------------------")
	fmt.Println("#3 подтверждён если HNSW_rec падает с селективностью. #6 подтверждён если BRUTE_QPS обгоняет HNSW_QPS при малом #pass.")
}

// TestFilterProfit_Correlated — ЧЕСТНЫЙ тест #3: фильтр КОРРЕЛИРОВАН с пространством
// (tenant = компактный кластер векторов, ближайших к случайному центру), как в реальной
// мультитенантности. Здесь filtered-HNSW может НЕ дойти до отфильтрованной области →
// проверяем, обваливается ли recall (в отличие от равномерного фильтра, где он держался).
//
//	go test ./kvstore/vector/ -run TestFilterProfit_Correlated -v -timeout 1200s
func TestFilterProfit_Correlated(t *testing.T) {
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("нет данных: %v", err)
	}
	const N = 200_000
	train = train[:N]
	if len(test) > 200 {
		test = test[:200]
	}
	K := 10

	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(8),
		DeltaMax:  20_000,
		Fanout:    4,
		EfSearch:  128,
		UseSQ:     true,
	}
	lvs := NewLeveledVectorStore(cfg)
	for i, v := range train {
		_ = lvs.Add(strconv.Itoa(i), v)
	}
	settleStart := time.Now()
	for time.Since(settleStart) < 180*time.Second {
		lvs.FlushDeltaSync()
		if !lvs.anyMergeInFlight() {
			time.Sleep(500 * time.Millisecond)
			if !lvs.anyMergeInFlight() {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Центр кластера — фиксированный вектор; ранжируем все вектора по близости к нему.
	center := train[12345]
	type dPair struct {
		id int
		d  float32
	}
	dist := make([]dPair, N)
	for i := range train {
		dist[i] = dPair{i, EuclideanDistance(center, train[i])}
	}
	sort.Slice(dist, func(i, j int) bool { return dist[i].d < dist[j].d })

	selectivities := []float64{0.5, 0.2, 0.05, 0.01, 0.002}

	fmt.Println()
	fmt.Printf("FILTER PROFIT (CORRELATED tenant=spatial cluster) — SIFT-%d dim128, K=%d, q=%d, ef=%d\n", N, K, len(test), cfg.EfSearch)
	fmt.Printf("%-8s %9s | %10s %10s | %10s %10s | %s\n",
		"select", "#pass", "HNSW_rec", "HNSW_QPS", "BRUTE_rec", "BRUTE_QPS", "winner")
	fmt.Println("-------------------------------------------------------------------------------------")
	for _, p := range selectivities {
		nPass := int(p * float64(N))
		passSet := make([]bool, N)
		passIdx := make([]int, nPass)
		for k := 0; k < nPass; k++ {
			passSet[dist[k].id] = true
			passIdx[k] = dist[k].id
		}
		fgt := filteredBruteGT(train, test, K, passSet)
		filterFn := func(key string) bool {
			i, _ := strconv.Atoi(key)
			return passSet[i]
		}
		hnswRec := recallFiltered(lvs, test, fgt, K, filterFn)
		hnswQPS := qpsFilteredStore(lvs, test, K, filterFn, 1, 3*time.Second)
		bruteQPS := qpsBruteFiltered(train, test, K, passIdx, 1, 3*time.Second)
		winner := "HNSW"
		if bruteQPS > hnswQPS {
			winner = "BRUTE"
		}
		fmt.Printf("%-8.3f %9d | %10.3f %10.0f | %10.3f %10.0f | %s\n",
			p, nPass, hnswRec, hnswQPS, 1.0, bruteQPS, winner)
	}
	fmt.Println("-------------------------------------------------------------------------------------")
	fmt.Println("#3 подтверждён ТУТ если HNSW_rec заметно < 1.0 при коррелированном фильтре (в отличие от равномерного).")
}

// TestFilterProfit_PredicateCost изолирует #2 (string→uint предикат) и #7 (непрерывный
// блок тенанта vs скан всего корпуса с предикатом). Без стора — чистый brute top-K по
// train, селективность 1%, все варианты ТОЧНЫ (recall=1.0), мерим только QPS_1thr.
//
//	(a) STRING:  скан всех N, предикат = map[string]tenant lookup по strconv.Itoa(id)  ← текущий API
//	(b) UINT:    скан всех N, предикат = passSet[id] []bool                              ← #2: коды вместо строк
//	(c) BLOCK:   итерируем ТОЛЬКО непрерывный блок тенанта (раскладка)                   ← #7: не трогаем 99% чужих
//
//	go test ./kvstore/vector/ -run TestFilterProfit_PredicateCost -v -timeout 300s
func TestFilterProfit_PredicateCost(t *testing.T) {
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("нет данных: %v", err)
	}
	const N = 200_000
	train = train[:N]
	if len(test) > 200 {
		test = test[:200]
	}
	K := 10
	const p = 0.01 // 1% селективность (типичный мелкий tenant)
	nPass := int(p * float64(N))

	// Коррелированный блок: ближайшие к центру (как в реальном tenant).
	center := train[12345]
	type dPair struct {
		id int
		d  float32
	}
	dist := make([]dPair, N)
	for i := range train {
		dist[i] = dPair{i, EuclideanDistance(center, train[i])}
	}
	sort.Slice(dist, func(i, j int) bool { return dist[i].d < dist[j].d })

	passSet := make([]bool, N)        // (b) uint-предикат
	tenantMap := make(map[string]int) // (a) строковый атрибут: key → tenantID
	blockVecs := make([][]float32, nPass)
	for k := 0; k < nPass; k++ {
		id := dist[k].id
		passSet[id] = true
		tenantMap[strconv.Itoa(id)] = 7 // целевой tenant
		blockVecs[k] = train[id]
	}

	const target = 7
	// (a) string-предикат: map-lookup по строковому ключу на КАЖДЫЙ вектор корпуса.
	qpsA := bruteQPSPred(train, test, K, N, 3*time.Second, func(id int) bool {
		return tenantMap[strconv.Itoa(id)] == target
	})
	// (b) uint-предикат: bitset на каждый вектор корпуса.
	qpsB := bruteQPSPred(train, test, K, N, 3*time.Second, func(id int) bool {
		return passSet[id]
	})
	// (c) block-layout: итерируем только nPass векторов тенанта, без предиката.
	qpsC := bruteQPSBlock(blockVecs, test, K, 3*time.Second)

	fmt.Println()
	fmt.Printf("PREDICATE COST (#2,#7) — SIFT-%d dim128, K=%d, q=%d, selectivity=%.1f%% (#pass=%d)\n",
		N, K, len(test), p*100, nPass)
	fmt.Printf("  (a) STRING predicate, scan-all:   %8.0f QPS\n", qpsA)
	fmt.Printf("  (b) UINT  predicate, scan-all:    %8.0f QPS   (#2: %.2f× vs string)\n", qpsB, qpsB/qpsA)
	fmt.Printf("  (c) BLOCK layout, scan-tenant:    %8.0f QPS   (#7: %.2f× vs uint-scan-all)\n", qpsC, qpsC/qpsB)
	fmt.Printf("  итог (c vs a): %.1f× от строкового скана всего корпуса к блочной раскладке\n", qpsC/qpsA)
}

// bruteQPSPred — top-K по корпусу [0,n) с предикатом по id, измеряет QPS_1thr.
func bruteQPSPred(vecs, queries [][]float32, K, n int, dur time.Duration, pred func(int) bool) float64 {
	deadline := time.Now().Add(dur)
	var ops, qi int
	for time.Now().Before(deadline) {
		q := queries[qi%len(queries)]
		qi++
		topKByPred(vecs, q, K, n, pred)
		ops++
	}
	return float64(ops) / dur.Seconds()
}

func topKByPred(vecs [][]float32, q []float32, K, n int, pred func(int) bool) {
	top := make([]float32, 0, K+1)
	for id := 0; id < n; id++ {
		if !pred(id) {
			continue
		}
		d := EuclideanDistance(q, vecs[id])
		if len(top) < K {
			top = append(top, d)
			if len(top) == K {
				sort.Slice(top, func(i, j int) bool { return top[i] < top[j] })
			}
			continue
		}
		if d < top[K-1] {
			pos := K - 1
			for pos > 0 && top[pos-1] > d {
				top[pos] = top[pos-1]
				pos--
			}
			top[pos] = d
		}
	}
	_ = top
}

// bruteQPSBlock — top-K только по блоку blockVecs (раскладка тенанта), без предиката.
func bruteQPSBlock(blockVecs, queries [][]float32, K int, dur time.Duration) float64 {
	deadline := time.Now().Add(dur)
	var ops, qi int
	for time.Now().Before(deadline) {
		q := queries[qi%len(queries)]
		qi++
		topKByPred(blockVecs, q, K, len(blockVecs), func(int) bool { return true })
		ops++
	}
	return float64(ops) / dur.Seconds()
}

// TestFilterProfit_Crossover ищет ИСТИННЫЙ кроссовер brute↔HNSW на коррелированном
// фильтре, сняв с HNSW строковый налог #2 настолько, насколько позволяет API движка.
// Движок отдаёт предикату string-ключ → эмулируем дешёвый предикат через map[string]struct{}
// (без strconv.Atoi). Это КОНСЕРВАТИВНО: настоящий tenantCodes[idx] uint32 был бы ещё
// дешевле (индексация слайса < хеш строки), т.е. реальный кроссовер ещё ниже.
//
//	go test ./kvstore/vector/ -run TestFilterProfit_Crossover -v -timeout 1200s
func TestFilterProfit_Crossover(t *testing.T) {
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("нет данных: %v", err)
	}
	const N = 200_000
	train = train[:N]
	if len(test) > 200 {
		test = test[:200]
	}
	K := 10

	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(8),
		DeltaMax:  20_000,
		Fanout:    4,
		EfSearch:  128,
		UseSQ:     true,
	}
	lvs := NewLeveledVectorStore(cfg)
	for i, v := range train {
		_ = lvs.Add(strconv.Itoa(i), v)
	}
	settleStart := time.Now()
	for time.Since(settleStart) < 180*time.Second {
		lvs.FlushDeltaSync()
		if !lvs.anyMergeInFlight() {
			time.Sleep(500 * time.Millisecond)
			if !lvs.anyMergeInFlight() {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Коррелированный кластер (реальный tenant).
	center := train[12345]
	type dPair struct {
		id int
		d  float32
	}
	dist := make([]dPair, N)
	for i := range train {
		dist[i] = dPair{i, EuclideanDistance(center, train[i])}
	}
	sort.Slice(dist, func(i, j int) bool { return dist[i].d < dist[j].d })

	// Мелкий шаг вокруг ожидаемого кроссовера.
	selectivities := []float64{0.5, 0.3, 0.2, 0.1, 0.05, 0.02, 0.01}

	fmt.Println()
	fmt.Printf("CROSSOVER (correlated tenant) — SIFT-%d dim128, K=%d, q=%d, ef=%d\n", N, K, len(test), cfg.EfSearch)
	fmt.Printf("HNSW_str = filterFn с Atoi (как сейчас);  HNSW_map = дешёвый предикат map[string] (эмуляция uint #2)\n")
	fmt.Printf("%-8s %8s | %9s %9s %9s | %9s | %s\n",
		"select", "#pass", "HNSW_str", "HNSW_map", "HNSWrec", "BRUTE", "winner(map vs brute)")
	fmt.Println("-----------------------------------------------------------------------------------------")
	for _, p := range selectivities {
		nPass := int(p * float64(N))
		passSet := make([]bool, N)
		passIdx := make([]int, nPass)
		allowed := make(map[string]struct{}, nPass)
		for k := 0; k < nPass; k++ {
			id := dist[k].id
			passSet[id] = true
			passIdx[k] = id
			allowed[strconv.Itoa(id)] = struct{}{}
		}
		fgt := filteredBruteGT(train, test, K, passSet)

		strFilter := func(key string) bool { i, _ := strconv.Atoi(key); return passSet[i] }
		mapFilter := func(key string) bool { _, ok := allowed[key]; return ok }

		qStr := qpsFilteredStore(lvs, test, K, strFilter, 1, 3*time.Second)
		qMap := qpsFilteredStore(lvs, test, K, mapFilter, 1, 3*time.Second)
		rec := recallFiltered(lvs, test, fgt, K, mapFilter)
		qBrute := qpsBruteFiltered(train, test, K, passIdx, 1, 3*time.Second)

		winner := "HNSW_map"
		if qBrute > qMap {
			winner = "BRUTE"
		}
		fmt.Printf("%-8.3f %8d | %9.0f %9.0f %9.3f | %9.0f | %s\n",
			p, nPass, qStr, qMap, rec, qBrute, winner)
	}
	fmt.Println("-----------------------------------------------------------------------------------------")
	fmt.Println("Истинный кроссовер = где BRUTE перестаёт обгонять HNSW_map. Реальный uint-предикат сдвинет его ещё ниже.")
}

// filteredBruteGT — точный top-K среди векторов, прошедших фильтр (параллельно по запросам).
func filteredBruteGT(vecs, queries [][]float32, K int, pass []bool) [][]int {
	gt := make([][]int, len(queries))
	var next int64 = -1
	var wg sync.WaitGroup
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				qi := int(atomic.AddInt64(&next, 1))
				if qi >= len(queries) {
					return
				}
				gt[qi] = topKFilteredIdx(vecs, queries[qi], K, pass)
			}
		}()
	}
	wg.Wait()
	return gt
}

// recallFiltered — recall@K фильтрованного HNSW-поиска движка против фильтрованного GT.
func recallFiltered(lvs *LeveledVectorStore, queries [][]float32, gt [][]int, K int, filterFn func(string) bool) float64 {
	var sum float64
	for qi, q := range queries {
		res, _ := lvs.SearchFiltered(q, K, filterFn, nil)
		ids := make([]int, len(res))
		for i, r := range res {
			ids[i], _ = strconv.Atoi(r.Key)
		}
		sum += recallVsGT(ids, gt[qi], K)
	}
	return sum / float64(len(queries))
}

// recallBruteFiltered — sanity-check, что brute точен (должно быть 1.0).
func recallBruteFiltered(vecs, queries [][]float32, gt [][]int, K int, passIdx []int) float64 {
	pass := make([]bool, len(vecs))
	for _, i := range passIdx {
		pass[i] = true
	}
	var sum float64
	for qi, q := range queries {
		ids := topKFilteredIdx(vecs, q, K, pass)
		sum += recallVsGT(ids, gt[qi], K)
	}
	return sum / float64(len(queries))
}

// qpsFilteredStore — QPS фильтрованного HNSW-поиска.
func qpsFilteredStore(lvs *LeveledVectorStore, queries [][]float32, K int, filterFn func(string) bool, W int, dur time.Duration) float64 {
	var idx int64 = -1
	var ops int64
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < W; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for time.Now().Before(deadline) {
				q := queries[int(atomic.AddInt64(&idx, 1))%len(queries)]
				_, _ = lvs.SearchFiltered(q, K, filterFn, nil)
				local++
			}
			atomic.AddInt64(&ops, int64(local))
		}()
	}
	wg.Wait()
	return float64(ops) / dur.Seconds()
}

// qpsBruteFiltered — QPS точного brute по отфильтрованному блоку (только проходящие id).
func qpsBruteFiltered(vecs, queries [][]float32, K int, passIdx []int, W int, dur time.Duration) float64 {
	pass := make([]bool, len(vecs))
	for _, i := range passIdx {
		pass[i] = true
	}
	var idx int64 = -1
	var ops int64
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < W; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for time.Now().Before(deadline) {
				q := queries[int(atomic.AddInt64(&idx, 1))%len(queries)]
				_ = topKFilteredIdx(vecs, q, K, pass)
				local++
			}
			atomic.AddInt64(&ops, int64(local))
		}()
	}
	wg.Wait()
	return float64(ops) / dur.Seconds()
}
