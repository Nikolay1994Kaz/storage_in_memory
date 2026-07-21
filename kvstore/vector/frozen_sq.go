package vector

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
	"sync"
	"unsafe"
)

// =============================================================================
// FrozenGraphSQ — SQ8-сжатый CSR HNSW граф.
//
// Replace-стратегия: float32 slab → []int8 codes + per-dim min/scale.
// Рабочий набор сжимается 4× (39MB → ~10MB), влезает в L3.
//
// Memory layout:
//
//	codes[i*dim : (i+1)*dim]            = квантованный вектор ноды i (int8)
//	layers[lyr].neigh[offs[i]:offs[i+1]] = соседи ноды i (CSR, без изменений)
//	sqMin[d] / sqScale[d]               = per-dim деквантование params
//
// Search: int8-traversal (ADC) для сбора efSearch кандидатов → деквант-rerank
// top-candidates по точному float32 distance к деквантованным векторам.
// =============================================================================

// FrozenGraphSQ — SQ8-compressed immutable CSR граф.
type FrozenGraphSQ struct {
	codes   []uint8   // n*dim байт — квантованные векторы (0..255)
	sqMin   []float32 // [dim] — min по каждой размерности
	sqScale []float32 // [dim] — (max-min)/255 для деквантования

	layers []FlatLayer  // CSR для каждого слоя (без изменений vs FrozenGraph)
	keys   internedKeys // [n], интернированы (blob+offs); пустой = tombstone

	entryPointID uint32
	maxLevel     int
	n            int
	dim          int

	// distFn — точная функция расстояния (float32), используется в rerank-phase.
	// Traversal-phase использует sq8EuclidImpl/sq8DotImpl (ADC over int8).
	distFn DistanceFunc

	// dotMode: true если distFn = dot product (1-dot), false если euclidean (L2²).
	// Определяет какую SQ8-distance использовать в traversal.
	dotMode bool

	// metric — метрика как данные: brute-путям нужно отличать cosine от dot
	// (у обеих dotMode=true), чтобы выбрать fused-ядро. См. sqBruteDist.
	metric Metric

	// zeroQ — нулевой query [dim] для cosine-ветки sqBruteDist (|approx|² через
	// sq8EuclidImpl). Аллоцируется один раз при freeze/load, read-only → безопасен
	// для конкурентных поисков; nil для остальных метрик.
	zeroQ []float32
}

// FreezeGraphSQ конвертирует *Graph → *FrozenGraphSQ (SQ8 CSR layout).
// Два прохода: (1) вычислить per-dim min/max, (2) квантовать.
// metric задаёт ADC-режим обхода и каноническую distFn для rerank.
func FreezeGraphSQ(g *Graph, metric Metric, keys map[uint64]string) *FrozenGraphSQ {
	return FreezeGraphSQOrdered(g, metric, keys, nil)
}

