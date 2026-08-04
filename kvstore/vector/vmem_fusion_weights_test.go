package vector

import (
	"errors"
	"math"
	"testing"
)

// =============================================================================
// Рычаг весов плеч RRF и дефолт полураспада — оба изменения родились из замера
// на LoCoMo (BENCHMARKS.md §9), и оба закреплены здесь ПОВЕДЕНИЕМ, а не
// сверкой константы. Тест «дефолт равен 365 дням» пережил бы возврат к 30
// правкой одного числа; тест на цену дефолта — нет.
// =============================================================================

// fwStore — стор с двумя фактами, плечи которых ТЯНУТ В РАЗНЫЕ СТОРОНЫ.
// Это предусловие всей группы: если бы плечи соглашались, любой вес давал бы
// один и тот же порядок, и выключенное плечо было бы неотличимо от рабочего —
// тест зеленел бы по неверной причине.
func fwStore(t *testing.T, lexAt, vecAt int64) (*LeveledVectorStore, string, string) {
	t.Helper()
	lvs := NewLeveledVectorStore(bm25TestConfig())
	t.Cleanup(func() { lvs.Close() })
	facts := []struct {
		id, text string
		vec      []float32
		at       int64
	}{
		// дословное совпадение с запросом, вектор далёкий
		{"lex", "aerial yoga class schedule", []float32{0, 1}, lexAt},
		// ни одного общего слова, вектор в упор
		{"vec", "completely unrelated wording here", []float32{1, 0}, vecAt},
	}
	for _, f := range facts {
		if _, err := lvs.Remember(RememberRequest{
			ID: f.id, Scope: "s", Text: f.text, Vector: f.vec,
			ValidFrom: f.at,
		}, f.at); err != nil {
			t.Fatalf("Remember %s: %v", f.id, err)
		}
	}
	return lvs, "lex", "vec"
}

func withWeights(req RecallRequest, text, vec float64) RecallRequest {
	out := req
	out.WeightText, out.WeightVec = &text, &vec
	return out
}

func fwFirst(t *testing.T, lvs *LeveledVectorStore, req RecallRequest, now int64) string {
	t.Helper()
	res, err := lvs.Recall(req, now)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("пустая выдача — корпус не тот, тест ничего не проверяет")
	}
	return res[0].Key
}

// TestVMEMFusionWeightsLever — рычаг делает ровно то, что обещает: меняет вес
// голоса плеча в ранговом слиянии.
func TestVMEMFusionWeightsLever(t *testing.T) {
	now := int64(1_000_000)
	lvs, lexKey, vecKey := fwStore(t, now, now)
	base := RecallRequest{Scope: "s", Query: "aerial yoga class schedule",
		K: 2, Vector: []float32{1, 0}}

	t.Run("плечи спорят — иначе группа бессмысленна", func(t *testing.T) {
		onlyText := fwFirst(t, lvs, withWeights(base, 1, 0), now)
		onlyVec := fwFirst(t, lvs, withWeights(base, 0, 1), now)
		if onlyText == onlyVec {
			t.Fatalf("плечи согласны (оба дают %s) — корпус не различает веса",
				onlyText)
		}
		if onlyText != lexKey {
			t.Errorf("вес (1,0): верх %s, ждали лексического фаворита %s",
				onlyText, lexKey)
		}
		if onlyVec != vecKey {
			t.Errorf("вес (0,1): верх %s, ждали векторного фаворита %s",
				onlyVec, vecKey)
		}
	})

	t.Run("равные веса ≡ отсутствие весов", func(t *testing.T) {
		got := fwFirst(t, lvs, withWeights(base, 1, 1), now)
		want := fwFirst(t, lvs, base, now)
		if got != want {
			t.Errorf("WEIGHTS 1 1 дало %s, запрос без весов — %s: единичный "+
				"вес обязан быть но-опом", got, want)
		}
	})

	t.Run("перевес смещает верхушку, не обнуляя плечо", func(t *testing.T) {
		// 100:1, а не 1:0 — проверяется именно ВЕС, а не выключатель.
		if got := fwFirst(t, lvs, withWeights(base, 100, 1), now); got != lexKey {
			t.Errorf("перевес лексики дал %s, ждали %s", got, lexKey)
		}
		if got := fwFirst(t, lvs, withWeights(base, 1, 100), now); got != vecKey {
			t.Errorf("перевес вектора дал %s, ждали %s", got, vecKey)
		}
	})

	t.Run("выключенное плечо не теряет своих кандидатов", func(t *testing.T) {
		// Вес 0 гасит ГОЛОС, но кандидат остаётся в пуле. Иначе выдача молча
		// укоротилась бы — дефект, который выглядит как скромность.
		res, err := lvs.Recall(withWeights(base, 1, 0), now)
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if len(res) != 2 {
			t.Fatalf("в выдаче %d фактов, ждали 2: вес 0 гасит голос, а не "+
				"выбрасывает кандидата", len(res))
		}
	})
}

