package vector

// =============================================================================
// П3-эксперимент (23.07): стейл-семантика публичных путей поиска
// (VSIM.SEARCHTEXT/HYBRID/SEARCH+фильтры) в переходных мульти-сегментных
// состояниях LSM — та же дыра, что чинил пере-суд VMEM.Recall (a77924b,
// TestVMEMSupersedeTwoSegmentLeak), но на публичной поверхности.
//
// Пороги назначены ДО прогона (утверждены Николаем 23.07):
//   цена фикса: QPS strict/QPS baseline ≥ 0.9× → фикс дешёвый, включать
//   вместо оговорки в доках; иначе — документировать с измеренной ценой.
//
// Замер честен только на МУЛЬТИ-сегментном состоянии: на консолидированном
// сторе (канонические линейки, 1 сегмент) цена строгого затенения ноль по
// построению — более свежих сегментов нет.
// =============================================================================

import (
	"fmt"
	"math/rand"
	"os"
	"slices"
	"testing"
	"time"
)

// TestStrictShadowCorrectness — пиннинг обеих семантик на минимальном репро
// (2 сегмента, upsert сменил атрибут / снял текст):
//   - StrictSegShadow=false: stale-копия ВСПЛЫВАЕТ (документированная
//     принятая семантика, компакция нормализует);
//   - StrictSegShadow=true: stale-копия гасится HasKey более свежего сегмента.
func TestStrictShadowCorrectness(t *testing.T) {
	cfg := bm25TestConfig()
	cfg.DeltaMax = 100
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Close()

	openAttr := Attributes{Cat: map[string]string{"stage": "open"}}
	closedAttr := Attributes{Cat: map[string]string{"stage": "closed"}}

	// Сегмент 1 (старый): d1 open + d2 open (контроль: обязан выживать всегда).
	if err := lvs.AddDoc("d1", mkVecN(8, 1), openAttr, "alpha beta gamma"); err != nil {
		t.Fatalf("AddDoc d1: %v", err)
	}
	if err := lvs.AddDoc("d2", mkVecN(8, 2), openAttr, "alpha delta"); err != nil {
		t.Fatalf("AddDoc d2: %v", err)
	}
	if err := lvs.AddDoc("d3", mkVecN(8, 3), openAttr, "omega psi"); err != nil {
		t.Fatalf("AddDoc d3: %v", err)
	}
	lvs.FlushDeltaSync()
	// Сегмент 2 (свежий): d1 закрыт (сменил атрибут), d3 потерял текст (upsert
	// чистым вектором). Свежие копии НЕ матчатся фильтру stage=open / запросу.
	if err := lvs.AddDoc("d1", mkVecN(8, 1), closedAttr, "alpha beta gamma"); err != nil {
		t.Fatalf("upsert d1: %v", err)
	}
	if err := lvs.Add("d3", mkVecN(8, 3)); err != nil {
		t.Fatalf("upsert d3: %v", err)
	}
	lvs.FlushDeltaSync()

	fOpen := Filter{Eq: map[string]string{"stage": "open"}}
	textKeys := func() []string {
		res, err := lvs.SearchTextFilter("alpha", 10, fOpen)
		if err != nil {
			t.Fatalf("SearchTextFilter: %v", err)
		}
		keys := make([]string, len(res))
		for i, r := range res {
			keys[i] = r.Key
		}
		return keys
	}
	textNoFilterKeys := func() []string {
		res, err := lvs.SearchTextFilter("omega", 10, Filter{})
		if err != nil {
			t.Fatalf("SearchTextFilter omega: %v", err)
		}
		keys := make([]string, len(res))
		for i, r := range res {
			keys[i] = r.Key
		}
		return keys
	}
	vecKeys := func() []string {
		res, err := lvs.SearchFilter(mkVecN(8, 1), 10, fOpen)
		if err != nil {
			t.Fatalf("SearchFilter: %v", err)
		}
		keys := make([]string, len(res))
		for i, r := range res {
			keys[i] = r.Key
		}
		return keys
	}

	// Базовая семантика: stale d1 (open-копия из старого сегмента) всплывает.
	lvs.cfg.StrictSegShadow = false
	if got := textKeys(); !slices.Contains(got, "d1") || !slices.Contains(got, "d2") {
		t.Fatalf("baseline text: ожидал stale d1 и живой d2, получил %v", got)
	}
	if got := textNoFilterKeys(); !slices.Contains(got, "d3") {
		t.Fatalf("baseline text-removed: ожидал stale d3, получил %v", got)
	}
	if got := vecKeys(); !slices.Contains(got, "d1") {
		t.Fatalf("baseline vector: ожидал stale d1, получил %v", got)
	}

	// Строгая семантика: stale гаснет, легитимные хиты не задеты.
	lvs.cfg.StrictSegShadow = true
	if got := textKeys(); slices.Contains(got, "d1") || !slices.Contains(got, "d2") {
		t.Fatalf("strict text: stale d1 обязан погаснуть, d2 выжить; получил %v", got)
	}
	if got := textNoFilterKeys(); slices.Contains(got, "d3") {
		t.Fatalf("strict text-removed: stale d3 обязан погаснуть; получил %v", got)
	}
	if got := vecKeys(); slices.Contains(got, "d1") {
		t.Fatalf("strict vector: stale d1 обязан погаснуть; получил %v", got)
	}
}

