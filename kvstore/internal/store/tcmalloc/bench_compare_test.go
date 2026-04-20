package tcmalloc

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store"
)

// ═══════════════════════════════════════════════════════════
// Сравнительный бенчмарк: ArenaStore vs TCMallocStore
//
// Запуск:
//   go test ./kvstore/internal/store/tcmalloc/ -run TestCompare -v -count=1
// ═══════════════════════════════════════════════════════════

func TestCompareStores(t *testing.T) {
	const (
		numWorkers = 12
		numOps     = 500_000
		valueSize  = 64
	)

	value := make([]byte, valueSize)
	for i := range value {
		value[i] = byte(i % 256)
	}

	t.Log("═══════════════════════════════════════════════════════")
	t.Logf("  Benchmark: ArenaStore vs TCMallocStore")
	t.Logf("  Workers: %d, Operations: %d, Value size: %dB", numWorkers, numOps, valueSize)
	t.Log("═══════════════════════════════════════════════════════")

	// ─── 1. ArenaStore SET (sequential) ───
	t.Log("\n--- ArenaStore SET (single goroutine) ---")
	arenaStore := store.NewArenaStore()
	start := time.Now()
	for i := 0; i < numOps; i++ {
		key := fmt.Sprintf("key:%d", i)
		arenaStore.Set(key, value)
	}
	arenaSetSeq := time.Since(start)
	arenaSetSeqRPS := float64(numOps) / arenaSetSeq.Seconds()
	t.Logf("ArenaStore SET sequential: %v (%.0f ops/sec)", arenaSetSeq, arenaSetSeqRPS)

	// ─── 2. TCMallocStore SET (sequential) ───
	t.Log("\n--- TCMallocStore SET (single goroutine) ---")
	tcStore := NewTCMallocStore(numWorkers)
	start = time.Now()
	for i := 0; i < numOps; i++ {
		key := fmt.Sprintf("key:%d", i)
		tcStore.Set(0, key, value)
	}
	tcSetSeq := time.Since(start)
	tcSetSeqRPS := float64(numOps) / tcSetSeq.Seconds()
	t.Logf("TCMallocStore SET sequential: %v (%.0f ops/sec)", tcSetSeq, tcSetSeqRPS)

	// ─── 3. ArenaStore SET (parallel, N workers) ───
	t.Log("\n--- ArenaStore SET (parallel, 12 workers) ---")
	arenaStore2 := store.NewArenaStore()
	start = time.Now()
	var wg sync.WaitGroup
	opsPerWorker := numOps / numWorkers
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			base := workerID * opsPerWorker
			for i := 0; i < opsPerWorker; i++ {
				key := fmt.Sprintf("key:%d", base+i)
				arenaStore2.Set(key, value)
			}
		}(w)
	}
	wg.Wait()
	arenaSetPar := time.Since(start)
	arenaSetParRPS := float64(numOps) / arenaSetPar.Seconds()
	t.Logf("ArenaStore SET parallel: %v (%.0f ops/sec)", arenaSetPar, arenaSetParRPS)

	// ─── 4. TCMallocStore SET (parallel, N workers) ───
	t.Log("\n--- TCMallocStore SET (parallel, 12 workers) ---")
	tcStore2 := NewTCMallocStore(numWorkers)
	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			base := workerID * opsPerWorker
			for i := 0; i < opsPerWorker; i++ {
				key := fmt.Sprintf("key:%d", base+i)
				tcStore2.Set(workerID, key, value)
			}
		}(w)
	}
	wg.Wait()
	tcSetPar := time.Since(start)
	tcSetParRPS := float64(numOps) / tcSetPar.Seconds()
	t.Logf("TCMallocStore SET parallel: %v (%.0f ops/sec)", tcSetPar, tcSetParRPS)

	// ─── 5. GET comparison ───
	t.Log("\n--- GET comparison (parallel, 12 workers, 500K reads) ---")

	// ArenaStore GET
	start = time.Now()
	var arenaHits atomic.Int64
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			base := workerID * opsPerWorker
			for i := 0; i < opsPerWorker; i++ {
				key := fmt.Sprintf("key:%d", base+i)
				if _, ok := arenaStore2.Get(key); ok {
					arenaHits.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	arenaGetPar := time.Since(start)
	arenaGetParRPS := float64(numOps) / arenaGetPar.Seconds()

	// TCMallocStore GET
	start = time.Now()
	var tcHits atomic.Int64
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			base := workerID * opsPerWorker
			for i := 0; i < opsPerWorker; i++ {
				key := fmt.Sprintf("key:%d", base+i)
				if _, ok := tcStore2.Get(key); ok {
					tcHits.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	tcGetPar := time.Since(start)
	tcGetParRPS := float64(numOps) / tcGetPar.Seconds()

	t.Logf("ArenaStore GET parallel:    %v (%.0f ops/sec, %d hits)", arenaGetPar, arenaGetParRPS, arenaHits.Load())
	t.Logf("TCMallocStore GET parallel: %v (%.0f ops/sec, %d hits)", tcGetPar, tcGetParRPS, tcHits.Load())

	// ─── 6. HOT KEY — главный тест contention ───
	t.Log("\n--- HOT KEY test (12 workers, all write to SAME shard) ---")

	arenaHot := store.NewArenaStore()
	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				// Все ключи попадают в один shard (hot spot!)
				key := fmt.Sprintf("hot:%d", i)
				arenaHot.Set(key, value)
			}
		}(w)
	}
	wg.Wait()
	arenaHotTime := time.Since(start)
	arenaHotRPS := float64(numOps) / arenaHotTime.Seconds()

	tcHot := NewTCMallocStore(numWorkers)
	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				key := fmt.Sprintf("hot:%d", i)
				tcHot.Set(workerID, key, value)
			}
		}(w)
	}
	wg.Wait()
	tcHotTime := time.Since(start)
	tcHotRPS := float64(numOps) / tcHotTime.Seconds()

	// ─── 7. Mixed load: 80% GET + 20% SET ───
	t.Log("\n--- Mixed load: 80%% GET + 20%% SET (parallel) ---")

	// Pre-fill both stores
	arenaMixed := store.NewArenaStore()
	tcMixed := NewTCMallocStore(numWorkers)
	for i := 0; i < 100_000; i++ {
		key := fmt.Sprintf("mix:%d", i)
		arenaMixed.Set(key, value)
		tcMixed.Set(0, key, value)
	}

	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID)))
			for i := 0; i < opsPerWorker; i++ {
				key := fmt.Sprintf("mix:%d", rng.Intn(100_000))
				if rng.Float32() < 0.8 {
					arenaMixed.Get(key)
				} else {
					arenaMixed.Set(key, value)
				}
			}
		}(w)
	}
	wg.Wait()
	arenaMixedTime := time.Since(start)
	arenaMixedRPS := float64(numOps) / arenaMixedTime.Seconds()

	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID)))
			for i := 0; i < opsPerWorker; i++ {
				key := fmt.Sprintf("mix:%d", rng.Intn(100_000))
				if rng.Float32() < 0.8 {
					tcMixed.Get(key)
				} else {
					tcMixed.Set(workerID, key, value)
				}
			}
		}(w)
	}
	wg.Wait()
	tcMixedTime := time.Since(start)
	tcMixedRPS := float64(numOps) / tcMixedTime.Seconds()

	// ─── Статистика TCMalloc ───
	stats := tcStore2.Stats()

	// ─── ИТОГИ ───
	t.Log("\n═══════════════════════════════════════════════════════════════")
	t.Log("  RESULTS")
	t.Log("═══════════════════════════════════════════════════════════════")
	t.Logf("  %-30s %12s %12s %8s", "Test", "ArenaStore", "TCMalloc", "Winner")
	t.Log("  ─────────────────────────────────────────────────────────────")
	t.Logf("  %-30s %10.0f/s %10.0f/s %8s", "SET sequential", arenaSetSeqRPS, tcSetSeqRPS, winner(arenaSetSeqRPS, tcSetSeqRPS))
	t.Logf("  %-30s %10.0f/s %10.0f/s %8s", "SET parallel (12 workers)", arenaSetParRPS, tcSetParRPS, winner(arenaSetParRPS, tcSetParRPS))
	t.Logf("  %-30s %10.0f/s %10.0f/s %8s", "GET parallel (12 workers)", arenaGetParRPS, tcGetParRPS, winner(arenaGetParRPS, tcGetParRPS))
	t.Logf("  %-30s %10.0f/s %10.0f/s %8s", "HOT KEY (contention test)", arenaHotRPS, tcHotRPS, winner(arenaHotRPS, tcHotRPS))
	t.Logf("  %-30s %10.0f/s %10.0f/s %8s", "Mixed 80/20 (12 workers)", arenaMixedRPS, tcMixedRPS, winner(arenaMixedRPS, tcMixedRPS))
	t.Log("  ─────────────────────────────────────────────────────────────")
	t.Logf("  TCMalloc stats: chunks=%v spans=%v allocs=%v refills=%v ratio=%.4f",
		stats["chunks"], stats["spans"], stats["total_allocs"], stats["total_refills"], stats["refill_ratio"])
	t.Log("═══════════════════════════════════════════════════════════════")
}

func winner(a, b float64) string {
	if a > b*1.05 {
		return "Arena"
	}
	if b > a*1.05 {
		return "TCMalloc"
	}
	return "≈ tied"
}
