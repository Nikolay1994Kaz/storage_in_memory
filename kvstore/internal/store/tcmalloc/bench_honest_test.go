package tcmalloc

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"kvstore/kvstore/internal/store"
)

// ═══════════════════════════════════════════════════════════
// Честный бенчмарк: без fmt.Sprintf, с разным contention
// ═══════════════════════════════════════════════════════════

// cheapKey генерирует ключ БЕЗ аллокаций (в отличие от fmt.Sprintf)
func cheapKey(buf []byte, prefix string, n int) string {
	copy(buf, prefix)
	s := strconv.AppendInt(buf[:len(prefix)], int64(n), 10)
	return *(*string)(unsafe.Pointer(&s)) // zero-copy string (жизнь = жизнь buf)
}

func TestHonestBenchmark(t *testing.T) {
	const (
		numWorkers = 12
		numOps     = 2_000_000
		valueSize  = 64
	)

	value := make([]byte, valueSize)
	opsPerWorker := numOps / numWorkers

	t.Log("═══════════════════════════════════════════════════════")
	t.Logf("  HONEST Benchmark (no fmt.Sprintf overhead)")
	t.Logf("  Workers: %d, Ops: %d, Value: %dB", numWorkers, numOps, valueSize)
	t.Log("═══════════════════════════════════════════════════════")

	// ─── Test 1: Pure allocation speed (single worker, no contention) ───
	t.Log("\n--- Test 1: Pure SET speed (single worker) ---")

	arena1 := store.NewArenaStore()
	buf1 := make([]byte, 32)
	start := time.Now()
	for i := 0; i < numOps; i++ {
		copy(buf1, "k:")
		s := strconv.AppendInt(buf1[:2], int64(i), 10)
		arena1.Set(string(s), value)
	}
	arenaSeq := time.Since(start)

	tc1 := NewTCMallocStore(numWorkers)
	start = time.Now()
	for i := 0; i < numOps; i++ {
		copy(buf1, "k:")
		s := strconv.AppendInt(buf1[:2], int64(i), 10)
		tc1.Set(0, string(s), value)
	}
	tcSeq := time.Since(start)

	t.Logf("  ArenaStore:    %v (%10.0f ops/sec)", arenaSeq, float64(numOps)/arenaSeq.Seconds())
	t.Logf("  TCMallocStore: %v (%10.0f ops/sec)", tcSeq, float64(numOps)/tcSeq.Seconds())

	// ─── Test 2: Parallel SET (12 workers, distributed keys) ───
	t.Log("\n--- Test 2: Parallel SET (12 workers, distributed keys) ---")

	arena2 := store.NewArenaStore()
	var wg sync.WaitGroup
	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			buf := make([]byte, 32)
			base := wid * opsPerWorker
			for i := 0; i < opsPerWorker; i++ {
				copy(buf, "k:")
				s := strconv.AppendInt(buf[:2], int64(base+i), 10)
				arena2.Set(string(s), value)
			}
		}(w)
	}
	wg.Wait()
	arenaPar := time.Since(start)

	tc2 := NewTCMallocStore(numWorkers)
	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			buf := make([]byte, 32)
			base := wid * opsPerWorker
			for i := 0; i < opsPerWorker; i++ {
				copy(buf, "k:")
				s := strconv.AppendInt(buf[:2], int64(base+i), 10)
				tc2.Set(wid, string(s), value)
			}
		}(w)
	}
	wg.Wait()
	tcPar := time.Since(start)

	t.Logf("  ArenaStore:    %v (%10.0f ops/sec)", arenaPar, float64(numOps)/arenaPar.Seconds())
	t.Logf("  TCMallocStore: %v (%10.0f ops/sec)", tcPar, float64(numOps)/tcPar.Seconds())

	// ─── Test 3: TRUE Hot Spot (all workers write to SAME 4 keys) ───
	t.Log("\n--- Test 3: TRUE Hot Spot (12 workers × 4 keys = MAX contention) ---")

	arena3 := store.NewArenaStore()
	hotKeys := []string{"hot:0", "hot:1", "hot:2", "hot:3"}
	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				arena3.Set(hotKeys[i%4], value)
			}
		}(w)
	}
	wg.Wait()
	arenaHot := time.Since(start)

	tc3 := NewTCMallocStore(numWorkers)
	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				tc3.Set(wid, hotKeys[i%4], value)
			}
		}(w)
	}
	wg.Wait()
	tcHot := time.Since(start)

	t.Logf("  ArenaStore:    %v (%10.0f ops/sec)", arenaHot, float64(numOps)/arenaHot.Seconds())
	t.Logf("  TCMallocStore: %v (%10.0f ops/sec)", tcHot, float64(numOps)/tcHot.Seconds())

	// ─── Test 4: Alloc-only benchmark (измеряем ЧИСТУЮ аллокацию) ───
	t.Log("\n--- Test 4: Pure ALLOC speed (no index, no hash, just memory) ---")

	var totalAlloc atomic.Int64

	start = time.Now()
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			cache := tc2.caches[wid]
			for i := 0; i < opsPerWorker; i++ {
				buf, _ := cache.Alloc(80) // 80 bytes → size class 128B
				buf[0] = byte(i)          // touch memory
				totalAlloc.Add(1)
			}
		}(w)
	}
	wg.Wait()
	tcAllocOnly := time.Since(start)

	t.Logf("  TCMalloc Alloc-only: %v (%10.0f ops/sec) ← THIS is the real speed",
		tcAllocOnly, float64(numOps)/tcAllocOnly.Seconds())
	t.Logf("  That's %.1f ns/alloc across %d workers", float64(tcAllocOnly.Nanoseconds())/float64(numOps), numWorkers)

	// ─── ИТОГИ ───
	t.Log("\n═══════════════════════════════════════════════════════════")
	t.Logf("  %-35s %12s %12s", "Test", "ArenaStore", "TCMalloc")
	t.Log("  ─────────────────────────────────────────────────────────")
	t.Logf("  %-35s %10.0f/s %10.0f/s", "SET sequential (1 worker)",
		float64(numOps)/arenaSeq.Seconds(), float64(numOps)/tcSeq.Seconds())
	t.Logf("  %-35s %10.0f/s %10.0f/s", "SET parallel (12 workers)",
		float64(numOps)/arenaPar.Seconds(), float64(numOps)/tcPar.Seconds())
	t.Logf("  %-35s %10.0f/s %10.0f/s", "HOT SPOT (4 keys, 12 workers)",
		float64(numOps)/arenaHot.Seconds(), float64(numOps)/tcHot.Seconds())
	t.Logf("  %-35s %12s %10.0f/s", "Pure ALLOC (no index)",
		"N/A", float64(numOps)/tcAllocOnly.Seconds())
	t.Log("═══════════════════════════════════════════════════════════")
}
