package vector

import (
	"fmt"
	"math"
	"testing"
)

// =============================================================================
// EXPLAIN (примитив 4): проверяется не «команда что-то отдаёт», а два свойства,
// без которых она вредна.
//
//  1. РАВЕНСТВО БОЕВОМУ ПУТИ. Объяснение, разошедшееся с ранжированием, хуже
//     отсутствия объяснения: им пользуются в разборе инцидента, где на него
//     опираются в решении «кого отзывать». Тест гоняет обе команды на одних
//     данных во всех режимах и требует совпадения порядка И скоров бит-в-бит.
//     Именно этот тест оправдывает решение вести трассу внутри Recall, а не
//     писать «объясняющую» копию скоринга рядом.
//  2. ПРИЧИНА ОТСЕВА НАЗВАНА ВЕРНО. «Факта не видно» — три разных диагноза
//     (стёрт / закрыт новой версией / отозван), и путать их нельзя: первый
//     необратим, второй нормальная жизнь, третий — след инцидента.
// =============================================================================

// explainKept — id фактов, попавших в выдачу, в порядке ранга.
func explainKept(t *testing.T, ex ExplainResult) []string {
	t.Helper()
	out := []string{}
	for _, f := range ex.Facts {
		if f.Drop == "" {
			out = append(out, f.Key)
		}
	}
	return out
}

// explainOf — запись по id (в том числе отсеянная).
func explainOf(t *testing.T, ex ExplainResult, id string) ExplainedFact {
	t.Helper()
	for _, f := range ex.Facts {
		if f.Key == id {
			return f
		}
	}
	t.Fatalf("в разложении нет факта %q", id)
	return ExplainedFact{}
}

// TestVMEMExplainMatchesRecall — EXPLAIN обязан объяснять ТОТ ЖЕ ответ, что
// отдал RECALL: тот же состав, тот же порядок, те же финальные скоры. Гоняется
// по всем режимам запроса и по обоим размещениям LSM (дельта и сегмент —
// разный код проекции атрибутов).
func TestVMEMExplainMatchesRecall(t *testing.T) {
	const dim = 8
	for _, state := range []string{"delta", "flushed"} {
		lvs := NewLeveledVectorStore(bm25TestConfig())
		// Importance НАРОЧНО не 0.5 ни у одного факта: при нейтральном 0.5
		// множитель равен ровно 1.0, и проверка «бит-в-бит» перестаёт что-либо
		// проверять — мутация «трасса пишет скор до множителей» проходила мимо
		// теста именно из-за этого (проверено мутацией 27.07).
		facts := []struct {
			id, text, source, typ string
			imp                   float64
			at                    int64
		}{
			{"a", "дедлайн проекта март", "human", "event", 0.9, 1000},
			{"b", "дедлайн проекта апрель", "web-scraper", "event", 0.1, 1200},
			{"c", "дедлайн проекта май", "email-agent", "task", 0.75, 1400},
			{"d", "совсем про другое", "human", "event", 0.2, 1600},
		}
		for i, f := range facts {
			imp := f.imp
			if _, err := lvs.Remember(RememberRequest{
				ID: f.id, Scope: "user:a", Text: f.text, Source: f.source,
				Type: f.typ, Importance: &imp, Vector: mkVecN(dim, float32(i)),
			}, f.at); err != nil {
				t.Fatalf("Remember %s: %v", f.id, err)
			}
		}
		if state == "flushed" {
			lvs.FlushDeltaSync()
		}
		// Отзыв одного источника: в выдаче его быть не должно, а в разложении
		// он обязан быть виден с названной причиной.
		if _, err := lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 1800); err != nil {
			t.Fatalf("Quarantine: %v", err)
		}

		asOf := int64(1300)
		cases := []struct {
			name string
			req  RecallRequest
		}{
			{"дефолт", RecallRequest{}},
			{"ASOF", RecallRequest{AsOf: &asOf}},
			{"ALL", RecallRequest{All: true}},
			{"SOURCE", RecallRequest{SourceEq: "human"}},
			{"TYPE", RecallRequest{TypeEq: "event"}},
			{"HALFLIFE", RecallRequest{HalfLifeSec: 60}},
			{"hybrid", RecallRequest{Vector: mkVecN(dim, 1)}},
			{"hybrid+ASOF", RecallRequest{Vector: mkVecN(dim, 1), AsOf: &asOf}},
			{"K=1", RecallRequest{K: 1}},
		}
		for _, tc := range cases {
			req := tc.req
			req.Scope, req.Query = "user:a", "дедлайн проекта"
			if req.K == 0 {
				req.K = 10
			}
			got, err := lvs.Recall(req, 2000)
			if err != nil {
				t.Fatalf("%s/%s: Recall: %v", state, tc.name, err)
			}
			ex, err := lvs.Explain(req, 2000)
			if err != nil {
				t.Fatalf("%s/%s: Explain: %v", state, tc.name, err)
			}
			kept := explainKept(t, ex)
			if len(kept) != len(got) {
				t.Fatalf("%s/%s: RECALL отдал %d фактов, EXPLAIN объясняет %d: %v vs %v",
					state, tc.name, len(got), len(kept), got, kept)
			}
			for i := range got {
				if got[i].Key != kept[i] {
					t.Errorf("%s/%s: порядок разошёлся на месте %d: RECALL %q, EXPLAIN %q",
						state, tc.name, i+1, got[i].Key, kept[i])
				}
				f := explainOf(t, ex, got[i].Key)
				if f.Final != got[i].Score { // именно бит-в-бит: это одна и та же арифметика
					t.Errorf("%s/%s: скор %q разошёлся: RECALL %v, EXPLAIN %v",
						state, tc.name, got[i].Key, got[i].Score, f.Final)
				}
				if f.Rank != i+1 {
					t.Errorf("%s/%s: ранг %q = %d, ожидался %d", state, tc.name, f.Key, f.Rank, i+1)
				}
			}
		}
		lvs.Close()
	}
}

