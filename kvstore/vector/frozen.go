package vector

import (
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"sync"
	"unsafe"
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
//
//	data[i*dim : (i+1)*dim]  = вектор ноды i  (contiguous float32 slab)
//	layers[lyr].neigh[layers[lyr].offs[i] : layers[lyr].offs[i+1]] = соседи ноды i на layer lyr (flat CSR)
//
// =============================================================================

// frozenSearchState — pool-able буфер для Search (как searchPool в graph.go).
type frozenSearchState struct {
	visited []uint64         // bitset, размер (n+63)/64
	cands   []frozenCandItem // minHeap кандидатов
	res     []FrozenResult   // maxHeap результатов
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
//
// neigh — uint32 (4 байта/сосед). Узлы в FrozenGraph нумеруются локально 0..n-1.
// uint32 снимает прежний потолок n ≤ 65535 (uint16): теперь frozen-сегмент любого
// размера корректен. Цена: +2 байта/сосед (~+11% памяти сегмента при dim=128,
// ~+2% при dim=768 — топология мала относительно векторов). Без этого merge >65535
// молча портил граф (overflow ID соседей) или падал на медленный hnswSegment.
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

	// Ключи (userKey) для каждого внутреннего ID.
	// Интернированы в blob+offs (см. internedKeys) — ноль указателей для GC.
	// Пустой ключ (offs равны) = нода удалена (tombstone).
	keys internedKeys

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
		keys:         buildInternedKeys(nodeKeys),
		entryPointID: uint32(g.entryPointID),
		maxLevel:     g.maxLevel,
		n:            n,
		dim:          dim,
		distFn:       distFn,
	}
}

// Len возвращает число нод в графе.
func (fg *FrozenGraph) Len() int { return fg.n }

// bruteRange — точный top-K по непрерывному диапазону [start,end) frozen-раскладки
// (нижний режим #7: blocked-brute по блоку тенанта). Перебирается только диапазон —
// чужие векторы не трогаются. filterFn != nil → применяется (tombstones/тенант).
// Ключи — zero-copy views в blob; клон делается на внешней границе search().
func (fg *FrozenGraph) bruteRange(query []float32, K, start, end int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	if K <= 0 || start >= end {
		return dst[:0]
	}
	top := dst[:0]
	for i := start; i < end; i++ {
		key := fg.keys.view(i)
		if key == "" {
			continue
		}
		if filterFn != nil && !filterFn(key) {
			continue
		}
		d := fg.distFn(query, fg.data[i*fg.dim:(i+1)*fg.dim])
		top = insertTopK(top, K, key, d)
	}
	return top
}

// viewKey — zero-copy ключ вектора i (для frozenFilterable).
func (fg *FrozenGraph) viewKey(i int) string { return fg.keys.view(i) }

// bruteRangeAttr — как bruteRange, но фильтр по frozen-индексу (predIdx(i) bool)
// вместо строкового ключа (рычаг #2: атрибуты по uint-колонкам, без строк).
// predIdx == nil → без фильтра.
func (fg *FrozenGraph) bruteRangeAttr(query []float32, K, start, end int, dst []FrozenResult, predIdx func(int) bool) []FrozenResult {
	if K <= 0 || start >= end {
		return dst[:0]
	}
	top := dst[:0]
	for i := start; i < end; i++ {
		if predIdx != nil && !predIdx(i) {
			continue
		}
		key := fg.keys.view(i)
		if key == "" {
			continue
		}
		d := fg.distFn(query, fg.data[i*fg.dim:(i+1)*fg.dim])
		top = insertTopK(top, K, key, d)
	}
	return top
}

// residualBruteBudget — residual-brute ВЫКЛЮЧЕН для float32-сегментов: на high-dim
// несжатый перебор неконкурентен обходу графа (дистанция в 4× дороже SQ8-ADC; в
// замерах dbpedia-1536 float32 brute давал ~1.0× везде). Низко-размерные блоки и так
// классифицируются Kind=brute (dim-aware порог высок) и сюда не попадают.
func (fg *FrozenGraph) residualBruteBudget(efSearch int) int { return 0 }

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
//
// filterFn != nil: фильтр применяется к РЕЗУЛЬТАТАМ, не к обходу графа.
// Это критически важно для корректности — узлы-мосты не отсекаются,
// граф-связность сохраняется. Только в result-heap кладём те, что прошли фильтр.
func (fg *FrozenGraph) Search(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool) []FrozenResult {
	return fg.searchWithIdx(query, K, efSearch, dst, filterFn, nil)
}

