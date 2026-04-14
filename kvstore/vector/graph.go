package vector

import (
	"container/heap"
	"math"
	"math/rand"
	"sort"
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
type Node struct {
	ID        uint64
	Vector    []float32
	Level     int        // максимальный слой, на котором нода присутствует
	Neighbors [][]uint64 // Neighbors[i] = ID соседей на слое i
}

// Graph — HNSW-граф.
//
// Это «карта городов» с несколькими слоями масштаба.
type Graph struct {
	mu sync.RWMutex

	// Хранилище всех нод. map[id] → нода.
	// Позже мы это заменим на арену — но сначала нужно понять алгоритм.
	nodes map[uint64]*Node

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
}

// NewGraph создаёт пустой HNSW-граф с параметрами по умолчанию.
//
// Параметры по умолчанию подобраны из оригинальной статьи HNSW (2016)
// и практики Pinecone/Weaviate/Milvus:
//
//	M=16, efConstruction=200 — хороший баланс скорость/качество.
func NewGraph(distance DistanceFunc) *Graph {
	m := 16
	return &Graph{
		nodes:          make(map[uint64]*Node),
		M:              m,
		M0:             2 * m, // слой 0 в 2 раза плотнее (из оригинальной статьи)
		Ml:             1.0 / math.Log(float64(m)),
		EfConstruction: 200,
		Distance:       distance,
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
// Это жадный алгоритм: начинаем с entryID, смотрим его соседей,
// если сосед ближе — идём к нему, смотрим ЕГО соседей, и так далее.
//
// Параметры:
//
//	query   — вектор, к которому ищем ближайших
//	entryID — откуда начинаем поиск на этом слое
//	ef      — сколько ближайших хотим найти (ширина поиска)
//	level   — на каком слое ищем
//
// Возвращает: до ef ближайших нод, отсортированных по расстоянию (ближайшая первая).
func (g *Graph) searchLayer(query []float32, entryID uint64, ef int, level int) []item {

	entryNode := g.nodes[entryID]
	entryDist := g.Distance(query, entryNode.Vector)

	// visited — множество уже проверенных нод.
	// Без этого мы бы ходили по кругу: A→B→C→A→B→...
	visited := make(map[uint64]bool)
	visited[entryID] = true

	// candidates — «очередь на проверку». minHeap: ближайший первый.
	// Мы берём из неё самого близкого кандидата и проверяем ЕГО соседей.
	candidates := &minHeap{{id: entryID, dist: entryDist}}
	heap.Init(candidates)

	// results — «лучшие найденные». maxHeap: самый далёкий первый.
	// Почему maxHeap? Потому что когда results переполняется (>ef),
	// мы выкидываем САМЫЙ ДАЛЁКИЙ результат. maxHeap делает это за O(log n).
	results := &maxHeap{{id: entryID, dist: entryDist}}
	heap.Init(results)

	for candidates.Len() > 0 {
		// 1. Берём ближайшего непроверенного кандидата
		closest := heap.Pop(candidates).(item)

		// 2. Смотрим на наш ХУДШИЙ результат (самый далёкий из найденных)
		farthestResult := (*results)[0] // peek — смотрим, но не удаляем

		// 3. Условие остановки:
		//    Если ближайший кандидат ДАЛЬШЕ, чем наш худший результат —
		//    значит проверять дальше бесполезно. Все оставшиеся кандидаты
		//    ещё дальше (это же minHeap — ближайший уже был первым).
		if closest.dist > farthestResult.dist {
			break
		}

		// 4. Проверяем ВСЕХ соседей этого кандидата на данном слое
		node := g.nodes[closest.id]
		for _, neighborID := range node.Neighbors[level] {
			// Пропускаем уже посещённых
			if visited[neighborID] {
				continue
			}
			visited[neighborID] = true

			// Считаем расстояние от запроса до этого соседа
			neighborNode := g.nodes[neighborID]
			neighborDist := g.Distance(query, neighborNode.Vector)

			// Снова смотрим на худший результат
			farthestResult = (*results)[0]

			// Добавляем соседа, если:
			//   а) он ближе, чем наш худший результат, ИЛИ
			//   б) у нас ещё нет ef результатов (places available)
			if neighborDist < farthestResult.dist || results.Len() < ef {
				heap.Push(candidates, item{id: neighborID, dist: neighborDist})
				heap.Push(results, item{id: neighborID, dist: neighborDist})

				// Если результатов больше ef — выкидываем самый далёкий
				if results.Len() > ef {
					heap.Pop(results) // maxHeap → удаляется самый далёкий
				}
			}
		}
	}

	// Извлекаем результаты и сортируем: ближайший первый.
	// results — maxHeap, поэтому Pop() возвращает от дальнего к ближнему.
	// Мы вытаскиваем всё, потом разворачиваем.
	result := make([]item, 0, results.Len())
	for results.Len() > 0 {
		result = append(result, heap.Pop(results).(item))
	}

	// Переворачиваем: было [далёкий, ..., ближний] → станет [ближний, ..., далёкий]
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return result
}

// Insert добавляет новый вектор в HNSW-граф.
//
// Это главная операция. Здесь происходит:
//  1. Генерация случайного уровня для ноды
//  2. Спуск по верхним слоям (навигация к нужному региону)
//  3. Поиск и подключение соседей на каждом слое
func (g *Graph) Insert(id uint64, vec []float32) {
	// 1. Кидаем «кубик» — на скольких слоях будет жить нода
	level := g.randomLevel()

	// 2. Создаём ноду
	newNode := &Node{
		ID:        id,
		Vector:    vec,
		Level:     level,
		Neighbors: make([][]uint64, level+1), // слои 0..level
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// 3. Добавляем ноду в хранилище
	g.nodes[id] = newNode

	// 4. Первая нода в графе — особый случай
	//    Она автоматически становится entry point. Соседей нет.
	if len(g.nodes) == 1 {
		g.entryPointID = id
		g.maxLevel = level
		return
	}

	ep := g.entryPointID // начинаем с текущего entry point

	// ═══════════════════════════════════════════════════
	// ФАЗА 1: Спуск по верхним слоям (выше level новой ноды)
	// ═══════════════════════════════════════════════════
	//
	// На этих слоях новая нода НЕ будет присутствовать.
	// Мы просто навигируемся ближе к месту вставки.
	// ef=1 — нам нужен только 1 ближайший (чтобы знать, откуда продолжить).
	//
	// Аналогия: летим на самолёте. Не высаживаемся — просто выбираем
	//           ближайший аэропорт для пересадки.
	for lc := g.maxLevel; lc > level; lc-- {
		results := g.searchLayer(vec, ep, 1, lc)
		if len(results) > 0 {
			ep = results[0].id // спускаемся: entry point для следующего слоя
		}
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 2: Поиск соседей и подключение (от level до 0)
	// ═══════════════════════════════════════════════════
	//
	// На этих слоях новая нода БУДЕТ жить.
	// Для каждого слоя:
	//   1. Ищем efConstruction ближайших
	//   2. Выбираем M лучших как соседей
	//   3. Строим двусторонние связи
	for lc := min(level, g.maxLevel); lc >= 0; lc-- {
		// Шаг 1: Ищем efConstruction ближайших нод на этом слое
		results := g.searchLayer(vec, ep, g.EfConstruction, lc)

		// Шаг 2: Выбираем M лучших (results уже отсортированы — ближайший первый)
		M := g.maxNeighbors(lc)
		if len(results) > M {
			results = results[:M]
		}

		// Шаг 3: Сохраняем ID выбранных соседей
		neighborIDs := make([]uint64, len(results))
		for i, r := range results {
			neighborIDs[i] = r.id
		}
		newNode.Neighbors[lc] = neighborIDs

		// Шаг 4: Обратные связи — соседи тоже должны знать о новой ноде.
		//
		// Граф ДВУСТОРОННИЙ: если A — сосед B, то B — сосед A.
		// Это важно для поиска: иначе мы бы не могли «прийти» к новой ноде.
		for _, r := range results {
			neighbor := g.nodes[r.id]
			neighbor.Neighbors[lc] = append(neighbor.Neighbors[lc], id)

			// Если у соседа стало слишком много связей — обрезаем.
			// Оставляем только M самых близких к нему.
			if len(neighbor.Neighbors[lc]) > M {
				g.pruneNeighbors(neighbor, lc, M)
			}
		}

		// entry point для следующего (нижнего) слоя =
		// ближайшая нода, найденная на текущем слое
		if len(results) > 0 {
			ep = results[0].id
		}
	}

	// ═══════════════════════════════════════════════════
	// Обновление entry point
	// ═══════════════════════════════════════════════════
	//
	// Если новая нода «выбросила» уровень выше текущего максимума —
	// она становится новым entry point.
	// Логично: мы всегда начинаем поиск с самого верхнего слоя.
	if level > g.maxLevel {
		g.entryPointID = id
		g.maxLevel = level
	}
}

// pruneNeighbors обрезает список соседей до maxCount.
//
// Когда у ноды слишком много соседей (больше M), нужно оставить
// только самых близких. Как в реальности: у города не бывает 100 автобанов,
// содержать дорого — оставляем только дороги к ближайшим городам.
func (g *Graph) pruneNeighbors(node *Node, level int, maxCount int) {
	// 1. Для каждого соседа считаем расстояние от НОДЫ (не от запроса!)
	neighbors := node.Neighbors[level]
	items := make([]item, len(neighbors))

	for i, nid := range neighbors {
		items[i] = item{
			id:   nid,
			dist: g.Distance(node.Vector, g.nodes[nid].Vector),
		}
	}

	// 2. Сортируем по расстоянию: ближайшие первые
	sort.Slice(items, func(i, j int) bool {
		return items[i].dist < items[j].dist
	})

	// 3. Обрезаем: оставляем только maxCount ближайших
	if len(items) > maxCount {
		items = items[:maxCount]
	}

	// 4. Записываем обратно
	pruned := make([]uint64, len(items))
	for i, it := range items {
		pruned[i] = it.id
	}
	node.Neighbors[level] = pruned
}
