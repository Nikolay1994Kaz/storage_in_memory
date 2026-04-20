package tcmalloc

import (
	"sync"
	"sync/atomic"
)

const (
	chunkSize = 1 * 1024 * 1024 // 1MB
)

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
	// Зачем? Чтобы по Handle за O(1) найти span.
	// Это аналог runtime.mheap_.allspans в Go runtime.
	//
	// Append-only: span'ы никогда не удаляются.
	// Чтение безопасно без лока: мы читаем только индексы < spanCount.
	// Запись под mu (AllocSpan уже держит mu).
	spans     []*Span
	spanCount atomic.Uint32
}

func NewMHeap() *MHeap {
	h := &MHeap{
		chunks: make([][]byte, 0, 16),
		spans:  make([]*Span, 0, 1024),
	}
	h.chunks = append(h.chunks, make([]byte, chunkSize))
	h.offset = 0
	return h
}

// AllocSpan аллоцирует span для указанного size class.
// Регистрирует span в реестре и присваивает spanID.
func (h *MHeap) AllocSpan(sizeClass int) *Span {
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

	// Создаём span
	s := NewSpan(data, elemSize)

	// Регистрируем в реестре (под mu, потокобезопасно)
	s.spanID = uint32(len(h.spans))
	h.spans = append(h.spans, s)
	h.spanCount.Store(uint32(len(h.spans)))

	return s
}

// GetSpan возвращает span по его ID.
//
// LOCK-FREE: spans — append-only, элементы не меняются.
// spanCount загружается атомарно.
func (h *MHeap) GetSpan(spanID uint32) *Span {
	if spanID >= h.spanCount.Load() {
		return nil
	}
	return h.spans[spanID]
}

// Resolve читает данные объекта по Handle.
//
// LOCK-FREE: вызывается на каждый GET.
// Путь: Handle → spanID → span → data[objIndex * elemSize]
func (h *MHeap) Resolve(handle Handle) []byte {
	s := h.spans[handle.SpanID()]
	idx := handle.ObjIndex()
	offset := idx * s.elemSize
	return s.data[offset : offset+s.elemSize]
}

// AllocLarge аллоцирует блок для "больших" объектов (> 4KB).
func (h *MHeap) AllocLarge(size int) ([]byte, Handle) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.offset+size > chunkSize {
		if size > chunkSize {
			newChunk := make([]byte, size)
			h.chunks = append(h.chunks, newChunk)
			// Для large objects создаём special span
			s := &Span{
				data:       newChunk,
				elemSize:   size,
				capacity:   1,
				allocIndex: 1,
			}
			s.spanID = uint32(len(h.spans))
			h.spans = append(h.spans, s)
			h.spanCount.Store(uint32(len(h.spans)))
			return newChunk, MakeHandle(s.spanID, 0)
		}

		h.chunks = append(h.chunks, make([]byte, chunkSize))
		h.offset = 0
	}

	chunkIdx := len(h.chunks) - 1
	_ = chunkIdx
	data := h.chunks[len(h.chunks)-1][h.offset : h.offset+size]
	s := &Span{
		data:       data,
		elemSize:   size,
		capacity:   1,
		allocIndex: 1,
	}
	s.spanID = uint32(len(h.spans))
	h.spans = append(h.spans, s)
	h.spanCount.Store(uint32(len(h.spans)))
	h.offset += size

	return data, MakeHandle(s.spanID, 0)
}

// Stats возвращает статистику аллокатора.
func (h *MHeap) Stats() (numChunks int, totalBytes int, usedBytes int, numSpans int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	numChunks = len(h.chunks)
	totalBytes = numChunks * chunkSize
	usedBytes = (numChunks-1)*chunkSize + h.offset
	numSpans = len(h.spans)
	return
}
