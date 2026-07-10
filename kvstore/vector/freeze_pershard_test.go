package vector

import (
	"fmt"
	"testing"
)

// =============================================================================
// Юнит-тесты freeze-per-shard: flush шардированной дельты без rebuild —
// каждый шард замораживается своим мини-сегментом (см. startFreezeShardsGoroutine).
// Профит-бенчи на реальных данных — freeze_pershard_profit_test.go.
// =============================================================================

// pfVec — детерминированный вектор с уникальным ближайшим соседом: базовый
// паттерн по i + разнесение по модулю, чтобы соседние i не склеивались.
func pfVec(dim, i int) []float32 {
	v := make([]float32, dim)
	for d := 0; d < dim; d++ {
		v[d] = float32(i%17) + float32((i*d)%29)*0.1
	}
	v[0] = float32(i) // гарантия уникальности/разделимости
	return v
}

// pfL0Snapshot возвращает копию levels[0] под RLock.
func pfL0Snapshot(lvs *LeveledVectorStore) []segment {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	out := make([]segment, len(lvs.levels[0]))
	copy(out, lvs.levels[0])
	return out
}

// pfFlushingLen возвращает длину flushing-списка под RLock.
func pfFlushingLen(lvs *LeveledVectorStore) int {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	return len(lvs.flushing)
}

// TestFreezePerShard_PublishesSQ8Minis — базовый контракт: flush шардированной
// дельты (UseSQ) публикует ПО МИНИ-СЕГМЕНТУ на непустой шард (не один rebuild-
// сегмент), flushing пустеет, все ключи находимы точным self-query.
func TestFreezePerShard_PublishesSQ8Minis(t *testing.T) {
	const (
		dim = 32
		N   = 600
		S   = 4
	)
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       N, // ровно одна полная дельта
		DeltaShards:    S,
		Fanout:         64, // merge не мешает подсчёту мини
		M:              16,
		EfConstruction: 100,
		EfSearch:       200,
		UseSQ:          true,
		NumBuilders:    1,
	})
	defer lvs.Close()

	for i := 0; i < N; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), pfVec(dim, i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	lvs.FlushDeltaSync()

	l0 := pfL0Snapshot(lvs)
	if len(l0) < 2 {
		t.Fatalf("ожидали ≥2 мини-сегментов (freeze-per-shard, S=%d), получили %d — flush ушёл в rebuild?", S, len(l0))
	}
	if len(l0) > S {
		t.Fatalf("мини-сегментов %d больше числа шардов %d", len(l0), S)
	}
	total := 0
	for i, seg := range l0 {
		if _, ok := seg.(*frozenSQSegment); !ok {
			t.Fatalf("levels[0][%d]: ожидали *frozenSQSegment, получили %T", i, seg)
		}
		total += seg.Len()
	}
	// Len() мини считает слоты графа (вкл. dead-ноды), но живых ключей суммарно
	// не меньше N — проверяем находимость каждого ключа точным self-query.
	if total < N {
		t.Fatalf("суммарный Len мини-сегментов %d < N=%d", total, N)
	}
	if fl := pfFlushingLen(lvs); fl != 0 {
		t.Fatalf("flushing не пуст после FlushDeltaSync: %d", fl)
	}
	for i := 0; i < N; i++ {
		res, err := lvs.Search(pfVec(dim, i), 1, nil)
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if len(res) == 0 || res[0].Key != fmt.Sprintf("k%d", i) {
			t.Fatalf("ключ k%d не находим self-query после freeze-per-shard (got %+v)", i, res)
		}
	}
}

// TestFreezePerShard_PublishesFP32Minis — то же для fp32-CSR (UseSQ=false,
// dim ≤ csrDimThreshold): мини = *frozenSegment.
func TestFreezePerShard_PublishesFP32Minis(t *testing.T) {
	const (
		dim = 16
		N   = 400
		S   = 4
	)
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       N,
		DeltaShards:    S,
		Fanout:         64,
		M:              16,
		EfConstruction: 100,
		EfSearch:       200,
		NumBuilders:    1,
	})
	defer lvs.Close()

	for i := 0; i < N; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), pfVec(dim, i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	lvs.FlushDeltaSync()

	l0 := pfL0Snapshot(lvs)
	if len(l0) < 2 {
		t.Fatalf("ожидали ≥2 fp32-мини (S=%d), получили %d", S, len(l0))
	}
	for i, seg := range l0 {
		if _, ok := seg.(*frozenSegment); !ok {
			t.Fatalf("levels[0][%d]: ожидали *frozenSegment, получили %T", i, seg)
		}
	}
	for i := 0; i < N; i++ {
		res, err := lvs.Search(pfVec(dim, i), 1, nil)
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if len(res) == 0 || res[0].Key != fmt.Sprintf("k%d", i) {
			t.Fatalf("ключ k%d не находим после fp32 freeze-per-shard", i)
		}
	}
}

