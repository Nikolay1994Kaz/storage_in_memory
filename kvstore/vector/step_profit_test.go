//go:build datasets

// Замер на внешнем датасете. Тег `datasets` намеренно держит его ВНЕ обычной
// сборки тестов: без файла в /tmp такой тест скипался молча, и «ok» пакета
// означал в том числе «тридцать функций даже не пытались запуститься».
// Запуск и получение данных — docs/BENCHMARKS.md, раздел Reproducing:
//
//	make test-datasets

package vector

// Прогрессивные бенчи "доказательства профита" по шагам оптимизации.
// Каждый шаг меряется на РЕАЛЬНЫХ данных (MNIST-784), до/после, чтобы видеть
// реальный выигрыш, а не теоретический. См. vector-optimization-theory.

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// buildGraphM строит сырой HNSW-граф ТОЧНО как production buildSegment:
// M, M0=2M, Ml, и переаллокация pruneBuf под M0 (см. leveled_store.go:2167).
func buildGraphM(vecs [][]float32, M, efC int) (*Graph, time.Duration) {
	return buildGraphMH(vecs, M, efC, false)
}

// buildGraphMH — buildGraphM с переключателем HNSW select-neighbors heuristic.
func buildGraphMH(vecs [][]float32, M, efC int, heuristic bool) (*Graph, time.Duration) {
	alloc := tcmalloc.NewTCMallocStore(1)
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = efC
	g.HeuristicSelect = heuristic
	if M > 0 {
		g.M = M
		g.M0 = 2 * M
		g.Ml = 1.0 / math.Log(float64(M))
		g.pruneBufItems = make([]item, 0, g.M0+1)
		g.pruneBufIDs = make([]uint64, 0, g.M0+1)
		g.insertBuf = make([]uint64, 0, g.M0+1)
	}
	t0 := time.Now()
	for _, v := range vecs {
		g.Insert(v)
	}
	return g, time.Since(t0)
}

// TestStep1_EfConstruction — доказательство профита efConstruction 400→200.
// Гипотеза: insert ~2× быстрее при незначительной потере recall.
// Реальные MNIST-784, M=32 (как сервер), efSearch=200 (как сервер).
func TestStep1_EfConstruction(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data insert benchmark")
	}
	train, test, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/mnist784.bin: %v", err)
	}
	const (
		M        = 32
		efSearch = 200
		K        = 10
	)
	N := len(train)
	dim := len(train[0])
	t.Logf("Датасет MNIST: N=%d dim=%d queries=%d", N, dim, len(test))

	gt := bruteGT(train, test, K)

	fmt.Println()
	fmt.Printf("ШАГ 1 — efConstruction (MNIST-784, N=%d, M=%d, efSearch=%d, recall@%d):\n", N, M, efSearch, K)
	fmt.Printf("%-8s %-12s %-12s %-10s %-12s\n", "efC", "build", "insert/s", "recall", "vs efC=400")
	fmt.Println("--------------------------------------------------------------")

	var base400 float64
	for _, efC := range []int{400, 200, 100} {
		g, dur := buildGraphM(train, M, efC)
		insRate := float64(N) / dur.Seconds()
		rec := recallGraph(g, test, gt, K, efSearch)
		speedup := ""
		if efC == 400 {
			base400 = insRate
			speedup = "baseline"
		} else {
			speedup = fmt.Sprintf("%.2f× insert", insRate/base400)
		}
		fmt.Printf("%-8d %-12s %-12.0f %-10.3f %-12s\n",
			efC, dur.Round(time.Millisecond), insRate, rec, speedup)
	}
	fmt.Println("--------------------------------------------------------------")
	fmt.Println("Вывод: смотрим, даёт ли efC=200 заметный insert-выигрыш при recall ≥ ~0.96.")
}

