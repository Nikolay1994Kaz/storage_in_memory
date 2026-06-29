package vector

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// Замер выигрыша QPS от тенант-раскладки (Вещь 1) — сквозной прод-путь.
//
// Вопрос: что РЕАЛЬНО дала тенант-aware раскладка + роутинг SearchTenant против
// наивного фильтрованного поиска, который сделал бы НЕ-тенант-aware движок?
//
//   baseline = SearchFiltered(q, K, tenantPred): полный HNSW-обход сегмента с
//              тенант-предикатом + ×4 расширение ef (как в search() при filterFn).
//              Это ровно то, что делает движок без знания о раскладке.
//   prod     = SearchTenant(q, K, tenant): роутинг через TenantCatalog →
//              blocked-brute по непрерывному диапазону (малый тенант) или
//              graph-фильтр (крупный). Это путь, который мы построили.
//
// Меряем многопоточный QPS (12 потоков — наша эталонная нагрузка) и recall обоих
// на ОДНОЙ геометрии, чтобы сравнение было iso-recall. Данные — РЕАЛЬНЫЙ SIFT-128
// (случайные врут), один консолидированный сегмент (фрагментация ломает brute).
//
//   go test ./kvstore/vector -run TestTenant_SearchTenantQPSGain -v -timeout 1200s
// =============================================================================

func TestTenant_SearchTenantQPSGain(t *testing.T) {
	if testing.Short() {
		t.Skip("long: строит 200k HNSW + многопоточный QPS sweep")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("нет данных: %v (python3 convert_sift200k.py)", err)
	}

	const (
		dim = 128
		K   = 10
		ef  = 128
		W   = 12              // эталонная многопоточная нагрузка
		dur = 3 * time.Second // на режим/тенант
	)

	// Целевые тенанты разных размеров: ниже порога 16384 (brute) и выше (graph),
	// чтобы увидеть оба регима роутинга. Остаток — фоновый тенант 0.
	blockSizes := []int{1000, 5000, 16000, 50000}
	totalTargets := 0
	for _, b := range blockSizes {
		totalTargets += b
	}
	if totalTargets >= len(train) {
		t.Fatalf("сумма блоков %d ≥ корпуса %d", totalTargets, len(train))
	}

	// Раскладка реальными SIFT-векторами; tenant id 1..len — targets, 0 — фон.
	vecsByTenant := make(map[uint64][][]float32)
	type kv struct {
		key string
		vec []float32
	}
	all := make([]kv, 0, len(train))
	off := 0
	for i, b := range blockSizes {
		tid := uint64(i + 1)
		for j := 0; j < b; j++ {
			v := train[off]
			off++
			vecsByTenant[tid] = append(vecsByTenant[tid], v)
			all = append(all, kv{strconv.FormatUint(tid, 10) + ":" + strconv.Itoa(j), v})
		}
	}
	for ; off < len(train); off++ {
		j := len(vecsByTenant[0])
		vecsByTenant[0] = append(vecsByTenant[0], train[off])
		all = append(all, kv{"0:" + strconv.Itoa(j), train[off]})
	}

	// Один консолидированный frozen-сегмент (как в калибровке): без авто-flush.
	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(8),
		DeltaMax:       len(train) + 1,
		Fanout:         4,
		EfConstruction: arEfConstruction,
		EfSearch:       ef,
		TenantOf:       tenantOfKey,
		BruteThreshold: DefaultBruteThreshold, // боевой порог — то, что валидируем
	}
	lvs := NewLeveledVectorStore(cfg)
	rng := rand.New(rand.NewSource(20260629))
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	tBuild := time.Now()
	for _, e := range all {
		if err := lvs.Add(e.key, e.vec); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	settle(lvs)
	buildDur := time.Since(tBuild)

	st := lvs.Stats()
	nseg := 0
	for _, n := range st.SegmentsByLevel {
		nseg += n
	}
	if nseg != 1 {
		t.Logf("ВНИМАНИЕ: сегментов %d, не 1 — выигрыш brute занижен фрагментацией (segs=%v)",
			nseg, st.SegmentsByLevel)
	}

	nQ := min(200, len(test))
	queries := test[:nQ]

	fmt.Println()
	fmt.Printf("SEARCHTENANT QPS GAIN — РЕАЛЬНЫЙ SIFT-128, N=%d, segs=%d, ef=%d, K=%d, W=%d, build=%.1fs\n",
		st.TotalVectors, nseg, ef, K, W, buildDur.Seconds())
	fmt.Printf("BruteThreshold=%d  (baseline=SearchFiltered full-graph+pred, prod=SearchTenant routed)\n",
		cfg.BruteThreshold)
	fmt.Printf("%-9s %-7s %-12s %-12s %-9s %-10s %-10s\n",
		"tenant_B", "route", "base_QPS", "tenant_QPS", "speedup", "base_rec", "tenant_rec")

	for i := range blockSizes {
		tid := uint64(i + 1)
		B := blockSizes[i]
		route := "brute"
		if B > cfg.BruteThreshold {
			route = "graph"
		}
		tenantPred := func(key string) bool { return tenantOfKey(key) == tid }

		// Фильтрованный GT: точный top-K среди векторов тенанта (ключи "tid:localid").
		pref := strconv.FormatUint(tid, 10) + ":"
		gtVecs := vecsByTenant[tid]
		gtKeys := make([]map[string]struct{}, len(queries))
		for qi, q := range queries {
			top := make([]FrozenResult, 0, K)
			for id, v := range gtVecs {
				top = insertTopK(top, K, strconv.Itoa(id), EuclideanDistance(q, v))
			}
			m := make(map[string]struct{}, len(top))
			for _, r := range top {
				m[pref+r.Key] = struct{}{}
			}
			gtKeys[qi] = m
		}
		recallOf := func(keys []string, qi int) float64 {
			if len(gtKeys[qi]) == 0 {
				return 0
			}
			hit := 0
			for _, k := range keys {
				if _, ok := gtKeys[qi][k]; ok {
					hit++
				}
			}
			return float64(hit) / float64(len(gtKeys[qi]))
		}

		// recall (untimed) — отдельным проходом.
		var baseRec, tenRec float64
		for qi, q := range queries {
			br, _ := lvs.SearchFiltered(q, K, tenantPred, nil)
			tr, _ := lvs.SearchTenant(q, K, tid, nil)
			baseRec += recallOf(keysOf(br), qi)
			tenRec += recallOf(keysOf(tr), qi)
		}
		baseRec /= float64(len(queries))
		tenRec /= float64(len(queries))

		baseQPS := runQPS(W, dur, queries, func(q []float32) {
			_, _ = lvs.SearchFiltered(q, K, tenantPred, nil)
		})
		tenQPS := runQPS(W, dur, queries, func(q []float32) {
			_, _ = lvs.SearchTenant(q, K, tid, nil)
		})

		fmt.Printf("%-9d %-7s %-12.0f %-12.0f %-9.2f %-10.3f %-10.3f\n",
			B, route, baseQPS, tenQPS, tenQPS/baseQPS, baseRec, tenRec)
	}
	fmt.Println("---")
	fmt.Println("Выигрыш = tenant_QPS/base_QPS при tenant_rec ≥ base_rec. brute-тенанты должны")
	fmt.Println("показать кратный отрыв (рычаг #7); graph-тенанты — паритет (тот же путь).")
}

func keysOf(res []VSearchResult) []string {
	ks := make([]string, len(res))
	for i, r := range res {
		ks[i] = r.Key
	}
	return ks
}

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
