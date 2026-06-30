package vector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sync"
	"unsafe"

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

	// LSH-индекс для предварительной фильтрации при поиске.
	// Плоский массив хэшей + POPCNT brute-force. 0 аллокаций на поиск.
	// Создаётся лениво при первой вставке (когда dim известен).
	lsh *LSHIndex

	allocator *tcmalloc.TCMallocStore // Ссылка на менеджер памяти
	EfSearch  int                     // Явная ширина поиска HNSW (если <= 0, то K * 10)
	UseLSH    bool                    // Включить LSH-фильтрацию для высокоразмерных векторов (по умолчанию false)

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

// SetHNSWParams настраивает параметры HNSW-графа.
func (vs *VectorStore) SetHNSWParams(M, efConstruction, efSearch int) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if M > 0 {
		vs.graph.M = M
		vs.graph.M0 = 2 * M
		vs.graph.Ml = 1.0 / math.Log(float64(M))
		vs.graph.pruneBufItems = make([]item, 0, vs.graph.M0+1)
		vs.graph.pruneBufIDs = make([]uint64, 0, vs.graph.M0+1)
		vs.graph.insertBuf = make([]uint64, 0, vs.graph.M0+1)
	}
	if efConstruction > 0 {
		vs.graph.EfConstruction = efConstruction
	}
	if efSearch > 0 {
		vs.EfSearch = efSearch
	}
}

// SetUseLSH включает или выключает использование LSH-индекса для поиска.
func (vs *VectorStore) SetUseLSH(use bool) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	vs.UseLSH = use
}


// Add добавляет вектор с указанным ключом.
func (vs *VectorStore) Add(key string, vec []float32) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	// Валидация размерности
	if vs.dim == 0 {
		vs.dim = len(vec)
		// Ленивая инициализация LSH-индекса при первой вставке,
		// только для высокоразмерных векторов (dim >= 256).
		if vs.dim >= 256 && vs.UseLSH {
			// seed=42 — детерминированные проекции для воспроизводимости.
			vs.lsh = NewLSHIndex(vs.dim, 42)
		}
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

	// LSH: вычисляем хэш и записываем в плоский массив
	if vs.lsh != nil {
		vs.lsh.Insert(idx, insertVec)
	}

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

	// LSH: помечаем sentinel (Hamming=64 → не пройдёт threshold)
	if vs.lsh != nil {
		vs.lsh.Delete(uint32(id))
	}

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
func (vs *VectorStore) Search(query []float32, K int, dst []VSearchResult) ([]VSearchResult, error) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	if vs.dim == 0 {
		return nil, nil // пустой граф
	}
	if len(query) != vs.dim {
		return nil, fmt.Errorf("query dimension mismatch: expected %d, got %d", vs.dim, len(query))
	}

	// Если размерность высокая (>= 256) и LSH инициализирован, используем LSH
	if vs.dim >= 256 && vs.lsh != nil {
		return vs.searchWithLSHNoLock(query, K, 14, dst)
	}

	return vs.searchNoLSH(query, K)
}

// SearchFiltered находит K ближайших векторов с фильтрацией по ключу.
//
// filterFn принимает строковый ключ вектора и возвращает true,
// если вектор должен попасть в результат.
//
// Пример использования:
//
//	vs.SearchFiltered(query, 10, func(key string) bool {
//		val, ok := kvStore.Get("meta:" + key)
//		return ok && string(val) == "electronics"
//	})
func (vs *VectorStore) SearchFiltered(query []float32, K int, filterFn func(string) bool, dst []VSearchResult) ([]VSearchResult, error) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	if vs.dim == 0 {
		return nil, nil
	}
	if len(query) != vs.dim {
		return nil, fmt.Errorf("query dimension mismatch: expected %d, got %d", vs.dim, len(query))
	}

	// Pre-normalization
	searchQuery := query
	if vs.autoNormalize {
		normalized := make([]float32, len(query))
		copy(normalized, query)
		Normalize(normalized)
		searchQuery = normalized
	}

	efSearch := vs.EfSearch
	if efSearch <= 0 {
		efSearch = K * 10
		if efSearch < 100 {
			efSearch = 100
		} else if efSearch > 300 {
			efSearch = 300
		}
	}

	// Обёртка: переводим string-фильтр в uint64-фильтр (internal ID → user key).
	internalFilter := func(nodeID uint64) bool {
		key, ok := vs.keys[nodeID]
		if !ok {
			return false // нода без ключа = мёртвая/удалённая
		}
		return filterFn(key)
	}

	results := vs.graph.SearchFiltered(searchQuery, K, efSearch, internalFilter)

	if cap(dst) < len(results) {
		dst = make([]VSearchResult, len(results))
	} else {
		dst = dst[:len(results)]
	}
	for i, r := range results {
		dst[i] = VSearchResult{
			Key:      vs.keys[r.ID],
			Distance: r.Distance,
		}
	}
	return dst, nil
}

// Get возвращает копию вектора по его ключу.
func (vs *VectorStore) Get(key string) ([]float32, bool) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	id, exists := vs.ids[key]
	if !exists {
		return nil, false
	}
	node := &vs.graph.nodes[id]
	if !node.Alive {
		return nil, false
	}

	origVec := vs.graph.arena.Get(node.VectorOffset)
	vecCopy := make([]float32, len(origVec))
	copy(vecCopy, origVec)
	return vecCopy, true
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
	vs.lsh = nil
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

func DeserializeVectorZeroCopy(data []byte) []float32 {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), len(data)/4)
}

// SerializeVectorWithAttrs кодирует вектор + атрибуты в один self-contained блоб
// для WAL-операции OpVSimAddAttrs. Формат: [uint32 nFloats][vec float32 LE...][attrs].
// Секция attrs использует тот же формат, что и снапшот (writeAttrs) → атрибуты
// становятся durable через WAL так же, как через снапшот (закрывает P0-4: replay
// больше не теряет attrs/tenant у векторов, добавленных после последнего снапшота).
func SerializeVectorWithAttrs(vec []float32, attrs Attributes) []byte {
	var buf bytes.Buffer
	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], uint32(len(vec)))
	buf.Write(u4[:])
	for _, v := range vec {
		binary.LittleEndian.PutUint32(u4[:], math.Float32bits(v))
		buf.Write(u4[:])
	}
	// writeAttrs пишет в io.Writer; на bytes.Buffer ошибки не бывает.
	_ = writeAttrs(&buf, attrs)
	return buf.Bytes()
}

// DeserializeVectorWithAttrs — обратная операция для replay OpVSimAddAttrs.
func DeserializeVectorWithAttrs(data []byte) ([]float32, Attributes, error) {
	r := bytes.NewReader(data)
	var u4 [4]byte
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return nil, Attributes{}, fmt.Errorf("vec+attrs: read nFloats: %w", err)
	}
	n := int(binary.LittleEndian.Uint32(u4[:]))
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return nil, Attributes{}, fmt.Errorf("vec+attrs: read float[%d]: %w", i, err)
		}
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(u4[:]))
	}
	attrs, err := readAttrs(r)
	if err != nil {
		return nil, Attributes{}, fmt.Errorf("vec+attrs: read attrs: %w", err)
	}
	return vec, attrs, nil
}

