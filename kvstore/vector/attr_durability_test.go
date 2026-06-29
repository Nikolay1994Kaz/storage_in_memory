package vector

import (
	"bytes"
	"math/rand"
	"strconv"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestAttr_DurabilityRoundTrip — колоночный слой + тенант-каталог переживают
// SaveBinary→LoadBinary (формат v2). После загрузки SearchFilter обязан давать
// ТЕ ЖЕ результаты, что до сохранения (иначе сегменты теряют cat/attrs и фильтр
// молча пропускает их).
func TestAttr_DurabilityRoundTrip(t *testing.T) {
	const (
		dim = 24
		N   = 5000
		K   = 10
	)
	rng := rand.New(rand.NewSource(20260629))

	mkCfg := func() LeveledConfig {
		return LeveledConfig{
			Distance:       EuclideanDistance,
			Allocator:      tcmalloc.NewTCMallocStore(4),
			DeltaMax:       1500,
			Fanout:         4,
			EfSearch:       64,
			PartitionAttr:  "tenant",
			BruteThreshold: 700,
		}
	}

	lvs := NewLeveledVectorStore(mkCfg())
	tenants := []string{"t1", "t2", "t3"}
	cats := []string{"books", "toys"}
	queries := make([][]float32, 50)
	for i := range queries {
		q := make([]float32, dim)
		for d := range q {
			q[d] = rng.Float32()
		}
		queries[i] = q
	}
	for i := 0; i < N; i++ {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()
		}
		if err := lvs.AddWithAttrs("k"+strconv.Itoa(i), v, Attributes{
			Cat: map[string]string{"tenant": tenants[rng.Intn(3)], "cat": cats[rng.Intn(2)]},
			Num: map[string]float64{"price": float64(rng.Intn(100))},
		}); err != nil {
			t.Fatalf("AddWithAttrs: %v", err)
		}
	}
	settle(lvs)

	filters := []Filter{
		{Eq: map[string]string{"tenant": "t1"}},
		{Eq: map[string]string{"tenant": "t2", "cat": "books"}},
		{Eq: map[string]string{"tenant": "t3", "cat": "toys"}, Range: map[string][2]float64{"price": {0, 50}}},
	}

	// Результаты ДО сохранения.
	type qr struct{ keys []string }
	before := make([][]qr, len(filters))
	resultKeys := func(store *LeveledVectorStore, f Filter, q []float32) []string {
		res, err := store.SearchFilter(q, K, f)
		if err != nil {
			t.Fatalf("SearchFilter: %v", err)
		}
		ks := make([]string, len(res))
		for i, r := range res {
			ks[i] = r.Key
		}
		return ks
	}
	for fi, f := range filters {
		before[fi] = make([]qr, len(queries))
		for qi, q := range queries {
			before[fi][qi] = qr{keys: resultKeys(lvs, f, q)}
		}
	}

	// Save → Load в свежий стор.
	var buf bytes.Buffer
	if err := lvs.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	t.Logf("снапшот %d байт", buf.Len())

	loaded := NewLeveledVectorStore(mkCfg())
	if err := loaded.LoadBinary(&buf); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}

	// Каталог + колонки должны быть восстановлены на сегментах.
	loaded.mu.RLock()
	nSeg, withCat, withAttrs := 0, 0, 0
	for _, lvl := range loaded.levels {
		for _, s := range lvl {
			nSeg++
			if s.Catalog() != nil {
				withCat++
			}
			if s.Attrs() != nil {
				withAttrs++
			}
		}
	}
	loaded.mu.RUnlock()
	if nSeg == 0 || withCat != nSeg || withAttrs != nSeg {
		t.Fatalf("после загрузки: segs=%d, с каталогом=%d, с атрибутами=%d (ожидали все)", nSeg, withCat, withAttrs)
	}

	// Результаты ПОСЛЕ загрузки — идентичны.
	for fi, f := range filters {
		for qi, q := range queries {
			got := resultKeys(loaded, f, q)
			want := before[fi][qi].keys
			if len(got) != len(want) {
				t.Fatalf("filter#%d q%d: len %d != %d (до сохранения)", fi, qi, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("filter#%d q%d поз %d: %q != %q (результаты разошлись после загрузки)", fi, qi, i, got[i], want[i])
				}
			}
		}
	}
}

// TestAttr_DurabilityLegacyTenantOf — segment в legacy-режиме TenantOf (без колонок)
// переживает round-trip: каталог восстановлен, attrs nil, SearchTenant работает.
func TestAttr_DurabilityLegacyTenantOf(t *testing.T) {
	const dim = 16
	rng := rand.New(rand.NewSource(7))
	mkCfg := func() LeveledConfig {
		return LeveledConfig{
			Distance:       EuclideanDistance,
			Allocator:      tcmalloc.NewTCMallocStore(4),
			DeltaMax:       1000,
			Fanout:         4,
			EfSearch:       64,
			TenantOf:       tenantOfKey,
			BruteThreshold: 500,
		}
	}
	lvs := NewLeveledVectorStore(mkCfg())
	for tenant := uint64(1); tenant <= 3; tenant++ {
		for i := 0; i < 1500; i++ {
			v := make([]float32, dim)
			for d := range v {
				v[d] = rng.Float32()
			}
			if err := lvs.Add(strconv.FormatUint(tenant, 10)+":"+strconv.Itoa(i), v); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
	}
	settle(lvs)

	var buf bytes.Buffer
	if err := lvs.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	loaded := NewLeveledVectorStore(mkCfg())
	if err := loaded.LoadBinary(&buf); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}

	loaded.mu.RLock()
	nSeg, withCat := 0, 0
	for _, lvl := range loaded.levels {
		for _, s := range lvl {
			nSeg++
			if s.Catalog() != nil {
				withCat++
			}
		}
	}
	loaded.mu.RUnlock()
	if nSeg == 0 || withCat != nSeg {
		t.Fatalf("legacy: segs=%d с каталогом=%d (ожидали все)", nSeg, withCat)
	}

	// SearchTenant на загруженном сторе возвращает только свой тенант.
	q := make([]float32, dim)
	for d := range q {
		q[d] = rng.Float32()
	}
	res, err := loaded.SearchTenant(q, 10, 2, nil)
	if err != nil {
		t.Fatalf("SearchTenant: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("SearchTenant вернул пусто после загрузки (каталог потерян?)")
	}
	for _, r := range res {
		if tenantOfKey(r.Key) != 2 {
			t.Fatalf("SearchTenant вернул чужой тенант: %s", r.Key)
		}
	}
}
