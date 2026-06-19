package vector

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// Unit-тесты для Delta, FrozenGraph, LeveledVectorStore
// =============================================================================

// ─── Helpers ─────────────────────────────────────────────────────────────────

func makeRandVec(dim int, rng *rand.Rand) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	return v
}

func makeRandVecs(n, dim int, seed int64) [][]float32 {
	rng := rand.New(rand.NewSource(seed))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = makeRandVec(dim, rng)
	}
	return vecs
}

// brute-force ground truth для recall тестов
func bruteForceTopK(vecs [][]float32, query []float32, K int) []int {
	type d struct{ idx int; dist float32 }
	ds := make([]d, len(vecs))
	for i, v := range vecs {
		ds[i] = d{i, EuclideanDistance(query, v)}
	}
	// partial sort: top K smallest
	for i := 0; i < K && i < len(ds); i++ {
		minJ := i
		for j := i + 1; j < len(ds); j++ {
			if ds[j].dist < ds[minJ].dist {
				minJ = j
			}
		}
		ds[i], ds[minJ] = ds[minJ], ds[i]
	}
	out := make([]int, K)
	for i := range out {
		out[i] = ds[i].idx
	}
	return out
}

// =============================================================================
// DeltaSegment тесты
// =============================================================================

func TestDelta_Append_And_BruteForce(t *testing.T) {
	const n, dim, K = 500, 128, 10
	d := NewDeltaSegment(dim, 1000)
	vecs := makeRandVecs(n, dim, 42)

	for i, v := range vecs {
		d.Append(fmt.Sprintf("key-%d", i), v)
	}

	if d.Len() != n {
		t.Fatalf("expected %d, got %d", n, d.Len())
	}

	query := makeRandVecs(1, dim, 999)[0]
	results := d.BruteForce(query, K, EuclideanDistance)

	if len(results) != K {
		t.Fatalf("expected %d results, got %d", K, len(results))
	}

	// Проверяем сортировку
	for i := 1; i < len(results); i++ {
		if results[i].dist < results[i-1].dist {
			t.Errorf("not sorted at %d: %.4f < %.4f", i, results[i].dist, results[i-1].dist)
		}
	}
	t.Logf("BruteForce top-%d dist range: [%.4f, %.4f]", K, results[0].dist, results[K-1].dist)
}

func TestDelta_Upsert(t *testing.T) {
	d := NewDeltaSegment(4, 100)
	d.Append("k1", []float32{1, 2, 3, 4})
	d.Append("k1", []float32{5, 6, 7, 8}) // upsert

	if d.Len() != 1 {
		t.Fatalf("expected 1 after upsert, got %d", d.Len())
	}

	// Проверяем что вектор обновился
	entries := d.ExtractAll()
	if len(entries) != 1 || entries[0].Vec[0] != 5 {
		t.Errorf("upsert did not update vector: %v", entries)
	}
}

func TestDelta_Delete(t *testing.T) {
	d := NewDeltaSegment(4, 100)
	d.Append("k1", []float32{1, 2, 3, 4})
	d.Append("k2", []float32{5, 6, 7, 8})

	ok := d.Delete("k1")
	if !ok {
		t.Fatal("Delete returned false for existing key")
	}

	entries := d.ExtractAll()
	if len(entries) != 1 || entries[0].Key != "k2" {
		t.Errorf("expected only k2, got %v", entries)
	}
}

func TestDelta_ExtractAll(t *testing.T) {
	const n, dim = 100, 16
	d := NewDeltaSegment(dim, 200)
	vecs := makeRandVecs(n, dim, 7)
	for i, v := range vecs {
		d.Append(fmt.Sprintf("key-%d", i), v)
	}

	entries := d.ExtractAll()
	if len(entries) != n {
		t.Fatalf("expected %d entries, got %d", n, len(entries))
	}
}

// =============================================================================
// FrozenGraph тесты
// =============================================================================