// FreezeGraphSQOrdered — FreezeGraphSQ с переупорядочиванием нод: pos[i] задаёт
// позицию graph-ноды i в frozen-раскладке (nil = identity). См. FreezeGraphOrdered.
func FreezeGraphSQOrdered(g *Graph, metric Metric, keys map[uint64]string, pos []uint32) *FrozenGraphSQ {
	n := len(g.nodes)
	if n == 0 {
		return nil
	}
	dim := g.arena.dim

	at := func(i int) int {
		if pos == nil {
			return i
		}
		return int(pos[i])
	}

	// ── Pass 1: вычислить per-dim min/max, копируя векторы во временный float32 slab
	// (на frozen-позициях). Делаем это одним проходом — float32 уже горячие при
	// копировании из arena.
	data := make([]float32, n*dim)
	nodeKeys := make([]string, n)
	minDim := make([]float32, dim)
	maxDim := make([]float32, dim)
	for d := 0; d < dim; d++ {
		minDim[d] = math.MaxFloat32
		maxDim[d] = -math.MaxFloat32
	}

	for i := 0; i < n; i++ {
		if !g.nodes[i].Alive {
			continue
		}
		p := at(i)
		vec := g.arena.Get(g.nodes[i].VectorOffset)
		copy(data[p*dim:(p+1)*dim], vec)
		nodeKeys[p] = keys[uint64(i)]
		// Обновляем min/max per-dim
		for d := 0; d < dim; d++ {
			v := data[p*dim+d]
			if v < minDim[d] {
				minDim[d] = v
			}
			if v > maxDim[d] {
				maxDim[d] = v
			}
		}
	}

	// ── Вычисляем scale (с защитой от деления на 0 для константных размерностей).
	sqMin := make([]float32, dim)
	sqScale := make([]float32, dim)
	for d := 0; d < dim; d++ {
		sqMin[d] = minDim[d]
		rng := maxDim[d] - minDim[d]
		if rng <= 0 {
			sqScale[d] = 0 // константная размерность — code всегда 0
		} else {
			sqScale[d] = rng / 255.0
		}
	}

	// ── Pass 2: квантовать data → codes (по frozen-позициям).
	codes := make([]uint8, n*dim)
	for i := 0; i < n; i++ {
		if !g.nodes[i].Alive {
			continue // оставляем нули в codes для мёртвых нод
		}
		base := at(i) * dim
		for d := 0; d < dim; d++ {
			if sqScale[d] == 0 {
				codes[base+d] = 0
				continue
			}
			// quantize: q = round((v - min) / scale)
			q := (data[base+d] - sqMin[d]) / sqScale[d]
			qi := int(q + 0.5) // round to nearest
			if qi < 0 {
				qi = 0
			} else if qi > 255 {
				qi = 255
			}
			// uint8 хранит 0..255 напрямую, без sign-issues.
			codes[base+d] = uint8(qi)
		}
	}

	// ── CSR слои (идентично FreezeGraphOrdered: смежность в graph-нумерации,
	// раскладка по frozen-позициям).
	layers := make([]FlatLayer, g.maxLevel+1)
	deg := make([]uint32, n)
	for lyr := 0; lyr <= g.maxLevel; lyr++ {
		clear(deg)
		for i := 0; i < n; i++ {
			if !g.nodes[i].Alive || g.nodes[i].Level < lyr {
				continue
			}
			deg[at(i)] = uint32(len(g.getNeighbors(g.nodes[i].NeighborsHandle, lyr)))
		}
		offs := make([]uint32, n+1)
		for p := 0; p < n; p++ {
			offs[p+1] = offs[p] + deg[p]
		}
		neigh := make([]uint32, offs[n])
		for i := 0; i < n; i++ {
			if !g.nodes[i].Alive || g.nodes[i].Level < lyr {
				continue
			}
			p := at(i)
			ns := g.getNeighbors(g.nodes[i].NeighborsHandle, lyr)
			for j, nb := range ns {
				neigh[int(offs[p])+j] = uint32(at(int(nb)))
			}
		}
		layers[lyr] = FlatLayer{neigh: neigh, offs: offs}
	}

	return &FrozenGraphSQ{
		codes:        codes,
		sqMin:        sqMin,
		sqScale:      sqScale,
		layers:       layers,
		keys:         buildInternedKeys(nodeKeys),
		entryPointID: uint32(at(int(g.entryPointID))),
		maxLevel:     g.maxLevel,
		n:            n,
		dim:          dim,
		distFn:       metric.DistanceFunc(),
		dotMode:      metric.IsDot(),
		metric:       metric,
		zeroQ:        makeZeroQ(metric, dim),
	}
}

// makeZeroQ — нулевой query для cosine-ветки sqBruteDist (nil для прочих метрик).
func makeZeroQ(metric Metric, dim int) []float32 {
	if metric != MetricCosine {
		return nil
	}
	return make([]float32, dim)
}

// Len возвращает число нод в графе.
func (fg *FrozenGraphSQ) Len() int { return fg.n }

