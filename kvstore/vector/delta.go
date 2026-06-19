package vector

import "slices"

// =============================================================================
// DeltaSegment — плоский буфер вставки (Write Path).
//
// Все Add-запросы попадают сюда первым делом.
// Никаких графов, никаких pointer-chase — чистый O(1) append.
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

// DeltaSegment — immutable после создания структура (lock снаружи в LeveledVectorStore).
type DeltaSegment struct {
	data    []float32      // flat: data[i*dim : (i+1)*dim] = вектор i
	keys    []string       // keys[i] = ключ вектора i
	keyIdx  map[string]int // ключ → индекс в data (для быстрого upsert/delete)
	dim     int
	maxSize int // максимальное кол-во векторов до flush
}

// NewDeltaSegment создаёт дельту с заданной размерностью и ёмкостью.
func NewDeltaSegment(dim, maxSize int) *DeltaSegment {
	return &DeltaSegment{
		data:    make([]float32, 0, maxSize*dim),
		keys:    make([]string, 0, maxSize),
		keyIdx:  make(map[string]int, maxSize),
		dim:     dim,
		maxSize: maxSize,
	}
}

// Len возвращает число векторов в дельте.
func (d *DeltaSegment) Len() int { return len(d.keys) }

// Full возвращает true, если дельта заполнена и нужен flush.
func (d *DeltaSegment) Full() bool { return len(d.keys) >= d.maxSize }

// Append добавляет или обновляет вектор по ключу.
// O(1) — append в slice + map write. Не аллоцирует вектор (копирует во flat slab).
func (d *DeltaSegment) Append(key string, vec []float32) {
	if idx, exists := d.keyIdx[key]; exists {
		// Upsert: перезаписываем in-place
		copy(d.data[idx*d.dim:(idx+1)*d.dim], vec)
		return
	}
	idx := len(d.keys)
	d.data = append(d.data, vec...)
	d.keys = append(d.keys, key)
	d.keyIdx[key] = idx
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
	return true
}

// BruteForce выполняет линейный поиск K ближайших в дельте.
// 1 alloc/op (сам heap, он же output) — быстрее чем pool+copy.
func (d *DeltaSegment) BruteForce(query []float32, K int, distFn DistanceFunc) []deltaResult {
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
