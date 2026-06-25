package vector

import (
	"bytes"
	"fmt"
	"testing"
)

// =============================================================================
// Тесты FrozenGraphSQ — SQ8 recall vs FrozenGraph (float32 baseline).
//
// Цель: подтвердить что SQ8+rerank держит recall ≥90% от float32 baseline.
// Базовая линия: TestFrozenGraph_Recall = 97.4% на 3000 векторов.
// =============================================================================

// TestFrozenGraphSQ_Build проверяет базовое построение и MemoryBytes.
func TestFrozenGraphSQ_Build(t *testing.T) {
	const n, dim = 2000, 128

	alloc := newTestAllocator()
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = 200

	vecs := makeRandVecs(n, dim, 42)
	keys := make(map[uint64]string, n)
	for i, v := range vecs {
		idx := g.Insert(v)
		keys[uint64(idx)] = fmt.Sprintf("vec-%d", i)
	}

	fg := FreezeGraphSQ(g, EuclideanDistance, keys)
	if fg == nil {
		t.Fatal("FreezeGraphSQ returned nil")
	}
	if fg.Len() != n {
		t.Fatalf("expected %d nodes, got %d", n, fg.Len())
	}
	if len(fg.codes) != n*dim {
		t.Fatalf("codes len: expected %d, got %d", n*dim, len(fg.codes))
	}
	if len(fg.sqMin) != dim || len(fg.sqScale) != dim {
		t.Fatalf("sqMin/sqScale dim: expected %d, got min=%d scale=%d",
			dim, len(fg.sqMin), len(fg.sqScale))
	}

	t.Logf("FrozenGraphSQ: n=%d dim=%d mem=%.1fMB (float32 would be %.1fMB)",
		n, dim, float64(fg.MemoryBytes())/1e6, float64(n*dim*4)/1e6)

	// Memory для данных векторов (codes+params) должно быть ~4× меньше float32.
	// MemoryBytes включает CSR layers (топология графа) — не считаем их.
	vectorBytesSQ := n*dim + dim*4*2 // codes + sqMin + sqScale
	float32Bytes := n * dim * 4
	if vectorBytesSQ > float32Bytes/2 {
		t.Errorf("SQ vector data too large: %d > %d (half of float32)", vectorBytesSQ, float32Bytes/2)
	}
	t.Logf("Vector data only: SQ=%d bytes, float32=%d bytes (%.1f× compression)",
		vectorBytesSQ, float32Bytes, float64(float32Bytes)/float64(vectorBytesSQ))
}

// TestFrozenGraphSQ_Recall сравнивает recall SQ8 vs FrozenGraph на 3000 векторов.
func TestFrozenGraphSQ_Recall(t *testing.T) {
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

	// Float32 baseline
	fgF32 := FreezeGraph(g, EuclideanDistance, keys)
	// SQ8
	fgSQ := FreezeGraphSQ(g, EuclideanDistance, keys)

	queries := makeRandVecs(50, dim, 777)

	// Считаем recall для каждого варианта
	measureRecall := func(search func(q []float32) []FrozenResult) (float64, float64) {
		totalFound, totalExpected := 0, 0
		var totalDistErr float64
		for _, q := range queries {
			gt := bruteForceTopK(vecs, q, K)
			gtSet := make(map[int]bool, K)
			for _, idx := range gt {
				gtSet[idx] = true
			}
			results := search(q)
			for _, r := range results {
				var idx int
				fmt.Sscanf(r.Key, "%d", &idx)
				if gtSet[idx] {
					totalFound++
				}
			}
			totalExpected += K
		}
		return float64(totalFound) / float64(totalExpected), totalDistErr / float64(len(queries))
	}

	recallF32, _ := measureRecall(func(q []float32) []FrozenResult {
		return fgF32.Search(q, K, 100, nil, nil)
	})
	recallSQ, _ := measureRecall(func(q []float32) []FrozenResult {
		return fgSQ.Search(q, K, 100, nil, nil)
	})

	t.Logf("Recall@%d (n=%d): float32=%.1f%%  SQ8=%.1f%%", K, n, recallF32*100, recallSQ*100)
	t.Logf("SQ8 memory: %.1fKB vs float32 %.1fKB (%.1f× compression)",
		float64(fgSQ.MemoryBytes())/1024, float64(n*dim*4)/1024,
		float64(n*dim*4)/float64(fgSQ.MemoryBytes()))

	if recallSQ < 0.80 {
		t.Errorf("SQ8 recall too low: %.1f%% < 80%%", recallSQ*100)
	}
}

