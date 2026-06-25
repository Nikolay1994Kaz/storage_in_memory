package vector

// Прогрессивные бенчи "доказательства профита" по шагам оптимизации.
// Каждый шаг меряется на РЕАЛЬНЫХ данных (MNIST-784), до/после, чтобы видеть
// реальный выигрыш, а не теоретический. См. vector-optimization-theory.

import (
	"fmt"
	"math"
	"math/rand"
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
