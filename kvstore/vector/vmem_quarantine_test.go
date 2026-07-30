package vector

import (
	"errors"
	"math"
	"slices"
	"testing"
)

// =============================================================================
// Карантин VMEM (примитив 2 слоя восстановления): массовый отзыв убеждений по
// происхождению. Проверяется то, ради чего он существует, а не то, что он
// «что-то делает»:
//   - выборочность (соседний источник и законные факты, записанные ПОСЛЕ
//     подсадки, целы) — это та единственная строка сравнительной таблицы, где
//     откат полным снапшотом проигрывает архитектурно, а не по старанию;
//   - история веры не переписана: AS_OF до момента отзыва по-прежнему
//     показывает факт («агент действительно так думал в 14:32»);
//   - идемпотентность: повторный отзыв не двигает момент вперёд, иначе
//     ответы про прошлое поехали бы;
//   - все размещения LSM — скан по колонке сегмента отдельный код от скана
//     по дельте.
// =============================================================================

// vmemQuarantineScenario — общая расстановка: отравленный источник, соседний
// источник и законный факт, записанный ПОСЛЕ подсадки (жертва отката целиком).
// Времена: подсадка 1000, законный факт 1500, карантин 2000, чтение 3000.
type vmemQuarantineScenario struct {
	lvs *LeveledVectorStore
}

func newQuarantineScenario(t *testing.T, flushAt int) *vmemQuarantineScenario {
	t.Helper()
	lvs := NewLeveledVectorStore(bm25TestConfig())
	facts := []struct {
		id, text, source string
		at               int64
	}{
		{"bad1", "дедлайн проекта март", "web-scraper", 1000},
		{"bad2", "дедлайн проекта апрель", "web-scraper", 1000},
		{"ok1", "дедлайн проекта май", "email-agent", 1000},
		{"legit", "дедлайн проекта июнь", "human", 1500}, // записан ПОСЛЕ подсадки
	}
	for i, f := range facts {
		if _, err := lvs.Remember(RememberRequest{ID: f.id, Scope: "user:a", Text: f.text, Source: f.source}, f.at); err != nil {
			t.Fatalf("Remember %s: %v", f.id, err)
		}
		if flushAt > 0 && i+1 == flushAt {
			lvs.FlushDeltaSync()
		}
	}
	return &vmemQuarantineScenario{lvs: lvs}
}

func (sc *vmemQuarantineScenario) recall(t *testing.T, req RecallRequest, now int64) []string {
	t.Helper()
	req.Scope, req.Query, req.K = "user:a", "дедлайн", 100
	return vmemRecallIDs(t, sc.lvs, req, now)
}

// TestVMEMQuarantineSelective — ядро: отзыв по источнику убирает только его,
// на всех размещениях LSM.
func TestVMEMQuarantineSelective(t *testing.T) {
	for _, st := range vmemLSMStates {
		t.Run(st.name, func(t *testing.T) {
			sc := newQuarantineScenario(t, st.flushAt(4))
			defer sc.lvs.Close()

			res, err := sc.lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 2000)
			if err != nil {
				t.Fatalf("Quarantine: %v", err)
			}
			if len(res.Docs) != 2 {
				t.Fatalf("отозвано %d фактов, ожидалось 2", len(res.Docs))
			}
			// ⭐Строка таблицы: отравленное ушло, законное — включая записанное
			// ПОСЛЕ подсадки — на месте. Откат снапшотом теряет здесь legit.
			if got := sc.recall(t, RecallRequest{}, 3000); !slices.Equal(got, []string{"legit", "ok1"}) {
				t.Errorf("после карантина видно %v, ожидалось [legit ok1]", got)
			}
		})
	}
}

