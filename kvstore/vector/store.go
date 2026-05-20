package vector

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
)

// VectorStore — обёртка над HNSW-графом для интеграции с Molten.
//
// Аналог ArenaStore для KV, но для векторов.
// Обеспечивает:
//   - Маппинг строковых ключей → внутренние uint64 ID
//   - Потокобезопасность
//   - Валидацию размерности
//   - Динамическое переключение движков (0 = Go/WASM Hybrid, 1 = Rust/WASM Full HNSW)
type VectorStore struct {
	graph *Graph

	// Двусторонний маппинг: строковый ключ ↔ внутренний ID.
	// Зачем? Graph работает с uint64 (для скорости),
	// а пользователь работает со строками ("product:shoes").
	keys map[uint64]string // internal ID → user key
	ids  map[string]uint64 // user key → internal ID

	// Атомарный счётчик для генерации уникальных ID.
	// Каждый новый вектор получает nextID, потом nextID++.
	nextID atomic.Uint64

	// Размерность векторов. Устанавливается при первой вставке.
	// Все последующие вставки должны совпадать.
	dim int

	// Слой интеграции WebAssembly HNSW (Rust) движка
	useWasmIndex bool
	wasmGraph    *WasmGraphIndex
	metric       uint32 // 0 = Euclidean, 1 = Cosine

	mu sync.RWMutex
}

// NewVectorStore создаёт хранилище векторов.
//
// distance — функция расстояния (EuclideanDistance или CosineDistance).
func NewVectorStore(distance DistanceFunc) *VectorStore {
	metric := uint32(0)
	// С помощью рефлексии определяем метрику (Euclidean или Cosine)
	if reflect.ValueOf(distance).Pointer() == reflect.ValueOf(CosineDistance).Pointer() {
		metric = 1
	}

	return &VectorStore{
		graph:  NewGraph(distance),
		keys:   make(map[uint64]string),
		ids:    make(map[string]uint64),
		metric: metric,
	}
}

// findWasmFile пытается найти файл .wasm в возможных путях
func findWasmFile() ([]byte, error) {
	candidates := []string{
		"rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm",
		"../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm",
		"../../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm",
	}
	var lastErr error
	for _, c := range candidates {
		data, err := os.ReadFile(c)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("failed to find compiled WASM binary: %w", lastErr)
}

// initWasmGraph инициализирует WASM Rust HNSW граф
func (vs *VectorStore) initWasmGraph() error {
	if vs.wasmGraph != nil {
		return nil
	}
	wasmBytes, err := findWasmFile()
	if err != nil {
		return err
	}
	g, err := NewWasmGraphIndex(context.Background(), wasmBytes, vs.dim, vs.metric)
	if err != nil {
		return err
	}
	vs.wasmGraph = g
	return nil
}

// SetEngine переключает движок векторного поиска:
//
//	0 = Go Engine (Current)
//	1 = Rust/WASM Full HNSW Engine (New)
//
// При переключении на Rust автоматически синхронизирует все существующие векторы!
func (vs *VectorStore) SetEngine(engine int) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if engine == 1 {
		if !vs.useWasmIndex {
			vs.useWasmIndex = true
			if vs.dim > 0 && vs.wasmGraph == nil {
				if err := vs.initWasmGraph(); err != nil {
					vs.useWasmIndex = false // Откатываемся при ошибке
					return err
				}
				// Синхронизируем все существующие векторы из Go в Rust WASM
				for id, key := range vs.keys {
					if node, ok := vs.graph.nodes[id]; ok {
						if err := vs.wasmGraph.Insert(id, node.Vector); err != nil {
							vs.useWasmIndex = false
							return fmt.Errorf("failed to sync vector %s to Rust HNSW: %w", key, err)
						}
					}
				}
			}
		}
	} else {
		vs.useWasmIndex = false
	}
	return nil
}

