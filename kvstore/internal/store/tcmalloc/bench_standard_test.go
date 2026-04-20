package tcmalloc

import (
	"strconv"
	"testing"
)

// Стандартные Go бенчмарки (Benchmark* функции).
// Go автоматически определяет сколько итераций нужно для точного замера.

func BenchmarkTCMalloc_Set(b *testing.B) {
	store := NewTCMallocStore(1)
	value := make([]byte, 64)
	buf := make([]byte, 32)

	b.ResetTimer() // сбрасываем таймер (init не считается)
	for i := 0; i < b.N; i++ {
		copy(buf, "k:")
		s := strconv.AppendInt(buf[:2], int64(i), 10)
		store.Set(0, string(s), value)
	}
}

func BenchmarkTCMalloc_AllocOnly(b *testing.B) {
	store := NewTCMallocStore(1)
	cache := store.caches[0]

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, _ := cache.Alloc(80)
		buf[0] = byte(i)
	}
}
