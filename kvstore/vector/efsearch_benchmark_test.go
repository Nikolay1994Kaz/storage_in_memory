package vector

import (
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestEfSearchTradeoff измеряет реальный trade-off efSearch → QPS vs Recall.
// Тестирует на 20K и 50K векторах dim=768 (как ESM-2 embeddings).
//
// Запуск: go test -v -run=TestEfSearchTradeoff ./vector/ -timeout 30m
func TestEfSearchTradeoff(t *testing.T) {
	if testing.Short() {
		t.Skip("тяжёлый бенчмарк; запуск без -short")
	}
	const dim = 768

	sizes := []int{20_000, 50_000}
	efValues := []int{10, 20, 50, 100, 200, 500}
	K := 10
	numQueries := 200
	numRecallQueries := 100

	fmt.Println(strings.Repeat("=", 90))
	fmt.Println("БЕНЧМАРК: efSearch → QPS vs Recall@10 Trade-off")
	fmt.Printf("  CPU: %d cores | dim=%d | K=%d\n", runtime.NumCPU(), dim, K)
	fmt.Println(strings.Repeat("=", 90))

	rng := rand.New(rand.NewSource(42))

	for _, N := range sizes {
		fmt.Printf("\n%s\n", strings.Repeat("─", 90))
		fmt.Printf("  N = %d vectors, dim = %d\n", N, dim)
		fmt.Printf("%s\n", strings.Repeat("─", 90))

		// Генерируем вектора (кластеры для реалистичности)
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

		// Строим HNSW граф
		allocator := tcmalloc.NewTCMallocStore(4)
		vs := NewVectorStoreCosine(allocator)

		fmt.Printf("\n  Building HNSW graph...\n")
		startBuild := time.Now()
		for i, vec := range vectors {
			key := fmt.Sprintf("v:%d", i)
			if err := vs.Add(key, vec); err != nil {
				t.Fatalf("Add error: %v", err)
			}
		}
		buildDur := time.Since(startBuild)
		fmt.Printf("  Build time: %v (%.0f vec/sec)\n", buildDur, float64(N)/buildDur.Seconds())

		// Генерируем queries
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
		// Brute-force ground truth для recall
		// ═══════════════════════════════════════════════════════════════
		fmt.Printf("\n  Computing brute-force ground truth for %d queries...\n", numRecallQueries)
		type distItem struct {
			key  string
			dist float32
		}
		groundTruth := make([][]string, numRecallQueries)
		for qi := 0; qi < numRecallQueries; qi++ {
			query := queries[qi]
			dists := make([]distItem, N)
			for i := 0; i < N; i++ {
				var dot float32
				for d := 0; d < dim; d++ {
					dot += query[d] * vectors[i][d]
				}
				dists[i] = distItem{key: fmt.Sprintf("v:%d", i), dist: 1 - dot}
			}
			sort.Slice(dists, func(a, b int) bool {
				return dists[a].dist < dists[b].dist
			})
			topK := make([]string, K)
			for i := 0; i < K; i++ {
				topK[i] = dists[i].key
			}
			groundTruth[qi] = topK
		}

		// ═══════════════════════════════════════════════════════════════
		// Тестируем каждый efSearch
		// ═══════════════════════════════════════════════════════════════
		fmt.Printf("\n  %8s │ %10s │ %10s │ %10s │ %12s │ %10s\n",
			"efSearch", "Recall@10", "QPS(1core)", "QPS(para)", "Latency(1)", "Latency(P)")
		fmt.Printf("  %s\n", strings.Repeat("─", 80))

		for _, ef := range efValues {
			// ── Recall ──
			totalRecall := 0.0
			for qi := 0; qi < numRecallQueries; qi++ {
				results := vs.graph.Search(queries[qi], K, ef)
				resultKeys := make(map[string]bool)
				for _, r := range results {
					key := vs.keys[r.ID]
					resultKeys[key] = true
				}
				overlap := 0
				for _, gtKey := range groundTruth[qi] {
					if resultKeys[gtKey] {
						overlap++
					}
				}
				totalRecall += float64(overlap) / float64(K)
			}
			avgRecall := totalRecall / float64(numRecallQueries)

			// ── Single-thread QPS ──
			runtime.GC()
			startSingle := time.Now()
			for qi := 0; qi < numQueries; qi++ {
				vs.graph.Search(queries[qi], K, ef)
			}
			singleDur := time.Since(startSingle)
			singleQPS := float64(numQueries) / singleDur.Seconds()
			singleLatency := float64(singleDur.Microseconds()) / float64(numQueries) / 1000.0

			// ── Parallel QPS ──
			concurrency := runtime.NumCPU()
			runtime.GC()
			var wg sync.WaitGroup
			perThread := numQueries / concurrency

			startPar := time.Now()
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
			parDur := time.Since(startPar)
			parQPS := float64(numQueries) / parDur.Seconds()
			parLatency := float64(parDur.Microseconds()) / float64(numQueries) / 1000.0

			fmt.Printf("  %8d │ %9.1f%% │ %10.0f │ %10.0f │ %9.3f ms │ %7.3f ms\n",
				ef, avgRecall*100, singleQPS, parQPS, singleLatency, parLatency)
		}

		// ═══════════════════════════════════════════════════════════════
		// Вывод: текущее значение efSearch = K*10
		// ═══════════════════════════════════════════════════════════════
		currentEf := K * 10
		if currentEf < 100 {
			currentEf = 100
		}
		fmt.Printf("\n  📌 Текущее значение в store.go: efSearch = max(K*10, 100) = %d\n", currentEf)
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 90))
	fmt.Println("КОНЕЦ БЕНЧМАРКА")
	fmt.Println(strings.Repeat("=", 90))
}
