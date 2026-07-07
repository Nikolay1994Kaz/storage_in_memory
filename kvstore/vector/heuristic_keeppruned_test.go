package vector

// Инвариант достижимости на unimodal-данных: на статичном графе self-query
// обязан находить каждую живую вершину при умеренном ef. Случайный порядок
// вставки независимых гауссиан — «дружелюбный» случай; адверсариальный
// (блочный порядок мультимодальных данных, как soak searchmiss 07.2026)
// закрыт TestSearchmiss_OracleRepro + шафлом buildSegment +
// Graph.RepairReachability.

import (
	"math/rand"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

func TestGraph_SelfQueryReachability(t *testing.T) {
	const (
		n   = 2000
		dim = 128
		ef  = 100
		k   = 10
	)
	rng := rand.New(rand.NewSource(4242))
	vecs := make([][]float32, n)
	for i := range vecs {
		v := make([]float32, dim)
		for d := range v {
			v[d] = float32(rng.NormFloat64())
		}
		vecs[i] = v
	}

	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	// Конфиг прод-сервера: M=32, efConstruction=400 (+ ресайз буферов под M0,
	// как в VectorStore.SetHNSWParams).
	g.M = 32
	g.M0 = 64
	g.EfConstruction = 400
	g.pruneBufItems = make([]item, 0, g.M0+1)
	g.pruneBufIDs = make([]uint64, 0, g.M0+1)
	g.insertBuf = make([]uint64, 0, g.M0+1)
	for i := range vecs {
		g.Insert(vecs[i])
	}

	miss := 0
	var missIdx []int
	for i := range vecs {
		res := g.Search(vecs[i], k, ef)
		found := false
		for _, r := range res {
			if r.ID == uint64(i) {
				found = true
				break
			}
		}
		if !found {
			miss++
			if len(missIdx) < 20 {
				missIdx = append(missIdx, i)
			}
		}
	}
	if miss > 0 {
		t.Fatalf("self-query: %d/%d вершин не находятся при ef=%d (примеры idx: %v)", miss, n, ef, missIdx)
	}
}