// sqBruteDist возвращает функцию точной дистанции по кодам одного вектора для
// brute-путей. Семантика ≡ прежнему «скалярный деквант в buf + distFn» (та же
// точность, что rerank-фаза), но через fused AVX2 ADC-ядра — деквант и дистанция
// одним SIMD-проходом (микробенч 11.07: euclid/dot 9–11.6×, cosine 5–6.8×):
//   - euclid: sq8EuclidImpl ≡ деквант+EuclideanDistance
//   - dot:    1−sq8DotImpl ≡ деквант+DotProductDistance
//   - cosine: два прохода существующими ядрами — dot = sq8DotImpl,
//     |approx|² = sq8EuclidImpl(нулевой query) = Σ approx²; codes после первого
//     прохода горячие в L1. Нормировка по |query| предвычислена на вызов, поэтому
//     скоры совпадают с CosineDistance(query, деквант) — сравнимость с rerank и
//     fp32-путями сохранена.
//
// Неизвестная метрика → прежний скалярный путь (fallback, поведение не меняется).
func (fg *FrozenGraphSQ) sqBruteDist(query []float32) func(codes []uint8) float32 {
	switch fg.metric {
	case MetricEuclidean:
		return func(cs []uint8) float32 {
			return sq8EuclidImpl(query, cs, fg.sqMin, fg.sqScale)
		}
	case MetricDot:
		return func(cs []uint8) float32 {
			return 1 - sq8DotImpl(query, cs, fg.sqMin, fg.sqScale)
		}
	case MetricCosine:
		qNorm2 := dotProductImpl(query, query)
		if qNorm2 == 0 {
			// CosineDistance при нулевом query → 1 для любого вектора.
			return func([]uint8) float32 { return 1 }
		}
		invQNorm := 1 / float32(math.Sqrt(float64(qNorm2)))
		zeroQ := fg.zeroQ
		if zeroQ == nil { // fg собран вручную (тесты) — аллоцируем на вызов
			zeroQ = make([]float32, fg.dim)
		}
		return func(cs []uint8) float32 {
			dot := sq8DotImpl(query, cs, fg.sqMin, fg.sqScale)
			normB2 := sq8EuclidImpl(zeroQ, cs, fg.sqMin, fg.sqScale)
			if normB2 == 0 {
				return 1
			}
			return 1 - dot*invQNorm/float32(math.Sqrt(float64(normB2)))
		}
	default:
		buf := make([]float32, fg.dim)
		return func(cs []uint8) float32 {
			for d := range cs {
				buf[d] = fg.sqMin[d] + float32(cs[d])*fg.sqScale[d]
			}
			return fg.distFn(query, buf)
		}
	}
}

// bruteRange — точный top-K по непрерывному диапазону [start,end) (нижний режим #7
// для SQ8-сегмента). Дистанция — fused ADC (см. sqBruteDist) с той же точностью,
// что у rerank-фазы Search, но исчерпывающе по блоку (recall=1.0 при SQ-точности).
// filterFn применяется к ключам.
func (fg *FrozenGraphSQ) bruteRange(query []float32, K, start, end int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	if K <= 0 || start >= end {
		return dst[:0]
	}
	top := dst[:0]
	dist := fg.sqBruteDist(query)
	for i := start; i < end; i++ {
		key := fg.keys.view(i)
		if key == "" {
			continue
		}
		if filterFn != nil && !filterFn(key) {
			continue
		}
		base := i * fg.dim
		top = insertTopK(top, K, key, dist(fg.codes[base:base+fg.dim]))
	}
	return top
}

// viewKey — zero-copy ключ вектора i (для frozenFilterable).
func (fg *FrozenGraphSQ) viewKey(i int) string { return fg.keys.view(i) }

// bruteRangeAttr — как bruteRange, но фильтр задан предикатом по frozen-индексу
// (predIdx(i) bool), а не строковым ключом. Это рычаг #2: проверка атрибутов идёт
// по uint-колонкам (codes[i]==code, lo≤vals[i]≤hi) — O(1) индексация, без strconv/hash
// строки на каждый вектор. predIdx == nil → без фильтра (весь блок тенанта).
func (fg *FrozenGraphSQ) bruteRangeAttr(query []float32, K, start, end int, dst []FrozenResult, predIdx func(int) bool) []FrozenResult {
	if K <= 0 || start >= end {
		return dst[:0]
	}
	top := dst[:0]
	dist := fg.sqBruteDist(query)
	for i := start; i < end; i++ {
		if predIdx != nil && !predIdx(i) {
			continue
		}
		key := fg.keys.view(i)
		if key == "" {
			continue
		}
		base := i * fg.dim
		top = insertTopK(top, K, key, dist(fg.codes[base:base+fg.dim]))
	}
	return top
}

