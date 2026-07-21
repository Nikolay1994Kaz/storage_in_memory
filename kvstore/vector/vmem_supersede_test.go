package vector

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// =============================================================================
// Шаг 4 VMEM — supersession (вариант «c»: re-ingest цели с закрытым valid_to).
// White-box ветки rememberSupersede: валидация цели атомарна (ошибка не
// оставляет полузаписанной пары), закрывающий док бит-в-бит повторяет цель
// (включая точечную выемку термов из frozen-слоя), «поздний прав» при upsert
// после закрытия, flushing-окно (цель между swap и публикацией сегмента),
// сериализация RMW против параллельных писателей (-race).
// =============================================================================

// vmemFactByKey — материализованный факт по ключу через живой путь чтения
// (white-box обёртка getFactDocLocked под RLock — для проверок состояния).
func vmemFactByKey(t *testing.T, lvs *LeveledVectorStore, key string) (DeltaEntry, bool) {
	t.Helper()
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	return lvs.getFactDocLocked(key)
}

// TestSupersedeValidationAtomic — цель не существует / чужой scope / не
// VMEM-док / стёрта: ошибка ДО любых вставок, новый факт не появляется.
func TestSupersedeValidationAtomic(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	if _, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "user:a", Text: "кофе без сахара"}, 100); err != nil {
		t.Fatalf("Remember цели: %v", err)
	}
	if err := lvs.AddDoc("plain", mkVecN(vmemPlaceholderDim, 0.5), Attributes{}, "просто док без контракта"); err != nil {
		t.Fatalf("AddDoc: %v", err)
	}

	cases := []struct {
		name   string
		target string
		scope  string
		want   error
	}{
		{"нет цели", "no-such-fact", "user:a", ErrVMEMSupersedesTarget},
		{"чужой scope", "f1", "user:b", ErrVMEMSupersedesScope},
		{"не VMEM-док", "plain", "user:a", ErrVMEMSupersedesScope},
	}
	for _, tc := range cases {
		id := "new:" + tc.name
		_, err := lvs.Remember(RememberRequest{ID: id, Scope: tc.scope, Text: "замена", Supersedes: tc.target}, 200)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v, ожидалась %v", tc.name, err, tc.want)
		}
		if _, ok := lvs.Get(id); ok {
			t.Errorf("%s: новый факт записан несмотря на ошибку валидации — пара полузаписана", tc.name)
		}
	}

	// Стёртая цель (erasure побеждает): Delete → supersedes = «цели нет».
	lvs.Delete("f1")
	if _, err := lvs.Remember(RememberRequest{ID: "f2", Scope: "user:a", Text: "чай", Supersedes: "f1"}, 300); !errors.Is(err, ErrVMEMSupersedesTarget) {
		t.Errorf("стёртая цель: err=%v, ожидалась ErrVMEMSupersedesTarget", err)
	}
}