// TestStep2_EfSearch — кривая recall↔QPS по efSearch (query-time ручка).
// Граф строится ОДИН раз (efC=400, M=32 — выбор шага 1). Свип efSearch
// показывает: где порог recall≥0.96 и какова его цена в QPS.
// QPS меряется на 12 потоках (как реальный e2e ~1100 QPS).
func TestStep2_EfSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data search benchmark")
	}
	train, test, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/mnist784.bin: %v", err)
	}
	const (
		M   = 32
		efC = 400
		K   = 10
		W   = 12
		dur = 3 * time.Second
	)
	N := len(train)
	t.Logf("Датасет MNIST: N=%d dim=%d queries=%d", N, len(train[0]), len(test))

	gt := bruteGT(train, test, K)
	g, build := buildGraphM(train, M, efC)
	t.Logf("Граф построен за %v (efC=%d, M=%d)", build.Round(time.Second), efC, M)

	fmt.Println()
	fmt.Printf("ШАГ 2 — efSearch recall↔QPS (MNIST-784, N=%d, M=%d, efC=%d, recall@%d, %d потоков):\n", N, M, efC, K, W)
	fmt.Printf("%-10s %-10s %-12s %-14s\n", "efSearch", "recall", "QPS_1thr", "QPS_12thr")
	fmt.Println("--------------------------------------------------------")

	for _, ef := range []int{50, 100, 150, 200, 300, 400} {
		rec := recallGraph(g, test, gt, K, ef)
		efLocal := ef
		qps1 := qpsGeneric(test, 1, dur, func(q []float32) { g.Search(q, K, efLocal) })
		qps12 := qpsGeneric(test, W, dur, func(q []float32) { g.Search(q, K, efLocal) })
		mark := ""
		if rec >= 0.96 {
			mark = "← recall≥0.96"
		}
		fmt.Printf("%-10d %-10.3f %-12.0f %-14.0f %s\n", ef, rec, qps1, qps12, mark)
	}
	fmt.Println("--------------------------------------------------------")
	fmt.Println("Вывод: выбираем минимальный efSearch с recall≥0.96 — это рабочая точка; QPS там = база для шага 3 (PQ).")
}

// TestStep4_HeuristicPrune — top-M vs HNSW select-neighbors heuristic.
// Гипотеза: эвристика даёт recall↑ при том же efSearch → можно снизить ef →
// больше QPS при равном recall. Цена — build чуть дороже.
// Сравниваем на одной кривой recall↔QPS (efSearch свип).
func TestStep4_HeuristicPrune(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data heuristic-prune benchmark")
	}
	train, test, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/mnist784.bin: %v", err)
	}
	const (
		M   = 32
		efC = 400
		K   = 10
		W   = 12
		dur = 3 * time.Second
	)
	N := len(train)
	gt := bruteGT(train, test, K)

	type variant struct {
		name      string
		heuristic bool
	}
	efs := []int{50, 100, 150, 200}

	fmt.Println()
	fmt.Printf("ШАГ 4 — top-M vs эвристика (MNIST-784, N=%d, M=%d, efC=%d, recall@%d, %d потоков):\n", N, M, efC, K, W)

	for _, v := range []variant{{"top-M (baseline)", false}, {"эвристика", true}} {
		// Фиксируем seed ПЕРЕД каждой сборкой → обе получают ИДЕНТИЧНЫЕ
		// случайные уровни HNSW. Иначе разница уровней — конфаунд (см. randomLevel).
		rand.Seed(42)
		g, build := buildGraphMH(train, M, efC, v.heuristic)
		fmt.Printf("\n[%s] build=%v (insert %.0f/s)\n", v.name, build.Round(time.Second), float64(N)/build.Seconds())
		fmt.Printf("  %-10s %-10s %-12s %-14s\n", "efSearch", "recall", "QPS_1thr", "QPS_12thr")
		for _, ef := range efs {
			rec := recallGraph(g, test, gt, K, ef)
			efLocal := ef
			qps1 := qpsGeneric(test, 1, dur, func(q []float32) { g.Search(q, K, efLocal) })
			qps12 := qpsGeneric(test, W, dur, func(q []float32) { g.Search(q, K, efLocal) })
			mark := ""
			if rec >= 0.96 {
				mark = "← recall≥0.96"
			}
			fmt.Printf("  %-10d %-10.3f %-12.0f %-14.0f %s\n", ef, rec, qps1, qps12, mark)
		}
	}
	fmt.Println("\nВывод: если у эвристики recall≥0.96 достигается на меньшем efSearch (= выше QPS) — профит реален.")
}

