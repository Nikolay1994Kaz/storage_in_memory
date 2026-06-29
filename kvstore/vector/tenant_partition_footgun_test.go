package vector

import (
	"strconv"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestTenant_PartitionAttr_SearchTenantFootgun — диагностика «tnt_rec=0.000»,
// найденного валидацией dbpedia (SearchTenant+SQ8+graph). Истинная причина —
// НЕ ANN/SQ8, а семантика API: при PartitionAttr каталог keyed по dict-коду
// атрибута (buildTenantCatalog ← pd.intern), а вызов SearchTenant(uint64) трактует
// аргумент как этот код. Если передать значение TenantOf (≠ dict-код) — Lookup
// попадает в ЧУЖОЙ блок → tenant-предикат отсекает всё → пусто.
//
// Доказательство: на ОДНОМ сторе SearchFilter(Eq{tenant:val}) (резолвит строку
// через dict внутри) находит тенанта, а SearchTenant(numericVal) — нет. Значит
// движок здоров, баг в использовании API (и в footgun-семантике SearchTenant
// при PartitionAttr).
//
// Детерминированное несовпадение код≠значение: вставляем тенант "1" ПЕРВЫМ
// (intern→code 0), тенант "0" вторым (intern→code 1). Тогда SearchTenant(1)
// смотрит code 1 = блок тенанта "0".
func TestTenant_PartitionAttr_SearchTenantFootgun(t *testing.T) {
	const (
		dim      = 32
		szPerT   = 80  // оба ≤ bruteThr → brute-route (frozenSQSegment использует диапазон)
		bruteThr = 100 // > szPerT → IndexBrute
		K        = 10
	)
	// Порядок вставки задаёт dict-коды: "1"→0, "0"→1 (несовпадение с TenantOf).
	order := []string{"1", "0"}

	mkVec := func(seed int) []float32 {
		v := make([]float32, dim)
		x := uint32(seed*2654435761 + 1)
		for d := range v {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			v[d] = float32(x%1000) / 1000
		}
		return v
	}

	type kv struct {
		key string
		vec []float32
		tn  string
	}
	var all []kv
	seed := 0
	for _, tn := range order {
		for i := 0; i < szPerT; i++ {
			seed++
			all = append(all, kv{tn + ":" + strconv.Itoa(i), mkVec(seed), tn})
		}
	}
	// НЕ перемешиваем — сохраняем порядок first-seen для детерминированных кодов.

	cfg := LeveledConfig{
		Metric:         MetricEuclidean,
		Allocator:      tcmalloc.NewTCMallocStore(2),
		DeltaMax:       len(all) + 1,
		Fanout:         4,
		EfConstruction: 200,
		EfSearch:       128,
		TenantOf:       tenantOfKey,
		PartitionAttr:  "tenant",
		BruteThreshold: bruteThr,
		UseSQ:          true,
	}
	lvs := NewLeveledVectorStore(cfg)
	for _, e := range all {
		if err := lvs.AddWithAttrs(e.key, e.vec, Attributes{Cat: map[string]string{"tenant": e.tn}}); err != nil {
			t.Fatal(err)
		}
	}
	settle(lvs)

	// GT для тенанта "1" (значение TenantOf = 1).
	const target = uint64(1)
	pass := make([]bool, len(all))
	idxVecs := make([][]float32, len(all))
	for i, e := range all {
		idxVecs[i] = e.vec
		pass[i] = tenantOfKey(e.key) == target
	}
	query := mkVec(99999)
	gt := topKFilteredIdx(idxVecs, query, K, pass)
	gtSet := make(map[string]struct{}, len(gt))
	for _, id := range gt {
		gtSet[all[id].key] = struct{}{}
	}
	recallOf := func(res []VSearchResult) float64 {
		hit := 0
		for _, r := range res {
			if _, ok := gtSet[r.Key]; ok {
				hit++
			}
		}
		return float64(hit) / float64(len(gt))
	}

	// Правильный API при PartitionAttr: SearchFilter резолвит строку через dict.
	resFilter, err := lvs.SearchFilter(query, K, Filter{Eq: map[string]string{"tenant": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	recFilter := recallOf(resFilter)

	// Footgun-гард: SearchTenant при PartitionAttr теперь возвращает ЯВНУЮ ошибку
	// (раньше тихо возвращал мусор — recall 0.000, вскрыто валидацией dbpedia).
	_, err = lvs.SearchTenant(query, K, target, nil)

	t.Logf("PartitionAttr=tenant, dict-код(\"1\")=0 ≠ TenantOf(1)=1")
	t.Logf("SearchFilter(Eq tenant=1) recall=%.3f  (правильный API)", recFilter)
	t.Logf("SearchTenant(1)           err=%v  (гард вместо тихого мусора)", err)

	if recFilter < 0.9 {
		t.Fatalf("SearchFilter должен находить тенанта (движок здоров), got %.3f", recFilter)
	}
	if err != errTenantWithPartition {
		t.Fatalf("SearchTenant при PartitionAttr должен вернуть errTenantWithPartition, got %v", err)
	}
}