func TestFrozenGraph_Build_And_Search(t *testing.T) {
	const n, dim, K = 2000, 128, 10

	// Строим HNSW
	alloc := newTestAllocator()
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = 200

	vecs := makeRandVecs(n, dim, 42)
	keys := make(map[uint64]string, n)
	for i, v := range vecs {
		idx := g.Insert(v)
		keys[uint64(idx)] = fmt.Sprintf("vec-%d", i)
	}

	// Freeze
	fg := FreezeGraph(g, EuclideanDistance, keys)
	if fg == nil {
		t.Fatal("FreezeGraph returned nil")
	}
	if fg.Len() != n {
		t.Fatalf("expected %d nodes, got %d", n, fg.Len())
	}

	query := makeRandVecs(1, dim, 999)[0]
	results := fg.Search(query, K, 100)

	if len(results) == 0 {
		t.Fatal("Search returned no results")
	}
	if len(results) > K {
		t.Fatalf("Search returned %d > K=%d", len(results), K)
	}

	// Сортировка
	for i := 1; i < len(results); i++ {
		if results[i].Dist < results[i-1].Dist {
			t.Errorf("results not sorted at %d", i)
		}
	}

	t.Logf("FrozenGraph top-%d: keys=%v dist=%.4f..%.4f mem=%.1fMB",
		K, results[0].Key, results[0].Dist, results[K-1].Dist,
		float64(fg.MemoryBytes())/1e6)
}

func TestFrozenGraph_Recall(t *testing.T) {
	const n, dim, K = 3000, 128, 10

	alloc := newTestAllocator()
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = 200

	vecs := makeRandVecs(n, dim, 42)
	keys := make(map[uint64]string, n)
	for i, v := range vecs {
		idx := g.Insert(v)
		keys[uint64(idx)] = fmt.Sprintf("%d", i)
	}

	fg := FreezeGraph(g, EuclideanDistance, keys)

	// Тест на 50 запросах
	queries := makeRandVecs(50, dim, 777)
	totalFound, totalExpected := 0, 0

	for _, q := range queries {
		gt := bruteForceTopK(vecs, q, K)
		gtSet := make(map[int]bool, K)
		for _, idx := range gt {
			gtSet[idx] = true
		}

		results := fg.Search(q, K, 100)
		for _, r := range results {
			// Ключ = индекс вектора
			var idx int
			fmt.Sscanf(r.Key, "%d", &idx)
			if gtSet[idx] {
				totalFound++
			}
		}
		totalExpected += K
	}

	recall := float64(totalFound) / float64(totalExpected)
	t.Logf("Recall@%d = %.1f%% (found %d/%d)", K, recall*100, totalFound, totalExpected)

	if recall < 0.80 {
		t.Errorf("Recall too low: %.1f%% < 80%%", recall*100)
	}
}

// =============================================================================
// LeveledVectorStore тесты
// =============================================================================

func TestLeveledStore_AddAndSearch(t *testing.T) {
	const n, dim, K = 500, 128, 10

	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       1000, // дельта не флашнется в этом тесте
		EfConstruction: 64,
		EfSearch:       100,
		Distance:       EuclideanDistance,
	})
	defer lvs.Close()

	vecs := makeRandVecs(n, dim, 42)
	for i, v := range vecs {
		if err := lvs.Add(fmt.Sprintf("key-%d", i), v); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}

	if lvs.Len() != n {
		t.Fatalf("expected %d vectors, got %d", n, lvs.Len())
	}

	query := makeRandVecs(1, dim, 999)[0]
	results, err := lvs.Search(query, K)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned no results")
	}

	// Сортировка
	for i := 1; i < len(results); i++ {
		if results[i].Dist < results[i-1].Dist {
			t.Errorf("results not sorted at %d", i)
		}
	}
	t.Logf("LeveledStore top-%d: %s dist=%.4f", K, results[0].Key, results[0].Dist)
}

