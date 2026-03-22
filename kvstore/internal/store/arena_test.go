package store

import (
	"fmt"
	"math/rand"
	"runtime"
	"testing"
	"time"
)

func TestGCPause_Arena(t *testing.T) {
	counts := []int{100_000, 1_000_000, 5_000_000}

	for _, count := range counts {
		t.Run(fmt.Sprintf("keys_%d", count), func(t *testing.T) {
			// Арена на 1GB (с запасом)
			s := NewArenaStore()
			value := make([]byte, 100)

			for i := 0; i < count; i++ {
				key := fmt.Sprintf("key_%d", i)
				s.Set(key, value)
			}

			runtime.GC()
			var stats runtime.MemStats

			start := time.Now()
			runtime.GC()
			gcDuration := time.Since(start)

			runtime.ReadMemStats(&stats)

			t.Logf("Keys: %d", count)
			t.Logf("GC Pause: %v", gcDuration)
			t.Logf("Heap Alloc: %.1f MB", float64(stats.HeapAlloc)/(1024*1024))
			t.Logf("Num GC: %d", stats.NumGC)
			t.Logf("Last GC Pause: %v", time.Duration(stats.PauseNs[(stats.NumGC+255)%256]))
			runtime.KeepAlive(s)
		})
	}
}

// BenchmarkArenaStore_Mixed — бенчмарк для сравнения с Naive и Sharded.
func BenchmarkArenaStore_Mixed(b *testing.B) {
	s := NewArenaStore()
	keys := generateKeys(10000)
	value := []byte("some_value_here")

	for _, k := range keys {
		s.Set(k, value)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		localRand := rand.New(rand.NewSource(rand.Int63()))
		for pb.Next() {
			key := keys[localRand.Intn(len(keys))]
			if localRand.Intn(100) < 80 {
				s.Get(key)
			} else {
				s.Set(key, value)
			}
		}
	})
}
