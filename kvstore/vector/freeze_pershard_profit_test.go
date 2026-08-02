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
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFreezePerShard_Profit* — бенчи ДО кода для плана freeze-per-shard
// (см. vector-freeze-per-shard-plan-20260710): вместо rebuild'а сегмента из slab
// (buildSegment: полная перестройка HNSW + RepairReachability, десятки секунд)
// каждый из S шардов дельты замораживается СВОИМ FreezeGraphSQ (копирование+
// квантование уже готового графа, без единой вставки). Гипотезы:
//
//  1. ЦЕНА: freeze S шардов на порядки дешевле rebuild → flushing-состояние
//     схлопывается за миллисекунды, бэклог физически не накапливается
//     (и кап insert из 17663b6 перестаёт срабатывать → −23% insert возвращается).
//  2. ПОИСК: fan-out по S мини-frozenSQ8 не медленнее fan-out по S живым
//     fp32-графам (корень brownout: 70% CPU в deltaShard.search) при
//     сравнимом recall; на высоких dim (memory-BW-bound) — быстрее.
//  3. ПАМЯТЬ: мини-сегменты ~4× компактнее fp32-дельты.
//
// Терминальное состояние (1 консолидированный сегмент) остаётся эталоном:
// мини-сегменты — переходная форма до обычного merge-каскада.

// TestFreezePerShard_Profit — SIFT-128 (euclidean), N = deltaMax(128) = 50000.
func TestFreezePerShard_Profit(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data freeze-per-shard benchmark")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/sift200k.bin: %v", err)
	}
	runFreezePerShardProfit(t, "SIFT-128", train, test, EuclideanDistance, MetricEuclidean)
}

// TestFreezePerShard_ProfitDBpedia — dbpedia-openai-1536 (cosine, нормализованные):
// датасет, на котором brownout наблюдался в e2e. dim=1536 memory-BW-bound —
// здесь SQ8 (4× трафик) должен выигрывать и по скорости fan-out-поиска.
func TestFreezePerShard_ProfitDBpedia(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data freeze-per-shard benchmark (dbpedia-1536)")
	}
	train, test, _, err := loadDBpediaRaw("/tmp/dbpedia100k.bin")
	if err != nil {
		t.Skipf("нет данных: %v (scripts/convert_dbpedia.py)", err)
	}
	if len(test) > 300 {
		test = test[:300] // 1536d брутфорс-GT дорог; 300 запросов достаточно
	}
	runFreezePerShardProfit(t, "dbpedia-1536", train, test, CosineDistance, MetricCosine)
}

