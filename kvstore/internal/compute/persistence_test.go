package compute

import (
	"os"
	"path/filepath"
	"testing"
)

// ─── SaveModule / DeleteModule ───────────────────────────

func TestSaveModule(t *testing.T) {
	tmpDir := t.TempDir() // Go автоматически удалит после теста

	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00} // WASM magic header

	err := SaveModule(tmpDir, "test_module", wasmBytes)
	if err != nil {
		t.Fatalf("SaveModule: %v", err)
	}

	// Файл существует
	path := filepath.Join(WasmDir(tmpDir), "test_module.wasm")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved module: %v", err)
	}

	if len(data) != len(wasmBytes) {
		t.Fatalf("saved %d bytes, want %d", len(data), len(wasmBytes))
	}

	// Содержимое совпадает
	for i := range data {
		if data[i] != wasmBytes[i] {
			t.Fatalf("byte[%d] = 0x%02x, want 0x%02x", i, data[i], wasmBytes[i])
		}
	}
}

func TestDeleteModule(t *testing.T) {
	tmpDir := t.TempDir()

	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6D}
	SaveModule(tmpDir, "to_delete", wasmBytes)

	path := filepath.Join(WasmDir(tmpDir), "to_delete.wasm")

	// Файл есть
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should exist before delete: %v", err)
	}

	// Удаляем
	DeleteModule(tmpDir, "to_delete")

	// Файл удалён
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file should be deleted")
	}
}

func TestDeleteModule_NonExistent(t *testing.T) {
	tmpDir := t.TempDir()

	// Не паникует при удалении несуществующего файла
	DeleteModule(tmpDir, "ghost")
}

// ─── SaveTriggers / LoadAll (JSON round-trip) ────────────

func TestSaveTriggers_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()

	// Создаём TriggerManager с триггерами
	tm := &TriggerManager{}
	tm.AddTrigger(OnSet, "tx:*", "fraud_scorer", "score")
	tm.AddTrigger(OnDel, "user:*", "audit_log", "on_delete")
	tm.AddTrigger(OnExpire, "session:*", "cleanup", "notify")

	// Сохраняем
	err := SaveTriggers(tmpDir, tm)
	if err != nil {
		t.Fatalf("SaveTriggers: %v", err)
	}

	// Проверяем: файл существует
	triggerPath := filepath.Join(WasmDir(tmpDir), "triggers.json")
	if _, err := os.Stat(triggerPath); err != nil {
		t.Fatalf("triggers.json should exist: %v", err)
	}

	// Восстанавливаем в новый TriggerManager
	engine := NewEngine()
	defer engine.Close()

	tm2 := NewTriggerManager(engine)
	err = LoadAll(tmpDir, engine, tm2)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// Проверяем: 3 триггера восстановлены
	restored := tm2.ListTriggers()
	if len(restored) != 3 {
		t.Fatalf("restored %d triggers, want 3", len(restored))
	}

	// Проверяем содержимое
	tests := []struct {
		event   TriggerEvent
		pattern string
		module  string
		fn      string
	}{
		{OnSet, "tx:*", "fraud_scorer", "score"},
		{OnDel, "user:*", "audit_log", "on_delete"},
		{OnExpire, "session:*", "cleanup", "notify"},
	}

	for i, want := range tests {
		got := restored[i]
		if got.Event != want.event {
			t.Fatalf("trigger[%d].Event = %q, want %q", i, got.Event, want.event)
		}
		if got.Pattern != want.pattern {
			t.Fatalf("trigger[%d].Pattern = %q, want %q", i, got.Pattern, want.pattern)
		}
		if got.ModuleName != want.module {
			t.Fatalf("trigger[%d].Module = %q, want %q", i, got.ModuleName, want.module)
		}
		if got.FuncName != want.fn {
			t.Fatalf("trigger[%d].Func = %q, want %q", i, got.FuncName, want.fn)
		}
	}
}

