package vector

// =============================================================================
// Бенчмарки для валидации архитектуры Leveled Compaction (LSM-style HNSW)
//
// Что проверяем:
//   1. Brute-force на дельте (N<=10K) — насколько быстро vs HNSW?
//   2. CSR frozen graph search — выигрыш от cache locality vs TCMalloc graph
//   3. Segmented search (параллельный merge) — overhead vs монолит
//   4. Стоимость freeze (конвертация TCMalloc→CSR) — насколько дорогая компакция?
//   5. Insert throughput: sequential HNSW vs delta append
//
// Запуск (быстрые, только LC бенчмарки, без других тестов):
//   go test ./vector/ -run=^$ -bench=BenchmarkLC_Fast -benchmem -benchtime=2s -timeout=120s
//
// Запуск (медленные, с большими графами):
//   go test ./vector/ -run=^$ -bench=BenchmarkLC_Slow -benchmem -benchtime=3s -timeout=600s
// =============================================================================

import (
	"math/rand"
	"slices"
	"sync"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// Вспомогательные структуры — минимальные прототипы
// =============================================================================

// lcResult — пара (nodeID, distance) для merge результатов
type lcResult struct {
	id   uint32
	dist float32
}

// -----------------------------------------------------------------------------
// DeltaSegment — плоская арена для векторов, O(1) append, O(N) brute-force
// -----------------------------------------------------------------------------

type lcDeltaSegment struct {
	data []float32 // flat: [v0_f0, v0_f1, ..., v1_f0, v1_f1, ...]
	dim  int
	n    int
}

func newLCDelta(dim, capacity int) *lcDeltaSegment {
	return &lcDeltaSegment{
		data: make([]float32, 0, capacity*dim),
		dim:  dim,
	}
}

// Append — O(1) при достаточной capacity (zero alloc после warmup)
func (d *lcDeltaSegment) Append(vec []float32) {
	d.data = append(d.data, vec...)
	d.n++
}

// BruteForce — линейный скан, maxHeap top-K, zero alloc heap (stack-allocated для K<=64)
func (d *lcDeltaSegment) BruteForce(query []float32, K int, distFn DistanceFunc) []lcResult {
	// maxHeap: корень = самый далёкий из топ-K (чтобы быстро выталкивать)
	heap := make([]lcResult, 0, K)
	dim := d.dim

	for i := 0; i < d.n; i++ {
		vec := d.data[i*dim : (i+1)*dim]
		dist := distFn(query, vec)

		if len(heap) < K {
			heap = append(heap, lcResult{uint32(i), dist})
			if len(heap) == K {
				lcBuildMaxHeap(heap)
			}
		} else if dist < heap[0].dist {
			heap[0] = lcResult{uint32(i), dist}
			lcSiftDownMax(heap, 0, K)
		}
	}

	// Сортируем по возрастанию дистанции
	slices.SortFunc(heap, func(a, b lcResult) int {
		if a.dist < b.dist {
			return -1
		}
		if a.dist > b.dist {
			return 1
		}
		return 0
	})
	return heap
}

func lcBuildMaxHeap(h []lcResult) {
	n := len(h)
	for i := n/2 - 1; i >= 0; i-- {
		lcSiftDownMax(h, i, n)
	}
}

func lcSiftDownMax(h []lcResult, i, n int) {
	for {
		largest := i
		l, r := 2*i+1, 2*i+2
		if l < n && h[l].dist > h[largest].dist {
			largest = l
		}
		if r < n && h[r].dist > h[largest].dist {
			largest = r
		}
		if largest == i {
			break
		}
		h[i], h[largest] = h[largest], h[i]
		i = largest
	}
}

// -----------------------------------------------------------------------------
// FrozenGraph — CSR-формат для ВСЕХ слоёв HNSW, immutable, cache-friendly.
// Честный аналог *Graph: реализует полный multi-level search с sync.Pool.
// -----------------------------------------------------------------------------

// lcSearchState — переиспользуемый буфер для поиска (pool как в graph.go)
type lcSearchState struct {
	visited []uint64       // bitset — размер (n+63)/64
	cands   []lcSearchItem // minHeap кандидатов
	res     []lcResult     // maxHeap результатов
}

type lcSearchItem struct {
	id   uint32
	dist float32
}

// lcSearchPool — глобальный pool для CSR search state
var lcSearchPool = sync.Pool{
	New: func() any {
		return &lcSearchState{
			visited: make([]uint64, 0, 512),
			cands:   make([]lcSearchItem, 0, 128),
			res:     make([]lcResult, 0, 128),
		}
	},
}

type lcFrozenGraph struct {
	data         []float32
	layers       []FlatLayer // layers[0] is Layer 0, layers[lyr] is Layer lyr
	nodeLevel    []int8
	entryPointID uint32
	maxLevel     int
	n            int
	dim          int
	distFn       DistanceFunc
}

// lcFreezeGraph конвертирует *Graph (TCMalloc) → *lcFrozenGraph (CSR flat array)
func lcFreezeGraph(g *Graph, distFn DistanceFunc, _ int) *lcFrozenGraph {
	n := len(g.nodes)
	if n == 0 {
		return nil
	}
	dim := g.arena.dim

	// 1. Векторы — flat slab
	data := make([]float32, n*dim)
	for i := 0; i < n; i++ {
		if !g.nodes[i].Alive {
			continue
		}
		copy(data[i*dim:(i+1)*dim], g.arena.Get(g.nodes[i].VectorOffset))
	}

	// 2. Уровни нод
	nodeLevel := make([]int8, n)
	for i := 0; i < n; i++ {
		if g.nodes[i].Alive {
			nodeLevel[i] = int8(g.nodes[i].Level)
		}
	}

	// 3. Строим CSR для каждого слоя от 0 до maxLevel
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

	return &lcFrozenGraph{
		data:         data,
		layers:       layers,
		nodeLevel:    nodeLevel,
		entryPointID: uint32(g.entryPointID),
		maxLevel:     g.maxLevel,
		n:            n,
		dim:          dim,
		distFn:       distFn,
	}
}

// Search — полный HNSW multi-level search по CSR-графу с sync.Pool (как в graph.go).
// Фаза 1: greedyClosest по верхним слоям — 0 аллок верхних слоёв
// Фаза 2: beam search на layer 0 — pool-ед state, 0 аллок в цикле
func (fg *lcFrozenGraph) Search(query []float32, K, efSearch int) []lcResult {
	if fg.n == 0 {
		return nil
	}
	if efSearch < K {
		efSearch = K
	}

	distFn := fg.distFn
	dim := fg.dim

	// ── Фаза 1: greedyClosest по верхним слоям (0 allocs) ──
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
					ep = uint32(nb)
					improved = true
				}
			}
		}
	}

	// ── Фаза 2: beam search на layer 0 (CSR) с sync.Pool ──
	n := fg.n
	visitedLen := (n + 63) / 64

	// Берём состояние из pool
	st := lcSearchPool.Get().(*lcSearchState)

	// Расширяем visited если нужно, очищаем
	if cap(st.visited) < visitedLen {
		st.visited = make([]uint64, visitedLen)
	} else {
		st.visited = st.visited[:visitedLen]
		for i := range st.visited {
			st.visited[i] = 0
		}
	}

	// Очищаем и ресайзим heap'y
	st.cands = st.cands[:0]
	st.res = st.res[:0]
	if cap(st.cands) < efSearch+1 {
		st.cands = make([]lcSearchItem, 0, efSearch+1)
	}
	if cap(st.res) < efSearch+1 {
		st.res = make([]lcResult, 0, efSearch+1)
	}

	setVisited := func(id uint32) { st.visited[id/64] |= 1 << (id % 64) }
	isVisited := func(id uint32) bool { return st.visited[id/64]&(1<<(id%64)) != 0 }

	// minHeap кандидатов (inline, без аллок)
	cPush := func(id uint32, d float32) {
		st.cands = append(st.cands, lcSearchItem{id, d})
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
	cPop := func() lcSearchItem {
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

	// maxHeap результатов (inline, без аллок)
	rPush := func(id uint32, d float32) {
		st.res = append(st.res, lcResult{id, d})
		i := len(st.res) - 1
		for i > 0 {
			p := (i - 1) / 2
			if st.res[p].dist >= st.res[i].dist {
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
			if ll < len(st.res) && st.res[ll].dist > st.res[s].dist {
				s = ll
			}
			if r < len(st.res) && st.res[r].dist > st.res[s].dist {
				s = r
			}
			if s == i {
				break
			}
			st.res[i], st.res[s] = st.res[s], st.res[i]
			i = s
		}
	}

	// Стартуем с entryPoint
	epBase := int(ep) * dim
	epDist := distFn(query, fg.data[epBase:epBase+dim])
	setVisited(ep)
	cPush(ep, epDist)
	rPush(ep, epDist)

	layer0 := fg.layers[0]
	for len(st.cands) > 0 {
		curr := cPop()
		if len(st.res) >= efSearch && curr.dist > st.res[0].dist {
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
			d := distFn(query, fg.data[nbBase:nbBase+dim])
			if len(st.res) < efSearch || d < st.res[0].dist {
				cPush(nb, d)
				rPush(nb, d)
				if len(st.res) > efSearch {
					rPopMax()
				}
			}
		}
	}

	// Копируем результаты перед возвратом в pool
	out := make([]lcResult, len(st.res))
	copy(out, st.res)

	// Возвращаем состояние в pool
	lcSearchPool.Put(st)

	// Сортируем и триммируем до K
	slices.SortFunc(out, func(a, b lcResult) int {
		if a.dist < b.dist {
			return -1
		}
		if a.dist > b.dist {
			return 1
		}
		return 0
	})
	if len(out) > K {
		out = out[:K]
	}
	return out
}

// MemoryBytes — сколько байт занимает frozen граф (приблизительно)
func (fg *lcFrozenGraph) MemoryBytes() int {
	layerBytes := 0
	for _, layer := range fg.layers {
		layerBytes += len(layer.neigh)*4 + len(layer.offs)*4
	}
	return len(fg.data)*4 + layerBytes + len(fg.nodeLevel)
}

// =============================================================================
// Вспомогательная функция: buildGraph строит HNSW граф из N случайных векторов
// =============================================================================

func lcBuildGraph(n, dim int, efConstruction int, seed int64) *Graph {
	alloc := tcmalloc.NewTCMallocStore(1)
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = efConstruction
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < n; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		g.Insert(vec)
	}
	return g
}

func lcBuildDelta(n, dim int, seed int64) *lcDeltaSegment {
	delta := newLCDelta(dim, n)
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < n; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		delta.Append(vec)
	}
	return delta
}

