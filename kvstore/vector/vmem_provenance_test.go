package vector

import (
	"fmt"
	"slices"
	"testing"
)

// =============================================================================
// Провенанс VMEM (примитив 1 слоя восстановления, 26.07): CAT-атрибут source +
// фильтр RECALL по нему. Решение контракта: «источник не объявлен» пишется
// ЯВНЫМ значением unknown, а не отсутствием атрибута — иначе массовый отзыв по
// источнику молча пропускал бы ровно те факты, за которые никто не расписался.
//
// Проверяется то, что действительно может сломаться, а не то, что очевидно:
//   - штамп по умолчанию и сохранение явного значения (кухня полей);
//   - фильтр на ВСЕХ размещениях LSM (дельта / frozen / смешанное) — батч-суд
//     CAT идёт разными путями для дельты и колоночного слоя;
//   - переживание РЕАЛЬНОГО merge L0→L1: новая CAT-колонка обязана пройти
//     decodeAt→buildSegmentAttrs слияния, иначе фильтр начнёт врать именно на
//     старых данных, то есть там, где форензику и спрашивают.
// =============================================================================

// TestVMEMProvenanceKitchen — кухня полей: source штампуется всегда.
func TestVMEMProvenanceKitchen(t *testing.T) {
	const now = int64(1_753_000_000)
	cases := []struct {
		name string
		req  string // значение Source на входе
		want string
	}{
		{"не задан → явный unknown", "", vmemSourceUnknown},
		{"объявленный источник сохраняется", "web-scraper", "web-scraper"},
		{"явный unknown допустим и неотличим от неявного", vmemSourceUnknown, vmemSourceUnknown},
	}
	for _, tc := range cases {
		doc, err := rememberDoc(RememberRequest{Scope: "user:dana", Text: "кофе без сахара", Source: tc.req}, now, 0)
		if err != nil {
			t.Fatalf("%s: rememberDoc: %v", tc.name, err)
		}
		got, ok := doc.Attrs.Cat[vmemAttrSource]
		if !ok {
			t.Fatalf("%s: атрибут source отсутствует — факт невидим для отзыва по источнику", tc.name)
		}
		if got != tc.want {
			t.Errorf("%s: source=%q, ожидалось %q", tc.name, got, tc.want)
		}
	}
}

// vmemProvenanceCorpus — три факта одного scope из разных источников. Тексты
// делят общий терм («дедлайн»), чтобы BM25-плечо доставало все три и отбор
// делал ИМЕННО фильтр, а не релевантность.
var vmemProvenanceCorpus = []struct {
	id, text, source string
}{
	{"p1", "дедлайн проекта март", "web-scraper"},
	{"p2", "дедлайн проекта апрель", "email-agent"},
	{"p3", "дедлайн проекта май", ""}, // источник не объявлен → unknown
}

// vmemRecallIDs — id из выдачи RECALL в порядке выдачи.
func vmemRecallIDs(t *testing.T, lvs *LeveledVectorStore, req RecallRequest, now int64) []string {
	t.Helper()
	res, err := lvs.Recall(req, now)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	ids := make([]string, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.Key)
	}
	slices.Sort(ids)
	return ids
}

// TestVMEMRecallSourceFilter — фильтр по источнику на всех размещениях LSM.
func TestVMEMRecallSourceFilter(t *testing.T) {
	for _, st := range vmemLSMStates {
		t.Run(st.name, func(t *testing.T) {
			lvs := NewLeveledVectorStore(bm25TestConfig())
			defer lvs.Close()

			flushAt := st.flushAt(len(vmemProvenanceCorpus))
			for i, f := range vmemProvenanceCorpus {
				if _, err := lvs.Remember(RememberRequest{ID: f.id, Scope: "user:a", Text: f.text, Source: f.source}, 100); err != nil {
					t.Fatalf("Remember %s: %v", f.id, err)
				}
				if flushAt > 0 && i+1 == flushAt {
					lvs.FlushDeltaSync()
				}
			}

			base := RecallRequest{Scope: "user:a", Query: "дедлайн", K: 10}
			cases := []struct {
				source string
				want   []string
			}{
				{"", []string{"p1", "p2", "p3"}},    // без фильтра — все
				{"web-scraper", []string{"p1"}},     // отравленный источник
				{"email-agent", []string{"p2"}},     // соседний источник не задет
				{vmemSourceUnknown, []string{"p3"}}, // необъявленный — первоклассный класс
				{"no-such-source", []string{}},      // источника нет — пусто, не всё
			}
			for _, tc := range cases {
				req := base
				req.SourceEq = tc.source
				got := vmemRecallIDs(t, lvs, req, 200)
				if !slices.Equal(got, tc.want) {
					t.Errorf("SOURCE=%q: выдача %v, ожидалось %v", tc.source, got, tc.want)
				}
			}
		})
	}
}

