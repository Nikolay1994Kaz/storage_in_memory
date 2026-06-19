package vector

import (
	"slices"
	"sync"
)

// =============================================================================
// FrozenGraph — CSR (Compressed Sparse Row) frozen HNSW graph.
//
// Иммутабельная, cache-friendly структура для поиска.
// Строится из *Graph через FreezeGraph() после compaction.
//
// Используется ТОЛЬКО для dim ≤ 256 (где cache locality даёт +14% QPS).
// Для dim ≥ 512 остаётся обычный *Graph (hnswSegment).
//
// Memory layout:
//   data[i*dim : (i+1)*dim]  = вектор ноды i  (contiguous float32 slab)
//   layers[lyr].neigh[layers[lyr].offs[i] : layers[lyr].offs[i+1]] = соседи ноды i на layer lyr (flat CSR)
// =============================================================================

// frozenSearchState — pool-able буфер для Search (как searchPool в graph.go).
type frozenSearchState struct {
	visited []uint64        // bitset, размер (n+63)/64
	cands   []frozenCandItem // minHeap кандидатов
	res     []FrozenResult  // maxHeap результатов
}

type frozenCandItem struct {
	id   uint32
	dist float32
}

// frozenSearchPool — глобальный pool, 0 аллокаций в горячем пути.
var frozenSearchPool = sync.Pool{
	New: func() any {
		return &frozenSearchState{
			visited: make([]uint64, 0, 512),
			cands:   make([]frozenCandItem, 0, 128),
			res:     make([]FrozenResult, 0, 128),
		}
	},
}

// FrozenResult — результат поиска из FrozenGraph.
type FrozenResult struct {
	Key  string
	Dist float32
}

// FlatLayer — плоское CSR представление связей одного слоя.
type FlatLayer struct {
	neigh []uint32
	offs  []uint32 // len = n+1
}

// FrozenGraph — иммутабельный CSR граф.
type FrozenGraph struct {
	// Векторы — flat slab: data[i*dim : (i+1)*dim]
	data []float32

	// CSR для каждого слоя от 0 до maxLevel
	// layers[0] - слой 0, layers[lyr] - слой lyr
	layers []FlatLayer

	// Ключи (userKey) для каждого внутреннего ID
	keys []string // len = n, keys[i] = "" если нода удалена

	entryPointID uint32
	maxLevel     int
	n            int
	dim          int
	distFn       DistanceFunc
}

// FreezeGraph конвертирует *Graph → *FrozenGraph (CSR layout).
// Принимает keys: map[internal_id] → user_key.
// Выполняется фоновым воркером — не блокирует запросы.
func FreezeGraph(g *Graph, distFn DistanceFunc, keys map[uint64]string) *FrozenGraph {
	n := len(g.nodes)
	if n == 0 {
		return nil
	}
	dim := g.arena.dim

	// 1. Flat vector slab
	data := make([]float32, n*dim)
	nodeKeys := make([]string, n)
	for i := 0; i < n; i++ {
		if !g.nodes[i].Alive {
			continue
		}
		copy(data[i*dim:(i+1)*dim], g.arena.Get(g.nodes[i].VectorOffset))
		nodeKeys[i] = keys[uint64(i)]
	}

	// 2. Строим CSR для каждого слоя от 0 до maxLevel
	layers := make([]FlatLayer, g.maxLevel+1)
	for lyr := 0; lyr <= g.maxLevel; lyr++ {
		offs := make([]uint32, n+1)
		for i := 0; i < n; i++ {
			if !g.nodes[i].Alive || g.nodes[i].Level < lyr {
				offs[i+1] = offs[i]
				continue
			}
			ns := g.getNeighbors(g.nodes[i].NeighborsHandle, lyr)
			offs[i+1] = offs[i] + uint32(len(ns))
		}
		neigh := make([]uint32, offs[n])
		for i := 0; i < n; i++ {
			if !g.nodes[i].Alive || g.nodes[i].Level < lyr {
				continue
			}
			ns := g.getNeighbors(g.nodes[i].NeighborsHandle, lyr)
			for j, nb := range ns {
				neigh[int(offs[i])+j] = uint32(nb)
			}
		}
		layers[lyr] = FlatLayer{neigh: neigh, offs: offs}
	}

	return &FrozenGraph{
		data:         data,
		layers:       layers,
		keys:         nodeKeys,
		entryPointID: uint32(g.entryPointID),
		maxLevel:     g.maxLevel,
		n:            n,
		dim:          dim,
		distFn:       distFn,
	}
}

// Len возвращает число нод в графе.
func (fg *FrozenGraph) Len() int { return fg.n }

// MemoryBytes возвращает приближённый размер в байтах.
func (fg *FrozenGraph) MemoryBytes() int {
	layerBytes := 0
	for _, layer := range fg.layers {
		layerBytes += len(layer.neigh)*4 + len(layer.offs)*4
	}
	return len(fg.data)*4 + layerBytes
}

