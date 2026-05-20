package vector

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
	"unsafe"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// WasmGraphIndex управляет экземпляром HNSW индекса, полностью размещенным в Rust WebAssembly.
//
// Все вызовы сериализуются с помощью Mutex для обеспечения 100% потокобезопасности
// работы с кучей и внутренним состоянием WebAssembly модуля.
type WasmGraphIndex struct {
	ctx     context.Context
	runtime wazero.Runtime
	mod     api.Module
	hnswPtr uint64 // Указатель на C++-подобную структуру RustHNSW в куче WASM
	dim     int
	metric  uint32 // 0 = Euclidean, 1 = Cosine

	// Экспортированные функции
	fnAlloc    api.Function
	fnDealloc  api.Function
	fnNew      api.Function
	fnFree     api.Function
	fnInsert   api.Function
	fnDelete   api.Function
	fnSearch   api.Function
	fnClear    api.Function
	fnInfo     api.Function

	mu sync.Mutex
}

// NewWasmGraphIndex компилирует и инстанцирует WASM модуль, а затем создает в его куче HNSW-индекс.
func NewWasmGraphIndex(ctx context.Context, wasmBytes []byte, dim int, metric uint32) (*WasmGraphIndex, error) {
	r := wazero.NewRuntime(ctx)

	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("failed to compile WASM HNSW module: %w", err)
	}

	name := fmt.Sprintf("vector_hnsw_%d", time.Now().UnixNano())
	config := wazero.NewModuleConfig().WithName(name)
	mod, err := r.InstantiateModule(ctx, compiled, config)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("failed to instantiate WASM HNSW module: %w", err)
	}

	w := &WasmGraphIndex{
		ctx:     ctx,
		runtime: r,
		mod:     mod,
		dim:     dim,
		metric:  metric,
	}

	// Поиск всех экспортов
	w.fnAlloc = mod.ExportedFunction("alloc")
	w.fnDealloc = mod.ExportedFunction("dealloc")
	w.fnNew = mod.ExportedFunction("wasm_hnsw_new")
	w.fnFree = mod.ExportedFunction("wasm_hnsw_free")
	w.fnInsert = mod.ExportedFunction("wasm_hnsw_insert")
	w.fnDelete = mod.ExportedFunction("wasm_hnsw_delete")
	w.fnSearch = mod.ExportedFunction("wasm_hnsw_search")
	w.fnClear = mod.ExportedFunction("wasm_hnsw_clear")
	w.fnInfo = mod.ExportedFunction("wasm_hnsw_info")

	if w.fnAlloc == nil || w.fnDealloc == nil || w.fnNew == nil || w.fnFree == nil ||
		w.fnInsert == nil || w.fnDelete == nil || w.fnSearch == nil || w.fnClear == nil || w.fnInfo == nil {
		r.Close(ctx)
		return nil, fmt.Errorf("missing required HNSW WASM exports")
	}

	// Инициализируем HNSW индекс в куче WASM
	// Seed = UnixNano
	seed := uint64(time.Now().UnixNano())
	res, err := w.fnNew.Call(ctx, uint64(dim), uint64(metric), seed)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("failed to call wasm_hnsw_new: %w", err)
	}
	w.hnswPtr = res[0]

	return w, nil
}

// Close освобождает ресурсы wazero и память в куче WASM.
func (w *WasmGraphIndex) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.hnswPtr != 0 {
		_, _ = w.fnFree.Call(w.ctx, w.hnswPtr)
		w.hnswPtr = 0
	}
	return w.runtime.Close(w.ctx)
}