// TestVMEMProvenanceSurvivesMerge — источник переживает реальное слияние
// L0→L1. Риск назван до кода: merge реконструирует entries через decodeAt и
// заново строит колонки, поэтому новая CAT-колонка проходит по пути, которого
// нет ни у дельты, ни у одиночного frozen-сегмента. Пайл-ап собирается при
// удержанном слоте merge (приём bmPileupL0), затем слот отпускается.
func TestVMEMProvenanceSurvivesMerge(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	// Держим слот L0 занятым: каждый flush кладёт свой мини-сегмент, планировщик
	// их не разгребает, пока пайл-ап не собран.
	lvs.mu.Lock()
	lvs.mergeInFlight[0] = true
	lvs.mu.Unlock()

	const nSegs = 12 // > Fanout (8) — иначе каскад не запустится
	poisoned := make([]string, 0, nSegs/2)
	for i := 0; i < nSegs; i++ {
		id := fmt.Sprintf("m%02d", i)
		src := "email-agent"
		if i%2 == 0 {
			src = "web-scraper"
			poisoned = append(poisoned, id)
		}
		if _, err := lvs.Remember(RememberRequest{ID: id, Scope: "user:a", Text: fmt.Sprintf("дедлайн проекта неделя %d", i), Source: src}, 100); err != nil {
			t.Fatalf("Remember %s: %v", id, err)
		}
		lvs.FlushDeltaSync() // один факт = один мини-сегмент L0
	}

	lvs.mu.RLock()
	n0 := len(lvs.levels[0])
	lvs.mu.RUnlock()
	if n0 <= lvs.cfg.Fanout {
		t.Fatalf("пайл-ап не собрался: %d сегментов в L0 (нужно > fanout=%d)", n0, lvs.cfg.Fanout)
	}

	// Отпускаем слот и ждём, пока L0 разгребётся в L1 — только после этого
	// факты лежат в СЛИТОМ сегменте.
	lvs.mu.Lock()
	lvs.mergeInFlight[0] = false
	lvs.mu.Unlock()
	lvs.maybeScheduleMerges(lvs.compactionEpoch.Load())
	waitUntil(t, func() bool {
		lvs.mu.RLock()
		defer lvs.mu.RUnlock()
		return len(lvs.levels[1]) > 0 && len(lvs.levels[0]) <= lvs.cfg.Fanout
	})

	slices.Sort(poisoned)
	got := vmemRecallIDs(t, lvs, RecallRequest{Scope: "user:a", Query: "дедлайн", K: 100, SourceEq: "web-scraper"}, 200)
	if !slices.Equal(got, poisoned) {
		t.Fatalf("после merge SOURCE=web-scraper дал %v, ожидалось %v — провенанс не пережил слияние", got, poisoned)
	}
	// Соседний источник цел: отзыв по источнику обязан быть выборочным.
	if n := len(vmemRecallIDs(t, lvs, RecallRequest{Scope: "user:a", Query: "дедлайн", K: 100, SourceEq: "email-agent"}, 200)); n != nSegs-len(poisoned) {
		t.Fatalf("после merge SOURCE=email-agent дал %d фактов, ожидалось %d", n, nSegs-len(poisoned))
	}
}
