package tcmalloc

import (
	"sync"
	"sync/atomic"
)

const (
	chunkSize = 1 * 1024 * 1024 // 1MB
)

// spanRegistry — append-only реестр span'ов с lock-free чтением.
//
// Проблема: []Span — это slice (pointer + len + cap).
// append() может переаллоцировать backing array.
// Если Resolve() читает старый backing array, а AllocSpan()
// уже создал новый — DATA RACE.
//
// Решение: atomic.Pointer на slice. При append создаём
// новый slice, копируем, atomic swap. Старый slice
// живёт пока все readers не уйдут (GC).
type spanRegistry struct {
	spans atomic.Pointer[[]*Span]
	count atomic.Uint32
}

func newSpanRegistry() *spanRegistry {
	r := &spanRegistry{}
	initial := make([]*Span, 0, 1024)
	r.spans.Store(&initial)
	return r
}

// append добавляет span в реестр. ВЫЗЫВАТЬ ПОД MUTEX.
func (r *spanRegistry) append(s *Span) {
	old := *r.spans.Load()
	newSlice := make([]*Span, len(old)+1, cap(old)+1)
	copy(newSlice, old)
	newSlice[len(old)] = s
	r.spans.Store(&newSlice)
	r.count.Store(uint32(len(newSlice)))
}

// get возвращает span по ID. LOCK-FREE.
func (r *spanRegistry) get(spanID uint32) *Span {
	spans := *r.spans.Load()
	if int(spanID) >= len(spans) {
		return nil
	}
	return spans[spanID]
}

// len возвращает количество span'ов.
func (r *spanRegistry) len() int {
	return int(r.count.Load())
}

// MHeap — глобальный аллокатор памяти.
//
// Аналог runtime.mheap_ в Go runtime.
//
// Два вида данных:
//
//	chunks — большие блоки памяти (1MB), из них нарезаются span'ы
//	spans  — реестр всех span'ов (append-only, lock-free чтение)
type MHeap struct {
	mu     sync.Mutex
	chunks [][]byte
	offset int

	// ─── Span Registry ───
	//
	// atomic.Pointer обеспечивает lock-free чтение для Resolve/GetSpan.
	// Запись (append) всегда под mu.
	registry *spanRegistry

	// ─── Large Object Pool ───
	//
	// Пул освобождённых large span'ов для переиспользования.
	// Без этого пула AllocLarge ВСЕГДА выделяет новую память,
	// а освобождённые large span'ы копятся мёртвым грузом → OOM.
	//
	// При FreeLarge() — span возвращается сюда.
	// При AllocLarge() — ищем best-fit из пула перед аллокацией.
	largeFree []*Span
}

func NewMHeap() *MHeap {
	h := &MHeap{
		chunks:    make([][]byte, 0, 16),
		registry:  newSpanRegistry(),
		largeFree: make([]*Span, 0, 16),
	}
	h.chunks = append(h.chunks, make([]byte, chunkSize))
	h.offset = 0
	return h
}

// AllocSpan аллоцирует span для указанного size class.
// Регистрирует span в реестре и присваивает spanID.
//
// central — MCentral, которому будет принадлежать span (для back-reference).
// Может быть nil для large objects.
func (h *MHeap) AllocSpan(sizeClass int, central *MCentral) *Span {
	elemSize := sizeClasses[sizeClass]
	numObjects := objectsPerSpan[sizeClass]
	spanSize := elemSize * numObjects

	h.mu.Lock()
	defer h.mu.Unlock()

	// Проверяем место в текущем chunk'е
	if h.offset+spanSize > chunkSize {
		h.chunks = append(h.chunks, make([]byte, chunkSize))
		h.offset = 0
	}

	chunkIdx := len(h.chunks) - 1
	data := h.chunks[chunkIdx][h.offset : h.offset+spanSize]
	h.offset += spanSize

	// Создаём span с back-reference на central
	s := NewSpan(data, elemSize, sizeClass, central)

	// Регистрируем в реестре (под mu, потокобезопасно)
	s.spanID = uint32(h.registry.len())
	h.registry.append(s)

	return s
}

