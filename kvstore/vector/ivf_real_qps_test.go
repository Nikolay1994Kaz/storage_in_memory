package vector

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// IVF REAL QPS BENCHMARK — измеряет реальный QPS с production FrozenGraphSQ.
//
// Использует НАСТОЯЩИЙ production код:
//   - k-means для кластеризации
//   - NewGraph + FreezeGraphSQ (наш реальный HNSW + SQ8)
//   - AVX2 distance functions
//
// Сравнивает:
//   1. Baseline: 1× FrozenGraphSQ на ВСЕХ 50k (как сейчас в production)
//   2. IVF: 16 кластеров по ~3k, nprobe=4 → 4× FrozenGraphSQ search
//
// Это даёт ЧЕСТНЫЙ QPS, потому что HNSW traversal = production code.
// =============================================================================

// ivfRealIndex — IVF с реальными production frozen сегментами внутри.
type ivfRealIndex struct {
	centroids [][]float32      // [nlist][dim]
	clusters  []*FrozenGraphSQ // production SQ8 frozen graphs!
	nprobe    int
	dim       int
}

func trainIVFReal(vecs [][]float32, nlist, M, efConstruction int) *ivfRealIndex {
	n := len(vecs)
	dim := len(vecs[0])
	_ = n

	// 1. K-means → centroids.
	centroids := kmeans(vecs, nlist, 20)

	// 2. Assign vectors to clusters.
	lists := make([][]int, nlist)
	for i, v := range vecs {
		bestC := 0
		bestD := EuclideanDistance(v, centroids[0])
		for c := 1; c < nlist; c++ {
			d := EuclideanDistance(v, centroids[c])
			if d < bestD {
				bestD = d
				bestC = c
			}
		}
		lists[bestC] = append(lists[bestC], i)
	}

	// 3. Для каждого кластера: build REAL production FrozenGraphSQ.
	allocator := tcmalloc.NewTCMallocStore(1)
	clusters := make([]*FrozenGraphSQ, nlist)
	for c := 0; c < nlist; c++ {
		if len(lists[c]) == 0 {
			continue
		}
		// Build HNSW graph.
		g := NewGraph(EuclideanDistance, allocator)
		g.EfConstruction = efConstruction
		if M > 0 {
			g.M = M
			g.M0 = 2 * M
			g.Ml = 1.0 / math.Log(float64(g.M))
			g.pruneBufItems = make([]item, 0, g.M0+1)
			g.pruneBufIDs = make([]uint64, 0, g.M0+1)
			g.insertBuf = make([]uint64, 0, g.M0+1)
		}
		keys := make(map[uint64]string, len(lists[c]))
		for _, vecIdx := range lists[c] {
			idx := g.Insert(vecs[vecIdx])
			keys[uint64(idx)] = fmt.Sprintf("v:%d", vecIdx)
		}
		// Freeze to SQ8 (production code!).
		fg := FreezeGraphSQ(g, MetricEuclidean, keys)
		clusters[c] = fg
	}

	return &ivfRealIndex{
		centroids: centroids,
		clusters:  clusters,
		nprobe:    4,
		dim:       dim,
	}
}

// searchTopNprobe находит nprobe ближайших кластеров.
func (idx *ivfRealIndex) searchTopNprobe(query []float32) []int {
	type cd struct {
		c    int
		dist float32
	}
	cds := make([]cd, len(idx.centroids))
	for c := range idx.centroids {
		cds[c] = cd{c, EuclideanDistance(query, idx.centroids[c])}
	}
	sort.Slice(cds, func(i, j int) bool { return cds[i].dist < cds[j].dist })
	nprobe := idx.nprobe
	if nprobe > len(cds) {
		nprobe = len(cds)
	}
	result := make([]int, nprobe)
	for i := 0; i < nprobe; i++ {
		result[i] = cds[i].c
	}
	return result
}

// search — real production search: nprobe clusters × FrozenGraphSQ.Search + merge.
func (idx *ivfRealIndex) search(query []float32, K, efSearch int) []FrozenResult {
	clusters := idx.searchTopNprobe(query)
	var allResults []FrozenResult
	for _, c := range clusters {
		if idx.clusters[c] == nil {
			continue
		}
		results := idx.clusters[c].Search(query, K, efSearch, nil, nil)
		allResults = append(allResults, results...)
	}
	// Sort + top-K.
	sort.Slice(allResults, func(i, j int) bool { return allResults[i].Dist < allResults[j].Dist })
	if len(allResults) > K {
		allResults = allResults[:K]
	}
	return allResults
}

// =============================================================================
// BENCHMARK
// =============================================================================

