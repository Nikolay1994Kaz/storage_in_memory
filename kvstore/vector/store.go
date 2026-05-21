package vector

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"unsafe"

	"kvstore/kvstore/internal/compute"
)

// VectorStore — обёртка над HNSW-графом для интеграции с Molten.
//
// Обеспечивает:
//   - Маппинг строковых ключей → внутренние uint64 ID
//   - Потокобезопасность
//   - Валидацию размерности
//   - Динамическое переключение движков (0 = Go, 1 = Go/WASM SIMD Hybrid)
type VectorStore struct {
	graph *Graph

	// Двусторонний маппинг: строковый ключ ↔ внутренний ID.
	keys map[uint64]string // internal ID → user key
	ids  map[string]uint64 // user key → internal ID

	// Атомарный счётчик для генерации уникальных ID.
	nextID atomic.Uint64

	// Размерность векторов. Устанавливается при первой вставке.
	dim int

	// Слой интеграции WebAssembly HNSW (Rust) движка
	useWasmIndex bool
	workerLocal  *compute.WorkerLocalEngine
	metric       uint32 // 0 = Euclidean, 1 = Cosine

	mu sync.RWMutex
}

// NewVectorStore создаёт хранилище векторов.
func NewVectorStore(distance DistanceFunc) *VectorStore {
	metric := uint32(0)
	if reflect.ValueOf(distance).Pointer() == reflect.ValueOf(CosineDistance).Pointer() {
		metric = 1
	}

	vs := &VectorStore{
		keys:   make(map[uint64]string),
		ids:    make(map[string]uint64),
		metric: metric,
	}
	vs.graph = NewGraph(vs.calculateDistance)
	return vs
}

// SetWorkerLocalEngine устанавливает пул воркеров WASM.
func (vs *VectorStore) SetWorkerLocalEngine(wle *compute.WorkerLocalEngine) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	vs.workerLocal = wle
}

// calculateDistance вычисляет расстояние между векторами с возможностью WASM-ускорения.
func (vs *VectorStore) calculateDistance(workerID int, a, b []float32) float32 {
	if vs.useWasmIndex && vs.workerLocal != nil {
		slot := vs.workerLocal.Slot(workerID, "vector_math")
		if slot != nil {
			var funcName string
			if vs.metric == 0 {
				funcName = "simd_euclidean_distance"
			} else {
				funcName = "simd_cosine_distance"
			}

			fn := slot.GetFunction(funcName)
			if fn != nil {
				// zero-alloc cast float32 slices to byte slices
				aBytes := unsafe.Slice((*byte)(unsafe.Pointer(&a[0])), len(a)*4)
				bBytes := unsafe.Slice((*byte)(unsafe.Pointer(&b[0])), len(b)*4)

				// Write vectors to slot persistent memory buffers
				slot.Memory().Write(compute.InputOffset, aBytes)
				slot.Memory().Write(compute.ValueOffset, bBytes)

				// Invoke optimized WASM SIMD kernel using zero-alloc CallWithStack
				slot.Stack[0] = uint64(compute.InputOffset)
				slot.Stack[1] = uint64(compute.ValueOffset)
				slot.Stack[2] = uint64(len(a))

				err := fn.CallWithStack(context.Background(), slot.Stack[:3])
				if err == nil {
					return math.Float32frombits(uint32(slot.Stack[0]))
				}
			}
		}
	}

	// Fallback to pure Go calculation
	if vs.metric == 0 {
		return EuclideanDistance(workerID, a, b)
	}
	return CosineDistance(workerID, a, b)
}

// SetEngine переключает движок векторного поиска:
//
//	0 = Go Engine (Current)
//	1 = Go/WASM SIMD Hybrid Engine (New)
func (vs *VectorStore) SetEngine(engine int) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if engine == 1 {
		vs.useWasmIndex = true
	} else {
		vs.useWasmIndex = false
	}
	return nil
}

// Engine возвращает текущий движок (0 = Go, 1 = Go/WASM Hybrid)
func (vs *VectorStore) Engine() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	if vs.useWasmIndex {
		return 1
	}
	return 0
}

// Add добавляет вектор с указанным ключом.
func (vs *VectorStore) Add(workerID int, key string, vec []float32) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	// Валидация размерности
	if vs.dim == 0 {
		vs.dim = len(vec)
	} else if len(vec) != vs.dim {
		return fmt.Errorf("dimension mismatch: expected %d, got %d", vs.dim, len(vec))
	}

	if oldID, exists := vs.ids[key]; exists {
		vs.graph.Delete(workerID, oldID)
		delete(vs.keys, oldID)
	}

	id := vs.nextID.Add(1)
	vs.ids[key] = id
	vs.keys[id] = key

	// Вставляем в Go граф с использованием calculateDistance
	vs.graph.Insert(workerID, id, vec)

	return nil
}

// Delete удаляет вектор по ключу.
func (vs *VectorStore) Delete(workerID int, key string) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	id, exists := vs.ids[key]
	if !exists {
		return false
	}

	// Удаляем из Go графа
	vs.graph.Delete(workerID, id)

	delete(vs.ids, key)
	delete(vs.keys, id)

	return true
}

// SearchResult — один результат поиска.
type VSearchResult struct {
	Key      string
	Distance float32
}

// Search находит K ближайших векторов к запросу.
func (vs *VectorStore) Search(workerID int, query []float32, K int) ([]VSearchResult, error) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	if vs.dim == 0 {
		return nil, nil // пустой граф
	}
	if len(query) != vs.dim {
		return nil, fmt.Errorf("query dimension mismatch: expected %d, got %d", vs.dim, len(query))
	}

	efSearch := K * 10
	if efSearch < 100 {
		efSearch = 100
	}

	// Поиск в Go HNSW графе (который вызовет calculateDistance)
	results := vs.graph.Search(workerID, query, K, efSearch)

	out := make([]VSearchResult, len(results))
	for i, r := range results {
		out[i] = VSearchResult{
			Key:      vs.keys[r.ID],
			Distance: r.Distance,
		}
	}
	return out, nil
}

// Info возвращает статистику хранилища.
func (vs *VectorStore) Info() (nodeCount int, dim int, maxLevel int) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return len(vs.ids), vs.dim, vs.graph.MaxLevel()
}

// ForEach вызывает fn для каждого вектора в хранилище.
func (vs *VectorStore) ForEach(fn func(key string, vec []float32)) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	vs.graph.mu.RLock()
	defer vs.graph.mu.RUnlock()

	for id, key := range vs.keys {
		if node, ok := vs.graph.nodes[id]; ok {
			fn(key, node.Vector)
		}
	}
}

// Clear удаляет ВСЕ векторы из хранилища.
func (vs *VectorStore) Clear() {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	vs.graph = NewGraph(vs.graph.Distance)
	vs.keys = make(map[uint64]string)
	vs.ids = make(map[string]uint64)
	vs.dim = 0
}

func SerializeVector(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func DeserializeVector(data []byte) []float32 {
	n := len(data) / 4
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		bits := binary.LittleEndian.Uint32(data[i*4:])
		vec[i] = math.Float32frombits(bits)
	}
	return vec
}
