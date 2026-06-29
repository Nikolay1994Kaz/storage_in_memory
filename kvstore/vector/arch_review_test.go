package vector

// =============================================================================
// ARCH REVIEW BENCH — изолированная проверка архитектурных гипотез.
//
// Запуск:
//   go test ./kvstore/vector/ -run TestArch -v -timeout 1800s
//
// Гипотезы:
//   H1 (FanOut)      : QPS падает ~линейно с числом сегментов при РАВНОМ recall.
//                      Причина 790 QPS — фан-аут, а не "потолок dim=128".
//   H2 (Dispatch)    : parallel-per-segment под нагрузкой проигрывает
//                      sequential-per-segment по aggregate QPS (oversubscription).
//   H3 (uint16)      : экономия uint16 vs uint32 на топологии мала (% от памяти),
//                      а потолок 65535 — дорогой.
//   H4 (SQ@highdim)  : проверяем claim пользователя, что SQ просаживает при dim>256.
// =============================================================================

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// fseg — единый интерфейс поиска для FrozenGraph и FrozenGraphSQ.
type fseg interface {
	Search(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult
}

const arEfConstruction = 128

// buildSegs строит k frozen-сегментов из vecs (глобальные id = индекс в vecs).
func buildSegs(vecs [][]float32, k int, useSQ bool) []fseg {
	n := len(vecs)
	per := (n + k - 1) / k
	segs := make([]fseg, 0, k)
	for s := 0; s < k; s++ {
		lo := s * per
		hi := min(lo+per, n)
		if lo >= hi {
			break
		}
		alloc := tcmalloc.NewTCMallocStore(1)
		g := NewGraph(EuclideanDistance, alloc)
		g.EfConstruction = arEfConstruction
		keys := make(map[uint64]string, hi-lo)
		for i := lo; i < hi; i++ {
			idx := g.Insert(vecs[i])
			keys[uint64(idx)] = strconv.Itoa(i)
		}
		if useSQ {
			segs = append(segs, FreezeGraphSQ(g, MetricEuclidean, keys))
		} else {
			segs = append(segs, FreezeGraph(g, EuclideanDistance, keys))
		}
	}
	return segs
}

// mergeTopK сливает результаты сегментов в общий top-K.
func mergeTopK(parts [][]FrozenResult, K int) []FrozenResult {
	var all []FrozenResult
	for _, p := range parts {
		all = append(all, p...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Dist < all[j].Dist })
	if len(all) > K {
		all = all[:K]
	}
	return all
}

// searchSeq — один запрос обходит все сегменты последовательно (1 горутина).
func searchSeq(segs []fseg, q []float32, K, ef int) []FrozenResult {
	parts := make([][]FrozenResult, len(segs))
	for i, s := range segs {
		r := s.Search(q, K, ef, nil, nil)
		parts[i] = r
	}
	return mergeTopK(parts, K)
}

// searchPar — один запрос порождает горутину на сегмент (как searchSegmentsParallel).
func searchPar(segs []fseg, q []float32, K, ef int) []FrozenResult {
	parts := make([][]FrozenResult, len(segs))
	var wg sync.WaitGroup
	for i, s := range segs {
		wg.Add(1)
		go func(i int, s fseg) {
			defer wg.Done()
			parts[i] = s.Search(q, K, ef, nil, nil)
		}(i, s)
	}
	wg.Wait()
	return mergeTopK(parts, K)
}

// bruteGT — точный top-K брутфорсом (глобальные id), параллельно по запросам.
func bruteGT(vecs, queries [][]float32, K int) [][]int {
	gt := make([][]int, len(queries))
	var next int64 = -1
	var wg sync.WaitGroup
	workers := 12
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			type cand struct {
				id int
				d  float32
			}
			for {
				qi := int(atomic.AddInt64(&next, 1))
				if qi >= len(queries) {
					return
				}
				q := queries[qi]
				top := make([]cand, 0, K+1)
				for id, v := range vecs {
					d := EuclideanDistance(q, v)
					if len(top) < K {
						top = append(top, cand{id, d})
						if len(top) == K {
							sort.Slice(top, func(i, j int) bool { return top[i].d < top[j].d })
						}
						continue
					}
					if d < top[K-1].d {
						// вставка с сохранением порядка
						pos := sort.Search(K, func(i int) bool { return top[i].d >= d })
						copy(top[pos+1:], top[pos:K-1])
						top[pos] = cand{id, d}
					}
				}
				if len(top) < K {
					sort.Slice(top, func(i, j int) bool { return top[i].d < top[j].d })
				}
				ids := make([]int, len(top))
				for i := range top {
					ids[i] = top[i].id
				}
				gt[qi] = ids
			}
		}()
	}
	wg.Wait()
	return gt
}

// recallAt считает средний recall@K по запросам.
func recallAt(segs []fseg, queries [][]float32, gt [][]int, K, ef int) float64 {
	var sum float64
	for qi, q := range queries {
		res := searchSeq(segs, q, K, ef)
		gtset := make(map[int]struct{}, K)
		for _, id := range gt[qi] {
			gtset[id] = struct{}{}
		}
		hit := 0
		for _, r := range res {
			id, _ := strconv.Atoi(r.Key)
			if _, ok := gtset[id]; ok {
				hit++
			}
		}
		sum += float64(hit) / float64(K)
	}
	return sum / float64(len(queries))
}

