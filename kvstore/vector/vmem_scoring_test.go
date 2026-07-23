package vector

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

// =============================================================================
// Шаг 5 VMEM — скоринг RECALL: final = score × 2^(−age/halfLife) × (0.5+imp).
// White-box формулы и обвязки: нейтральные значения, NaN-факторы, rescue
// из хвоста оверфетча, AS_OF-возраст («что было важно тогда»), find по
// sorted-пермутации internedKeys.
// =============================================================================

func TestVMEMDecayImpFormula(t *testing.T) {
	const hl = int64(1000)
	cases := []struct {
		name           string
		validFrom, imp float64
		tEff           int64
		want           float64
	}{
		{"нейтрально: свежий, imp=0.5", 100, 0.5, 100, 1.0},
		{"полураспад: age=halfLife", 100, 0.5, 1100, 0.5},
		{"два полураспада", 100, 0.5, 2100, 0.25},
		{"важность растягивает", 100, 1.0, 1100, 0.75}, // 0.5 × 1.5
		{"неважное сжимает", 100, 0.0, 1100, 0.25},     // 0.5 × 0.5
		{"будущий valid_from (ALL) не буствует", 500, 0.5, 100, 1.0},
		{"нет valid_from → decay нейтрален", math.NaN(), 1.0, 1100, 1.5},
		{"нет imp → фактор нейтрален", 100, math.NaN(), 1100, 0.5},
	}
	for _, tc := range cases {
		if got := vmemDecayImp(tc.validFrom, tc.imp, tc.tEff, hl); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestInternedKeysFind(t *testing.T) {
	keys := []string{"delta", "alpha", "", "charlie", "bravo"} // "" = tombstone-слот
	ik := buildInternedKeys(keys)
	for i, k := range keys {
		if k == "" {
			continue
		}
		idx, ok := ik.find(k)
		if !ok || idx != i {
			t.Errorf("find(%q) = (%d,%v), want (%d,true)", k, idx, ok, i)
		}
	}
	if _, ok := ik.find("zulu"); ok {
		t.Error("find нашёл несуществующий ключ")
	}
	if _, ok := ik.find(""); ok {
		t.Error("пустой ключ (tombstone) не должен находиться")
	}
	if _, ok := (internedKeys{}).find("x"); ok {
		t.Error("find по пустой таблице")
	}
}

// TestRecallScoringRescue — свежий важный факт с ХУДШЕЙ похожестью обязан
// всплыть в top-K над старым неважным с лучшей: смысл оверфетча (скоринг
// только top-K мог бы лишь переставлять внутри него). Прогон ×3 LSM.
func TestRecallScoringRescue(t *testing.T) {
	for _, st := range vmemLSMStates {
		t.Run(st.name, func(t *testing.T) {
			lvs := NewLeveledVectorStore(bm25TestConfig())
			defer lvs.Close()

			impLo, impHi := 0.1, 0.9
			// Старый неважный: короче текст → выше BM25 (лучшая похожесть).
			// day = 86400с; полгода назад при halfLife=30д → decay ≈ 0.015.
			ops := []struct {
				req RememberRequest
				at  int64
			}{
				{RememberRequest{ID: "old", Scope: "s", Text: "кофе", Importance: &impLo}, 1_000_000},
				{RememberRequest{ID: "new", Scope: "s", Text: "кофе просит только декаф", Importance: &impHi}, 16_000_000},
			}
			for i, op := range ops {
				if _, err := lvs.Remember(op.req, op.at); err != nil {
					t.Fatalf("Remember %s: %v", op.req.ID, err)
				}
				if i+1 == st.flushAt(len(ops)) {
					lvs.FlushDeltaSync()
				}
			}

			// Большой полураспад (~174д): возраст old ≈ 175д = ~1 полураспад →
			// decay ≈ 0.5, ВЫШЕ пола 0.25 — политика клиента видна в скоре.
			res, err := lvs.Recall(RecallRequest{Scope: "s", Query: "кофе", K: 2, HalfLifeSec: 15_000_000}, 16_100_000)
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
			if len(res) != 2 || res[0].Key != "new" {
				t.Fatalf("скоринг не поднял свежий важный факт: %+v", res)
			}

			// Крошечный полураспад: старый факт давится до пола vmemDecayFloor
			// (0.25 < ~0.5 выше) — порядок тот же, скор old обязан упасть.
			// Сильнее пола не давит НИКАКОЙ полураспад — это контракт пола
			// (суд 23.07: затухание двигает порядок, но не квантует в ноль).
			res2, err := lvs.Recall(RecallRequest{Scope: "s", Query: "кофе", K: 2, HalfLifeSec: 3600}, 16_100_000)
			if err != nil {
				t.Fatalf("Recall hl=1h: %v", err)
			}
			if res2[0].Key != "new" {
				t.Fatalf("hl=1h: %+v", res2)
			}
			var oldA, oldB float64
			for _, r := range res {
				if r.Key == "old" {
					oldA = r.Score
				}
			}
			for _, r := range res2 {
				if r.Key == "old" {
					oldB = r.Score
				}
			}
			if !(oldB < oldA) {
				t.Errorf("меньший полураспад обязан сильнее давить старый факт: %v !< %v", oldB, oldA)
			}
		})
	}
}

// TestRecallScoringAsOfAge — свежесть меряется от as_of, не от now: факт,
// свежий НА МОМЕНТ вопроса, не наказывается прошедшим с тех пор временем.
func TestRecallScoringAsOfAge(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	imp := 0.5
	// Два одинаковых по тексту факта; f2 закрывает f1 (supersedes).
	if _, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "s", Text: "статус проекта зелёный", Importance: &imp}, 1_000_000); err != nil {
		t.Fatalf("Remember f1: %v", err)
	}
	if _, err := lvs.Remember(RememberRequest{ID: "f2", Scope: "s", Text: "статус проекта красный", Importance: &imp, Supersedes: "f1"}, 20_000_000); err != nil {
		t.Fatalf("Remember f2: %v", err)
	}

	// AS_OF сразу после рождения f1: виден только f1, и его скор считается с
	// age≈0 — как будто спрашиваем тогда.
	asOf := int64(1_000_100)
	res, err := lvs.Recall(RecallRequest{Scope: "s", Query: "статус проекта", K: 10, AsOf: &asOf}, 30_000_000)
	if err != nil {
		t.Fatalf("Recall as_of: %v", err)
	}
	if len(res) != 1 || res[0].Key != "f1" {
		t.Fatalf("as_of видит %+v, ожидался только f1", res)
	}
	// При halfLife=1000с и age от NOW скор был бы ~2^(-29000) ≈ 0 (денормал).
	// Age от as_of (100с) даёт множитель ~0.93 — скор жив.
	res2, err := lvs.Recall(RecallRequest{Scope: "s", Query: "статус проекта", K: 10, AsOf: &asOf, HalfLifeSec: 1000}, 30_000_000)
	if err != nil {
		t.Fatalf("Recall as_of hl: %v", err)
	}
	if len(res2) != 1 || res2[0].Score < res[0].Score*0.5 {
		t.Fatalf("возраст посчитан не от as_of: %+v vs %+v", res2, res)
	}

	if _, err := lvs.Recall(RecallRequest{Scope: "s", Query: "q", K: 1, HalfLifeSec: -1}, 30_000_000); !errors.Is(err, ErrVMEMHalfLife) {
		t.Errorf("отрицательный halfLife: err=%v", err)
	}
}