// TestSupersedeClosesInterval — семантическое ядро шага 4 при цели в дельте и
// во frozen-сегменте: закрывающий док повторяет цель бит-в-бит (вектор, термы
// через termsForDoc, остальные атрибуты), valid_to = valid_from наследника;
// RECALL-дефолт видит только наследника, AS_OF до замены — только цель.
func TestSupersedeClosesInterval(t *testing.T) {
	for _, st := range vmemLSMStates[:2] { // delta и flushed: размещение цели
		t.Run(st.name, func(t *testing.T) {
			lvs := NewLeveledVectorStore(bm25TestConfig())
			defer lvs.Close()

			imp := 0.8
			rr1, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "user:a", Text: "живёт в караганде давно", Type: "profile", Importance: &imp, TTL: 100000}, 100)
			if err != nil {
				t.Fatalf("Remember f1: %v", err)
			}
			if st.flushAt(1) > 0 {
				lvs.FlushDeltaSync()
			}

			rr2, err := lvs.Remember(RememberRequest{ID: "f2", Scope: "user:a", Text: "переехала в алматы, живёт там", Supersedes: "f1"}, 200)
			if err != nil {
				t.Fatalf("Remember f2: %v", err)
			}
			if rr2.Closed == nil {
				t.Fatal("Closed=nil при supersedes")
			}
			cl := *rr2.Closed

			// Закрывающий док = цель бит-в-бит, кроме valid_to.
			orig := rr1.Doc
			if cl.ID != "f1" {
				t.Errorf("Closed.ID=%q", cl.ID)
			}
			if len(cl.Vec) != len(orig.Vec) {
				t.Fatalf("Closed.Vec len=%d != %d", len(cl.Vec), len(orig.Vec))
			}
			for i := range cl.Vec {
				if math.Float32bits(cl.Vec[i]) != math.Float32bits(orig.Vec[i]) {
					t.Fatalf("Closed.Vec[%d] расходится битово с целью", i)
				}
			}
			if !reflect.DeepEqual(cl.Terms, orig.Terms) {
				t.Errorf("Closed.Terms=%v != термы цели %v (выемка из %s)", cl.Terms, orig.Terms, st.name)
			}
			wantAttrs := Attributes{Cat: map[string]string{vmemAttrScope: "user:a", vmemAttrType: "profile"}, Num: map[string]float64{}}
			for k, v := range orig.Attrs.Num {
				wantAttrs.Num[k] = v
			}
			wantAttrs.Num[vmemAttrValidTo] = 200
			if !reflect.DeepEqual(cl.Attrs, wantAttrs) {
				t.Errorf("Closed.Attrs=%+v, ожидалось %+v", cl.Attrs, wantAttrs)
			}

			// Времена: дефолтный RECALL (now=300) видит только наследника,
			// AS_OF=150 — только цель («живёт» матчится с обоими текстами).
			nowRes, err := lvs.Recall(RecallRequest{Scope: "user:a", Query: "живёт", K: 10}, 300)
			if err != nil {
				t.Fatalf("Recall now: %v", err)
			}
			asOf := int64(150)
			oldRes, err := lvs.Recall(RecallRequest{Scope: "user:a", Query: "живёт", K: 10, AsOf: &asOf}, 300)
			if err != nil {
				t.Fatalf("Recall as_of: %v", err)
			}
			keys := func(rs []VTextResult) []string {
				out := make([]string, 0, len(rs))
				for _, r := range rs {
					out = append(out, r.Key)
				}
				slices.Sort(out)
				return out
			}
			if got := keys(nowRes); !slices.Equal(got, []string{"f2"}) {
				t.Errorf("RECALL now видит %v, ожидался только f2", got)
			}
			if got := keys(oldRes); !slices.Equal(got, []string{"f1"}) {
				t.Errorf("RECALL as_of=150 видит %v, ожидался только f1", got)
			}
		})
	}
}

// TestSupersedeChainAndReopen — цепочка f1←f2←f3 закрывает интервалы каскадом,
// а upsert цели ПОСЛЕ закрытия открывает её заново («поздний прав» — тот же
// исход, что у сериализованной гонки upsert-после-supersede).
func TestSupersedeChainAndReopen(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	mustRemember := func(req RememberRequest, now int64) RememberResult {
		t.Helper()
		rr, err := lvs.Remember(req, now)
		if err != nil {
			t.Fatalf("Remember %s: %v", req.ID, err)
		}
		return rr
	}
	mustRemember(RememberRequest{ID: "f1", Scope: "s", Text: "джуниор"}, 100)
	mustRemember(RememberRequest{ID: "f2", Scope: "s", Text: "мидл", Supersedes: "f1"}, 150)
	mustRemember(RememberRequest{ID: "f3", Scope: "s", Text: "сеньор", Supersedes: "f2"}, 200)

	validTo := func(key string) float64 {
		t.Helper()
		e, ok := vmemFactByKey(t, lvs, key)
		if !ok {
			t.Fatalf("%s не найден", key)
		}
		return e.Attrs.Num[vmemAttrValidTo]
	}
	if got := validTo("f1"); got != 150 {
		t.Errorf("valid_to(f1)=%v, ожидалось 150", got)
	}
	if got := validTo("f2"); got != 200 {
		t.Errorf("valid_to(f2)=%v, ожидалось 200", got)
	}
	if got := validTo("f3"); got != float64(vmemOpenValidTo) {
		t.Errorf("valid_to(f3)=%v, ожидался сентинел", got)
	}

	// Upsert f1 после закрытия: полная замена → интервал открыт заново.
	mustRemember(RememberRequest{ID: "f1", Scope: "s", Text: "снова джуниор"}, 300)
	if got := validTo("f1"); got != float64(vmemOpenValidTo) {
		t.Errorf("valid_to(f1) после upsert=%v, ожидался сентинел (поздний прав)", got)
	}
}

