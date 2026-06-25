package vector

import (
	"math"
	"slices"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// DeltaSegment — буфер вставки (Write Path) с инкрементальным HNSW-индексом.
//
// Все Add-запросы попадают сюда первым делом. Хранение — плоский slab (data),
// он остаётся источником истины для flush (ExtractAll). ДОПОЛНИТЕЛЬНО каждый
// вектор инкрементально вставляется в живой HNSW-граф `idx` (модель Qdrant):
// поиск по дельте идёт через idx.Search() за log(n), а не брутфорсом O(n).
// Это снимает просадку QPS во время записи при крупной дельте.
//
// idx — только для поиска: при flush он отбрасывается (Close), сегмент строится
// из slab. (Будущая оптимизация P5: freeze idx напрямую вместо rebuild.)
//
// Когда буфер заполняется (delta.Full()), фоновый compactor забирает
// все векторы (ExtractAll), строит HNSW, замораживает в CSR (если dim≤256)
// и ставит готовый сегмент в levels[0].
// =============================================================================

// DeltaEntry — ключ + вектор в дельте.
type DeltaEntry struct {
	Key string
	Vec []float32 // ссылка в data-слайсе, не копия
}

// DeltaSegment — буфер вставки. Лок снаружи (LeveledVectorStore.mu):
// Append/Delete — под Lock, Search — под RLock. idx безопасен для конкурентных
// Search, т.к. Insert/Delete сериализованы внешним Lock.
type DeltaSegment struct {
	data    []float32      // flat: data[i*dim : (i+1)*dim] = вектор i (источник истины для flush)
	keys    []string       // keys[i] = ключ вектора i
	keyIdx  map[string]int // ключ → индекс в data (для быстрого upsert/delete)
	dim     int
	maxSize int // максимальное кол-во векторов до flush

	// Инкрементальный HNSW-индекс для поиска (Qdrant-style). Отбрасывается на flush.
	idx     *Graph
	alloc   *tcmalloc.TCMallocStore
	distFn  DistanceFunc
	nodeKey map[uint32]string // id узла графа → ключ (маппинг результатов поиска)
	keyNode map[string]uint32 // ключ → текущий id узла (для удаления при upsert/delete)
}

// NewDeltaSegment создаёт дельту с заданной размерностью и ёмкостью.
// distFn нужна для инкрементального HNSW-индекса поиска; m/efC — параметры графа
// (0 = дефолты NewGraph). m/efC задают, чтобы граф дельты совпадал с сегментами
// — тогда freeze-on-flush даёт эквивалентный сегмент.
func NewDeltaSegment(dim, maxSize int, distFn DistanceFunc, m, efC int) *DeltaSegment {
	alloc := tcmalloc.NewTCMallocStore(1)
	g := NewGraph(distFn, alloc)
	if efC > 0 {
		g.EfConstruction = efC
	}
	if m > 0 {
		g.M = m
		g.M0 = 2 * m
		g.Ml = 1.0 / math.Log(float64(m))
		g.pruneBufItems = make([]item, 0, g.M0+1)
		g.pruneBufIDs = make([]uint64, 0, g.M0+1)
		g.insertBuf = make([]uint64, 0, g.M0+1)
	}
	return &DeltaSegment{
		data:    make([]float32, 0, maxSize*dim),
		keys:    make([]string, 0, maxSize),
		keyIdx:  make(map[string]int, maxSize),
		dim:     dim,
		maxSize: maxSize,
		idx:     g,
		alloc:   alloc,
		distFn:  distFn,
		nodeKey: make(map[uint32]string, maxSize),
		keyNode: make(map[string]uint32, maxSize),
	}
}

// Close освобождает аллокатор HNSW-индекса (останавливает его goroutine).
// Вызывать после swap дельты, когда она больше не используется для поиска
// И граф больше не нужен (freeze скопировал данные в Go-память, либо rebuild завершён).
func (d *DeltaSegment) Close() {
	if d.alloc != nil {
		d.alloc.Close()
		d.alloc = nil
	}
}

// Graph возвращает HNSW-граф дельты (для freeze-on-flush: заморозка без rebuild).
func (d *DeltaSegment) Graph() *Graph { return d.idx }

// FreezeKeys возвращает map[node_id]→key для FreezeGraph/FreezeGraphSQ.
func (d *DeltaSegment) FreezeKeys() map[uint64]string {
	m := make(map[uint64]string, len(d.nodeKey))
	for gid, key := range d.nodeKey {
		m[uint64(gid)] = key
	}
	return m
}

// Len возвращает число ЖИВЫХ векторов в дельте (без tombstones).
// keyIdx содержит только живые ключи (Delete удаляет из map), а keys[] —
// все слоты включая tombstoned (пустые строки). Поэтому считаем по keyIdx.
func (d *DeltaSegment) Len() int { return len(d.keyIdx) }

// Full возвращает true, если дельта заполнена и нужен flush.
// Считаем по числу занятых слотов (len(keys)), а не живых — tombstone оставляет
// слот занятым до compaction, поэтому Full корректно отражает заполненность памяти.
func (d *DeltaSegment) Full() bool { return len(d.keys) >= d.maxSize }

// Append добавляет или обновляет вектор по ключу.
// O(1) — append в slice + map write. Не аллоцирует вектор (копирует во flat slab).
func (d *DeltaSegment) Append(key string, vec []float32) {
	if slot, exists := d.keyIdx[key]; exists {
		// Upsert: перезаписываем slab in-place
		copy(d.data[slot*d.dim:(slot+1)*d.dim], vec)
		// В графе: удаляем старый узел, вставляем новый.
		if oldGid, ok := d.keyNode[key]; ok {
			d.idx.Delete(uint64(oldGid))
			delete(d.nodeKey, oldGid)
		}
		gid := d.idx.Insert(vec)
		d.keyNode[key] = gid
		d.nodeKey[gid] = key
		return
	}
	slot := len(d.keys)
	d.data = append(d.data, vec...)
	d.keys = append(d.keys, key)
	d.keyIdx[key] = slot
	gid := d.idx.Insert(vec)
	d.keyNode[key] = gid
	d.nodeKey[gid] = key
}

// Delete помечает вектор как удалённый (tombstone — пустая строка ключа).
// Не освобождает память — compaction уберёт при следующем flush.
func (d *DeltaSegment) Delete(key string) bool {
	idx, exists := d.keyIdx[key]
	if !exists {
		return false
	}
	delete(d.keyIdx, key)
	d.keys[idx] = "" // tombstone
	// Убираем из HNSW-индекса.
	if gid, ok := d.keyNode[key]; ok {
		d.idx.Delete(uint64(gid))
		delete(d.nodeKey, gid)
		delete(d.keyNode, key)
	}
	return true
}

// Search ищет K ближайших через инкрементальный HNSW-индекс (за log(n)).
// filterFn применяется к РЕЗУЛЬТАТАМ (как в frozen-сегментах), не к обходу графа.
// Вызывать под RLock (idx не мутируется конкурентно — Append/Delete под Lock).
func (d *DeltaSegment) Search(query []float32, K, efSearch int, filterFn func(string) bool) []deltaResult {
	if d.idx == nil || len(d.keyNode) == 0 {
		return nil
	}
	raw := d.idx.Search(query, K, efSearch)
	out := make([]deltaResult, 0, len(raw))
	for _, r := range raw {
		key, ok := d.nodeKey[uint32(r.ID)]
		if !ok {
			continue // узел удалён — пропускаем
		}
		if filterFn != nil && !filterFn(key) {
			continue
		}
		out = append(out, deltaResult{key: key, dist: r.Distance})
	}
	return out
}

// BruteForce выполняет линейный поиск K ближайших в дельте.
// filterFn != nil — включается только векторы, для которых filterFn(key) == true.
// 1 alloc/op (сам heap, он же output) — быстрее чем pool+copy.
func (d *DeltaSegment) BruteForce(query []float32, K int, distFn DistanceFunc, filterFn func(string) bool) []deltaResult {
	n := len(d.keys)
	if n == 0 {
		return nil
	}

	// maxHeap размером K — один alloc, он же выходной срез (без дополнительного copy)
	heap := make([]deltaResult, 0, K+1)

	heapPush := func(r deltaResult) {
		heap = append(heap, r)
		i := len(heap) - 1
		for i > 0 {
			p := (i - 1) / 2
			if heap[p].dist >= heap[i].dist {
				break
			}
			heap[p], heap[i] = heap[i], heap[p]
			i = p
		}
	}
	heapPop := func() {
		l := len(heap) - 1
		heap[0] = heap[l]
		heap = heap[:l]
		i := 0
		for {
			ll, r, s := 2*i+1, 2*i+2, i
			if ll < len(heap) && heap[ll].dist > heap[s].dist {
				s = ll
			}
			if r < len(heap) && heap[r].dist > heap[s].dist {
				s = r
			}
			if s == i {
				break
			}
			heap[i], heap[s] = heap[s], heap[i]
			i = s
		}
	}

	dim := d.dim
	for i := 0; i < n; i++ {
		if d.keys[i] == "" {
			continue // tombstone
		}
		if filterFn != nil && !filterFn(d.keys[i]) {
			continue // не проходит фильтр
		}
		vec := d.data[i*dim : (i+1)*dim]
		dist := distFn(query, vec)
		if len(heap) < K {
			heapPush(deltaResult{key: d.keys[i], dist: dist})
		} else if dist < heap[0].dist {
			heapPop()
			heapPush(deltaResult{key: d.keys[i], dist: dist})
		}
	}

	// Сортируем и возвращаем heap напрямую (нет лишнего copy)
	slices.SortFunc(heap, func(a, b deltaResult) int {
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

// ExtractAll возвращает все живые записи (key, vec-slice) для compaction.
// Возвращает срезы на внутренний data-буфер — не копирует.
// Вызывать только когда дельта больше не используется для записи.
func (d *DeltaSegment) ExtractAll() []DeltaEntry {
	out := make([]DeltaEntry, 0, len(d.keys))
	for i, key := range d.keys {
		if key == "" {
			continue // tombstone
		}
		out = append(out, DeltaEntry{
			Key: key,
			Vec: d.data[i*d.dim : (i+1)*d.dim],
		})
	}
	return out
}

// deltaResult — внутренний тип результата BruteForce.
type deltaResult struct {
	key  string
	dist float32
}