// TestStep5_ConcurrentAddCeiling — потолок конкурентных вставок.
// Гипотеза: Add держит глобальный lvs.mu.Lock, под ним тяжёлый HNSW Insert,
// всё сериализовано → insert/s НЕ масштабируется с числом горутин.
// Меряем insert/s на реальном LeveledVectorStore при W=1,2,4,8,12 писателях.
//
// DeltaMax > N → все вставки остаются в дельте, без флашей/компакции —
// изолируем чистую стоимость лока на горячем пути Add, а не шум фонового merge.
func TestStep5_ConcurrentAddCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data concurrent-insert benchmark")
	}
	train, _, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/mnist784.bin: %v", err)
	}
	const (
		M   = 32
		efC = 400
	)
	N := len(train)
	dim := len(train[0])
	t.Logf("Датасет MNIST: N=%d dim=%d", N, dim)

	fmt.Println()
	fmt.Printf("ШАГ 5 — потолок конкурентных Add (MNIST-784, N=%d, M=%d, efC=%d, без флашей):\n", N, M, efC)
	fmt.Printf("%-10s %-14s %-12s %-12s\n", "writers", "wall", "insert/s", "vs 1thr")
	fmt.Println("--------------------------------------------------------")

	var base1 float64
	for _, W := range []int{1, 2, 4, 8, 12} {
		lvs := NewLeveledVectorStore(LeveledConfig{
			DeltaMax:       N + 1, // всё в дельту, без флаша
			M:              M,
			EfConstruction: efC,
			Distance:       EuclideanDistance,
		})

		var idx int64 = -1
		var wg sync.WaitGroup
		t0 := time.Now()
		for w := 0; w < W; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(atomic.AddInt64(&idx, 1))
					if i >= N {
						return
					}
					_ = lvs.Add(fmt.Sprintf("k%d", i), train[i])
				}
			}()
		}
		wg.Wait()
		dur := time.Since(t0)
		rate := float64(N) / dur.Seconds()
		speedup := ""
		if W == 1 {
			base1 = rate
			speedup = "baseline"
		} else {
			speedup = fmt.Sprintf("%.2f×", rate/base1)
		}
		fmt.Printf("%-10d %-14s %-12.0f %-12s\n", W, dur.Round(time.Millisecond), rate, speedup)
		lvs.Close()
	}
	fmt.Println("--------------------------------------------------------")
	fmt.Println("Вывод: если insert/s плоский при росте writers — упёрлись в глобальный лок;")
	fmt.Println("это обосновывает пер-узловые/шардированные локи дельты (следующий шаг).")
}

// TestStep5_ShardedAddScaling — доказательство профита шардированной дельты
// НА РЕАЛЬНЫХ ДАННЫХ (MNIST-784) с проверкой recall: цифры отсюда стоят в
// справке флага -delta-shards.
//
// ⚠ЭТО РУЧНОЙ ПРОФИТ-БЕНЧ, А НЕ СТОРОЖ, и полагаться на него как на страховку
// нельзя: без /tmp/mnist784.bin он МОЛЧА СКИПАЕТСЯ, а пропущенный тест в
// выводе `go test` неотличим от пройденного. Датасета на машине нет, поэтому
// свойство «шардирование ускоряет вставку» шесть недель не стерёг никто —
// выяснилось 01.08, когда порог скорости в TestLeveledStore_500k упал впервые
// за это время.
//
// Автоматический сторож того же свойства теперь живёт отдельно и на синтетике,
// то есть исполняется всегда: TestShardedInsertScaling. Здешний бенч оставлен
// ради recall на реальных данных — синтетика этого не доказывает.
// Гипотеза: DeltaShards=S + S писателей → insert/s масштабируется ~S× (вставки
// в разные шарды идут под отдельными локами, без сериализации на lvs.mu).
// Recall шардированного поиска (фан-аут+merge) должен остаться ≥0.96 — иначе
// выигрыш по записи куплен потерей качества поиска.
//
// DeltaMax > N → всё остаётся в дельте (без флашей): меряем чистый write-path.
func TestStep5_ShardedAddScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data sharded-insert benchmark")
	}
	train, test, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/mnist784.bin: %v", err)
	}
	const (
		M        = 32
		efC      = 400
		efSearch = 200
		K        = 10
	)
	N := len(train)
	dim := len(train[0])
	gt := bruteGT(train, test, K)
	t.Logf("Датасет MNIST: N=%d dim=%d queries=%d", N, dim, len(test))

	fmt.Println()
	fmt.Printf("ШАГ 5 — шардированная дельта (MNIST-784, N=%d, M=%d, efC=%d, writers=shards, без флашей):\n", N, M, efC)
	fmt.Printf("%-9s %-9s %-14s %-12s %-12s %-10s\n", "shards", "writers", "wall", "insert/s", "vs 1shard", "recall@10")
	fmt.Println("----------------------------------------------------------------------")

	var base1 float64
	for _, S := range []int{1, 2, 4, 8, 12} {
		lvs := NewLeveledVectorStore(LeveledConfig{
			DeltaMax:       N + 1, // всё в дельту, без флаша
			DeltaShards:    S,
			M:              M,
			EfConstruction: efC,
			EfSearch:       efSearch,
			Distance:       EuclideanDistance,
		})

		var idx int64 = -1
		var wg sync.WaitGroup
		t0 := time.Now()
		for w := 0; w < S; w++ { // writers = shards
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(atomic.AddInt64(&idx, 1))
					if i >= N {
						return
					}
					_ = lvs.Add(fmt.Sprintf("%d", i), train[i]) // числовые ключи для recallStore
				}
			}()
		}
		wg.Wait()
		dur := time.Since(t0)
		rate := float64(N) / dur.Seconds()

		// Recall шардированного поиска против брутфорс-GT.
		rec := recallStore(lvs, test, gt, K)

		speedup := ""
		if S == 1 {
			base1 = rate
			speedup = "baseline"
		} else {
			speedup = fmt.Sprintf("%.2f×", rate/base1)
		}
		mark := ""
		if rec >= 0.96 {
			mark = "✓"
		}
		fmt.Printf("%-9d %-9d %-14s %-12.0f %-12s %.3f %s\n",
			S, S, dur.Round(time.Millisecond), rate, speedup, rec, mark)
		lvs.Close()
	}
	fmt.Println("----------------------------------------------------------------------")
	fmt.Println("Вывод: insert/s растёт с числом шардов при recall≥0.96 → шардинг снимает потолок записи.")
}