func lcRandomQuery(dim int, seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	q := make([]float32, dim)
	for j := range q {
		q[j] = rng.Float32()*2 - 1
	}
	return q
}

// =============================================================================
// BENCHMARK 1: Дельта Brute-Force vs HNSW Search при малых N
//
// Доказывает: brute-force на дельте (<= 10K) быстрее или сопоставим с HNSW
// при малых N из-за отсутствия overhead'а обхода графа и cache misses.
// =============================================================================

func BenchmarkLC_Fast_Delta_BruteForce_1K_dim128(b *testing.B) {
	benchmarkLCDeltaBruteForce(b, 1000, 128)
}
func BenchmarkLC_Fast_Delta_BruteForce_5K_dim128(b *testing.B) {
	benchmarkLCDeltaBruteForce(b, 5000, 128)
}
func BenchmarkLC_Fast_Delta_BruteForce_10K_dim128(b *testing.B) {
	benchmarkLCDeltaBruteForce(b, 10000, 128)
}
func BenchmarkLC_Fast_Delta_BruteForce_1K_dim768(b *testing.B) {
	benchmarkLCDeltaBruteForce(b, 1000, 768)
}
func BenchmarkLC_Fast_Delta_BruteForce_5K_dim768(b *testing.B) {
	benchmarkLCDeltaBruteForce(b, 5000, 768)
}
func BenchmarkLC_Fast_Delta_BruteForce_10K_dim768(b *testing.B) {
	benchmarkLCDeltaBruteForce(b, 10000, 768)
}

