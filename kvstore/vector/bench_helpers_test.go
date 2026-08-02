package vector

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Общие хелперы замеров: расчёт ground truth перебором, recall с предикатом,
// прогон QPS.
//
// ⚠Лежат ВНЕ тега `datasets` намеренно. Пока они жили внутри файлов с замерами
// на внешних датасетах, тег уносил за собой и их — а ими пользуются обычные
// тесты пакета, которые с датасетами никак не связаны. Симптом был
// обманчивый: `undefined: filterGTByPred` в файле, который ничего про
// датасеты не знает.

// —— перенесено из filter_scale_test.go ——
// filterGTByPred — точный top-K по индексам среди векторов, где pred(i) (параллельно).
func filterGTByPred(vecs, queries [][]float32, K int, pred func(int) bool) [][]int {
	pass := make([]bool, len(vecs))
	for i := range vecs {
		pass[i] = pred(i)
	}
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

// —— перенесено из filter_scale_test.go ——
// recallStorePred — recall@K результатов движка (ключи=строковые id) против GT (индексы).
func recallStorePred(queries [][]float32, gt [][]int, K int, search func([]float32) []VSearchResult) float64 {
	var sum float64
	for qi, q := range queries {
		res := search(q)
		ids := make([]int, len(res))
		for i, r := range res {
			ids[i], _ = strconv.Atoi(r.Key)
		}
		sum += recallVsGT(ids, gt[qi], K)
	}
	return sum / float64(len(queries))
}

// —— перенесено из ivf_big_dim_test.go ——
func l3StatusStr(mb float64) string {
	if mb <= 12 {
		return "✓ fits"
	}
	return fmt.Sprintf("❌ %.1f× over", mb/12)
}

// —— перенесено из ivf_big_dim_test.go ——
// measureQPSBatch — вспомогательный для testQPSAtScale
func measureQPSBatch(searchFn func(q []float32), queries [][]float32, workers int, duration time.Duration) float64 {
	var count atomic.Int64
	done := make(chan struct{})

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			i := id
			for {
				select {
				case <-done:
					return
				default:
					searchFn(queries[i%len(queries)])
					count.Add(1)
					i++
				}
			}
		}(w)
	}

	time.Sleep(duration)
	close(done)
	wg.Wait()

	return float64(count.Load()) / duration.Seconds()
}

// —— перенесено из filter_profit_test.go ——
// topKFilteredIdx — линейный скан с top-K по индексам, где pass[i].
func topKFilteredIdx(vecs [][]float32, q []float32, K int, pass []bool) []int {
	type cand struct {
		id int
		d  float32
	}
	top := make([]cand, 0, K+1)
	for id, v := range vecs {
		if pass != nil && !pass[id] {
			continue
		}
		d := EuclideanDistance(q, v)
		if len(top) < K {
			top = append(top, cand{id, d})
			if len(top) == K {
				for i := 1; i < len(top); i++ {
					for j := i; j > 0 && top[j].d < top[j-1].d; j-- {
						top[j], top[j-1] = top[j-1], top[j]
					}
				}
			}
			continue
		}
		if d < top[K-1].d {
			pos := K - 1
			for pos > 0 && top[pos-1].d > d {
				top[pos] = top[pos-1]
				pos--
			}
			top[pos] = cand{id, d}
		}
	}
	// финальная сортировка (если top<K)
	for i := 1; i < len(top); i++ {
		for j := i; j > 0 && top[j].d < top[j-1].d; j-- {
			top[j], top[j-1] = top[j-1], top[j]
		}
	}
	ids := make([]int, len(top))
	for i := range top {
		ids[i] = top[i].id
	}
	return ids
}

// —— перенесено из tenant_qps_test.go ——
// runQPS гоняет fn по запросам в W потоков в течение dur, возвращает запросов/с.
func runQPS(W int, dur time.Duration, queries [][]float32, fn func([]float32)) float64 {
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
				fn(q)
				local++
			}
			atomic.AddInt64(&ops, int64(local))
		}()
	}
	wg.Wait()
	return float64(ops) / dur.Seconds()
}

// —— перенесено из filter_profit_test.go ——
func recallVsGT(foundIDs []int, gt []int, K int) float64 {
	gtset := make(map[int]struct{}, K)
	for _, id := range gt {
		gtset[id] = struct{}{}
	}
	hit := 0
	for _, id := range foundIDs {
		if _, ok := gtset[id]; ok {
			hit++
		}
	}
	return float64(hit) / float64(K)
}
