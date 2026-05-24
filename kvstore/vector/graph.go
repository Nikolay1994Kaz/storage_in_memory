package vector

import (
	"math"
	"math/rand"
	"slices"
	"sync"
)

// Node — одна точка в графе HNSW.
//
// Представь город на карте:
//   - ID — уникальный номер города
//   - Vector — координаты (широта, долгота, ... в N-мерном пространстве)
//   - Level — "значимость" города. Москва = 3 (есть на слоях 0,1,2,3).
//     Рязань = 0 (только на слое 0).
//   - Neighbors — список соседних городов НА КАЖДОМ слое.
//     Neighbors[0] = соседи на слое 0
//     Neighbors[1] = соседи на слое 1 (если Level >= 1)
//     и т.д.

// searchState — переиспользуемые буферы для searchLayer.
//
// Вместо того чтобы на КАЖДЫЙ поиск создавать map, heap, slice —
// мы создаём их ОДИН РАЗ и переиспользуем через sync.Pool.
//
// Это тот же паттерн, что ты использовал в Pub/Sub:
//
//	var subscriberSlicePool = sync.Pool{...}
//
// Только здесь мы пулим не слайс, а целый набор буферов.
type searchState struct {
	visited    []uint64 // bitset: 1 бит на ноду (вместо map[uint64]bool)
	candidates minHeap  // очередь кандидатов (minHeap)
	results    maxHeap  // лучшие результаты (maxHeap)
	collected  []item   // буфер для сбора финальных результатов
}

// isVisited проверяет, была ли нода посещена.
// Одна инструкция AND — вместо hash + bucket lookup в map.
func (s *searchState) isVisited(id uint64) bool {
	return s.visited[id/64]&(1<<(id%64)) != 0
}

// setVisited помечает ноду как посещённую.
// Одна инструкция OR — вместо hash + bucket insert в map.
func (s *searchState) setVisited(id uint64) {
	s.visited[id/64] |= 1 << (id % 64)
}

// searchPool — пул переиспользуемых состояний поиска.
//
// sync.Pool.New вызывается только когда пул ПУСТ.
// После первых нескольких запросов — все берётся из пула, 0 аллокаций.
var searchPool = sync.Pool{
	New: func() any {
		return &searchState{
			visited:    make([]uint64, 0, 256), // вырастет при первом acquire
			candidates: make(minHeap, 0, 256),
			results:    make(maxHeap, 0, 256),
			collected:  make([]item, 0, 256),
		}
	},
}

// acquire берёт состояние из пула и СБРАСЫВАЕТ все буферы.
//
// nodeSlots — количество ячеек в []Node (включая дыры от Delete).
// Bitset растёт по мере роста графа, но никогда не уменьшается
// (чтобы не делать лишних аллокаций).
//
// Обнуление bitset через for-range: Go компилятор распознаёт этот паттерн
// и генерирует runtime.memclrNoHeapPointers — наносекунды для 1-2 КБ.
func (s *searchState) acquire(nodeSlots int) {
	needed := (nodeSlots + 63) / 64 // ceil(nodeSlots / 64)
	if cap(s.visited) < needed {
		s.visited = make([]uint64, needed)
	} else {
		s.visited = s.visited[:needed]
		for i := range s.visited {
			s.visited[i] = 0 // memclr intrinsic
		}
	}
	s.candidates = s.candidates[:0] // обнуляем длину, capacity остаётся
	s.results = s.results[:0]       // то же
	s.collected = s.collected[:0]   // то же
}

type Node struct {
	ID              uint64
	VectorOffset    uint32
	NeighborsOffset uint32 // смещение в NeighborsArena
	Level           int    // максимальный слой, на котором нода присутствует
	Alive           bool   // маркер «живая ли нода» (tombstone при Delete)
}