// strictShadowCorpus строит мульти-сегментный корпус с историей upsert'ов:
// nSeg сегментов по batch доков; в каждом батче k>0 первые upserts доков —
// повторные версии случайных доков из ПРЕДЫДУЩИХ батчей (смена scope-атрибута
// → для старого scope копия становится стейл). Возвращает store.
func strictShadowCorpus(t *testing.T, dim, nSeg, batch, upserts, vocab, nTok, nScopes int) *LeveledVectorStore {
	t.Helper()
	cfg := bm25TestConfig()
	cfg.DeltaMax = batch + upserts + 1
	lvs := NewLeveledVectorStore(cfg)
	rng := rand.New(rand.NewSource(42))
	scopeOf := func(doc, version int) Attributes {
		return Attributes{Cat: map[string]string{"scope": fmt.Sprintf("user:%03d", (doc*7+version*13)%nScopes)}}
	}
	vec := make([]float32, dim)
	mkv := func(seed int) []float32 {
		r := rand.New(rand.NewSource(int64(seed)))
		for i := range vec {
			vec[i] = r.Float32()
		}
		return vec
	}
	for k := 0; k < nSeg; k++ {
		if k > 0 {
			for u := 0; u < upserts; u++ {
				doc := rng.Intn(k * batch) // случайный док из предыдущих батчей
				id := fmt.Sprintf("doc:%06d", doc)
				if err := lvs.AddDoc(id, mkv(doc), scopeOf(doc, k), vmemSynthText(rng, vocab, nTok)); err != nil {
					t.Fatalf("upsert %s: %v", id, err)
				}
			}
		}
		for i := 0; i < batch; i++ {
			doc := k*batch + i
			id := fmt.Sprintf("doc:%06d", doc)
			if err := lvs.AddDoc(id, mkv(doc), scopeOf(doc, 0), vmemSynthText(rng, vocab, nTok)); err != nil {
				t.Fatalf("AddDoc %s: %v", id, err)
			}
		}
		lvs.FlushDeltaSync()
	}
	return lvs
}

// strictShadowMeasure — A/B одним стором: чередует флаг off/on по раундам
// (rounds на сторону), возвращает медианные QPS обеих сторон. Одинаковые
// запросы, одинаковое состояние LSM — вариативность сборки исключена.
func strictShadowMeasure(t *testing.T, lvs *LeveledVectorStore, run func() int, nQ, rounds int) (qpsOff, qpsOn float64) {
	t.Helper()
	measure := func() float64 {
		t0 := time.Now()
		hits := run()
		el := time.Since(t0)
		_ = hits
		return float64(nQ) / el.Seconds()
	}
	run() // прогрев (страницы, кеши)
	var off, on []float64
	for r := 0; r < rounds; r++ {
		lvs.cfg.StrictSegShadow = false
		off = append(off, measure())
		lvs.cfg.StrictSegShadow = true
		on = append(on, measure())
	}
	lvs.cfg.StrictSegShadow = false
	slices.Sort(off)
	slices.Sort(on)
	return off[len(off)/2], on[len(on)/2]
}

