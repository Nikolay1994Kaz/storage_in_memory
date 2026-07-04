package compute

import (
	"fmt"
	"log/slog"
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
	slog.Info("wasm: trigger added",
		"id", trigger.ID, "event", event, "pattern", pattern, "module", moduleName, "func", funcName)

	return trigger.ID
}

// RemoveTrigger удаляет триггер по ID.
func (tm *TriggerManager) RemoveTrigger(id string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	for i, t := range tm.triggers {
		if t.ID == id {
			tm.triggers = append(tm.triggers[:i], tm.triggers[i+1:]...)
			slog.Info("wasm: trigger removed", "id", id)
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
// workerID определяет какой worker-local WASM-инстанс использовать.
//
// Два пути выполнения:
//   - Reactor-модуль (worker-local): WorkerLocal.Exec() — ~1µs, 0 allocs
//   - Command-модуль (legacy):       ExecFunctionWithKey() — ~14ms, 53K allocs
func (tm *TriggerManager) Fire(event TriggerEvent, key string, workerID int) {
	tm.mu.RLock()
	// Fast path: если триггеров нет — выходим без аллокаций.
	// Типичный случай: Fire() вызывается на КАЖДЫЙ SET/DEL,
	// но триггеры настроены только у 1% пользователей.
	if len(tm.triggers) == 0 {
		tm.mu.RUnlock()
		return
	}
	triggers := make([]*Trigger, len(tm.triggers))
	copy(triggers, tm.triggers)
	tm.mu.RUnlock()

	for _, t := range triggers {
		if t.Event != event {
			continue
		}

		matched, err := filepath.Match(t.Pattern, key)
		if err != nil || !matched {
			continue
		}

		// Быстрый путь: Reactor-модуль через worker-local инстанс
		if tm.engine.WorkerLocal != nil && tm.engine.WorkerLocal.HasModule(t.ModuleName) {
			_, err = tm.engine.WorkerLocal.Exec(workerID, t.ModuleName, t.FuncName, []byte(key))
		} else {
			// Fallback: Command-модуль через per-call instantiation
			err = tm.engine.ExecFunctionWithKey(t.ModuleName, t.FuncName, key)
		}

		if err != nil {
			slog.Error("wasm: trigger error", "id", t.ID, "err", err)
		}
	}
}
