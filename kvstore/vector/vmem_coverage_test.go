package vector

import (
	"math"
	"testing"
)

// Покрытие провенансом: проверяется то, ради чего метрика существует —
// различение ТРЁХ состояний источника. Два из них («конкретный» и литеральный
// unknown) отзываемы массово, третье (атрибута нет — факты, записанные до
// провенанса) не отзываемо и никаким предикатом не выражается. Если метрика
// сольёт unknown и отсутствие, она соврёт ровно в ту сторону, в которую нам
// выгодно, — покажет покрытие лучше реального.
func TestVMEMProvenanceCoverage(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	// Факт с объявленным источником и факт без него (штамп unknown).
	if _, err := lvs.Remember(RememberRequest{
		ID: "declared", Scope: "user:a", Text: "дедлайн март", Source: "human",
	}, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := lvs.Remember(RememberRequest{
		ID: "unknown1", Scope: "user:a", Text: "дедлайн апрель",
	}, 1000); err != nil {
		t.Fatal(err)
	}
	// Факт ДРУГОГО scope — в отчёт по user:a попадать не должен.
	if _, err := lvs.Remember(RememberRequest{
		ID: "other", Scope: "user:b", Text: "дедлайн май", Source: "human",
	}, 1000); err != nil {
		t.Fatal(err)
	}
	// Легаси: факт со scope, но БЕЗ колонки source — то, что физически лежит
	// у пользователей, писавших память до появления провенанса. Кладём мимо
	// Remember, потому что Remember штампует unknown по контракту.
	legacy := RememberedDoc{
		ID:  "legacy",
		Vec: vmemPlaceholderVector("legacy", lvs.dim),
		Attrs: Attributes{
			Cat: map[string]string{vmemAttrScope: "user:a"},
			Num: map[string]float64{
				vmemAttrValidFrom: 1000, vmemAttrValidTo: float64(vmemOpenValidTo),
				vmemAttrExpiresAt: float64(vmemOpenValidTo), vmemAttrImp: 0.5,
			},
		},
		Terms: []TermTF{{Term: "дедлайн", TF: 1}},
	}
	if err := lvs.AddDocTerms(legacy.ID, legacy.Vec, legacy.Attrs, legacy.Terms); err != nil {
		t.Fatalf("AddDocTerms legacy: %v", err)
	}

	reps := lvs.ProvenanceCoverage("user:a")
	if len(reps) != 1 {
		t.Fatalf("отчётов %d, ожидался ровно один (user:a): %+v", len(reps), reps)
	}
	r := reps[0]
	if r.Total != 3 {
		t.Errorf("всего фактов %d, ожидалось 3 (чужой scope не считается)", r.Total)
	}
	if got := r.BySource["human"]; got != 1 {
		t.Errorf("с объявленным источником %d, ожидался 1", got)
	}
	if got := r.BySource["unknown"]; got != 1 {
		t.Errorf("со штампом unknown %d, ожидался 1", got)
	}
	if got := r.BySource[""]; got != 1 {
		t.Errorf("без атрибута %d, ожидался 1 — слепое пятно обязано считаться отдельно", got)
	}
	if math.Abs(r.Declared()-1.0/3.0) > 1e-9 {
		t.Errorf("доля объявленных %v, ожидалась 1/3", r.Declared())
	}
	// Потолок восстановления: отозвать можно всё, кроме факта без атрибута.
	if math.Abs(r.Revocable()-2.0/3.0) > 1e-9 {
		t.Errorf("доля отзываемых %v, ожидалась 2/3", r.Revocable())
	}

	// Без сужения — оба scope, и чужой не потерян.
	if all := lvs.ProvenanceCoverage(""); len(all) != 2 {
		t.Errorf("без сужения отчётов %d, ожидалось 2", len(all))
	}
}
