package vector

import (
	"math/rand"
	"strconv"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// tenantOfKey — демо-деривация тенанта из ключа: key = "<tenant>:<id>".
// В реале тенант берётся из колоночного атрибутного слоя; здесь — из префикса ключа,
// чтобы тест был самодостаточным и не тянул внешние данные.
func tenantOfKey(key string) uint64 {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			t, _ := strconv.ParseUint(key[:i], 10, 64)
			return t
		}
	}
	return 0
}

// makeEntries строит entries, ПЕРЕМЕШАННЫЕ по тенантам (как они приходят в дельту во
// времени — вперемешку), чтобы проверить, что именно сортировка создаёт контигуозность.
func makeEntries(tenantSizes map[uint64]int, dim int, rng *rand.Rand) []DeltaEntry {
	var entries []DeltaEntry
	for tenant, n := range tenantSizes {
		for i := 0; i < n; i++ {
			v := make([]float32, dim)
			for d := range v {
				v[d] = rng.Float32()
			}
			entries = append(entries, DeltaEntry{
				Key: strconv.FormatUint(tenant, 10) + ":" + strconv.Itoa(i),
				Vec: v,
			})
		}
	}
	// перемешать, имитируя временной порядок вставки вперемешку по тенантам
	rng.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
	return entries
}

// TestTenant_SortCreatesContiguity — #5: до сортировки тенанты разорваны, после —
// каждый занимает один непрерывный блок (инвариант verifyContiguous держится).
func TestTenant_SortCreatesContiguity(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	sizes := map[uint64]int{1: 100, 2: 50, 3: 5, 4: 200, 5: 1}
	entries := makeEntries(sizes, 8, rng)

	// До сортировки раскладка перемешана → инвариант должен НЕ держаться.
	if err := verifyContiguous(entries, tenantOfKey); err == nil {
		t.Fatal("ожидали нарушение инварианта на перемешанных entries, но verifyContiguous прошёл")
	}

	// Сортировка = ведущий ключ компакции.
	sortEntriesByTenant(entries, tenantOfKey)

	// После сортировки инвариант #5 обязан держаться.
	if err := verifyContiguous(entries, tenantOfKey); err != nil {
		t.Fatalf("после sortEntriesByTenant инвариант #5 нарушен: %v", err)
	}
}

// TestTenant_CatalogRangesAndKind — каталог: диапазоны точны, покрывают всё, не
// пересекаются; Kind проставлен по порогу #6.
func TestTenant_CatalogRangesAndKind(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	sizes := map[uint64]int{
		10: 5,     // мелкий → brute
		20: 100,   // мелкий → brute
		30: 25000, // крупный → graph (выше порога)
	}
	entries := makeEntries(sizes, 4, rng)
	sortEntriesByTenant(entries, tenantOfKey)

	const threshold = 16384
	cat, err := buildTenantCatalog(entries, tenantOfKey, threshold)
	if err != nil {
		t.Fatalf("buildTenantCatalog: %v", err)
	}

	if cat.Tenants() != len(sizes) {
		t.Fatalf("ожидали %d тенантов, получили %d", len(sizes), cat.Tenants())
	}

	// Каждый диапазон: правильный размер и Kind.
	for tenant, want := range sizes {
		r, ok := cat.Lookup(tenant)
		if !ok {
			t.Fatalf("тенант %d отсутствует в каталоге", tenant)
		}
		if r.Len() != want {
			t.Errorf("тенант %d: размер блока %d, ожидали %d", tenant, r.Len(), want)
		}
		wantKind := IndexBrute
		if want > threshold {
			wantKind = IndexGraph
		}
		if r.Kind != wantKind {
			t.Errorf("тенант %d (size=%d, thr=%d): Kind=%s, ожидали %s", tenant, want, threshold, r.Kind, wantKind)
		}
	}

	// Диапазоны покрывают [0,N) без дыр и пересечений.
	ranges := cat.All()
	prevEnd := 0
	total := 0
	for _, r := range ranges {
		if r.Start != prevEnd {
			t.Errorf("дыра/пересечение: диапазон тенанта %d начинается на %d, ожидали %d", r.Tenant, r.Start, prevEnd)
		}
		prevEnd = r.End
		total += r.Len()
	}
	if total != len(entries) {
		t.Errorf("сумма диапазонов %d != числу записей %d", total, len(entries))
	}
}