// TestVMEMExplainNamesDropReason — три разных «факта не видно» получают три
// разных диагноза, а не общее «нет в выдаче».
func TestVMEMExplainNamesDropReason(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	// erased — стёрт по TTL; superseded — закрыт новой версией; poisoned —
	// отозван карантином; alien — чужой тип; ok — доживает до выдачи.
	mk := func(id, text, source, typ string, ttl int64, at int64) {
		t.Helper()
		if _, err := lvs.Remember(RememberRequest{
			ID: id, Scope: "user:a", Text: text, Source: source, Type: typ, TTL: ttl,
		}, at); err != nil {
			t.Fatalf("Remember %s: %v", id, err)
		}
	}
	mk("erased", "дедлайн проекта январь", "human", "event", 100, 1000)
	mk("superseded", "дедлайн проекта февраль", "human", "event", 0, 1000)
	mk("poisoned", "дедлайн проекта март", "web-scraper", "event", 0, 1000)
	mk("alien", "дедлайн проекта апрель", "human", "note", 0, 1000)
	mk("ok", "дедлайн проекта май", "human", "event", 0, 1000)

	if _, err := lvs.Remember(RememberRequest{
		ID: "newer", Scope: "user:a", Text: "дедлайн проекта июнь",
		Source: "human", Type: "event", Supersedes: "superseded",
	}, 1500); err != nil {
		t.Fatalf("Remember newer: %v", err)
	}
	if _, err := lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 1800); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	ex, err := lvs.Explain(RecallRequest{
		Scope: "user:a", Query: "дедлайн проекта", K: 10, TypeEq: "event",
	}, 2000)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	want := map[string]DropReason{
		"poisoned": DropQuarantine, // отзыв судится ТОЛЬКО пере-судом → всегда виден
		"ok":       "",
		"newer":    "",
	}
	for id, reason := range want {
		got := explainOf(t, ex, id).Drop
		if got != reason {
			t.Errorf("%s: причина %q, ожидалась %q", id, got, reason)
		}
	}
	// А закрытый новой версией и отсечённый по типу кандидатами не становятся
	// вовсе: обе оси судятся ещё пре-фильтром индекса. Их невидимость
	// выражается ОТСУТСТВИЕМ в разложении, и это честнее, чем показать их с
	// придуманным вердиктом: на этом запросе система про них ничего не считала.
	for _, id := range []string{"superseded", "alien"} {
		for _, f := range ex.Facts {
			if f.Key == id {
				t.Errorf("%s стал кандидатом дефолтного запроса (вердикт %q)", id, f.Drop)
			}
		}
	}
	// Способ увидеть — снять то, что их сняло: ALL для валидности и отказ от
	// TYPE для типа.
	exAll, err := lvs.Explain(RecallRequest{
		Scope: "user:a", Query: "дедлайн проекта", K: 10, All: true,
	}, 2000)
	if err != nil {
		t.Fatalf("Explain ALL: %v", err)
	}
	for _, id := range []string{"superseded", "alien"} {
		if d := explainOf(t, exAll, id).Drop; d != "" {
			t.Errorf("в режиме ALL без TYPE факт %s получил вердикт %q, ожидалось «в выдаче»", id, d)
		}
	}
	// Стёртого нет и быть не должно НИ В ОДНОМ режиме: erasure снимается
	// пре-фильтром индекса, и ALL его тоже не воскрешает. Право быть забытым
	// сильнее и машины времени, и объяснимости — решение, а не недосмотр.
	for _, f := range exAll.Facts {
		if f.Key == "erased" {
			t.Errorf("стёртый факт виден в разложении ALL с вердиктом %q", f.Drop)
		}
	}
}

