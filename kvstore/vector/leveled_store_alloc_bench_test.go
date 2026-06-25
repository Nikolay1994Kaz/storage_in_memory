package vector

import (
	"fmt"
	"math/rand"
	"testing"
)

// dummySegment имитирует сегмент HNSW/FrozenGraph для бенчмаркинга оверхеда поиска.
type dummySegment struct {
	results []FrozenResult
}

func (d *dummySegment) Len() int {
	return len(d.results)
}

func (d *dummySegment) HasKey(key string) bool {
	for _, r := range d.results {
		if r.Key == key {
			return true
		}
	}
	return false
}

func (d *dummySegment) Search(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	n := len(d.results)
	if n > K {
		n = K
	}
	if cap(dst) < n {
		dst = make([]FrozenResult, n)
	} else {
		dst = dst[:n]
	}
	copy(dst, d.results[:n])
	return dst
}

func newDummyLeveledStore(numSegs, dim, K int) *LeveledVectorStore {
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax: 1000,
		EfSearch: 100,
		Distance: EuclideanDistance,
	})
	// Задаем dim, чтобы прошел валидацию
	lvs.dim = dim

	// Наполняем levels[0] dummy-сегментами
	rng := rand.New(rand.NewSource(42))
	for s := 0; s < numSegs; s++ {
		// Каждый сегмент возвращает K результатов
		results := make([]FrozenResult, K)
		for i := 0; i < K; i++ {
			results[i] = FrozenResult{
				Key:  fmt.Sprintf("seg%d-key%d", s, i),
				Dist: rng.Float32() * 100,
			}
		}
		lvs.levels[0] = append(lvs.levels[0], &dummySegment{results: results})
	}
	return lvs
}

func BenchmarkLeveledFramework_1Seg(b *testing.B) {
	const dim = 128
	const K = 10
	lvs := newDummyLeveledStore(1, dim, K)
	query := make([]float32, dim)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = lvs.Search(query, K, nil)
	}
}

func BenchmarkLeveledFramework_3Segs(b *testing.B) {
	const dim = 128
	const K = 10
	lvs := newDummyLeveledStore(3, dim, K)
	query := make([]float32, dim)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = lvs.Search(query, K, nil)
	}
}

func BenchmarkLeveledFramework_5Segs(b *testing.B) {
	const dim = 128
	const K = 10
	lvs := newDummyLeveledStore(5, dim, K)
	query := make([]float32, dim)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = lvs.Search(query, K, nil)
	}
}

func BenchmarkLeveledFramework_10Segs(b *testing.B) {
	const dim = 128
	const K = 10
	lvs := newDummyLeveledStore(10, dim, K)
	query := make([]float32, dim)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = lvs.Search(query, K, nil)
	}
}
