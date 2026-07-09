package vector

import (
	"fmt"
	"testing"
	"time"
)

// idleTestVec — детерминированный УНИКАЛЬНЫЙ вектор (инъективен по i):
// первая координата разводит все i, остальные дают геометрию.
func idleTestVec(i, dim int) []float32 {
	v := make([]float32, dim)
	v[0] = float32(i)
	for d := 1; d < dim; d++ {
		v[d] = float32((i*31+d*7)%97) / 97.0
	}
	return v
}

// waitForState поллит Stats() до предиката или дедлайна.
func waitForState(t *testing.T, lvs *LeveledVectorStore, timeout time.Duration, pred func(LeveledStats) bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred(lvs.Stats()) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return pred(lvs.Stats())
}

func segCount(st LeveledStats) int {
	n := 0
	for _, c := range st.SegmentsByLevel {
		n += c
	}
	return n
}

// TestIdleConsolidation_SubFanoutMerged — главный сценарий bulk-load→тишина:
// суб-fanout сегменты (< Fanout на уровне) обычным каскадом не мержатся никогда;
// idle-консолидация обязана сфлашить остаток дельты и собрать всё в 1 сегмент.
func TestIdleConsolidation_SubFanoutMerged(t *testing.T) {
	const dim, n = 8, 250 // DeltaMax=100 → 2 полных флаша (L0) + 50 в дельте
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:             100,
		IdleConsolidateAfter: 100 * time.Millisecond,
	})
	defer lvs.Close()

	for i := 0; i < n; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), idleTestVec(i, dim)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	ok := waitForState(t, lvs, 5*time.Second, func(st LeveledStats) bool {
		return st.DeltaLen == 0 && segCount(st) == 1 && st.TotalVectors == n
	})
	if !ok {
		st := lvs.Stats()
		t.Fatalf("консолидация не сошлась: delta=%d segsByLevel=%v total=%d",
			st.DeltaLen, st.SegmentsByLevel, st.TotalVectors)
	}

	// Search-sanity: после консолидации каждый вектор находит сам себя top-1.
	miss := 0
	for i := 0; i < n; i++ {
		res, err := lvs.Search(idleTestVec(i, dim), 1, nil)
		if err != nil || len(res) == 0 || res[0].Key != fmt.Sprintf("k%d", i) {
			miss++
		}
	}
	if miss > 0 {
		t.Fatalf("self-findability после консолидации: %d/%d промахов", miss, n)
	}
}

// TestIdleConsolidation_SkipWhenDominant — guard от рестройки-шторма: если
// крупнейший сегмент уже держит ≥90% векторов, полная консолидация не запускается
// (капля свежих данных не должна пересобирать весь индекс после каждой паузы).
func TestIdleConsolidation_SkipWhenDominant(t *testing.T) {
	const dim = 8
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:             1000,
		IdleConsolidateAfter: 100 * time.Millisecond,
	})
	defer lvs.Close()

	// 1000 векторов → полный флаш → 1 крупный L0.
	for i := 0; i < 1000; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), idleTestVec(i, dim)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if !waitForState(t, lvs, 5*time.Second, func(st LeveledStats) bool {
		return segCount(st) == 1 && st.DeltaLen == 0
	}) {
		t.Fatalf("не дождались флаша крупного сегмента: %+v", lvs.Stats())
	}

	// Капля: 20 векторов (2% данных) → idle-флаш остатка дельты обязан пройти...
	for i := 1000; i < 1020; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), idleTestVec(i, dim)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if !waitForState(t, lvs, 5*time.Second, func(st LeveledStats) bool {
		return st.DeltaLen == 0 && segCount(st) == 2
	}) {
		t.Fatalf("остаток дельты не сфлашен в мелкий сегмент: %+v", lvs.Stats())
	}

	// ...а консолидация — НЕТ (крупнейший держит 98%). Даём три окна затишья.
	time.Sleep(400 * time.Millisecond)
	if st := lvs.Stats(); segCount(st) != 2 {
		t.Fatalf("guard не сработал: ожидали 2 сегмента (98%%+2%%), получили %v", st.SegmentsByLevel)
	}
}

// TestIdleConsolidation_Disabled — 0 = выключено: остаток дельты не флашится,
// сегменты не трогаются (прежнее поведение без фичи).
func TestIdleConsolidation_Disabled(t *testing.T) {
	const dim = 8
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax: 100,
		// IdleConsolidateAfter не задан = 0 = off
	})
	defer lvs.Close()

	for i := 0; i < 150; i++ { // 1 полный флаш + 50 в дельте
		if err := lvs.Add(fmt.Sprintf("k%d", i), idleTestVec(i, dim)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if !waitForState(t, lvs, 5*time.Second, func(st LeveledStats) bool {
		return segCount(st) == 1
	}) {
		t.Fatalf("флаш полной дельты не прошёл: %+v", lvs.Stats())
	}

	// Флаш-граница асинхронна (в сегмент может уехать чуть больше DeltaMax),
	// поэтому фиксируем состояние и проверяем, что оно НЕ меняется со временем.
	before := lvs.Stats()
	if before.DeltaLen == 0 {
		t.Fatalf("ожидали непустой остаток дельты, получили 0: %+v", before)
	}
	time.Sleep(300 * time.Millisecond)
	st := lvs.Stats()
	if st.DeltaLen != before.DeltaLen || segCount(st) != 1 {
		t.Fatalf("без фичи состояние не должно меняться: delta=%d→%d segs=%v", before.DeltaLen, st.DeltaLen, st.SegmentsByLevel)
	}
}

// TestIdleConsolidation_UpsertSurvives — дедуп при консолидации: ключ, живущий
// в старом сегменте и переписанный позже (upsert через flush), после merge всех
// уровней обязан остаться в ЕДИНСТВЕННОЙ и СВЕЖЕЙ копии.
func TestIdleConsolidation_UpsertSurvives(t *testing.T) {
	const dim = 8
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:             100,
		IdleConsolidateAfter: 100 * time.Millisecond,
	})
	defer lvs.Close()

	for i := 0; i < 100; i++ { // полный флаш → сегмент со старой копией k0
		if err := lvs.Add(fmt.Sprintf("k%d", i), idleTestVec(i, dim)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	// Upsert k0 новым значением + добор до второго флаша.
	fresh := idleTestVec(9999, dim)
	if err := lvs.Add("k0", fresh); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for i := 100; i < 199; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), idleTestVec(i, dim)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	// TotalVectors в предикате обязателен: Stats() не учитывает дельту в полёте
	// флаша (swap сделан, сегмент ещё не опубликован) — без него предикат ловит
	// переходное окно «delta=0, seg=1, второй сегмент ещё строится».
	if !waitForState(t, lvs, 5*time.Second, func(st LeveledStats) bool {
		return st.DeltaLen == 0 && segCount(st) == 1 && st.TotalVectors == 199
	}) {
		t.Fatalf("консолидация не сошлась: %+v", lvs.Stats())
	}

	st := lvs.Stats()
	if st.TotalVectors != 199 { // k0..k198 = 199 уникальных ключей, k0 — без дубля
		t.Fatalf("дубль upsert-ключа пережил консолидацию: total=%d, ожидали 199", st.TotalVectors)
	}
	got, ok := lvs.Get("k0")
	if !ok {
		t.Fatal("k0 пропал после консолидации")
	}
	for d := range got {
		if got[d] != fresh[d] {
			t.Fatalf("k0 после консолидации — СТАРАЯ копия (dim %d: %v != %v)", d, got[d], fresh[d])
		}
	}
}
