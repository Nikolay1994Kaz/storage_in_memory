package vector

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestIVF_QPS_Scale сравнивает QPS на разных размерах и dim.
// Симулирует нагрузку: что будет с QPS при росте базы.
func TestIVF_QPS_Scale(t *testing.T) {
	if testing.Short() {
		t.Skip("QPS scale test is slow")
	}

	alloc := tcmalloc.NewTCMallocStore(1)

	// Тестируем на РЕАЛЬНОМ SIFT-128 (у нас есть HDF5 конвертированный),
	// а также симулируем 768 через padding (для замера memory, recall будет другой).
	siftVecs, siftQueries, err := loadSIFTRaw("/tmp/sift_raw.bin")
	if err != nil {
		t.Skipf("SIFT raw not found: %v", err)
	}

	// Полные 50k SIFT.
	t.Run("dim128_50k", func(t *testing.T) {
		testQPSAtScale(t, alloc, siftVecs, siftQueries, "SIFT-128 50k")
	})

	// Симуляция dim=768: просто добавляем нули к SIFT.
	// (Recall будет некорректен, но QPS и memory — честные, т.к. умножение на 6).
	t.Run("dim768_simulated", func(t *testing.T) {
		paddedVecs := make([][]float32, len(siftVecs))
		for i, v := range siftVecs {
			paddedVecs[i] = make([]float32, 768)
			copy(paddedVecs[i], v)
			// остальные 640 dim = 0
		}
		paddedQueries := make([][]float32, len(siftQueries))
		for i, q := range siftQueries {
			paddedQueries[i] = make([]float32, 768)
			copy(paddedQueries[i], q)
		}
		testQPSAtScale(t, alloc, paddedVecs, paddedQueries, "dim768 50k (simulated)")
	})
}

func testQPSAtScale(t *testing.T, alloc *tcmalloc.TCMallocStore, vecs, queries [][]float32, name string) {
	const K = 10
	const efSearch = 200
	dim := len(vecs[0])
	n := len(vecs)

	// ── Baseline: один большой frozen graph ──
	t.Logf("[%s] Building baseline 1× FrozenGraphSQ...", name)
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = 400
	g.M = 32
	g.M0 = 64
	g.Ml = 1.0 / math.Log(float64(g.M))
	g.pruneBufItems = make([]item, 0, g.M0+1)
	g.pruneBufIDs = make([]uint64, 0, g.M0+1)
	g.insertBuf = make([]uint64, 0, g.M0+1)

	for _, v := range vecs {
		g.Insert(v)
	}
	baselineFG := FreezeGraphSQ(g, EuclideanDistance, nil)
	baselineMem := float64(baselineFG.MemoryBytes()) / 1e6

	baselineQPS := measureQPSBatch(func(q []float32) {
		baselineFG.Search(q, K, efSearch, nil, nil)
	}, queries, 12, 3*time.Second)
	t.Logf("[%s] BASELINE: mem=%.1fMB QPS=%.0f (L3 %s)", name, baselineMem, baselineQPS, l3StatusStr(baselineMem))

	// ── IVF: 16 кластеров ──
	t.Logf("[%s] Building IVF 16 clusters...", name)
	ivf := trainIVFReal(vecs, 16, 32, 400)
	ivf.nprobe = 4

	totalIVFMem := 0
	for _, c := range ivf.clusters {
		if c != nil {
			totalIVFMem += c.MemoryBytes()
		}
	}
	ivfMem := float64(totalIVFMem) / 1e6

	ivfQPS := measureQPSBatch(func(q []float32) {
		clusters := ivf.searchTopNprobe(q)
		var allResults []FrozenResult
		for _, c := range clusters {
			if ivf.clusters[c] == nil {
				continue
			}
			results := ivf.clusters[c].Search(q, K, efSearch, nil, nil)
			allResults = append(allResults, results...)
		}
		sort.Slice(allResults, func(i, j int) bool { return allResults[i].Dist < allResults[j].Dist })
	}, queries, 12, 3*time.Second)

	t.Logf("[%s] IVF(16,nprobe=4): mem=%.1fMB QPS=%.0f (%.2f× vs baseline) (L3 %s)",
		name, ivfMem, ivfQPS, ivfQPS/baselineQPS, l3StatusStr(ivfMem/4))

	_ = n
	_ = dim
}

func l3StatusStr(mb float64) string {
	if mb <= 12 {
		return "✓ fits"
	}
	return fmt.Sprintf("❌ %.1f× over", mb/12)
}

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

// randFloatVecs генерирует случайные векторы (для тестов где нет SIFT).
func randFloatVecs(n, dim int, seed int64) [][]float32 {
	rng := rand.New(rand.NewSource(seed))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = make([]float32, dim)
		for d := range vecs[i] {
			vecs[i][d] = rng.Float32() * 100
		}
	}
	return vecs
}
