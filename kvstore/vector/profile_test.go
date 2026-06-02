package vector

import (
	"fmt"
	"math/rand"
	"sync/atomic"
	"runtime"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
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

	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		g.Insert(vec)
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

// BenchmarkSearch_Cosine — поиск с «сырым» CosineDistance.
// 3 цикла + 2×sqrt + деление на каждый вызов distance.
func BenchmarkSearch_Cosine(b *testing.B) {
	const (
		numVectors = 10000
		dim        = 128
		K          = 10
		efSearch   = 100
	)

	rng := rand.New(rand.NewSource(42))

	g := NewGraph(CosineDistance, tcmalloc.NewTCMallocStore(1))
	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		g.Insert(vec)
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

// BenchmarkSearch_CosinePreNorm — поиск с pre-normalized DotProductDistance.
// 1 цикл, 0 sqrt, 0 делений. Вектора нормализованы при вставке.
func BenchmarkSearch_CosinePreNorm(b *testing.B) {
	const (
		numVectors = 10000
		dim        = 128
		K          = 10
		efSearch   = 100
	)

	rng := rand.New(rand.NewSource(42))

	g := NewGraph(DotProductDistance, tcmalloc.NewTCMallocStore(1))
	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		Normalize(vec) // pre-normalize при вставке
		g.Insert(vec)
	}

	queries := make([][]float32, 100)
	for i := range queries {
		q := make([]float32, dim)
		for j := range q {
			q[j] = rng.Float32()*2 - 1
		}
		Normalize(q) // pre-normalize запрос
		queries[i] = q
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		g.Search(queries[i%len(queries)], K, efSearch)
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

	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		vs.Add(fmt.Sprintf("vec:%d", i), vec)
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
		vs.Search(queries[i%len(queries)], K)
	}
}

// BenchmarkSearch_Full_Go_Parallel — параллельный поиск через VectorStore.
func BenchmarkSearch_Full_Go_Parallel(b *testing.B) {
	const (
		numVectors = 10000
		dim        = 128
		K          = 10
	)

	rng := rand.New(rand.NewSource(42))

	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		vs.Add(fmt.Sprintf("vec:%d", i), vec)
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
	_ = uint32(runtime.NumCPU())
	b.RunParallel(func(pb *testing.PB) {
		atomic.AddUint32(&counter, 1)
		i := 0
		for pb.Next() {
			vs.Search(queries[i%len(queries)], K)
			i++
		}
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// Бенчмарк: изолированный Go distance по размерностям.
//
//	go test ./kvstore/vector/ -bench 'BenchmarkDistance_' -benchtime=3s
// ═══════════════════════════════════════════════════════════════════════════════

func benchmarkDistanceGo(b *testing.B, dim int) {
	rng := rand.New(rand.NewSource(42))
	a := make([]float32, dim)
	bv := make([]float32, dim)
	for j := range a {
		a[j] = rng.Float32()*2 - 1
		bv[j] = rng.Float32()*2 - 1
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		EuclideanDistance(a, bv)
	}
}

// dim=128 (Ollama nomic-embed-text по умолчанию)
func BenchmarkDistance_Go_128(b *testing.B) { benchmarkDistanceGo(b, 128) }

// dim=256
func BenchmarkDistance_Go_256(b *testing.B) { benchmarkDistanceGo(b, 256) }

// dim=512
func BenchmarkDistance_Go_512(b *testing.B) { benchmarkDistanceGo(b, 512) }

// dim=768 (BERT, nomic-embed-text, многие LLM)
func BenchmarkDistance_Go_768(b *testing.B) { benchmarkDistanceGo(b, 768) }

// dim=1024
func BenchmarkDistance_Go_1024(b *testing.B) { benchmarkDistanceGo(b, 1024) }

// dim=1536 (OpenAI text-embedding-ada-002)
func BenchmarkDistance_Go_1536(b *testing.B) { benchmarkDistanceGo(b, 1536) }

// dim=2048
func BenchmarkDistance_Go_2048(b *testing.B) { benchmarkDistanceGo(b, 2048) }

// dim=3072 (OpenAI text-embedding-3-large)
func BenchmarkDistance_Go_3072(b *testing.B) { benchmarkDistanceGo(b, 3072) }

// ═══════════════════════════════════════════════════════════════════════════════
// Сравнение: Euclidean vs Cosine vs DotProduct (dim=128)
// ═══════════════════════════════════════════════════════════════════════════════

func BenchmarkDistance_Cosine_128(b *testing.B) {
	rng := rand.New(rand.NewSource(42))
	a := make([]float32, 128)
	bv := make([]float32, 128)
	for j := range a {
		a[j] = rng.Float32()*2 - 1
		bv[j] = rng.Float32()*2 - 1
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		CosineDistance(a, bv)
	}
}

func BenchmarkDistance_DotProduct_128(b *testing.B) {
	rng := rand.New(rand.NewSource(42))
	a := make([]float32, 128)
	bv := make([]float32, 128)
	for j := range a {
		a[j] = rng.Float32()*2 - 1
		bv[j] = rng.Float32()*2 - 1
	}
	Normalize(a)
	Normalize(bv)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		DotProductDistance(a, bv)
	}
}
