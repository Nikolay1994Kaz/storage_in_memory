package compute

import (
	"context"
	"fmt"
	"log"
	"runtime"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Memory Layout — договор между Host и Guest.
// Должен совпадать с константами в WASM-модуле.
const (
	InputOffset  uint32 = 16384 // Host → Guest: входные данные (key)
	ValueOffset  uint32 = 20480 // Host ↔ Guest: kv_get результат
	OutputOffset uint32 = 24576 // Guest → Host: результат функции
	MaxInputLen  uint32 = 1024
	MaxOutputLen uint32 = 4096
)

// ModuleTier определяет стратегию жизненного цикла WASM-инстанса.
type ModuleTier int

const (
	TierPinned  ModuleTier = 0 // Наш код, zero-alloc, инстанс вечный
	TierBudget  ModuleTier = 1 // Сторонний reactor, рециклинг при превышении памяти
	TierCommand ModuleTier = 2 // Legacy command (_start), per-call instantiation
)

// WorkerSlot — один постоянный WASM-инстанс, принадлежащий конкретному воркеру.
// Никаких мьютексов: один воркер — один слот — один поток исполнения.
type WorkerSlot struct {
	WorkerID int
	instance api.Module
	memory   api.Memory
	funcs    map[string]api.Function // кеш ExportedFunction lookups

	// Для Tier 1: рециклинг
	memoryBudget uint32                // максимальный размер memory (в страницах, 1 страница = 64KB)
	compiled     wazero.CompiledModule // для пересоздания при recycle
	moduleName   string

	// Стек для zero-allocation вызовов CallWithStack
	Stack [8]uint64

	// Pre-created context с workerID для host-функций.
	// Создаётся один раз при createSlot — zero-alloc на hot path.
	ctx context.Context
}

// WorkerLocalEngine управляет per-worker WASM-слотами.
// Один Engine может обслуживать несколько модулей — для каждого своя группа слотов.
type WorkerLocalEngine struct {
	numWorkers int
	modules    map[string][]*WorkerSlot // moduleName → [numWorkers]WorkerSlot
	rt         wazero.Runtime
	ownsRT     bool // true если мы сами создали runtime (нужно закрыть в Close)
}

// NewWorkerLocalEngine создаёт engine для N воркеров.
// Создаёт ОТДЕЛЬНЫЙ wazero.Runtime БЕЗ WithCloseOnContextDone —
// иначе _start(proc_exit) закроет persistent-инстанс навсегда.
// Host-функции и WASI регистрируются через Engine.
func NewWorkerLocalEngine(e *Engine) *WorkerLocalEngine {
	ctx := context.Background()
	n := runtime.NumCPU()

	// Отдельный runtime без WithCloseOnContextDone
	rt := wazero.NewRuntime(ctx)

	// Регистрируем те же host-функции, привязанные к тому же Engine
	e.registerHostFunctionsOn(rt)

	// WASI — нужен для TinyGo модулей (proc_exit, fd_write, etc.)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	return &WorkerLocalEngine{
		numWorkers: n,
		modules:    make(map[string][]*WorkerSlot),
		rt:         rt,
		ownsRT:     true,
	}
}

// WarmUpFromBytes компилирует и создаёт persistent инстансы из WASM-байтов.
func (wle *WorkerLocalEngine) WarmUpFromBytes(name string, wasmBytes []byte, tier ModuleTier) error {
	ctx := context.Background()

	compiled, err := wle.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return fmt.Errorf("compile on worker-local runtime: %w", err)
	}

	slots := make([]*WorkerSlot, wle.numWorkers)

	var budget uint32
	if tier == TierBudget {
		budget = 128 // 128 * 64KB = 8MB
	}

	for i := 0; i < wle.numWorkers; i++ {
		slot, err := wle.createSlot(i, name, compiled, budget)
		if err != nil {
			for j := 0; j < i; j++ {
				slots[j].instance.Close(context.Background())
			}
			return fmt.Errorf("warmup worker %d: %w", i, err)
		}
		slots[i] = slot
	}

	wle.modules[name] = slots
	log.Printf("[wasm] Worker-local: module '%s' warmed up (%d instances, tier=%d)", name, wle.numWorkers, tier)
	return nil
}