// TestVMEMFusionWeightsRejected — бессмысленные веса ПАДАЮТ, а не работают
// наполовину. Молча принятый вес — иллюзия управления: команда «сработала»,
// не изменив ничего.
func TestVMEMFusionWeightsRejected(t *testing.T) {
	now := int64(1_000_000)
	lvs, _, _ := fwStore(t, now, now)
	vec := []float32{1, 0}

	cases := []struct {
		name       string
		text, vecW float64
		vector     []float32
		want       error
	}{
		{"оба нуля — ранжировать нечем", 0, 0, vec, ErrVMEMWeights},
		{"отрицательный текстовый", -1, 1, vec, ErrVMEMWeights},
		{"отрицательный векторный", 1, -0.5, vec, ErrVMEMWeights},
		{"NaN проходит любое сравнение — ловится явно", math.NaN(), 1, vec, ErrVMEMWeights},
		{"NaN в векторном плече", 1, math.NaN(), vec, ErrVMEMWeights},
		{"веса без вектора — применять не к чему", 1, 0, nil, ErrVMEMWeightsNoVec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := withWeights(RecallRequest{Scope: "s", Query: "aerial yoga",
				K: 5, Vector: tc.vector}, tc.text, tc.vecW)
			if _, err := lvs.Recall(req, now); !errors.Is(err, tc.want) {
				t.Errorf("получили %v, ждали %v", err, tc.want)
			}
		})
	}
}

// TestVMEMExplainReportsAppliedWeights — EXPLAIN печатает ФАКТИЧЕСКИ
// применённые веса. Объяснение, молчащее о весе, разойдётся с ранжированием
// ровно в разборе инцидента, где цена ошибки максимальна.
func TestVMEMExplainReportsAppliedWeights(t *testing.T) {
	now := int64(1_000_000)
	lvs, _, _ := fwStore(t, now, now)
	base := RecallRequest{Scope: "s", Query: "aerial yoga", K: 5,
		Vector: []float32{1, 0}}

	t.Run("без весов печатается 1.0, а не ноль структуры", func(t *testing.T) {
		ex, err := lvs.Explain(base, now)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if ex.WeightText != 1 || ex.WeightVec != 1 {
			t.Errorf("веса по умолчанию (%v,%v), ждали (1,1)",
				ex.WeightText, ex.WeightVec)
		}
	})

	t.Run("заданные веса доезжают до объяснения", func(t *testing.T) {
		ex, err := lvs.Explain(withWeights(base, 3, 0.25), now)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if ex.WeightText != 3 || ex.WeightVec != 0.25 {
			t.Errorf("веса в объяснении (%v,%v), ждали (3,0.25)",
				ex.WeightText, ex.WeightVec)
		}
	})

	t.Run("на BM25-only пути веса нейтральны", func(t *testing.T) {
		ex, err := lvs.Explain(RecallRequest{Scope: "s", Query: "aerial", K: 5}, now)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if ex.Hybrid {
			t.Fatal("путь оказался гибридным — тест проверяет не то")
		}
		if ex.WeightText != 1 || ex.WeightVec != 1 {
			t.Errorf("на одноплечем пути веса (%v,%v), ждали (1,1)",
				ex.WeightText, ex.WeightVec)
		}
	})
}

