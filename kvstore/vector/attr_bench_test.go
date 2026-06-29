package vector

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestAttr_PredicateCostUintVsString изолирует рычаг #2: фильтрация brute по блоку
// тенанта через uint-колонку (bruteRangeAttr, codes[i]==code) против строкового
// предиката (bruteRange, filterFn(key)). Оба точны (recall=1.0 по построению brute),
// меряем ТОЛЬКО стоимость предиката — поэтому случайные векторы допустимы (recall не
// измеряется, важна цена на вектор).
//
//	go test ./kvstore/vector -run TestAttr_PredicateCostUintVsString -v
func TestAttr_PredicateCostUintVsString(t *testing.T) {
	if testing.Short() {
		t.Skip("строит HNSW + sweep")
	}
	const (
		dim   = 128
		N     = 60_000
		block = 12_000 // целевой тенант (brute-режим)
		K     = 10
		dur   = 3 * time.Second
	)
	rng := rand.New(rand.NewSource(20260629))

	// tenant "hot" — первые block записей; остальное — "bg". Атрибут tenant в
	// колонке; ключи "i" (числовые) для строкового предиката-аналога.
	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(8),
		DeltaMax:       N + 1, // один сегмент
		Fanout:         4,
		EfSearch:       128,
		PartitionAttr:  "tenant",
		BruteThreshold: 16384,
	}
	lvs := NewLeveledVectorStore(cfg)
	for i := 0; i < N; i++ {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()
		}
		tenant := "bg"
		if i < block {
			tenant = "hot"
		}
		if err := lvs.AddWithAttrs(strconv.Itoa(i), v, Attributes{Cat: map[string]string{"tenant": tenant}}); err != nil {
			t.Fatalf("AddWithAttrs: %v", err)
		}
	}
	settle(lvs)

	// Достаём единственный frozen-сегмент и блок тенанта "hot".
	lvs.mu.RLock()
	var fseg *frozenSegment
	var fsq *frozenSQSegment
	for _, lvl := range lvs.levels {
		for _, s := range lvl {
			switch seg := s.(type) {
			case *frozenSegment:
				fseg = seg
			case *frozenSQSegment:
				fsq = seg
			}
		}
	}
	lvs.mu.RUnlock()

	var cat *TenantCatalog
	var sa *segmentAttrs
	var fg frozenFilterable
	switch {
	case fseg != nil:
		cat, sa, fg = fseg.cat, fseg.attrs, fseg.fg
	case fsq != nil:
		cat, sa, fg = fsq.cat, fsq.attrs, fsq.fg
	default:
		t.Fatal("нет frozen-сегмента (ожидали один консолидированный)")
	}
	if cat == nil || sa == nil {
		t.Fatal("нет каталога/атрибутов на сегменте")
	}
	code, ok := sa.dict["tenant"].lookup("hot")
	if !ok {
		t.Fatal("тенант hot не в dict")
	}
	r, ok := cat.Lookup(uint64(code))
	if !ok {
		t.Fatalf("тенант hot не в каталоге")
	}
	t.Logf("блок hot: [%d,%d) size=%d kind=%s", r.Start, r.End, r.Len(), r.Kind)

	// Запросы.
	queries := make([][]float32, 100)
	for i := range queries {
		q := make([]float32, dim)
		for d := range q {
			q[d] = rng.Float32()
		}
		queries[i] = q
	}

	// (A) uint-предикат: codes[i]==code (через колонку).
	codes := sa.cat["tenant"].codes
	predUint := func(i int) bool { return codes[i] == code }
	// (B) строковый предикат: парсим ключ → проверяем диапазон id (аналог tenantOfKey).
	predStr := func(key string) bool {
		id, _ := strconv.Atoi(key)
		return id < block
	}

	runQPS := func(fn func(q []float32)) float64 {
		deadline := time.Now().Add(dur)
		var ops, qi int
		for time.Now().Before(deadline) {
			fn(queries[qi%len(queries)])
			qi++
			ops++
		}
		return float64(ops) / dur.Seconds()
	}

	dst := make([]FrozenResult, 0, K)
	qpsUint := runQPS(func(q []float32) { _ = fg.bruteRangeAttr(q, K, r.Start, r.End, dst[:0], predUint) })
	qpsStr := runQPS(func(q []float32) {
		switch {
		case fseg != nil:
			_ = fseg.fg.bruteRange(q, K, r.Start, r.End, dst[:0], predStr)
		default:
			_ = fsq.fg.bruteRange(q, K, r.Start, r.End, dst[:0], predStr)
		}
	})

	fmt.Println()
	fmt.Printf("PREDICATE COST #2 — random dim%d, block=%d, K=%d (1 thread)\n", dim, r.Len(), K)
	fmt.Printf("  uint codes[i]==code : %8.0f QPS\n", qpsUint)
	fmt.Printf("  string filterFn(key): %8.0f QPS\n", qpsStr)
	fmt.Printf("  выигрыш #2 (uint/string): %.2f×\n", qpsUint/qpsStr)
}
