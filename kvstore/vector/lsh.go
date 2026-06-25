package vector

import (
	"fmt"
	"math"
	"math/bits"
	"math/rand"
	"sync"
)

// ============================================================================
// LSH Index — Locally Sensitive Hashing через плоский массив + POPCNT
// ============================================================================
//
// Вариант 1: Brute-force POPCNT по плоскому []uint64.
//
// Архитектура:
//   - hashes []uint64 — плоский массив, индекс = внутренний ID ноды
//   - При Insert: вычисляем 64-бит SimHash через случайные гиперплоскости
//   - При Search: XOR каждого хэша с хэшем запроса, POPCNT ≤ threshold → кандидат
//   - При Delete: sentinel 0xFFFFFFFFFFFFFFFF (Hamming=64 с любым запросом)
//
// Почему плоский массив, а не map/B+Tree:
//   - 0 аллокаций на поиск и вставку
//   - GC видит один объект ([]uint64)
//   - Линейный скан = идеальная cache locality (CPU prefetcher)
//   - 1M хэшей × 1ns/POPCNT ≈ 1ms — приемлемо для pre-filter
//
// Производительность:
//   - Insert: O(dim × numProjections) для хэша + O(1) запись
//   - Search: O(N) сканирование с POPCNT (N = количество векторов)
//   - Delete: O(1) — запись sentinel
//   - RAM:    N × 8 байт

const (
	lshNumBits   = 64    // Количество бит в хэше (= количество гиперплоскостей)
	lshSentinel  = ^uint64(0) // 0xFFFFFFFFFFFFFFFF — sentinel для удалённых
)

// LSHIndex — индекс Locality-Sensitive Hashing.
//
// Все поля — плоские массивы, невидимые для GC (кроме float32 в проекциях).
// Переиспользуемый буфер candidateBuf исключает аллокации на горячем пути.
type LSHIndex struct {
	// Плоский массив хэшей. Индекс = внутренний ID ноды (uint32 из Graph.Insert).
	// Sentinel = lshSentinel (Hamming=64 с любым запросом → автоматически не проходит threshold).
	hashes []uint64

	// Матрица случайных проекций: [64][dim]float32.
	// Каждая строка — нормальный вектор одной гиперплоскости.
	// Генерируется один раз при создании индекса, потом read-only.
	projections [][]float32

	// Размерность векторов.
	dim int

	// Переиспользуемый буфер для результатов поиска кандидатов.
	// Исключает make() на горячем пути.
	candidateBuf []uint64

	// Буфер для вычисления dot product при хэшировании.
	// Переиспользуется между вызовами ComputeHash.
	dotBuf []float64
}

// NewLSHIndex создаёт LSH-индекс для векторов указанной размерности.
//
// seed — начальное значение PRNG для генерации матрицы проекций.
// Один и тот же seed = одни и те же проекции = воспроизводимые хэши.
func NewLSHIndex(dim int, seed int64) *LSHIndex {
	rng := rand.New(rand.NewSource(seed))

	// Генерируем матрицу случайных проекций: 64 гиперплоскостей
	projections := make([][]float32, lshNumBits)
	for i := 0; i < lshNumBits; i++ {
		proj := make([]float32, dim)
		for j := 0; j < dim; j++ {
			// Стандартное нормальное распределение (Box-Muller не нужен,
			// rand.NormFloat64 использует Ziggurat — быстрее)
			proj[j] = float32(rng.NormFloat64())
		}
		projections[i] = proj
	}

	return &LSHIndex{
		hashes:       make([]uint64, 0, 10000),
		projections:  projections,
		dim:          dim,
		candidateBuf: make([]uint64, 0, 1024),
		dotBuf:       make([]float64, lshNumBits),
	}
}

// ComputeHash вычисляет 64-бит SimHash для вектора.
//
// Алгоритм:
//   1. Для каждой из 64 гиперплоскостей считаем dot product с вектором
//   2. Если dot >= 0 → бит = 1, иначе бит = 0
//   3. Собираем 64 бита в одно uint64
//
// Свойство: если два вектора близки по косинусному сходству,
// их хэши будут отличаться на малое число бит (малое расстояние Хэмминга).
//
// Сложность: O(dim × 64) = O(dim) multiply-add операций.
// Для dim=768: 768 × 64 = 49,152 операции ≈ 5-10μs.
func (idx *LSHIndex) ComputeHash(vec []float32) uint64 {
	var hash uint64
	for bit := 0; bit < lshNumBits; bit++ {
		proj := idx.projections[bit]
		var dot float32
		for j := 0; j < len(vec); j++ {
			dot += vec[j] * proj[j]
		}
		if dot >= 0 {
			hash |= 1 << uint(bit)
		}
	}
	return hash
}