// createSlot инстанцирует один WASM-модуль для одного воркера.
// _start вызывается нормально (TinyGo runtime init), но proc_exit(0)
// НЕ закрывает модуль потому что наш runtime без WithCloseOnContextDone.
func (wle *WorkerLocalEngine) createSlot(workerID int, name string, compiled wazero.CompiledModule, budget uint32) (*WorkerSlot, error) {
	ctx := context.Background()
	instanceName := fmt.Sprintf("%s_w%d", name, workerID)

	instance, err := wle.rt.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().
			WithName(instanceName).
			WithStartFunctions(), // Skip _start: proc_exit always closes the module in wazero
	)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}

	// Вызываем _initialize (TinyGo: __wasm_call_ctors).
	// Это инициализирует глобальные переменные и runtime БЕЗ proc_exit.
	// _start = main() + proc_exit → мёртвый инстанс (нельзя!)
	// _initialize = constructors only → инстанс жив
	if initFn := instance.ExportedFunction("_initialize"); initFn != nil {
		if _, err := initFn.Call(ctx); err != nil {
			instance.Close(ctx)
			return nil, fmt.Errorf("_initialize: %w", err)
		}
	}

	// Кешируем все exported functions — избегаем map lookup на каждый вызов
	// Ключи map-а ExportedFunctions() = имена экспортов (то что вызывает Host)
	funcs := make(map[string]api.Function)
	for exportName := range compiled.ExportedFunctions() {
		if fn := instance.ExportedFunction(exportName); fn != nil {
			funcs[exportName] = fn
		}
	}

	return &WorkerSlot{
		WorkerID:     workerID,
		instance:     instance,
		memory:       instance.Memory(),
		funcs:        funcs,
		memoryBudget: budget,
		compiled:     compiled,
		moduleName:   name,
		ctx:          context.WithValue(context.Background(), workerIDCtxKey{}, workerID),
	}, nil
}

// Exec — горячий путь. Вызывает функцию WASM на слоте конкретного воркера.
//
// Контракт:
//  1. Host пишет key в InputOffset (0)
//  2. Guest читает key, делает работу, пишет результат в OutputOffset (5120)
//  3. Guest возвращает длину результата
//  4. Host читает результат из OutputOffset
//
// ZERO ALLOCATION на Host-стороне (Memory.Write = memcpy, fn.Call = стек).
func (wle *WorkerLocalEngine) Exec(workerID int, moduleName, funcName string, key []byte) ([]byte, error) {
	slots, ok := wle.modules[moduleName]
	if !ok {
		return nil, fmt.Errorf("module '%s' not loaded as worker-local", moduleName)
	}
	slot := slots[workerID]

	// 1. Пишем key в INPUT_REGION
	if !slot.memory.Write(InputOffset, key) {
		return nil, fmt.Errorf("failed to write key to WASM memory")
	}

	// 2. Вызываем функцию
	fn, ok := slot.funcs[funcName]
	if !ok {
		return nil, fmt.Errorf("function '%s' not found in module '%s'", funcName, moduleName)
	}

	slot.Stack[0] = uint64(InputOffset)
	slot.Stack[1] = uint64(len(key))
	err := fn.CallWithStack(slot.ctx, slot.Stack[:2])
	if err != nil {
		return nil, fmt.Errorf("exec error: %w", err)
	}

	// 3. Читаем результат из OUTPUT_REGION
	resultLen := uint32(slot.Stack[0])
	if resultLen == 0 {
		return nil, nil
	}

	output, ok := slot.memory.Read(OutputOffset, resultLen)
	if !ok {
		return nil, fmt.Errorf("failed to read output from WASM memory")
	}

	// 4. Tier 1: проверяем бюджет памяти (раз в вызов — O(1))
	if slot.memoryBudget > 0 {
		currentPages := slot.memory.Size() / 65536 // Size() в байтах, делим на 64KB
		if currentPages > slot.memoryBudget {
			log.Printf("[wasm] Worker %d module '%s': memory %d pages > budget %d, recycling",
				workerID, moduleName, currentPages, slot.memoryBudget)
			if err := wle.recycleSlot(slot); err != nil {
				log.Printf("[wasm] Recycle error: %v", err)
			}
		}
	}

	return output, nil
}