// qps измеряет throughput: W воркеров крутят запросы dur времени.
func qps(segs []fseg, queries [][]float32, K, ef, W int, dur time.Duration,
	dispatch func([]fseg, []float32, int, int) []FrozenResult) float64 {
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
				_ = dispatch(segs, q, K, ef)
				local++
			}
			atomic.AddInt64(&ops, int64(local))
		}()
	}
	wg.Wait()
	return float64(ops) / dur.Seconds()
}

// =============================================================================
// H1 + H2: FanOut и Dispatch
// =============================================================================

func TestArch_FanOut(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	const N = 100_000
	if len(train) > N {
		train = train[:N]
	}
	K, ef, W := 10, 64, 12
	qDur := 3 * time.Second

	t.Logf("Dataset: N=%d dim=%d queries=%d | K=%d efPerSeg=%d workers=%d",
		len(train), len(train[0]), len(test), K, ef, W)
	t.Log("Считаю ground truth (brute force)...")
	t0 := time.Now()
	gt := bruteGT(train, test, K)
	t.Logf("GT готов за %v", time.Since(t0).Round(time.Millisecond))

	fmt.Println()
	fmt.Println("H1/H2 FAN-OUT (float32 frozen, fixed efPerSeg=64, equal quality):")
	fmt.Printf("%-8s %-9s %-12s %-14s %-14s %-10s\n",
		"segs", "recall", "QPS_1thr", "QPS_seq(12)", "QPS_par(12)", "par/seq")
	for _, k := range []int{1, 2, 4, 8, 16} {
		segs := buildSegs(train, k, false)
		rec := recallAt(segs, test, gt, K, ef)
		q1 := qps(segs, test, K, ef, 1, qDur, searchSeq)
		qSeq := qps(segs, test, K, ef, W, qDur, searchSeq)
		qPar := qps(segs, test, K, ef, W, qDur, searchPar)
		fmt.Printf("%-8d %-9.3f %-12.0f %-14.0f %-14.0f %-10.2f\n",
			k, rec, q1, qSeq, qPar, qPar/qSeq)
	}
	fmt.Println()
	fmt.Println("Чтение: если recall ~равен по строкам, а QPS падает с ростом segs —")
	fmt.Println("это чистый оверхед фан-аута (H1). Если QPS_par < QPS_seq при 12 воркерах —")
	fmt.Println("parallel-per-segment вредит throughput под нагрузкой (H2).")
}

// TestArch_FanOutMatched — честное сравнение QPS при РАВНОМ recall.
// Для каждого k подбираем ef, дающий recall ≥ target, и меряем QPS там.
func TestArch_FanOutMatched(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	const N = 100_000
	if len(train) > N {
		train = train[:N]
	}
	K := 10
	const target = 0.95
	qDur := 3 * time.Second
	gt := bruteGT(train, test, K)

	fmt.Println()
	fmt.Printf("H1 MATCHED-RECALL (float32, N=%d, цель recall≥%.2f):\n", len(train), target)
	fmt.Printf("%-8s %-8s %-9s %-12s %-14s\n", "segs", "ef*", "recall", "QPS_1thr", "QPS_seq(12)")
	efGrid := []int{16, 24, 32, 48, 64, 96, 128, 192, 256, 384, 512}
	for _, k := range []int{1, 4, 16} {
		segs := buildSegs(train, k, false)
		bestEf, bestRec := -1, 0.0
		for _, ef := range efGrid {
			rec := recallAt(segs, test, gt, K, ef)
			bestEf, bestRec = ef, rec
			if rec >= target {
				break
			}
		}
		q1 := qps(segs, test, K, bestEf, 1, qDur, searchSeq)
		qSeq := qps(segs, test, K, bestEf, 12, qDur, searchSeq)
		fmt.Printf("%-8d %-8d %-9.3f %-12.0f %-14.0f\n", k, bestEf, bestRec, q1, qSeq)
	}
	fmt.Println("Вывод: при равном recall сравниваем QPS. Падение = чистая цена фан-аута (H1).")
}

// qpsStore меряет throughput реального LeveledVectorStore (текущий диспетчер поиска).
func qpsStore(lvs *LeveledVectorStore, queries [][]float32, K, W int, dur time.Duration) float64 {
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
				_, _ = lvs.Search(q, K, nil)
				local++
			}
			atomic.AddInt64(&ops, int64(local))
		}()
	}
	wg.Wait()
	return float64(ops) / dur.Seconds()
}

func recallStore(lvs *LeveledVectorStore, queries [][]float32, gt [][]int, K int) float64 {
	var sum float64
	for qi, q := range queries {
		res, _ := lvs.Search(q, K, nil)
		gtset := make(map[int]struct{}, K)
		for _, id := range gt[qi] {
			gtset[id] = struct{}{}
		}
		hit := 0
		for _, r := range res {
			id, _ := strconv.Atoi(r.Key)
			if _, ok := gtset[id]; ok {
				hit++
			}
		}
		sum += float64(hit) / float64(K)
	}
	return sum / float64(len(queries))
}