// TestVMEMQuarantinePreservesBelief — история веры не переписана: AS_OF до
// момента отзыва показывает отравленный факт (агент действительно так думал),
// ALL показывает его всегда, а прикладное время не тронуто вовсе.
func TestVMEMQuarantinePreservesBelief(t *testing.T) {
	sc := newQuarantineScenario(t, 0)
	defer sc.lvs.Close()

	if _, err := sc.lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 2000); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	asOf := int64(1500)
	if got := sc.recall(t, RecallRequest{AsOf: &asOf}, 3000); !slices.Contains(got, "bad1") {
		t.Errorf("AS_OF %d: %v — отравленный факт обязан быть виден ДО момента отзыва (улика)", asOf, got)
	}
	after := int64(2500)
	if got := sc.recall(t, RecallRequest{AsOf: &after}, 3000); slices.Contains(got, "bad1") {
		t.Errorf("AS_OF %d: %v — после момента отзыва факт виден быть не должен", after, got)
	}
	if got := sc.recall(t, RecallRequest{All: true}, 3000); !slices.Contains(got, "bad1") {
		t.Errorf("ALL: %v — форензический режим обязан показывать отозванное", got)
	}
	// Прикладное время не тронуто: карантин — отдельная ось, а не supersede.
	doc, ok := vmemFactByKey(t, sc.lvs, "bad1")
	if !ok {
		t.Fatal("bad1 исчез физически — карантин не должен удалять")
	}
	if vt := doc.Attrs.Num[vmemAttrValidTo]; vt != float64(vmemOpenValidTo) {
		t.Errorf("valid_to=%v — карантин переписал прикладное время, а обязан был только пометить отзыв", vt)
	}
	if q := sc.lvs.vmemQuarantinedAt("bad1"); q != 2000 {
		t.Errorf("quarantined_at=%v, ожидалось 2000", q)
	}
}

// TestVMEMQuarantineIdempotent — повторный отзыв ничего не отзывает заново и
// НЕ двигает момент вперёд (иначе ответы AS_OF про прошлое поехали бы).
func TestVMEMQuarantineIdempotent(t *testing.T) {
	sc := newQuarantineScenario(t, 0)
	defer sc.lvs.Close()

	if _, err := sc.lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 2000); err != nil {
		t.Fatalf("Quarantine 1: %v", err)
	}
	res, err := sc.lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 2600)
	if err != nil {
		t.Fatalf("Quarantine 2: %v", err)
	}
	if len(res.Docs) != 0 {
		t.Errorf("повторный карантин отозвал %d фактов, ожидалось 0", len(res.Docs))
	}
	if q := sc.lvs.vmemQuarantinedAt("bad1"); q != 2000 {
		t.Errorf("quarantined_at=%v — момент отзыва уехал вперёд, история AS_OF сломана", q)
	}
}

// TestVMEMQuarantineBoundaries — нижняя граница SINCE, чужой scope и
// валидация. Каждая граница — способ отозвать лишнее, поэтому проверяются
// вместе.
func TestVMEMQuarantineBoundaries(t *testing.T) {
	sc := newQuarantineScenario(t, 0)
	defer sc.lvs.Close()

	// SINCE позже подсадки — под предикат не попадает никто.
	res, err := sc.lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper", Since: 1200}, 2000)
	if err != nil {
		t.Fatalf("Quarantine SINCE: %v", err)
	}
	if len(res.Docs) != 0 {
		t.Errorf("SINCE=1200 отозвал %d фактов при valid_from=1000, ожидалось 0", len(res.Docs))
	}
	// Чужой scope: тот же источник, другая память — не трогаем.
	if _, err := sc.lvs.Remember(RememberRequest{ID: "other", Scope: "user:b", Text: "дедлайн чужой", Source: "web-scraper"}, 1000); err != nil {
		t.Fatalf("Remember other: %v", err)
	}
	if _, err := sc.lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 2000); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if q := sc.lvs.vmemQuarantinedAt("other"); !math.IsNaN(q) {
		t.Errorf("факт чужого scope отозван (quarantined_at=%v) — карантин пробил границу памяти", q)
	}
	// Отзыв без источника = «отозвать всё»; такой команды быть не должно.
	if _, err := sc.lvs.Quarantine(QuarantineRequest{Scope: "user:a"}, 2000); !errors.Is(err, ErrVMEMQuarantineSource) {
		t.Errorf("карантин без источника: err=%v, ожидалась ErrVMEMQuarantineSource", err)
	}
	if _, err := sc.lvs.Quarantine(QuarantineRequest{Source: "web-scraper"}, 2000); !errors.Is(err, ErrVMEMScope) {
		t.Errorf("карантин без scope: err=%v, ожидалась ErrVMEMScope", err)
	}
}