// ─── LoadAll: пустая директория (первый запуск) ──────────

func TestLoadAll_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()

	engine := NewEngine()
	defer engine.Close()

	tm := NewTriggerManager(engine)

	// Нет директории wasm/ → нет ошибки
	err := LoadAll(tmpDir, engine, tm)
	if err != nil {
		t.Fatalf("LoadAll on empty dir: %v", err)
	}

	if len(tm.ListTriggers()) != 0 {
		t.Fatal("should have 0 triggers on first launch")
	}
}

// ─── LoadAll: модули без триггеров ───────────────────────

func TestLoadAll_ModulesWithoutTriggers(t *testing.T) {
	tmpDir := t.TempDir()

	// Сохраняем .wasm файл, но не triggers.json
	SaveModule(tmpDir, "some_module", []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00})

	engine := NewEngine()
	defer engine.Close()

	tm := NewTriggerManager(engine)

	// LoadAll должен загрузить модуль, но не упасть на отсутствии triggers.json
	err := LoadAll(tmpDir, engine, tm)
	// Примечание: LoadModule может упасть на невалидном WASM, это нормально.
	// Главное — нет паники и triggers пустые.
	_ = err

	if len(tm.ListTriggers()) != 0 {
		t.Fatal("should have 0 triggers when triggers.json is missing")
	}
}

// ─── SaveTriggers: пустой список ─────────────────────────

func TestSaveTriggers_Empty(t *testing.T) {
	tmpDir := t.TempDir()

	tm := &TriggerManager{}

	err := SaveTriggers(tmpDir, tm)
	if err != nil {
		t.Fatalf("SaveTriggers empty: %v", err)
	}

	// Файл содержит пустой JSON массив
	data, err := os.ReadFile(filepath.Join(WasmDir(tmpDir), "triggers.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("empty triggers should produce '[]', got %q", string(data))
	}
}

// ─── WasmDir ─────────────────────────────────────────────

func TestWasmDir(t *testing.T) {
	dir := WasmDir("/data")
	expected := filepath.Join("/data", "wasm")
	if dir != expected {
		t.Fatalf("WasmDir = %q, want %q", dir, expected)
	}
}

// ─── Перезапись модуля (обновление) ──────────────────────

func TestSaveModule_Overwrite(t *testing.T) {
	tmpDir := t.TempDir()

	v1 := []byte{0x01, 0x02, 0x03}
	v2 := []byte{0x04, 0x05, 0x06, 0x07, 0x08}

	SaveModule(tmpDir, "updatable", v1)
	SaveModule(tmpDir, "updatable", v2) // перезаписываем

	path := filepath.Join(WasmDir(tmpDir), "updatable.wasm")
	data, _ := os.ReadFile(path)

	if len(data) != len(v2) {
		t.Fatalf("overwrite: got %d bytes, want %d", len(data), len(v2))
	}
	if data[0] != 0x04 {
		t.Fatalf("overwrite: first byte = 0x%02x, want 0x04", data[0])
	}
}

// ─── LoadAll: битый triggers.json → ошибка ───────────────
//
// На диске мусор вместо валидного JSON.
// LoadAll должен вернуть ошибку, а не panic.

func TestLoadAll_CorruptedTriggersJSON(t *testing.T) {
	tmpDir := t.TempDir()

	// Создаём директорию wasm/ и пишем мусор в triggers.json
	dir := WasmDir(tmpDir)
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "triggers.json"), []byte("{{{invalid json"), 0644)

	engine := NewEngine()
	defer engine.Close()

	tm := NewTriggerManager(engine)

	err := LoadAll(tmpDir, engine, tm)
	if err == nil {
		t.Fatal("LoadAll should return error for corrupted triggers.json")
	}

	// Триггеры не загружены
	if len(tm.ListTriggers()) != 0 {
		t.Fatal("corrupted JSON should result in 0 triggers")
	}
}