// TestStep5_ShardedSearchCost — цена шардинга для SEARCH QPS.
// Шардинг дельты ускоряет запись, но поиск теперь бьёт по S графам (фан-аут+merge)
// вместо одного. Меряем штраф на РЕАЛИСТИЧНОМ размере дельты (= deltaMax для 784 ≈ 33k),
// delta-only (худший случай: вся нагрузка поиска в дельте; на mixed-сторе дельта —
// лишь одна параллельная ветвь среди frozen-сегментов, штраф меньше).
func TestStep5_ShardedSearchCost(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data sharded-search-cost benchmark")
	}
	train, test, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/mnist784.bin: %v", err)
	}
	const (
		M        = 32
		efC      = 400
		efSearch = 200
		K        = 10
		W        = 12
		qDur     = 3 * time.Second
	)
	dim := len(train[0])
	N := deltaMax(dim, 0) // реалистичный максимум дельты (≈33444 для 784)
	if N > len(train) {
		N = len(train)
	}
	sub := train[:N]
	gt := bruteGT(sub, test, K)
	t.Logf("Реалистичный размер дельты N=%d dim=%d queries=%d", N, dim, len(test))

	fmt.Println()
	fmt.Printf("ШАГ 5 — цена шардинга для SEARCH (delta-only, N=%d, M=%d, efSearch=%d, recall@%d):\n", N, M, efSearch, K)
	fmt.Printf("%-9s %-12s %-14s %-12s %-10s\n", "shards", "QPS_1thr", "QPS_12thr", "vs 1shard", "recall@10")
	fmt.Println("----------------------------------------------------------------------")

	var base12 float64
	for _, S := range []int{1, 4, 8} {
		lvs := NewLeveledVectorStore(LeveledConfig{
			DeltaMax:       N + 1, // всё в дельту (delta-only)
			DeltaShards:    S,
			M:              M,
			EfConstruction: efC,
			EfSearch:       efSearch,
			Distance:       EuclideanDistance,
		})
		for i := 0; i < N; i++ {
			_ = lvs.Add(fmt.Sprintf("%d", i), sub[i])
		}

		searchFn := func(q []float32) {
			dst := make([]VSearchResult, 0, K)
			_, _ = lvs.Search(q, K, dst)
		}
		qps1 := qpsGeneric(test, 1, qDur, searchFn)
		qps12 := qpsGeneric(test, W, qDur, searchFn)
		rec := recallStore(lvs, test, gt, K)

		ratio := ""
		if S == 1 {
			base12 = qps12
			ratio = "baseline"
		} else {
			ratio = fmt.Sprintf("%.2f×", qps12/base12)
		}
		fmt.Printf("%-9d %-12.0f %-14.0f %-12s %.3f\n", S, qps1, qps12, ratio, rec)
		lvs.Close()
	}
	fmt.Println("----------------------------------------------------------------------")
	fmt.Println("Вывод: насколько падает search QPS от фан-аута по шардам (верхняя граница; на mixed меньше).")
}