// Graph — HNSW-граф.
//
// Это «карта городов» с несколькими слоями масштаба.
type Graph struct {
	// Хранилище всех нод — плоский массив (арена).
	// Индекс в слайсе = внутренний ID ноды. Прямой доступ O(1) без хэширования.
	// GC видит один объект вместо тысяч отдельных *Node.
	nodes     []Node    // плоский массив, индекс = ID ноды
	nodeCount int       // количество живых нод (без дыр/tombstone)
	freeIDs   []uint32  // стек свободных индексов (от Delete)

	// Точка входа — нода на самом верхнем уровне.
	// Поиск ВСЕГДА начинается с неё (как «вылет из Москвы»).
	entryPointID uint64

	// Текущий максимальный слой в графе.
	// Растёт по мере вставки нод (если новая нода «выбросила» высокий уровень).
	maxLevel int

	// ─── Параметры алгоритма ───

	// M — максимальное количество соседей на слоях >= 1.
	// На слое 0 используется M0 = 2*M (нижний слой самый плотный).
	//
	// Аналогия: M = сколько прямых авиарейсов из каждого города.
	// Больше рейсов = быстрее найдёшь нужный город, но дороже содержать аэропорт.
	M  int
	M0 int // = 2*M, максимум соседей на слое 0

	// Ml — параметр для генерации случайного уровня ноды.
	// ml = 1 / ln(M)
	//
	// Управляет тем, как часто ноды попадают на верхние слои.
	// Чем меньше ml — тем «площе» структура (меньше слоёв).
	Ml float64

	// EfConstruction — ширина поиска при ВСТАВКЕ.
	// Сколько кандидатов в соседи мы рассматриваем.
	// Больше = точнее граф, но медленнее вставка.
	EfConstruction int

	// Функция расстояния (Euclidean или Cosine).
	// Тот же паттерн, что Handler в server.go — функция как параметр.
	Distance DistanceFunc

	arena          *VectorArena
	neighborsArena *NeighborsArena

	// ─── Переиспользуемые буферы (zero-alloc pruning/insert) ───
	// Безопасны без мьютекса: Insert и Delete вызываются только под
	// эксклюзивным mu.Lock() в VectorStore.
	pruneBufItems   []item   // буфер для pruneNeighbors (capacity = M0)
	pruneBufIDs     []uint64 // буфер для ID после pruning (capacity = M0)
	insertBuf       []uint64 // буфер для обратных связей в Insert (capacity = M0+1)
	searchResultBuf []item   // буфер для результатов searchLayer
}

// NewGraph создаёт пустой HNSW-граф с параметрами по умолчанию.
//
// Параметры по умолчанию подобраны из оригинальной статьи HNSW (2016)
// и практики Pinecone/Weaviate/Milvus:
//
//	M=16, efConstruction=200 — хороший баланс скорость/качество.
func NewGraph(distance DistanceFunc) *Graph {
	m := 16
	m0 := 2 * m
	return &Graph{
		nodes:           make([]Node, 0, 10000),
		freeIDs:         make([]uint32, 0, 64),
		M:               m,
		M0:              m0, // слой 0 в 2 раза плотнее (из оригинальной статьи)
		Ml:              1.0 / math.Log(float64(m)),
		EfConstruction:  200,
		Distance:        distance,
		neighborsArena:  NewNeighborsArena(m, m0, 10000),
		pruneBufItems:   make([]item, 0, m0+1),
		pruneBufIDs:     make([]uint64, 0, m0+1),
		insertBuf:       make([]uint64, 0, m0+1),
		searchResultBuf: make([]item, 0, 256),
	}
}

// randomLevel генерирует случайный уровень для новой ноды.
//
// Это тот самый "кубик", который решает: Москва (level=3) или Рязань (level=0)?
//
// Используется экспоненциальное распределение:
//
//	level = floor(-ln(random) * mL)
//
// Что это значит на пальцах:
//   - random — случайное число от 0 до 1
//   - -ln(random) — почти всегда маленькое число (0-2), РЕДКО большое (>3)
//   - Умножаем на mL (~0.36) → ещё уменьшаем
//   - floor → округляем вниз до целого
//
// Результат:
//
//	level=0 выпадает в ~64% случаев
//	level=1 — ~23%
//	level=2 — ~8%
//	level=3 — ~3%
//	level=4 — ~1%
//
// Это даёт нам «пирамиду»: много нод внизу, мало наверху.
// Идеально для навигации: верхние слои = быстрый грубый поиск,
// нижние = точная доводка.
func (g *Graph) randomLevel() int {
	return int(-math.Log(rand.Float64()) * g.Ml)
}

// maxNeighbors возвращает максимум соседей для данного слоя.
//
// Слой 0 — самый плотный, там 2*M соседей.
// Все остальные слои — M соседей.
//
// Почему? Слой 0 содержит ВСЕ ноды. Чтобы поиск на нём был точным,
// нужно больше связей. Верхние слои разреженные — M достаточно.
func (g *Graph) maxNeighbors(level int) int {
	if level == 0 {
		return g.M0
	}
	return g.M
}

