package vector

import (
	"math/rand"
	"testing"
)

// BenchmarkSearch_Full — полный HNSW Search.
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
		g.Insert(uint64(i), vec)
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
		g.Search(queries[i%len(queries)], K, efSearch)
	}
}
