package main

// Покрытие КЛЮЧОМ: какая доля фактов вообще может быть крипто-стёрта.
//
// Главное свойство, которое здесь проверяется, — отчёт не должен завышать
// покрытие. Скоуп, у которого ключ ЕСТЬ, но часть фактов записана до
// включения шифрования, обязан показывать неполную долю: иначе аудитор
// примет нулевое покрытие за полное, а квитанция станет документом,
// утверждающим неправду.

import (
	"strconv"
	"testing"

	"kvstore/kvstore/vector"
)

func coverageFields(t *testing.T, e *execEnv, scope string) map[string]string {
	t.Helper()
	v := e.do("VMEM.COVERAGE", scope)
	if v.Typ == '-' {
		t.Fatalf("VMEM.COVERAGE: %v", v.Str)
	}
	if len(v.Array) == 0 {
		t.Fatalf("VMEM.COVERAGE пуст для scope %s", scope)
	}
	out := map[string]string{}
	row := v.Array[0].Array
	for i := 0; i+1 < len(row); i += 2 {
		out[row[i].Str] = row[i+1].Str
	}
	return out
}

func TestVMEMCoverage_KeyAxisDoesNotOverstate(t *testing.T) {
	e := newExecEnv(t)
	ring := enableKeyring(t, e.dir)
	_ = ring

	// Факт, записанный ДО включения шифрования: атрибута sealed нет.
	sealingActive = false
	e.do("VMEM.REMEMBER", "alice", "TEXT", "legacy fact written before encryption")

	// И два — уже под конвертом.
	sealingActive = true
	t.Cleanup(func() { sealingActive = false })
	e.do("VMEM.REMEMBER", "alice", "TEXT", "sealed fact one")
	e.do("VMEM.REMEMBER", "alice", "TEXT", "sealed fact two")

	f := coverageFields(t, e, "alice")
	if f["total"] != "3" {
		t.Fatalf("total=%s, ожидалось 3", f["total"])
	}
	if f["sealed"] != "2" || f["unsealed"] != "1" {
		t.Errorf("sealed=%s unsealed=%s, ожидалось 2 и 1", f["sealed"], f["unsealed"])
	}
	share, err := strconv.ParseFloat(f["sealed_share"], 64)
	if err != nil {
		t.Fatalf("sealed_share=%q: %v", f["sealed_share"], err)
	}
	// ⭐Суть проверки: доля НЕ единица, хотя ключ у скоупа есть. Отчёт,
	// считающий покрытие по наличию KEK, показал бы здесь 1.0 и соврал бы.
	if share > 0.7 {
		t.Errorf("sealed_share=%v — покрытие завышено: легаси-факт зачтён как стираемый", share)
	}
	if f["has_key"] != "1" {
		t.Errorf("has_key=%s, а ключ у скоупа есть", f["has_key"])
	}
}

// TestVMEMCoverage_KeyAxisReachesFull — ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ к тесту
// выше: на полностью запечатанном скоупе доля обязана быть единицей. Без него
// проверка «доля не 1.0» проходила бы и у отчёта, который всегда возвращает 0.
func TestVMEMCoverage_KeyAxisReachesFull(t *testing.T) {
	e := newExecEnv(t)
	enableKeyring(t, e.dir)
	sealingActive = true
	t.Cleanup(func() { sealingActive = false })

	e.do("VMEM.REMEMBER", "bob", "TEXT", "sealed from the start")
	f := coverageFields(t, e, "bob")
	if f["sealed"] != "1" || f["unsealed"] != "0" || f["sealed_share"] != "1.0000" {
		t.Errorf("sealed=%s unsealed=%s share=%s — ожидалось полное покрытие",
			f["sealed"], f["unsealed"], f["sealed_share"])
	}
}

// TestVMEMCoverage_FieldsStayPaired — поля отчёта не должны разъезжаться при
// добавлении новых: разбивка source:* сортируется отдельно от фиксированных,
// и ошибка в границе молча утащила бы фиксированное поле в хвост.
func TestVMEMCoverage_FieldsStayPaired(t *testing.T) {
	e := newExecEnv(t)
	enableKeyring(t, e.dir)
	sealingActive = true
	t.Cleanup(func() { sealingActive = false })

	e.do("VMEM.REMEMBER", "carol", "TEXT", "fact one", "SOURCE", "email-agent")
	e.do("VMEM.REMEMBER", "carol", "TEXT", "fact two", "SOURCE", "human")

	f := coverageFields(t, e, "carol")
	for _, name := range []string{"scope", "total", "sealed", "unsealed", "sealed_share",
		"has_key", "declared", "unknown", "absent", "declared_share", "revocable_share"} {
		if _, ok := f[name]; !ok {
			t.Errorf("поле %q пропало из отчёта", name)
		}
	}
	if f["source:email-agent"] != "1" || f["source:human"] != "1" {
		t.Errorf("разбивка по источникам разъехалась: %v", f)
	}
}

// TestKeyCoverage_IgnoresForeignVectors — посторонние векторы (не VMEM) в
// покрытие не входят: у них нет скоупа, и стирать их нечем.
func TestKeyCoverage_IgnoresForeignVectors(t *testing.T) {
	e := newExecEnv(t)
	enableKeyring(t, e.dir)
	sealingActive = true
	t.Cleanup(func() { sealingActive = false })

	e.do("VMEM.REMEMBER", "dave", "TEXT", "a fact")
	lvs := e.vec.(*vector.LeveledVectorStore)
	// 32 — размерность placeholder-вектора ступени 0, её задал факт выше.
	if err := lvs.AddDocTerms("foreign", make([]float32, 32),
		vector.Attributes{Cat: map[string]string{"lang": "ru"}}, nil); err != nil {
		t.Fatalf("AddDocTerms: %v", err)
	}

	reports := lvs.KeyCoverage("")
	for _, r := range reports {
		if r.Scope == "" {
			t.Error("посторонний вектор попал в покрытие как отдельный scope")
		}
		if r.Scope == "dave" && r.Total != 1 {
			t.Errorf("scope dave: total=%d, ожидалось 1", r.Total)
		}
	}
}
