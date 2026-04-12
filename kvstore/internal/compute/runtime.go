package compute

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// CompiledModule — загруженный и скомпилированный WASM-модуль.
type CompiledModule struct {
	Name      string
	Compiled  wazero.CompiledModule
	LoadedAt  time.Time     // когда модуль был загружен
	ExecCount atomic.Uint64 // сколько раз вызывался (атомарный счётчик)
}

type execTxKey struct{}

type WasmTxCtx struct {
	InTx  bool
	Queue []func()
}

// Engine — WASM compute engine.
// Управляет загрузкой модулей, их выполнением и host-функциями.
type Engine struct {
	mu      sync.RWMutex
	runtime wazero.Runtime
	modules map[string]*CompiledModule // name → compiled module

	// Timeout для WASM-вызовов (по умолчанию 10ms).
	// Зависший модуль будет прерван после этого времени.
	ExecTimeout time.Duration

	GlobalLock   func()
	GlobalUnlock func()

	// Callback-мостики к основному движку.
	// StoreGet/StoreSet/StoreDel — прямые операции (без WAL).
	StoreGet func(key string) ([]byte, bool)
	StoreSet func(key string, value []byte)
	StoreDel func(key string)

	// StoreSetWithWAL/StoreDelWithWAL — операции с WAL (для durability).
	// Используются из host-функций, чтобы данные из WASM переживали рестарт.
	StoreSetWithWAL func(key string, value []byte) error
	StoreDelWithWAL func(key string) error

	// Publish — отправка сообщения в Pub/Sub канал.
	Publish func(channel, message string)
}

// NewEngine создаёт WASM compute engine.
func NewEngine() *Engine {
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)

	e := &Engine{
		runtime:     r,
		modules:     make(map[string]*CompiledModule),
		ExecTimeout: 10 * time.Millisecond, // Phase 0: max 10ms на вызов
	}

	// Host-функции регистрируются один раз (до WASI)
	if _, err := e.registerHostFunctions(ctx); err != nil {
		log.Fatalf("[wasm] Failed to register host functions: %v", err)
	}

	// WASI нужен для Go-модулей (GOOS=wasip1)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	return e
}

// Close освобождает ресурсы WASM-рантайма.
func (e *Engine) Close() {
	e.runtime.Close(context.Background())
}

// LoadModule загружает WASM-модуль из байтов.
// После загрузки модуль доступен по имени для WASM.EXEC.
func (e *Engine) LoadModule(name string, wasmBytes []byte) error {
	ctx := context.Background()

	// Компилируем WASM (один раз, потом переиспользуем)
	compiled, err := e.runtime.CompileModule(ctx, wasmBytes)
	if err != nil {
		return fmt.Errorf("compile error: %w", err)
	}

	e.mu.Lock()
	e.modules[name] = &CompiledModule{
		Name:     name,
		Compiled: compiled,
		LoadedAt: time.Now(),
	}
	e.mu.Unlock()

	log.Printf("[wasm] Module '%s' loaded successfully", name)
	return nil
}

// DropModule удаляет WASM-модуль.
func (e *Engine) DropModule(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	mod, exists := e.modules[name]
	if !exists {
		return fmt.Errorf("module '%s' not found", name)
	}

	mod.Compiled.Close(context.Background())
	delete(e.modules, name)
	log.Printf("[wasm] Module '%s' dropped", name)
	return nil
}

// ListModules возвращает имена загруженных модулей.
func (e *Engine) ListModules() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	names := make([]string, 0, len(e.modules))
	for name := range e.modules {
		names = append(names, name)
	}
	return names
}

// ModuleInfo возвращает информацию о модуле для WASM.INFO.
func (e *Engine) ModuleInfo(name string) (loadedAt time.Time, execCount uint64, found bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	mod, exists := e.modules[name]
	if !exists {
		return time.Time{}, 0, false
	}
	return mod.LoadedAt, mod.ExecCount.Load(), true
}

