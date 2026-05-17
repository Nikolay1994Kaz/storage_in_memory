package compute

import (
	"strings"
	"testing"
	"time"
)

// ─── Тестовые WASM-модули (hand-crafted binary) ──────────

// addWasm: func add(i32, i32) → i32 { return a + b }
var addWasm = []byte{
	0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x07, 0x01, 0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7F,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x07, 0x01, 0x03, 0x61, 0x64, 0x64, 0x00, 0x00,
	0x0A, 0x09, 0x01, 0x07, 0x00, 0x20, 0x00, 0x20, 0x01, 0x6A, 0x0B,
}

// loopWasm: func loop() { loop { br 0 } } — бесконечный цикл
var loopWasm = []byte{
	0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x08, 0x01, 0x04, 0x6C, 0x6F, 0x6F, 0x70, 0x00, 0x00,
	0x0A, 0x09, 0x01, 0x07, 0x00, 0x03, 0x40, 0x0C, 0x00, 0x0B, 0x0B,
}

// processWasm: memory(1 page) + func process(i32, i32) — для ExecFunctionWithKey
var processWasm = []byte{
	0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x06, 0x01, 0x60, 0x02, 0x7F, 0x7F, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x05, 0x03, 0x01, 0x00, 0x01,
	0x07, 0x14, 0x02,
	0x06, 0x6D, 0x65, 0x6D, 0x6F, 0x72, 0x79, 0x02, 0x00,
	0x07, 0x70, 0x72, 0x6F, 0x63, 0x65, 0x73, 0x73, 0x00, 0x00,
	0x0A, 0x04, 0x01, 0x02, 0x00, 0x0B,
}

// ─── LoadModule ──────────────────────────────────────────

func TestEngine_LoadModule(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	if err := e.LoadModule("calc", addWasm); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	if len(e.ListModules()) != 1 {
		t.Fatal("should have 1 module")
	}
}

func TestEngine_LoadModule_Invalid(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	if err := e.LoadModule("bad", []byte{0xDE, 0xAD}); err == nil {
		t.Fatal("should fail for invalid WASM")
	}
}

// ─── DropModule ──────────────────────────────────────────

func TestEngine_DropModule(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	e.LoadModule("temp", addWasm)
	if err := e.DropModule("temp"); err != nil {
		t.Fatalf("DropModule: %v", err)
	}
	if len(e.ListModules()) != 0 {
		t.Fatal("module should be removed")
	}
}

func TestEngine_DropModule_NotFound(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	if err := e.DropModule("ghost"); err == nil {
		t.Fatal("should fail for non-existent module")
	}
}

// ─── ListModules ─────────────────────────────────────────

func TestEngine_ListModules(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	e.LoadModule("a", addWasm)
	e.LoadModule("b", addWasm)

	modules := e.ListModules()
	if len(modules) != 2 {
		t.Fatalf("got %d modules, want 2", len(modules))
	}
}

func TestEngine_ListModules_Empty(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	if len(e.ListModules()) != 0 {
		t.Fatal("empty engine should have 0 modules")
	}
}

// ─── ModuleInfo ──────────────────────────────────────────

func TestEngine_ModuleInfo(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	before := time.Now()
	e.LoadModule("calc", addWasm)

	loadedAt, execCount, found := e.ModuleInfo("calc")
	if !found {
		t.Fatal("module should be found")
	}
	if loadedAt.Before(before) {
		t.Fatal("loadedAt should be >= before")
	}
	if execCount != 0 {
		t.Fatalf("execCount = %d, want 0", execCount)
	}
}

func TestEngine_ModuleInfo_NotFound(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	_, _, found := e.ModuleInfo("ghost")
	if found {
		t.Fatal("should not find non-existent module")
	}
}

// ─── ExecFunction: add(2, 3) = 5 ────────────────────────

func TestEngine_ExecFunction(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	e.LoadModule("calc", addWasm)

	results, err := e.ExecFunction("calc", "add", 2, 3)
	if err != nil {
		t.Fatalf("ExecFunction: %v", err)
	}
	if len(results) != 1 || results[0] != 5 {
		t.Fatalf("add(2,3) = %v, want [5]", results)
	}
}

func TestEngine_ExecFunction_LargeNumbers(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	e.LoadModule("calc", addWasm)

	results, err := e.ExecFunction("calc", "add", 1000000, 2000000)
	if err != nil {
		t.Fatalf("ExecFunction: %v", err)
	}
	if results[0] != 3000000 {
		t.Fatalf("add(1M, 2M) = %d, want 3000000", results[0])
	}
}

