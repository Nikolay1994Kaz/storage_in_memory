package store

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestGCPause(t *testing.T) {
	counts := []int{100_000, 1_000_000, 5_000_000}
	for _, count := range counts {
		t.Run(fmt.Sprintf("keys_%d", count), func(t *testing.T) {
			s := NewShardedStore()
			value := make([]byte, 100) // 100 байт на значение
			// Загружаем данные
			for i := 0; i < count; i++ {
				key := fmt.Sprintf("key_%d", i)
				s.Set(key, value)
			}
			// Принудительно запускаем GC и замеряем время
			runtime.GC() // прогреваем
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