// ExecFunction вызывает функцию WASM-модуля.
// Использует context.WithTimeout для защиты от зависших модулей.
func (e *Engine) ExecFunction(moduleName, funcName string, args ...uint64) ([]uint64, error) {
	e.mu.RLock()
	mod, exists := e.modules[moduleName]
	e.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("module '%s' not found", moduleName)
	}

	// Timeout: зависший WASM не заблокирует worker навсегда
	ctx, cancel := context.WithTimeout(context.Background(), e.ExecTimeout)
	defer cancel()

	tx := &WasmTxCtx{}
	ctx = context.WithValue(ctx, execTxKey{}, tx)

	// Уникальное имя для каждого инстанса (wazero требует уникальности)
	instanceName := fmt.Sprintf("%s_%d", moduleName, time.Now().UnixNano())

	// Создаём новый инстанс модуля (изолированный!)
	instance, err := e.runtime.InstantiateModule(ctx, mod.Compiled,
		wazero.NewModuleConfig().WithName(instanceName).
			WithStdout(os.Stdout).WithStderr(os.Stderr))
	if err != nil {
		return nil, fmt.Errorf("instantiate error: %w", err)
	}
	defer instance.Close(ctx)

	// Находим и вызываем функцию
	fn := instance.ExportedFunction(funcName)
	if fn == nil {
		return nil, fmt.Errorf("function '%s' not found in module '%s'", funcName, moduleName)
	}

	// Инкрементируем счётчик вызовов
	mod.ExecCount.Add(1)

	results, err := fn.Call(ctx, args...)
	if err != nil {
		if exitErr, ok := err.(*sys.ExitError); ok && exitErr.ExitCode() == 0 {
			return results, nil
		}
		return nil, fmt.Errorf("exec error: %w", err)
	}

	return results, nil
}

// ExecFunctionWithKey вызывает WASM-функцию, передавая key через линейную память.
// Используется триггерами: Fire() → ExecFunctionWithKey(module, func, "user:123").
//
// Механизм передачи ключа:
//  1. Инстанцируем WASM-модуль
//  2. Записываем key в WASM-memory по offset 0
//  3. Вызываем функцию с аргументами (key_ptr=0, key_len=len(key))
//  4. WASM-модуль читает ключ из своей памяти
func (e *Engine) ExecFunctionWithKey(moduleName, funcName, key string) error {
	e.mu.RLock()
	mod, exists := e.modules[moduleName]
	e.mu.RUnlock()

	if !exists {
		return fmt.Errorf("module '%s' not found", moduleName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.ExecTimeout)
	defer cancel()

	tx := &WasmTxCtx{}
	ctx = context.WithValue(ctx, execTxKey{}, tx)

	instanceName := fmt.Sprintf("%s_%d", moduleName, time.Now().UnixNano())

	instance, err := e.runtime.InstantiateModule(ctx, mod.Compiled,
		wazero.NewModuleConfig().WithName(instanceName).
			WithStdout(os.Stdout).WithStderr(os.Stderr))
	if err != nil {
		return fmt.Errorf("instantiate error: %w", err)
	}
	defer instance.Close(ctx)

	fn := instance.ExportedFunction(funcName)
	if fn == nil {
		return fmt.Errorf("function '%s' not found in module '%s'", funcName, moduleName)
	}

	// Записываем ключ в WASM-memory по offset 0
	keyBytes := []byte(key)
	if !instance.Memory().Write(0, keyBytes) {
		return fmt.Errorf("failed to write key to WASM memory")
	}

	// Инкрементируем счётчик вызовов
	mod.ExecCount.Add(1)

	// Вызываем: func process(key_ptr i32, key_len i32)
	_, err = fn.Call(ctx, uint64(0), uint64(len(keyBytes)))
	if err != nil {
		// WASI-модули (TinyGo) вызывают proc_exit(0) при нормальном завершении.
		// wazero сообщает это как ошибку, но exit_code=0 — это успех.
		if exitErr, ok := err.(*sys.ExitError); ok && exitErr.ExitCode() == 0 {
			return nil // нормальное завершение
		}
		return fmt.Errorf("exec error: %w", err)
	}

	return nil
}
