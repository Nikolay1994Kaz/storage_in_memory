package zset

import (
	"fmt"
	"sync"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestZSetAllocRace_CrossSet — репро гонки аллокатора в zset (порча данных).
//
// БАГ: btree.New хардкодил workerID=0 → ВСЕ zset-деревья аллоцируют узлы и
// member-строки из общего caches[0], вызываясь из ПРОИЗВОЛЬНЫХ epoll-воркеров.
// MCache — single-writer (Alloc пишет span.allocIndex/freeStack/AllocCount без
// локов). Два конкурентных ZADD в РАЗНЫЕ наборы берут РАЗНЫЕ per-set локи (S3),
// поэтому не сериализуются → оба зовут store.Alloc(0,…) → конкурентная запись в
// caches[0] = data race (ловится -race) и тихая порча (один objIndex двум
// аллокациям).
//
// Per-set лок (S3) лечит ТОЛЬКО same-set. Здесь наборы разные → S3 не спасает.
//
// Фикс: аллокация из кэша ВЫЗЫВАЮЩЕГО воркера (caches[workerID]); каждый воркер
// — одна горутина → single-writer соблюдён. Запускать под `go test -race`.
func TestZSetAllocRace_CrossSet(t *testing.T) {
	const (
		numWorkers = 8
		iters      = 3000
	)
	store := tcmalloc.NewTCMallocStore(numWorkers)
	defer store.Close()

	reg := New(store)

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			set := fmt.Sprintf("set%d", id) // РАЗНЫЕ наборы → разные per-set локи
			for i := 0; i < iters; i++ {
				reg.ZAdd(id, set, float64(i), fmt.Sprintf("m%d", i))
			}
		}(w)
	}
	wg.Wait()

	// Санити: каждый набор должен содержать ровно iters элементов (без порчи).
	for w := 0; w < numWorkers; w++ {
		if got := reg.ZCard(fmt.Sprintf("set%d", w)); got != iters {
			t.Fatalf("set%d: ZCard=%d, want %d (порча при конкурентной аллокации)", w, got, iters)
		}
	}
}

// TestZSetAllocRace_VsKV — репро гонки ZADD(любой воркер) vs KV Set(worker 0).
//
// ZADD с воркера w≠0 аллоцировал btree-узлы из caches[0] (баг), а параллельный
// обычный KV SET на воркере 0 аллоцирует оттуда же → конкурентная запись в
// caches[0]. Фикс разводит их по caches[w] и caches[0].
func TestZSetAllocRace_VsKV(t *testing.T) {
	const (
		numWorkers = 4
		iters      = 3000
	)
	store := tcmalloc.NewTCMallocStore(numWorkers)
	defer store.Close()

	reg := New(store)

	var wg sync.WaitGroup
	// Воркер 0: поток обычных KV SET (использует caches[0]).
	wg.Add(1)
	go func() {
		defer wg.Done()
		val := make([]byte, 40)
		for i := 0; i < iters; i++ {
			store.Set(0, fmt.Sprintf("kv%d", i%64), val)
		}
	}()
	// Воркеры 1..N-1: ZADD в свои наборы (btree-аллокации).
	for w := 1; w < numWorkers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			set := fmt.Sprintf("z%d", id)
			for i := 0; i < iters; i++ {
				reg.ZAdd(id, set, float64(i), fmt.Sprintf("m%d", i))
			}
		}(w)
	}
	wg.Wait()
}
