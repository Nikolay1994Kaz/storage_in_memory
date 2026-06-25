package vector

// =============================================================================
// Stress-тест на 500k векторов
//
// Запуск:
//   go test ./kvstore/vector/ -run TestLeveledStore_500k -v -timeout 600s
//   go test ./kvstore/vector/ -run TestLeveledStore_500k_NoDeadlock -v -timeout 120s
// =============================================================================

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLeveledStore_500k — основной stress-тест.
// Вставляет 500k векторов из 12 горутин, затем синхронизирует через
// FlushDeltaSync и проверяет self-recall и QPS.
func TestLeveledStore_500k(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping 500k stress test in short mode (run without -short for full test)")
	}
	const (
		N        = 500_000
		dim      = 128
		K        = 10
		workers  = 12
		deltaMax = 10_000
	)

	lvs := NewLeveledVectorStore(LeveledConfig{
		// DeltaMax=0: авто-расчёт (теперь по умолчанию 50_000).
		// 500k / 50k = 10 L0 сегментов. Меньше сегментов → быстрее search (Worker Pool).
		DeltaMax:       0,
		EfConstruction: 200,
		M:              32,
		EfSearch:       100,
		Distance:       EuclideanDistance,
		// 12 билдеров = NumCPU на этом железе; полностью загружает 12 ядер.
		NumBuilders:    12,
		Fanout:         4,
		UseSQ:          true,
	})
	defer lvs.Close()

	// ── 1. Параллельная вставка ───────────────────────────────────────────────
	vecs := makeRandVecs(N, dim, 42)

	t.Logf("Inserting %dk vectors with %d workers (deltaMax=%d)...", N/1000, workers, deltaMax)
	start := time.Now()

	var inserted atomic.Int64
	var wg sync.WaitGroup
	chunkSize := N / workers

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			lo := workerID * chunkSize
			hi := lo + chunkSize
			if workerID == workers-1 {
				hi = N
			}
			for i := lo; i < hi; i++ {
				if err := lvs.Add(fmt.Sprintf("key-%d", i), vecs[i]); err != nil {
					t.Errorf("worker %d: Add(%d) failed: %v", workerID, i, err)
					return
				}
				inserted.Add(1)
			}
		}(w)
	}

	// Прогресс каждые 5 секунд
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				ins := inserted.Load()
				elapsed := time.Since(start).Seconds()
				rate := float64(ins) / elapsed
				stats := lvs.Stats()
				t.Logf("  progress: %d/%d (%.0f vec/s) | segments=%v delta=%d",
					ins, N, rate, stats.SegmentsByLevel, stats.DeltaLen)
			}
		}
	}()

	wg.Wait()
	close(progressDone)

	insertDuration := time.Since(start)
	insertRate := float64(N) / insertDuration.Seconds()
	t.Logf("Insert done: %d vectors in %.1fs → %.0f vec/s", N, insertDuration.Seconds(), insertRate)

	if insertRate < 1_000 {
		t.Errorf("Insert rate too low: %.0f vec/s (expect ≥ 1000)", insertRate)
	}

	// ── 2. FlushDeltaSync — должен вернуться без deadlock ────────────────────
	t.Log("Waiting for FlushDeltaSync...")
	syncStart := time.Now()

	flushDone := make(chan struct{})
	go func() {
		lvs.FlushDeltaSync()
		close(flushDone)
	}()

	select {
	case <-flushDone:
		t.Logf("FlushDeltaSync completed in %.1fs", time.Since(syncStart).Seconds())
	case <-time.After(300 * time.Second):
		stats := lvs.Stats()
		t.Fatalf("DEADLOCK: FlushDeltaSync did not return in 300s | delta=%d segments=%v",
			stats.DeltaLen, stats.SegmentsByLevel)
	}

	// ── 3. Проверка количества векторов ──────────────────────────────────────
	stats := lvs.Stats()
	t.Logf("Post-flush stats: total=%d delta=%d segments=%v dataBytes=%.1fMB",
		stats.TotalVectors, stats.DeltaLen, stats.SegmentsByLevel,
		float64(stats.DataBytes)/1e6)

	// После FlushDeltaSync дельта должна быть пуста
	if stats.DeltaLen > 0 {
		t.Errorf("DeltaLen=%d after FlushDeltaSync (expected 0)", stats.DeltaLen)
	}
	if stats.TotalVectors < N {
		t.Errorf("Data loss: got %d vectors, expected %d", stats.TotalVectors, N)
	}

	// ── 4. Self-recall@10: каждый вектор должен находить сам себя ────────────
	t.Log("Measuring self-recall@10...")
	rng := rand.New(rand.NewSource(777))
	nQueries := 200
	hits := 0

	indices := rng.Perm(N)[:nQueries]
	for _, idx := range indices {
		q := vecs[idx]
		key := fmt.Sprintf("key-%d", idx)

		results, err := lvs.Search(q, K, nil)
		if err != nil {
			t.Errorf("Search failed: %v", err)
			continue
		}
		for _, r := range results {
			if r.Key == key {
				hits++
				break
			}
		}
	}

	selfRecall := float64(hits) / float64(nQueries) * 100
	t.Logf("Self-recall@10: %.1f%% (%d/%d)", selfRecall, hits, nQueries)

	if selfRecall < 80.0 {
		t.Errorf("Self-recall too low: %.1f%% < 80%%", selfRecall)
	}

	// ── 4.5. Ожидание background merges (L0→L1→L2) ───────────────────────────
	// FlushDeltaSync завершается когда delta-flushes готовы, но cascade merges
	// продолжаются в фоне. Ждём пока число L0 сегментов стабилизируется ( merges
	// уменьшают L0, увеличивая L1/L2). Проверяем каждые 10s, выходим когда
	// L0 ≤ fanout (нет переполнения для merge).
	t.Log("Waiting for background merges (L0→L1→L2)...")
	mergeDeadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(mergeDeadline) {
		s := lvs.Stats()
		t.Logf("  merge progress: L0=%d L1=%d L2=%d segments=%v",
			s.SegmentsByLevel[0], s.SegmentsByLevel[1], s.SegmentsByLevel[2], s.SegmentsByLevel)
		// L0 < fanout значит нет переполнения → merges закончены (или не запустятся).
		if s.SegmentsByLevel[0] < 4 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	finalStats := lvs.Stats()
	t.Logf("Merge complete: segments=%v total=%d dataBytes=%.1fMB",
		finalStats.SegmentsByLevel, finalStats.TotalVectors, float64(finalStats.DataBytes)/1e6)

	// ── 5. QPS — поиск в 12 горутин 5 секунд ─────────────────────────────────
	t.Log("Measuring search QPS (5s)...")
	qpsQueries := makeRandVecs(200, dim, 888)
	var qpsCount atomic.Int64
	qpsDone := make(chan struct{})

	qpsStart := time.Now()
	for w := 0; w < workers; w++ {
		go func(id int) {
			i := id
			for {
				select {
				case <-qpsDone:
					return
				default:
					q := qpsQueries[i%len(qpsQueries)]
					_, _ = lvs.Search(q, K, nil)
					qpsCount.Add(1)
					i++
				}
			}
		}(w)
	}

	time.Sleep(5 * time.Second)
	close(qpsDone)
	qps := float64(qpsCount.Load()) / time.Since(qpsStart).Seconds()

	// ── Summary ───────────────────────────────────────────────────────────────
	t.Logf("=== SUMMARY 500k vectors dim=%d ===", dim)
	t.Logf("  Insert:        %.0f vec/s (%.1fs total)", insertRate, insertDuration.Seconds())
	t.Logf("  FlushDeltaSync: %.1fs", time.Since(syncStart).Seconds())
	t.Logf("  Self-Recall@10: %.1f%%", selfRecall)
	t.Logf("  QPS (%d workers): %.0f", workers, qps)
}