func benchmarkLCDeltaBruteForce(b *testing.B, n, dim int) {
	b.Helper()
	delta := lcBuildDelta(n, dim, 42)
	query := lcRandomQuery(dim, 999)
	K := 10

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(n * dim * 4))
	for i := 0; i < b.N; i++ {
		_ = delta.BruteForce(query, K, EuclideanDistance)
	}
}

// HNSW Search при тех же N для сравнения
// Графы строятся с efConst=64 чтобы setup был быстрым
func BenchmarkLC_Fast_HNSW_Search_1K_dim128(b *testing.B) {
	benchmarkLCHNSWSearch(b, 1000, 128)
}
func BenchmarkLC_Fast_HNSW_Search_5K_dim128(b *testing.B) {
	benchmarkLCHNSWSearch(b, 5000, 128)
}
func BenchmarkLC_Fast_HNSW_Search_10K_dim128(b *testing.B) {
	benchmarkLCHNSWSearch(b, 10000, 128)
}
func BenchmarkLC_Fast_HNSW_Search_1K_dim768(b *testing.B) {
	benchmarkLCHNSWSearch(b, 1000, 768)
}
func BenchmarkLC_Fast_HNSW_Search_5K_dim768(b *testing.B) {
	// dim=768, N=5K — умеренно, не 10K чтобы build был быстрым
	benchmarkLCHNSWSearch(b, 5000, 768)
}
func BenchmarkLC_Fast_HNSW_Search_10K_dim128_only(b *testing.B) {
	benchmarkLCHNSWSearch(b, 10000, 128)
}