// TestArch_P2_Segments — реальный LeveledVectorStore: сколько сегментов в устойчивом
// состоянии, какого они ТИПА (frozen=быстрый / hnsw=медленный fallback), QPS, recall.
func TestArch_P2_Segments(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	const N = 200_000
	train = train[:N]
	K := 10
	gt := bruteGT(train, test, K)

	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(8),
		DeltaMax:  20_000, // мельче → больше сегментов (имитация 500k/50k фрагментации)
		Fanout:    4,
		EfSearch:  128,
		UseSQ:     true,
	}
	lvs := NewLeveledVectorStore(cfg)
	t0 := time.Now()
	for i, v := range train {
		_ = lvs.Add(strconv.Itoa(i), v)
	}
	t.Logf("Inserted %d за %v, ждём устаканивания компакции...", N, time.Since(t0).Round(time.Second))

	// Дать каскаду merge устояться.
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
	lvs.mu.RLock()
	var nFrozen, nSQ, nHNSW, total int
	for _, lvl := range lvs.levels {
		for _, s := range lvl {
			total++
			switch s.(type) {
			case *frozenSegment:
				nFrozen++
			case *frozenSQSegment:
				nSQ++
			case *hnswSegment:
				nHNSW++
			}
		}
	}
	lvs.mu.RUnlock()

	rec := recallStore(lvs, test, gt, K)
	q12 := qpsStore(lvs, test, K, 12, 3*time.Second)

	fmt.Println()
	fmt.Printf("P2 SEGMENTS (LeveledVectorStore, N=%d, deltaMax=20k, fanout=4, UseSQ):\n", N)
	fmt.Printf("  SegmentsByLevel: %v\n", st.SegmentsByLevel)
	fmt.Printf("  searchable сегментов: %d (frozenSQ=%d, frozen=%d, hnsw_SLOW=%d)\n",
		total, nSQ, nFrozen, nHNSW)
	fmt.Printf("  recall@10: %.3f | QPS(12thr): %.0f | settle: %v\n",
		rec, q12, time.Since(settleStart).Round(time.Second))
	fmt.Println("Цель P2: total ≤ 4 и hnsw_SLOW=0. Если так — консолидация уже ок после P1.")
}

// measureLeveled500k — общий замер: build 500k, устаканить, вернуть стат-строку.
func measureLeveled500k(t *testing.T, deltaMax int) {
	train, test, err := loadSIFTRaw("/tmp/sift500k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	K := 10
	gt := bruteGT(train, test, K)
	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(8),
		DeltaMax:  deltaMax,
		Fanout:    4,
		EfSearch:  128,
		UseSQ:     true,
	}
	lvs := NewLeveledVectorStore(cfg)
	for i, v := range train {
		_ = lvs.Add(strconv.Itoa(i), v)
	}
	settleStart := time.Now()
	for time.Since(settleStart) < 500*time.Second {
		lvs.FlushDeltaSync()
		if !lvs.anyMergeInFlight() {
			time.Sleep(500 * time.Millisecond)
			if !lvs.anyMergeInFlight() {
				break
			}
		}
		time.Sleep(time.Second)
	}
	st := lvs.Stats()
	lvs.mu.RLock()
	var nSQ, nFrozen, nHNSW, total int
	for _, lvl := range lvs.levels {
		for _, s := range lvl {
			total++
			switch s.(type) {
			case *frozenSQSegment:
				nSQ++
			case *frozenSegment:
				nFrozen++
			case *hnswSegment:
				nHNSW++
			}
		}
	}
	lvs.mu.RUnlock()
	rec := recallStore(lvs, test, gt, K)
	q1 := qpsStore(lvs, test, K, 1, 3*time.Second)
	q12 := qpsStore(lvs, test, K, 12, 3*time.Second)
	fmt.Println()
	fmt.Printf("P2 500k deltaMax=%d: segs=%v total=%d (frozenSQ=%d hnsw_SLOW=%d)\n",
		deltaMax, st.SegmentsByLevel, total, nSQ, nHNSW)
	fmt.Printf("  recall@10: %.3f | QPS_1thr: %.0f | QPS_12thr: %.0f | settle: %v\n",
		rec, q1, q12, time.Since(settleStart).Round(time.Second))
}

// TestArch_P2_500k — продакшн-конфиг 50k (было 790 QPS до P1).
func TestArch_P2_500k(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	measureLeveled500k(t, 50_000)
}

// TestArch_P2_500k_DM100 — deltaMax=100k → 2 сегмента (проверка «4→2 лучше?»).
func TestArch_P2_500k_DM100(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	measureLeveled500k(t, 100_000)
}

// qpsGeneric — общий замер throughput для произвольной search-функции.
func qpsGeneric(queries [][]float32, W int, dur time.Duration, fn func(q []float32)) float64 {
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
				fn(queries[int(atomic.AddInt64(&idx, 1))%len(queries)])
				local++
			}
			atomic.AddInt64(&ops, int64(local))
		}()
	}
	wg.Wait()
	return float64(ops) / dur.Seconds()
}

