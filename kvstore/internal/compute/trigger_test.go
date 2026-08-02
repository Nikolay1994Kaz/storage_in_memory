package compute

import (
	"sync"
	"testing"
)

// ─── AddTrigger: базовый сценарий ────────────────────────

func TestTriggerManager_AddTrigger(t *testing.T) {
	tm := &TriggerManager{}

	id := tm.AddTrigger(OnSet, "tx:*", "fraud_scorer", "score_transaction")

	if id != "trigger_1" {
		t.Fatalf("first trigger ID = %q, want 'trigger_1'", id)
	}

	triggers := tm.ListTriggers()
	if len(triggers) != 1 {
		t.Fatalf("ListTriggers: got %d, want 1", len(triggers))
	}

	tr := triggers[0]
	if tr.Event != OnSet {
		t.Fatalf("Event = %q, want SET", tr.Event)
	}
	if tr.Pattern != "tx:*" {
		t.Fatalf("Pattern = %q, want 'tx:*'", tr.Pattern)
	}
	if tr.ModuleName != "fraud_scorer" {
		t.Fatalf("ModuleName = %q", tr.ModuleName)
	}
	if tr.FuncName != "score_transaction" {
		t.Fatalf("FuncName = %q", tr.FuncName)
	}
}

// ─── ID автоинкремент ────────────────────────────────────

func TestTriggerManager_IDAutoIncrement(t *testing.T) {
	tm := &TriggerManager{}

	id1 := tm.AddTrigger(OnSet, "*", "mod", "fn")
	id2 := tm.AddTrigger(OnDel, "*", "mod", "fn")
	id3 := tm.AddTrigger(OnExpire, "*", "mod", "fn")

	if id1 != "trigger_1" || id2 != "trigger_2" || id3 != "trigger_3" {
		t.Fatalf("IDs: %q, %q, %q", id1, id2, id3)
	}
}

// ─── RemoveTrigger ───────────────────────────────────────

func TestTriggerManager_RemoveTrigger(t *testing.T) {
	tm := &TriggerManager{}

	id1 := tm.AddTrigger(OnSet, "a:*", "mod", "fn")
	tm.AddTrigger(OnDel, "b:*", "mod", "fn")

	// Удаляем первый
	ok := tm.RemoveTrigger(id1)
	if !ok {
		t.Fatal("RemoveTrigger should return true for existing trigger")
	}

	triggers := tm.ListTriggers()
	if len(triggers) != 1 {
		t.Fatalf("after remove: %d triggers, want 1", len(triggers))
	}
	if triggers[0].Pattern != "b:*" {
		t.Fatalf("remaining trigger pattern = %q, want 'b:*'", triggers[0].Pattern)
	}
}

// ─── RemoveTrigger: несуществующий ID ────────────────────

func TestTriggerManager_RemoveNonExistent(t *testing.T) {
	tm := &TriggerManager{}

	ok := tm.RemoveTrigger("trigger_999")
	if ok {
		t.Fatal("RemoveTrigger should return false for non-existent ID")
	}
}

// ─── ListTriggers: пустой менеджер ───────────────────────

func TestTriggerManager_ListEmpty(t *testing.T) {
	tm := &TriggerManager{}

	triggers := tm.ListTriggers()
	if len(triggers) != 0 {
		t.Fatalf("empty manager: %d triggers", len(triggers))
	}
}

// ─── ListTriggers: возвращает копию (не мутирует оригинал) ─

func TestTriggerManager_ListReturnsCopy(t *testing.T) {
	tm := &TriggerManager{}
	tm.AddTrigger(OnSet, "*", "mod", "fn")

	list := tm.ListTriggers()
	list[0] = nil // мутируем копию

	// Оригинал не тронут
	original := tm.ListTriggers()
	if original[0] == nil {
		t.Fatal("ListTriggers should return a copy, not a reference")
	}
}

// ─── Fire: glob-паттерн сопоставление ────────────────────
//
// Fire вызывает engine.ExecFunctionWithKey, который упадёт
// с "module not found" (engine пустой). Но мы проверяем
// что паттерны правильно матчатся.
//
// Для этого используем счётчик вызовов через обёртку.