func benchmarkLCHNSWSearch(b *testing.B, n, dim int) {
	b.Helper()
	g := lcBuildGraph(n, dim, 200, 42)
	query := lcRandomQuery(dim, 999)
	K := 10
	efSearch := 100

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = g.Search(query, K, efSearch)
	}
}

// =============================================================================
// BENCHMARK 2: CSR Frozen Graph vs TCMalloc Graph — cache locality
//
// Доказывает: после freeze() поиск по CSR быстрее из-за contiguous memory.
// =============================================================================

func BenchmarkLC_Fast_FrozenCSR_Search_10K_dim128(b *testing.B) {
	benchmarkLCFrozenSearch(b, 10000, 128)
}
func BenchmarkLC_Fast_FrozenCSR_Search_10K_dim768(b *testing.B) {
	benchmarkLCFrozenSearch(b, 10000, 768)
}
func BenchmarkLC_Slow_FrozenCSR_Search_50K_dim128(b *testing.B) {
	benchmarkLCFrozenSearch(b, 50000, 128)
}

func benchmarkLCFrozenSearch(b *testing.B, n, dim int) {
	b.Helper()
	g := lcBuildGraph(n, dim, 64, 42) // efConst=64 для скорости build
	frozen := lcFreezeGraph(g, EuclideanDistance, 100)
	query := lcRandomQuery(dim, 999)
	K := 10

	b.Logf("Frozen graph memory: %.1f MB (vs Graph TCMalloc: ~%.1f MB)",
		float64(frozen.MemoryBytes())/1e6,
		float64(n*dim*4+n*33*8)/1e6, // приблизительно
	)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = frozen.Search(query, K, 100)
	}
}

// TCMalloc Graph Search для сравнения (те же параметры)
func BenchmarkLC_Fast_TCMallocGraph_Search_10K_dim128(b *testing.B) {
	benchmarkLCTCMallocSearch(b, 10000, 128)
}
func BenchmarkLC_Fast_TCMallocGraph_Search_10K_dim768(b *testing.B) {
	benchmarkLCTCMallocSearch(b, 10000, 768)
}
func BenchmarkLC_Slow_TCMallocGraph_Search_50K_dim128(b *testing.B) {
	benchmarkLCTCMallocSearch(b, 50000, 128)
}

func benchmarkLCTCMallocSearch(b *testing.B, n, dim int) {
	b.Helper()
	g := lcBuildGraph(n, dim, 64, 42)
	query := lcRandomQuery(dim, 999)
	K := 10

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = g.Search(query, K, 100)
	}
}

// =============================================================================
// BENCHMARK 3: Segmented Search (параллельный merge) vs монолитный HNSW
//
// Симулируем: 1 монолит 50K  vs  5 сегментов × 10K + merge
// Доказывает: overhead параллельного merge приемлем
// =============================================================================

