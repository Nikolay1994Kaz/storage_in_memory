package vector

import (
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

func tombVec(dim int, fill float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = fill
	}
	return v
}

// 3a. Delete после upsert-через-flush не должен воскрешать старую копию.
// Сценарий: Add(k,v1)→flush (v1 в сегменте)→Add(k,v2) (v2 в дельте, v1 ещё в
// сегменте). Delete(k) находил k в дельте, hard-delete'ил v2, СНИМАЛ tombstone
// и возвращался — оставляя v1 живым в сегменте. Get/Search воскрешали v1.
func TestTombstone_DeleteAfterUpsertNoResurrection(t *testing.T) {
	const dim = 8
	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(1),
		DeltaMax:  10000,
		Fanout:    4,
		EfSearch:  64,
	}
	lvs := NewLeveledVectorStore(cfg)

	v1 := tombVec(dim, 1)
	v2 := tombVec(dim, 2)

	if err := lvs.Add("k", v1); err != nil {
		t.Fatalf("Add v1: %v", err)
	}
	lvs.FlushDeltaSync() // v1 уезжает в сегмент
	if err := lvs.Add("k", v2); err != nil {
		t.Fatalf("Add v2: %v", err)
	}

	if !lvs.Delete("k") {
		t.Fatal("Delete должен вернуть true")
	}

	// Ключ обязан исчезнуть отовсюду.
	if _, ok := lvs.Get("k"); ok {
		t.Fatal("ВОСКРЕШЕНИЕ: Get нашёл удалённый ключ (старая копия v1 осталась в сегменте)")
	}
	res, err := lvs.Search(v1, 5, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range res {
		if r.Key == "k" {
			t.Fatal("ВОСКРЕШЕНИЕ: Search вернул удалённый ключ")
		}
	}
	// И после flush дельты (tombstone обязан пережить и вымести v1 при merge).
	lvs.FlushDeltaSync()
	if _, ok := lvs.Get("k"); ok {
		t.Fatal("ВОСКРЕШЕНИЕ после flush: Get нашёл удалённый ключ")
	}
}

// 3b/#1. Merge НЕ должен чистить tombstone ключа, живущего в НЕ-мёрдженном
// сегменте. Старый код брал снапшот ВСЕХ tombstones и после build стирал их
// ГЛОБАЛЬНО, хотя merge обрабатывает лишь подмножество сегментов (fanout уровня).
// Ключ, чья копия осталась в сегменте другого уровня, воскресал.
func TestTombstone_MergeKeepsTombstoneOfNonMergedKey(t *testing.T) {
	const dim = 4
	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(1),
		Fanout:    4,
		EfSearch:  32,
	}
	lvs := NewLeveledVectorStore(cfg)
	lvs.dim = dim

	alloc := tcmalloc.NewTCMallocStore(1)
	segMerged := lvs.buildSegmentWithAllocator([]DeltaEntry{{Key: "a", Vec: tombVec(dim, 1)}}, dim, alloc)
	segOther := lvs.buildSegmentWithAllocator([]DeltaEntry{{Key: "z", Vec: tombVec(dim, 2)}}, dim, alloc)
	lvs.levels[0] = []segment{segMerged}
	lvs.levels[1] = []segment{segOther}

	// "z" удалён, но физически жив в segOther (НЕ во входе merge).
	addTombstone(&lvs.tombstones, "z")

	// Мержим только segMerged. Снапшот ts содержит "z".
	if seg, _ := lvs.mergeSegmentsWithAllocator([]segment{segMerged}, tcmalloc.NewTCMallocStore(1)); seg == nil {
		t.Fatal("merge вернул nil")
	}

	t2 := lvs.tombstones.Load()
	if t2 == nil {
		t.Fatal("ВОСКРЕШЕНИЕ: merge стёр tombstone 'z', хотя 'z' жив в не-мёрдженном сегменте")
	}
	if _, ok := (*t2)["z"]; !ok {
		t.Fatal("ВОСКРЕШЕНИЕ: merge стёр tombstone 'z' — копия из segOther воскреснет")
	}
}