// Insert добавляет хэш для ноды с указанным внутренним ID.
//
// nodeID — индекс в Graph.nodes[] (возвращается Graph.Insert).
// Массив hashes автоматически растёт при необходимости.
//
// 0 аллокаций если capacity достаточен (типичный случай после warmup).
func (idx *LSHIndex) Insert(nodeID uint32, vec []float32) {
	hash := idx.ComputeHash(vec)
	id := int(nodeID)

	// Расширяем массив если нужно (заполняем sentinel'ами)
	for len(idx.hashes) <= id {
		idx.hashes = append(idx.hashes, lshSentinel)
	}
	idx.hashes[id] = hash
}

// Delete помечает ноду как удалённую (sentinel).
//
// Sentinel = 0xFFFFFFFFFFFFFFFF. Hamming distance с ЛЮБЫМ запросом = 64 бит
// (потому что для запроса с 32 единицами: XOR даст ~32 единицы,
// а для запроса из всех нулей: XOR даст 64 единицы).
// В любом случае sentinel НИКОГДА не пройдёт threshold ≤ 10-15.
//
// O(1), 0 аллокаций.
func (idx *LSHIndex) Delete(nodeID uint32) {
	id := int(nodeID)
	if id < len(idx.hashes) {
		idx.hashes[id] = lshSentinel
	}
}

// FindCandidates ищет все ноды, чей LSH-хэш отличается от queryHash
// не более чем на threshold бит (расстояние Хэмминга ≤ threshold).
//
// Возвращает слайс ID кандидатов через переиспользуемый буфер (0 аллокаций).
//
// ВНИМАНИЕ: возвращённый слайс валиден только до следующего вызова FindCandidates.
//
// Сложность: O(N), где N = len(hashes).
// На каждый элемент: XOR (1 такт) + POPCNT (1 такт) + branch (1 такт) = ~1ns.
// Для 1M элементов: ~1ms.
func (idx *LSHIndex) FindCandidates(queryHash uint64, threshold int) []uint64 {
	idx.candidateBuf = idx.candidateBuf[:0]

	for i, h := range idx.hashes {
		if bits.OnesCount64(queryHash^h) <= threshold {
			idx.candidateBuf = append(idx.candidateBuf, uint64(i))
		}
	}

	return idx.candidateBuf
}

// FindCandidatesCount возвращает количество кандидатов без сбора ID.
// Полезно для тюнинга threshold.
func (idx *LSHIndex) FindCandidatesCount(queryHash uint64, threshold int) int {
	count := 0
	for _, h := range idx.hashes {
		if bits.OnesCount64(queryHash^h) <= threshold {
			count++
		}
	}
	return count
}

// EstimateThreshold подбирает threshold, который даст примерно targetCount кандидатов.
// Сканирует все хэши для threshold от 0 до maxThreshold, возвращает первый,
// дающий >= targetCount.
func (idx *LSHIndex) EstimateThreshold(queryHash uint64, targetCount int, maxThreshold int) int {
	for t := 0; t <= maxThreshold; t++ {
		count := idx.FindCandidatesCount(queryHash, t)
		if count >= targetCount {
			return t
		}
	}
	return maxThreshold
}

// Len возвращает текущий размер массива хэшей.
func (idx *LSHIndex) Len() int {
	return len(idx.hashes)
}

// Stats возвращает статистику индекса.
func (idx *LSHIndex) Stats() LSHStats {
	alive := 0
	for _, h := range idx.hashes {
		if h != lshSentinel {
			alive++
		}
	}
	return LSHStats{
		TotalSlots: len(idx.hashes),
		AliveCount: alive,
		MemoryBytes: len(idx.hashes)*8 + len(idx.projections)*idx.dim*4,
	}
}

type LSHStats struct {
	TotalSlots  int
	AliveCount  int
	MemoryBytes int
}

// ============================================================================
// Интеграция LSH с HNSW через searchLayerFiltered
// ============================================================================

type lshScoredResult struct {
	key  string
	dist float32
}

type lshSearchState struct {
	scored []lshScoredResult
}

var lshSearchPool = sync.Pool{
	New: func() any {
		return &lshSearchState{
			scored: make([]lshScoredResult, 0, 1024),
		}
	},
}

// SearchWithLSH выполняет двухфазный поиск: LSH pre-filter → Brute-force refine.
// Блокирующая обертка для внешних вызовов.
func (vs *VectorStore) SearchWithLSH(query []float32, K int, threshold int, dst []VSearchResult) ([]VSearchResult, error) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	return vs.searchWithLSHNoLock(query, K, threshold, dst)
}

