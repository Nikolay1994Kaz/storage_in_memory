package vector

import (
	"slices"
	"sync"
	"sync/atomic"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// LeveledVectorStore — LSM-style векторное хранилище с Delta + Compaction.
//
// Архитектура:
//
//   Write Path (все размерности):
//     Add(key, vec) → DeltaSegment (O(1) append)
//     Когда delta.Full() → trigger background compaction
//
//   Read Path:
//     Search(q, K) → параллельно по delta + все сегменты уровней → merge top-K
//
//   Compaction (background, не блокирует reads):
//     L0: buildHNSW(deltaMax vectors) → [CSR если dim≤256]
//     L1: merge 4×L0 → buildHNSW(4*deltaMax) → [CSR если dim≤256]
//     L2: merge 4×L1 → buildHNSW(16*deltaMax) → ...
//
// Параметры по умолчанию:
//   deltaMax = min(10_000, 40MB / (dim * 4))
//   fanout   = 4
//   CSR threshold = dim ≤ 256
// =============================================================================

const (
	// csrDimThreshold — граница: dim ≤ N → freeze в CSR (+14% QPS),
	// dim > N → оставляем plain HNSW (distance compute доминирует).
	csrDimThreshold = 256

	// defaultFanout — сколько сегментов одного уровня сливается в следующий.
	defaultFanout = 4

	// maxLevels — максимальное число уровней LSM-иерархии.
	maxLevels = 8
)

// =============================================================================
// Segment interface — единица хранения (либо FrozenGraph, либо plain HNSW)
// =============================================================================

type segment interface {
	// Search выполняет поиск K ближайших с заданным efSearch.
	Search(query []float32, K, efSearch int) []segResult
	// Len возвращает число векторов в сегменте.
	Len() int
}

// segResult — универсальный тип результата поиска по сегменту.
type segResult struct {
	Key  string
	Dist float32
}

// -----------------------------------------------------------------------------
// frozenSegment — обёртка вокруг *FrozenGraph, реализует segment.
// -----------------------------------------------------------------------------

type frozenSegment struct {
	fg *FrozenGraph
}

func (s *frozenSegment) Len() int { return s.fg.Len() }

func (s *frozenSegment) Search(query []float32, K, efSearch int) []segResult {
	raw := s.fg.Search(query, K, efSearch)
	out := make([]segResult, len(raw))
	for i, r := range raw {
		out[i] = segResult{Key: r.Key, Dist: r.Dist}
	}
	return out
}

// -----------------------------------------------------------------------------
// hnswSegment — обёртка вокруг *Graph (plain HNSW), реализует segment.
// -----------------------------------------------------------------------------

type hnswSegment struct {
	g    *Graph
	keys map[uint64]string // internal_id → user_key
	mu   sync.RWMutex      // защита от чтения пока graаф строится
}

func (s *hnswSegment) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.g.nodeCount
}

func (s *hnswSegment) Search(query []float32, K, efSearch int) []segResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	raw := s.g.Search(query, K, efSearch)
	out := make([]segResult, 0, len(raw))
	for _, r := range raw {
		key, ok := s.keys[r.ID]
		if !ok || key == "" {
			continue
		}
		out = append(out, segResult{Key: key, Dist: r.Distance})
	}
	return out
}

// =============================================================================
// LeveledVectorStore
// =============================================================================

// LeveledConfig — параметры хранилища.
type LeveledConfig struct {
	// DeltaMax — максимальное число векторов в дельте до flush.
	// 0 = автоматически: min(10_000, 40MB / (dim * 4)).
	DeltaMax int

	// Fanout — кол-во сегментов одного уровня до merge в следующий.
	Fanout int

	// EfConstruction — ширина поиска при построении HNSW.
	EfConstruction int

	// EfSearch — ширина поиска при запросах.
	EfSearch int

	// Distance — функция расстояния.
	Distance DistanceFunc

	// Allocator — менеджер памяти для HNSW.
	Allocator *tcmalloc.TCMallocStore
}

