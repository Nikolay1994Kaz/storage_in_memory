package vector

import (
	"errors"
	"testing"
)

// =============================================================================
// Миграция легаси: проверяется не «команда что-то дописала», а три свойства,
// без которых она вредна.
//   1. Чинит то, ради чего сделана: после неё легаси-факт СТАНОВИТСЯ отзываемым
//      (до неё карантин по источнику его не видит вообще).
//   2. НИКОГДА не переписывает уже объявленный источник — иначе миграция
//      уничтожает улику того, кто наполнил память.
//   3. Не трогает ничего, кроме провенанса: прикладное время, importance и
//      текст обязаны остаться теми же, иначе «починка» переписывает историю.
// =============================================================================

// legacyFact — факт со scope, но БЕЗ колонки source: так физически лежат данные,
// записанные до появления провенанса. Мимо Remember, потому что тот штампует
// unknown по контракту.
func legacyFact(t *testing.T, lvs *LeveledVectorStore, id, scope, term string, validFrom int64) {
	t.Helper()
	attrs := Attributes{
		Cat: map[string]string{vmemAttrScope: scope},
		Num: map[string]float64{
			vmemAttrValidFrom: float64(validFrom), vmemAttrValidTo: float64(vmemOpenValidTo),
			vmemAttrExpiresAt: float64(vmemOpenValidTo), vmemAttrImp: 0.75,
		},
	}
	terms := []TermTF{{Term: "дедлайн", TF: 1}, {Term: term, TF: 1}}
	if err := lvs.AddDocTerms(id, vmemPlaceholderVector(id, lvs.dim), attrs, terms); err != nil {
		t.Fatalf("AddDocTerms %s: %v", id, err)
	}
}

func TestVMEMBackfillMakesLegacyRevocable(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	// Один факт с объявленным источником — он задаёт размерность стора и
	// служит контролем «чужое не тронуто».
	if _, err := lvs.Remember(RememberRequest{
		ID: "declared", Scope: "user:a", Text: "дедлайн март", Source: "human",
	}, 1000); err != nil {
		t.Fatal(err)
	}
	legacyFact(t, lvs, "legacy1", "user:a", "апрель", 900)
	legacyFact(t, lvs, "legacy2", "user:a", "май", 950)
	legacyFact(t, lvs, "alien", "user:b", "июнь", 900) // чужой scope

	// ДО миграции: отозвать легаси нечем — под предикат источника никто не
	// попадает, потому что источника нет.
	if res, err := lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "unknown"}, 2000); err != nil {
		t.Fatalf("Quarantine до миграции: %v", err)
	} else if len(res.Docs) != 0 {
		t.Fatalf("до миграции отозвано %d фактов, ожидался 0", len(res.Docs))
	}

	res, err := lvs.BackfillSource(BackfillSourceRequest{Scope: "user:a", Source: "unknown"}, 2000)
	if err != nil {
		t.Fatalf("BackfillSource: %v", err)
	}
	if len(res.Docs) != 2 {
		t.Fatalf("мигрировано %d фактов, ожидалось 2 (чужой scope и объявленный источник не трогаются)", len(res.Docs))
	}
	for _, d := range res.Docs {
		checkWALReady(t, d)
	}

	// Свойство 2: объявленный источник цел.
	if got := lvs.sourceOf(t, "declared"); got != "human" {
		t.Errorf("источник объявленного факта стал %q — миграция переписала улику", got)
	}
	// Чужой scope не тронут.
	if got := lvs.sourceOf(t, "alien"); got != "" {
		t.Errorf("факт чужого scope получил источник %q", got)
	}
	// Свойство 3: кроме провенанса — ничего.
	rep := lvs.ProvenanceCoverage("user:a")
	if len(rep) != 1 || rep[0].Total != 3 {
		t.Fatalf("после миграции покрытие: %+v", rep)
	}
	if rep[0].BySource[""] != 0 {
		t.Errorf("слепое пятно осталось: %d фактов без атрибута", rep[0].BySource[""])
	}
	if rep[0].Revocable() != 1.0 {
		t.Errorf("доля отзываемых %v, ожидалась 1.0 — ради этого миграция и делается", rep[0].Revocable())
	}

	// Свойство 1: теперь отзыв работает — и именно по unknown.
	q, err := lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "unknown"}, 2100)
	if err != nil {
		t.Fatalf("Quarantine после миграции: %v", err)
	}
	if len(q.Docs) != 2 {
		t.Errorf("после миграции отозвано %d, ожидалось 2", len(q.Docs))
	}
}

