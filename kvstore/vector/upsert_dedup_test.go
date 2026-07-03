package vector

import (
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// newDedupStore — общий конфиг для тестов дедупа (без партиции, Euclidean).
func newDedupStore(t *testing.T) *LeveledVectorStore {
	t.Helper()
	return NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(2),
		DeltaMax:       1000,
		Fanout:         8, // > числа сегментов в тестах → нет авто-merge
		EfSearch:       64,
		M:              16,
		EfConstruction: 100,
	})
}

func mkVecN(dim int, v float32) []float32 {
	out := make([]float32, dim)
	for i := range out {
		out[i] = v
	}
	return out
}

// TestUpsert_NoStaleDuplicate — страж корректности #1 (frozen-vs-frozen).
//
// Репро дефекта: upsert уже сфлашенного ключа не гасит старую копию, а flat-merge
// результатов поиска не дедуплицирует по ключу → один ключ дважды в top-K.
// Здесь обе копии сфлашены (детерминированно, без гонки с фоновым flush): stale
// в старом сегменте, fresh в новом. Поиск обязан вернуть ключ РОВНО раз — свежую
// копию (провенанс-ранг: levels[0], больший индекс = свежее).
func TestUpsert_NoStaleDuplicate(t *testing.T) {
	const dim = 8
	lvs := newDedupStore(t)
	defer lvs.Clear()

	if err := lvs.Add("k", mkVecN(dim, 10)); err != nil { // stale
		t.Fatalf("Add stale: %v", err)
	}
	lvs.FlushDeltaSync()
	if err := lvs.Add("k", mkVecN(dim, 0)); err != nil { // fresh == query
		t.Fatalf("Add fresh: %v", err)
	}
	lvs.FlushDeltaSync()

	res, err := lvs.Search(mkVecN(dim, 0), 10, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	count := 0
	var dist float32
	for _, r := range res {
		if r.Key == "k" {
			count++
			dist = r.Distance
		}
	}
	if count != 1 {
		t.Fatalf("ключ \"k\" в top-K %d раз (ожидали 1) — дубль между сегментами: %+v", count, res)
	}
	if dist > 1e-3 {
		t.Fatalf("расстояние до \"k\" = %g (ожидали ~0) — дедуп оставил stale-копию", dist)
	}
}

// TestUpsert_StaleDoesNotEvictLegit — страж «stale вытесняет легитимный K-й».
//
// K=1. Ключ upsert-нут ДАЛЕКО от запроса, но его прежняя (близкая) копия
// сфлашена. Если stale-копия остаётся в выдаче, она займёт единственный слот и
// вытеснит легитимного соседа. Обе стороны сфлашены → детерминированно.
func TestUpsert_StaleDoesNotEvictLegit(t *testing.T) {
	const dim = 8
	lvs := newDedupStore(t)
	defer lvs.Clear()
	query := mkVecN(dim, 0)

	if err := lvs.Add("k", mkVecN(dim, 0.1)); err != nil { // stale близко к запросу
		t.Fatalf("Add k stale: %v", err)
	}
	lvs.FlushDeltaSync()
	if err := lvs.Add("k", mkVecN(dim, 100)); err != nil { // fresh далеко
		t.Fatalf("Add k fresh: %v", err)
	}
	if err := lvs.Add("legit", mkVecN(dim, 0.2)); err != nil { // легитимный ближайший
		t.Fatalf("Add legit: %v", err)
	}
	lvs.FlushDeltaSync()

	res, err := lvs.Search(query, 1, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("ожидали 1 результат, получили %d: %+v", len(res), res)
	}
	if res[0].Key != "legit" {
		t.Fatalf("top-1 = %q (ожидали \"legit\") — stale-копия \"k\" вытеснила легитимного соседа: %+v", res[0].Key, res)
	}
}

// TestMerge_DedupsByKey — merge двух сегментов с одним ключом даёт ОДНУ запись
// (свежую). Без дедупа получился бы дубль внутри одного сегмента, который
// провенанс-дедуп поиска уже не различит (одинаковый ранг).
func TestMerge_DedupsByKey(t *testing.T) {
	const dim = 8
	lvs := newDedupStore(t)
	defer lvs.Clear()

	lvs.Add("k", mkVecN(dim, 10)) // stale
	lvs.FlushDeltaSync()
	lvs.Add("k", mkVecN(dim, 0)) // fresh
	lvs.FlushDeltaSync()

	// Собираем сегменты level 0 (oldest→newest) и мержим напрямую.
	lvs.mu.RLock()
	segs := append([]segment(nil), lvs.levels[0]...)
	lvs.mu.RUnlock()
	if len(segs) < 2 {
		t.Fatalf("ожидали ≥2 сегмента в level 0, получили %d", len(segs))
	}

	merged, _ := lvs.mergeSegmentsWithAllocator(segs, tcmalloc.NewTCMallocStore(1))
	if merged == nil {
		t.Fatal("merge вернул nil")
	}
	if merged.Len() != 1 {
		t.Fatalf("merged.Len()=%d (ожидали 1) — merge не дедуплицировал ключ", merged.Len())
	}
	raw := merged.Search(mkVecN(dim, 0), 1, 64, nil, nil)
	if len(raw) != 1 || raw[0].Dist > 1e-3 {
		t.Fatalf("merged содержит stale-копию: %+v (ожидали fresh, dist~0)", raw)
	}
}

// TestDelta_Contains — юнит на механизм затенения: активная дельта знает свои
// живые ключи (O(1), используется поиском для отбрасывания устаревших frozen-копий).
func TestDelta_Contains(t *testing.T) {
	const dim = 4
	d := NewDeltaSegmentSharded(dim, 1000, EuclideanDistance, 16, 100, 1)
	defer d.Close()

	if d.Contains("k") {
		t.Fatal("пустая дельта не должна содержать ключ")
	}
	d.Append("k", mkVecN(dim, 1))
	if !d.Contains("k") {
		t.Fatal("после Append дельта должна содержать ключ")
	}
	if d.Contains("missing") {
		t.Fatal("Contains вернул true для отсутствующего ключа")
	}
	if !d.Delete("k") {
		t.Fatal("Delete существующего ключа вернул false")
	}
	if d.Contains("k") {
		t.Fatal("после Delete ключ не должен присутствовать (не затеняет frozen)")
	}
}