// TestTenant_CatalogRejectsUnsorted — buildTenantCatalog обязан отказать, если
// записи не сгруппированы (защита от молчаливо неверного каталога).
func TestTenant_CatalogRejectsUnsorted(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	entries := makeEntries(map[uint64]int{1: 10, 2: 10, 3: 10}, 4, rng)
	// НЕ сортируем намеренно.
	if _, err := buildTenantCatalog(entries, tenantOfKey, 16384); err == nil {
		t.Fatal("ожидали ошибку на несгруппированных entries, получили nil")
	}
}

// TestTenant_BlockBruteMatchesGroundTruth — #7: blocked-brute по диапазону тенанта
// даёт ровно тот же top-K, что и точный перебор среди векторов этого тенанта
// (recall=1.0), и НЕ возвращает чужих.
func TestTenant_BlockBruteMatchesGroundTruth(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	const dim = 16
	sizes := map[uint64]int{1: 300, 2: 150, 3: 40}
	entries := makeEntries(sizes, dim, rng)
	sortEntriesByTenant(entries, tenantOfKey)

	cat, err := buildTenantCatalog(entries, tenantOfKey, 16384)
	if err != nil {
		t.Fatalf("buildTenantCatalog: %v", err)
	}

	// flat-раскладка сегмента из отсортированных entries (как сделает buildSegment).
	data := make([]float32, len(entries)*dim)
	keys := make([]string, len(entries))
	for i, e := range entries {
		copy(data[i*dim:(i+1)*dim], e.Vec)
		keys[i] = e.Key
	}

	const K = 10
	const targetTenant = 2
	r, _ := cat.Lookup(targetTenant)

	query := make([]float32, dim)
	for d := range query {
		query[d] = rng.Float32()
	}

	got := blockBruteSearch(data, keys, dim, r, query, K, EuclideanDistance)

	// Ground truth: точный top-K среди ТОЛЬКО векторов targetTenant.
	type cand struct {
		key string
		d   float32
	}
	var all []cand
	for i, e := range entries {
		if tenantOfKey(e.Key) != targetTenant {
			continue
		}
		all = append(all, cand{e.Key, EuclideanDistance(query, data[i*dim:(i+1)*dim])})
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].d < all[j-1].d; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}

	if len(got) != K {
		t.Fatalf("blockBruteSearch вернул %d, ожидали %d", len(got), K)
	}
	for i := 0; i < K; i++ {
		if got[i].Key != all[i].key {
			t.Errorf("позиция %d: got %s, ожидали %s", i, got[i].Key, all[i].key)
		}
		if tenantOfKey(got[i].Key) != targetTenant {
			t.Errorf("blocked-brute вернул чужого тенанта: %s", got[i].Key)
		}
	}
}

// orderedKeys возвращает ключи сегмента в порядке frozen id (= порядок раскладки
// после sortEntriesByTenant). Для проверки контигуозности произведённого сегмента.
func orderedKeys(seg segment) []string {
	switch s := seg.(type) {
	case *frozenSegment:
		out := make([]string, s.fg.n)
		for i := 0; i < s.fg.n; i++ {
			out[i] = s.fg.keys.view(i)
		}
		return out
	case *frozenSQSegment:
		out := make([]string, s.fg.n)
		for i := 0; i < s.fg.n; i++ {
			out[i] = s.fg.keys.view(i)
		}
		return out
	default:
		return nil
	}
}

