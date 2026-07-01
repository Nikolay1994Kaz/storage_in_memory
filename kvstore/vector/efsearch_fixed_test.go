package vector

import (
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestEfSearchRecallFixed проверяет recall с ПРАВИЛЬНОЙ distance function
// (DotProductDistance — та же что в HNSW, включая SIMD).
func TestEfSearchRecallFixed(t *testing.T) {
	if testing.Short() {
		t.Skip("тяжёлый бенчмарк; запуск без -short")
	}
	const dim = 768

	sizes := []int{20_000}
	efValues := []int{10, 50, 100, 200, 500, 1000}
	K := 10
	numQueries := 200

	fmt.Println(strings.Repeat("=", 90))
	fmt.Println("БЕНЧМАРК: efSearch (с корректной distance function)")
	fmt.Println(strings.Repeat("=", 90))

	rng := rand.New(rand.NewSource(42))

	for _, N := range sizes {
		// Генерируем кластерные данные
		numClusters := 50
		centroids := make([][]float32, numClusters)
		for i := range centroids {
			c := make([]float32, dim)
			for j := range c {
				c[j] = float32(rng.NormFloat64())
			}
			Normalize(c)
			centroids[i] = c
		}

		vectors := make([][]float32, N)
		for i := range vectors {
			cluster := rng.Intn(numClusters)
			v := make([]float32, dim)
			for j := range v {
				v[j] = centroids[cluster][j] + float32(rng.NormFloat64()*0.05)
			}
			Normalize(v)
			vectors[i] = v
		}

		// Строим граф
		allocator := tcmalloc.NewTCMallocStore(4)
		vs := NewVectorStoreCosine(allocator)

		fmt.Printf("\n  Building HNSW (%d vectors, dim=%d, M=%d, efC=%d)...\n",
			N, dim, 16, 200)
		for i, vec := range vectors {
			vs.Add(fmt.Sprintf("v:%d", i), vec)
		}

		// Queries
		queries := make([][]float32, numQueries)
		for i := range queries {
			cluster := rng.Intn(numClusters)
			q := make([]float32, dim)
			for j := range q {
				q[j] = centroids[cluster][j] + float32(rng.NormFloat64()*0.05)
			}
			Normalize(q)
			queries[i] = q
		}

		// ═══════════════════════════════════════════════════════════════
		// Brute-force с DotProductDistance (та же SIMD function!)
		// ═══════════════════════════════════════════════════════════════
		fmt.Printf("  Computing ground truth with DotProductDistance (SIMD)...\n")

		type idDist struct {
			id   uint64
			dist float32
		}

		groundTruth := make([]map[uint64]bool, numQueries)
		for qi := 0; qi < numQueries; qi++ {
			dists := make([]idDist, N)
			for i := 0; i < N; i++ {
				dists[i] = idDist{
					id:   uint64(i),
					dist: DotProductDistance(queries[qi], vectors[i]),
				}
			}
			// Partial selection sort for top-K (correct, no float issues)
			for i := 0; i < K; i++ {
				minIdx := i
				for j := i + 1; j < N; j++ {
					if dists[j].dist < dists[minIdx].dist {
						minIdx = j
					}
				}
				dists[i], dists[minIdx] = dists[minIdx], dists[i]
			}
			gt := make(map[uint64]bool)
			for i := 0; i < K; i++ {
				gt[dists[i].id] = true
			}
			groundTruth[qi] = gt
		}

		// ═══════════════════════════════════════════════════════════════
		// Тест каждого efSearch
		// ═══════════════════════════════════════════════════════════════
		fmt.Printf("\n  %8s │ %10s │ %10s │ %10s │ %10s\n",
			"efSearch", "Recall@10", "QPS(1core)", "QPS(para)", "Lat(para)")
		fmt.Printf("  %s\n", strings.Repeat("─", 65))

		for _, ef := range efValues {
			// Recall
			totalRecall := 0.0
			for qi := 0; qi < numQueries; qi++ {
				results := vs.graph.Search(queries[qi], K, ef)
				overlap := 0
				for _, r := range results {
					if groundTruth[qi][r.ID] {
						overlap++
					}
				}
				totalRecall += float64(overlap) / float64(K)
			}
			avgRecall := totalRecall / float64(numQueries)

			// Single-thread QPS
			runtime.GC()
			start := time.Now()
			for qi := 0; qi < numQueries; qi++ {
				vs.graph.Search(queries[qi], K, ef)
			}
			singleDur := time.Since(start)
			singleQPS := float64(numQueries) / singleDur.Seconds()

			// Parallel QPS
			runtime.GC()
			var wg sync.WaitGroup
			concurrency := runtime.NumCPU()
			perThread := numQueries / concurrency

			startP := time.Now()
			for tid := 0; tid < concurrency; tid++ {
				wg.Add(1)
				go func(t int) {
					defer wg.Done()
					for qi := t * perThread; qi < (t+1)*perThread; qi++ {
						vs.graph.Search(queries[qi%numQueries], K, ef)
					}
				}(tid)
			}
			wg.Wait()
			parDur := time.Since(startP)
			parQPS := float64(numQueries) / parDur.Seconds()
			parLat := float64(parDur.Microseconds()) / float64(numQueries) / 1000.0

			fmt.Printf("  %8d │ %9.1f%% │ %10.0f │ %10.0f │ %7.3f ms\n",
				ef, avgRecall*100, singleQPS, parQPS, parLat)
		}
	}

	fmt.Println(strings.Repeat("=", 90))
}
