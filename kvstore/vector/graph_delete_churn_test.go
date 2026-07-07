package vector

import (
	"math/rand"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// BenchmarkGraphDeleteUpsertChurn воспроизводит паттерн дельта-шарда под
// upsert-нагрузкой: на каждый upsert существующего ключа AppendWithAttrs
// делает idx.Delete(oldGid) + idx.Insert (delta.go). Метрика — B/op и
// allocs/op самого Delete-пути: до фикса Фаза 2 Delete копировала список
// соседей КАЖДОЙ ноды графа независимо от того, ссылается ли она на
// удаляемый id (~16ГБ churn за soak).
func BenchmarkGraphDeleteUpsertChurn(b *testing.B) {
	const (
		n   = 4096 // порядок живого дельта-шарда
		dim = 128
	)
	rng := rand.New(rand.NewSource(42))
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	vecs := make([][]float32, n)
	ids := make([]uint64, n)
	for i := range vecs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()
		}
		vecs[i] = v
		ids[i] = uint64(g.Insert(v))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		slot := i % n
		g.Delete(ids[slot])
		ids[slot] = uint64(g.Insert(vecs[slot]))
	}
}