// searchWithLSHNoLock выполняет двухфазный поиск без захвата RLock.
//
// Фаза 1: Вычисляем LSH-хэш запроса, находим кандидатов с малым Hamming distance.
// Фаза 2: Считаем ТОЧНОЕ расстояние для каждого кандидата, выбираем top-K.
//
// Использует lshSearchPool для предотвращения аллокаций в куче на горячем пути.
func (vs *VectorStore) searchWithLSHNoLock(query []float32, K int, threshold int, dst []VSearchResult) ([]VSearchResult, error) {
	if vs.dim == 0 || vs.lsh == nil {
		return vs.searchNoLSH(query, K)
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

	// ═══════════════════════════════════════════════════
	// ФАЗА 1: LSH — грубый отбор через POPCNT
	// ═══════════════════════════════════════════════════
	queryHash := vs.lsh.ComputeHash(searchQuery)

	minCandidates := vs.EfSearch
	if minCandidates <= 0 {
		minCandidates = K * 10
		if minCandidates < 100 {
			minCandidates = 100
		}
	}

	// Адаптивный threshold: если кандидатов мало — расширяем
	currentThreshold := threshold
	var candidates []uint64
	for currentThreshold <= 32 {
		candidates = vs.lsh.FindCandidates(queryHash, currentThreshold)
		if len(candidates) >= minCandidates {
			break
		}
		currentThreshold += 2
	}

	// Если даже с threshold=32 мало кандидатов — fallback на обычный поиск
	if len(candidates) < K {
		return vs.searchNoLSH(query, K)
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 2: Brute-force точный расчёт по кандидатам
	// ═══════════════════════════════════════════════════
	//
	// Считаем Distance для каждого кандидата, собираем top-K.
	// При len(candidates) = 500-2000 это быстрее, чем HNSW Search
	// (нет overhead'а на heap, pool, visited bitset).

	state := lshSearchPool.Get().(*lshSearchState)
	if cap(state.scored) < len(candidates) {
		state.scored = make([]lshScoredResult, 0, len(candidates))
	} else {
		state.scored = state.scored[:0]
	}

	// Собираем все расстояния
	for _, nodeID := range candidates {
		if nodeID >= uint64(len(vs.graph.nodes)) {
			continue
		}
		node := &vs.graph.nodes[nodeID]
		if !node.Alive {
			continue
		}
		vec := vs.graph.arena.Get(node.VectorOffset)
		dist := vs.graph.Distance(searchQuery, vec)
		state.scored = append(state.scored, lshScoredResult{
			key:  vs.keys[nodeID],
			dist: dist,
		})
	}

	// Partial sort: находим top-K через selection
	if len(state.scored) > K {
		// Простая сортировка insertion sort для малых K
		// (K обычно 5-50, scored обычно 100-2000)
		for i := 0; i < K && i < len(state.scored); i++ {
			minIdx := i
			for j := i + 1; j < len(state.scored); j++ {
				if state.scored[j].dist < state.scored[minIdx].dist {
					minIdx = j
				}
			}
			state.scored[i], state.scored[minIdx] = state.scored[minIdx], state.scored[i]
		}
		state.scored = state.scored[:K]
	}

	out := make([]VSearchResult, len(state.scored))
	for i, s := range state.scored {
		out[i] = VSearchResult{
			Key:      s.key,
			Distance: s.dist,
		}
	}

	// Очищаем ссылки на строки перед возвратом в пул для GC
	for i := range state.scored {
		state.scored[i].key = ""
	}
	lshSearchPool.Put(state)

	return out, nil
}

// searchNoLSH — fallback на обычный поиск без LSH.
func (vs *VectorStore) searchNoLSH(query []float32, K int) ([]VSearchResult, error) {
	if vs.dim == 0 {
		return nil, nil
	}

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

// makeCandidateBitset строит bitset из списка кандидатов для O(1) lookup.
func makeCandidateBitset(candidates []uint64) []uint64 {
	// Находим максимальный ID
	var maxID uint64
	for _, id := range candidates {
		if id > maxID {
			maxID = id
		}
	}

	bitset := make([]uint64, maxID/64+1)
	for _, id := range candidates {
		bitset[id/64] |= 1 << (id % 64)
	}
	return bitset
}

// HammingDistance вычисляет расстояние Хэмминга между двумя хэшами.
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// SimHashSimilarity оценивает косинусное сходство по расстоянию Хэмминга.
// Возвращает приблизительное значение cosine similarity в диапазоне [-1, 1].
func SimHashSimilarity(hammingDist int) float64 {
	return math.Cos(math.Pi * float64(hammingDist) / float64(lshNumBits))
}