// TestStrictShadowQPSProbe — цена строгого затенения на мульти-сегментном
// состоянии. Два режима сегментов (HasKey радикально разный):
//   - frozen (dim=8): бинпоиск O(log n) по sorted-пермутации;
//   - hnsw (dim=300, >256): линейный O(N) обход keys.
//
// Порог (утверждён до прогона): ratio ≥ 0.9× — фикс дешёвый.
func TestStrictShadowQPSProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("профит-бенч: только полный прогон")
	}
	const (
		vocab   = 20000
		nTok    = 30
		nScopes = 100
	)

	verdict := func(name string, qpsOff, qpsOn float64) {
		ratio := qpsOn / qpsOff
		status := "ПОРОГ ПРОЙДЕН (≥0.9×)"
		if ratio < 0.9 {
			status = "ПОРОГ ПРОВАЛЕН (<0.9×)"
		}
		t.Logf("%s: baseline %.0f QPS → strict %.0f QPS, ratio %.3f× — %s", name, qpsOff, qpsOn, ratio, status)
	}

	// --- Режим A: frozen-сегменты (dim=8), 6×10k + 20% upsert-истории. ---
	{
		lvs := strictShadowCorpus(t, 8, 6, 10000, 2000, vocab, nTok, nScopes)
		nSegs := len(lvs.collectSegments())
		t.Logf("frozen-режим: %d сегментов", nSegs)

		const nQ = 20000
		rng := rand.New(rand.NewSource(7))
		queries := make([]string, nQ)
		scopes := make([]Filter, nQ)
		for i := range queries {
			queries[i] = vmemSynthText(rng, vocab, 4)
			scopes[i] = Filter{Eq: map[string]string{"scope": fmt.Sprintf("user:%03d", rng.Intn(nScopes))}}
		}
		qpsOff, qpsOn := strictShadowMeasure(t, lvs, func() int {
			hits := 0
			for i := 0; i < nQ; i++ {
				res, err := lvs.SearchTextFilter(queries[i], 10, scopes[i])
				if err != nil {
					t.Fatalf("SearchTextFilter: %v", err)
				}
				hits += len(res)
			}
			return hits
		}, nQ, 3)
		verdict("SEARCHTEXT+EQ frozen(6seg)", qpsOff, qpsOn)

		// Векторный путь на том же сторе.
		qv := make([][]float32, 2000)
		rngV := rand.New(rand.NewSource(11))
		for i := range qv {
			v := make([]float32, 8)
			for j := range v {
				v[j] = rngV.Float32()
			}
			qv[i] = v
		}
		qpsOff, qpsOn = strictShadowMeasure(t, lvs, func() int {
			hits := 0
			for i := 0; i < len(qv); i++ {
				res, err := lvs.SearchFilter(qv[i], 10, scopes[i])
				if err != nil {
					t.Fatalf("SearchFilter: %v", err)
				}
				hits += len(res)
			}
			return hits
		}, len(qv), 5)
		verdict("VSIM SearchFilter frozen(6seg)", qpsOff, qpsOn)
		lvs.Close()
	}

	// --- Режим B: hnsw-сегменты (dim=300), 3×6k + upsert-история. ---
	if os.Getenv("STRICT_PROBE_FROZEN_ONLY") != "" {
		return // профилировочный гейт: только режим A
	}
	{
		lvs := strictShadowCorpus(t, 300, 3, 6000, 1200, vocab, nTok, nScopes)
		nSegs := len(lvs.collectSegments())
		t.Logf("hnsw-режим: %d сегментов", nSegs)

		const nQ = 3000
		rng := rand.New(rand.NewSource(7))
		queries := make([]string, nQ)
		scopes := make([]Filter, nQ)
		for i := range queries {
			queries[i] = vmemSynthText(rng, vocab, 4)
			scopes[i] = Filter{Eq: map[string]string{"scope": fmt.Sprintf("user:%03d", rng.Intn(nScopes))}}
		}
		qpsOff, qpsOn := strictShadowMeasure(t, lvs, func() int {
			hits := 0
			for i := 0; i < nQ; i++ {
				res, err := lvs.SearchTextFilter(queries[i], 10, scopes[i])
				if err != nil {
					t.Fatalf("SearchTextFilter: %v", err)
				}
				hits += len(res)
			}
			return hits
		}, nQ, 3)
		verdict("SEARCHTEXT+EQ hnsw(3seg)", qpsOff, qpsOn)
		lvs.Close()
	}
}