func TestLeveledStore_DimMismatch(t *testing.T) {
	lvs := NewLeveledVectorStore(LeveledConfig{Distance: EuclideanDistance})
	defer lvs.Close()

	if err := lvs.Add("k1", []float32{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := lvs.Add("k2", []float32{1, 2}); err == nil {
		t.Fatal("expected dim mismatch error, got nil")
	}
}

func TestLeveledStore_Compaction_L0(t *testing.T) {
	const dim = 128
	const deltaMax = 200 // маленькая дельта для быстрого теста

	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       deltaMax,
		EfConstruction: 64,
		EfSearch:       100,
		Distance:       EuclideanDistance,
	})
	defer lvs.Close()

	// Вставляем deltaMax+50 векторов — должна сработать компакция
	vecs := makeRandVecs(deltaMax+50, dim, 42)
	for i, v := range vecs {
		if err := lvs.Add(fmt.Sprintf("key-%d", i), v); err != nil {
			t.Fatalf("Add[%d] failed: %v", i, err)
		}
	}

	// Даём время компактору
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lvs.mu.RLock()
		l0len := len(lvs.levels[0])
		deltaLen := 0
		if lvs.delta != nil {
			deltaLen = lvs.delta.Len()
		}
		lvs.mu.RUnlock()

		if l0len > 0 {
			t.Logf("Compaction done: L0 has %d segment(s), delta has %d vectors", l0len, deltaLen)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	lvs.mu.RLock()
	l0len := len(lvs.levels[0])
	lvs.mu.RUnlock()
	if l0len == 0 {
		t.Error("Compaction did not produce any L0 segments")
	}

	// Поиск должен работать после компакции
	query := makeRandVecs(1, dim, 999)[0]
	results, err := lvs.Search(query, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned no results after compaction")
	}
	t.Logf("Post-compaction search: %d results, top dist=%.4f", len(results), results[0].Dist)
}

func TestLeveledStore_Recall(t *testing.T) {
	const n, dim, K = 2000, 128, 10
	const deltaMax = 500

	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       deltaMax,
		EfConstruction: 200,
		EfSearch:       100,
		Distance:       EuclideanDistance,
	})
	defer lvs.Close()

	vecs := makeRandVecs(n, dim, 42)
	for i, v := range vecs {
		if err := lvs.Add(fmt.Sprintf("%d", i), v); err != nil {
			t.Fatalf("Add[%d]: %v", i, err)
		}
	}

	// Ждём компакцию всех дельт
	lvs.FlushDelta()
	time.Sleep(3 * time.Second)

	queries := makeRandVecs(30, dim, 777)
	totalFound, totalExpected := 0, 0

	for _, q := range queries {
		gt := bruteForceTopK(vecs, q, K)
		gtSet := make(map[int]bool, K)
		for _, idx := range gt {
			gtSet[idx] = true
		}

		results, err := lvs.Search(q, K)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range results {
			var idx int
			fmt.Sscanf(r.Key, "%d", &idx)
			if gtSet[idx] {
				totalFound++
			}
		}
		totalExpected += K
	}

	recall := float64(totalFound) / float64(totalExpected)
	t.Logf("LeveledStore Recall@%d = %.1f%%", K, recall*100)

	if recall < 0.75 {
		t.Errorf("Recall too low: %.1f%% < 75%%", recall*100)
	}
}

// =============================================================================
// Бенчмарки для LeveledVectorStore
// =============================================================================

func BenchmarkLeveled_Add_dim128(b *testing.B) {
	benchmarkLeveledAdd(b, 128)
}
func BenchmarkLeveled_Add_dim768(b *testing.B) {
	benchmarkLeveledAdd(b, 768)
}

func benchmarkLeveledAdd(b *testing.B, dim int) {
	b.Helper()
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax: 1_000_000, // не флашить во время бенчмарка
		Distance: EuclideanDistance,
	})
	defer lvs.Close()

	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float32, 10_000)
	for i := range vecs {
		vecs[i] = makeRandVec(dim, rng)
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(dim * 4))
	for i := 0; i < b.N; i++ {
		_ = lvs.Add(fmt.Sprintf("k%d", i), vecs[i%len(vecs)])
	}
}

func BenchmarkLeveled_Search_dim128(b *testing.B) {
	benchmarkLeveledSearch(b, 128)
}
func BenchmarkLeveled_Search_dim768(b *testing.B) {
	benchmarkLeveledSearch(b, 768)
}

func benchmarkLeveledSearch(b *testing.B, dim int) {
	b.Helper()
	const n = 5000
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       n + 1, // всё в дельте (brute-force path)
		EfConstruction: 64,
		EfSearch:       100,
		Distance:       EuclideanDistance,
	})
	defer lvs.Close()

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		_ = lvs.Add(fmt.Sprintf("k%d", i), makeRandVec(dim, rng))
	}

	query := makeRandVec(dim, rng)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = lvs.Search(query, 10)
	}
}

// =============================================================================
// Вспомогательные функции
// =============================================================================

// newTestAllocator создаёт аллокатор для тестов.
func newTestAllocator() *tcmalloc.TCMallocStore {
	return tcmalloc.NewTCMallocStore(1)
}