// TestVMEMDefaultHalfLifeSurvivesLongHistory — ЦЕНА дефолта, а не его число.
//
// Замер на LoCoMo: при полураспаде 30 дней штраф λ·age/halfLife доходит до
// +39.7 в знаменателе 1/(60+rank+…), и верный факт восьмимесячной давности
// проигрывает свежему, стоящему сороковым по релевантности (−11.8 пункта
// recall@5). Дефолт поднят до года. Здесь закреплено именно поведение:
// релевантный старый факт обязан пережить полугодовой возраст НА ДЕФОЛТЕ — и
// обязан утонуть, если полураспад вернуть к 30 дням явно.
func TestVMEMDefaultHalfLifeSurvivesLongHistory(t *testing.T) {
	const day = int64(24 * 3600)
	now := int64(2_000_000_000)
	old := now - 180*day

	// ⭐Корпус подобран под АРИФМЕТИКУ слияния, а не на глаз. Старый факт
	// стоит первым в обоих плечах, свежий отвлекающий — заметно ниже.
	// Условие обгона: 2/(61+λ·age/HL) против 2/(60+r), то есть старый держится,
	// пока r > 1 + λ·age/HL.
	//   HL=365 дн: штраф 5·180/365 = 2.47 → нужен разрыв r ≥ 4;
	//   HL=30  дн: штраф 5·180/30  = 30.0 → тонет против любого r ≤ 31.
	// Промежуточные факты тоже старые: будь они свежими, они обогнали бы
	// цель уже на дефолте, и тест поймал бы их, а не затухание.
	facts := []struct {
		id, text string
		vec      []float32
		at       int64
	}{
		{"target", "aerial yoga class schedule morning", []float32{1.00, 0.00}, old},
		{"mid1", "aerial yoga class schedule", []float32{0.99, 0.14}, old},
		{"mid2", "aerial yoga class", []float32{0.97, 0.24}, old},
		{"mid3", "aerial yoga session", []float32{0.94, 0.34}, old},
		{"fresh", "aerial pilates", []float32{0.90, 0.44}, now},
	}
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()
	for _, f := range facts {
		if _, err := lvs.Remember(RememberRequest{
			ID: f.id, Scope: "s", Text: f.text, Vector: f.vec, ValidFrom: f.at,
		}, f.at); err != nil {
			t.Fatalf("Remember %s: %v", f.id, err)
		}
	}

	req := RecallRequest{Scope: "s", Query: "aerial yoga class schedule morning",
		K: 5, Vector: []float32{1, 0}}

	// Предусловие: без затухания цель первая, а свежий отвлекающий — не ближе
	// четвёртого места. Если корпус этого не даёт, дальнейшие проверки
	// зеленели бы (или краснели) не по той причине.
	t.Run("предусловие: разрыв рангов достаточен", func(t *testing.T) {
		noDecay := req
		noDecay.HalfLifeSec = 100 * 365 * day // затухание практически выключено
		res, err := lvs.Recall(noDecay, now)
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if len(res) == 0 || res[0].Key != "target" {
			t.Fatalf("без затухания верх = %v, ждали target — корпус не тот", res)
		}
		freshRank := 0
		for i, r := range res {
			if r.Key == "fresh" {
				freshRank = i + 1
			}
		}
		if freshRank < 4 {
			t.Fatalf("свежий отвлекающий на месте %d, нужен ≥4: при таком "+
				"разрыве затухание перевернуло бы выдачу на любом полураспаде",
				freshRank)
		}
	})

	t.Run("на дефолте старый релевантный факт держится", func(t *testing.T) {
		if got := fwFirst(t, lvs, req, now); got != "target" {
			t.Errorf("верх = %s, ждали target: дефолтный полураспад снова "+
				"топит длинную историю", got)
		}
	})

	// Обратная сторона: механизм не выключен, он настроен. С прежним дефолтом
	// в 30 дней тот же факт обязан утонуть — если и здесь он наверху, значит
	// затухание не действует вовсе, и проверка выше ничего не значила.
	t.Run("с прежним дефолтом 30 дней тот же факт тонет", func(t *testing.T) {
		short := req
		short.HalfLifeSec = 30 * day
		if got := fwFirst(t, lvs, short, now); got == "target" {
			t.Error("при полураспаде 30 дней старый факт всё ещё наверху — " +
				"затухание не действует, и проверка дефолта бессмысленна")
		}
	})
}