// recycleSlot — закрывает и пересоздаёт инстанс из compiled cache.
// Вызывается ТОЛЬКО при превышении memory budget (Tier 1).
// Это редкое событие — раз на ~100K вызовов для sloppy-модулей.
func (wle *WorkerLocalEngine) recycleSlot(slot *WorkerSlot) error {
	ctx := context.Background()
	slot.instance.Close(ctx)

	newSlot, err := wle.createSlot(slot.WorkerID, slot.moduleName, slot.compiled, slot.memoryBudget)
	if err != nil {
		return err
	}

	// Обновляем слот на месте
	slot.instance = newSlot.instance
	slot.memory = newSlot.memory
	slot.funcs = newSlot.funcs
	return nil
}

// HasModule проверяет, загружен ли модуль как worker-local.
func (wle *WorkerLocalEngine) HasModule(name string) bool {
	_, ok := wle.modules[name]
	return ok
}

// DropModule закрывает все инстансы модуля.
func (wle *WorkerLocalEngine) DropModule(name string) {
	slots, ok := wle.modules[name]
	if !ok {
		return
	}
	ctx := context.Background()
	for _, slot := range slots {
		if slot != nil && slot.instance != nil {
			slot.instance.Close(ctx)
		}
	}
	delete(wle.modules, name)
	log.Printf("[wasm] Worker-local: module '%s' dropped (%d instances closed)", name, len(slots))
}

// Close закрывает все модули и worker-local runtime.
func (wle *WorkerLocalEngine) Close() {
	for name := range wle.modules {
		wle.DropModule(name)
	}
	if wle.ownsRT {
		wle.rt.Close(context.Background())
	}
}

// IsReactorModule проверяет, является ли скомпилированный модуль Reactor-ом.
// TinyGo всегда генерирует _start (даже с пустым main), поэтому наличие
// _start НЕ является признаком Command-модуля.
// Настоящий признак Reactor-а — наличие экспорта _initialize.
//
// ВАЖНО: compiled.ExportedFunctions() возвращает map[string]FunctionDefinition,
// где ключ — имя экспорта, а def.Name() — внутреннее имя WASM-функции.
// Для _initialize: key="_initialize", но Name()="__wasm_call_ctors".
// Поэтому проверяем ключ map-а, а не Name().
func IsReactorModule(compiled wazero.CompiledModule) bool {
	_, hasInit := compiled.ExportedFunctions()["_initialize"]
	return hasInit
}

// Slot returns the WorkerSlot for a specific worker and module name.
func (wle *WorkerLocalEngine) Slot(workerID int, moduleName string) *WorkerSlot {
	slots, ok := wle.modules[moduleName]
	if !ok {
		return nil
	}
	if workerID < 0 || workerID >= len(slots) {
		return nil
	}
	return slots[workerID]
}

// GetFunction returns a cached exported function by name.
func (ws *WorkerSlot) GetFunction(name string) api.Function {
	if ws == nil {
		return nil
	}
	return ws.funcs[name]
}

// Memory returns the WASM instance's linear memory.
func (ws *WorkerSlot) Memory() api.Memory {
	if ws == nil {
		return nil
	}
	return ws.memory
}

// workerIDCtxKey — ключ контекста для передачи workerID в host-функции WASM.
// Используется вместо парсинга workerID из имени инстанса.
type workerIDCtxKey struct{}

// WorkerIDFromContext извлекает workerID из контекста.
// Возвращает 0 если workerID не установлен.
func WorkerIDFromContext(ctx context.Context) int {
	if id, ok := ctx.Value(workerIDCtxKey{}).(int); ok {
		return id
	}
	return 0
}

// NumWorkers возвращает количество воркеров.
func (wle *WorkerLocalEngine) NumWorkers() int {
	return wle.numWorkers
}

