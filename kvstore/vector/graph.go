package vector

import (
	"math"
	"math/rand"
	"slices"
	"sync"
	"unsafe"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// Node — одна точка в графе HNSW.
type Node struct {
	ID              uint64
	VectorOffset    uint64
	NeighborsHandle tcmalloc.Handle // Дескриптор блока связей в TCMalloc
	Level           int             // максимальный слой, на котором нода присутствует
	Alive           bool            // маркер «живая ли нода» (tombstone при Delete)
}

// Graph — HNSW-граф.
type Graph struct {
	// Хранилище всех нод — плоский массив (арена).
	// Инндекс в слайсе = внутренний ID ноды. Прямой доступ O(1) без хэширования.
	nodes     []Node   // плоский массив, индекс = ID ноды
	nodeCount int      // количество живых нод (без дыр/tombstone)
	freeIDs   []uint32 // стек свободных индексов (от Delete)

	// Точка входа — нода на самом верхнем уровне.
	entryPointID uint64

	// Текущий максимальный слой в графе.
	maxLevel int

	// ─── Параметры алгоритма ───
	M  int
	M0 int // = 2*M, максимум соседей на слое 0

	Ml float64

	// EfConstruction — ширина поиска при ВСТАВКЕ.
	EfConstruction int

	// Функция расстояния (Euclidean или Cosine).
	Distance DistanceFunc

	arena     *VectorArena
	allocator *tcmalloc.TCMallocStore // Менеджер памяти для neighbors блоков

	// ─── Переиспользуемые буферы (zero-alloc pruning/insert) ───
	pruneBufItems []item   // буфер для pruneNeighbors (capacity = M0)
	pruneBufIDs   []uint64 // буфер для ID после pruning (capacity = M0)
	insertBuf     []uint64 // буфер для обратных связей в Insert (capacity = M0+1)
}

// NewGraph создаёт пустой HNSW-граф с параметрами по умолчанию.
func NewGraph(distance DistanceFunc, allocator *tcmalloc.TCMallocStore) *Graph {
	m := 16
	m0 := 2 * m
	return &Graph{
		nodes:          make([]Node, 0, 10000),
		freeIDs:        make([]uint32, 0, 64),
		M:              m,
		M0:             m0, // слой 0 в 2 раза плотнее (из оригинальной статьи)
		Ml:             1.0 / math.Log(float64(m)),
		EfConstruction: 200,
		Distance:       distance,
		allocator:      allocator,
		pruneBufItems:  make([]item, 0, m0+1),
		pruneBufIDs:    make([]uint64, 0, m0+1),
		insertBuf:      make([]uint64, 0, m0+1),
	}
}

// bytesToUint64 делает zero-copy кастинг слайса байт в слайс uint64
func bytesToUint64(b []byte) []uint64 {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&b[0])), len(b)/8)
}

func (g *Graph) neighborsBlockSize(level int) int {
	if level == 0 {
		return 1 + g.M0
	}
	return 1 + g.M0 + level*(1+g.M)
}

func (g *Graph) offsetForLevel(targetLevel int) int {
	if targetLevel == 0 {
		return 0
	}
	return 1 + g.M0 + (targetLevel-1)*(1+g.M)
}

func (g *Graph) getNeighbors(handle tcmalloc.Handle, targetLevel int) []uint64 {
	if uint64(handle) == 0 {
		return nil
	}
	byteBuf := g.allocator.Resolve(handle)
	uint64Buf := bytesToUint64(byteBuf)

	offset := g.offsetForLevel(targetLevel)
	length := int(uint64Buf[offset])
	if length == 0 {
		return nil
	}
	return uint64Buf[offset+1 : offset+1+length]
}

