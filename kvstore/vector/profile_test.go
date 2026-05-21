package vector

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sync/atomic"
	"testing"

	"kvstore/kvstore/internal/compute"
)

// BenchmarkSearch_Full — полный HNSW Search на чистом Go графе.
// Используй с pprof для анализа CPU:
//
//	go test ./kvstore/vector/ -bench BenchmarkSearch_Full -cpuprofile cpu.prof -benchtime=5s
//	go tool pprof -top -cum cpu.prof
func BenchmarkSearch_Full(b *testing.B) {
	const (
		numVectors = 10000
		dim        = 128
		K          = 10
		efSearch   = 100
	)

	rng := rand.New(rand.NewSource(42))

	g := NewGraph(EuclideanDistance)
	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		g.Insert(0, uint64(i), vec)
	}

	queries := make([][]float32, 100)
	for i := range queries {
		q := make([]float32, dim)
		for j := range q {
			q[j] = rng.Float32()*2 - 1
		}
		queries[i] = q
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		g.Search(0, queries[i%len(queries)], K, efSearch)
	}
}

// BenchmarkSearch_Full_Go — поиск через VectorStore на чистом Go.
func BenchmarkSearch_Full_Go(b *testing.B) {
	const (
		numVectors = 10000
		dim        = 128
		K          = 10
	)

	rng := rand.New(rand.NewSource(42))

	vs := NewVectorStore(EuclideanDistance)
	vs.SetEngine(0) // Чистый Go

	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		vs.Add(0, fmt.Sprintf("vec:%d", i), vec)
	}

	queries := make([][]float32, 100)
	for i := range queries {
		q := make([]float32, dim)
		for j := range q {
			q[j] = rng.Float32()*2 - 1
		}
		queries[i] = q
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		vs.Search(0, queries[i%len(queries)], K)
	}
}

// BenchmarkSearch_Full_Wasm — поиск через VectorStore с WASM SIMD ускорением расчёта расстояний.
func BenchmarkSearch_Full_Wasm(b *testing.B) {
	const (
		numVectors = 10000
		dim        = 128
		K          = 10
	)

	rng := rand.New(rand.NewSource(42))

	wasmBytes, err := os.ReadFile("../../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm")
	if err != nil {
		b.Skip("skipping WASM benchmark because vector_math_wasm.wasm not found")
	}

	e := compute.NewEngine()
	defer e.Close()

	wle := compute.NewWorkerLocalEngine(e)
	defer wle.Close()

	if err := wle.WarmUpFromBytes("vector_math", wasmBytes, compute.TierPinned); err != nil {
		b.Fatalf("failed to warm up WASM: %v", err)
	}

	vs := NewVectorStore(EuclideanDistance)
	vs.SetWorkerLocalEngine(wle)
	vs.SetEngine(1) // Включаем WASM SIMD ускорение

	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		vs.Add(0, fmt.Sprintf("vec:%d", i), vec)
	}

	queries := make([][]float32, 100)
	for i := range queries {
		q := make([]float32, dim)
		for j := range q {
			q[j] = rng.Float32()*2 - 1
		}
		queries[i] = q
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		vs.Search(0, queries[i%len(queries)], K)
	}
}

// BenchmarkSearch_Full_Go_Parallel — параллельный поиск через VectorStore на чистом Go.
func BenchmarkSearch_Full_Go_Parallel(b *testing.B) {
	const (
		numVectors = 10000
		dim        = 128
		K          = 10
	)

	rng := rand.New(rand.NewSource(42))

	vs := NewVectorStore(EuclideanDistance)
	vs.SetEngine(0) // Чистый Go

	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		vs.Add(0, fmt.Sprintf("vec:%d", i), vec)
	}

	queries := make([][]float32, 100)
	for i := range queries {
		q := make([]float32, dim)
		for j := range q {
			q[j] = rng.Float32()*2 - 1
		}
		queries[i] = q
	}

	b.ResetTimer()
	b.ReportAllocs()
	var counter uint32
	numWorkers := uint32(runtime.NumCPU())
	b.RunParallel(func(pb *testing.PB) {
		workerID := int(atomic.AddUint32(&counter, 1) % numWorkers)
		i := 0
		for pb.Next() {
			vs.Search(workerID, queries[i%len(queries)], K)
			i++
		}
	})
}

// BenchmarkSearch_Full_Wasm_Parallel — параллельный поиск через VectorStore с WASM SIMD в параллельном режиме.
func BenchmarkSearch_Full_Wasm_Parallel(b *testing.B) {
	const (
		numVectors = 10000
		dim        = 128
		K          = 10
	)

	rng := rand.New(rand.NewSource(42))

	wasmBytes, err := os.ReadFile("../../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm")
	if err != nil {
		b.Skip("skipping WASM benchmark because vector_math_wasm.wasm not found")
	}

	e := compute.NewEngine()
	defer e.Close()

	wle := compute.NewWorkerLocalEngine(e)
	defer wle.Close()

	if err := wle.WarmUpFromBytes("vector_math", wasmBytes, compute.TierPinned); err != nil {
		b.Fatalf("failed to warm up WASM: %v", err)
	}

	vs := NewVectorStore(EuclideanDistance)
	vs.SetWorkerLocalEngine(wle)
	vs.SetEngine(1) // Включаем WASM SIMD ускорение

	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		vs.Add(0, fmt.Sprintf("vec:%d", i), vec)
	}

	queries := make([][]float32, 100)
	for i := range queries {
		q := make([]float32, dim)
		for j := range q {
			q[j] = rng.Float32()*2 - 1
		}
		queries[i] = q
	}

	b.ResetTimer()
	b.ReportAllocs()
	var counter uint32
	numWorkers := uint32(runtime.NumCPU())
	b.RunParallel(func(pb *testing.PB) {
		workerID := int(atomic.AddUint32(&counter, 1) % numWorkers)
		i := 0
		for pb.Next() {
			vs.Search(workerID, queries[i%len(queries)], K)
			i++
		}
	})
}