// Insert вставляет вектор в Rust HNSW индекс.
func (w *WasmGraphIndex) Insert(id uint64, vec []float32) error {
	if len(vec) != w.dim {
		return fmt.Errorf("dimension mismatch: expected %d, got %d", w.dim, len(vec))
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.hnswPtr == 0 {
		return fmt.Errorf("wasm HNSW index is closed")
	}

	// Аллоцируем буфер под вектор в WASM памяти
	vecSize := uint64(w.dim * 4)
	res, err := w.fnAlloc.Call(w.ctx, vecSize)
	if err != nil {
		return fmt.Errorf("wasm alloc failed: %w", err)
	}
	vecPtr := uint32(res[0])
	defer w.fnDealloc.Call(w.ctx, uint64(vecPtr), vecSize)

	// Пишем вектор в память
	vecBytes := unsafe.Slice((*byte)(unsafe.Pointer(&vec[0])), len(vec)*4)
	w.mod.Memory().Write(vecPtr, vecBytes)

	// Вызываем insert
	_, err = w.fnInsert.Call(w.ctx, w.hnswPtr, id, uint64(vecPtr))
	if err != nil {
		return fmt.Errorf("wasm insert failed: %w", err)
	}

	return nil
}

// Delete удаляет вектор из Rust HNSW индекса.
func (w *WasmGraphIndex) Delete(id uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.hnswPtr == 0 {
		return false
	}

	res, err := w.fnDelete.Call(w.ctx, w.hnswPtr, id)
	if err != nil {
		return false
	}

	return res[0] == 1
}

// Search находит K ближайших соседей.
func (w *WasmGraphIndex) Search(query []float32, K int, efSearch int) []SearchResult {
	if len(query) != w.dim || K <= 0 {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.hnswPtr == 0 {
		return nil
	}

	// 1. Аллоцируем буфер для query в WASM
	querySize := uint64(w.dim * 4)
	resQuery, err := w.fnAlloc.Call(w.ctx, querySize)
	if err != nil {
		return nil
	}
	queryPtr := uint32(resQuery[0])
	defer w.fnDealloc.Call(w.ctx, uint64(queryPtr), querySize)

	// Пишем query в память
	queryBytes := unsafe.Slice((*byte)(unsafe.Pointer(&query[0])), len(query)*4)
	w.mod.Memory().Write(queryPtr, queryBytes)

	// 2. Аллоцируем выходные буферы под результаты (K штук)
	outIdsSize := uint64(K * 8) // uint64
	resOutIds, err := w.fnAlloc.Call(w.ctx, outIdsSize)
	if err != nil {
		return nil
	}
	outIdsPtr := uint32(resOutIds[0])
	defer w.fnDealloc.Call(w.ctx, uint64(outIdsPtr), outIdsSize)

	outDistsSize := uint64(K * 4) // float32
	resOutDists, err := w.fnAlloc.Call(w.ctx, outDistsSize)
	if err != nil {
		return nil
	}
	outDistsPtr := uint32(resOutDists[0])
	defer w.fnDealloc.Call(w.ctx, uint64(outDistsPtr), outDistsSize)

	// 3. Вызываем поиск
	resCount, err := w.fnSearch.Call(w.ctx,
		w.hnswPtr,
		uint64(queryPtr),
		uint64(K),
		uint64(efSearch),
		uint64(outIdsPtr),
		uint64(outDistsPtr),
	)
	if err != nil {
		return nil
	}

	count := int(resCount[0])
	if count <= 0 {
		return nil
	}

	// Читаем результаты напрямую из WASM памяти
	idsBytes, ok := w.mod.Memory().Read(outIdsPtr, uint32(count*8))
	if !ok {
		return nil
	}
	distsBytes, ok := w.mod.Memory().Read(outDistsPtr, uint32(count*4))
	if !ok {
		return nil
	}

	results := make([]SearchResult, count)
	for i := 0; i < count; i++ {
		id := *(*uint64)(unsafe.Pointer(&idsBytes[i*8]))
		distBits := *(*uint32)(unsafe.Pointer(&distsBytes[i*4]))
		dist := math.Float32frombits(distBits)
		results[i] = SearchResult{
			ID:       id,
			Distance: dist,
		}
	}

	return results
}

// Clear полностью очищает Rust HNSW индекс.
func (w *WasmGraphIndex) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.hnswPtr != 0 {
		_, _ = w.fnClear.Call(w.ctx, w.hnswPtr)
	}
}

// Info возвращает статистику индекса.
func (w *WasmGraphIndex) Info() (nodeCount int, dimension int, maxLevel int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.hnswPtr == 0 {
		return 0, 0, 0
	}

	// Аллоцируем 3 указателя по 4 байта
	size := uint64(4)
	resNodes, _ := w.fnAlloc.Call(w.ctx, size)
	ptrNodes := uint32(resNodes[0])
	defer w.fnDealloc.Call(w.ctx, uint64(ptrNodes), size)

	resDim, _ := w.fnAlloc.Call(w.ctx, size)
	ptrDim := uint32(resDim[0])
	defer w.fnDealloc.Call(w.ctx, uint64(ptrDim), size)

	resMaxLvl, _ := w.fnAlloc.Call(w.ctx, size)
	ptrMaxLvl := uint32(resMaxLvl[0])
	defer w.fnDealloc.Call(w.ctx, uint64(ptrMaxLvl), size)

	_, err := w.fnInfo.Call(w.ctx,
		w.hnswPtr,
		uint64(ptrNodes),
		uint64(ptrDim),
		uint64(ptrMaxLvl),
	)
	if err != nil {
		return 0, 0, 0
	}

	bytesNodes, _ := w.mod.Memory().Read(ptrNodes, 4)
	bytesDim, _ := w.mod.Memory().Read(ptrDim, 4)
	bytesMaxLvl, _ := w.mod.Memory().Read(ptrMaxLvl, 4)

	nodeCount = int(*(*uint32)(unsafe.Pointer(&bytesNodes[0])))
	dimension = int(*(*uint32)(unsafe.Pointer(&bytesDim[0])))
	maxLevel = int(*(*uint32)(unsafe.Pointer(&bytesMaxLvl[0])))

	return nodeCount, dimension, maxLevel
}