// TestStep5_ShardedSearchMixed — РЕАЛИСТИЧНЫЙ mixed-стор: основная масса в frozen-
// сегментах, дельта мала (как в проде: дельта ~1-2% при 2-3М, тут ~9%). Сегменты
// строятся через FlushDeltaSync, затем небольшая живая дельта. Показывает НЕТТО-
// влияние шардинга дельты на search QPS, когда дельта — не весь стор.
func TestStep5_ShardedSearchMixed(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data mixed-search benchmark")
	}
	train, test, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/mnist784.bin: %v", err)
	}
	const (
		M        = 32
		efC      = 400
		efSearch = 200
		K        = 10
		W        = 12
		qDur     = 3 * time.Second
		deltaCap = 10_000 // маленькая дельта → сегменты доминируют
	)
	dim := len(train[0])
	segVecs := 50_000 // кратно deltaCap → 5 сегментов, дренаж через FlushDeltaSync
	liveDelta := 5_000
	N := segVecs + liveDelta
	if N > len(train) {
		t.Skipf("датасет мал: нужно %d, есть %d", N, len(train))
	}
	gt := bruteGT(train[:N], test, K)
	deltaShare := float64(liveDelta) / float64(N) * 100
	t.Logf("Mixed: всего N=%d, в сегментах=%d, живая дельта=%d (~%.0f%%), dim=%d",
		N, segVecs, liveDelta, deltaShare, dim)

	fmt.Println()
	fmt.Printf("ШАГ 5 — search на MIXED-сторе (N=%d, дельта~%.0f%%, M=%d, efSearch=%d, recall@%d):\n", N, deltaShare, M, efSearch, K)
	fmt.Printf("%-9s %-12s %-14s %-12s %-10s\n", "shards", "QPS_1thr", "QPS_12thr", "vs 1shard", "recall@10")
	fmt.Println("----------------------------------------------------------------------")

	var base12 float64
	for _, S := range []int{1, 4, 8} {
		lvs := NewLeveledVectorStore(LeveledConfig{
			DeltaMax:       deltaCap,
			DeltaShards:    S,
			M:              M,
			EfConstruction: efC,
			EfSearch:       efSearch,
			Distance:       EuclideanDistance,
		})
		// Заполняем сегменты: кратно deltaCap, затем синхронный флаш в frozen.
		for i := 0; i < segVecs; i++ {
			_ = lvs.Add(fmt.Sprintf("%d", i), train[i])
		}
		lvs.FlushDeltaSync()
		// Остаток → живая дельта (шардированная).
		for i := segVecs; i < N; i++ {
			_ = lvs.Add(fmt.Sprintf("%d", i), train[i])
		}

		searchFn := func(q []float32) {
			dst := make([]VSearchResult, 0, K)
			_, _ = lvs.Search(q, K, dst)
		}
		qps1 := qpsGeneric(test, 1, qDur, searchFn)
		qps12 := qpsGeneric(test, W, qDur, searchFn)
		rec := recallStore(lvs, test, gt, K)

		ratio := ""
		if S == 1 {
			base12 = qps12
			ratio = "baseline"
		} else {
			ratio = fmt.Sprintf("%.2f×", qps12/base12)
		}
		fmt.Printf("%-9d %-12.0f %-14.0f %-12s %.3f\n", S, qps1, qps12, ratio, rec)
		lvs.Close()
	}
	fmt.Println("----------------------------------------------------------------------")
	fmt.Println("Вывод: НЕТТО-штраф search QPS на реалистичном mixed-сторе — основа решения о дефолте.")
}

// TestStep6_CurrentStateQPS — ТЕКУЩАЯ способность движка в serve-конфиге сервера
// (SQ8, M=32, efC=400, эвристика ВКЛ по умолчанию), на реальных MNIST-784 60k,
// данные осели во frozen-SQ-сегменты (как steady-state serve). Свип efSearch
// показывает кривую recall↔QPS СЕЙЧАС (после шагов 2+4) — где мы относительно
// планки 2.5-3К QPS @ recall≥0.96. Старое число 1100 QPS (ef=200, без эвристики)
// устарело — это его честная замена.
func TestStep6_CurrentStateQPS(t *testing.T) {
	if testing.Short() {
		t.Skip("long: real-data current-state QPS benchmark")
	}
	train, test, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/mnist784.bin: %v", err)
	}
	const (
		M    = 32
		efC  = 400
		K    = 10
		W    = 12
		qDur = 3 * time.Second
	)
	N := len(train)
	dim := len(train[0])
	gt := bruteGT(train[:N], test, K)
	t.Logf("Датасет MNIST: N=%d dim=%d queries=%d", N, dim, len(test))

	// Стор как сервер: SQ8, M=32, efC=400, эвристика по умолчанию.
	lvs := NewLeveledVectorStore(LeveledConfig{
		M:              M,
		EfConstruction: efC,
		EfSearch:       100,
		UseSQ:          true,
		Distance:       EuclideanDistance,
	})
	for i := 0; i < N; i++ {
		_ = lvs.Add(fmt.Sprintf("%d", i), train[i])
	}
	lvs.FlushDeltaSync() // осесть во frozen-SQ-сегменты (steady-state serve)
	defer lvs.Close()

	fmt.Println()
	fmt.Printf("ШАГ 6 — ТЕКУЩЕЕ состояние QPS (MNIST-784, N=%d, SQ8, M=%d, efC=%d, эвристика ВКЛ, %d потоков):\n", N, M, efC, W)
	fmt.Printf("%-10s %-10s %-12s %-14s %-12s\n", "efSearch", "recall", "QPS_1thr", "QPS_12thr", "vs план2.5К")
	fmt.Println("----------------------------------------------------------------------")

	for _, ef := range []int{50, 75, 100, 150, 200} {
		lvs.SetHNSWParams(0, 0, ef)
		rec := recallStore(lvs, test, gt, K)
		searchFn := func(q []float32) {
			dst := make([]VSearchResult, 0, K)
			_, _ = lvs.Search(q, K, dst)
		}
		qps1 := qpsGeneric(test, 1, qDur, searchFn)
		qps12 := qpsGeneric(test, W, qDur, searchFn)
		mark := ""
		if rec >= 0.96 && qps12 >= 2500 {
			mark = "✓ цель достигнута"
		} else if rec >= 0.96 {
			mark = "recall ok"
		}
		fmt.Printf("%-10d %-10.3f %-12.0f %-14.0f %-12s\n", ef, rec, qps1, qps12, mark)
	}
	fmt.Println("----------------------------------------------------------------------")
	fmt.Println("Вывод: минимальный efSearch с recall≥0.96 = рабочая точка; смотрим QPS_12 vs планка 2.5-3К.")
}