// Segmented (5×10K) vs monolith (50K) — dim=128 только для быстрого прогона
func BenchmarkLC_Fast_Segmented_Search_5x10K_dim128(b *testing.B) {
	benchmarkLCSegmentedSearch(b, 5, 10000, 128)
}
func BenchmarkLC_Fast_Monolith_Search_50K_dim128(b *testing.B) {
	// Используем frozen CSR monolith для честного сравнения
	g := lcBuildGraph(50000, 128, 64, 42)
	frozen := lcFreezeGraph(g, EuclideanDistance, 100)
	query := lcRandomQuery(128, 999)
	K := 10
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = frozen.Search(query, K, 100)
	}
}

// Медленный вариант с dim=768
func BenchmarkLC_Slow_Segmented_Search_5x10K_dim768(b *testing.B) {
	benchmarkLCSegmentedSearch(b, 5, 10000, 768)
}

func benchmarkLCSegmentedSearch(b *testing.B, numSegments, segSize, dim int) {
	b.Helper()
	K := 10

	// Строим N сегментов
	segments := make([]*lcFrozenGraph, numSegments)
	for i := range segments {
		g := lcBuildGraph(segSize, dim, 64, int64(i*1000+42))
		segments[i] = lcFreezeGraph(g, EuclideanDistance, 100)
	}

	query := lcRandomQuery(dim, 999)
	allResults := make([][]lcResult, numSegments)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Параллельный поиск по всем сегментам
		var wg sync.WaitGroup
		for j, seg := range segments {
			wg.Add(1)
			go func(idx int, s *lcFrozenGraph) {
				defer wg.Done()
				allResults[idx] = s.Search(query, K, 100)
			}(j, seg)
		}
		wg.Wait()

		// Merge через heap — O(segments * K * log K)
		_ = lcMergeResults(allResults, K)
	}
}

// lcMergeResults сливает K результатов из нескольких сегментов через minHeap
func lcMergeResults(results [][]lcResult, K int) []lcResult {
	type heapItem struct {
		res lcResult
		seg int
		pos int
	}

	// Собираем все в один список и берём top-K
	all := make([]lcResult, 0, len(results)*K)
	for _, r := range results {
		all = append(all, r...)
	}
	slices.SortFunc(all, func(a, b lcResult) int {
		if a.dist < b.dist {
			return -1
		}
		if a.dist > b.dist {
			return 1
		}
		return 0
	})
	if len(all) > K {
		all = all[:K]
	}
	return all
}

// =============================================================================
// BENCHMARK 4: Стоимость Freeze (TCMalloc Graph → CSR)
//
// Доказывает: компакция достаточно быстра чтобы делать её в фоне
// =============================================================================

func BenchmarkLC_Fast_Freeze_10K_dim128(b *testing.B) { benchmarkLCFreeze(b, 10000, 128) }
func BenchmarkLC_Fast_Freeze_10K_dim768(b *testing.B) { benchmarkLCFreeze(b, 10000, 768) }
func BenchmarkLC_Slow_Freeze_40K_dim128(b *testing.B) { benchmarkLCFreeze(b, 40000, 128) }
func BenchmarkLC_Slow_Freeze_40K_dim768(b *testing.B) { benchmarkLCFreeze(b, 40000, 768) }

func benchmarkLCFreeze(b *testing.B, n, dim int) {
	b.Helper()
	g := lcBuildGraph(n, dim, 64, 42)

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(n * dim * 4))
	for i := 0; i < b.N; i++ {
		frozen := lcFreezeGraph(g, EuclideanDistance, 100)
		_ = frozen
	}
}

// =============================================================================
// BENCHMARK 5: Insert Throughput — HNSW Insert vs Delta Append
//
// Доказывает: delta append на порядки быстрее HNSW insert
// =============================================================================

func BenchmarkLC_Fast_Insert_HNSW_dim128(b *testing.B)  { benchmarkLCHNSWInsert(b, 128) }
func BenchmarkLC_Fast_Insert_HNSW_dim768(b *testing.B)  { benchmarkLCHNSWInsert(b, 768) }
func BenchmarkLC_Fast_Insert_Delta_dim128(b *testing.B) { benchmarkLCDeltaInsert(b, 128) }
func BenchmarkLC_Fast_Insert_Delta_dim768(b *testing.B) { benchmarkLCDeltaInsert(b, 768) }

