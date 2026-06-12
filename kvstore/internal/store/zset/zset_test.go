package zset

import (
	"fmt"
	"math/rand"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

func newTestRegistry() (*ZSetRegistry, *tcmalloc.TCMallocStore) {
	store := tcmalloc.NewTCMallocStore(1)
	reg := New(store)
	return reg, store
}

// ── Функциональные тесты ────────────────────────────────────

func TestZAddZScore(t *testing.T) {
	reg, _ := newTestRegistry()

	// Добавляем member
	isNew := reg.ZAdd(0, "prices", 9.99, "apple")
	if !isNew {
		t.Fatal("expected new member")
	}

	// Проверяем score через обратный индекс
	score, ok := reg.ZScore("prices", "apple")
	if !ok || score != 9.99 {
		t.Fatalf("ZScore = (%v, %v), want (9.99, true)", score, ok)
	}

	// Обновляем score
	isNew = reg.ZAdd(0, "prices", 19.99, "apple")
	if isNew {
		t.Fatal("expected update, not new")
	}

	score, ok = reg.ZScore("prices", "apple")
	if !ok || score != 19.99 {
		t.Fatalf("ZScore after update = (%v, %v), want (19.99, true)", score, ok)
	}
}

func TestZRem(t *testing.T) {
	reg, _ := newTestRegistry()

	reg.ZAdd(0, "prices", 9.99, "apple")
	reg.ZAdd(0, "prices", 19.99, "banana")

	ok := reg.ZRem(0, "prices", "apple")
	if !ok {
		t.Fatal("ZRem should return true")
	}

	_, ok = reg.ZScore("prices", "apple")
	if ok {
		t.Fatal("apple should be gone")
	}

	if reg.ZCard("prices") != 1 {
		t.Fatalf("ZCard = %d, want 1", reg.ZCard("prices"))
	}
}

func TestZRangeByScore(t *testing.T) {
	reg, _ := newTestRegistry()

	reg.ZAdd(0, "prices", 5.0, "cheap")
	reg.ZAdd(0, "prices", 50.0, "mid")
	reg.ZAdd(0, "prices", 500.0, "expensive")
	reg.ZAdd(0, "prices", 5000.0, "luxury")

	results := reg.ZRangeByScore("prices", 10, 1000)
	if len(results) != 2 {
		t.Fatalf("ZRangeByScore(10,1000) = %d results, want 2", len(results))
	}
	if results[0].Member != "mid" || results[1].Member != "expensive" {
		t.Fatalf("unexpected results: %v", results)
	}
}

func TestMembersInRange(t *testing.T) {
	reg, _ := newTestRegistry()

	for i := 0; i < 1000; i++ {
		reg.ZAdd(0, "prices", float64(i), fmt.Sprintf("item:%d", i))
	}

	allowed := reg.MembersInRange("prices", 100, 200)
	if len(allowed) != 101 { // 100..200 inclusive
		t.Fatalf("MembersInRange(100,200) = %d, want 101", len(allowed))
	}
}

func TestDuplicateScoresInZSet(t *testing.T) {
	reg, _ := newTestRegistry()

	reg.ZAdd(0, "prices", 9.99, "apple")
	reg.ZAdd(0, "prices", 9.99, "banana")
	reg.ZAdd(0, "prices", 9.99, "cherry")

	if reg.ZCard("prices") != 3 {
		t.Fatalf("ZCard = %d, want 3", reg.ZCard("prices"))
	}

	results := reg.ZRangeByScore("prices", 9.99, 9.99)
	if len(results) != 3 {
		t.Fatalf("ZRangeByScore(9.99,9.99) = %d results, want 3", len(results))
	}
}

func TestMultipleSortedSets(t *testing.T) {
	reg, _ := newTestRegistry()

	reg.ZAdd(0, "prices", 9.99, "apple")
	reg.ZAdd(0, "ratings", 4.5, "apple")

	score, ok := reg.ZScore("prices", "apple")
	if !ok || score != 9.99 {
		t.Fatal("prices:apple should be 9.99")
	}

	score, ok = reg.ZScore("ratings", "apple")
	if !ok || score != 4.5 {
		t.Fatal("ratings:apple should be 4.5")
	}

	if reg.ZCard("prices") != 1 || reg.ZCard("ratings") != 1 {
		t.Fatal("each set should have 1 element")
	}
}

// ── Бенчмарки ──────────────────────────────────────────────

func BenchmarkZAdd(b *testing.B) {
	reg, _ := newTestRegistry()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reg.ZAdd(0, "bench", float64(i), fmt.Sprintf("item:%d", i))
	}
}

func BenchmarkZScore(b *testing.B) {
	reg, _ := newTestRegistry()
	for i := 0; i < 100000; i++ {
		reg.ZAdd(0, "bench", float64(i), fmt.Sprintf("item:%d", i))
	}
	keys := make([]string, 100000)
	for i := range keys {
		keys[i] = fmt.Sprintf("item:%d", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reg.ZScore("bench", keys[i%100000])
	}
}

func BenchmarkZRangeByScore(b *testing.B) {
	reg, _ := newTestRegistry()
	for i := 0; i < 100000; i++ {
		reg.ZAdd(0, "bench", float64(i), fmt.Sprintf("item:%d", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := float64(i % 99000)
		reg.ZRangeByScore("bench", start, start+100)
	}
}

func BenchmarkMembersInRange(b *testing.B) {
	reg, _ := newTestRegistry()
	for i := 0; i < 100000; i++ {
		reg.ZAdd(0, "bench", float64(i), fmt.Sprintf("item:%d", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := float64(i % 99000)
		reg.MembersInRange("bench", start, start+100)
	}
}

// BenchmarkMembersInRangeParallel — параллельное чтение ranges.
// Имитирует нагрузку VSIM.SEARCHRANGE: множество reader'ов вычисляют allowed set.
func BenchmarkMembersInRangeParallel(b *testing.B) {
	reg, _ := newTestRegistry()
	for i := 0; i < 100000; i++ {
		reg.ZAdd(0, "bench", float64(i), fmt.Sprintf("item:%d", i))
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(42))
		for pb.Next() {
			start := float64(rng.Intn(99000))
			reg.MembersInRange("bench", start, start+100)
		}
	})
}

// BenchmarkZScoreParallel — параллельные lock-free ZSCORE.
func BenchmarkZScoreParallel(b *testing.B) {
	reg, _ := newTestRegistry()
	for i := 0; i < 100000; i++ {
		reg.ZAdd(0, "bench", float64(i), fmt.Sprintf("item:%d", i))
	}
	keys := make([]string, 100000)
	for i := range keys {
		keys[i] = fmt.Sprintf("item:%d", i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			reg.ZScore("bench", keys[i%100000])
			i++
		}
	})
}