// LeveledVectorStore — основное хранилище с leveled compaction.
type LeveledVectorStore struct {
	cfg LeveledConfig

	mu    sync.RWMutex
	delta *DeltaSegment
	dim   int // устанавливается при первой вставке

	// levels[i] — слой i, содержит []segment, упорядоченных по времени создания.
	// levels[0] — самые свежие малые сегменты (каждый ≈ deltaMax векторов).
	levels [maxLevels][]segment

	// compacting — атомарный флаг: идёт ли фоновая компакция.
	compacting atomic.Bool

	// compactCh — сигнальный канал для запуска фоновой компакции.
	compactCh chan struct{}

	// done — канал для остановки фонового воркера.
	done chan struct{}
}

// NewLeveledVectorStore создаёт хранилище и запускает фоновый compactor.
func NewLeveledVectorStore(cfg LeveledConfig) *LeveledVectorStore {
	if cfg.Fanout <= 0 {
		cfg.Fanout = defaultFanout
	}
	if cfg.EfConstruction <= 0 {
		cfg.EfConstruction = 200
	}
	if cfg.EfSearch <= 0 {
		cfg.EfSearch = 100
	}
	if cfg.Distance == nil {
		cfg.Distance = EuclideanDistance
	}
	if cfg.Allocator == nil {
		cfg.Allocator = tcmalloc.NewTCMallocStore(1)
	}

	lvs := &LeveledVectorStore{
		cfg:       cfg,
		compactCh: make(chan struct{}, 1),
		done:      make(chan struct{}),
	}

	// Фоновый compactor
	go lvs.compactWorker()

	return lvs
}

// deltaMax вычисляет максимальный размер дельты для заданной размерности.
// min(10_000, 40MB / (dim * 4 bytes))
func deltaMax(dim, cfgMax int) int {
	if cfgMax > 0 {
		return cfgMax
	}
	const maxBytes = 40 * 1024 * 1024
	bySize := maxBytes / (dim * 4)
	if bySize > 10_000 {
		return 10_000
	}
	return bySize
}

// Add добавляет вектор с заданным ключом.
// O(1) на горячем пути (append в flat slab).
func (lvs *LeveledVectorStore) Add(key string, vec []float32) error {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()

	// Инициализация при первой вставке
	if lvs.dim == 0 {
		lvs.dim = len(vec)
		max := deltaMax(lvs.dim, lvs.cfg.DeltaMax)
		lvs.delta = NewDeltaSegment(lvs.dim, max)
	} else if len(vec) != lvs.dim {
		return &dimMismatchError{expected: lvs.dim, got: len(vec)}
	}

	lvs.delta.Append(key, vec)

	// Если дельта заполнена — сигнализируем фоновому воркеру
	if lvs.delta.Full() {
		lvs.triggerCompact()
	}

	return nil
}

// Delete помечает ключ как удалённый.
func (lvs *LeveledVectorStore) Delete(key string) bool {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	if lvs.delta == nil {
		return false
	}
	return lvs.delta.Delete(key)
}

// FlushDelta принудительно флашит дельту (для тестов и graceful shutdown).
func (lvs *LeveledVectorStore) FlushDelta() {
	lvs.mu.Lock()
	if lvs.delta == nil || lvs.delta.Len() == 0 {
		lvs.mu.Unlock()
		return
	}
	lvs.triggerCompact()
	lvs.mu.Unlock()
}

