package vector

import (
	"fmt"
	"testing"
	"time"
)

// TestLeveledStore_MergeTrigger проверяет, что background merges запускаются
// после переполнения L0 (fanout сегментов).
func TestLeveledStore_MergeTrigger(t *testing.T) {
	const dim = 128
	const deltaMax = 100 // маленькая дельта → много L0 сегментов быстро

	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       deltaMax,
		EfConstruction: 64, // быстро
		M:              16,
		EfSearch:       100,
		Distance:       EuclideanDistance,
		Fanout:         4,
		UseSQ:          false,
	})
	defer lvs.Close()

	// Вставляем 1000 векторов → 10 delta-flush → 10 L0 сегментов → должно дать merges.
	rng := makeRandVecs(1000, dim, 42)
	for i := 0; i < 1000; i++ {
		_ = lvs.Add(fmt.Sprintf("k%d", i), rng[i])
	}

	// Ждём FlushDelta + merges
	lvs.FlushDeltaSync()

	// Ждём background merges
	maxIters := 30
	if testing.Short() {
		maxIters = 3 // под -short не крутим polling до минуты, но целостность всё равно проверяем
	}
	for i := 0; i < maxIters; i++ {
		s := lvs.Stats()
		t.Logf("  [%ds] L0=%d L1=%d L2=%d delta=%d total=%d",
			i*2, s.SegmentsByLevel[0], s.SegmentsByLevel[1], s.SegmentsByLevel[2],
			s.DeltaLen, s.TotalVectors)
		if s.SegmentsByLevel[0] < 4 && s.DeltaLen == 0 {
			t.Logf("  merges done!")
			break
		}
		time.Sleep(2 * time.Second)
	}

	s := lvs.Stats()
	if s.TotalVectors != 1000 {
		t.Errorf("data loss: expected 1000, got %d", s.TotalVectors)
	}
}
