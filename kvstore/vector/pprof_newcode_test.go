package vector

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"
)

// =============================================================================
// pprof профилирование новых подсистем:
//   go test ./vector/ -run TestPprofNewCode -v -count=1 -timeout 120s
//
// После запуска читаем результаты:
//   go tool pprof -text cpu.prof
//   go tool pprof -text mem.prof
//   go tool pprof -text alloc.prof
// =============================================================================

func TestPprofNewCode(t *testing.T) {
	const (
		dim      = 128
		n        = 5000
		nQueries = 10_000
		K        = 10
		efSearch = 100
	)

	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = makeRandVec(dim, rng)
	}
	query := makeRandVec(dim, rng)

	// ─── 1. DeltaSegment ─────────────────────────────────────────────────────
	t.Run("Delta_BruteForce", func(t *testing.T) {
		d := NewDeltaSegment(dim, n+1, EuclideanDistance, 0, 0)
		for i, v := range vecs {
			d.Append(fmt.Sprintf("k%d", i), v)
		}

		cpuF, _ := os.Create("prof_delta_cpu.prof")
		pprof.StartCPUProfile(cpuF)

		runtime.GC()
		var memBefore, memAfter runtime.MemStats
		runtime.ReadMemStats(&memBefore)

		for i := 0; i < nQueries; i++ {
			_ = d.BruteForce(query, K, EuclideanDistance, nil)
		}

		pprof.StopCPUProfile()
		cpuF.Close()

		runtime.GC()
		runtime.ReadMemStats(&memAfter)

		allocsPerOp := float64(memAfter.Mallocs-memBefore.Mallocs) / float64(nQueries)
		bytesPerOp := float64(memAfter.TotalAlloc-memBefore.TotalAlloc) / float64(nQueries)
		t.Logf("DeltaBruteForce  allocs/op=%.2f  bytes/op=%.2f  GC_runs=%d",
			allocsPerOp, bytesPerOp, memAfter.NumGC-memBefore.NumGC)
	})

	// ─── 2. FrozenGraph Search ───────────────────────────────────────────────
	t.Run("FrozenGraph_Search", func(t *testing.T) {
		alloc := newTestAllocator()
		g := NewGraph(EuclideanDistance, alloc)
		g.EfConstruction = 200
		keys := make(map[uint64]string, n)
		for i, v := range vecs {
			idx := g.Insert(v)
			keys[uint64(idx)] = fmt.Sprintf("k%d", i)
		}
		fg := FreezeGraph(g, EuclideanDistance, keys)

		cpuF, _ := os.Create("prof_frozen_cpu.prof")
		pprof.StartCPUProfile(cpuF)

		runtime.GC()
		var memBefore, memAfter runtime.MemStats
		runtime.ReadMemStats(&memBefore)

		for i := 0; i < nQueries; i++ {
			_ = fg.Search(query, K, efSearch, nil, nil)

		}

		pprof.StopCPUProfile()
		cpuF.Close()

		runtime.GC()
		runtime.ReadMemStats(&memAfter)

		allocsPerOp := float64(memAfter.Mallocs-memBefore.Mallocs) / float64(nQueries)
		bytesPerOp := float64(memAfter.TotalAlloc-memBefore.TotalAlloc) / float64(nQueries)
		t.Logf("FrozenGraph.Search  allocs/op=%.2f  bytes/op=%.2f  GC_runs=%d",
			allocsPerOp, bytesPerOp, memAfter.NumGC-memBefore.NumGC)
	})

	// ─── 3. LeveledVectorStore.Search (только delta path) ───────────────────
	t.Run("LeveledStore_DeltaSearch", func(t *testing.T) {
		lvs := NewLeveledVectorStore(LeveledConfig{
			DeltaMax: n + 1, // не флашим
			EfSearch: efSearch,
			Distance: EuclideanDistance,
		})
		defer lvs.Close()
		for i, v := range vecs {
			_ = lvs.Add(fmt.Sprintf("k%d", i), v)
		}

		runtime.GC()
		var memBefore, memAfter runtime.MemStats
		runtime.ReadMemStats(&memBefore)

		cpuF, _ := os.Create("prof_leveled_delta_cpu.prof")
		pprof.StartCPUProfile(cpuF)

		for i := 0; i < nQueries; i++ {
			_, _ = lvs.Search(query, K, nil)
		}

		pprof.StopCPUProfile()
		cpuF.Close()

		runtime.GC()
		runtime.ReadMemStats(&memAfter)

		allocsPerOp := float64(memAfter.Mallocs-memBefore.Mallocs) / float64(nQueries)
		bytesPerOp := float64(memAfter.TotalAlloc-memBefore.TotalAlloc) / float64(nQueries)
		t.Logf("LeveledStore.Search(delta only)  allocs/op=%.2f  bytes/op=%.2f  GC_runs=%d",
			allocsPerOp, bytesPerOp, memAfter.NumGC-memBefore.NumGC)
	})

	// ─── 4. LeveledVectorStore.Search (после компакции) ─────────────────────
	t.Run("LeveledStore_PostCompactionSearch", func(t *testing.T) {
		lvs := NewLeveledVectorStore(LeveledConfig{
			DeltaMax:       500,
			EfConstruction: 64,
			EfSearch:       efSearch,
			Distance:       EuclideanDistance,
		})
		defer lvs.Close()
		for i, v := range vecs {
			_ = lvs.Add(fmt.Sprintf("k%d", i), v)
		}
		// Ждём компакцию
		lvs.FlushDelta()
		time.Sleep(2 * time.Second)

		lvs.mu.RLock()
		l0 := len(lvs.levels[0])
		lvs.mu.RUnlock()
		t.Logf("L0 segments after compaction: %d", l0)

		runtime.GC()
		var memBefore, memAfter runtime.MemStats
		runtime.ReadMemStats(&memBefore)

		cpuF, _ := os.Create("prof_leveled_compacted_cpu.prof")
		pprof.StartCPUProfile(cpuF)

		for i := 0; i < nQueries; i++ {
			_, _ = lvs.Search(query, K, nil)
		}

		pprof.StopCPUProfile()
		cpuF.Close()

		runtime.GC()
		runtime.ReadMemStats(&memAfter)

		allocsPerOp := float64(memAfter.Mallocs-memBefore.Mallocs) / float64(nQueries)
		bytesPerOp := float64(memAfter.TotalAlloc-memBefore.TotalAlloc) / float64(nQueries)
		t.Logf("LeveledStore.Search(compacted)  allocs/op=%.2f  bytes/op=%.2f  GC_runs=%d",
			allocsPerOp, bytesPerOp, memAfter.NumGC-memBefore.NumGC)
	})
}