// TestLeveledStore_500k_NoDeadlock — быстрая проверка только на отсутствие
// deadlock при 100k векторах. Завершается за ~30-60s.
func TestLeveledStore_500k_NoDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping 500k NoDeadlock test in short mode")
	}
	const (
		N       = 100_000
		dim     = 128
		workers = 8
	)

	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax: 10_000,
		// М и EfConstruction намеренно низкие: тест проверяет отсутствие
		// deadlock (pipeline-корректность), а НЕ качество поиска.
		// M=32/EfC=200 занимают 20s+ под race detector — 60s timeout мал.
		// M=8/EfC=50 занимают ~1-2s под race: быстро и детерминированно.
		EfConstruction: 50,
		M:              8,
		EfSearch:       50,
		Distance:       EuclideanDistance,
		NumBuilders:    6,
		Fanout:         4,
		UseSQ:          true,
	})
	defer lvs.Close()

	vecs := makeRandVecs(N, dim, 1)

	var wg sync.WaitGroup
	chunkSize := N / workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			lo := id * chunkSize
			hi := lo + chunkSize
			if id == workers-1 {
				hi = N
			}
			for i := lo; i < hi; i++ {
				_ = lvs.Add(fmt.Sprintf("k%d", i), vecs[i])
			}
		}(w)
	}

	start := time.Now()
	wg.Wait()
	t.Logf("Inserted %d vectors in %.1fs", N, time.Since(start).Seconds())

	// FlushDeltaSync must NOT deadlock
	done := make(chan struct{})
	go func() {
		lvs.FlushDeltaSync()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("FlushDeltaSync OK (%.1fs total)", time.Since(start).Seconds())
	case <-time.After(60 * time.Second):
		stats := lvs.Stats()
		t.Fatalf("DEADLOCK: FlushDeltaSync hung 60s | delta=%d segs=%v",
			stats.DeltaLen, stats.SegmentsByLevel)
	}

	stats := lvs.Stats()
	if stats.DeltaLen > 0 {
		t.Errorf("Delta not empty after flush: %d vectors", stats.DeltaLen)
	}
	if stats.TotalVectors < N {
		t.Errorf("Data loss: got %d vectors, expected %d", stats.TotalVectors, N)
	}
	t.Logf("After flush: total=%d/%d segments=%v", stats.TotalVectors, N, stats.SegmentsByLevel)
}