func TestTriggerManager_Fire_PatternMatching(t *testing.T) {
	// Engine с заглушкой — ExecFunctionWithKey вернёт ошибку,
	// но Fire её просто залогирует и продолжит.
	engine := NewEngine()
	defer engine.Close()

	tm := NewTriggerManager(engine)
	fires := countFires(t)

	// Добавляем триггеры с разными паттернами
	tm.AddTrigger(OnSet, "tx:*", "mod", "fn")   // tx:123 ✓, user:1 ✗
	tm.AddTrigger(OnSet, "user:*", "mod", "fn") // user:1 ✓, tx:123 ✗
	tm.AddTrigger(OnDel, "*", "mod", "fn")      // любой ключ, но только DEL

	// Каждое совпадение считается отдельно: раньше здесь стояло «Если дошли сюда
	// без паники — паттерны работают корректно», и тест был зелёным даже если бы
	// Fire не матчил вообще ничего.
	tm.Fire(OnSet, "tx:123", 0)
	wantFires(t, fires, 1, "tx:123 обязан сматчить tx:*")

	tm.Fire(OnSet, "user:1", 0)
	wantFires(t, fires, 2, "user:1 обязан сматчить user:*")

	tm.Fire(OnSet, "random", 0)
	wantFires(t, fires, 2, "random не подходит ни под один SET-триггер")

	tm.Fire(OnDel, "anything", 0)
	wantFires(t, fires, 3, "DEL-триггер с паттерном * обязан сработать")
}

// ─── Fire: несовпадающее событие не вызывает триггер ─────

func TestTriggerManager_Fire_EventMismatch(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()

	tm := NewTriggerManager(engine)
	fires := countFires(t)

	// Триггер только на SET
	tm.AddTrigger(OnSet, "*", "mod", "fn")

	// DEL и EXPIRE не должны его вызвать. Комментарий здесь раньше сам называл
	// наблюдаемый признак («была бы ошибка в логах»), но проверки не было.
	tm.Fire(OnDel, "any-key", 0)
	tm.Fire(OnExpire, "any-key", 0)
	wantFires(t, fires, 0, "SET-триггер не должен срабатывать на DEL/EXPIRE")
}

// ─── Fire: пустой менеджер — нет паники ──────────────────

func TestTriggerManager_Fire_Empty(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()

	tm := NewTriggerManager(engine)
	fires := countFires(t)

	// Не паникует при вызове без триггеров — и ничего не вызывает: это ещё и
	// проверка быстрого пути `len(tm.triggers) == 0`, ради которого Fire висит
	// на каждом SET/DEL.
	tm.Fire(OnSet, "key", 0)
	tm.Fire(OnDel, "key", 0)
	wantFires(t, fires, 0, "пустой менеджер не имеет что вызывать")
}

// ─── Конкурентный доступ: Add + Fire + Remove ────────────

func TestTriggerManager_Concurrent(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()

	tm := NewTriggerManager(engine)

	var wg sync.WaitGroup

	// Добавляем триггеры из 10 горутин
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm.AddTrigger(OnSet, "*", "mod", "fn")
		}()
	}

	// Параллельно Fire из 10 горутин
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm.Fire(OnSet, "key", 0)
		}()
	}

	// Параллельно List
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm.ListTriggers()
		}()
	}

	wg.Wait()

	// Должно быть 10 триггеров
	if len(tm.ListTriggers()) != 10 {
		t.Fatalf("expected 10 triggers, got %d", len(tm.ListTriggers()))
	}
}

// ─── Fire: два триггера матчат один ключ → оба срабатывают ─

func TestTriggerManager_Fire_MultipleMatch(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()

	tm := NewTriggerManager(engine)
	fires := countFires(t)

	// Два триггера на SET, оба матчат "tx:123"
	tm.AddTrigger(OnSet, "tx:*", "mod_a", "fn_a")
	tm.AddTrigger(OnSet, "*", "mod_b", "fn_b") // wildcard — тоже матчит

	// Fire не должен остановиться после первого совпадения. Раньше «оба
	// отработали» было только в комментарии: тест не отличил бы двойное
	// срабатывание от одинарного, а именно это он и проверяет.
	tm.Fire(OnSet, "tx:123", 0)
	wantFires(t, fires, 2, "оба совпавших триггера обязаны сработать, а не первый")
}

// ─── Fire: невалидный glob-паттерн → не паникует ─────────
//
// filepath.Match("[") возвращает ошибку ErrBadPattern.
// Fire должен пропустить такой триггер, а не крашиться.

func TestTriggerManager_Fire_BadPattern(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()

	tm := NewTriggerManager(engine)
	fires := countFires(t)

	// "[" — невалидный glob
	tm.AddTrigger(OnSet, "[", "mod", "fn")
	// Нормальный триггер после битого
	tm.AddTrigger(OnSet, "*", "mod", "fn")

	// Ровно одно срабатывание: битый паттерн пропущен, а следующий за ним —
	// отработал. Число важнее отсутствия паники: 0 означал бы, что ErrBadPattern
	// прерывает весь обход и глушит исправные триггеры.
	tm.Fire(OnSet, "any-key", 0)
	wantFires(t, fires, 1, "битый паттерн пропускается, но не отменяет остальные")
}