// residualBruteBudget — для SQ8 (ADC-перебор дёшев) residual-brute по блоку выгоднее
// обхода filtered-HNSW, пока совпавших ≤ efSearch×residualBruteFactor. См.
// searchFilterFrozen. efSearch≤0 → берём 1 (хотя бы минимальный бюджет, не отключаем).
func (fg *FrozenGraphSQ) residualBruteBudget(efSearch int) int {
	if efSearch <= 0 {
		efSearch = 1
	}
	return efSearch * residualBruteFactor
}

// MemoryBytes возвращает приближённый размер в байтах.
func (fg *FrozenGraphSQ) MemoryBytes() int {
	layerBytes := 0
	for _, layer := range fg.layers {
		layerBytes += len(layer.neigh)*4 + len(layer.offs)*4
	}
	return len(fg.codes) + len(fg.sqMin)*4 + len(fg.sqScale)*4 + layerBytes
}

// =============================================================================
// Search — int8 traversal (ADC) + деквант-rerank
// =============================================================================

// frozenSQSearchState — pool-able буфер для FrozenGraphSQ.Search.
// Дополнительно к frozenSearchState хранит resIDs []uint32 — node-ID каждого
// результата (для rerank по codes).
type frozenSQSearchState struct {
	visited []uint64         // bitset
	cands   []frozenCandItem // minHeap кандидатов
	res     []FrozenResult   // maxHeap результатов
	resIDs  []uint32         // node-ID для каждого res[i] (parallel массив)
}

var frozenSQSearchPool = sync.Pool{
	New: func() any {
		return &frozenSQSearchState{
			visited: make([]uint64, 0, 512),
			cands:   make([]frozenCandItem, 0, 128),
			res:     make([]FrozenResult, 0, 128),
			resIDs:  make([]uint32, 0, 128),
		}
	},
}

// sqApproxDist — диспетчер: euclidean или dot в зависимости от dotMode.
// Возвращает approximate distance query↔codes по SQ8-ADC.
func (fg *FrozenGraphSQ) sqApproxDist(query []float32, codesSlice []uint8) float32 {
	if fg.dotMode {
		// sq8DotImpl возвращает СХОДСТВО (q·approx, больше=ближе), а обход ждёт
		// РАССТОЯНИЕ (меньше=ближе) → конвертируем в 1-dot (как DotProductDistance).
		// Без этого dot-режим инвертирует порядок (recall→0). Путь не исполнялся
		// раньше из-за бага isDotDistance, поэтому инверсия не всплывала.
		return 1 - sq8DotImpl(query, codesSlice, fg.sqMin, fg.sqScale)
	}
	return sq8EuclidImpl(query, codesSlice, fg.sqMin, fg.sqScale)
}

// Search выполняет multi-level HNSW поиск по SQ8-графу с последующим rerank.
//
// Фаза 1: greedyClosest по верхним слоям (int8 ADC distance, 0 аллок).
// Фаза 2: beam search на layer 0 (int8 ADC, sync.Pool).
// Фаза 3: rerank — деквантование top-candidates + точный float32 distance.
//
// filterFn != nil: фильтр применяется к результатам, не к обходу (как в FrozenGraph).
func (fg *FrozenGraphSQ) Search(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	return fg.searchWithIdx(query, K, efSearch, dst, filterFn, nil)
}

