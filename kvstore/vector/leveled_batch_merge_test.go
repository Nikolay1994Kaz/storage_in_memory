package vector

import (
	"fmt"
	"testing"
)

// bmPileupL0 заливает N векторов при удержанном mergeInFlight[0] → freeze-per-shard
// копит мини-сегменты в L0 без разгребания (модель пайл-апа bulk-load, e2e 10.07:
// 111 мини к концу заливки). Возвращает размер пайл-апа.
func bmPileupL0(t *testing.T, lvs *LeveledVectorStore, n, dim int) int {
	t.Helper()

	// Держим слот merge L0 занятым — планировщик не трогает копящиеся мини.
	lvs.mu.Lock()
	lvs.mergeInFlight[0] = true
	lvs.mu.Unlock()

	for i := 0; i < n; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), pfVec(dim, i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	lvs.FlushDeltaSync()

	lvs.mu.RLock()
	n0 := len(lvs.levels[0])
	lvs.mu.RUnlock()
	if n0 <= lvs.cfg.Fanout {
		t.Fatalf("пайл-ап не собрался: %d сегментов в L0 (нужно > fanout=%d)", n0, lvs.cfg.Fanout)
	}
	return n0
}

// bmReleaseMerges отпускает удержанный слот и пинает планировщик.
func bmReleaseMerges(lvs *LeveledVectorStore) {
	lvs.mu.Lock()
	lvs.mergeInFlight[0] = false
	lvs.mu.Unlock()
	lvs.maybeScheduleMerges(lvs.compactionEpoch.Load())
}

// bmExpectedRounds моделирует batch-каскад: сколько L1-сегментов даст пайл-ап n0
// при капе batchMax и сколько суб-fanout остатка застрянет в L0 (его по дизайну
// дочищает idle-консолидация, не каскад).
func bmExpectedRounds(n0, fanout, batchMax int) (rounds, leftover int) {
	rem := n0
	for rem >= fanout {
		take := rem
		if take > batchMax {
			take = batchMax
		}
		rem -= take
		rounds++
	}
	return rounds, rem
}

// TestBatchMerge_L0PileupSingleRound — весь пайл-ап мини-сегментов L0 уезжает
// ОДНИМ batch-merge в один L1-сегмент. Прежний каскад ровно по fanout дал бы
// ceil(n0/fanout) L1-сегментов и столько же полных rebuild-заходов.
func TestBatchMerge_L0PileupSingleRound(t *testing.T) {
	const (
		dim = 16
		N   = 600 // 3 полных дельты × 4 шарда ≈ 12 мини в L0
	)
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       200,
		DeltaShards:    4,
		Fanout:         4,
		M:              16,
		EfConstruction: 100,
		EfSearch:       200,
		UseSQ:          true,
		NumBuilders:    1,
	})
	defer lvs.Close()

	n0 := bmPileupL0(t, lvs, N, dim)
	if n0 > lvs.cfg.MergeBatchMax {
		t.Fatalf("пайл-ап %d превысил дефолтный кап %d — тест проверяет один заход", n0, lvs.cfg.MergeBatchMax)
	}
	bmReleaseMerges(lvs)

	waitUntil(t, func() bool {
		lvs.mu.RLock()
		defer lvs.mu.RUnlock()
		return len(lvs.levels[0]) == 0 && len(lvs.levels[1]) == 1
	})

	for i := 0; i < N; i++ {
		res, err := lvs.Search(pfVec(dim, i), 1, nil)
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if len(res) == 0 || res[0].Key != fmt.Sprintf("k%d", i) {
			t.Fatalf("ключ k%d потерян после batch-merge", i)
		}
	}
}

// TestBatchMerge_CapRespected — кап MergeBatchMax ограничивает один заход:
// пайл-ап больше капа разгребается за ceil-число заходов, каждый ≤ капа.
func TestBatchMerge_CapRespected(t *testing.T) {
	const (
		dim      = 16
		N        = 600
		batchCap = 6
	)
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       200,
		DeltaShards:    4,
		Fanout:         4,
		MergeBatchMax:  batchCap,
		M:              16,
		EfConstruction: 100,
		EfSearch:       200,
		UseSQ:          true,
		NumBuilders:    1,
	})
	defer lvs.Close()

	n0 := bmPileupL0(t, lvs, N, dim)
	if n0 <= batchCap {
		t.Fatalf("пайл-ап %d не превысил кап %d — тест не проверяет ограничение", n0, batchCap)
	}
	rounds, leftover := bmExpectedRounds(n0, lvs.cfg.Fanout, batchCap)
	bmReleaseMerges(lvs)

	waitUntil(t, func() bool {
		lvs.mu.RLock()
		defer lvs.mu.RUnlock()
		return len(lvs.levels[0]) == leftover && len(lvs.levels[1]) == rounds
	})

	for i := 0; i < N; i++ {
		res, err := lvs.Search(pfVec(dim, i), 1, nil)
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if len(res) == 0 || res[0].Key != fmt.Sprintf("k%d", i) {
			t.Fatalf("ключ k%d потерян после капованного batch-merge", i)
		}
	}
}
