package vector

import (
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// ============================================================================
// Product Quantization — минимальная реализация для бенчмарка
// ============================================================================

// PQIndex — Product Quantization индекс.
// Разбивает вектор dim=D на M подвекторов по dsub=D/M элементов.
// Каждый подвектор квантуется в 1 байт (256 центроидов через k-means).
type PQIndex struct {
	dim       int       // исходная размерность (768)
	m         int       // кол-во подвекторов (96)
	dsub      int       // размер подвектора (8)
	centroids []float32 // [m * 256 * dsub] — все центроиды
	codes     []byte    // [n * m] — сжатые коды всех векторов
	n         int       // кол-во закодированных векторов
}

// NewPQIndex создаёт PQ с m подвекторами.
func NewPQIndex(dim, m int) *PQIndex {
	if dim%m != 0 {
		panic("dim must be divisible by m")
	}
	return &PQIndex{
		dim:  dim,
		m:    m,
		dsub: dim / m,
	}
}

// Train обучает центроиды k-means на выборке векторов.
// Simplified: используем random sample как центроиды (в продакшене — полный k-means).
func (pq *PQIndex) Train(vectors [][]float32) {
	ksub := 256 // 256 центроидов на подвектор
	pq.centroids = make([]float32, pq.m*ksub*pq.dsub)

	rng := rand.New(rand.NewSource(42))

	for sub := 0; sub < pq.m; sub++ {
		// Простой k-means (5 итераций)
		offset := sub * pq.dsub

		// Инициализация: случайные вектора как центроиды
		for k := 0; k < ksub; k++ {
			vi := rng.Intn(len(vectors))
			centroidBase := (sub*ksub + k) * pq.dsub
			copy(pq.centroids[centroidBase:centroidBase+pq.dsub],
				vectors[vi][offset:offset+pq.dsub])
		}

		// K-means итерации
		assignments := make([]int, len(vectors))
		sums := make([]float32, ksub*pq.dsub)
		counts := make([]int, ksub)

		for iter := 0; iter < 8; iter++ {
			// Assign
			for i, vec := range vectors {
				subvec := vec[offset : offset+pq.dsub]
				bestK := 0
				bestDist := float32(math.MaxFloat32)
				for k := 0; k < ksub; k++ {
					centroidBase := (sub*ksub + k) * pq.dsub
					var dist float32
					for d := 0; d < pq.dsub; d++ {
						diff := subvec[d] - pq.centroids[centroidBase+d]
						dist += diff * diff
					}
					if dist < bestDist {
						bestDist = dist
						bestK = k
					}
				}
				assignments[i] = bestK
			}

			// Update centroids
			for i := range sums {
				sums[i] = 0
			}
			for i := range counts {
				counts[i] = 0
			}
			for i, vec := range vectors {
				k := assignments[i]
				counts[k]++
				for d := 0; d < pq.dsub; d++ {
					sums[k*pq.dsub+d] += vec[offset+d]
				}
			}
			for k := 0; k < ksub; k++ {
				centroidBase := (sub*ksub + k) * pq.dsub
				if counts[k] > 0 {
					for d := 0; d < pq.dsub; d++ {
						pq.centroids[centroidBase+d] = sums[k*pq.dsub+d] / float32(counts[k])
					}
				}
			}
		}
	}
}

// Encode сжимает вектор в m байт.
func (pq *PQIndex) Encode(vec []float32) []byte {
	code := make([]byte, pq.m)
	ksub := 256
	for sub := 0; sub < pq.m; sub++ {
		offset := sub * pq.dsub
		subvec := vec[offset : offset+pq.dsub]
		bestK := 0
		bestDist := float32(math.MaxFloat32)
		for k := 0; k < ksub; k++ {
			centroidBase := (sub*ksub + k) * pq.dsub
			var dist float32
			for d := 0; d < pq.dsub; d++ {
				diff := subvec[d] - pq.centroids[centroidBase+d]
				dist += diff * diff
			}
			if dist < bestDist {
				bestDist = dist
				bestK = k
			}
		}
		code[sub] = byte(bestK)
	}
	return code
}

// EncodeAll сжимает все вектора.
func (pq *PQIndex) EncodeAll(vectors [][]float32) {
	pq.n = len(vectors)
	pq.codes = make([]byte, pq.n*pq.m)
	for i, vec := range vectors {
		code := pq.Encode(vec)
		copy(pq.codes[i*pq.m:(i+1)*pq.m], code)
	}
}

// precomputeDistTable строит таблицу distance[sub][centroid] для query.
func (pq *PQIndex) precomputeDistTable(query []float32) []float32 {
	ksub := 256
	table := make([]float32, pq.m*ksub)
	for sub := 0; sub < pq.m; sub++ {
		offset := sub * pq.dsub
		subquery := query[offset : offset+pq.dsub]
		for k := 0; k < ksub; k++ {
			centroidBase := (sub*ksub + k) * pq.dsub
			var dist float32
			for d := 0; d < pq.dsub; d++ {
				diff := subquery[d] - pq.centroids[centroidBase+d]
				dist += diff * diff
			}
			table[sub*ksub+k] = dist
		}
	}
	return table
}

// DistancePQ считает distance от query до вектора #i через таблицу.
func (pq *PQIndex) DistancePQ(distTable []float32, i int) float32 {
	var dist float32
	codeBase := i * pq.m
	for sub := 0; sub < pq.m; sub++ {
		dist += distTable[sub*256+int(pq.codes[codeBase+sub])]
	}
	return dist
}

// ============================================================================
// БЕНЧМАРК: Full Vectors vs PQ — Cache Effect
// ============================================================================

func TestPQBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("тяжёлый бенчмарк; запуск без -short")
	}
	const (
		dim = 768
		m   = 96 // подвекторов (768/96 = 8 floats каждый)
	)

	sizes := []int{10_000, 50_000, 150_000}

	fmt.Println(strings.Repeat("=", 80))
	fmt.Println("БЕНЧМАРК: Product Quantization vs Full Vectors (Cache Effect)")
	fmt.Printf("  CPU: %d cores | dim=%d | PQ m=%d (code=%d bytes)\n",
		runtime.NumCPU(), dim, m, m)

	// Получаем размер L3 cache (приблизительно)
	fmt.Printf("  Pointer size: %d bytes (arch: %s)\n", unsafe.Sizeof(uintptr(0)),
		runtime.GOARCH)
	fmt.Println(strings.Repeat("=", 80))

	rng := rand.New(rand.NewSource(42))

	for _, N := range sizes {
		fmt.Printf("\n%s\n", strings.Repeat("─", 80))
		fmt.Printf("  N = %d vectors, dim = %d\n", N, dim)
		fmt.Printf("%s\n", strings.Repeat("─", 80))

		// Генерируем вектора
		vectors := make([][]float32, N)
		for i := range vectors {
			v := make([]float32, dim)
			for j := range v {
				v[j] = float32(rng.NormFloat64())
			}
			// Normalize
			var norm float32
			for _, x := range v {
				norm += x * x
			}
			norm = float32(math.Sqrt(float64(norm)))
			for j := range v {
				v[j] /= norm
			}
			vectors[i] = v
		}

		// ═══════════════════════════════════════════════════════════════
		// 1. Размеры в памяти
		// ═══════════════════════════════════════════════════════════════
		fullSize := N * dim * 4
		pqSize := N * m
		fullSizeMB := float64(fullSize) / 1024 / 1024
		pqSizeMB := float64(pqSize) / 1024 / 1024

		fmt.Printf("\n  📊 Размер данных:\n")
		fmt.Printf("     Full vectors: %8.1f MB (%d × %d × 4B)\n", fullSizeMB, N, dim)
		fmt.Printf("     PQ codes:     %8.1f MB (%d × %dB)  [%.0fx сжатие]\n",
			pqSizeMB, N, m, fullSizeMB/pqSizeMB)

		// ═══════════════════════════════════════════════════════════════
		// 2. Плоский массив (имитация VectorArena)
		// ═══════════════════════════════════════════════════════════════
		arena := make([]float32, N*dim)
		for i, vec := range vectors {
			copy(arena[i*dim:(i+1)*dim], vec)
		}

		// ═══════════════════════════════════════════════════════════════
		// 3. PQ: Train + Encode
		// ═══════════════════════════════════════════════════════════════
		trainSample := vectors
		if len(trainSample) > 10000 {
			trainSample = vectors[:10000]
		}

		fmt.Printf("\n  🔧 PQ Training (k-means, 8 iterations)...\n")
		startTrain := time.Now()
		pqIdx := NewPQIndex(dim, m)
		pqIdx.Train(trainSample)
		trainDur := time.Since(startTrain)
		fmt.Printf("     Train time: %v\n", trainDur)

		startEncode := time.Now()
		pqIdx.EncodeAll(vectors)
		encodeDur := time.Since(startEncode)
		fmt.Printf("     Encode time: %v (%.0f vec/sec)\n",
			encodeDur, float64(N)/encodeDur.Seconds())

		// ═══════════════════════════════════════════════════════════════
		// 4. Бенчмарк: Random Access Distance (имитация HNSW traversal)
		// ═══════════════════════════════════════════════════════════════
		numQueries := 200
		nodesPerSearch := 500 // ~кол-во нод которые HNSW посещает при search

		// Генерируем query и random access pattern (как HNSW прыгает по графу)
		queries := make([][]float32, numQueries)
		accessPatterns := make([][]int, numQueries)
		for i := range queries {
			q := make([]float32, dim)
			for j := range q {
				q[j] = float32(rng.NormFloat64())
			}
			var norm float32
			for _, x := range q {
				norm += x * x
			}
			norm = float32(math.Sqrt(float64(norm)))
			for j := range q {
				q[j] /= norm
			}
			queries[i] = q

			pattern := make([]int, nodesPerSearch)
			for j := range pattern {
				pattern[j] = rng.Intn(N)
			}
			accessPatterns[i] = pattern
		}

		// ── 4a. Full vector distance (random access по arena) ──
		runtime.GC()
		time.Sleep(10 * time.Millisecond)

		startFull := time.Now()
		var sinkFull float32
		for qi := 0; qi < numQueries; qi++ {
			query := queries[qi]
			for _, nodeIdx := range accessPatterns[qi] {
				vecStart := nodeIdx * dim
				vec := arena[vecStart : vecStart+dim]
				var dot float32
				for d := 0; d < dim; d++ {
					dot += query[d] * vec[d]
				}
				sinkFull += 1 - dot
			}
		}
		fullDur := time.Since(startFull)
		fullPerSearch := fullDur / time.Duration(numQueries)
		fullDistPerSec := float64(numQueries*nodesPerSearch) / fullDur.Seconds()

		// ── 4b. PQ distance (random access по codes) ──
		runtime.GC()
		time.Sleep(10 * time.Millisecond)

		startPQ := time.Now()
		var sinkPQ float32
		for qi := 0; qi < numQueries; qi++ {
			distTable := pqIdx.precomputeDistTable(queries[qi])
			for _, nodeIdx := range accessPatterns[qi] {
				sinkPQ += pqIdx.DistancePQ(distTable, nodeIdx)
			}
		}
		pqDur := time.Since(startPQ)
		pqPerSearch := pqDur / time.Duration(numQueries)
		pqDistPerSec := float64(numQueries*nodesPerSearch) / pqDur.Seconds()

		speedup := float64(fullDur) / float64(pqDur)

		fmt.Printf("\n  ⚡ Random Access Distance (%d queries × %d nodes):\n", numQueries, nodesPerSearch)
		fmt.Printf("     Full vectors: %v/search  (%.0f dist/sec)\n", fullPerSearch, fullDistPerSec)
		fmt.Printf("     PQ codes:     %v/search  (%.0f dist/sec)\n", pqPerSearch, pqDistPerSec)
		fmt.Printf("     Speedup:      %.1fx\n", speedup)

		// ═══════════════════════════════════════════════════════════════
		// 5. Parallel benchmark (12 cores)
		// ═══════════════════════════════════════════════════════════════
		concurrency := runtime.NumCPU()

		// ── 5a. Full parallel ──
		runtime.GC()
		time.Sleep(10 * time.Millisecond)

		startFullP := time.Now()
		var wg sync.WaitGroup
		perThread := numQueries / concurrency
		for tid := 0; tid < concurrency; tid++ {
			wg.Add(1)
			go func(t int) {
				defer wg.Done()
				var localSink float32
				for qi := t * perThread; qi < (t+1)*perThread; qi++ {
					query := queries[qi%numQueries]
					for _, nodeIdx := range accessPatterns[qi%numQueries] {
						vecStart := nodeIdx * dim
						vec := arena[vecStart : vecStart+dim]
						var dot float32
						for d := 0; d < dim; d++ {
							dot += query[d] * vec[d]
						}
						localSink += 1 - dot
					}
				}
				sinkFull += localSink
			}(tid)
		}
		wg.Wait()
		fullParDur := time.Since(startFullP)
		fullParQPS := float64(numQueries) / fullParDur.Seconds()

		// ── 5b. PQ parallel ──
		runtime.GC()
		time.Sleep(10 * time.Millisecond)

		startPQP := time.Now()
		for tid := 0; tid < concurrency; tid++ {
			wg.Add(1)
			go func(t int) {
				defer wg.Done()
				var localSink float32
				for qi := t * perThread; qi < (t+1)*perThread; qi++ {
					distTable := pqIdx.precomputeDistTable(queries[qi%numQueries])
					for _, nodeIdx := range accessPatterns[qi%numQueries] {
						localSink += pqIdx.DistancePQ(distTable, nodeIdx)
					}
				}
				sinkPQ += localSink
			}(tid)
		}
		wg.Wait()
		pqParDur := time.Since(startPQP)
		pqParQPS := float64(numQueries) / pqParDur.Seconds()

		parSpeedup := float64(fullParDur) / float64(pqParDur)

		fmt.Printf("\n  🔥 Parallel (%d cores, %d queries × %d nodes):\n",
			concurrency, numQueries, nodesPerSearch)
		fmt.Printf("     Full vectors: %.0f QPS (%.1f ms/query)\n",
			fullParQPS, float64(fullParDur.Microseconds())/float64(numQueries)/1000)
		fmt.Printf("     PQ codes:     %.0f QPS (%.1f ms/query)\n",
			pqParQPS, float64(pqParDur.Microseconds())/float64(numQueries)/1000)
		fmt.Printf("     Speedup:      %.1fx\n", parSpeedup)

		// ═══════════════════════════════════════════════════════════════
		// 6. Recall: PQ distance vs exact distance
		// ═══════════════════════════════════════════════════════════════
		K := 10
		numRecallQueries := 100
		totalRecall := 0.0

		for qi := 0; qi < numRecallQueries; qi++ {
			query := queries[qi%numQueries]
			distTable := pqIdx.precomputeDistTable(query)

			// Exact top-K
			type kv struct {
				idx  int
				dist float32
			}
			exactDists := make([]kv, N)
			for i := 0; i < N; i++ {
				vecStart := i * dim
				vec := arena[vecStart : vecStart+dim]
				var dot float32
				for d := 0; d < dim; d++ {
					dot += query[d] * vec[d]
				}
				exactDists[i] = kv{i, 1 - dot}
			}
			// Partial sort: find top-K
			topK := make([]kv, K)
			copy(topK, exactDists[:K])
			for i := 1; i < K; i++ {
				for j := i; j > 0 && topK[j].dist < topK[j-1].dist; j-- {
					topK[j], topK[j-1] = topK[j-1], topK[j]
				}
			}
			for i := K; i < N; i++ {
				if exactDists[i].dist < topK[K-1].dist {
					topK[K-1] = exactDists[i]
					for j := K - 1; j > 0 && topK[j].dist < topK[j-1].dist; j-- {
						topK[j], topK[j-1] = topK[j-1], topK[j]
					}
				}
			}
			exactSet := make(map[int]bool)
			for _, kv := range topK {
				exactSet[kv.idx] = true
			}

			// PQ top-K (brute force over all codes)
			pqDists := make([]kv, N)
			for i := 0; i < N; i++ {
				pqDists[i] = kv{i, pqIdx.DistancePQ(distTable, i)}
			}
			pqTopK := make([]kv, K)
			copy(pqTopK, pqDists[:K])
			for i := 1; i < K; i++ {
				for j := i; j > 0 && pqTopK[j].dist < pqTopK[j-1].dist; j-- {
					pqTopK[j], pqTopK[j-1] = pqTopK[j-1], pqTopK[j]
				}
			}
			for i := K; i < N; i++ {
				if pqDists[i].dist < pqTopK[K-1].dist {
					pqTopK[K-1] = pqDists[i]
					for j := K - 1; j > 0 && pqTopK[j].dist < pqTopK[j-1].dist; j-- {
						pqTopK[j], pqTopK[j-1] = pqTopK[j-1], pqTopK[j]
					}
				}
			}

			overlap := 0
			for _, kv := range pqTopK {
				if exactSet[kv.idx] {
					overlap++
				}
			}
			totalRecall += float64(overlap) / float64(K)
		}

		avgRecall := totalRecall / float64(numRecallQueries)
		fmt.Printf("\n  🎯 PQ Recall@%d: %.1f%% (over %d queries)\n", K, avgRecall*100, numRecallQueries)

		// Prevent compiler from optimizing away
		if sinkFull == -999 && sinkPQ == -999 {
			fmt.Println("never")
		}
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 80))
	fmt.Println("КОНЕЦ БЕНЧМАРКА")
	fmt.Println(strings.Repeat("=", 80))
}