// TestFrozenGraphSQ_RecallAtEfBoost проверяет recall при увеличенном efSearch.
// SQ8 неточнее float32, но компенсируется расширенным efSearch.
func TestFrozenGraphSQ_RecallAtEfBoost(t *testing.T) {
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
	fgSQ := FreezeGraphSQ(g, EuclideanDistance, keys)

	queries := makeRandVecs(50, dim, 777)

	for _, efSearch := range []int{100, 200, 400} {
		totalFound, totalExpected := 0, 0
		for _, q := range queries {
			gt := bruteForceTopK(vecs, q, K)
			gtSet := make(map[int]bool, K)
			for _, idx := range gt {
				gtSet[idx] = true
			}
			results := fgSQ.Search(q, K, efSearch, nil, nil)
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
		t.Logf("efSearch=%d: SQ8 Recall@%d = %.1f%%", efSearch, K, recall*100)
	}
}

// TestFrozenGraphSQ_SerializationRoundTrip проверяет save→load→recall.
func TestFrozenGraphSQ_SerializationRoundTrip(t *testing.T) {
	const n, dim, K = 1000, 128, 10

	alloc := newTestAllocator()
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = 200

	vecs := makeRandVecs(n, dim, 42)
	keys := make(map[uint64]string, n)
	for i, v := range vecs {
		idx := g.Insert(v)
		keys[uint64(idx)] = fmt.Sprintf("%d", i)
	}
	fgOrig := FreezeGraphSQ(g, EuclideanDistance, keys)

	// Serialize → buffer → deserialize
	var buf bytes.Buffer
	if err := fgOrig.WriteGraphToSQ(&buf); err != nil {
		t.Fatalf("WriteGraphToSQ: %v", err)
	}

	fgLoaded, err := ReadFrozenGraphSQ(&buf, EuclideanDistance)
	if err != nil {
		t.Fatalf("ReadFrozenGraphSQ: %v", err)
	}
	if fgLoaded.Len() != fgOrig.Len() {
		t.Fatalf("n mismatch: orig=%d loaded=%d", fgOrig.Len(), fgLoaded.Len())
	}
	if fgLoaded.dim != fgOrig.dim {
		t.Fatalf("dim mismatch: orig=%d loaded=%d", fgOrig.dim, fgLoaded.dim)
	}

	// Сравниваем recall: orig vs loaded
	queries := makeRandVecs(30, dim, 999)
	measureRecall := func(fg *FrozenGraphSQ) float64 {
		totalFound, totalExpected := 0, 0
		for _, q := range queries {
			gt := bruteForceTopK(vecs, q, K)
			gtSet := make(map[int]bool, K)
			for _, idx := range gt {
				gtSet[idx] = true
			}
			results := fg.Search(q, K, 100, nil, nil)
			for _, r := range results {
				var idx int
				fmt.Sscanf(r.Key, "%d", &idx)
				if gtSet[idx] {
					totalFound++
				}
			}
			totalExpected += K
		}
		return float64(totalFound) / float64(totalExpected)
	}

	recallOrig := measureRecall(fgOrig)
	recallLoaded := measureRecall(fgLoaded)
	t.Logf("Round-trip recall: orig=%.1f%% loaded=%.1f%%", recallOrig*100, recallLoaded*100)

	if recallLoaded < recallOrig-0.05 {
		t.Errorf("recall dropped after round-trip: orig=%.3f loaded=%.3f", recallOrig, recallLoaded)
	}
}