// TestArch_DeltaBrute — стоимость брутфорс-дельты vs индексированной (HNSW) дельты.
// Доказывает/опровергает: индексировать дельту (модель Qdrant) выгодно.
func TestArch_DeltaBrute(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	K, ef := 10, 128
	qDur := 3 * time.Second

	fmt.Println()
	fmt.Println("DELTA: brute-force vs HNSW-индекс (тот же буфер), SIFT dim=128:")
	fmt.Printf("%-9s %-22s %-12s %-12s %-10s\n", "n_delta", "method", "QPS_1thr", "QPS_12thr", "recall")
	for _, n := range []int{10_000, 25_000, 50_000, 100_000} {
		sub := train[:n]
		gt := bruteGT(sub, test, K)

		// Брутфорс-дельта (как сейчас).
		delta := NewDeltaSegment(128, n+1, EuclideanDistance, 0, 0)
		for i, v := range sub {
			delta.Append(strconv.Itoa(i), v)
		}
		bfQ1 := qpsGeneric(test, 1, qDur, func(q []float32) {
			_ = delta.BruteForce(q, K, EuclideanDistance, nil)
		})
		bfQN := qpsGeneric(test, 12, qDur, func(q []float32) {
			_ = delta.BruteForce(q, K, EuclideanDistance, nil)
		})

		// HNSW-индекс того же буфера (модель Qdrant).
		g := buildGraph(sub, 200)
		hnQ1 := qpsGeneric(test, 1, qDur, func(q []float32) {
			_ = g.Search(q, K, ef)
		})
		hnQN := qpsGeneric(test, 12, qDur, func(q []float32) {
			_ = g.Search(q, K, ef)
		})
		hnRec := recallGraph(g, test, gt, K, ef)

		fmt.Printf("%-9d %-22s %-12.0f %-12.0f %-10s\n", n, "brute-force(текущее)", bfQ1, bfQN, "1.000")
		fmt.Printf("%-9d %-22s %-12.0f %-12.0f %-10.3f\n", n, "HNSW(Qdrant)", hnQ1, hnQN, hnRec)
	}
	fmt.Println()
	fmt.Println("Если HNSW кратно быстрее на 25-100k — индексировать дельту выгодно (теория верна).")
}

// TestArch_DeltaIndexed — end-to-end проверка индексированной дельты (Qdrant-модель):
// recall сохраняется, поиск по непустой дельте больше НЕ брутфорсный (быстрый).
func TestArch_DeltaIndexed(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	K := 10

	// Фаза A: только дельта (100k, без flush) — путь fast-path-2 (delta.Search).
	subA := train[:100_000]
	gtA := bruteGT(subA, test, K)
	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(8),
		DeltaMax:  300_000, // высоко → авто-flush не сработает, всё в дельте
		Fanout:    4,
		EfSearch:  128,
		UseSQ:     true,
	}
	lvs := NewLeveledVectorStore(cfg)
	for i, v := range subA {
		_ = lvs.Add(strconv.Itoa(i), v)
	}
	st := lvs.Stats()
	recA := recallStore(lvs, test, gtA, K)
	qA := qpsStore(lvs, test, K, 12, 3*time.Second)

	// Фаза B: flush дельты в сегмент + добавить 50k свежих в дельту (mixed).
	lvs.FlushDeltaSync()
	for i := 100_000; i < 150_000; i++ {
		_ = lvs.Add(strconv.Itoa(i), train[i])
	}
	subB := train[:150_000]
	gtB := bruteGT(subB, test, K)
	stB := lvs.Stats()
	recB := recallStore(lvs, test, gtB, K)
	qB := qpsStore(lvs, test, K, 12, 3*time.Second)

	fmt.Println()
	fmt.Println("DELTA INDEXED (end-to-end, Qdrant-модель):")
	fmt.Printf("  Фаза A (только дельта 100k, deltaLen=%d): recall@10=%.3f QPS_12=%.0f\n",
		st.DeltaLen, recA, qA)
	fmt.Printf("  Фаза B (сегменты + дельта 50k, deltaLen=%d, segs=%v): recall@10=%.3f QPS_12=%.0f\n",
		stB.DeltaLen, stB.SegmentsByLevel, recB, qB)
	fmt.Println("Ожидание: recall ~0.95+, QPS на дельте 100k кратно выше брутфорсных ~2014.")
	if recA < 0.85 {
		t.Errorf("Фаза A recall слишком низкий: %.3f", recA)
	}
	if recB < 0.85 {
		t.Errorf("Фаза B recall слишком низкий: %.3f", recB)
	}
}

// TestArch_DeltaFlushDebug — локализуем, откуда берётся много сегментов.
func TestArch_DeltaFlushDebug(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	r := rand.New(rand.NewSource(7))
	mk := func() []float32 {
		v := make([]float32, 128)
		for d := range v {
			v[d] = r.Float32()
		}
		return v
	}
	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(8),
		DeltaMax:  300_000,
		Fanout:    4,
		EfSearch:  128,
		UseSQ:     true,
	}
	lvs := NewLeveledVectorStore(cfg)
	for i := 0; i < 100_000; i++ {
		_ = lvs.Add(strconv.Itoa(i), mk())
	}
	s1 := lvs.Stats()
	t.Logf("после 100k Add: deltaLen=%d segs=%v", s1.DeltaLen, s1.SegmentsByLevel)
	lvs.FlushDeltaSync()
	s2 := lvs.Stats()
	t.Logf("после FlushDeltaSync: deltaLen=%d segs=%v", s2.DeltaLen, s2.SegmentsByLevel)
	for i := 100_000; i < 150_000; i++ {
		_ = lvs.Add(strconv.Itoa(i), mk())
	}
	s3 := lvs.Stats()
	t.Logf("после +50k Add: deltaLen=%d segs=%v", s3.DeltaLen, s3.SegmentsByLevel)
	time.Sleep(3 * time.Second)
	s4 := lvs.Stats()
	t.Logf("после sleep 3s: deltaLen=%d segs=%v", s4.DeltaLen, s4.SegmentsByLevel)
}