// Search выполняет поиск по delta + всем сегментам уровней.
//
// Быстрые пути (0 горутин):
//   - Только delta: прямой BruteForce, без merge
//   - Один сегмент (+ пустая delta): прямой Search, без merge
//
// Параллельный путь: N сегментов + delta, горутины только при N > goroutineThreshold.
func (lvs *LeveledVectorStore) Search(query []float32, K int) ([]LeveledResult, error) {
	lvs.mu.RLock()

	if lvs.dim == 0 {
		lvs.mu.RUnlock()
		return nil, nil
	}
	if len(query) != lvs.dim {
		lvs.mu.RUnlock()
		return nil, &dimMismatchError{expected: lvs.dim, got: len(query)}
	}

	efSearch := lvs.cfg.EfSearch
	if efSearch < K {
		efSearch = K
	}

	// Считаем сегменты и delta под блокировкой
	nSegs := 0
	for i := range lvs.levels {
		nSegs += len(lvs.levels[i])
	}
	delta := lvs.delta
	hasDelta := delta != nil && delta.Len() > 0

	// Fast path 1: ничего нет — возвращаем nil
	if !hasDelta && nSegs == 0 {
		lvs.mu.RUnlock()
		return nil, nil
	}

	// Fast path 2: только delta, нет сегментов — BruteForce без горутин and merge
	if nSegs == 0 && hasDelta {
		lvs.mu.RUnlock()
		raw := delta.BruteForce(query, K, lvs.cfg.Distance)
		out := make([]LeveledResult, len(raw))
		for i, r := range raw {
			out[i] = LeveledResult{Key: r.key, Dist: r.dist}
		}
		return out, nil
	}

	// Fast path 3: один сегмент, delta пуста — прямой Search без merge
	if nSegs == 1 && !hasDelta {
		var seg segment
		for i := range lvs.levels {
			if len(lvs.levels[i]) > 0 {
				seg = lvs.levels[i][0]
				break
			}
		}
		lvs.mu.RUnlock()
		raw := seg.Search(query, K, efSearch) // K кандидатов — достаточно для single-segment
		if len(raw) > K {
			raw = raw[:K]
		}
		out := make([]LeveledResult, len(raw))
		for i, r := range raw {
			out[i] = LeveledResult{Key: r.Key, Dist: r.Dist}
		}
		return out, nil
	}

	// General path: N сегментов + delta.
	// Копируем указатели сегментов (дешёво: только указатели, ~8 байт на сегмент)
	segs := make([]segment, 0, nSegs)
	for i := range lvs.levels {
		segs = append(segs, lvs.levels[i]...)
	}
	lvs.mu.RUnlock()

	// Параллельный поиск: горутина на каждый источник
	nWorkers := nSegs
	if hasDelta {
		nWorkers++
	}

	// Плоский буфер результатов — без 2D среза
	type workerOut struct {
		res []segResult
	}
	outs := make([]workerOut, nWorkers)
	var wg sync.WaitGroup
	wg.Add(nWorkers)

	idx := 0
	if hasDelta {
		capturedIdx := idx
		go func() {
			defer wg.Done()
			raw := delta.BruteForce(query, K, lvs.cfg.Distance)
			out := make([]segResult, len(raw))
			for i, r := range raw {
				out[i] = segResult{Key: r.key, Dist: r.dist}
			}
			outs[capturedIdx].res = out
		}()
		idx++
	}
	for _, seg := range segs {
		capturedIdx, seg := idx, seg
		go func() {
			defer wg.Done()
			// Запрашиваем efSearch кандидатов — не K!
			// Merge из N сегментов требует достаточно вариантов для global top-K.
			outs[capturedIdx].res = seg.Search(query, efSearch, efSearch)
		}()
		idx++
	}
	wg.Wait()

	// Flat merge: собираем всё в один срез и берём top-K
	total := 0
	for i := range outs {
		total += len(outs[i].res)
	}
	combined := make([]LeveledResult, 0, total)
	for i := range outs {
		for _, r := range outs[i].res {
			combined = append(combined, LeveledResult{Key: r.Key, Dist: r.Dist})
		}
	}
	slices.SortFunc(combined, func(a, b LeveledResult) int {
		if a.Dist < b.Dist {
			return -1
		}
		if a.Dist > b.Dist {
			return 1
		}
		return 0
	})
	if len(combined) > K {
		combined = combined[:K]
	}
	return combined, nil
}