func settle(lvs *LeveledVectorStore) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		lvs.FlushDeltaSync()
		if !lvs.anyMergeInFlight() {
			time.Sleep(200 * time.Millisecond)
			if !lvs.anyMergeInFlight() {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestTenant_CompactionProducesContiguousSegments — ИНТЕГРАЦИЯ (Вещь 1): при
// TenantOf != nil компакция выдаёт сегменты, где каждый тенант лежит непрерывным
// блоком (инвариант #5), и на каждом сегменте висит корректный TenantCatalog (#6).
// Это пост-merge защёлка: если раскладка разъедется — тест упадёт.
func TestTenant_CompactionProducesContiguousSegments(t *testing.T) {
	const dim = 16
	rng := rand.New(rand.NewSource(2026))

	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(4),
		DeltaMax:       2000, // мелкая дельта → много flush+merge
		Fanout:         4,
		EfSearch:       64,
		TenantOf:       tenantOfKey,
		BruteThreshold: 1000, // мелкий порог → часть тенантов graph, часть brute
	}
	lvs := NewLeveledVectorStore(cfg)

	// 4 тенанта разного размера; вставляем ВПЕРЕМЕШКУ (как во времени).
	// Объём >8×DeltaMax → ≥4 L0 → срабатывает L1-merge: проверяем главное
	// утверждение #5 — merge СОБИРАЕТ разбросанные по L0 куски тенанта в один
	// непрерывный диапазон в merged-сегменте.
	sizes := map[uint64]int{1: 7000, 2: 4000, 3: 2500, 4: 900}
	type kv struct {
		key string
		vec []float32
	}
	var all []kv
	for tenant, n := range sizes {
		for i := 0; i < n; i++ {
			v := make([]float32, dim)
			for d := range v {
				v[d] = rng.Float32()
			}
			all = append(all, kv{strconv.FormatUint(tenant, 10) + ":" + strconv.Itoa(i), v})
		}
	}
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	for _, e := range all {
		if err := lvs.Add(e.key, e.vec); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	settle(lvs)

	st := lvs.Stats()
	t.Logf("segs by level: %v, total=%d", st.SegmentsByLevel, st.TotalVectors)

	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	nSegs := 0
	for lyr := range lvs.levels {
		for _, seg := range lvs.levels[lyr] {
			nSegs++
			// 1. Каталог обязан быть.
			cat := seg.Catalog()
			if cat == nil {
				t.Fatalf("сегмент L%d без каталога (TenantOf задан)", lyr)
			}
			// 2. Раскладка сегмента контигуозна по тенантам (инвариант #5).
			keys := orderedKeys(seg)
			if keys == nil {
				continue // hnswSegment не отдаёт ordered keys; пропускаем (dim≤csr → frozen)
			}
			ents := make([]DeltaEntry, len(keys))
			for i, k := range keys {
				ents[i] = DeltaEntry{Key: k}
			}
			if err := verifyContiguous(ents, tenantOfKey); err != nil {
				t.Fatalf("сегмент L%d не контигуозен: %v", lyr, err)
			}
			// 3. Каждый диапазон каталога содержит ТОЛЬКО своего тенанта,
			//    а Kind согласован с порогом.
			for _, r := range cat.All() {
				for i := r.Start; i < r.End; i++ {
					if keys[i] == "" {
						continue
					}
					if got := tenantOfKey(keys[i]); got != r.Tenant {
						t.Fatalf("L%d диапазон тенанта %d содержит чужой ключ %q (tenant %d)",
							lyr, r.Tenant, keys[i], got)
					}
				}
				wantKind := IndexBrute
				if r.Len() > cfg.BruteThreshold {
					wantKind = IndexGraph
				}
				if r.Kind != wantKind {
					t.Errorf("L%d тенант %d size=%d: Kind=%s, ожидали %s",
						lyr, r.Tenant, r.Len(), r.Kind, wantKind)
				}
			}
		}
	}
	if nSegs == 0 {
		t.Fatal("после settle нет сегментов — нечего проверять")
	}
}

// buildTenantStore — стор с tenant-режимом, набитый тенантами заданных размеров.
func buildTenantStore(t *testing.T, dim int, useSQ bool, bruteThreshold int, sizes map[uint64]int, seed int64) (*LeveledVectorStore, map[uint64][][]float32) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(4),
		DeltaMax:       2000,
		Fanout:         4,
		EfSearch:       128,
		UseSQ:          useSQ,
		TenantOf:       tenantOfKey,
		BruteThreshold: bruteThreshold,
	}
	lvs := NewLeveledVectorStore(cfg)
	vecsByTenant := make(map[uint64][][]float32)
	type kv struct {
		key string
		vec []float32
	}
	var all []kv
	for tenant, n := range sizes {
		for i := 0; i < n; i++ {
			v := make([]float32, dim)
			for d := range v {
				v[d] = rng.Float32()
			}
			vecsByTenant[tenant] = append(vecsByTenant[tenant], v)
			all = append(all, kv{strconv.FormatUint(tenant, 10) + ":" + strconv.Itoa(i), v})
		}
	}
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	for _, e := range all {
		if err := lvs.Add(e.key, e.vec); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	settle(lvs)
	return lvs, vecsByTenant
}

// tenantGT — точный top-K среди векторов ОДНОГО тенантa (ground truth).
func tenantGT(vecs [][]float32, query []float32, K int) []int {
	type c struct {
		id int
		d  float32
	}
	cs := make([]c, len(vecs))
	for i, v := range vecs {
		cs[i] = c{i, EuclideanDistance(query, v)}
	}
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].d < cs[j-1].d; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
	if len(cs) > K {
		cs = cs[:K]
	}
	ids := make([]int, len(cs))
	for i := range cs {
		ids[i] = cs[i].id
	}
	return ids
}