// searchWithIdx — тело Search с дополнительным фильтром по frozen-индексу узла
// (node id = frozen id). predIdx применяется к результатам наравне с filterFn
// (логическое И). Используется SearchFilter для атрибут-фильтрации по uint-колонкам
// (рычаг #2) и для ограничения graph-обхода диапазоном блока тенанта. nil → без idx-фильтра.
func (fg *FrozenGraph) searchWithIdx(query []float32, K, efSearch int, dst []FrozenResult, filterFn func(string) bool, predIdx func(int) bool) []FrozenResult {
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
					ep = uint32(nb)
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

	// Стартуем с entry point: всегда в cands для обхода.
	// В результаты — только если проходит фильтр.
	epBase := int(ep) * dim
	epDist := distFn(query, fg.data[epBase:epBase+dim])
	setVisited(ep)
	cPush(ep, epDist)
	// view() — zero-copy, валиден на время поиска (fg жив). Клонируем при выдаче.
	epKey := fg.keys.view(int(ep))
	if (filterFn == nil || (epKey != "" && filterFn(epKey))) && (predIdx == nil || predIdx(int(ep))) {
		rPush(epKey, epDist)
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
			d := distFn(query, fg.data[nbBase:nbBase+dim])
			if len(st.res) < efSearch || d < st.res[0].Dist {
				// Всегда добавляем в кандидаты (граф-обход не фильтруем).
				cPush(nb, d)
				// В результаты — только если проходит фильтр.
				nbKey := fg.keys.view(int(nb))
				if (filterFn == nil || (nbKey != "" && filterFn(nbKey))) && (predIdx == nil || predIdx(int(nb))) {
					rPush(nbKey, d)
					if len(st.res) > efSearch {
						rPopMax()
					}
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

	if cap(dst) < topK {
		dst = make([]FrozenResult, topK)
	} else {
		dst = dst[:topK]
	}
	// st.res[i].Key — zero-copy view в blob (валиден, пока жив fg). Отдаём views
	// БЕЗ клонирования: семантика времени жизни та же, что была у []string.
	// Клонирование — ОБЯЗАННОСТЬ внешней границы (leveled search), где клонируются
	// только K выживших после merge, а не efSearch×N кандидатов на каждый сегмент.
	copy(dst, st.res[:topK])

	// Возвращаем состояние в pool
	frozenSearchPool.Put(st)
	return dst
}

// =============================================================================
// Сериализация FrozenGraph — O(N) memcpy через unsafe.Slice.
//
// Формат (little-endian):
//   [4B n][4B dim][4B maxLevel][4B entryPointID]
//   [n*dim*4B data float32 slab]
//   [4B nLayers]
//   для каждого слоя:
//     [4B nOffs][nOffs*4B offs uint32][4B nNeigh][nNeigh*4B neigh uint32]
//   [4B nKeys]
//   для каждого ключа:
//     [2B keyLen][keyLen B key bytes]
//
// =============================================================================

// WriteGraphTo сериализует FrozenGraph в w. Zero-alloc для data/offs/neigh слайсов
// (используется unsafe.Slice → прямой доступ к памяти без копии в []byte).
func (fg *FrozenGraph) WriteGraphTo(w io.Writer) error {
	// Заголовок: n, dim, maxLevel, entryPointID
	var hdr [16]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(fg.n))
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(fg.dim))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(fg.maxLevel))
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(fg.entryPointID))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("frozen: write header: %w", err)
	}

	// Data slab (float32 → bytes, zero-alloc)
	if fg.n > 0 && fg.dim > 0 {
		b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.data[0])), len(fg.data)*4)
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("frozen: write data: %w", err)
		}
	}

	// Layers
	var u4 [4]byte
	binary.LittleEndian.PutUint32(u4[:], uint32(len(fg.layers)))
	if _, err := w.Write(u4[:]); err != nil {
		return fmt.Errorf("frozen: write nLayers: %w", err)
	}
	for i, layer := range fg.layers {
		// offs
		binary.LittleEndian.PutUint32(u4[:], uint32(len(layer.offs)))
		if _, err := w.Write(u4[:]); err != nil {
			return fmt.Errorf("frozen: write nOffs[%d]: %w", i, err)
		}
		if len(layer.offs) > 0 {
			b := unsafe.Slice((*byte)(unsafe.Pointer(&layer.offs[0])), len(layer.offs)*4)
			if _, err := w.Write(b); err != nil {
				return fmt.Errorf("frozen: write offs[%d]: %w", i, err)
			}
		}
		// neigh
		binary.LittleEndian.PutUint32(u4[:], uint32(len(layer.neigh)))
		if _, err := w.Write(u4[:]); err != nil {
			return fmt.Errorf("frozen: write nNeigh[%d]: %w", i, err)
		}
		if len(layer.neigh) > 0 {
			b := unsafe.Slice((*byte)(unsafe.Pointer(&layer.neigh[0])), len(layer.neigh)*4)
			if _, err := w.Write(b); err != nil {
				return fmt.Errorf("frozen: write neigh[%d]: %w", i, err)
			}
		}
	}

	// Keys: [4B nKeys] + для каждого [2B len][bytes]
	binary.LittleEndian.PutUint32(u4[:], uint32(fg.n))
	if _, err := w.Write(u4[:]); err != nil {
		return fmt.Errorf("frozen: write nKeys: %w", err)
	}
	var klen [2]byte
	for i := 0; i < fg.n; i++ {
		key := fg.keys.view(i) // read-only, пишется сразу — view безопасен
		binary.LittleEndian.PutUint16(klen[:], uint16(len(key)))
		if _, err := w.Write(klen[:]); err != nil {
			return fmt.Errorf("frozen: write keyLen[%d]: %w", i, err)
		}
		if len(key) > 0 {
			// io.WriteString избегает []byte(key) alloc для *bufio.Writer
			if _, err := io.WriteString(w, key); err != nil {
				return fmt.Errorf("frozen: write key[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// ReadFrozenGraph десериализует FrozenGraph из r.
// distFn должна совпадать с той, что использовалась при сохранении.
func ReadFrozenGraph(r io.Reader, distFn DistanceFunc) (*FrozenGraph, error) {
	// Заголовок
	var hdr [16]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("frozen: read header: %w", err)
	}
	n := int(binary.LittleEndian.Uint32(hdr[0:4]))
	dim := int(binary.LittleEndian.Uint32(hdr[4:8]))
	maxLevel := int(binary.LittleEndian.Uint32(hdr[8:12]))
	entryPointID := uint32(binary.LittleEndian.Uint32(hdr[12:16]))

	fg := &FrozenGraph{
		n:            n,
		dim:          dim,
		maxLevel:     maxLevel,
		entryPointID: entryPointID,
		distFn:       distFn,
	}

	// Data slab
	if n > 0 && dim > 0 {
		fg.data = make([]float32, n*dim)
		b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.data[0])), len(fg.data)*4)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, fmt.Errorf("frozen: read data: %w", err)
		}
	}

	// Layers
	var u4 [4]byte
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return nil, fmt.Errorf("frozen: read nLayers: %w", err)
	}
	nLayers := int(binary.LittleEndian.Uint32(u4[:]))
	fg.layers = make([]FlatLayer, nLayers)
	for i := range fg.layers {
		// offs
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return nil, fmt.Errorf("frozen: read nOffs[%d]: %w", i, err)
		}
		nOffs := int(binary.LittleEndian.Uint32(u4[:]))
		if nOffs > 0 {
			fg.layers[i].offs = make([]uint32, nOffs)
			b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.layers[i].offs[0])), nOffs*4)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, fmt.Errorf("frozen: read offs[%d]: %w", i, err)
			}
		}
		// neigh
		if _, err := io.ReadFull(r, u4[:]); err != nil {
			return nil, fmt.Errorf("frozen: read nNeigh[%d]: %w", i, err)
		}
		nNeigh := int(binary.LittleEndian.Uint32(u4[:]))
		if nNeigh > 0 {
			fg.layers[i].neigh = make([]uint32, nNeigh)
			b := unsafe.Slice((*byte)(unsafe.Pointer(&fg.layers[i].neigh[0])), nNeigh*4)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, fmt.Errorf("frozen: read neigh[%d]: %w", i, err)
			}
		}
	}

	// Keys
	if _, err := io.ReadFull(r, u4[:]); err != nil {
		return nil, fmt.Errorf("frozen: read nKeys: %w", err)
	}
	nKeys := int(binary.LittleEndian.Uint32(u4[:]))
	offs := make([]uint32, nKeys+1)
	var blob []byte
	var klen [2]byte
	for i := 0; i < nKeys; i++ {
		if _, err := io.ReadFull(r, klen[:]); err != nil {
			return nil, fmt.Errorf("frozen: read keyLen[%d]: %w", i, err)
		}
		kl := int(binary.LittleEndian.Uint16(klen[:]))
		if kl > 0 {
			// читаем байты ключа прямо в blob — без per-key string() аллокации
			blob = slices.Grow(blob, kl)
			start := len(blob)
			blob = blob[:start+kl]
			if _, err := io.ReadFull(r, blob[start:]); err != nil {
				return nil, fmt.Errorf("frozen: read key[%d]: %w", i, err)
			}
		}
		offs[i+1] = uint32(len(blob))
	}
	fg.keys = internedKeys{blob: blob, offs: offs}

	return fg, nil
}