// TestSupersedeFlushWindow — цель в СФЛАШИВАЕМОЙ дельте (окно между swap и
// публикацией сегмента, харнесс buildSem): getFactDocLocked обязан найти её в
// lvs.flushing, закрытие не теряется. Родня семейства flush-visibility-грабель.
func TestSupersedeFlushWindow(t *testing.T) {
	cfg := leveledConfigForVisibility()
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Clear()

	if _, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "user:a", Text: "переезд в алматы"}, 100); err != nil {
		t.Fatalf("Remember f1: %v", err)
	}

	lvs.buildSem <- struct{}{} // пришпиливаем build: swap случится, публикация — нет
	lvs.triggerCompact()
	waitUntil(t, func() bool {
		lvs.mu.RLock()
		defer lvs.mu.RUnlock()
		deltaEmpty := lvs.delta == nil || lvs.delta.Len() == 0
		nSegs := 0
		for i := range lvs.levels {
			nSegs += len(lvs.levels[i])
		}
		return deltaEmpty && nSegs == 0 && lvs.inFlightBuilds.Load() > 0
	})

	rr, err := lvs.Remember(RememberRequest{ID: "f2", Scope: "user:a", Text: "возврат в караганду", Supersedes: "f1"}, 200)
	<-lvs.buildSem
	if err != nil {
		t.Fatalf("Remember f2 в окне flush: %v", err)
	}
	if rr.Closed == nil || rr.Closed.ID != "f1" {
		t.Fatalf("Closed=%+v, ожидался re-ingest f1", rr.Closed)
	}
	wantTerms := TokenizeDoc("переезд в алматы")
	if !reflect.DeepEqual(rr.Closed.Terms, wantTerms) {
		t.Errorf("термы цели из flushing-дельты: %v, ожидалось %v", rr.Closed.Terms, wantTerms)
	}
	if got := rr.Closed.Attrs.Num[vmemAttrValidTo]; got != 200 {
		t.Errorf("valid_to=%v, ожидалось 200", got)
	}

	// После публикации сегмента закрытие остаётся видимым: свежая копия f1
	// (активная дельта) затеняет опубликованную открытую.
	waitUntil(t, func() bool { return lvs.anyMergeInFlight() == false && lvs.inFlightBuilds.Load() == 0 })
	e, ok := vmemFactByKey(t, lvs, "f1")
	if !ok {
		t.Fatal("f1 пропал после публикации сегмента")
	}
	if got := e.Attrs.Num[vmemAttrValidTo]; got != 200 {
		t.Errorf("valid_to(f1) после публикации=%v, ожидалось 200", got)
	}
}

// TestSupersedeConcurrentWriters — сериализация RMW: параллельные upsert'ы
// цели и supersedes-замены не рвут состояние (химера «старый текст поверх
// нового» невозможна — RMW атомарен под lvs.mu.Lock). Детерминированный
// финал: последовательный upsert + supersede после join обязаны победить.
// Гонки данных ловит -race.
func TestSupersedeConcurrentWriters(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	if _, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "s", Text: "исходный вариант"}, 100); err != nil {
		t.Fatalf("Remember f1: %v", err)
	}

	texts := make(map[string]bool)
	texts["исходный вариант"] = true
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		text := fmt.Sprintf("вариант номер %d", g)
		texts[text] = true
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "s", Text: text}, 200); err != nil {
					t.Errorf("upsert f1: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := lvs.Remember(RememberRequest{ID: fmt.Sprintf("g%d", g), Scope: "s", Text: "замена", Supersedes: "f1"}, 250); err != nil {
					t.Errorf("supersede f1: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Химера: термы f1 обязаны совпадать с токенизацией ОДНОГО из текстов.
	e, ok := vmemFactByKey(t, lvs, "f1")
	if !ok {
		t.Fatal("f1 пропал")
	}
	matched := false
	for text := range texts {
		if reflect.DeepEqual(e.Terms, TokenizeDoc(text)) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("термы f1 (%v) не совпадают ни с одним написанным текстом — состояние порвано", e.Terms)
	}

	// Детерминированный финал: поздний прав.
	if _, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "s", Text: "финальный текст"}, 300); err != nil {
		t.Fatalf("финальный upsert: %v", err)
	}
	rr, err := lvs.Remember(RememberRequest{ID: "final", Scope: "s", Text: "закрывающая замена", Supersedes: "f1"}, 400)
	if err != nil {
		t.Fatalf("финальный supersede: %v", err)
	}
	if !reflect.DeepEqual(rr.Closed.Terms, TokenizeDoc("финальный текст")) {
		t.Fatalf("закрыт не финальный текст: %v — lost update", rr.Closed.Terms)
	}
	if got := rr.Closed.Attrs.Num[vmemAttrValidTo]; got != 400 {
		t.Errorf("valid_to=%v, ожидалось 400", got)
	}
}