// topKIdx — до K индексов [0,n) с наименьшим dist(i), отсортированы по возрастанию.
// Bounded max-heap размера K → O(n log K), стоимость доминирует dist(), не сортировка.
func topKIdx(n, K int, dist func(i int) float32) []int {
	type vd struct {
		idx int
		d   float32
	}
	h := make([]vd, 0, K)
	up := func(i int) {
		for i > 0 {
			p := (i - 1) / 2
			if h[p].d >= h[i].d {
				break
			}
			h[p], h[i] = h[i], h[p]
			i = p
		}
	}
	down := func() {
		i := 0
		for {
			l, r, s := 2*i+1, 2*i+2, i
			if l < len(h) && h[l].d > h[s].d {
				s = l
			}
			if r < len(h) && h[r].d > h[s].d {
				s = r
			}
			if s == i {
				break
			}
			h[i], h[s] = h[s], h[i]
			i = s
		}
	}
	for i := 0; i < n; i++ {
		d := dist(i)
		if len(h) < K {
			h = append(h, vd{i, d})
			up(len(h) - 1)
		} else if d < h[0].d {
			h[0] = vd{i, d}
			down()
		}
	}
	sort.Slice(h, func(a, b int) bool { return h[a].d < h[b].d })
	ids := make([]int, len(h))
	for i := range h {
		ids[i] = h[i].idx
	}
	return ids
}

func bruteFloat32(vecs [][]float32, q []float32, K int) []int {
	return topKIdx(len(vecs), K, func(i int) float32 { return EuclideanDistance(q, vecs[i]) })
}

// pqSearch — PQ-ADC через LUT. vecs==nil → PQ-only top-K; vecs!=nil → top-efRerank
// по ADC, затем точный rerank по float32 → top-K (модель FrozenGraphPQ).
func pqSearch(pq *pqIndex, q []float32, K, efRerank int, vecs [][]float32) []int {
	M, dsub := pq.M, pq.dsub
	lut := make([]float32, M*256) // LUT[m*256+c] = ||q_sub_m - centroid[m][c]||²
	for m := 0; m < M; m++ {
		sub := q[m*dsub : (m+1)*dsub]
		base := m * 256
		for c := 0; c < 256; c++ {
			lut[base+c] = EuclideanDistance(sub, pq.centroids[m][c])
		}
	}
	adc := func(i int) float32 {
		code := pq.codes[i]
		var d float32
		for m := 0; m < M; m++ {
			d += lut[m*256+int(code[m])]
		}
		return d
	}
	if vecs == nil {
		return topKIdx(len(pq.codes), K, adc)
	}
	cand := topKIdx(len(pq.codes), efRerank, adc)
	type vd struct {
		idx int
		d   float32
	}
	rr := make([]vd, len(cand))
	for j, id := range cand {
		rr[j] = vd{id, EuclideanDistance(q, vecs[id])}
	}
	sort.Slice(rr, func(a, b int) bool { return rr[a].d < rr[b].d })
	if len(rr) > K {
		rr = rr[:K]
	}
	ids := make([]int, len(rr))
	for j := range rr {
		ids[j] = rr[j].idx
	}
	return ids
}

func recallIDs(found, gt []int, K int) float64 {
	gtset := make(map[int]struct{}, K)
	for _, id := range gt {
		gtset[id] = struct{}{}
	}
	hit := 0
	for _, id := range found {
		if _, ok := gtset[id]; ok {
			hit++
		}
	}
	return float64(hit) / float64(K)
}

// TestArch_PQ784 — валидация PQ на РЕАЛЬНЫХ 784-мерных данных (MNIST, ~ваши 768).
// Решает: даёт ли PQ память (M байт/вектор) при приемлемом recall, и быстра ли ADC.
func TestArch_PQ784(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	K := 10
	dim := len(train[0])
	qDur := 3 * time.Second
	t.Logf("MNIST-784: n=%d dim=%d queries=%d", len(train), dim, len(test))
	gt := bruteGT(train, test, K)
	f32q := qpsGeneric(test, 12, qDur, func(q []float32) { _ = bruteFloat32(train, q, K) })

	fmt.Println()
	fmt.Printf("PQ на РЕАЛЬНЫХ 784-мерных (MNIST, n=%d, K=%d, efRerank=100):\n", len(train), K)
	fmt.Printf("  float32 baseline: %d B/vec, recall=1.000, QPS_12=%.0f\n", dim*4, f32q)
	fmt.Printf("  SQ8 (для справки): %d B/vec\n", dim)
	fmt.Printf("%-10s %-9s %-12s %-15s %-12s %-10s\n",
		"PQ", "B/vec", "recall_only", "recall+rerank", "QPS_only_12", "сжатие")
	for _, M := range []int{16, 28, 49, 98} {
		pq := trainPQ(train, M)
		var sOnly, sRerank float64
		for qi, q := range test {
			sOnly += recallIDs(pqSearch(pq, q, K, 0, nil), gt[qi], K)
			sRerank += recallIDs(pqSearch(pq, q, K, 100, train), gt[qi], K)
		}
		recOnly := sOnly / float64(len(test))
		recRerank := sRerank / float64(len(test))
		qOnly := qpsGeneric(test, 12, qDur, func(q []float32) { _ = pqSearch(pq, q, K, 0, nil) })
		fmt.Printf("M=%-8d %-9d %-12.3f %-15.3f %-12.0f %-.0f×f32\n",
			M, M, recOnly, recRerank, qOnly, float64(dim*4)/float64(M))
	}
	fmt.Println("Решение: если PQ+rerank recall ≥0.9 при M B/vec (десятки байт) — PQ оправдан для 768.")
}