// TestVMEMExplainStaleCopyValidity — DropValidity достижим ровно там, где живёт
// пере-суд: открытая копия из СТАРОГО сегмента, чью свежайшую версию уже
// закрыли (регресс шага 8, TestVMEMSupersedeTwoSegmentLeak). Разложение обязано
// показать её как отсеянную валидностью, а не молча проглотить: именно этот
// класс расхождения «копия vs свежайшая версия» дважды ловил нас в сторе.
func TestVMEMExplainStaleCopyValidity(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	if _, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "u", Text: "alpha beta unique1"}, 100); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync() // сегмент A: открытый f1
	if _, err := lvs.Remember(RememberRequest{
		ID: "f2", Scope: "u", Text: "alpha beta unique2", Supersedes: "f1",
	}, 200); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync() // сегмент B: закрытый f1 + наследник f2

	ex, err := lvs.Explain(RecallRequest{Scope: "u", Query: "alpha beta", K: 10}, 300)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if d := explainOf(t, ex, "f1").Drop; d != DropValidity {
		t.Errorf("стейл-копия f1 получила вердикт %q, ожидался %q", d, DropValidity)
	}
	if d := explainOf(t, ex, "f2").Drop; d != "" {
		t.Errorf("наследник f2 отсеян с причиной %q", d)
	}
}

// TestVMEMExplainBelowK — «правда в памяти есть, но её не слышно» обязано
// называться своим именем, а не сливаться с отфильтрованным.
func TestVMEMExplainBelowK(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	for i := 1; i <= 4; i++ {
		if _, err := lvs.Remember(RememberRequest{
			ID: fmt.Sprintf("f%d", i), Scope: "user:a",
			Text: fmt.Sprintf("дедлайн проекта вариант %d", i), Source: "human",
		}, 1000); err != nil {
			t.Fatalf("Remember f%d: %v", i, err)
		}
	}
	ex, err := lvs.Explain(RecallRequest{
		Scope: "user:a", Query: "дедлайн проекта", K: 2,
	}, 2000)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	kept, below := 0, 0
	for _, f := range ex.Facts {
		switch f.Drop {
		case "":
			kept++
		case DropBelowK:
			below++
			if math.IsNaN(f.Final) {
				t.Errorf("%s: обрезан по K, но финальный скор не посчитан", f.Key)
			}
		default:
			t.Errorf("%s: неожиданная причина %q", f.Key, f.Drop)
		}
	}
	if kept != 2 || below != 2 {
		t.Errorf("в выдаче %d, обрезано по K %d; ожидалось 2 и 2", kept, below)
	}
}

// TestVMEMExplainLocalisesSource — то, ради чего примитив существует: оператор
// видит неверный ответ и по разложению узнаёт, КТО его сформировал. Без этого
// шага отзыв по происхождению — гадание, а не операция.
func TestVMEMExplainLocalisesSource(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	for i := 1; i <= 3; i++ {
		if _, err := lvs.Remember(RememberRequest{
			ID: fmt.Sprintf("legit%d", i), Scope: "user:a",
			Text: fmt.Sprintf("проект решение номер %d принято", i), Source: "human",
		}, 1000); err != nil {
			t.Fatalf("Remember legit%d: %v", i, err)
		}
	}
	// Подсадка свежее законных фактов — потому и всплывает наверх.
	if _, err := lvs.Remember(RememberRequest{
		ID: "poison", Scope: "user:a", Text: "проект отменён работы прекращены",
		Source: "email-agent",
	}, 1900); err != nil {
		t.Fatalf("Remember poison: %v", err)
	}

	ex, err := lvs.Explain(RecallRequest{Scope: "user:a", Query: "проект", K: 5}, 2000)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	kept := explainKept(t, ex)
	if len(kept) == 0 {
		t.Fatal("выдача пуста — объяснять нечего")
	}
	p := explainOf(t, ex, "poison")
	if p.Drop != "" {
		t.Fatalf("подсадка не в выдаче (%q) — сценарий не воспроизведён", p.Drop)
	}
	// Источник читается прямо с разложения — это и есть аргумент QUARANTINE.
	if p.Source != "email-agent" {
		t.Errorf("источник подсадки в разложении %q, ожидался email-agent", p.Source)
	}
	if p.TextRank == 0 {
		t.Errorf("подсадка не пришла из лексического плеча — разложение врёт про путь")
	}
	// Разложение обязано сходиться: финал = база × множители памяти.
	want := p.Base * p.DecayMul * p.ImpMul
	if math.Abs(p.Final-want) > 1e-12 {
		t.Errorf("арифметика не сходится: final %v, base %v × decay %v × imp %v = %v",
			p.Final, p.Base, p.DecayMul, p.ImpMul, want)
	}
	if p.AgeSec != 100 {
		t.Errorf("возраст подсадки %v, ожидался 100 секунд", p.AgeSec)
	}
}

// TestVMEMExplainEmptyScope — пустой ответ не должен выглядеть как «всё
// отсеяно»: кандидатов не было вовсе, и это другой диагноз.
func TestVMEMExplainEmptyScope(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	ex, err := lvs.Explain(RecallRequest{Scope: "user:пусто", Query: "что угодно", K: 5}, 2000)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(ex.Facts) != 0 {
		t.Errorf("на пустом scope разложение содержит %d записей", len(ex.Facts))
	}
}