// TestTenant_SearchOnlyReturnsTenant — SearchTenant НИКОГДА не возвращает чужих,
// и для мелкого тенанта (brute-путь) recall=1.0 против точного GT этого тенанта.
func TestTenant_SearchFloat32BrutePath(t *testing.T) {
	const dim = 32
	const K = 10
	// targetTenant=3 (40 векторов) < threshold=1000 → brute; tenant 1 крупный.
	sizes := map[uint64]int{1: 5000, 2: 1200, 3: 40}
	lvs, vecs := buildTenantStore(t, dim, false, 1000, sizes, 11)

	const target = 3
	rng := rand.New(rand.NewSource(777))
	var sumRecall float64
	const Q = 50
	for q := 0; q < Q; q++ {
		query := make([]float32, dim)
		for d := range query {
			query[d] = rng.Float32()
		}
		res, err := lvs.SearchTenant(query, K, target, nil)
		if err != nil {
			t.Fatalf("SearchTenant: %v", err)
		}
		// 1. Только свой тенант.
		for _, r := range res {
			if tenantOfKey(r.Key) != target {
				t.Fatalf("SearchTenant вернул чужого тенанта: %q", r.Key)
			}
		}
		// 2. recall против GT этого тенанта.
		gtIDs := tenantGT(vecs[target], query, K)
		gtKeys := make(map[string]struct{}, len(gtIDs))
		for _, id := range gtIDs {
			gtKeys[strconv.FormatUint(target, 10)+":"+strconv.Itoa(id)] = struct{}{}
		}
		hit := 0
		for _, r := range res {
			if _, ok := gtKeys[r.Key]; ok {
				hit++
			}
		}
		sumRecall += float64(hit) / float64(K)
	}
	recall := sumRecall / Q
	t.Logf("float32 brute-path recall=%.4f", recall)
	if recall < 0.999 {
		t.Errorf("brute-путь должен давать recall=1.0, получили %.4f", recall)
	}
}

// TestTenant_SearchSQGraphAndBrute — на SQ-сторе проверяем оба пути: мелкий тенант
// (brute по SQ, recall≈1) и крупный (graph с тенант-фильтром, recall высокий).
func TestTenant_SearchSQ(t *testing.T) {
	const dim = 64
	const K = 10
	sizes := map[uint64]int{1: 6000, 2: 50}
	lvs, vecs := buildTenantStore(t, dim, true, 1000, sizes, 22)

	rng := rand.New(rand.NewSource(555))
	check := func(target uint64, minRecall float64) {
		var sumRecall float64
		const Q = 50
		for q := 0; q < Q; q++ {
			query := make([]float32, dim)
			for d := range query {
				query[d] = rng.Float32()
			}
			res, err := lvs.SearchTenant(query, K, target, nil)
			if err != nil {
				t.Fatalf("SearchTenant: %v", err)
			}
			for _, r := range res {
				if tenantOfKey(r.Key) != target {
					t.Fatalf("tenant %d: вернул чужого %q", target, r.Key)
				}
			}
			gtIDs := tenantGT(vecs[target], query, K)
			gtKeys := make(map[string]struct{}, len(gtIDs))
			for _, id := range gtIDs {
				gtKeys[strconv.FormatUint(target, 10)+":"+strconv.Itoa(id)] = struct{}{}
			}
			hit := 0
			for _, r := range res {
				if _, ok := gtKeys[r.Key]; ok {
					hit++
				}
			}
			sumRecall += float64(hit) / float64(len(gtIDs))
		}
		recall := sumRecall / Q
		t.Logf("SQ tenant=%d recall=%.4f (min %.2f)", target, recall, minRecall)
		if recall < minRecall {
			t.Errorf("tenant %d: recall %.4f < %.2f", target, recall, minRecall)
		}
	}
	check(2, 0.95) // мелкий → brute по SQ, recall высокий
	check(1, 0.85) // крупный → graph с фильтром
}

// TestTenant_SearchModeOff — без TenantOf SearchTenant обязан вернуть ошибку.
func TestTenant_SearchModeOff(t *testing.T) {
	cfg := LeveledConfig{Distance: EuclideanDistance, Allocator: tcmalloc.NewTCMallocStore(1)}
	lvs := NewLeveledVectorStore(cfg)
	_ = lvs.Add("a", []float32{1, 2, 3})
	if _, err := lvs.SearchTenant([]float32{1, 2, 3}, 5, 0, nil); err == nil {
		t.Fatal("ожидали ошибку при выключенном тенант-режиме")
	}
}