// 3b/reclaim. Обратная сторона: когда ключ ВЫМЕТЕН из всех сегментов merge'ем,
// tombstone обязан быть очищен (иначе утечка памяти tombstone-мапы). Проверяем,
// что перенос очистки в applyResult не сломал реклейм.
func TestTombstone_MergeClearsWhenFullyPurged(t *testing.T) {
	const dim = 8
	cfg := LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(1),
		DeltaMax:  10000,
		Fanout:    2, // 2 L0-сегмента → авто-merge в L1
		EfSearch:  32,
	}
	lvs := NewLeveledVectorStore(cfg)

	if err := lvs.Add("gone", tombVec(dim, 1)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	lvs.FlushDeltaSync() // L0 seg1 = {gone}
	if !lvs.Delete("gone") {
		t.Fatal("Delete должен вернуть true")
	}
	// tombstone поставлен (gone жив в seg1).
	if tv := lvs.tombstones.Load(); tv == nil {
		t.Fatal("ожидали tombstone 'gone' после Delete")
	}

	// Второй L0-сегмент → coordinator сольёт seg1+seg2 в L1, вымыв 'gone'.
	if err := lvs.Add("x", tombVec(dim, 2)); err != nil {
		t.Fatalf("Add x: %v", err)
	}
	lvs.FlushDeltaSync()
	settle(lvs) // дождаться merge + applyResult

	if _, ok := lvs.Get("gone"); ok {
		t.Fatal("ВОСКРЕШЕНИЕ: 'gone' найден после merge")
	}
	// tombstone 'gone' очищен (ключа больше нет ни в одном сегменте).
	if tv := lvs.tombstones.Load(); tv != nil {
		if _, ok := (*tv)["gone"]; ok {
			t.Fatal("реклейм сломан: tombstone 'gone' не очищен, хотя ключ вымыт из всех сегментов")
		}
	}
}

// 3c/#4. Get/Delete/ForEach/Len должны видеть ключ в сфлашиваемой дельте
// (flush-окно между swap дельты и публикацией сегмента). Раньше это учитывал
// только Search → в окне Delete тихо возвращал false (ключ не удалялся), Get
// возвращал miss, Len недосчитывал, ForEach пропускал.
func TestTombstone_FlushingWindowVisibility(t *testing.T) {
	const dim = 8
	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(1),
		M:              16,
		EfConstruction: 200,
		EfSearch:       32,
	}
	lvs := NewLeveledVectorStore(cfg)
	lvs.dim = dim

	// Симулируем flush-окно: дельта с ключом "f" висит в lvs.flushing (swap был,
	// сегмент из неё ещё не опубликован).
	fd := NewDeltaSegmentSharded(dim, 1000, cfg.Distance, cfg.M, cfg.EfConstruction, 1)
	fd.AppendWithAttrs("f", tombVec(dim, 1), Attributes{})
	lvs.flushing = append(lvs.flushing, fd)

	if _, ok := lvs.Get("f"); !ok {
		t.Fatal("Get не видит ключ в сфлашиваемой дельте (flush-окно)")
	}
	if n := lvs.Len(); n != 1 {
		t.Fatalf("Len=%d, ожидали 1 (ключ в flushing не посчитан)", n)
	}
	seen := false
	lvs.ForEach(func(k string, _ []float32) {
		if k == "f" {
			seen = true
		}
	})
	if !seen {
		t.Fatal("ForEach не обошёл ключ в сфлашиваемой дельте")
	}

	// Delete видит и удаляет через tombstone (мутировать flushing-дельту нельзя).
	if !lvs.Delete("f") {
		t.Fatal("Delete тихо провалился для ключа в сфлашиваемой дельте")
	}
	if _, ok := lvs.Get("f"); ok {
		t.Fatal("Get видит ключ после Delete (tombstone не замаскировал flushing-копию)")
	}
}