// Len возвращает общее число векторов (delta + все сегменты).
func (lvs *LeveledVectorStore) Len() int {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	total := 0
	if lvs.delta != nil {
		total += lvs.delta.Len()
	}
	for _, level := range lvs.levels {
		for _, seg := range level {
			total += seg.Len()
		}
	}
	return total
}

// Close останавливает фоновый воркер.
func (lvs *LeveledVectorStore) Close() {
	close(lvs.done)
}

// =============================================================================
// Внутренние методы
// =============================================================================

// collectSegments возвращает flat срез всех активных сегментов (под RLock).
func (lvs *LeveledVectorStore) collectSegments() []segment {
	var segs []segment
	for _, level := range lvs.levels {
		segs = append(segs, level...)
	}
	return segs
}

// triggerCompact посылает сигнал в non-blocking канал.
// Вызывать под write lock.
func (lvs *LeveledVectorStore) triggerCompact() {
	select {
	case lvs.compactCh <- struct{}{}:
	default: // уже есть сигнал, не блокируем
	}
}

// compactWorker — фоновая горутина, слушает compactCh и выполняет compaction.
func (lvs *LeveledVectorStore) compactWorker() {
	for {
		select {
		case <-lvs.done:
			return
		case <-lvs.compactCh:
			if lvs.compacting.CompareAndSwap(false, true) {
				lvs.runCompaction()
				lvs.compacting.Store(false)
			}
		}
	}
}

// runCompaction выполняет один полный цикл compaction:
//  1. Flush delta → новый L0 сегмент
//  2. Если L0 переполнен → merge L0 → L1
//  3. Если L1 переполнен → merge L1 → L2
//  4. ...и т.д. по уровням
func (lvs *LeveledVectorStore) runCompaction() {
	// 1. Flush delta
	newSeg := lvs.flushDeltaToSegment()
	if newSeg == nil {
		return
	}

	// 2. Добавляем сегмент в L0
	lvs.mu.Lock()
	lvs.levels[0] = append(lvs.levels[0], newSeg)
	lvs.mu.Unlock()

	// 3. Каскадное слияние уровней
	fanout := lvs.cfg.Fanout
	for lyr := 0; lyr < maxLevels-1; lyr++ {
		lvs.mu.RLock()
		nSegs := len(lvs.levels[lyr])
		lvs.mu.RUnlock()

		if nSegs < fanout {
			break // этот уровень ещё не переполнен
		}

		// Забираем все сегменты с уровня lyr
		lvs.mu.Lock()
		toMerge := lvs.levels[lyr]
		lvs.levels[lyr] = nil
		lvs.mu.Unlock()

		// Строим merge-сегмент (вне lock!)
		merged := lvs.mergeSegments(toMerge)
		if merged == nil {
			continue
		}

		// Ставим на следующий уровень
		lvs.mu.Lock()
		lvs.levels[lyr+1] = append(lvs.levels[lyr+1], merged)
		lvs.mu.Unlock()
	}
}

// flushDeltaToSegment атомарно забирает текущую дельту и строит из неё сегмент.
// Дельта заменяется пустой — запись продолжается без блокировки.
func (lvs *LeveledVectorStore) flushDeltaToSegment() segment {
	lvs.mu.Lock()
	if lvs.delta == nil || lvs.delta.Len() == 0 {
		lvs.mu.Unlock()
		return nil
	}
	// Атомарная замена: старая дельта уходит на compaction, новая пустая
	oldDelta := lvs.delta
	max := deltaMax(lvs.dim, lvs.cfg.DeltaMax)
	lvs.delta = NewDeltaSegment(lvs.dim, max)
	dim := lvs.dim
	lvs.mu.Unlock()

	// Всё дальше — вне lock. Запись идёт в новую дельту.
	entries := oldDelta.ExtractAll()
	if len(entries) == 0 {
		return nil
	}

	return lvs.buildSegment(entries, dim)
}