// searchWithIdx — тело Search с доп. фильтром по frozen-индексу (node id = frozen id).
// predIdx применяется к результатам наравне с filterFn (И). См. FrozenGraph.searchWithIdx.
func (fg *FrozenGraphSQ) searchWithIdx(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool, predIdx func(int) bool) []FrozenResult {
	if fg.n == 0 {
		return nil
	}
	if efSearch < K {
		efSearch = K
	}

	dim := fg.dim

	// ── Фаза 1: greedyClosest (0 аллок, int8 ADC distance) ──
	ep := fg.entryPointID
	for lyr := fg.maxLevel; lyr > 0; lyr-- {
		layer := fg.layers[lyr]
		bestDist := fg.sqApproxDist(query, fg.codes[int(ep)*dim:int(ep)*dim+dim])
		improved := true
		for improved {
			improved = false
			start := layer.offs[ep]
			end := layer.offs[ep+1]
			for idx := start; idx < end; idx++ {
				nb := layer.neigh[idx]
				nbBase := int(nb) * dim
				d := fg.sqApproxDist(query, fg.codes[nbBase:nbBase+dim])
				if d < bestDist {
					bestDist = d
					ep = uint32(nb)
					improved = true
				}
			}
		}
	}

	// ── Фаза 2: beam search на layer 0 (int8 ADC, sync.Pool) ──
	n := fg.n
	visitedLen := (n + 63) / 64

	st := frozenSQSearchPool.Get().(*frozenSQSearchState)

	if cap(st.visited) < visitedLen {
		st.visited = make([]uint64, visitedLen)
	} else {
		st.visited = st.visited[:visitedLen]
		for i := range st.visited {
			st.visited[i] = 0
		}
	}
	st.cands = st.cands[:0]
	st.res = st.res[:0]
	st.resIDs = st.resIDs[:0]
	if cap(st.cands) < efSearch+1 {
		st.cands = make([]frozenCandItem, 0, efSearch+1)
	}
	if cap(st.res) < efSearch+1 {
		st.res = make([]FrozenResult, 0, efSearch+1)
	}
	if cap(st.resIDs) < efSearch+1 {
		st.resIDs = make([]uint32, 0, efSearch+1)
	}

	setVisited := func(id uint32) { st.visited[id/64] |= 1 << (id % 64) }
	isVisited := func(id uint32) bool { return st.visited[id/64]&(1<<(id%64)) != 0 }

	// minHeap кандидатов (ADC distance)
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

	// maxHeap результатов (ADC distance), с parallel resIDs
	rPush := func(id uint32, key string, d float32) {
		st.res = append(st.res, FrozenResult{key, d})
		st.resIDs = append(st.resIDs, id)
		i := len(st.res) - 1
		for i > 0 {
			p := (i - 1) / 2
			if st.res[p].Dist >= st.res[i].Dist {
				break
			}
			st.res[p], st.res[i] = st.res[i], st.res[p]
			st.resIDs[p], st.resIDs[i] = st.resIDs[i], st.resIDs[p]
			i = p
		}
	}
	rPopMax := func() {
		l := len(st.res) - 1
		st.res[0] = st.res[l]
		st.resIDs[0] = st.resIDs[l]
		st.res = st.res[:l]
		st.resIDs = st.resIDs[:l]
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
			st.resIDs[i], st.resIDs[s] = st.resIDs[s], st.resIDs[i]
			i = s
		}
	}

	// Стартуем с entry point
	epBase := int(ep) * dim
	epDist := fg.sqApproxDist(query, fg.codes[epBase:epBase+dim])
	setVisited(ep)
	cPush(ep, epDist)
	// view() — zero-copy, валиден на время поиска (fg жив). Клонируем при выдаче.
	epKey := fg.keys.view(int(ep))
	if (filterFn == nil || (epKey != "" && filterFn(epKey))) && (predIdx == nil || predIdx(int(ep))) {
		rPush(ep, epKey, epDist)
	}

	layer0 := fg.layers[0]
	for len(st.cands) > 0 {
		curr := cPop()
		if len(st.res) >= efSearch && curr.dist > st.res[0].Dist {
			break
		}
		start := layer0.offs[curr.id]
		end := layer0.offs[curr.id+1]
		for idx := start; idx < end; idx++ {
			nb := uint32(layer0.neigh[idx])
			if isVisited(nb) {
				continue
			}
			setVisited(nb)
			nbBase := int(nb) * dim
			d := fg.sqApproxDist(query, fg.codes[nbBase:nbBase+dim])
			if len(st.res) < efSearch || d < st.res[0].Dist {
				cPush(nb, d)
				nbKey := fg.keys.view(int(nb))
				if (filterFn == nil || (nbKey != "" && filterFn(nbKey))) && (predIdx == nil || predIdx(int(nb))) {
					rPush(nb, nbKey, d)
					if len(st.res) > efSearch {
						rPopMax()
					}
				}
			}
		}
	}

	// ── Фаза 3: RERANK — точный distance каждого кандидата fused ADC-ядром ──
	// Семантика прежняя (≡ скалярный деквант + distFn, см. sqBruteDist): rerank
	// переупорядочивает top-candidates по точному расстоянию к деквантованным
	// векторам. Это сохраняет recall: ADC-noise влияет на traversal, но финальный
	// порядок кандидатов определяется более точно.
	rerankDist := fg.sqBruteDist(query)
	for i := range st.res {
		base := int(st.resIDs[i]) * dim
		st.res[i].Dist = rerankDist(fg.codes[base : base+dim])
	}

	// Сортируем по точному расстоянию, берём top-K
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

	if cap(dst) < topK {
		dst = make([]FrozenResult, topK)
	} else {
		dst = dst[:topK]
	}
	// st.res[i].Key — zero-copy view в blob (валиден, пока жив fg). Отдаём views
	// БЕЗ клонирования; клонирование — обязанность внешней границы (leveled search).
	copy(dst, st.res[:topK])

	frozenSQSearchPool.Put(st)
	return dst
}

