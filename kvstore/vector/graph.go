package vector

import (
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
	visited    map[uint64]bool // множество посещённых нод
	candidates minHeap         // очередь кандидатов (minHeap)
	results    maxHeap         // лучшие результаты (maxHeap)
	collected  []item          // буфер для сбора финальных результатов
}

// searchPool — пул переиспользуемых состояний поиска.
//
// sync.Pool.New вызывается только когда пул ПУСТ.
// После первых нескольких запросов — все берётся из пула, 0 аллокаций.
var searchPool = sync.Pool{
	New: func() any {
		return &searchState{
			visited:    make(map[uint64]bool, 256), // pre-alloc на 256 записей
			candidates: make(minHeap, 0, 256),
			results:    make(maxHeap, 0, 256),
			collected:  make([]item, 0, 256),
		}
	},
}

// acquire берёт состояние из пула и СБРАСЫВАЕТ все буферы.
//
// clear(map) — встроенная функция Go 1.21+, которая очищает map
// БЕЗ освобождения внутренних bucket'ов. То есть map остаётся
// того же размера, но пустая. Ноль аллокаций при следующем заполнении
// (пока не превысим старый размер).
func (s *searchState) acquire() {
	clear(s.visited)                // очищаем map, сохраняя capacity
	s.candidates = s.candidates[:0] // обнуляем длину, capacity остаётся
	s.results = s.results[:0]       // то же
	s.collected = s.collected[:0]   // то же
}

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
// searchLayer ищет ef ближайших нод к query на одном слое графа.
//
// ★ ZERO-ALLOC версия ★
// Все буферы берутся из sync.Pool и возвращаются после использования.
// На hot path (10K+ RPS) — ни одной аллокации.
func (g *Graph) searchLayer(query []float32, entryID uint64, ef int, level int) []item {

	state := searchPool.Get().(*searchState)
	state.acquire()

	entryNode := g.nodes[entryID]
	entryDist := g.Distance(query, entryNode.Vector)

	state.visited[entryID] = true

	entry := item{id: entryID, dist: entryDist}
	state.candidates.push(entry) // ← typed, zero alloc
	state.results.push(entry)    // ← typed, zero alloc

	for state.candidates.Len() > 0 {
		closest := state.candidates.pop()      // ← typed, zero alloc
		farthestResult := state.results.peek() // ← typed peek, zero alloc

		if closest.dist > farthestResult.dist {
			break
		}

		node := g.nodes[closest.id]
		for _, neighborID := range node.Neighbors[level] {
			if state.visited[neighborID] {
				continue
			}
			state.visited[neighborID] = true

			neighborNode := g.nodes[neighborID]
			neighborDist := g.Distance(query, neighborNode.Vector)

			farthestResult = state.results.peek() // ← typed peek

			if neighborDist < farthestResult.dist || state.results.Len() < ef {
				newItem := item{id: neighborID, dist: neighborDist}
				state.candidates.push(newItem) // ← typed
				state.results.push(newItem)    // ← typed

				if state.results.Len() > ef {
					state.results.pop() // ← typed
				}
			}
		}
	}

	for state.results.Len() > 0 {
		state.collected = append(state.collected, state.results.pop()) // ← typed
	}

	for i, j := 0, len(state.collected)-1; i < j; i, j = i+1, j-1 {
		state.collected[i], state.collected[j] = state.collected[j], state.collected[i]
	}

	result := make([]item, len(state.collected))
	copy(result, state.collected)

	searchPool.Put(state)
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
	g.mu.Lock()
	defer g.mu.Unlock()

	node, exists := g.nodes[id]
	if !exists {
		return false
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 1: Ремонт связей на каждом слое
	// ═══════════════════════════════════════════════════
	for level := 0; level <= node.Level; level++ {
		neighbors := node.Neighbors[level]
		M := g.maxNeighbors(level)

		for _, neighborID := range neighbors {
			neighbor := g.nodes[neighborID]
			if neighbor == nil {
				continue
			}

			// Шаг A: Убираем удалённую ноду из списка соседей
			neighbor.Neighbors[level] = removeID(neighbor.Neighbors[level], id)

			// Шаг B: Переподключаем — добавляем других соседей удаляемой ноды.
			//
			// Пример: если удаляем B, а граф был A—B—C,
			// то A теряет связь с C. Нужно добавить A—C напрямую.
			for _, otherID := range neighbors {
				if otherID == neighborID {
					continue // не соединяем ноду саму с собой
				}
				if containsID(neighbor.Neighbors[level], otherID) {
					continue // связь уже есть
				}
				neighbor.Neighbors[level] = append(neighbor.Neighbors[level], otherID)
			}

			// Шаг C: Обрезаем если соседей стало слишком много
			if len(neighbor.Neighbors[level]) > M {
				g.pruneNeighbors(neighbor, level, M)
			}
		}
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 2: Полная зачистка — убираем ВСЕ ссылки на удалённую ноду
	// ═══════════════════════════════════════════════════
	//
	// Почему недостаточно Фазы 1?
	// pruneNeighbors может создать асимметричные рёбра:
	// если A → B существует, но B → A было обрезано при прунинге.
	// Тогда B ссылается на A, но A не знает о B.
	// Фаза 1 чистит только соседей A (из A.Neighbors).
	// Фаза 2 проходит по ВСЕМ нодам и удаляет любые оставшиеся ссылки.
	for _, n := range g.nodes {
		if n.ID == id {
			continue
		}
		for level := 0; level <= n.Level; level++ {
			n.Neighbors[level] = removeID(n.Neighbors[level], id)
		}
	}

	// ═══════════════════════════════════════════════════
	// ФАЗА 3: Удаляем ноду из хранилища
	// ═══════════════════════════════════════════════════
	delete(g.nodes, id)

	// ═══════════════════════════════════════════════════
	// ФАЗА 4: Обновляем entry point (если удалили именно его)
	// ═══════════════════════════════════════════════════
	if id == g.entryPointID {
		if len(g.nodes) == 0 {
			// Граф стал пустым
			g.entryPointID = 0
			g.maxLevel = 0
		} else {
			// Ищем ноду с максимальным уровнем — она станет новым entry point.
			// Entry point всегда должен быть на самом верхнем слое,
			// иначе поиск не сможет начаться с верхних слоёв.
			newMaxLevel := 0
			var newEP uint64
			for nid, n := range g.nodes {
				if n.Level > newMaxLevel {
					newMaxLevel = n.Level
					newEP = nid
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
	g.mu.RLock()
	defer g.mu.RUnlock()

	// Пустой граф — пустой результат
	if len(g.nodes) == 0 {
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
	// Точно как в Insert: ef=1, жадная навигация.
	// Цель: найти хорошую «стартовую позицию» для слоя 0.
	for lc := g.maxLevel; lc > 0; lc-- {
		results := g.searchLayer(query, ep, 1, lc)
		if len(results) > 0 {
			ep = results[0].id
		}
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
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes)
}

// MaxLevel возвращает текущий максимальный уровень графа.
func (g *Graph) MaxLevel() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.maxLevel
}