func runFreezePerShardProfit(t *testing.T, name string, train, test [][]float32, distFn DistanceFunc, metric Metric) {
	const (
		M        = 32
		efC      = 400
		efSearch = 100
		K        = 10
		S        = 12 // делта-шардов (прод-конфиг S12 из e2e)
		qDur     = 2 * time.Second
	)
	dim := len(train[0])
	N := deltaMax(dim, 0) // реалистичный размер флашащейся дельты
	if N > len(train) {
		N = len(train)
	}
	sub := train[:N]
	// GT евклидом валиден и для cosine: dbpedia нормализован (L2² монотонен 1-dot).
	gt := bruteGT(sub, test, K)
	t.Logf("%s: N=%d dim=%d queries=%d shards=%d", name, N, dim, len(test), S)

	// ── Наполняем шардированную дельту (как в проде при -delta-shards=12) ──
	d := NewDeltaSegmentSharded(dim, N+1, distFn, M, efC, S)
	var idx int64 = -1
	var wg sync.WaitGroup
	tFill0 := time.Now()
	for w := 0; w < S; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&idx, 1))
				if i >= N {
					return
				}
				d.Append(strconv.Itoa(i), sub[i])
			}
		}()
	}
	wg.Wait()
	fillDur := time.Since(tFill0)
	insRate := float64(N) / fillDur.Seconds()
	t.Logf("дельта наполнена: %v (%.0f insert/s, %d writers)", fillDur.Round(time.Millisecond), insRate, S)

	// ── ПУТЬ Б (план): freeze каждого шарда без rebuild ──
	// Меряем ПЕРВЫМ (до buildSegment): freeze читает живые графы дельты as-is.
	freezeShard := func(s *deltaShard) *FrozenGraphSQ {
		keys := make(map[uint64]string, len(s.nodeKey))
		for gid, k := range s.nodeKey {
			keys[uint64(gid)] = k
		}
		return FreezeGraphSQ(s.idx, metric, keys)
	}

	// Последовательно (нижняя граница — 1 build-worker).
	tF0 := time.Now()
	minis := make([]*FrozenGraphSQ, 0, S)
	for _, s := range d.shards {
		if fg := freezeShard(s); fg != nil {
			minis = append(minis, fg)
		}
	}
	freezeSeq := time.Since(tF0)

	// Параллельно (S горутин — так и будет в проде: шарды независимы).
	tF0 = time.Now()
	minisPar := make([]*FrozenGraphSQ, S)
	var fwg sync.WaitGroup
	for i, s := range d.shards {
		fwg.Add(1)
		go func(i int, s *deltaShard) {
			defer fwg.Done()
			minisPar[i] = freezeShard(s)
		}(i, s)
	}
	fwg.Wait()
	freezePar := time.Since(tF0)
	_ = minisPar

	// ── ПУТЬ А (сегодня): rebuild через buildSegment (UseSQ, прод-путь S12) ──
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       N + 1,
		M:              M,
		EfConstruction: efC,
		EfSearch:       efSearch,
		Distance:       distFn,
		Metric:         metric,
		UseSQ:          true,
	})
	defer lvs.Close()
	entries := d.ExtractAll()
	tR0 := time.Now()
	seg := lvs.buildSegment(entries, dim, -1)
	rebuild := time.Since(tR0)
	if seg == nil {
		t.Fatal("buildSegment вернул nil")
	}

	fmt.Println()
	fmt.Printf("FREEZE-PER-SHARD [%s] — цена схлопывания flushing (N=%d, dim=%d, M=%d, efC=%d, S=%d):\n", name, N, dim, M, efC, S)
	fmt.Printf("  rebuild (buildSegment, путь сегодня):  %12v\n", rebuild.Round(time.Millisecond))
	fmt.Printf("  freeze 12 шардов последовательно:      %12v   (%.0f× быстрее)\n", freezeSeq.Round(time.Millisecond), rebuild.Seconds()/freezeSeq.Seconds())
	fmt.Printf("  freeze 12 шардов параллельно:          %12v   (%.0f× быстрее)\n", freezePar.Round(time.Millisecond), rebuild.Seconds()/freezePar.Seconds())
	fmt.Printf("  наполнение дельты (%d writers):        %12v\n", S, fillDur.Round(time.Millisecond))
	fmt.Printf("  → бэклог: дельта наполняется в %.0f× дольше, чем замораживается (parallel) — flushing не накапливается\n",
		fillDur.Seconds()/freezePar.Seconds())

	// ── Память переходного состояния ──
	fp32Bytes := N * dim * 4 // только slab; живые графы дельты сверху (арены+мапы)
	miniBytes := 0
	for _, fg := range minis {
		miniBytes += fg.MemoryBytes()
	}
	fmt.Printf("  память: fp32-slab дельты %d MB (+графы) → мини-frozenSQ8 %d MB\n",
		fp32Bytes>>20, miniBytes>>20)

	// ── Поиск: QPS + recall в трёх состояниях ──
	// (a) переходное СЕГОДНЯ: fan-out по S живым fp32-графам (deltaShard.search)
	// (b) переходное ПЛАН:    fan-out по S мини-frozenSQ8 + merge top-K
	// (c) терминал:           1 консолидированный SQ8-сегмент (эталон)
	searchDelta := func(q []float32) []int {
		res := d.Search(q, K, efSearch, nil)
		out := make([]int, 0, len(res))
		for _, r := range res {
			id, _ := strconv.Atoi(r.key)
			out = append(out, id)
		}
		return out
	}
	searchMinis := func(q []float32) []int {
		parts := make([][]FrozenResult, 0, len(minis))
		for _, fg := range minis {
			parts = append(parts, fg.Search(q, K, efSearch, nil, nil))
		}
		merged := mergeTopK(parts, K)
		out := make([]int, 0, len(merged))
		for _, r := range merged {
			id, _ := strconv.Atoi(r.Key)
			out = append(out, id)
		}
		return out
	}
	searchSeg := func(q []float32) []int {
		res := seg.Search(q, K, efSearch, nil, nil)
		out := make([]int, 0, len(res))
		for _, r := range res {
			id, _ := strconv.Atoi(r.Key)
			out = append(out, id)
		}
		return out
	}

	recallOf := func(search func([]float32) []int) float64 {
		var sum float64
		for qi, q := range test {
			got := search(q)
			gtSet := make(map[int]struct{}, K)
			for _, id := range gt[qi] {
				gtSet[id] = struct{}{}
			}
			hit := 0
			for _, id := range got {
				if _, ok := gtSet[id]; ok {
					hit++
				}
			}
			sum += float64(hit) / float64(K)
		}
		return sum / float64(len(test))
	}

	qpsOf := func(search func([]float32) []int, threads int) float64 {
		var cnt int64
		stop := make(chan struct{})
		var qwg sync.WaitGroup
		for w := 0; w < threads; w++ {
			qwg.Add(1)
			go func(seed int) {
				defer qwg.Done()
				i := seed
				for {
					select {
					case <-stop:
						return
					default:
					}
					search(test[i%len(test)])
					atomic.AddInt64(&cnt, 1)
					i++
				}
			}(w)
		}
		time.Sleep(qDur)
		close(stop)
		qwg.Wait()
		return float64(atomic.LoadInt64(&cnt)) / qDur.Seconds()
	}

	threads := runtime.NumCPU()
	if threads > 12 {
		threads = 12
	}

	type row struct {
		name   string
		search func([]float32) []int
	}
	rows := []row{
		{"(a) 12 fp32-графов (flushing сегодня)", searchDelta},
		{"(b) 12 мини-frozenSQ8 (план)", searchMinis},
		{"(c) 1 сегмент SQ8 (терминал)", searchSeg},
	}
	fmt.Println()
	fmt.Printf("ПОИСК в переходном состоянии (efSearch=%d, K=%d, %d потоков, recall@%d vs брутфорс):\n", efSearch, K, threads, K)
	fmt.Printf("%-42s %-12s %-12s %-10s\n", "состояние", "QPS_1thr", fmt.Sprintf("QPS_%dthr", threads), "recall@10")
	fmt.Println("--------------------------------------------------------------------------------")
	var qpsA, qpsB float64
	for i, r := range rows {
		rec := recallOf(r.search)
		q1 := qpsOf(r.search, 1)
		qN := qpsOf(r.search, threads)
		if i == 0 {
			qpsA = qN
		}
		if i == 1 {
			qpsB = qN
		}
		fmt.Printf("%-42s %-12.0f %-12.0f %.4f\n", r.name, q1, qN, rec)
	}
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf("Вывод: freeze-per-shard %.0f× дешевле rebuild; fan-out-поиск по мини-сегментам %.2f× vs fp32-графы.\n",
		rebuild.Seconds()/freezePar.Seconds(), qpsB/qpsA)

	// Страховка от регрессии смысла: recall плана не должен проседать против
	// сегодняшнего переходного состояния (SQ8-квантование стоит ≤~2пп).
	recA := recallOf(searchDelta)
	recB := recallOf(searchMinis)
	if recB < recA-0.02 {
		t.Fatalf("recall мини-сегментов %.4f просел против fp32-дельты %.4f (>2пп) — план не окупается", recB, recA)
	}
}