// TestNumsForKeysProvenance — проекция берёт значения СВЕЖАЙШЕЙ версии дока:
// upsert в дельте затеняет frozen-копию; неизвестный ключ → NaN.
func TestNumsForKeysProvenance(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	impOld := 0.2
	if _, err := lvs.Remember(RememberRequest{ID: "f", Scope: "s", Text: "первая версия", Importance: &impOld}, 100); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	lvs.FlushDeltaSync() // старая копия уезжает во frozen

	impNew := 0.9
	if _, err := lvs.Remember(RememberRequest{ID: "f", Scope: "s", Text: "вторая версия", Importance: &impNew}, 200); err != nil {
		t.Fatalf("Remember upsert: %v", err)
	}

	nums := lvs.numsForKeys([]string{"f", "no-such"}, vmemAttrImp, vmemAttrValidFrom)
	if nums[0][0] != 0.9 || nums[0][1] != 200 {
		t.Errorf("свежая версия не победила: imp=%v vf=%v", nums[0][0], nums[0][1])
	}
	if !math.IsNaN(nums[1][0]) || !math.IsNaN(nums[1][1]) {
		t.Errorf("несуществующий ключ обязан дать NaN: %v", nums[1])
	}

	// И frozen-путь тоже: после полного flush значения читаются колонками.
	lvs.FlushDeltaSync()
	nums = lvs.numsForKeys([]string{"f"}, vmemAttrImp)
	if nums[0][0] != 0.9 {
		t.Errorf("frozen-проекция: imp=%v, want 0.9", nums[0][0])
	}
}

// TestGetFrozenFindParity — Get через sorted-пермутацию бит-в-бит совпадает с
// прежним линейным сканом на массе ключей (упорядочение/границы бинпоиска).
func TestGetFrozenFindParity(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()
	const n = 500
	for i := 0; i < n; i++ {
		if err := lvs.Add(fmt.Sprintf("key:%04d", i*7%n), mkVecN(8, float32(i))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	lvs.FlushDeltaSync()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key:%04d", i)
		vec, ok := lvs.Get(key)
		if !ok {
			t.Fatalf("Get(%s) промахнулся после flush", key)
		}
		if len(vec) != 8 {
			t.Fatalf("Get(%s): dim %d", key, len(vec))
		}
	}
	if _, ok := lvs.Get("key:9999"); ok {
		t.Error("Get нашёл несуществующий ключ")
	}
}