// TestSIFT1M_Validation — валидация на ЭТАЛОННОМ датасете SIFT-1M (1М векторов,
// dim 128, реальные image-дескрипторы). Это датасет, на котором ann-benchmarks.com
// ранжирует hnswlib/Qdrant/Faiss → наши recall↔QPS можно положить рядом с
// опубликованными и сказать «мировой уровень» по факту, а не на словах.
// Конфиг как в эталоне: M=16, efC=200, float32 (без SQ — как публикуют). Свип ef
// от низких значений (где индустрия меряет высокий QPS).
func TestSIFT1M_Validation(t *testing.T) {
	if testing.Short() {
		t.Skip("long: SIFT-1M validation")
	}
	train, test, err := loadSIFTRaw("/tmp/sift1m.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/sift1m.bin: %v", err)
	}
	const (
		M    = 16
		efC  = 200
		K    = 10
		W    = 12
		qDur = 3 * time.Second
	)
	N := len(train)
	dim := len(train[0])
	t.Logf("SIFT-1M: N=%d dim=%d queries=%d", N, dim, len(test))

	tGT := time.Now()
	gt := bruteGT(train, test, K)
	t.Logf("bruteGT (exact) построен за %v", time.Since(tGT).Round(time.Second))

	lvs := NewLeveledVectorStore(LeveledConfig{
		M:              M,
		EfConstruction: efC,
		EfSearch:       64,
		Distance:       EuclideanDistance,
	})
	tB := time.Now()
	for i := 0; i < N; i++ {
		_ = lvs.Add(fmt.Sprintf("%d", i), train[i])
	}
	lvs.FlushDeltaSync()
	buildDur := time.Since(tB)
	defer lvs.Close()
	t.Logf("Построение 1М за %v (insert %.0f/s)", buildDur.Round(time.Second), float64(N)/buildDur.Seconds())

	fmt.Println()
	fmt.Printf("ВАЛИДАЦИЯ SIFT-1M (N=%d, dim=%d, M=%d, efC=%d, float32, recall@%d, %d потоков):\n", N, dim, M, efC, K, W)
	fmt.Printf("%-10s %-10s %-12s %-14s\n", "efSearch", "recall", "QPS_1thr", "QPS_12thr")
	fmt.Println("--------------------------------------------------------")
	for _, ef := range []int{16, 32, 64, 128, 256} {
		lvs.SetHNSWParams(0, 0, ef)
		rec := recallStore(lvs, test, gt, K)
		searchFn := func(q []float32) {
			dst := make([]VSearchResult, 0, K)
			_, _ = lvs.Search(q, K, dst)
		}
		qps1 := qpsGeneric(test, 1, qDur, searchFn)
		qps12 := qpsGeneric(test, W, qDur, searchFn)
		mark := ""
		if rec >= 0.96 {
			mark = "← recall≥0.96"
		}
		fmt.Printf("%-10d %-10.3f %-12.0f %-14.0f %s\n", ef, rec, qps1, qps12, mark)
	}
	fmt.Println("--------------------------------------------------------")
	fmt.Println("Сравни с публичными hnswlib/Qdrant на SIFT-1M: recall@10≈0.95-0.99, та же лига QPS = мировой уровень.")
}