// searchLayer ищет ef ближайших нод к query на одном слое графа.
//
// ★ ZERO-ALLOC версия ★
// Все буферы берутся из sync.Pool и возвращаются после использования.
// Результат записывается в переиспользуемый буфер dst (или searchResultBuf).
func (g *Graph) searchLayer(query []float32, entryID uint64, ef int, level int) []item {

	state := searchPool.Get().(*searchState)
	state.acquire(len(g.nodes))

	entryNode := &g.nodes[entryID]
	entryDist := g.Distance(query, g.arena.Get(entryNode.VectorOffset))

	state.setVisited(entryID)

	entry := item{id: entryID, dist: entryDist}
	state.candidates.push(entry)
	state.results.push(entry)

	for state.candidates.Len() > 0 {
		closest := state.candidates.pop()
		farthestResult := state.results.peek()

		if closest.dist > farthestResult.dist {
			break
		}

		node := &g.nodes[closest.id]
		for _, neighborID := range g.neighborsArena.GetNeighbors(node.NeighborsOffset, level) {
			if state.isVisited(neighborID) {
				continue
			}
			state.setVisited(neighborID)

			neighborNode := &g.nodes[neighborID]
			neighborDist := g.Distance(query, g.arena.Get(neighborNode.VectorOffset))

			farthestResult = state.results.peek()

			if neighborDist < farthestResult.dist || state.results.Len() < ef {
				newItem := item{id: neighborID, dist: neighborDist}
				state.candidates.push(newItem)
				state.results.push(newItem)

				if state.results.Len() > ef {
					state.results.pop()
				}
			}
		}
	}

	// Собираем результаты из maxHeap в обратном порядке
	for state.results.Len() > 0 {
		state.collected = append(state.collected, state.results.pop())
	}
	for i, j := 0, len(state.collected)-1; i < j; i, j = i+1, j-1 {
		state.collected[i], state.collected[j] = state.collected[j], state.collected[i]
	}

	// ★ Вместо make([]item) — переиспользуем буфер searchResultBuf.
	// Безопасно: вызывающий код использует результат до следующего searchLayer.
	g.searchResultBuf = g.searchResultBuf[:0]
	g.searchResultBuf = append(g.searchResultBuf, state.collected...)

	searchPool.Put(state)
	return g.searchResultBuf
}

// greedyClosest — специализированный поиск ближайшей ноды для ef=1.
//
// На верхних слоях HNSW мы ищем ровно одну ближайшую ноду (жадный спуск).
// searchLayer для ef=1 избыточен: pool + map + heap ради одного числа.
//
// greedyClosest делает то же самое, но:
//   - 0 аллокаций
//   - Не использует sync.Pool, map, heap
//   - Компилируется в тесный цикл с Distance-вызовами
//
// Не отслеживает visited — на верхних слоях граф разрежённый,
// циклов практически не бывает. Даже если нода проверится дважды —
// это один лишний Distance (~200ns), что дешевле pool+map (~500ns+).
func (g *Graph) greedyClosest(query []float32, entryID uint64, level int) uint64 {
	bestID := entryID
	bestDist := g.Distance(query, g.arena.Get(g.nodes[entryID].VectorOffset))

	improved := true
	for improved {
		improved = false
		node := &g.nodes[bestID]
		for _, neighborID := range g.neighborsArena.GetNeighbors(node.NeighborsOffset, level) {
			dist := g.Distance(query, g.arena.Get(g.nodes[neighborID].VectorOffset))
			if dist < bestDist {
				bestID = neighborID
				bestDist = dist
				improved = true
			}
		}
	}
	return bestID
}