// Engine возвращает текущий движок (0 = Go, 1 = Rust/WASM)
func (vs *VectorStore) Engine() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	if vs.useWasmIndex {
		return 1
	}
	return 0
}

// Add добавляет вектор с указанным ключом.
//
// Если ключ уже существует — удаляет старую ноду из графа и вставляет новую.
// Если размерность не совпадает с первым вектором — ошибка.
func (vs *VectorStore) Add(key string, vec []float32) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	// Валидация размерности
	if vs.dim == 0 {
		vs.dim = len(vec) // первый вектор задаёт размерность
	} else if len(vec) != vs.dim {
		return fmt.Errorf("dimension mismatch: expected %d, got %d", vs.dim, len(vec))
	}

	// Если ключ уже есть — удаляем старую ноду из графа.
	// Без этого старая нода остаётся как «зомби»: занимает память,
	// участвует в поиске, но маппинг уже указывает на новый ID.
	if oldID, exists := vs.ids[key]; exists {
		vs.graph.Delete(oldID)
		if vs.wasmGraph != nil {
			vs.wasmGraph.Delete(oldID)
		}
		delete(vs.keys, oldID)
	}

	id := vs.nextID.Add(1)
	vs.ids[key] = id
	vs.keys[id] = key

	// Вставляем в Go граф (сохраняем всегда для возможности бесшовного отката обратно)
	vs.graph.Insert(id, vec)

	// Если включен или был инициализирован Rust WASM
	if vs.useWasmIndex {
		if err := vs.initWasmGraph(); err != nil {
			return err
		}
	}
	if vs.wasmGraph != nil {
		if err := vs.wasmGraph.Insert(id, vec); err != nil {
			return err
		}
	}

	return nil
}

// Delete удаляет вектор по ключу.
//
// Удаляет ноду из HNSW-графа (с ремонтом связей) и очищает маппинги.
// Возвращает true если ключ существовал, false если нечего было удалять.
func (vs *VectorStore) Delete(key string) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	id, exists := vs.ids[key]
	if !exists {
		return false
	}

	// Удаляем из Go графа
	vs.graph.Delete(id)

	// Удаляем из Rust WASM графа
	if vs.wasmGraph != nil {
		vs.wasmGraph.Delete(id)
	}

	delete(vs.ids, key)
	delete(vs.keys, id)

	return true
}

// Search находит K ближайших векторов к запросу.
type VSearchResult struct {
	Key      string
	Distance float32
}

func (vs *VectorStore) Search(query []float32, K int) ([]VSearchResult, error) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	if vs.dim == 0 {
		return nil, nil // пустой граф
	}
	if len(query) != vs.dim {
		return nil, fmt.Errorf("query dimension mismatch: expected %d, got %d", vs.dim, len(query))
	}

	// efSearch = max(K * 10, 100) — хороший баланс точность/скорость.
	efSearch := K * 10
	if efSearch < 100 {
		efSearch = 100
	}

	var results []SearchResult
	if vs.useWasmIndex && vs.wasmGraph != nil {
		// Полный поиск в Rust HNSW WASM
		results = vs.wasmGraph.Search(query, K, efSearch)
	} else {
		// Поиск в Go HNSW
		results = vs.graph.Search(query, K, efSearch)
	}

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

	if vs.useWasmIndex && vs.wasmGraph != nil {
		return vs.wasmGraph.Info()
	}

	return len(vs.ids), vs.dim, vs.graph.MaxLevel()
}

// ForEach вызывает fn для каждого вектора в хранилище.
// Используется для полной синхронизации при репликации.
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
//
// Используется при Full Sync репликации: реплика получает +FULLSYNC
// и очищает все данные перед приёмом свежего слепка от мастера.
// Аналог FLUSHALL в Redis.
func (vs *VectorStore) Clear() {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	vs.graph = NewGraph(vs.graph.Distance)
	if vs.wasmGraph != nil {
		vs.wasmGraph.Clear()
	}
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
