package main

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/vector"
)

func main() {
	fmt.Println("====================================================")
	fmt.Println("🚀 STARTING PRODUCTION VECTOR STORAGE BENCHMARK 🚀")
	fmt.Println("====================================================")

	// Scenario 1: 100,000 vectors of 128 dimensions (Lightweight embeddings, e.g. nomic-embed-text-v1.5)
	runBenchmark(100000, 128, "L2 (Euclidean)")

	// Scenario 2: 50,000 vectors of 768 dimensions (Standard BERT/LLM embeddings)
	runBenchmark(50000, 768, "Cosine (Pre-Normalized)")
}

func runBenchmark(numVectors int, dim int, metricName string) {
	fmt.Printf("\n--- Scenario: %d vectors, %d dimensions, Metric: %s ---\n", numVectors, dim, metricName)

	// 1. Generate random vectors
	fmt.Println("⏳ Generating random vectors...")
	rng := rand.New(rand.NewSource(42))
	data := make([][]float32, numVectors)
	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := 0; j < dim; j++ {
			vec[j] = rng.Float32()*2 - 1
		}
		data[i] = vec
	}

	// 2. Build the vector store
	fmt.Println("⏳ Indexing vectors (building HNSW graph)...")
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	var store *vector.VectorStore
	if metricName == "Cosine (Pre-Normalized)" {
		store = vector.NewVectorStoreCosine()
	} else {
		store = vector.NewVectorStore(vector.EuclideanDistance)
	}

	startTime := time.Now()
	for i := 0; i < numVectors; i++ {
		key := fmt.Sprintf("vec_%d", i)
		err := store.Add(key, data[i])
		if err != nil {
			fmt.Printf("Error adding vector: %v\n", err)
			os.Exit(1)
		}
	}
	buildDuration := time.Since(startTime)

	runtime.GC() // Очищаем временный мусор от реаллокаций слайсов перед замером
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	memAllocated := float64(memAfter.TotalAlloc-memBefore.TotalAlloc) / (1024 * 1024)
	heapInUse := float64(memAfter.HeapInuse) / (1024 * 1024)
	liveObjects := float64(memAfter.Alloc) / (1024 * 1024)

	fmt.Printf("✅ Indexing Completed:\n")
	fmt.Printf("   - Total Index Time: %.2f seconds\n", buildDuration.Seconds())
	fmt.Printf("   - Indexing Speed:   %.2f vectors/sec\n", float64(numVectors)/buildDuration.Seconds())
	fmt.Printf("   - Total Allocated:   %.2f MB (cumulative memory allocated during build)\n", memAllocated)
	fmt.Printf("   - Heap In Use:       %.2f MB (virtual memory pages held by OS)\n", heapInUse)
	fmt.Printf("   - Live Objects:      %.2f MB (actual active memory size after GC)\n", liveObjects)

	// 3. Search Benchmarks (Single-threaded)
	fmt.Println("⏳ Benchmarking search (single-threaded, 1000 queries)...")
	queryCount := 1000
	queries := make([][]float32, queryCount)
	for i := 0; i < queryCount; i++ {
		q := make([]float32, dim)
		for j := 0; j < dim; j++ {
			q[j] = rng.Float32()*2 - 1
		}
		queries[i] = q
	}

	latencies := make([]time.Duration, queryCount)
	startTime = time.Now()
	for i := 0; i < queryCount; i++ {
		qStart := time.Now()
		_, _ = store.Search(queries[i], 10)
		latencies[i] = time.Since(qStart)
	}
	totalSearchTime := time.Since(startTime)

	// Calculate latency percentiles
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})
	p50 := latencies[queryCount*50/100]
	p90 := latencies[queryCount*90/100]
	p95 := latencies[queryCount*95/100]
	p99 := latencies[queryCount*99/100]

	fmt.Printf("✅ Single-Threaded Search (K=10):\n")
	fmt.Printf("   - Throughput: %.2f QPS\n", float64(queryCount)/totalSearchTime.Seconds())
	fmt.Printf("   - Latency Median (p50): %.2f ms\n", float64(p50.Microseconds())/1000.0)
	fmt.Printf("   - Latency p90:          %.2f ms\n", float64(p90.Microseconds())/1000.0)
	fmt.Printf("   - Latency p95:          %.2f ms\n", float64(p95.Microseconds())/1000.0)
	fmt.Printf("   - Latency p99:          %.2f ms\n", float64(p99.Microseconds())/1000.0)

	// 4. Search Benchmarks (Multi-threaded / Concurrent)
	numCPU := runtime.NumCPU()
	fmt.Printf("⏳ Benchmarking concurrent search (%d threads/workers, 5 seconds)...\n", numCPU)

	var searchCount uint64
	var stop uint32
	var wg sync.WaitGroup
	var mu sync.Mutex
	allLatencies := make([]time.Duration, 0, 100000)

	startBench := time.Now()
	for w := 0; w < numCPU; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			localRng := rand.New(rand.NewSource(int64(42 + workerID)))
			localLatencies := make([]time.Duration, 0, 5000)

			for atomic.LoadUint32(&stop) == 0 {
				q := make([]float32, dim)
				for j := 0; j < dim; j++ {
					q[j] = localRng.Float32()*2 - 1
				}

				qStart := time.Now()
				_, _ = store.Search(q, 10)
				duration := time.Since(qStart)

				localLatencies = append(localLatencies, duration)
				atomic.AddUint64(&searchCount, 1)
			}

			mu.Lock()
			allLatencies = append(allLatencies, localLatencies...)
			mu.Unlock()
		}(w)
	}

	time.Sleep(5 * time.Second)
	atomic.StoreUint32(&stop, 1)
	wg.Wait()
	actualDuration := time.Since(startBench)

	sort.Slice(allLatencies, func(i, j int) bool {
		return allLatencies[i] < allLatencies[j]
	})
	totalQueries := len(allLatencies)
	var cp50, cp90, cp95, cp99 time.Duration
	if totalQueries > 0 {
		cp50 = allLatencies[totalQueries*50/100]
		cp90 = allLatencies[totalQueries*90/100]
		cp95 = allLatencies[totalQueries*95/100]
		cp99 = allLatencies[totalQueries*99/100]
	}

	fmt.Printf("✅ Concurrent Search (K=10):\n")
	fmt.Printf("   - Total Queries Executed: %d\n", searchCount)
	fmt.Printf("   - Overall Throughput:     %.2f QPS\n", float64(searchCount)/actualDuration.Seconds())
	fmt.Printf("   - Concurrent p50 Latency:  %.2f ms\n", float64(cp50.Microseconds())/1000.0)
	fmt.Printf("   - Concurrent p90 Latency:  %.2f ms\n", float64(cp90.Microseconds())/1000.0)
	fmt.Printf("   - Concurrent p95 Latency:  %.2f ms\n", float64(cp95.Microseconds())/1000.0)
	fmt.Printf("   - Concurrent p99 Latency:  %.2f ms\n", float64(cp99.Microseconds())/1000.0)
}