// TestIVF_Real_QPS сравнивает QPS: baseline (1 big graph) vs IVF (16 small graphs).
func TestIVF_Real_QPS(t *testing.T) {
	if testing.Short() {
		t.Skip("IVF real QPS benchmark is slow")
	}
	const K = 10
	const efSearch = 200

	// Load REAL SIFT data.
	vecs, queries, err := loadSIFTRaw("/tmp/sift_raw.bin")
	if err != nil {
		t.Skipf("SIFT raw not found: %v (run convert_sift.py)", err)
	}
	n := len(vecs)
	dim := len(vecs[0])
	t.Logf("Loaded SIFT: %d vectors, dim=%d, %d queries", n, dim, len(queries))

	// Ground truth.
	gt := make([][]int, len(queries))
	for i, q := range queries {
		gt[i] = bruteForceTopKIdx(vecs, q, K)
	}

	// ── Baseline: 1× FrozenGraphSQ на ВСЕХ 50k ──
	t.Log("Building baseline: 1× FrozenGraphSQ on all 50k vectors...")
	allocator := tcmalloc.NewTCMallocStore(1)
	g := NewGraph(EuclideanDistance, allocator)
	g.EfConstruction = 400
	g.M = 32
	g.M0 = 64
	g.Ml = 1.0 / math.Log(float64(g.M))
	g.pruneBufItems = make([]item, 0, g.M0+1)
	g.pruneBufIDs = make([]uint64, 0, g.M0+1)
	g.insertBuf = make([]uint64, 0, g.M0+1)
	keys := make(map[uint64]string, n)
	for i, v := range vecs {
		idx := g.Insert(v)
		keys[uint64(idx)] = fmt.Sprintf("v:%d", i)
	}
	baselineFG := FreezeGraphSQ(g, MetricEuclidean, keys)
	t.Logf("  baseline: n=%d mem=%.1fMB", baselineFG.n, float64(baselineFG.MemoryBytes())/1e6)

	// Measure baseline recall + QPS.
	// Сравниваем по строковым ключам, так как HNSW перемешивает внутренние ID.
	baselineRecall := 0.0
	for i, q := range queries {
		results := baselineFG.Search(q, K, efSearch, nil, nil)
		resSet := make(map[string]bool, len(results))
		for _, r := range results {
			resSet[r.Key] = true
		}
		gtSet := make(map[string]bool, K)
		for _, gtIdx := range gt[i] {
			gtSet[fmt.Sprintf("v:%d", gtIdx)] = true
		}
		hits := 0
		for k := range resSet {
			if gtSet[k] {
				hits++
			}
		}
		baselineRecall += float64(hits) / float64(K)
	}
	baselineRecall /= float64(len(queries)) * 100

	baselineQPS := measureQPS(func(q []float32) {
		baselineFG.Search(q, K, efSearch, nil, nil)
	}, queries, 12, 5*time.Second)
	t.Logf("  BASELINE: recall=%.1f%% QPS=%.0f", baselineRecall, baselineQPS)

	// ── IVF: 16 clusters × FrozenGraphSQ ──
	for _, nlist := range []int{4, 16} {
		t.Logf("Building IVF: nlist=%d...", nlist)
		ivf := trainIVFReal(vecs, nlist, 32, 400)

		// Cluster stats.
		totalMem := 0
		for _, c := range ivf.clusters {
			if c != nil {
				totalMem += c.MemoryBytes()
			}
		}
		t.Logf("  IVF nlist=%d: total cluster mem=%.1fMB (avg %.1fMB/cluster)",
			nlist, float64(totalMem)/1e6, float64(totalMem)/float64(nlist)/1e6)

		for _, nprobe := range []int{2, 4} {
			if nprobe > nlist {
				continue
			}
			ivf.nprobe = nprobe

			// Recall.
			ivfRecall := 0.0
			for i, q := range queries {
				results := ivf.search(q, K, efSearch)
				resSet := make(map[string]bool, len(results))
				for _, r := range results {
					resSet[r.Key] = true
				}
				gtSet := make(map[string]bool, K)
				for _, gtIdx := range gt[i] {
					gtSet[fmt.Sprintf("v:%d", gtIdx)] = true
				}
				hits := 0
				for k := range resSet {
					if gtSet[k] {
						hits++
					}
				}
				ivfRecall += float64(hits) / float64(K)
			}
			ivfRecall /= float64(len(queries)) * 100

			// QPS.
			ivfQPS := measureQPS(func(q []float32) {
				ivf.search(q, K, efSearch)
			}, queries, 12, 5*time.Second)

			t.Logf("  IVF nlist=%d nprobe=%d: recall=%.1f%% QPS=%.0f (%.1f× vs baseline)",
				nlist, nprobe, ivfRecall, ivfQPS, ivfQPS/baselineQPS)
		}
	}
}

// roundDist округляет дистанцию для сравнения (избегает float32 precision issues).
func roundDist(d float32) float32 {
	return float32(int(d*1000)) / 1000
}

// measureQPS запускает search в N воркерах T секунд, возвращает QPS.
func measureQPS(searchFn func(q []float32), queries [][]float32, workers int, duration time.Duration) float64 {
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