// ─── ExecFunction: ошибки ────────────────────────────────

func TestEngine_ExecFunction_ModuleNotFound(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	_, err := e.ExecFunction("ghost", "fn")
	if err == nil {
		t.Fatal("should fail for non-existent module")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error should mention 'not found', got: %v", err)
	}
}

func TestEngine_ExecFunction_FuncNotFound(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	e.LoadModule("calc", addWasm)

	_, err := e.ExecFunction("calc", "nonexistent")
	if err == nil {
		t.Fatal("should fail for non-existent function")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error should mention 'not found', got: %v", err)
	}
}

// ─── ExecFunction: счётчик вызовов ───────────────────────

func TestEngine_ExecCount(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	e.LoadModule("calc", addWasm)

	e.ExecFunction("calc", "add", 1, 1)
	e.ExecFunction("calc", "add", 2, 2)
	e.ExecFunction("calc", "add", 3, 3)

	_, count, _ := e.ModuleInfo("calc")
	if count != 3 {
		t.Fatalf("ExecCount = %d, want 3", count)
	}
}

// ─── ExecFunction: timeout (бесконечный цикл) ───────────
//
// Модуль loopWasm содержит loop { br 0 } — бесконечный цикл.
// Engine имеет ExecTimeout = 10ms. Зависший WASM прерывается.

func TestEngine_ExecTimeout(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	e.ExecTimeout = 50 * time.Millisecond // чуть больше для стабильности

	e.LoadModule("evil", loopWasm)

	start := time.Now()
	_, err := e.ExecFunction("evil", "loop")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("infinite loop should timeout with error")
	}

	// Должен завершиться примерно за ExecTimeout, не за секунды
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took %v, should be ~50ms", elapsed)
	}
}

// ─── ExecFunctionWithKey ─────────────────────────────────
//
// Записывает ключ в WASM-memory и вызывает process(ptr=0, len).
// processWasm имеет экспортированную memory и функцию process(i32, i32).

func TestEngine_ExecFunctionWithKey(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	e.LoadModule("handler", processWasm)

	err := e.ExecFunctionWithKey("handler", "process", "user:123")
	if err != nil {
		t.Fatalf("ExecFunctionWithKey: %v", err)
	}

	// Счётчик инкрементировался
	_, count, _ := e.ModuleInfo("handler")
	if count != 1 {
		t.Fatalf("ExecCount = %d, want 1", count)
	}
}

func TestEngine_ExecFunctionWithKey_ModuleNotFound(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	err := e.ExecFunctionWithKey("ghost", "fn", "key")
	if err == nil {
		t.Fatal("should fail for non-existent module")
	}
}

// ─── Множественные вызовы (изоляция инстансов) ───────────
//
// Каждый ExecFunction создаёт новый WASM-инстанс.
// Убеждаемся что вызовы не влияют друг на друга.

func TestEngine_MultipleExec_Isolation(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	e.LoadModule("calc", addWasm)

	for i := uint64(0); i < 10; i++ {
		results, err := e.ExecFunction("calc", "add", i, i)
		if err != nil {
			t.Fatalf("exec #%d: %v", i, err)
		}
		if results[0] != i*2 {
			t.Fatalf("add(%d,%d) = %d, want %d", i, i, results[0], i*2)
		}
	}
}

// ─── Callback fields (StoreGet/Set/Del) ──────────────────
//
// Проверяем что Engine корректно хранит callback'и.
// Реальный вызов через WASM требует модуль с импортами host-функций,
// но мы можем проверить что поля устанавливаются.

func TestEngine_CallbacksWiring(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	store := map[string][]byte{}

	e.StoreGet = func(key string) ([]byte, bool) {
		v, ok := store[key]
		return v, ok
	}
	e.StoreSet = func(key string, value []byte) {
		store[key] = value
	}
	e.StoreDel = func(key string) {
		delete(store, key)
	}

	// Прямой вызов callback'ов (без WASM)
	e.StoreSet("hello", []byte("world"))
	val, ok := e.StoreGet("hello")
	if !ok || string(val) != "world" {
		t.Fatalf("StoreGet = %q/%v, want world/true", val, ok)
	}

	e.StoreDel("hello")
	_, ok = e.StoreGet("hello")
	if ok {
		t.Fatal("key should be deleted")
	}
}

// ─── Close: повторный Close не паникует ──────────────────

func TestEngine_Close_Idempotent(t *testing.T) {
	e := NewEngine()
	e.Close()
	// Второй Close не должен паниковать
	e.Close()
}