// TestVMEMQuarantineErasureWins — стирание сильнее отзыва: FORGET после
// карантина не воскрешает факт ни в одном режиме, а истёкший по TTL факт не
// попадает под отзыв (успех карантина не зависит от расписания жнеца).
func TestVMEMQuarantineErasureWins(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	if _, err := lvs.Remember(RememberRequest{ID: "gone", Scope: "user:a", Text: "дедлайн истекающий", Source: "web-scraper", TTL: 100}, 1000); err != nil {
		t.Fatalf("Remember gone: %v", err)
	}
	// now=2000 > valid_from+TTL=1100 → факт уже невидим на чтении.
	res, err := lvs.Quarantine(QuarantineRequest{Scope: "user:a", Source: "web-scraper"}, 2000)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if len(res.Docs) != 0 {
		t.Errorf("истёкший по TTL факт отозван (%d) — карантин зависит от жнеца", len(res.Docs))
	}
}

// TestVMEMQuarantineSkipsRacedUpsert — проверка предиката SOURCE в фазе
// ПРИГОВОРА, а не только в скане. Между фазами блокировка ОТПУЩЕНА:
// collectBySource берёт mu.RLock и снимает его по defer, quarantineKeys берёт
// mu.Lock заново. В это окно параллельный клиент может перезаписать факт,
// объявив другой источник, — и без перепроверки по свежайшей версии отзыв
// «источника A» унесёт факт, который сейчас принадлежит источнику B. Это
// прямое нарушение того единственного свойства, ради которого карантин
// существует: соседние источники обязаны остаться нетронутыми.
//
// Гонка воспроизводится детерминированно: фазы вызываются раздельно, а между
// ними делается upsert. Написан после того, как мутация «снять предикат в
// приговоре» прошла мимо ВСЕХ тестов пакета — они проверяли только скан, и
// один и тот же фильтр в двух фазах маскировал её. Тот же класс и тот же
// приём, что TestVMEMBackfillSkipsRacedUpsert: правило «двухфазная операция
// требует теста, вызывающего фазы раздельно» было выведено для BACKFILL и не
// перенесено на вторую такую операцию.
func TestVMEMQuarantineSkipsRacedUpsert(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	if _, err := lvs.Remember(RememberRequest{
		ID: "raced", Scope: "user:a", Text: "дедлайн проекта март", Source: "web-scraper",
	}, 1000); err != nil {
		t.Fatal(err)
	}

	// Фаза скана: кандидат отобран, источник у него ещё отравленный.
	cands := lvs.collectBySource("web-scraper", 100)
	if len(cands) != 1 || cands[0] != "raced" {
		t.Fatalf("скан отобрал %v, ожидался ровно [raced]", cands)
	}

	// ...и тут параллельный клиент перезаписывает факт другим источником.
	if _, err := lvs.Remember(RememberRequest{
		ID: "raced", Scope: "user:a", Text: "дедлайн проекта март", Source: "human",
	}, 1500); err != nil {
		t.Fatal(err)
	}

	// Фаза приговора обязана его отбросить: сейчас это факт от human.
	res, err := lvs.quarantineKeys(cands, QuarantineRequest{
		Scope: "user:a", Source: "web-scraper",
	}, 2000)
	if err != nil {
		t.Fatalf("quarantineKeys: %v", err)
	}
	if len(res.Docs) != 0 {
		t.Errorf("отозвано %d фактов — приговор не перепроверил свежайшую версию, "+
			"отзыв web-scraper унёс факт, принадлежащий human", len(res.Docs))
	}
	if q := lvs.vmemQuarantinedAt("raced"); !math.IsNaN(q) {
		t.Errorf("quarantined_at=%v — факт соседнего источника отозван", q)
	}

	// ⭐Парный ПОЛОЖИТЕЛЬНЫЙ контроль: без гонки тот же путь обязан отозвать.
	// Без него «0 отозвано» зачлось бы и в случае, когда приговор отвергает
	// вообще всё — то есть тест был бы зелёным по неверной причине.
	if _, err := lvs.Remember(RememberRequest{
		ID: "plain", Scope: "user:a", Text: "дедлайн проекта апрель", Source: "web-scraper",
	}, 1000); err != nil {
		t.Fatal(err)
	}
	ok, err := lvs.quarantineKeys(lvs.collectBySource("web-scraper", 100), QuarantineRequest{
		Scope: "user:a", Source: "web-scraper",
	}, 2000)
	if err != nil {
		t.Fatalf("quarantineKeys (контроль): %v", err)
	}
	if len(ok.Docs) != 1 || ok.Docs[0].ID != "plain" {
		t.Fatalf("положительный контроль: отозвано %d — путь не работает вовсе, "+
			"нулевой результат выше ничего не доказывает", len(ok.Docs))
	}
}