// TestVMEMBackfillIdempotent — повторный запуск не находит ничего: предикат
// «атрибута нет» перестаёт выполняться, и это единственный предикат.
func TestVMEMBackfillIdempotent(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	if _, err := lvs.Remember(RememberRequest{
		ID: "seed", Scope: "user:a", Text: "дедлайн март", Source: "human",
	}, 1000); err != nil {
		t.Fatal(err)
	}
	legacyFact(t, lvs, "legacy1", "user:a", "апрель", 900)

	first, err := lvs.BackfillSource(BackfillSourceRequest{Scope: "user:a", Source: "unknown"}, 2000)
	if err != nil || len(first.Docs) != 1 {
		t.Fatalf("первый прогон: %d фактов, err=%v", len(first.Docs), err)
	}
	for _, d := range first.Docs {
		checkWALReady(t, d)
	}
	second, err := lvs.BackfillSource(BackfillSourceRequest{Scope: "user:a", Source: "import-crm"}, 2100)
	if err != nil {
		t.Fatalf("второй прогон: %v", err)
	}
	if len(second.Docs) != 0 {
		t.Errorf("второй прогон мигрировал %d фактов — источник переписывается", len(second.Docs))
	}
	if got := lvs.sourceOf(t, "legacy1"); got != "unknown" {
		t.Errorf("источник после второго прогона %q, ожидался unknown", got)
	}
}

// TestVMEMBackfillSkipsRacedUpsert — проверка «источника нет» в фазе ПРИГОВОРА,
// а не только в скане. Между сканом и приговором факт может обзавестись
// источником (обычный upsert от параллельного клиента), и без перепроверки по
// свежайшей версии миграция затрёт только что объявленный провенанс — то есть
// уничтожит улику. Это тот же класс расхождения «кандидат против свежайшей
// версии», что уже дважды ловил нас (регресс шага 8, пропуск SourceEq).
//
// Гонка воспроизводится детерминированно: фазы вызываются раздельно, а между
// ними делается upsert. Написан после того, как мутация «снять предикат в
// приговоре» прошла мимо остальных тестов — они проверяли только скан.
func TestVMEMBackfillSkipsRacedUpsert(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	if _, err := lvs.Remember(RememberRequest{
		ID: "seed", Scope: "user:a", Text: "дедлайн март", Source: "human",
	}, 1000); err != nil {
		t.Fatal(err)
	}
	legacyFact(t, lvs, "raced", "user:a", "апрель", 900)

	// Фаза скана: кандидат отобран, источника у него ещё нет.
	cands := lvs.collectSourceless("user:a", 100)
	if len(cands) != 1 || cands[0] != "raced" {
		t.Fatalf("скан отобрал %v, ожидался ровно [raced]", cands)
	}
	// ...и тут параллельный клиент перезаписывает факт, объявив источник.
	if _, err := lvs.Remember(RememberRequest{
		ID: "raced", Scope: "user:a", Text: "дедлайн апрель", Source: "crm-import",
	}, 1500); err != nil {
		t.Fatal(err)
	}
	// Фаза приговора обязана его отбросить.
	res, err := lvs.backfillKeys(cands, BackfillSourceRequest{Scope: "user:a", Source: "unknown"}, 2000)
	if err != nil {
		t.Fatalf("backfillKeys: %v", err)
	}
	if len(res.Docs) != 0 {
		t.Errorf("мигрировано %d фактов — приговор не перепроверил свежайшую версию", len(res.Docs))
	}
	if got := lvs.sourceOf(t, "raced"); got != "crm-import" {
		t.Errorf("источник стал %q — миграция затёрла только что объявленный провенанс", got)
	}
}

// TestVMEMBackfillValidation — пустое значение отвергается: команда,
// дописывающая пустую строку, тихо не сделала бы ничего.
func TestVMEMBackfillValidation(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	if _, err := lvs.BackfillSource(BackfillSourceRequest{Scope: "user:a"}, 2000); !errors.Is(err, ErrVMEMBackfillSource) {
		t.Errorf("пустой source: err=%v, ожидался ErrVMEMBackfillSource", err)
	}
	if _, err := lvs.BackfillSource(BackfillSourceRequest{Source: "unknown"}, 2000); !errors.Is(err, ErrVMEMScope) {
		t.Errorf("пустой scope: err=%v, ожидался ErrVMEMScope", err)
	}
}

// checkWALReady — BackfillSource уже положил версию в дельту; проверяем, что
// возвращённый документ самодостаточен для WAL-записи (ключ, вектор, атрибуты).
func checkWALReady(t *testing.T, d RememberedDoc) {
	t.Helper()
	if d.ID == "" || len(d.Vec) == 0 || d.Attrs.Cat[vmemAttrSource] == "" {
		t.Fatalf("неполный документ для WAL: %+v", d)
	}
}

// sourceOf — источник свежайшей версии ключа ("" = атрибута нет).
func (lvs *LeveledVectorStore) sourceOf(t *testing.T, key string) string {
	t.Helper()
	return lvs.catForKeys([]string{key}, vmemAttrSource)[0]
}