func (g *Graph) setNeighbors(handle tcmalloc.Handle, targetLevel int, neighbors []uint64) {
	if uint64(handle) == 0 {
		return
	}
	byteBuf := g.allocator.Resolve(handle)
	uint64Buf := bytesToUint64(byteBuf)

	offset := g.offsetForLevel(targetLevel)
	uint64Buf[offset] = uint64(len(neighbors))
	if len(neighbors) > 0 {
		copy(uint64Buf[offset+1:], neighbors)
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
// Использует переданный из пула state для безопасной параллельной работы.
func (g *Graph) searchLayer(state *searchState, query []float32, entryID uint64, ef int, level int) []item {

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

		// НОВЫЙ КОД (batch):
		node := &g.nodes[closest.id]
		neighbors := g.getNeighbors(node.NeighborsHandle, level)

		// Фаза 1: собрать offset-ы НЕПОСЕЩЁННЫХ соседей
		state.batchOffsets = state.batchOffsets[:0]
		state.batchIDs = state.batchIDs[:0]

		for _, neighborID := range neighbors {
			if state.isVisited(neighborID) {
				continue
			}
			state.setVisited(neighborID)

			neighborNode := &g.nodes[neighborID]
			state.batchOffsets = append(state.batchOffsets, neighborNode.VectorOffset)
			state.batchIDs = append(state.batchIDs, neighborID)
		}

		// Фаза 2: batch distance — один «вызов» на все offset-ы
		if len(state.batchOffsets) > 0 {
			// Убедиться что буфер результатов достаточного размера
			if cap(state.batchDists) < len(state.batchOffsets) {
				state.batchDists = make([]float32, len(state.batchOffsets))
			}
			state.batchDists = state.batchDists[:len(state.batchOffsets)]

			g.batchDistance(query, state.batchOffsets, state.batchDists)

			// Фаза 3: разобрать результаты
			farthestResult = state.results.peek()
			for i, neighborID := range state.batchIDs {
				neighborDist := state.batchDists[i]

				if neighborDist < farthestResult.dist || state.results.Len() < ef {
					newItem := item{id: neighborID, dist: neighborDist}
					state.candidates.push(newItem)
					state.results.push(newItem)

					if state.results.Len() > ef {
						state.results.pop()
					}
					farthestResult = state.results.peek()
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

	return state.collected
}

// searchLayerFiltered — searchLayer с фильтрацией результатов.
//
// Отличие от searchLayer: нода, не прошедшая filterFn, добавляется
// в candidates (чтобы продолжить обход графа), но НЕ в results.
//
// Это критически важно при высокой селективности фильтра:
// если 99% нод не проходят фильтр, без добавления в candidates
// поиск "застрянет" — не сможет найти путь через граф к подходящим нодам.
//
// filterFn принимает внутренний ID ноды и возвращает true, если нода
// проходит фильтр. filterFn НЕ должен быть nil — для поиска без фильтра
// используйте searchLayer.
func (g *Graph) searchLayerFiltered(state *searchState, query []float32, entryID uint64, ef int, level int, filterFn func(uint64) bool) []item {

	entryNode := &g.nodes[entryID]
	entryDist := g.Distance(query, g.arena.Get(entryNode.VectorOffset))

	state.setVisited(entryID)

	entry := item{id: entryID, dist: entryDist}
	state.candidates.push(entry)
	// Точка входа добавляется в results только если проходит фильтр.
	if filterFn(entryID) {
		state.results.push(entry)
	}

	for state.candidates.Len() > 0 {
		closest := state.candidates.pop()

		// Условие остановки: если results непуст и closest дальше самого далёкого результата.
		if state.results.Len() > 0 {
			farthestResult := state.results.peek()
			if closest.dist > farthestResult.dist && state.results.Len() >= ef {
				break
			}
		}

		node := &g.nodes[closest.id]
		neighbors := g.getNeighbors(node.NeighborsHandle, level)

		// Фаза 1: собрать offset-ы НЕПОСЕЩЁННЫХ соседей
		state.batchOffsets = state.batchOffsets[:0]
		state.batchIDs = state.batchIDs[:0]

		for _, neighborID := range neighbors {
			if state.isVisited(neighborID) {
				continue
			}
			state.setVisited(neighborID)

			neighborNode := &g.nodes[neighborID]
			state.batchOffsets = append(state.batchOffsets, neighborNode.VectorOffset)
			state.batchIDs = append(state.batchIDs, neighborID)
		}

		// Фаза 2: batch distance
		if len(state.batchOffsets) > 0 {
			if cap(state.batchDists) < len(state.batchOffsets) {
				state.batchDists = make([]float32, len(state.batchOffsets))
			}
			state.batchDists = state.batchDists[:len(state.batchOffsets)]

			g.batchDistance(query, state.batchOffsets, state.batchDists)

			// Фаза 3: разобрать результаты с учётом фильтра
			for i, neighborID := range state.batchIDs {
				neighborDist := state.batchDists[i]

				// В candidates добавляем ВСЕГДА — для продолжения обхода графа.
				newItem := item{id: neighborID, dist: neighborDist}

				farthestDist := float32(math.MaxFloat32)
				if state.results.Len() > 0 {
					farthestDist = state.results.peek().dist
				}

				if neighborDist < farthestDist || state.results.Len() < ef {
					state.candidates.push(newItem)

					// В results — только если проходит фильтр.
					if filterFn(neighborID) {
						state.results.push(newItem)

						if state.results.Len() > ef {
							state.results.pop()
						}
					}
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

	return state.collected
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
		for _, neighborID := range g.getNeighbors(node.NeighborsHandle, level) {
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
	buf, handle := g.allocator.Alloc(0, g.neighborsBlockSize(level)*8)
	for i := range buf {
		buf[i] = 0
	}

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
		NeighborsHandle: handle,
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
	state := searchPool.Get().(*searchState)
	defer searchPool.Put(state)

	for lc := min(level, g.maxLevel); lc >= 0; lc-- {
		state.acquire(len(g.nodes))
		results := g.searchLayer(state, vec, ep, g.EfConstruction, lc)
		

		M := g.maxNeighbors(lc)
		if len(results) > M {
			results = results[:M]
		}

		// Шаг 3: Сохраняем ID выбранных соседей через арену (★ zero-alloc через insertBuf)
		neighborIDs := g.insertBuf[:len(results)]
		for i, r := range results {
			neighborIDs[i] = r.id
		}
		g.setNeighbors(g.nodes[idx].NeighborsHandle, lc, neighborIDs)

		// Шаг 4: Обратные связи через арену (★ zero-alloc через insertBuf)
		for _, r := range results {
			neighbor := &g.nodes[r.id]
			existing := g.getNeighbors(neighbor.NeighborsHandle, lc)

			// Переиспользуем insertBuf: [existing..., id]
			updated := g.insertBuf[:len(existing)+1]
			copy(updated, existing)
			updated[len(existing)] = id

			if len(updated) > M {
				g.setNeighbors(neighbor.NeighborsHandle, lc, updated[:M])
				g.pruneNeighborsFromList(neighbor, lc, M, updated)
			} else {
				g.setNeighbors(neighbor.NeighborsHandle, lc, updated)
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
	neighbors := g.getNeighbors(node.NeighborsHandle, level)

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
	g.setNeighbors(node.NeighborsHandle, level, pruned)
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
	g.setNeighbors(node.NeighborsHandle, level, pruned)
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
		origNeighbors := g.getNeighbors(node.NeighborsHandle, level)
		neighbors := make([]uint64, len(origNeighbors))
		copy(neighbors, origNeighbors)

		M := g.maxNeighbors(level)

		for _, neighborID := range neighbors {
			if neighborID >= uint64(len(g.nodes)) || !g.nodes[neighborID].Alive {
				continue
			}
			neighbor := &g.nodes[neighborID]

			// Шаг A: Убираем удалённую ноду из списка соседей
			nNeighbors := g.getNeighbors(neighbor.NeighborsHandle, level)
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
				g.setNeighbors(neighbor.NeighborsHandle, level, cleaned)
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
			nNeighbors := g.getNeighbors(n.NeighborsHandle, level)
			cleaned := removeID(append([]uint64{}, nNeighbors...), id)
			if len(cleaned) != len(nNeighbors) {
				g.setNeighbors(n.NeighborsHandle, level, cleaned)
			}
		}
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 3: Tombstone + free list
	// ═══════════════════════════════════════════════════
	g.allocator.Free(0, node.NeighborsHandle)
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

	state := searchPool.Get().(*searchState)
	state.acquire(len(g.nodes))
	defer searchPool.Put(state)

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
	results := g.searchLayer(state, query, ep, efSearch, 0)

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

// SearchFiltered — поиск ближайших K векторов с фильтрацией.
//
// filterFn принимает внутренний ID ноды и возвращает true, если нода
// должна попасть в результат. Используется для Single-Stage Filtering:
// фильтрация происходит ВНУТРИ обхода графа, а не post-hoc.
//
// Пример: найти ближайшие 10 товаров из категории "электроника".
func (g *Graph) SearchFiltered(query []float32, K int, efSearch int, filterFn func(uint64) bool) []SearchResult {
	if g.nodeCount == 0 {
		return nil
	}

	// При фильтрации увеличиваем efSearch — часть кандидатов будет отфильтрована.
	// Множитель 3× даёт хороший баланс recall/performance для 10-50% selectivity.
	filteredEf := efSearch * 3
	if filteredEf < K*10 {
		filteredEf = K * 10
	}

	state := searchPool.Get().(*searchState)
	state.acquire(len(g.nodes))
	defer searchPool.Put(state)

	ep := g.entryPointID

	// ФАЗА 1: Спуск по верхним слоям (без фильтра — навигация)
	for lc := g.maxLevel; lc > 0; lc-- {
		ep = g.greedyClosest(query, ep, lc)
	}

	// ФАЗА 2: Фильтрованный поиск на слое 0
	results := g.searchLayerFiltered(state, query, ep, filteredEf, 0, filterFn)

	// Обрезаем до K
	if len(results) > K {
		results = results[:K]
	}

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

// batchDistance считает расстояние от query до каждого вектора по offset-ам.
//
// Зачем batch, а не по одному?
// Cache locality: offsets собраны подряд в буфере searchState,
// CPU prefetcher эффективнее загружает данные.
//
// query    — поисковый вектор
// offsets  — массив VectorOffset-ов из Node-ов
// results  — куда записать расстояния (len >= len(offsets))
//
// Все массивы — переиспользуемые буферы из searchState, 0 аллокаций.
func (g *Graph) batchDistance(query []float32, offsets []uint64, results []float32) {
	for i, off := range offsets {
		vec := g.arena.Get(off)
		results[i] = g.Distance(query, vec)
	}
}

type searchState struct {
	visited      []uint64
	candidates   minHeap
	results      maxHeap
	collected    []item
	batchOffsets []uint64
	batchIDs     []uint64
	batchDists   []float32
}

func (s *searchState) isVisited(id uint64) bool {
	return s.visited[id/64]&(1<<(id%64)) != 0
}

func (s *searchState) setVisited(id uint64) {
	s.visited[id/64] |= 1 << (id % 64)
}

func (s *searchState) acquire(nodeSlots int) {
	needed := (nodeSlots + 63) / 64
	if cap(s.visited) < needed {
		s.visited = make([]uint64, needed)
	} else {
		s.visited = s.visited[:needed]
		for i := range s.visited {
			s.visited[i] = 0
		}
	}
	s.candidates = s.candidates[:0]
	s.results = s.results[:0]
	s.collected = s.collected[:0]
}

var searchPool = sync.Pool{
	New: func() any {
		return &searchState{
			visited:      make([]uint64, 0, 128),
			candidates:   make(minHeap, 0, 128),
			results:      make(maxHeap, 0, 128),
			collected:    make([]item, 0, 128),
			batchOffsets: make([]uint64, 0, 32),
			batchIDs:     make([]uint64, 0, 32),
			batchDists:   make([]float32, 0, 32),
		}
	},
}