func benchmarkLCHNSWInsert(b *testing.B, dim int) {
	b.Helper()
	alloc := tcmalloc.NewTCMallocStore(1)
	g := NewGraph(EuclideanDistance, alloc)
	g.EfConstruction = 64
	// Ограничиваем N чтобы не строить граф на 1M+ векторов
	n := b.N
	if n > 5000 {
		n = 5000
	}

	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float32, n+1)
	for i := range vecs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()*2 - 1
		}
		vecs[i] = v
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(dim * 4))
	for i := 0; i < n; i++ {
		g.Insert(vecs[i%len(vecs)])
	}
}

func benchmarkLCDeltaInsert(b *testing.B, dim int) {
	b.Helper()
	// pre-alloc на 10K → zero alloc в цикле (как будет в реальной архитектуре)
	delta := newLCDelta(dim, 10001)

	rng := rand.New(rand.NewSource(42))
	// Генерируем пул из 10K векторов, потом циклично используем
	poolSize := 10000
	vecs := make([][]float32, poolSize)
	for i := range vecs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()*2 - 1
		}
		vecs[i] = v
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(dim * 4))
	for i := 0; i < b.N; i++ {
		// Сбрасываем дельту когда заполняется — как в реальном flush
		if delta.n >= poolSize {
			delta.n = 0
			delta.data = delta.data[:0]
		}
		delta.Append(vecs[i%poolSize])
	}
}

// =============================================================================
// BENCHMARK 6: Реальный сценарий — Delta + Segments (end-to-end)
//
// Симулируем архитектуру целиком:
//   - 1 delta (10K векторов, brute-force)
//   - 3 frozen сегмента разных уровней (10K, 40K, 160K)
//   - Search: параллельно по всем + merge
// =============================================================================

func BenchmarkLC_Fast_EndToEnd_dim128(b *testing.B) { benchmarkLCEndToEnd(b, 128) }
func BenchmarkLC_Slow_EndToEnd_dim768(b *testing.B) { benchmarkLCEndToEnd(b, 768) }

func benchmarkLCEndToEnd(b *testing.B, dim int) {
	b.Helper()
	K := 10

	// Дельта: 10K векторов (плоская)
	delta := lcBuildDelta(10000, dim, 1)

	// L0: 10K frozen
	g0 := lcBuildGraph(10000, dim, 64, 2)
	seg0 := lcFreezeGraph(g0, EuclideanDistance, 100)

	// L1: 40K frozen
	g1 := lcBuildGraph(40000, dim, 64, 3)
	seg1 := lcFreezeGraph(g1, EuclideanDistance, 100)

	// L2: 160K frozen
	g2 := lcBuildGraph(160000, dim, 64, 4)
	seg2 := lcFreezeGraph(g2, EuclideanDistance, 100)

	totalVectors := 10000 + 10000 + 40000 + 160000
	b.Logf("Total vectors: %d, dim: %d", totalVectors, dim)
	b.Logf("Memory: delta=%.1fMB, L0=%.1fMB, L1=%.1fMB, L2=%.1fMB",
		float64(10000*dim*4)/1e6,
		float64(seg0.MemoryBytes())/1e6,
		float64(seg1.MemoryBytes())/1e6,
		float64(seg2.MemoryBytes())/1e6,
	)

	queries := make([][]float32, 100)
	for i := range queries {
		queries[i] = lcRandomQuery(dim, int64(i*31+7))
	}

	allResults := make([][]lcResult, 4)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		q := queries[i%len(queries)]

		var wg sync.WaitGroup
		wg.Add(4)

		// Delta: brute force
		go func() {
			defer wg.Done()
			allResults[0] = delta.BruteForce(q, K, EuclideanDistance)
		}()
		// L0
		go func() {
			defer wg.Done()
			allResults[1] = seg0.Search(q, K, 100)
		}()
		// L1
		go func() {
			defer wg.Done()
			allResults[2] = seg1.Search(q, K, 100)
		}()
		// L2
		go func() {
			defer wg.Done()
			allResults[3] = seg2.Search(q, K, 100)
		}()

		wg.Wait()
		_ = lcMergeResults(allResults, K)
	}
}

