package compute

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
)

// Оракул срабатываний триггера.
//
// 🚨Зачем окольный путь. Fire ничего не возвращает и счётчиков не имеет,
// поэтому пять тестов вокруг него проверяли ровно одно — «не паникует», а один
// так и писал в комментарии: «Если дошли сюда без паники — паттерны работают
// корректно». Мутация «не матчить ни один паттерн» проходила их ВСЕ, при том
// что называются они PatternMatching, MultipleMatch и EventMismatch.
//
// Наблюдаемый след у сработавшего триггера ровно один: модули в тестах не
// загружены, поэтому каждое срабатывание доходит до ExecFunctionWithKey,
// получает «module not found» и пишет `wasm: trigger error`. Эти записи и
// считаем — сама возможность такой проверки была названа в комментарии
// EventMismatch, но не использована.

type fireCountingHandler struct{ n *atomic.Int64 }

func (h fireCountingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h fireCountingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "wasm: trigger error" {
		h.n.Add(1)
	}
	return nil
}

func (h fireCountingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h fireCountingHandler) WithGroup(string) slog.Handler      { return h }

// countFires подменяет slog на время теста и возвращает счётчик срабатываний.
// Тесты пакета не параллельные (проверено: ни одного t.Parallel), поэтому
// глобальная подмена безопасна; прежний логгер возвращается через Cleanup.
func countFires(t *testing.T) func() int {
	t.Helper()
	var n atomic.Int64
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(fireCountingHandler{&n}))
	return func() int { return int(n.Load()) }
}

// wantFires — проверка с подписью, зачем это число ожидается.
func wantFires(t *testing.T, got func() int, want int, why string) {
	t.Helper()
	if n := got(); n != want {
		t.Errorf("срабатываний триггера %d, ожидалось %d — %s", n, want, why)
	}
}

// TestCountFires_Oracle — отрицательный контроль САМОГО оракула.
//
// Без него проверки ниже могли бы оказаться зелёными по неверной причине:
// счётчик, который всегда возвращает 0, «подтвердил» бы каждый тест, где
// ожидается ноль (а таких два из пяти).
func TestCountFires_Oracle(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	tm := NewTriggerManager(engine)
	fires := countFires(t)

	// Заведомо совпадающий триггер обязан дать ровно одну запись…
	tm.AddTrigger(OnSet, "*", "mod", "fn")
	tm.Fire(OnSet, "k", 0)
	wantFires(t, fires, 1, "оракул не видит даже гарантированное срабатывание")

	// …а заведомо несовпадающее событие — не добавить ни одной.
	tm.Fire(OnExpire, "k", 0)
	wantFires(t, fires, 1, "оракул считает то, чего не было")
}