// mergeSegments сливает несколько сегментов в один новый.
// Извлекает векторы из каждого сегмента и строит новый HNSW с нуля.
func (lvs *LeveledVectorStore) mergeSegments(segs []segment) segment {
	dim := 0
	lvs.mu.RLock()
	dim = lvs.dim
	lvs.mu.RUnlock()

	if dim == 0 {
		return nil
	}

	// Извлекаем все векторы из всех сегментов
	var entries []DeltaEntry
	for _, seg := range segs {
		switch s := seg.(type) {
		case *frozenSegment:
			// Извлекаем из CSR data-слайса
			fg := s.fg
			for i := 0; i < fg.n; i++ {
				if fg.keys[i] == "" {
					continue
				}
				vec := fg.data[i*fg.dim : (i+1)*fg.dim]
				// Копируем — fg.data может уйти из памяти после merge
				vecCopy := make([]float32, dim)
				copy(vecCopy, vec)
				entries = append(entries, DeltaEntry{Key: fg.keys[i], Vec: vecCopy})
			}
		case *hnswSegment:
			// Извлекаем из arena
			s.mu.RLock()
			g := s.g
			for i := range g.nodes {
				if !g.nodes[i].Alive {
					continue
				}
				key, ok := s.keys[uint64(i)]
				if !ok || key == "" {
					continue
				}
				raw := g.arena.Get(g.nodes[i].VectorOffset)
				vecCopy := make([]float32, dim)
				copy(vecCopy, raw)
				entries = append(entries, DeltaEntry{Key: key, Vec: vecCopy})
			}
			s.mu.RUnlock()
		}
	}

	if len(entries) == 0 {
		return nil
	}

	return lvs.buildSegment(entries, dim)
}

// buildSegment строит сегмент из набора (key, vec) записей.
// Dim-adaptive: dim ≤ csrDimThreshold → FrozenGraph, иначе → hnswSegment.
func (lvs *LeveledVectorStore) buildSegment(entries []DeltaEntry, dim int) segment {
	n := len(entries)
	if n == 0 {
		return nil
	}

	// Строим HNSW из всех векторов.
	// Используем общий аллокатор из конфига — НЕ создаём новый per-segment,
	// иначе каждый NewTCMallocStore запускает фоновую горутину runDeferredFree.
	g := NewGraph(lvs.cfg.Distance, lvs.cfg.Allocator)
	g.EfConstruction = lvs.cfg.EfConstruction

	keys := make(map[uint64]string, n)
	for i, e := range entries {
		idx := g.Insert(e.Vec)
		keys[uint64(idx)] = e.Key
		_ = i
	}

	// Dim-adaptive: freeze в CSR или оставить как HNSW
	if dim <= csrDimThreshold {
		fg := FreezeGraph(g, lvs.cfg.Distance, keys)
		if fg == nil {
			return nil
		}
		return &frozenSegment{fg: fg}
	}

	return &hnswSegment{g: g, keys: keys}
}

// =============================================================================
// Merge результатов
// =============================================================================

// LeveledResult — результат поиска из LeveledVectorStore.
type LeveledResult struct {
	Key  string
	Dist float32
}

// mergeTopK сливает K результатов из нескольких источников (используется только в general пати).
func mergeTopK(src []segResult, K int) []LeveledResult {
	if len(src) == 0 {
		return nil
	}
	slices.SortFunc(src, func(a, b segResult) int {
		if a.Dist < b.Dist {
			return -1
		}
		if a.Dist > b.Dist {
			return 1
		}
		return 0
	})
	if len(src) > K {
		src = src[:K]
	}
	out := make([]LeveledResult, len(src))
	for i, r := range src {
		out[i] = LeveledResult{Key: r.Key, Dist: r.Dist}
	}
	return out
}

// =============================================================================
// Утилиты
// =============================================================================

type dimMismatchError struct {
	expected, got int
}

func (e *dimMismatchError) Error() string {
	return "vector dimension mismatch: expected " +
		itoa(e.expected) + ", got " + itoa(e.got)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := 20
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
