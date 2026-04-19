package vector

import (
	"encoding/binary"
	"fmt"
	"math"
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

	mu sync.RWMutex
}

// NewVectorStore создаёт хранилище векторов.
//
// distance — функция расстояния (EuclideanDistance или CosineDistance).
func NewVectorStore(distance DistanceFunc) *VectorStore {
	return &VectorStore{
		graph: NewGraph(distance),
		keys:  make(map[uint64]string),
		ids:   make(map[string]uint64),
	}
}

// Add добавляет вектор с указанным ключом.
//
// Если ключ уже существует — обновляет вектор.
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

	// Если ключ уже есть — удаляем старый и вставляем новый.
	// HNSW не поддерживает in-place update, поэтому re-insert.
	// TODO: реализовать Delete для HNSW-графа.
	if _, exists := vs.ids[key]; exists {
		// Пока просто перезаписываем маппинг (старая нода остаётся в графе).
		// Это неидеально, но для первой версии достаточно.
	}

	id := vs.nextID.Add(1)
	vs.ids[key] = id
	vs.keys[id] = key

	vs.graph.Insert(id, vec)
	return nil
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

	results := vs.graph.Search(query, K, efSearch)

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