// GetSpan возвращает span по его ID.
//
// LOCK-FREE: spans — append-only, atomic.Pointer swap.
func (h *MHeap) GetSpan(spanID uint32) *Span {
	return h.registry.get(spanID)
}

// Resolve читает данные объекта по Handle.
//
// LOCK-FREE: вызывается на каждый GET.
// Путь: Handle → spanID → span → data[objIndex * elemSize]
func (h *MHeap) Resolve(handle Handle) []byte {
	s := h.registry.get(handle.SpanID())
	idx := handle.ObjIndex()
	offset := idx * s.elemSize
	return s.data[offset : offset+s.elemSize]
}

// AllocLarge аллоцирует блок для "больших" объектов (> 4KB).
//
// Порядок:
//  1. Ищем в largeFree пуле best-fit span (elemSize >= size, минимальный)
//  2. Не нашли → выделяем новую память из chunk'а
func (h *MHeap) AllocLarge(size int) ([]byte, Handle) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// ─── Путь 1: переиспользование из largeFree (best-fit) ───
	bestIdx := -1
	bestSize := int(^uint(0) >> 1) // max int
	for i, s := range h.largeFree {
		if s.elemSize >= size && s.elemSize < bestSize {
			bestIdx = i
			bestSize = s.elemSize
		}
	}
	if bestIdx >= 0 {
		s := h.largeFree[bestIdx]
		// Удаляем из пула (swap с последним, O(1))
		h.largeFree[bestIdx] = h.largeFree[len(h.largeFree)-1]
		h.largeFree = h.largeFree[:len(h.largeFree)-1]
		// Сбрасываем состояние для переиспользования
		s.allocIndex = 1
		s.freeStack = s.freeStack[:0]
		return s.data[:size], MakeHandle(s.spanID, 0)
	}

	// ─── Путь 2: аллокация новой памяти ───
	if h.offset+size > chunkSize {
		if size > chunkSize {
			newChunk := make([]byte, size)
			h.chunks = append(h.chunks, newChunk)
			s := &Span{
				data:       newChunk,
				elemSize:   size,
				sizeClass:  -1, // маркер large object
				capacity:   1,
				allocIndex: 1,
			}
			s.spanID = uint32(h.registry.len())
			h.registry.append(s)
			return newChunk, MakeHandle(s.spanID, 0)
		}

		h.chunks = append(h.chunks, make([]byte, chunkSize))
		h.offset = 0
	}

	data := h.chunks[len(h.chunks)-1][h.offset : h.offset+size]
	s := &Span{
		data:       data,
		elemSize:   size,
		sizeClass:  -1, // маркер large object
		capacity:   1,
		allocIndex: 1,
	}
	s.spanID = uint32(h.registry.len())
	h.registry.append(s)
	h.offset += size

	return data, MakeHandle(s.spanID, 0)
}

// FreeLarge возвращает large span в пул для переиспользования.
//
// Вызывается из MCache.Free() когда span.sizeClass == -1.
// Span не удаляется из registry (он append-only), но его data
// будет переиспользован при следующем AllocLarge.
func (h *MHeap) FreeLarge(s *Span) {
	h.mu.Lock()
	s.allocIndex = 0
	s.freeStack = s.freeStack[:0]
	h.largeFree = append(h.largeFree, s)
	h.mu.Unlock()
}

// Stats возвращает статистику аллокатора.
func (h *MHeap) Stats() (numChunks int, totalBytes int, usedBytes int, numSpans int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	numChunks = len(h.chunks)
	totalBytes = numChunks * chunkSize
	usedBytes = (numChunks-1)*chunkSize + h.offset
	numSpans = h.registry.len()
	return
}
