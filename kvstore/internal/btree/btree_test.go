package btree

import (
	"fmt"
	"math/rand"
	"testing"
	"unsafe"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// newTestTree — хелпер: создаёт TCMallocStore + B+Tree для тестов.
func newTestTree() (*BPTree, *tcmalloc.TCMallocStore) {
	store := tcmalloc.NewTCMallocStore(1)
	tree := New(store, 0)
	return tree, store
}

// ── Структура node ──────────────────────────────────────────

func TestNodeSize(t *testing.T) {
	t.Logf("sizeof(item) = %d bytes", unsafe.Sizeof(item{}))
	t.Logf("sizeof(node) = %d bytes", unsafe.Sizeof(node{}))
	t.Logf("nodeSize var = %d bytes", nodeSize)

	// node должен быть чисто числовым — проверяем что размер фиксирован
	if nodeSize == 0 {
		t.Fatal("nodeSize is 0")
	}
}

// ── Базовые операции ────────────────────────────────────────

func TestInsertAndSearch(t *testing.T) {
	tree, _ := newTestTree()

	tree.Insert(50, "fifty")
	tree.Insert(30, "thirty")
	tree.Insert(70, "seventy")
	tree.Insert(10, "ten")
	tree.Insert(40, "forty")

	tests := []struct {
		score float64
		want  string
		ok    bool
	}{
		{50, "fifty", true},
		{30, "thirty", true},
		{10, "ten", true},
		{99, "", false},
	}

	for _, tt := range tests {
		got, ok := tree.Search(tt.score)
		if ok != tt.ok || got != tt.want {
			t.Errorf("Search(%v) = (%q, %v), want (%q, %v)",
				tt.score, got, ok, tt.want, tt.ok)
		}
	}
}

func TestInsertDuplicate(t *testing.T) {
	tree, _ := newTestTree()

	tree.Insert(10, "old")
	tree.Insert(10, "new")

	got, _ := tree.Search(10)
	if got != "new" {
		t.Fatalf("got %q, want %q", got, "new")
	}
	if tree.Len() != 1 {
		t.Fatalf("Len = %d, want 1", tree.Len())
	}
}

// ── Split (массовая вставка) ────────────────────────────────

func TestSplit(t *testing.T) {
	tree, _ := newTestTree()

	for i := 0; i < 1000; i++ {
		tree.Insert(float64(i), fmt.Sprintf("val-%d", i))
	}
	if tree.Len() != 1000 {
		t.Fatalf("Len = %d, want 1000", tree.Len())
	}
	for i := 0; i < 1000; i++ {
		val, ok := tree.Search(float64(i))
		if !ok || val != fmt.Sprintf("val-%d", i) {
			t.Fatalf("Search(%d) = (%q, %v)", i, val, ok)
		}
	}
}

func TestSplitReverse(t *testing.T) {
	tree, _ := newTestTree()

	for i := 999; i >= 0; i-- {
		tree.Insert(float64(i), fmt.Sprintf("val-%d", i))
	}
	if tree.Len() != 1000 {
		t.Fatalf("Len = %d, want 1000", tree.Len())
	}
	for i := 0; i < 1000; i++ {
		val, ok := tree.Search(float64(i))
		if !ok || val != fmt.Sprintf("val-%d", i) {
			t.Fatalf("Search(%d) = (%q, %v)", i, val, ok)
		}
	}
}

// ── Delete ──────────────────────────────────────────────────

func TestDelete(t *testing.T) {
	tree, _ := newTestTree()

	for i := 0; i < 100; i++ {
		tree.Insert(float64(i), fmt.Sprintf("v%d", i))
	}
	if !tree.Delete(50) {
		t.Fatal("Delete(50) should return true")
	}
	if _, ok := tree.Search(50); ok {
		t.Fatal("score 50 should be gone")
	}
	if tree.Len() != 99 {
		t.Fatalf("Len = %d, want 99", tree.Len())
	}
	if tree.Delete(999) {
		t.Fatal("Delete(999) should return false")
	}
}

func TestDeleteAll(t *testing.T) {
	tree, _ := newTestTree()

	for i := 0; i < 100; i++ {
		tree.Insert(float64(i), fmt.Sprintf("v%d", i))
	}
	for i := 0; i < 100; i++ {
		if !tree.Delete(float64(i)) {
			t.Fatalf("Delete(%d) should return true", i)
		}
	}
	if tree.Len() != 0 {
		t.Fatalf("Len = %d, want 0", tree.Len())
	}
}

// ── RangeSearch ─────────────────────────────────────────────

func TestRangeSearch(t *testing.T) {
	tree, _ := newTestTree()

	for i := 0; i < 100; i++ {
		tree.Insert(float64(i), fmt.Sprintf("v%d", i))
	}
	results := tree.RangeSearch(25, 30)
	if len(results) != 6 { // 25,26,27,28,29,30
		t.Fatalf("RangeSearch(25,30): got %d results, want 6", len(results))
	}
	for i, r := range results {
		expected := float64(25 + i)
		if r.Score != expected {
			t.Fatalf("result[%d].Score = %v, want %v", i, r.Score, expected)
		}
	}
}

func TestRangeSearchEmpty(t *testing.T) {
	tree, _ := newTestTree()

	for i := 0; i < 10; i++ {
		tree.Insert(float64(i), "v")
	}
	results := tree.RangeSearch(100, 200)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

// ── Min / Max ───────────────────────────────────────────────

func TestMinMax(t *testing.T) {
	tree, _ := newTestTree()

	tree.Insert(50, "a")
	tree.Insert(10, "b")
	tree.Insert(90, "c")

	minK, _, _ := tree.Min()
	maxK, _, _ := tree.Max()
	if minK != 10 {
		t.Fatalf("Min = %v, want 10", minK)
	}
	if maxK != 90 {
		t.Fatalf("Max = %v, want 90", maxK)
	}
}

func TestMinMaxEmpty(t *testing.T) {
	tree, _ := newTestTree()

	_, _, ok := tree.Min()
	if ok {
		t.Fatal("Min on empty tree should return false")
	}
	_, _, ok = tree.Max()
	if ok {
		t.Fatal("Max on empty tree should return false")
	}
}

// ── ForEach ─────────────────────────────────────────────────

func TestForEach(t *testing.T) {
	tree, _ := newTestTree()

	tree.Insert(30, "c")
	tree.Insert(10, "a")
	tree.Insert(20, "b")

	var scores []float64
	tree.ForEach(func(score float64, member string) {
		scores = append(scores, score)
	})
	if len(scores) != 3 || scores[0] != 10 || scores[1] != 20 || scores[2] != 30 {
		t.Fatalf("ForEach order: %v, want [10 20 30]", scores)
	}
}

// ── Random stress test ──────────────────────────────────────

func TestRandomInsertSearch(t *testing.T) {
	tree, _ := newTestTree()
	m := make(map[float64]string)

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 10000; i++ {
		score := float64(rng.Intn(5000))
		val := fmt.Sprintf("v%d", i)
		tree.Insert(score, val)
		m[score] = val
	}
	if tree.Len() != len(m) {
		t.Fatalf("Len = %d, want %d", tree.Len(), len(m))
	}
	for score, val := range m {
		got, ok := tree.Search(score)
		if !ok || got != val {
			t.Fatalf("Search(%v) = (%q, %v), want (%q, true)", score, got, ok, val)
		}
	}
}

// ── Memory accounting ───────────────────────────────────────

func TestMemoryAccounting(t *testing.T) {
	store := tcmalloc.NewTCMallocStore(1)
	before := store.UsedMemory()

	tree := New(store, 0)
	afterCreate := store.UsedMemory()

	if afterCreate <= before {
		t.Fatalf("creating tree should allocate memory: before=%d, after=%d",
			before, afterCreate)
	}
	t.Logf("empty tree: %d bytes allocated", afterCreate-before)

	for i := 0; i < 1000; i++ {
		tree.Insert(float64(i), fmt.Sprintf("member-%d", i))
	}
	afterInsert := store.UsedMemory()
	t.Logf("1000 inserts: %d bytes total (%d bytes for tree data)",
		afterInsert-before, afterInsert-afterCreate)

	// IsOOM должен видеть память B+Tree
	store.SetMaxMemory(afterInsert + 1)
	if store.IsOOM() {
		t.Fatal("should not be OOM yet")
	}
	store.SetMaxMemory(1) // лимит 1 байт
	if !store.IsOOM() {
		t.Fatal("should be OOM now")
	}
}

// ── Benchmarks ──────────────────────────────────────────────

func BenchmarkInsert(b *testing.B) {
	tree, _ := newTestTree()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tree.Insert(float64(i), "v")
	}
}

func BenchmarkSearch(b *testing.B) {
	tree, _ := newTestTree()
	for i := 0; i < 100000; i++ {
		tree.Insert(float64(i), "v")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tree.Search(float64(i % 100000))
	}
}

func BenchmarkRangeSearch(b *testing.B) {
	tree, _ := newTestTree()
	for i := 0; i < 100000; i++ {
		tree.Insert(float64(i), "v")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tree.RangeSearch(float64(i%99000), float64(i%99000+100))
	}
}

// ── Concurrent Benchmarks (seqlock) ─────────────────────────

// BenchmarkSearchParallel — параллельные lock-free Search'ы.
// С RWMutex: reader'ы конкурируют через RLock atomic counter.
// С seqlock: reader'ы полностью независимы, zero contention.
func BenchmarkSearchParallel(b *testing.B) {
	tree, _ := newTestTree()
	for i := 0; i < 100000; i++ {
		tree.Insert(float64(i), "v")
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			tree.Search(float64(i % 100000))
			i++
		}
	})
}

// BenchmarkSearchParallelWithWrites — 90% read / 10% write mix.
// Показывает эффект lock-free Search при конкурентных Insert/Delete.
// Writer'ы берут mu.Lock, но reader'ы (Search) НЕ блокируются.
func BenchmarkSearchParallelWithWrites(b *testing.B) {
	tree, _ := newTestTree()
	for i := 0; i < 100000; i++ {
		tree.Insert(float64(i), "v")
	}

	// Фоновый writer: Insert + Delete в цикле
	stopCh := make(chan struct{})
	go func() {
		j := 200000
		for {
			select {
			case <-stopCh:
				return
			default:
				tree.Insert(float64(j), "w")
				tree.Delete(float64(j))
				j++
			}
		}
	}()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			tree.Search(float64(i % 100000))
			i++
		}
	})
	b.StopTimer()
	close(stopCh)
}