// =============================================================================
// Детальные бенчмарки с -benchmem (самый точный способ считать аллокации)
// go test ./vector/ -run=^$ -bench="BenchmarkAlloc" -benchmem -benchtime=5s -timeout=120s
// =============================================================================

func BenchmarkAlloc_Delta_BruteForce_dim128(b *testing.B) {
	const n, dim, K = 5000, 128, 10
	d := NewDeltaSegment(dim, n+1, EuclideanDistance, 0, 0)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		d.Append(fmt.Sprintf("k%d", i), makeRandVec(dim, rng))
	}
	query := makeRandVec(dim, rng)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = d.BruteForce(query, K, EuclideanDistance, nil)
	}
}

func BenchmarkAlloc_FrozenSearch_dim128(b *testing.B) {
	const n, dim, K = 5000, 128, 10
	alloc := newTestAllocator()
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = 200
	rng := rand.New(rand.NewSource(42))
	keys := make(map[uint64]string, n)
	for i := 0; i < n; i++ {
		v := makeRandVec(dim, rng)
		idx := g.Insert(v)
		keys[uint64(idx)] = fmt.Sprintf("k%d", i)
	}
	fg := FreezeGraph(g, EuclideanDistance, keys)
	query := makeRandVec(dim, rng)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = fg.Search(query, K, 100, nil, nil)

	}
}

func BenchmarkAlloc_LeveledSearch_DeltaOnly(b *testing.B) {
	const n, dim, K = 5000, 128, 10
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax: n + 1,
		EfSearch: 100,
		Distance: EuclideanDistance,
	})
	b.Cleanup(func() { lvs.Close() })
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		_ = lvs.Add(fmt.Sprintf("k%d", i), makeRandVec(dim, rng))
	}
	query := makeRandVec(dim, rng)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = lvs.Search(query, K, nil)
	}
}

func BenchmarkAlloc_LeveledSearch_Compacted(b *testing.B) {
	const n, dim, K = 5000, 128, 10
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       500,
		EfConstruction: 64,
		EfSearch:       100,
		Distance:       EuclideanDistance,
	})
	b.Cleanup(func() { lvs.Close() })
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		_ = lvs.Add(fmt.Sprintf("k%d", i), makeRandVec(dim, rng))
	}
	lvs.FlushDelta()
	time.Sleep(2 * time.Second)
	query := makeRandVec(dim, rng)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = lvs.Search(query, K, nil)
	}
}

func BenchmarkAlloc_LeveledAdd(b *testing.B) {
	const dim = 128
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax: 1_000_000,
		Distance: EuclideanDistance,
	})
	b.Cleanup(func() { lvs.Close() })
	rng := rand.New(rand.NewSource(42))
	vec := makeRandVec(dim, rng)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = lvs.Add("key", vec) // upsert одного ключа — минимум аллокаций
	}
}