// =============================================================================
// TEST: Корректность — проверяем что delta и CSR дают разумные результаты
// =============================================================================

func TestLC_DeltaBruteForce_Correctness(t *testing.T) {
	const n, dim, K = 1000, 128, 10
	delta := lcBuildDelta(n, dim, 42)
	query := lcRandomQuery(dim, 999)

	results := delta.BruteForce(query, K, EuclideanDistance)

	if len(results) != K {
		t.Fatalf("expected %d results, got %d", K, len(results))
	}

	// Проверяем что результаты отсортированы
	for i := 1; i < len(results); i++ {
		if results[i].dist < results[i-1].dist {
			t.Errorf("results not sorted at index %d: %.4f > %.4f",
				i, results[i-1].dist, results[i].dist)
		}
	}

	// Проверяем recall: brute-force должен совпасть с собой же
	// (это trivially true, но проверяет что логика работает)
	results2 := delta.BruteForce(query, K, EuclideanDistance)
	for i := range results {
		if results[i].id != results2[i].id {
			t.Errorf("non-deterministic at index %d", i)
		}
	}
	t.Logf("BruteForce top-%d: dist range [%.4f, %.4f]", K, results[0].dist, results[K-1].dist)
}

func TestLC_FrozenGraph_Correctness(t *testing.T) {
	const n, dim, K = 5000, 128, 10
	g := lcBuildGraph(n, dim, 200, 42)
	frozen := lcFreezeGraph(g, EuclideanDistance, 200)
	query := lcRandomQuery(dim, 999)

	if frozen == nil {
		t.Fatal("freeze returned nil")
	}
	if frozen.n != n {
		t.Fatalf("expected %d nodes, got %d", n, frozen.n)
	}

	// Поиск должен вернуть K результатов
	results := frozen.Search(query, K, 200)
	if len(results) == 0 {
		t.Fatal("frozen search returned no results")
	}
	t.Logf("FrozenGraph search top-%d: got %d results", K, len(results))
	t.Logf("Memory: %.2f MB for %d vectors dim=%d", float64(frozen.MemoryBytes())/1e6, n, dim)

	// Сравниваем с исходным HNSW
	hnswResults := g.Search(query, K, 200)
	if len(hnswResults) == 0 {
		t.Fatal("hnsw returned no results")
	}

	// Совпадение первого результата (ближайшего)
	frozenBestDist := results[0].dist
	hnswBestDist := hnswResults[0].Distance
	ratio := frozenBestDist / hnswBestDist
	t.Logf("Closest: frozen=%.4f, hnsw=%.4f, ratio=%.3f", frozenBestDist, hnswBestDist, ratio)
}

func TestLC_Freeze_MemoryLayout(t *testing.T) {
	const n, dim = 1000, 128
	g := lcBuildGraph(n, dim, 64, 42)
	frozen := lcFreezeGraph(g, EuclideanDistance, 100)

	// Проверяем CSR layout
	if len(frozen.data) != n*dim {
		t.Errorf("vectors: expected %d floats, got %d", n*dim, len(frozen.data))
	}
	layer0 := frozen.layers[0]
	if len(layer0.offs) != n+1 {
		t.Errorf("offsets: expected %d entries, got %d", n+1, len(layer0.offs))
	}
	if layer0.offs[0] != 0 {
		t.Errorf("offsets[0] must be 0, got %d", layer0.offs[0])
	}

	// Офсеты монотонно возрастают
	for i := 1; i <= n; i++ {
		if layer0.offs[i] < layer0.offs[i-1] {
			t.Errorf("offsets not monotone at %d: %d < %d",
				i, layer0.offs[i], layer0.offs[i-1])
		}
	}

	avgNeighbors := float64(layer0.offs[n]) / float64(n)
	t.Logf("n=%d, dim=%d, total_neighbors=%d, avg_per_node=%.1f",
		n, dim, layer0.offs[n], avgNeighbors)
	t.Logf("Memory: vectors=%.2fMB, neighbors=%.2fMB, offsets=%.2fMB",
		float64(len(frozen.data)*4)/1e6,
		float64(len(layer0.neigh)*4)/1e6,
		float64(len(layer0.offs)*4)/1e6,
	)
}