// Search выполняет полный multi-level HNSW поиск по CSR-графу.
// Фаза 1: greedyClosest по верхним слоям (0 аллок).
// Фаза 2: beam search на layer 0 с sync.Pool (1 аллок — выходной срез).
func (fg *FrozenGraph) Search(query []float32, K, efSearch int) []FrozenResult {
	if fg.n == 0 {
		return nil
	}
	if efSearch < K {
		efSearch = K
	}

	distFn := fg.distFn
	dim := fg.dim

	// ── Фаза 1: greedyClosest (0 аллок) ──
	ep := fg.entryPointID
	for lyr := fg.maxLevel; lyr > 0; lyr-- {
		layer := fg.layers[lyr]
		base := int(ep) * dim
		bestDist := distFn(query, fg.data[base:base+dim])
		improved := true
		for improved {
			improved = false
			start := layer.offs[ep]
			end := layer.offs[ep+1]
			for idx := start; idx < end; idx++ {
				nb := layer.neigh[idx]
				nbBase := int(nb) * dim
				d := distFn(query, fg.data[nbBase:nbBase+dim])
				if d < bestDist {
					bestDist = d
					ep = nb
					improved = true
				}
			}
		}
	}

	// ── Фаза 2: beam search на layer 0, sync.Pool ──
	n := fg.n
	visitedLen := (n + 63) / 64

	st := frozenSearchPool.Get().(*frozenSearchState)

	// Очищаем/расширяем visited bitset
	if cap(st.visited) < visitedLen {
		st.visited = make([]uint64, visitedLen)
	} else {
		st.visited = st.visited[:visitedLen]
		for i := range st.visited {
			st.visited[i] = 0
		}
	}

	// Очищаем heap'ы, расширяем если нужно
	st.cands = st.cands[:0]
	st.res = st.res[:0]
	if cap(st.cands) < efSearch+1 {
		st.cands = make([]frozenCandItem, 0, efSearch+1)
	}
	if cap(st.res) < efSearch+1 {
		st.res = make([]FrozenResult, 0, efSearch+1)
	}

	setVisited := func(id uint32) { st.visited[id/64] |= 1 << (id % 64) }
	isVisited := func(id uint32) bool { return st.visited[id/64]&(1<<(id%64)) != 0 }

	// minHeap кандидатов
	cPush := func(id uint32, d float32) {
		st.cands = append(st.cands, frozenCandItem{id, d})
		i := len(st.cands) - 1
		for i > 0 {
			p := (i - 1) / 2
			if st.cands[p].dist <= st.cands[i].dist {
				break
			}
			st.cands[p], st.cands[i] = st.cands[i], st.cands[p]
			i = p
		}
	}
	cPop := func() frozenCandItem {
		top := st.cands[0]
		l := len(st.cands) - 1
		st.cands[0] = st.cands[l]
		st.cands = st.cands[:l]
		i := 0
		for {
			ll, r, s := 2*i+1, 2*i+2, i
			if ll < len(st.cands) && st.cands[ll].dist < st.cands[s].dist {
				s = ll
			}
			if r < len(st.cands) && st.cands[r].dist < st.cands[s].dist {
				s = r
			}
			if s == i {
				break
			}
			st.cands[i], st.cands[s] = st.cands[s], st.cands[i]
			i = s
		}
		return top
	}

	// maxHeap результатов
	rPush := func(key string, d float32) {
		st.res = append(st.res, FrozenResult{key, d})
		i := len(st.res) - 1
		for i > 0 {
			p := (i - 1) / 2
			if st.res[p].Dist >= st.res[i].Dist {
				break
			}
			st.res[p], st.res[i] = st.res[i], st.res[p]
			i = p
		}
	}
	rPopMax := func() {
		l := len(st.res) - 1
		st.res[0] = st.res[l]
		st.res = st.res[:l]
		i := 0
		for {
			ll, r, s := 2*i+1, 2*i+2, i
			if ll < len(st.res) && st.res[ll].Dist > st.res[s].Dist {
				s = ll
			}
			if r < len(st.res) && st.res[r].Dist > st.res[s].Dist {
				s = r
			}
			if s == i {
				break
			}
			st.res[i], st.res[s] = st.res[s], st.res[i]
			i = s
		}
	}

	// Стартуем с entry point
	epBase := int(ep) * dim
	epDist := distFn(query, fg.data[epBase:epBase+dim])
	setVisited(ep)
	cPush(ep, epDist)
	rPush(fg.keys[ep], epDist)

	layer0 := fg.layers[0]
	for len(st.cands) > 0 {
		curr := cPop()
		if len(st.res) >= efSearch && curr.dist > st.res[0].Dist {
			break
		}
		start := layer0.offs[curr.id]
		end := layer0.offs[curr.id+1]
		for idx := start; idx < end; idx++ {
			nb := layer0.neigh[idx]
			if isVisited(nb) {
				continue
			}
			setVisited(nb)
			nbBase := int(nb) * dim
			d := distFn(query, fg.data[nbBase:nbBase+dim])
			if len(st.res) < efSearch || d < st.res[0].Dist {
				cPush(nb, d)
				rPush(fg.keys[nb], d)
				if len(st.res) > efSearch {
					rPopMax()
				}
			}
		}
	}

	// Сортируем IN pool (безопасно — мы владеем st до Put),
	// затем копируем только top-K. Аллоцируем K, а не efSearch.
	topK := len(st.res)
	if topK > K {
		topK = K
	}
	slices.SortFunc(st.res, func(a, b FrozenResult) int {
		if a.Dist < b.Dist {
			return -1
		}
		if a.Dist > b.Dist {
			return 1
		}
		return 0
	})
	out := make([]FrozenResult, topK) // только K, не efSearch
	copy(out, st.res[:topK])

	// Возвращаем состояние в pool
	frozenSearchPool.Put(st)
	return out
}