// TestSIFT1M_SegmentEffect — проверка гипотезы: мульти-сегментный fan-out (поиск
// по N сегментам + merge на КАЖДЫЙ запрос) — причина слабого многопоточного
// скейлинга vs монолитный граф hnswlib. A/B на SIFT-1M:
//
//	A = много сегментов (DeltaMax=50k → ~неск. frozen-сегментов)
//	B = один сегмент    (DeltaMax=N  → 1 крупный frozen-сегмент, как монолит hnswlib)
//
// Если B скейлит лучше / даёт выше QPS при равном recall — гипотеза верна,
// консолидация сегментов = бесплатный рычаг QPS (через deltaMax/fanout).
func TestSIFT1M_SegmentEffect(t *testing.T) {
	if testing.Short() {
		t.Skip("long: SIFT-1M segment-count A/B")
	}
	train, test, err := loadSIFTRaw("/tmp/sift1m.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/sift1m.bin: %v", err)
	}
	const (
		M    = 16
		efC  = 200
		K    = 10
		W    = 12
		qDur = 3 * time.Second
	)
	N := len(train)
	dim := len(train[0])
	gt := bruteGT(train, test, K)
	t.Logf("SIFT-1M: N=%d dim=%d queries=%d", N, dim, len(test))

	type cfg struct {
		name     string
		deltaMax int
	}
	for _, c := range []cfg{{"A: много сегментов (deltaMax=50k)", 50_000}, {"B: один сегмент (deltaMax=N)", N}} {
		lvs := NewLeveledVectorStore(LeveledConfig{
			DeltaMax:       c.deltaMax,
			M:              M,
			EfConstruction: efC,
			EfSearch:       64,
			Distance:       EuclideanDistance,
		})
		for i := 0; i < N; i++ {
			_ = lvs.Add(fmt.Sprintf("%d", i), train[i])
		}
		lvs.FlushDeltaSync()
		st := lvs.Stats()
		nSeg := 0
		for _, s := range st.SegmentsByLevel {
			nSeg += s
		}

		fmt.Println()
		fmt.Printf("[%s] сегментов=%d (по уровням %v):\n", c.name, nSeg, st.SegmentsByLevel)
		fmt.Printf("  %-10s %-10s %-12s %-14s %-10s\n", "efSearch", "recall", "QPS_1thr", "QPS_12thr", "scaling")
		for _, ef := range []int{32, 64, 128} {
			lvs.SetHNSWParams(0, 0, ef)
			rec := recallStore(lvs, test, gt, K)
			searchFn := func(q []float32) {
				dst := make([]VSearchResult, 0, K)
				_, _ = lvs.Search(q, K, dst)
			}
			qps1 := qpsGeneric(test, 1, qDur, searchFn)
			qps12 := qpsGeneric(test, W, qDur, searchFn)
			fmt.Printf("  %-10d %-10.3f %-12.0f %-14.0f %.2f×\n", ef, rec, qps1, qps12, qps12/qps1)
		}
		lvs.Close()
	}
	fmt.Println()
	fmt.Println("Вывод: если B (1 сегмент) даёт выше QPS/scaling при равном recall — fan-out по сегментам подтверждён как рычаг.")
}

// TestSIFT1M_DeltaMaxSweep — практическая кривая «настройка deltaMax → QPS», БЕЗ
// единой строки кода (только LeveledConfig.DeltaMax). Показывает: сколько
// сегментов и какой QPS при разных настройках консолидации, и где realistic
// sweet spot (2-4 сегмента vs крайность 1). Прямой ответ «крутишь настройку — QPS растёт».
func TestSIFT1M_DeltaMaxSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("long: SIFT-1M deltaMax sweep")
	}
	train, test, err := loadSIFTRaw("/tmp/sift1m.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/sift1m.bin: %v", err)
	}
	const (
		M    = 16
		efC  = 200
		K    = 10
		W    = 12
		qDur = 3 * time.Second
	)
	N := len(train)
	dim := len(train[0])
	gt := bruteGT(train, test, K)
	t.Logf("SIFT-1M: N=%d dim=%d queries=%d", N, dim, len(test))

	fmt.Println()
	fmt.Printf("SIFT-1M — настройка deltaMax → QPS (БЕЗ кода, только конфиг; M=%d, efC=%d, %d потоков):\n", M, efC, W)
	fmt.Printf("%-12s %-8s %-8s %-10s %-12s %-14s %-8s\n", "deltaMax", "segs", "ef", "recall", "QPS_1thr", "QPS_12thr", "scaling")
	fmt.Println("--------------------------------------------------------------------------")

	for _, dm := range []int{50_000, 250_000, 1_000_000} {
		lvs := NewLeveledVectorStore(LeveledConfig{
			DeltaMax:       dm,
			M:              M,
			EfConstruction: efC,
			EfSearch:       64,
			Distance:       EuclideanDistance,
		})
		for i := 0; i < N; i++ {
			_ = lvs.Add(fmt.Sprintf("%d", i), train[i])
		}
		lvs.FlushDeltaSync()
		st := lvs.Stats()
		nSeg := 0
		for _, s := range st.SegmentsByLevel {
			nSeg += s
		}
		for _, ef := range []int{64, 128} {
			lvs.SetHNSWParams(0, 0, ef)
			rec := recallStore(lvs, test, gt, K)
			searchFn := func(q []float32) {
				dst := make([]VSearchResult, 0, K)
				_, _ = lvs.Search(q, K, dst)
			}
			qps1 := qpsGeneric(test, 1, qDur, searchFn)
			qps12 := qpsGeneric(test, W, qDur, searchFn)
			fmt.Printf("%-12d %-8d %-8d %-10.3f %-12.0f %-14.0f %.2f×\n",
				dm, nSeg, ef, rec, qps1, qps12, qps12/qps1)
		}
		lvs.Close()
	}
	fmt.Println("--------------------------------------------------------------------------")
	fmt.Println("Вывод: меньше сегментов (выше deltaMax) → выше QPS при равном recall, чистой настройкой.")
}