// Insert добавляет новый вектор в HNSW-граф.
//
// Возвращает внутренний индекс ноды в плоском массиве nodes.
// VectorStore использует этот индекс для маппинга ключ ↔ нода.
//
// Это главная операция. Здесь происходит:
//  1. Генерация случайного уровня для ноды
//  2. Спуск по верхним слоям (навигация к нужному региону)
//  3. Поиск и подключение соседей на каждом слое
func (g *Graph) Insert(vec []float32) uint32 {
	// 1. Кидаем «кубик» — на скольких слоях будет жить нода
	level := g.randomLevel()

	if g.arena == nil {
		g.arena = NewVectorArena(len(vec), 10000)
	}
	vecOffset := g.arena.Allocate(vec)
	neighborsOffset := g.neighborsArena.Allocate(level)

	// 2. Выделяем ячейку: из free list или append в конец
	var idx uint32
	if len(g.freeIDs) > 0 {
		idx = g.freeIDs[len(g.freeIDs)-1]
		g.freeIDs = g.freeIDs[:len(g.freeIDs)-1]
	} else {
		idx = uint32(len(g.nodes))
		g.nodes = append(g.nodes, Node{})
	}

	// 3. Записываем ноду по значению в плоский массив
	g.nodes[idx] = Node{
		ID:              uint64(idx),
		VectorOffset:    vecOffset,
		NeighborsOffset: neighborsOffset,
		Level:           level,
		Alive:           true,
	}
	g.nodeCount++
	id := uint64(idx)

	// 4. Первая нода в графе — особый случай
	//    Она автоматически становится entry point. Соседей нет.
	if g.nodeCount == 1 {
		g.entryPointID = id
		g.maxLevel = level
		return idx
	}

	ep := g.entryPointID // начинаем с текущего entry point

	// ═══════════════════════════════════════════════════
	// ФАЗА 1: Спуск по верхним слоям (выше level новой ноды)
	// ═══════════════════════════════════════════════════
	// ★ greedyClosest вместо searchLayer(ef=1) — без pool/map/heap.
	for lc := g.maxLevel; lc > level; lc-- {
		ep = g.greedyClosest(vec, ep, lc)
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 2: Поиск соседей и подключение (от level до 0)
	// ═══════════════════════════════════════════════════
	for lc := min(level, g.maxLevel); lc >= 0; lc-- {
		results := g.searchLayer(vec, ep, g.EfConstruction, lc)

		M := g.maxNeighbors(lc)
		if len(results) > M {
			results = results[:M]
		}

		// Шаг 3: Сохраняем ID выбранных соседей через арену (★ zero-alloc через insertBuf)
		neighborIDs := g.insertBuf[:len(results)]
		for i, r := range results {
			neighborIDs[i] = r.id
		}
		g.neighborsArena.SetNeighbors(g.nodes[idx].NeighborsOffset, lc, neighborIDs)

		// Шаг 4: Обратные связи через арену (★ zero-alloc через insertBuf)
		for _, r := range results {
			neighbor := &g.nodes[r.id]
			existing := g.neighborsArena.GetNeighbors(neighbor.NeighborsOffset, lc)

			// Переиспользуем insertBuf: [existing..., id]
			updated := g.insertBuf[:len(existing)+1]
			copy(updated, existing)
			updated[len(existing)] = id

			if len(updated) > M {
				g.neighborsArena.SetNeighbors(neighbor.NeighborsOffset, lc, updated[:M])
				g.pruneNeighborsFromList(neighbor, lc, M, updated)
			} else {
				g.neighborsArena.SetNeighbors(neighbor.NeighborsOffset, lc, updated)
			}
		}

		if len(results) > 0 {
			ep = results[0].id
		}
	}

	// ═══════════════════════════════════════════════════
	// Обновление entry point
	// ═══════════════════════════════════════════════════
	if level > g.maxLevel {
		g.entryPointID = id
		g.maxLevel = level
	}

	return idx
}

// pruneNeighbors обрезает список соседей до maxCount.
//
// ★ ZERO-ALLOC версия ★
// Использует переиспользуемые буферы pruneBufItems/pruneBufIDs из Graph.
// slices.SortFunc вместо sort.Slice — без рефлексии и аллокаций.
func (g *Graph) pruneNeighbors(node *Node, level int, maxCount int) {
	neighbors := g.neighborsArena.GetNeighbors(node.NeighborsOffset, level)

	// 1. Переиспользуем буфер items (расширяем при необходимости)
	if cap(g.pruneBufItems) < len(neighbors) {
		g.pruneBufItems = make([]item, len(neighbors))
	}
	items := g.pruneBufItems[:len(neighbors)]
	for i, nid := range neighbors {
		items[i] = item{
			id:   nid,
			dist: g.Distance(g.arena.Get(node.VectorOffset), g.arena.Get(g.nodes[nid].VectorOffset)),
		}
	}

	// 2. slices.SortFunc — generics, без reflect.Swapper, 0 аллокаций
	slices.SortFunc(items, func(a, b item) int {
		if a.dist < b.dist {
			return -1
		}
		if a.dist > b.dist {
			return 1
		}
		return 0
	})

	if len(items) > maxCount {
		items = items[:maxCount]
	}

	// 3. Переиспользуем буфер pruned (расширяем при необходимости)
	if cap(g.pruneBufIDs) < len(items) {
		g.pruneBufIDs = make([]uint64, len(items))
	}
	pruned := g.pruneBufIDs[:len(items)]
	for i, it := range items {
		pruned[i] = it.id
	}
	g.neighborsArena.SetNeighbors(node.NeighborsOffset, level, pruned)
}

// pruneNeighborsFromList — то же что pruneNeighbors, но принимает готовый список соседей.
//
// ★ ZERO-ALLOC версия ★
func (g *Graph) pruneNeighborsFromList(node *Node, level int, maxCount int, neighbors []uint64) {
	// Переиспользуем буфер items (расширяем при необходимости)
	if cap(g.pruneBufItems) < len(neighbors) {
		g.pruneBufItems = make([]item, len(neighbors))
	}
	items := g.pruneBufItems[:len(neighbors)]
	for i, nid := range neighbors {
		items[i] = item{
			id:   nid,
			dist: g.Distance(g.arena.Get(node.VectorOffset), g.arena.Get(g.nodes[nid].VectorOffset)),
		}
	}

	slices.SortFunc(items, func(a, b item) int {
		if a.dist < b.dist {
			return -1
		}
		if a.dist > b.dist {
			return 1
		}
		return 0
	})

	if len(items) > maxCount {
		items = items[:maxCount]
	}

	if cap(g.pruneBufIDs) < len(items) {
		g.pruneBufIDs = make([]uint64, len(items))
	}
	pruned := g.pruneBufIDs[:len(items)]
	for i, it := range items {
		pruned[i] = it.id
	}
	g.neighborsArena.SetNeighbors(node.NeighborsOffset, level, pruned)
}

// Delete удаляет ноду из HNSW-графа.
//
// Это обратная операция к Insert. Удаление из графа — нетривиальная задача,
// потому что нода может быть «мостом» между двумя частями графа.
// Если просто вырезать ноду, граф может распасться на куски,
// и часть нод станет недостижимой при поиске.
//
// Алгоритм:
//
//  1. Для каждого слоя, на котором нода присутствует:
//     a. У каждого соседа удаляем ссылку на удаляемую ноду
//     b. Переподключаем соседей друг к другу (чтобы не порвать граф)
//     c. Обрезаем лишние связи (если у соседа стало > M соседей)
//  2. Если удалённая нода была entry point — выбираем нового
//  3. Удаляем ноду из хранилища
//
// Аналогия: сносим город на карте. Все дороги, которые шли ЧЕРЕЗ этот город,
// нужно перенаправить напрямую между оставшимися городами,
// иначе часть страны окажется в изоляции.
func (g *Graph) Delete(id uint64) bool {
	if id >= uint64(len(g.nodes)) || !g.nodes[id].Alive {
		return false
	}
	node := &g.nodes[id]
	g.arena.Free(node.VectorOffset)

	// ═══════════════════════════════════════════════════
	// ФАЗА 1: Ремонт связей на каждом слое
	// ═══════════════════════════════════════════════════
	for level := 0; level <= node.Level; level++ {
		// Копируем соседей, т.к. будем модифицировать арену
		origNeighbors := g.neighborsArena.GetNeighbors(node.NeighborsOffset, level)
		neighbors := make([]uint64, len(origNeighbors))
		copy(neighbors, origNeighbors)

		M := g.maxNeighbors(level)

		for _, neighborID := range neighbors {
			if neighborID >= uint64(len(g.nodes)) || !g.nodes[neighborID].Alive {
				continue
			}
			neighbor := &g.nodes[neighborID]

			// Шаг A: Убираем удалённую ноду из списка соседей
			nNeighbors := g.neighborsArena.GetNeighbors(neighbor.NeighborsOffset, level)
			cleaned := removeID(append([]uint64{}, nNeighbors...), id)

			// Шаг B: Переподключаем — добавляем других соседей удаляемой ноды
			for _, otherID := range neighbors {
				if otherID == neighborID {
					continue
				}
				if containsID(cleaned, otherID) {
					continue
				}
				cleaned = append(cleaned, otherID)
			}

			// Шаг C: Записываем, с прунингом если нужно
			if len(cleaned) > M {
				g.pruneNeighborsFromList(neighbor, level, M, cleaned)
			} else {
				g.neighborsArena.SetNeighbors(neighbor.NeighborsOffset, level, cleaned)
			}
		}
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 2: Полная зачистка — убираем ВСЕ ссылки на удалённую ноду
	// ═══════════════════════════════════════════════════
	for i := range g.nodes {
		n := &g.nodes[i]
		if !n.Alive || n.ID == id {
			continue
		}
		for level := 0; level <= n.Level; level++ {
			nNeighbors := g.neighborsArena.GetNeighbors(n.NeighborsOffset, level)
			cleaned := removeID(append([]uint64{}, nNeighbors...), id)
			if len(cleaned) != len(nNeighbors) {
				g.neighborsArena.SetNeighbors(n.NeighborsOffset, level, cleaned)
			}
		}
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 3: Tombstone + free list
	// ═══════════════════════════════════════════════════
	g.neighborsArena.Free(node.NeighborsOffset, node.Level)
	node.Alive = false
	g.freeIDs = append(g.freeIDs, uint32(id))
	g.nodeCount--

	// ═══════════════════════════════════════════════════
	// ФАЗА 4: Обновляем entry point (если удалили именно его)
	// ═══════════════════════════════════════════════════
	if id == g.entryPointID {
		if g.nodeCount == 0 {
			g.entryPointID = 0
			g.maxLevel = 0
		} else {
			newMaxLevel := -1
			var newEP uint64
			for i := range g.nodes {
				n := &g.nodes[i]
				if !n.Alive {
					continue
				}
				if n.Level > newMaxLevel {
					newMaxLevel = n.Level
					newEP = uint64(i)
				}
			}
			g.entryPointID = newEP
			g.maxLevel = newMaxLevel
		}
	}

	return true
}

// removeID удаляет первое вхождение val из слайса.
// Не делает аллокаций — сдвигает элементы на месте.
func removeID(s []uint64, val uint64) []uint64 {
	for i, v := range s {
		if v == val {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

// containsID проверяет, есть ли val в слайсе.
func containsID(s []uint64, val uint64) bool {
	for _, v := range s {
		if v == val {
			return true
		}
	}
	return false
}

// SearchResult — один результат поиска.
//
// Это экспортируемая структура (с большой буквы) — её увидит вызывающий код.
// Внутренний тип item (с маленькой) остаётся деталью реализации.
type SearchResult struct {
	ID       uint64
	Distance float32
}

// Search находит K ближайших соседей к запросу.
//
// Параметры:
//
//	query    — вектор, к которому ищем ближайших
//	K        — сколько результатов вернуть
//	efSearch — ширина поиска. Больше = точнее, но медленнее.
//	           Должен быть >= K. Типичные значения: 50–200.
//
// Возвращает: до K ближайших, отсортированных по расстоянию.
//
// Аналогия:
//
//	efSearch = "скольких людей спросить на улице"
//	K        = "скольких показать в ответе"
//	Можно спросить 100 (efSearch=100), но показать только 10 (K=10).
//	Чем больше спросишь — тем вероятнее найдёшь лучших.
func (g *Graph) Search(query []float32, K int, efSearch int) []SearchResult {
	// Пустой граф — пустой результат
	if g.nodeCount == 0 {
		return nil
	}

	// efSearch не может быть меньше K
	// (иначе мы найдём меньше кандидатов, чем нужно вернуть)
	if efSearch < K {
		efSearch = K
	}

	ep := g.entryPointID

	// ═══════════════════════════════════════════════════
	// ФАЗА 1: Спуск по верхним слоям (слой maxLevel → слой 1)
	// ═══════════════════════════════════════════════════
	//
	// ★ greedyClosest вместо searchLayer(ef=1) — без pool/map/heap.
	// Цель: найти хорошую «стартовую позицию» для слоя 0.
	for lc := g.maxLevel; lc > 0; lc-- {
		ep = g.greedyClosest(query, ep, lc)
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 2: Полный поиск на слое 0
	// ═══════════════════════════════════════════════════
	//
	// Слой 0 содержит ВСЕ ноды. Здесь ищем efSearch ближайших.
	results := g.searchLayer(query, ep, efSearch, 0)

	// Обрезаем до K
	if len(results) > K {
		results = results[:K]
	}

	// Конвертируем внутренний item → экспортируемый SearchResult
	out := make([]SearchResult, len(results))
	for i, r := range results {
		out[i] = SearchResult{
			ID:       r.id,
			Distance: r.dist,
		}
	}

	return out
}

// Len возвращает количество нод в графе.
func (g *Graph) Len() int {
	return g.nodeCount
}

// MaxLevel возвращает текущий максимальный уровень графа.
func (g *Graph) MaxLevel() int {
	return g.maxLevel
}
