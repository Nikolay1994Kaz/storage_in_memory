package vector

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// VectorStore — обёртка над HNSW-графом для интеграции с Molten.
type VectorStore struct {
	graph *Graph

	// Двусторонний маппинг: строковый ключ ↔ внутренний индекс в []Node.
	keys map[uint64]string // internal index → user key
	ids  map[string]uint64 // user key → internal index

	// Размерность векторов. Устанавливается при первой вставке.
	dim int

	// autoNormalize: если true, все вставляемые векторы и запросы
	// автоматически нормализуются. Это позволяет использовать DotProductDistance
	// вместо CosineDistance (×3 быстрее).
	autoNormalize bool

	allocator *tcmalloc.TCMallocStore // Ссылка на менеджер памяти

	mu sync.RWMutex
}

// NewVectorStore создаёт хранилище векторов с произвольной метрикой.
func NewVectorStore(distance DistanceFunc, allocator *tcmalloc.TCMallocStore) *VectorStore {
	vs := &VectorStore{
		keys:      make(map[uint64]string),
		ids:       make(map[string]uint64),
		allocator: allocator,
	}
	vs.graph = NewGraph(distance, allocator)
	return vs
}

// NewVectorStoreCosine создаёт хранилище для косинусного поиска.
func NewVectorStoreCosine(allocator *tcmalloc.TCMallocStore) *VectorStore {
	vs := &VectorStore{
		keys:          make(map[uint64]string),
		ids:           make(map[string]uint64),
		autoNormalize: true,
		allocator:     allocator,
	}
	vs.graph = NewGraph(DotProductDistance, allocator)
	return vs
}


// Add добавляет вектор с указанным ключом.
func (vs *VectorStore) Add(key string, vec []float32) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	// Валидация размерности
	if vs.dim == 0 {
		vs.dim = len(vec)
	} else if len(vec) != vs.dim {
		return fmt.Errorf("dimension mismatch: expected %d, got %d", vs.dim, len(vec))
	}

	if oldID, exists := vs.ids[key]; exists {
		vs.graph.Delete(oldID)
		delete(vs.keys, oldID)
	}

	// Pre-normalization: нормализуем копию вектора перед вставкой.
	// Оригинал пользователя не трогаем.
	insertVec := vec
	if vs.autoNormalize {
		normalized := make([]float32, len(vec))
		copy(normalized, vec)
		Normalize(normalized)
		insertVec = normalized
	}

	idx := vs.graph.Insert(insertVec)
	id := uint64(idx)
	vs.ids[key] = id
	vs.keys[id] = key

	return nil
}

// Delete удаляет вектор по ключу.
func (vs *VectorStore) Delete(key string) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	id, exists := vs.ids[key]
	if !exists {
		return false
	}

	vs.graph.Delete(id)

	delete(vs.ids, key)
	delete(vs.keys, id)

	return true
}

// VSearchResult — один результат поиска.
type VSearchResult struct {
	Key      string
	Distance float32
}

// Search находит K ближайших векторов к запросу.
func (vs *VectorStore) Search(query []float32, K int) ([]VSearchResult, error) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	if vs.dim == 0 {
		return nil, nil // пустой граф
	}
	if len(query) != vs.dim {
		return nil, fmt.Errorf("query dimension mismatch: expected %d, got %d", vs.dim, len(query))
	}

	// Pre-normalization: нормализуем копию запроса.
	searchQuery := query
	if vs.autoNormalize {
		normalized := make([]float32, len(query))
		copy(normalized, query)
		Normalize(normalized)
		searchQuery = normalized
	}

	efSearch := K * 10
	if efSearch < 100 {
		efSearch = 100
	}

	results := vs.graph.Search(searchQuery, K, efSearch)

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

	for id, key := range vs.keys {
		node := &vs.graph.nodes[id]
		if node.Alive {
			fn(key, vs.graph.arena.Get(node.VectorOffset))
		}
	}
}

// Clear удаляет ВСЕ векторы из хранилища.
func (vs *VectorStore) Clear() {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	vs.graph = NewGraph(vs.graph.Distance, vs.allocator)
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