// TestArch_SQ784 — проверка включённого SQ8 для 768 на РЕАЛЬНЫХ 784-мерных (MNIST).
// float32 (hnswSegment) vs SQ8 (frozen): recall, QPS, память.
func TestArch_SQ784(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/mnist784.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	K := 10
	gt := bruteGT(train, test, K)

	run := func(useSQ bool, label string) {
		cfg := LeveledConfig{
			Distance:  EuclideanDistance,
			Allocator: tcmalloc.NewTCMallocStore(8),
			DeltaMax:  50_000,
			Fanout:    4,
			EfSearch:  128,
			UseSQ:     useSQ,
		}
		lvs := NewLeveledVectorStore(cfg)
		for i, v := range train {
			_ = lvs.Add(strconv.Itoa(i), v)
		}
		settleStart := time.Now()
		for time.Since(settleStart) < 200*time.Second {
			lvs.FlushDeltaSync()
			if !lvs.anyMergeInFlight() {
				time.Sleep(500 * time.Millisecond)
				if !lvs.anyMergeInFlight() {
					break
				}
			}
			time.Sleep(time.Second)
		}
		st := lvs.Stats()
		lvs.mu.RLock()
		var nSQ, nFr, nHN, total int
		for _, lvl := range lvs.levels {
			for _, s := range lvl {
				total++
				switch s.(type) {
				case *frozenSQSegment:
					nSQ++
				case *frozenSegment:
					nFr++
				case *hnswSegment:
					nHN++
				}
			}
		}
		lvs.mu.RUnlock()
		rec := recallStore(lvs, test, gt, K)
		q1 := qpsStore(lvs, test, K, 1, 3*time.Second)
		q12 := qpsStore(lvs, test, K, 12, 3*time.Second)
		fmt.Printf("  %-22s segs=%v (SQ=%d frozen=%d hnsw=%d) recall=%.3f QPS_1=%.0f QPS_12=%.0f mem=%.0fMB\n",
			label, st.SegmentsByLevel, nSQ, nFr, nHN, rec, q1, q12, float64(st.DataBytes)/1e6)
	}

	fmt.Println()
	fmt.Printf("SQ8@768 на MNIST-784 (n=%d, K=%d, ef=128):\n", len(train), K)
	run(false, "float32 (hnswSegment)")
	run(true, "SQ8 (frozen)")
	fmt.Println("Ожидание: SQ8 ~4× меньше памяти, recall ~0.96, QPS не хуже (на scale — лучше).")
}

// buildGraph строит сырой *Graph (uint64-соседи, без uint16-overflow).
func buildGraph(vecs [][]float32, efC int) *Graph {
	alloc := tcmalloc.NewTCMallocStore(1)
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = efC
	for _, v := range vecs {
		g.Insert(v)
	}
	return g
}

func recallGraph(g *Graph, queries [][]float32, gt [][]int, K, ef int) float64 {
	var sum float64
	for qi, q := range queries {
		res := g.Search(q, K, ef)
		gtset := make(map[int]struct{}, K)
		for _, id := range gt[qi] {
			gtset[id] = struct{}{}
		}
		hit := 0
		for _, r := range res {
			if _, ok := gtset[int(r.ID)]; ok {
				hit++
			}
		}
		sum += float64(hit) / float64(K)
	}
	return sum / float64(len(queries))
}

// TestArch_GraphQuality — здоров ли HNSW сам по себе при росте N?
// Сырой *Graph (uint64-соседи) → uint16-overflow исключён. Если recall падает
// с ростом N при фикс. ef — это дефект построения/навигации графа, НЕ фан-аут.
func TestArch_GraphQuality(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	K := 10
	fmt.Println()
	fmt.Println("HNSW QUALITY (сырой *Graph, efConstruction=200, recall@10):")
	fmt.Printf("%-9s %-10s %-10s %-10s %-10s\n", "N", "ef=64", "ef=128", "ef=256", "ef=512")
	for _, N := range []int{25_000, 50_000, 100_000, 200_000} {
		sub := train[:N]
		gt := bruteGT(sub, test, K)
		g := buildGraph(sub, 200)
		r64 := recallGraph(g, test, gt, K, 64)
		r128 := recallGraph(g, test, gt, K, 128)
		r256 := recallGraph(g, test, gt, K, 256)
		r512 := recallGraph(g, test, gt, K, 512)
		fmt.Printf("%-9d %-10.3f %-10.3f %-10.3f %-10.3f\n", N, r64, r128, r256, r512)
	}
	fmt.Println("Здоровый HNSW: recall растёт с ef к ~0.99 и слабо зависит от N.")
	fmt.Println("Если recall сатурируется НИЖЕ 0.95 или падает с N — дефект графа.")
}

