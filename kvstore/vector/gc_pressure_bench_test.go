package vector

// =============================================================================
// Эмпирическая проверка нагрузки на GC от строковых мап на горячем пути.
//
// Тезисы, которые проверяем замером (а не на бумаге):
//   A) frozen-путь интернирует ключи → GC-скан O(N) строковой мапы устранён.
//      Меряем стоимость ОДНОГО форсированного GC при N живых ключах:
//      map[string]uint64 vs internedKeys{blob,offs}+[]uint64.
//   C) tombstones COW-копируется на КАЖДОЕ удаление → суммарные аллокации
//      растут как O(D^2). Меряем bytes-total для разных D.
//   D) реальный LeveledVectorStore: число объектов кучи и NumGC после набивки.
//
// Запуск:
//   go test ./vector/ -run TestGCReport -v -timeout 600s
// =============================================================================

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// avgForcedGC возвращает среднее настенное время одного runtime.GC() за rounds
// итераций. Форсированный GC доминируется mark-фазой, которая сканирует
// указатели живой кучи — то есть стоимость прямо пропорциональна числу
// указателей в удерживаемых структурах.
func avgForcedGC(rounds int) time.Duration {
	runtime.GC() // прогрев / стабилизация
	start := time.Now()
	for i := 0; i < rounds; i++ {
		runtime.GC()
	}
	return time.Since(start) / time.Duration(rounds)
}

func heapObjects() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapObjects
}

func makeKeys(n int) []string {
	ks := make([]string, n)
	for i := 0; i < n; i++ {
		ks[i] = fmt.Sprintf("doc-%08d", i) // типичный RAG-ключ
	}
	return ks
}

// -----------------------------------------------------------------------------
// Сценарий A: GC-скан строковой мапы vs интернированного эквивалента
// -----------------------------------------------------------------------------
func TestGCReport_InterningVsStringMap(t *testing.T) {
	const rounds = 20
	sizes := []int{50_000, 200_000, 1_000_000}

	t.Logf("\n=== A. Стоимость 1×GC при N живых ключах: map[string]uint64 vs interned ===")
	t.Logf("%-10s | %-14s | %-14s | %-8s | %-16s | %-16s",
		"N", "map ns/GC", "interned ns/GC", "speedup", "map HeapObjs", "interned HeapObjs")

	for _, n := range sizes {
		// --- map[string]uint64 ---
		base := heapObjects()
		m := make(map[string]uint64, n)
		{
			keys := makeKeys(n)
			for i, k := range keys {
				m[k] = uint64(i)
			}
			keys = nil // отпускаем срез: бэкинги строк держит только мапа
		}
		runtime.GC()
		mapObjs := heapObjects() - base
		mapGC := avgForcedGC(rounds)
		runtime.KeepAlive(m)
		m = nil
		runtime.GC()

		// --- internedKeys{blob,offs} + []uint64 ---
		base2 := heapObjects()
		var ik internedKeys
		vals := make([]uint64, n)
		{
			keys := makeKeys(n)
			ik = buildInternedKeys(keys)
			for i := range keys {
				vals[i] = uint64(i)
			}
			keys = nil
		}
		runtime.GC()
		intObjs := heapObjects() - base2
		intGC := avgForcedGC(rounds)
		runtime.KeepAlive(ik)
		runtime.KeepAlive(vals)
		ik = internedKeys{}
		vals = nil
		runtime.GC()

		speedup := float64(mapGC) / float64(intGC)
		t.Logf("%-10d | %-14d | %-14d | %-7.2fx | %-16d | %-16d",
			n, mapGC.Nanoseconds(), intGC.Nanoseconds(), speedup, mapObjs, intObjs)
	}
	t.Logf("Вывод: interned держит ~3 объекта кучи независимо от N → mark-скан почти не зависит от N.")
}

// -----------------------------------------------------------------------------
// Сценарий C: churn аллокаций от COW-tombstones
// -----------------------------------------------------------------------------
func TestGCReport_TombstoneChurn(t *testing.T) {
	Ds := []int{1_000, 2_000, 4_000, 8_000, 16_000}

	t.Logf("\n=== C. COW-tombstones: суммарные аллокации на D удалений ===")
	t.Logf("%-8s | %-16s | %-16s | %-14s",
		"D", "bytes total", "bytes/delete", "mallocs total")

	var prevBytes float64
	for _, D := range Ds {
		keys := makeKeys(D)
		var p atomic.Pointer[map[string]struct{}]

		var m1, m2 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m1)
		for _, k := range keys {
			addTombstone(&p, k)
		}
		runtime.ReadMemStats(&m2)
		runtime.KeepAlive(&p)

		totalBytes := m2.TotalAlloc - m1.TotalAlloc
		totalMallocs := m2.Mallocs - m1.Mallocs
		perDelete := float64(totalBytes) / float64(D)

		growth := ""
		if prevBytes > 0 {
			growth = fmt.Sprintf("  (×%.2f при ×2 D)", float64(totalBytes)/prevBytes)
		}
		prevBytes = float64(totalBytes)

		t.Logf("%-8d | %-16d | %-16.0f | %-14d%s",
			D, totalBytes, perDelete, totalMallocs, growth)
	}
	t.Logf("Вывод: если bytes-total учетверяется при удвоении D → подтверждённый O(D^2) churn.")
}

// -----------------------------------------------------------------------------
// Сценарий D: реальный LeveledVectorStore — объекты кучи и GC после набивки
// -----------------------------------------------------------------------------
func TestGCReport_RealLeveledStore(t *testing.T) {
	if testing.Short() {
		t.Skip("skip в -short (строит реальный HNSW)")
	}
	const (
		n   = 100_000
		dim = 128
	)
	alloc := tcmalloc.NewTCMallocStore(1)
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: alloc,
		UseSQ:     true,
	})
	defer lvs.Close()

	vec := make([]float32, dim)
	rng := uint64(12345)
	nextF := func() float32 { // дешёвый детерминированный PRNG (Date/rand-агностично)
		rng ^= rng << 13
		rng ^= rng >> 7
		rng ^= rng << 17
		return float32(rng%1000) / 1000.0
	}

	var m0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	gc0 := m0.NumGC

	for i := 0; i < n; i++ {
		for j := range vec {
			vec[j] = nextF()
		}
		if err := lvs.Add(fmt.Sprintf("doc-%08d", i), vec); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	lvs.FlushDeltaSync()

	runtime.GC()
	objsAfter := heapObjects()
	gcSettle := avgForcedGC(10)

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	t.Logf("\n=== D. Реальный LeveledVectorStore: N=%d, dim=%d, UseSQ=true ===", n, dim)
	t.Logf("NumGC за набивку:        %d", m1.NumGC-gc0)
	t.Logf("HeapObjects после flush: %d  (= %.2f объектов/вектор)",
		objsAfter, float64(objsAfter)/float64(n))
	t.Logf("Время 1×GC (live set):   %v", gcSettle)
	t.Logf("HeapAlloc:               %.1f MB", float64(m1.HeapAlloc)/1e6)
	t.Logf("GCCPUFraction:           %.4f", m1.GCCPUFraction)
	t.Logf("Вывод: объектов/вектор ≪ 1 → frozen-ключи в blob, не N строковых объектов.")

	runtime.KeepAlive(lvs)
}