// TestGIST1M_Validation — кульминация: ТВОЙ целевой путь (высокая размерность 960
// ≈ 768, SQ8) на эталонном GIST-1M, консолидированный в 1 сегмент (как монолит
// hnswlib). Высокая внутр. размерность = честный тест (сложнее SIFT/MNIST).
// SQ8 = 4× меньше трафика памяти → на bandwidth-bound масштабе должен помогать.
// Сравнивать с hnswlib float32 (scratchpad/h2h_hnswlib.py на /tmp/gist).
func TestGIST1M_Validation(t *testing.T) {
	if testing.Short() {
		t.Skip("long: GIST-1M validation (heavy build ~30-40min)")
	}
	train, test, err := loadSIFTRaw("/tmp/gist_sub.bin")
	if err != nil {
		t.Skipf("no dataset /tmp/gist_sub.bin: %v", err)
	}
	const (
		M    = 16
		efC  = 200
		K    = 10
		W    = 12
		qDur = 3 * time.Second
	)
	N := len(train)
	dim := len(train[0])
	tGT := time.Now()
	gt := bruteGT(train, test, K)
	t.Logf("GIST-1M: N=%d dim=%d queries=%d; bruteGT за %v", N, dim, len(test), time.Since(tGT).Round(time.Second))

	// Целевой путь: SQ8, консолидация в 1 сегмент (deltaMax=N → freeze-on-flush SQ8).
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       N,
		M:              M,
		EfConstruction: efC,
		EfSearch:       64,
		UseSQ:          true,
		Distance:       EuclideanDistance,
	})
	tB := time.Now()
	for i := 0; i < N; i++ {
		_ = lvs.Add(fmt.Sprintf("%d", i), train[i])
	}
	lvs.FlushDeltaSync()
	defer lvs.Close()
	st := lvs.Stats()
	nSeg := 0
	for _, s := range st.SegmentsByLevel {
		nSeg += s
	}
	t.Logf("Построение 1М×%d SQ8 за %v (%.0f/s), сегментов=%d", dim, time.Since(tB).Round(time.Second), float64(N)/time.Since(tB).Seconds(), nSeg)

	fmt.Println()
	fmt.Printf("ВАЛИДАЦИЯ GIST-1M (N=%d, dim=%d, SQ8, M=%d, efC=%d, сегментов=%d, recall@%d, %d потоков):\n", N, dim, M, efC, nSeg, K, W)
	fmt.Printf("%-10s %-10s %-12s %-14s\n", "efSearch", "recall", "QPS_1thr", "QPS_12thr")
	fmt.Println("--------------------------------------------------------")
	for _, ef := range []int{32, 64, 128, 256} {
		lvs.SetHNSWParams(0, 0, ef)
		rec := recallStore(lvs, test, gt, K)
		searchFn := func(q []float32) {
			dst := make([]VSearchResult, 0, K)
			_, _ = lvs.Search(q, K, dst)
		}
		qps1 := qpsGeneric(test, 1, qDur, searchFn)
		qps12 := qpsGeneric(test, W, qDur, searchFn)
		mark := ""
		if rec >= 0.96 {
			mark = "← recall≥0.96"
		}
		fmt.Printf("%-10d %-10.3f %-12.0f %-14.0f %s\n", ef, rec, qps1, qps12, mark)
	}
	fmt.Println("--------------------------------------------------------")
	fmt.Println("Сравни с hnswlib float32 на GIST-1M (та же машина): наш SQ8 = 4× меньше памяти/трафика.")
}
