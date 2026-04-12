package compute

import (
	"fmt"
	"log"
	"path/filepath"
	"sync"
)

// TriggerEvent — тип события, на которое реагирует триггер.
type TriggerEvent string

const (
	OnSet    TriggerEvent = "SET"
	OnDel    TriggerEvent = "DEL"
	OnExpire TriggerEvent = "EXPIRE"
)

// Trigger — связка: "при событии X на ключах Y → вызвать функцию Z из модуля W".
type Trigger struct {
	ID         string       // уникальный ID триггера
	Event      TriggerEvent // SET, DEL, EXPIRE
	Pattern    string       // glob-паттерн: "tx:*", "user:*", "*"
	ModuleName string       // имя WASM-модуля
	FuncName   string       // имя функции в модуле
}

// TriggerManager управляет триггерами.
type TriggerManager struct {
	mu       sync.RWMutex
	triggers []*Trigger
	nextID   int
	engine   *Engine
}

// NewTriggerManager создаёт менеджер триггеров.
func NewTriggerManager(engine *Engine) *TriggerManager {
	return &TriggerManager{
		engine: engine,
	}
}

// AddTrigger добавляет новый триггер.
// Пример: AddTrigger(OnSet, "tx:*", "fraud_scorer", "score_transaction")
func (tm *TriggerManager) AddTrigger(event TriggerEvent, pattern, moduleName, funcName string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.nextID++
	trigger := &Trigger{
		ID:         fmt.Sprintf("trigger_%d", tm.nextID),
		Event:      event,
		Pattern:    pattern,
		ModuleName: moduleName,
		FuncName:   funcName,
	}

	tm.triggers = append(tm.triggers, trigger)
	log.Printf("[wasm] Trigger added: %s on %s '%s' → %s.%s",
		trigger.ID, event, pattern, moduleName, funcName)

	return trigger.ID
}

// RemoveTrigger удаляет триггер по ID.
func (tm *TriggerManager) RemoveTrigger(id string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	for i, t := range tm.triggers {
		if t.ID == id {
			tm.triggers = append(tm.triggers[:i], tm.triggers[i+1:]...)
			log.Printf("[wasm] Trigger removed: %s", id)
			return true
		}
	}
	return false
}

// ListTriggers возвращает все зарегистрированные триггеры.
func (tm *TriggerManager) ListTriggers() []*Trigger {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	result := make([]*Trigger, len(tm.triggers))
	copy(result, tm.triggers)
	return result
}

// Fire вызывается при каждом SET/DEL/EXPIRE.
// Проверяет все триггеры — если паттерн совпадает, запускает WASM-функцию.
//
// BUG FIX: Теперь передаёт key в WASM-функцию через ExecFunctionWithKey.
// Раньше вызывал ExecFunction() без аргументов — WASM не знал, какой ключ
// вызвал триггер, что делало триггеры бесполезными.
func (tm *TriggerManager) Fire(event TriggerEvent, key string) {
	tm.mu.RLock()
	triggers := make([]*Trigger, len(tm.triggers))
	copy(triggers, tm.triggers)
	tm.mu.RUnlock()

	for _, t := range triggers {
		// Проверяем: событие совпадает?
		if t.Event != event {
			continue
		}

		// Проверяем: паттерн совпадает?
		matched, err := filepath.Match(t.Pattern, key)
		if err != nil || !matched {
			continue
		}

		// Совпало! Запускаем WASM-функцию с передачей ключа
		log.Printf("[wasm] Trigger %s fired: %s on key '%s' → %s.%s",
			t.ID, event, key, t.ModuleName, t.FuncName)

		// Передаём ключ в WASM-memory и вызываем функцию
		// WASM-функция получит (key_ptr, key_len) как аргументы
		err = tm.engine.ExecFunctionWithKey(t.ModuleName, t.FuncName, key)
		if err != nil {
			log.Printf("[wasm] Trigger %s error: %v", t.ID, err)
		}
	}
}