// TestArch_FrozenOverflow — прямое доказательство порчи FrozenGraph при n>65535.
func TestArch_FrozenOverflow(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	K, ef := 10, 256
	fmt.Println()
	fmt.Println("UINT16 OVERFLOW (один FrozenGraph, ef=256, recall@10):")
	fmt.Printf("%-9s %-10s %-10s\n", "n", "recall", "статус")
	for _, n := range []int{60_000, 65_000, 70_000, 100_000} {
		sub := train[:n]
		gt := bruteGT(sub, test, K)
		segs := buildSegs(sub, 1, false)
		rec := recallAt(segs, test, gt, K, ef)
		status := "ok"
		if n > 65535 {
			status = "OVERFLOW (>65535)"
		}
		fmt.Printf("%-9d %-10.3f %-10s\n", n, rec, status)
	}
	fmt.Println("Cliff после 65535 = молчаливая порча графа uint16-соседями.")
	fmt.Println("ВЫВОД H3: убрать uint16 (→uint32) ОБЯЗАТЕЛЬНО до любого роста сегментов.")
}

// TestArch_CleanFanOut — чистый fan-out при N=60k (frozen для k=1 валиден, без overflow).
func TestArch_CleanFanOut(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	const N = 60_000 // < 65535 → один FrozenGraph корректен
	train = train[:N]
	K := 10
	const target = 0.95
	qDur := 3 * time.Second
	gt := bruteGT(train, test, K)
	efGrid := []int{16, 24, 32, 48, 64, 96, 128, 192, 256, 384, 512}

	fmt.Println()
	fmt.Printf("CLEAN FAN-OUT (N=%d, frozen валиден для всех k, recall≥%.2f):\n", N, target)
	fmt.Printf("%-8s %-8s %-9s %-12s %-14s\n", "segs", "ef*", "recall", "QPS_1thr", "QPS_seq(12)")
	for _, k := range []int{1, 2, 4, 8} {
		segs := buildSegs(train, k, false)
		bestEf, bestRec := efGrid[len(efGrid)-1], 0.0
		for _, ef := range efGrid {
			rec := recallAt(segs, test, gt, K, ef)
			if rec >= target {
				bestEf, bestRec = ef, rec
				break
			}
			bestEf, bestRec = ef, rec
		}
		q1 := qps(segs, test, K, bestEf, 1, qDur, searchSeq)
		qSeq := qps(segs, test, K, bestEf, 12, qDur, searchSeq)
		fmt.Printf("%-8d %-8d %-9.3f %-12.0f %-14.0f\n", k, bestEf, bestRec, q1, qSeq)
	}
	fmt.Println("Теперь k=1 — здоровый монолит. Сравнение QPS при равном recall = цена фан-аута.")
}

// TestArch_SQReal — SQ8 vs float32 на РЕАЛЬНОМ SIFT при n=65k (recall валиден,
// float32 уже задевает L3 ~33MB). Чистая проверка claim "SQ просаживает".
func TestArch_SQReal(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	const N = 65_000 // < 65535 → frozen валиден
	train = train[:N]
	K := 10
	qDur := 3 * time.Second
	gt := bruteGT(train, test, K)

	fmt.Println()
	fmt.Printf("H4 SQ8 vs float32 РЕАЛЬНЫЙ SIFT (n=%d, dim=128, float32≈33MB vs SQ8≈8MB):\n", N)
	fmt.Printf("%-10s %-9s %-9s %-12s %-12s %-9s %-9s\n",
		"method", "ef", "recall", "QPS_1thr", "QPS_12thr", "mem_MB", "vsF32")
	for _, ef := range []int{64, 128, 256} {
		fSegs := buildSegs(train, 1, false)
		fg := fSegs[0].(*FrozenGraph)
		fRec := recallAt(fSegs, test, gt, K, ef)
		fQ1 := qps(fSegs, test, K, ef, 1, qDur, searchSeq)
		fQN := qps(fSegs, test, K, ef, 12, qDur, searchSeq)
		fMem := float64(fg.MemoryBytes()) / 1e6

		sSegs := buildSegs(train, 1, true)
		sg := sSegs[0].(*FrozenGraphSQ)
		sRec := recallAt(sSegs, test, gt, K, ef)
		sQ1 := qps(sSegs, test, K, ef, 1, qDur, searchSeq)
		sQN := qps(sSegs, test, K, ef, 12, qDur, searchSeq)
		sMem := float64(sg.MemoryBytes()) / 1e6

		fmt.Printf("%-10s %-9d %-9.3f %-12.0f %-12.0f %-9.1f %-9s\n",
			"float32", ef, fRec, fQ1, fQN, fMem, "1.00x")
		fmt.Printf("%-10s %-9d %-9.3f %-12.0f %-12.0f %-9.1f %-9.2fx\n",
			"SQ8", ef, sRec, sQ1, sQN, sMem, sQN/fQN)
	}
	fmt.Println("Если SQ8 vsF32 < 1.0 даже на реальных данных при L3-spill — claim подтверждён.")
}

