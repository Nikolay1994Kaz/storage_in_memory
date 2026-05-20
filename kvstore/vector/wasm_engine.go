package vector

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// WasmEngine — обёртка над рантаймом wazero для ускорения векторной математики.
//
// Использует sync.Pool, содержащий изолированные инстансы WASM-модулей (wasmInstance).
// Это гарантирует 100% потокобезопасность и истинную параллельность выполнения без блокировок (Mutex)
// на hot path расчетов. Каждая горутина получает свой независимый инстанс WASM со своей
// линейной памятью и аллоцированными буферами.
type WasmEngine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	dim      int
	pool     sync.Pool
	modCount uint64
}

// wasmInstance представляет изолированный инстанс WebAssembly модуля с предварительно аллоцированными буферами.
type wasmInstance struct {
	mod         api.Module
	fnEuclidean api.Function
	fnCosine    api.Function
	ptrA        uint32
	ptrB        uint32
}

// NewWasmEngine создает новый движок WASM, компилирует байткод и настраивает буферы.
func NewWasmEngine(ctx context.Context, wasmBytes []byte, dimension int) (*WasmEngine, error) {
	r := wazero.NewRuntime(ctx)

	// Компилируем модуль один раз (это самая тяжелая JIT-операция, ~10-50ms)
	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("failed to compile WASM module: %w", err)
	}

	engine := &WasmEngine{
		runtime:  r,
		compiled: compiled,
		dim:      dimension,
	}

	// Инстанцируем первый тестовый модуль для валидации экспортов и прогрева
	name := "vector_math_init"
	config := wazero.NewModuleConfig().WithName(name)
	mod, err := r.InstantiateModule(ctx, compiled, config)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("failed to instantiate WASM module: %w", err)
	}

	fnEuclidean := mod.ExportedFunction("simd_euclidean_distance")
	if fnEuclidean == nil {
		r.Close(ctx)
		return nil, fmt.Errorf("simd_euclidean_distance not exported by WASM module")
	}

	fnCosine := mod.ExportedFunction("simd_cosine_distance")
	if fnCosine == nil {
		r.Close(ctx)
		return nil, fmt.Errorf("simd_cosine_distance not exported by WASM module")
	}

	fnAlloc := mod.ExportedFunction("alloc")
	if fnAlloc == nil {
		r.Close(ctx)
		return nil, fmt.Errorf("alloc function not found")
	}

	// Выделяем буферы под векторы для тестового инстанса
	vecSize := uint64(dimension * 4)
	resA, err := fnAlloc.Call(ctx, vecSize)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("WASM alloc A failed: %w", err)
	}
	resB, err := fnAlloc.Call(ctx, vecSize)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("WASM alloc B failed: %w", err)
	}

	initialInstance := &wasmInstance{
		mod:         mod,
		fnEuclidean: fnEuclidean,
		fnCosine:    fnCosine,
		ptrA:        uint32(resA[0]),
		ptrB:        uint32(resB[0]),
	}

	// Настраиваем sync.Pool для параллельной работы горутин
	engine.pool = sync.Pool{
		New: func() any {
			instCtx := context.Background()
			instName := fmt.Sprintf("vector_math_%d", atomic.AddUint64(&engine.modCount, 1))
			instConfig := wazero.NewModuleConfig().WithName(instName)

			instMod, err := engine.runtime.InstantiateModule(instCtx, engine.compiled, instConfig)
			if err != nil {
				panic(fmt.Sprintf("failed to instantiate WASM module in pool: %v", err))
			}

			instEuclidean := instMod.ExportedFunction("simd_euclidean_distance")
			instCosine := instMod.ExportedFunction("simd_cosine_distance")
			instAlloc := instMod.ExportedFunction("alloc")

			if instEuclidean == nil || instCosine == nil || instAlloc == nil {
				panic("required WASM exports not found in pooled instance")
			}

			instResA, err := instAlloc.Call(instCtx, vecSize)
			if err != nil {
				panic(fmt.Sprintf("WASM alloc A failed in pooled instance: %v", err))
			}
			instResB, err := instAlloc.Call(instCtx, vecSize)
			if err != nil {
				panic(fmt.Sprintf("WASM alloc B failed in pooled instance: %v", err))
			}

			return &wasmInstance{
				mod:         instMod,
				fnEuclidean: instEuclidean,
				fnCosine:    instCosine,
				ptrA:        uint32(instResA[0]),
				ptrB:        uint32(instResB[0]),
			}
		},
	}

	// Помещаем первый инстанс в пул, чтобы не тратить его впустую
	engine.pool.Put(initialInstance)

	return engine, nil
}

// Close корректно освобождает ресурсы wazero.
func (we *WasmEngine) Close(ctx context.Context) error {
	// runtime.Close автоматически закроет все инстанцированные модули и скомпилированный модуль.
	return we.runtime.Close(ctx)
}

// EuclideanDistanceFunc возвращает функцию, совместимую с типом DistanceFunc из HNSW.
//
// Это 100% совместимый "дроп-ин" заменитель для EuclideanDistance!
func (we *WasmEngine) EuclideanDistanceFunc() DistanceFunc {
	return func(a, b []float32) float32 {
		if len(a) != we.dim || len(b) != we.dim {
			panic("dimension mismatch in WASM distance calculation")
		}

		ctx := context.Background()

		// 1. Быстро берем свободный изолированный инстанс из пула (без блокировок на hot path)
		inst := we.pool.Get().(*wasmInstance)
		defer we.pool.Put(inst)

		memory := inst.mod.Memory()

		// 2. Пишем векторы в изолированную память инстанса через unsafe кастинг (без аллокаций в Go)
		aBytes := unsafe.Slice((*byte)(unsafe.Pointer(&a[0])), len(a)*4)
		memory.Write(inst.ptrA, aBytes)

		bBytes := unsafe.Slice((*byte)(unsafe.Pointer(&b[0])), len(b)*4)
		memory.Write(inst.ptrB, bBytes)

		// 3. Вызываем SIMD-код
		results, err := inst.fnEuclidean.Call(ctx,
			uint64(inst.ptrA),
			uint64(inst.ptrB),
			uint64(we.dim),
		)
		if err != nil {
			panic(fmt.Sprintf("WASM call failed: %v", err))
		}

		// Возвращаем результат как float32
		return math.Float32frombits(uint32(results[0]))
	}
}

// CosineDistanceFunc возвращает функцию косинусного расстояния для HNSW.
func (we *WasmEngine) CosineDistanceFunc() DistanceFunc {
	return func(a, b []float32) float32 {
		if len(a) != we.dim || len(b) != we.dim {
			panic("dimension mismatch in WASM distance calculation")
		}

		ctx := context.Background()

		// 1. Берем изолированный инстанс
		inst := we.pool.Get().(*wasmInstance)
		defer we.pool.Put(inst)

		memory := inst.mod.Memory()

		// 2. Записываем вектора в изолированную память
		aBytes := unsafe.Slice((*byte)(unsafe.Pointer(&a[0])), len(a)*4)
		memory.Write(inst.ptrA, aBytes)

		bBytes := unsafe.Slice((*byte)(unsafe.Pointer(&b[0])), len(b)*4)
		memory.Write(inst.ptrB, bBytes)

		// 3. Вызываем SIMD-код
		results, err := inst.fnCosine.Call(ctx,
			uint64(inst.ptrA),
			uint64(inst.ptrB),
			uint64(we.dim),
		)
		if err != nil {
			panic(fmt.Sprintf("WASM call failed: %v", err))
		}

		return math.Float32frombits(uint32(results[0]))
	}
}