// =============================================================================
// Сериализация FrozenGraphSQ
//
// Формат (little-endian):
//   [4B n][4B dim][4B maxLevel][4B entryPointID]
//   [dim*4B sqMin float32]
//   [dim*4B sqScale float32]
//   [n*dim B codes int8]            ← 1 байт/элемент (было *4 для float32)
//   [4B nLayers]
//   для каждого слоя: [4B nOffs][offs][4B nNeigh][neigh]
//   [4B nKeys] + для каждого [2B len][bytes]
// =============================================================================

// WriteGraphToSQ сериализует FrozenGraphSQ. Zero-alloc для codes (unsafe.Slice).
func (fg *FrozenGraphSQ) WriteGraphToSQ(w io.Writer) error {
	var hdr [16]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(fg.n))
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(fg.dim))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(fg.maxLevel))
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(fg.entryPointID))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("frozenSQ: write header: %w", err)
	}

	// sqMin [dim*4]
	if fg.dim > 0 {
		b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.sqMin[0])), len(fg.sqMin)*4)
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("frozenSQ: write sqMin: %w", err)
		}
	}
	// sqScale [dim*4]
	if fg.dim > 0 {
		b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.sqScale[0])), len(fg.sqScale)*4)
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("frozenSQ: write sqScale: %w", err)
		}
	}
	// codes [n*dim] — 1 байт/элемент (int8 = byte по размеру)
	if fg.n > 0 && fg.dim > 0 {
		b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.codes[0])), len(fg.codes))
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("frozenSQ: write codes: %w", err)
		}
	}

	// Layers (идентично WriteGraphTo)
	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], uint32(len(fg.layers)))
	if _, err := w.Write(u4[:]); err != nil {
		return fmt.Errorf("frozenSQ: write nLayers: %w", err)
	}
	for i, layer := range fg.layers {
		binary.LittleEndian.PutUint32(u4[:], uint32(len(layer.offs)))
		if _, err := w.Write(u4[:]); err != nil {
			return fmt.Errorf("frozenSQ: write nOffs[%d]: %w", i, err)
		}
		if len(layer.offs) > 0 {
			b := unsafe.Slice((*byte)(unsafe.Pointer(&layer.offs[0])), len(layer.offs)*4)
			if _, err := w.Write(b); err != nil {
				return fmt.Errorf("frozenSQ: write offs[%d]: %w", i, err)
			}
		}
		binary.LittleEndian.PutUint32(u4[:], uint32(len(layer.neigh)))
		if _, err := w.Write(u4[:]); err != nil {
			return fmt.Errorf("frozenSQ: write nNeigh[%d]: %w", i, err)
		}
		if len(layer.neigh) > 0 {
			b := unsafe.Slice((*byte)(unsafe.Pointer(&layer.neigh[0])), len(layer.neigh)*4)
			if _, err := w.Write(b); err != nil {
				return fmt.Errorf("frozenSQ: write neigh[%d]: %w", i, err)
			}
		}
	}

	// Keys (идентично WriteGraphTo)
	binary.LittleEndian.PutUint32(u4[:], uint32(fg.n))
	if _, err := w.Write(u4[:]); err != nil {
		return fmt.Errorf("frozenSQ: write nKeys: %w", err)
	}
	var klen [2]byte
	for i := 0; i < fg.n; i++ {
		key := fg.keys.view(i) // read-only, пишется сразу — view безопасен
		binary.LittleEndian.PutUint16(klen[:], uint16(len(key)))
		if _, err := w.Write(klen[:]); err != nil {
			return fmt.Errorf("frozenSQ: write keyLen[%d]: %w", i, err)
		}
		if len(key) > 0 {
			if _, err := io.WriteString(w, key); err != nil {
				return fmt.Errorf("frozenSQ: write key[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// ReadFrozenGraphSQ десериализует FrozenGraphSQ.
// metric задаёт ADC-режим обхода и каноническую distFn для rerank.
func ReadFrozenGraphSQ(r io.Reader, metric Metric) (*FrozenGraphSQ, error) {
	var hdr [16]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("frozenSQ: read header: %w", err)
	}
	n := int(binary.LittleEndian.Uint32(hdr[0:4]))
	dim := int(binary.LittleEndian.Uint32(hdr[4:8]))
	maxLevel := int(binary.LittleEndian.Uint32(hdr[8:12]))
	entryPointID := uint32(binary.LittleEndian.Uint32(hdr[12:16]))

	fg := &FrozenGraphSQ{
		n:            n,
		dim:          dim,
		maxLevel:     maxLevel,
		entryPointID: entryPointID,
		distFn:       metric.DistanceFunc(),
		dotMode:      metric.IsDot(),
		metric:       metric,
		zeroQ:        makeZeroQ(metric, dim),
	}

	// sqMin
	if dim > 0 {
		fg.sqMin = make([]float32, dim)
		b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.sqMin[0])), dim*4)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, fmt.Errorf("frozenSQ: read sqMin: %w", err)
		}
	}
	// sqScale
	if dim > 0 {
		fg.sqScale = make([]float32, dim)
		b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.sqScale[0])), dim*4)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, fmt.Errorf("frozenSQ: read sqScale: %w", err)
		}
	}
	// codes [n*dim bytes] — int8 = byte по размеру, читаем через unsafe.Slice
	if n > 0 && dim > 0 {
		fg.codes = make([]uint8, n*dim)
		b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.codes[0])), n*dim)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, fmt.Errorf("frozenSQ: read codes: %w", err)
		}
	}

	// Layers
	var u4 [4]byte
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return nil, fmt.Errorf("frozenSQ: read nLayers: %w", err)
	}
	nLayers := int(binary.LittleEndian.Uint32(u4[:]))
	fg.layers = make([]FlatLayer, nLayers)
	for i := range fg.layers {
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return nil, fmt.Errorf("frozenSQ: read nOffs[%d]: %w", i, err)
		}
		nOffs := int(binary.LittleEndian.Uint32(u4[:]))
		if nOffs > 0 {
			fg.layers[i].offs = make([]uint32, nOffs)
			b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.layers[i].offs[0])), nOffs*4)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, fmt.Errorf("frozenSQ: read offs[%d]: %w", i, err)
			}
		}
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return nil, fmt.Errorf("frozenSQ: read nNeigh[%d]: %w", i, err)
		}
		nNeigh := int(binary.LittleEndian.Uint32(u4[:]))
		if nNeigh > 0 {
			fg.layers[i].neigh = make([]uint32, nNeigh)
			b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.layers[i].neigh[0])), nNeigh*4)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, fmt.Errorf("frozenSQ: read neigh[%d]: %w", i, err)
			}
		}
	}

	// Keys
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return nil, fmt.Errorf("frozenSQ: read nKeys: %w", err)
	}
	nKeys := int(binary.LittleEndian.Uint32(u4[:]))
	offs := make([]uint32, nKeys+1)
	var blob []byte
	var klen [2]byte
	for i := 0; i < nKeys; i++ {
		if _, err := io.ReadFull(r, klen[:]); err != nil {
			return nil, fmt.Errorf("frozenSQ: read keyLen[%d]: %w", i, err)
		}
		kl := int(binary.LittleEndian.Uint16(klen[:]))
		if kl > 0 {
			// читаем байты ключа прямо в blob — без per-key string() аллокации
			blob = slices.Grow(blob, kl)
			start := len(blob)
			blob = blob[:start+kl]
			if _, err := io.ReadFull(r, blob[start:]); err != nil {
				return nil, fmt.Errorf("frozenSQ: read key[%d]: %w", i, err)
			}
		}
		offs[i+1] = uint32(len(blob))
	}
	fg.keys = newInternedKeys(blob, offs)

	return fg, nil
}