// TestFreezePerShard_UpsertDeleteSurviveFreeze — переходы состояния дельты
// (upsert перезаписывает граф-ноду, delete ставит tombstone) обязаны корректно
// пережить заморозку: удалённый ключ не возвращается, upsert виден новой версией.
func TestFreezePerShard_UpsertDeleteSurviveFreeze(t *testing.T) {
	const (
		dim = 16
		N   = 200
		S   = 4
	)
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       N * 2, // флашим вручную, Add не триггерит
		DeltaShards:    S,
		Fanout:         64,
		M:              16,
		EfConstruction: 100,
		EfSearch:       200,
		UseSQ:          true,
		NumBuilders:    1,
	})
	defer lvs.Close()

	for i := 0; i < N; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), pfVec(dim, i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	// Upsert k5 новым вектором (далёким от старого), delete k7.
	newVec5 := pfVec(dim, 5000)
	if err := lvs.Add("k5", newVec5); err != nil {
		t.Fatalf("upsert k5: %v", err)
	}
	if !lvs.Delete("k7") {
		t.Fatal("Delete(k7) вернул false")
	}
	lvs.FlushDeltaSync()

	if len(pfL0Snapshot(lvs)) < 2 {
		t.Fatalf("ожидали мини-сегменты freeze-per-shard, получили %d", len(pfL0Snapshot(lvs)))
	}
	// k5 находим по НОВОМУ вектору.
	res, err := lvs.Search(newVec5, 1, nil)
	if err != nil {
		t.Fatalf("Search(newVec5): %v", err)
	}
	if len(res) == 0 || res[0].Key != "k5" {
		t.Fatalf("upsert k5 не пережил freeze: %+v", res)
	}
	// k7 удалён — не должен возвращаться даже точным self-query.
	res, err = lvs.Search(pfVec(dim, 7), 3, nil)
	if err != nil {
		t.Fatalf("Search(vec7): %v", err)
	}
	for _, r := range res {
		if r.Key == "k7" {
			t.Fatalf("удалённый k7 вернулся из поиска после freeze: %+v", res)
		}
	}
	if _, ok := lvs.Get("k7"); ok {
		t.Fatal("Get(k7) видит удалённый ключ после freeze")
	}
}

// TestFreezePerShard_AttrsFallbackToRebuild — атрибуты требуют колоночного слоя
// (freeze его не строит) → flush обязан уйти в rebuild: ОДИН сегмент с attrs,
// SearchFilter работает после flush.
func TestFreezePerShard_AttrsFallbackToRebuild(t *testing.T) {
	const (
		dim = 16
		N   = 200
		S   = 4
	)
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       N * 2,
		DeltaShards:    S,
		Fanout:         64,
		M:              16,
		EfConstruction: 100,
		EfSearch:       200,
		UseSQ:          true,
		NumBuilders:    1,
	})
	defer lvs.Close()

	for i := 0; i < N; i++ {
		attrs := Attributes{Cat: map[string]string{"color": fmt.Sprintf("c%d", i%2)}}
		if err := lvs.AddWithAttrs(fmt.Sprintf("k%d", i), pfVec(dim, i), attrs); err != nil {
			t.Fatalf("AddWithAttrs(%d): %v", i, err)
		}
	}
	lvs.FlushDeltaSync()

	l0 := pfL0Snapshot(lvs)
	if len(l0) != 1 {
		t.Fatalf("attrs-дельта обязана флашиться rebuild'ом в 1 сегмент, получили %d", len(l0))
	}
	if l0[0].Attrs() == nil {
		t.Fatal("rebuild-сегмент без колоночного слоя attrs")
	}
	// Фильтр по атрибуту работает после flush.
	res, err := lvs.SearchFilter(pfVec(dim, 4), 5, Filter{Eq: map[string]string{"color": "c0"}})
	if err != nil {
		t.Fatalf("SearchFilter: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("SearchFilter не вернул результатов после flush attrs-дельты")
	}
	for _, r := range res {
		var i int
		fmt.Sscanf(r.Key, "k%d", &i)
		if i%2 != 0 {
			t.Fatalf("SearchFilter вернул ключ %s с color=c1 при фильтре c0", r.Key)
		}
	}
}

// TestFreezePerShard_HighDimFP32FallbackToRebuild — fp32 при dim>csrDimThreshold:
// frozen-CSR неприменим → rebuild в один hnswSegment (прежнее поведение).
func TestFreezePerShard_HighDimFP32FallbackToRebuild(t *testing.T) {
	const (
		dim = csrDimThreshold + 44 // 300
		N   = 150
		S   = 4
	)
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       N * 2,
		DeltaShards:    S,
		Fanout:         64,
		M:              16,
		EfConstruction: 100,
		EfSearch:       200,
		NumBuilders:    1,
	})
	defer lvs.Close()

	for i := 0; i < N; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), pfVec(dim, i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	lvs.FlushDeltaSync()

	l0 := pfL0Snapshot(lvs)
	if len(l0) != 1 {
		t.Fatalf("fp32 dim>%d обязан флашиться rebuild'ом в 1 сегмент, получили %d", csrDimThreshold, len(l0))
	}
	if _, ok := l0[0].(*hnswSegment); !ok {
		t.Fatalf("ожидали *hnswSegment, получили %T", l0[0])
	}
}

// TestFreezePerShard_MergeConsolidatesMinis — мини-сегменты не вечны: merge-каскад
// склеивает их обычным путём (mergeSegments дедуплицирует и перестраивает с
// repair). После склейки все ключи остаются находимы.
func TestFreezePerShard_MergeConsolidatesMinis(t *testing.T) {
	const (
		dim = 16
		S   = 4
	)
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       200,
		DeltaShards:    S,
		Fanout:         4, // 2 флаша × ≤4 мини ≥ fanout → merge
		M:              16,
		EfConstruction: 100,
		EfSearch:       200,
		UseSQ:          true,
		NumBuilders:    1,
	})
	defer lvs.Close()

	const N = 400 // 2 полных дельты
	for i := 0; i < N; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), pfVec(dim, i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	lvs.FlushDeltaSync()

	// Merge-каскад пинается из applyResult; ждём появления L1-сегмента.
	waitUntil(t, func() bool {
		lvs.mu.RLock()
		defer lvs.mu.RUnlock()
		return len(lvs.levels[1]) > 0
	})

	for i := 0; i < N; i++ {
		res, err := lvs.Search(pfVec(dim, i), 1, nil)
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if len(res) == 0 || res[0].Key != fmt.Sprintf("k%d", i) {
			t.Fatalf("ключ k%d потерян после merge мини-сегментов", i)
		}
	}
}