// =============================================================================
// H3: uint16 vs uint32 топология — доля памяти
// =============================================================================

func TestArch_TopologyMemory(t *testing.T) {
	train, _, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v", err)
	}
	const N = 50_000
	if len(train) > N {
		train = train[:N]
	}
	segs := buildSegs(train, 1, false)
	fg := segs[0].(*FrozenGraph)

	var neighEntries int
	for _, l := range fg.layers {
		neighEntries += len(l.neigh)
	}
	dataBytes := fg.n * fg.dim * 4
	topoU16 := neighEntries * 2
	topoU32 := neighEntries * 4
	offsBytes := 0
	for _, l := range fg.layers {
		offsBytes += len(l.offs) * 4
	}

	fmt.Println()
	fmt.Println("H3 TOPOLOGY MEMORY (float32 frozen, n=50k, dim=128):")
	fmt.Printf("  векторы (data):        %8.1f MB\n", float64(dataBytes)/1e6)
	fmt.Printf("  топология uint16:      %8.1f MB  (%.1f%% от data)\n",
		float64(topoU16)/1e6, 100*float64(topoU16)/float64(dataBytes))
	fmt.Printf("  топология uint32:      %8.1f MB  (%.1f%% от data)\n",
		float64(topoU32)/1e6, 100*float64(topoU32)/float64(dataBytes))
	fmt.Printf("  CSR offs (uint32):     %8.1f MB\n", float64(offsBytes)/1e6)
	fmt.Printf("  ЦЕНА uint32 vs uint16: +%.1f MB (+%.1f%% к памяти сегмента)\n",
		float64(topoU32-topoU16)/1e6,
		100*float64(topoU32-topoU16)/float64(dataBytes+topoU16+offsBytes))
	fmt.Println("  Вывод: если прирост памяти мал, потолок 65535 не стоит этой экономии.")
}

// =============================================================================
// H4: SQ8 vs float32 на разных dim — проверка claim "SQ просаживает при dim>256"
// =============================================================================

func synthClustered(n, dim, nClusters int, seed int64) [][]float32 {
	r := rand.New(rand.NewSource(seed))
	centers := make([][]float32, nClusters)
	for c := range centers {
		centers[c] = make([]float32, dim)
		for d := 0; d < dim; d++ {
			centers[c][d] = r.Float32()*2 - 1
		}
	}
	vecs := make([][]float32, n)
	for i := 0; i < n; i++ {
		c := centers[r.Intn(nClusters)]
		v := make([]float32, dim)
		for d := 0; d < dim; d++ {
			v[d] = c[d] + float32(r.NormFloat64())*0.1
		}
		vecs[i] = v
	}
	return vecs
}

func TestArch_SQHighDim(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	const n, nq = 30_000, 300
	K, ef, W := 10, 100, 12
	qDur := 3 * time.Second

	fmt.Println()
	fmt.Println("H4 SQ8 vs float32 по размерности (synth, n=30k, 1 сегмент):")
	fmt.Printf("%-6s %-16s %-10s %-12s %-12s %-10s %-9s\n",
		"dim", "method", "recall", "QPS_1thr", "QPS_12thr", "mem_MB", "vsF32")
	for _, dim := range []int{128, 256, 512, 768} {
		all := synthClustered(n+nq, dim, 200, int64(1000+dim))
		train, queries := all[:n], all[n:]
		gt := bruteGT(train, queries, K)

		// float32
		fSegs := buildSegs(train, 1, false)
		fg := fSegs[0].(*FrozenGraph)
		fRec := recallAt(fSegs, queries, gt, K, ef)
		fQ1 := qps(fSegs, queries, K, ef, 1, qDur, searchSeq)
		fQN := qps(fSegs, queries, K, ef, W, qDur, searchSeq)
		fMem := float64(fg.MemoryBytes()) / 1e6

		// SQ8
		sSegs := buildSegs(train, 1, true)
		sg := sSegs[0].(*FrozenGraphSQ)
		sRec := recallAt(sSegs, queries, gt, K, ef)
		sQ1 := qps(sSegs, queries, K, ef, 1, qDur, searchSeq)
		sQN := qps(sSegs, queries, K, ef, W, qDur, searchSeq)
		sMem := float64(sg.MemoryBytes()) / 1e6

		fmt.Printf("%-6d %-16s %-10.3f %-12.0f %-12.0f %-10.1f %-9s\n",
			dim, "float32", fRec, fQ1, fQN, fMem, "1.00x")
		fmt.Printf("%-6d %-16s %-10.3f %-12.0f %-12.0f %-10.1f %-9.2fx\n",
			dim, "SQ8", sRec, sQ1, sQN, sMem, sQN/fQN)
	}
	fmt.Println()
	fmt.Println("Чтение: vsF32 = QPS_SQ8 / QPS_float32 (12 потоков). <1.0 => SQ8 медленнее (claim верен).")
}
