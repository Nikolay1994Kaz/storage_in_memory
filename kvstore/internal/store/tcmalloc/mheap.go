package tcmalloc

import (
	"sync"
	"sync/atomic"
)

const (
	chunkSize = 1 * 1024 * 1024 // 1MB
)

// ─── Span Registry: Chunked Array ───────────────────────────
//
// Двухуровневый индекс для append-only реестра span'ов.
//
// Проблема COW-slice: при каждом append копируем ВСЕ элементы
// в новый backing array → O(n) на append, O(n²) суммарно.
//
// Решение: Chunked Array (как Go runtime для mspan арены).
//
//   Level 1 (L1): [chunk0_ptr | chunk1_ptr | ...]   — atomic.Pointer, COW только здесь
//   Level 2 (L2): chunk0 = [span0, span1, ... span1023]  — фиксированный массив, НИКОГДА не перемещается
//
// Lookup: spanID → chunks[spanID >> 10][spanID & 0x3FF]
//
// Характеристики:
//   append: O(1) — запись в слот + (раз в 1024) COW на L1 (десятки указателей)
//   get:    O(1) — два array access, LOCK-FREE
//   copy:   ZERO для L2 chunks (они аллоцируются один раз и живут вечно)
//
// Сравнение с COW-slice для 100K span'ов:
//   COW-slice:    ~5 млрд копирований указателей, 100K аллокаций backing array
//   Chunked:      ~9600 копирований указателей, 98 аллокаций chunks

const (
	regChunkBits = 10                // 1024 span'ов на chunk
	regChunkSize = 1 << regChunkBits // 1024
	regChunkMask = regChunkSize - 1  // 0x3FF
)

// spanChunk — фиксированный массив указателей на span'ы.
// Аллоцируется ОДИН раз при создании и никогда не перемещается.
// Это гарантирует, что lock-free readers всегда видят валидный указатель.
type spanChunk [regChunkSize]*Span

type spanRegistry struct {
	chunks atomic.Pointer[[]*spanChunk] // L1: массив указателей на chunks
	count  atomic.Uint32
}

func newSpanRegistry() *spanRegistry {
	r := &spanRegistry{}
	initial := make([]*spanChunk, 0, 16)
	r.chunks.Store(&initial)
	return r
}

// append добавляет span в реестр. ВЫЗЫВАТЬ ПОД MUTEX.
//
// O(1) — запись в слот текущего chunk'а.
// Раз в regChunkSize (1024) вызовов — аллоцируем новый chunk
// и COW на L1 массиве (копируем только указатели на chunks, не сами span'ы).
func (r *spanRegistry) append(s *Span) {
	id := r.count.Load()
	ci := id >> regChunkBits // chunk index
	si := id & regChunkMask  // slot index внутри chunk'а

	chunks := *r.chunks.Load()

	if int(ci) >= len(chunks) {
		// Нужен новый L2 chunk.
		// COW на L1: копируем только массив указателей (десятки элементов, не тысячи).
		newChunk := new(spanChunk)
		newL1 := make([]*spanChunk, len(chunks)+1)
		copy(newL1, chunks)
		newL1[ci] = newChunk
		r.chunks.Store(&newL1)
		chunks = newL1
	}

	chunks[ci][si] = s
	r.count.Store(id + 1)
}

// get возвращает span по ID. LOCK-FREE.
//
// Два array access: chunks[ci][si].
// Оба массива иммутабельны с точки зрения readers:
//
//	L1 — подменяется через atomic.Pointer (старый живёт пока readers не уйдут)
//	L2 — никогда не перемещается (фиксированный массив)
func (r *spanRegistry) get(spanID uint32) *Span {
	chunks := *r.chunks.Load()
	ci := spanID >> regChunkBits
	if int(ci) >= len(chunks) {
		return nil
	}
	return chunks[ci][spanID&regChunkMask]
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
	mu        sync.Mutex
	chunks    [][]byte
	offset    int
	usedBytes atomic.Int64
	maxMemory int64

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
	h.registry.append(nil) // Reserve spanID = 0 to avoid zero-value Handle collision
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
	h.usedBytes.Add(int64(spanSize))

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
	if s == nil {
		return nil
	}
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
		// НЕ добавляем к usedBytes — эта память уже была учтена при первой аллокации
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
			h.usedBytes.Add(int64(size))
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
	h.usedBytes.Add(int64(size))

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
	// Вычитаем из usedBytes ПОД MUTEX.
	// Если вычитать после Unlock — другой worker может успеть
	// взять span из largeFree через AllocLarge (который НЕ добавляет
	// к usedBytes для recycled span'ов) → usedBytes временно завышен → ложный OOM.
	h.usedBytes.Add(-int64(s.elemSize))
	h.mu.Unlock()
}

// ══════════════════════════════════════════════════
// MEMORY LIMITS
// ══════════════════════════════════════════════════

// UsedMemory возвращает текущее потребление памяти (в байтах).
//
// Атомарное чтение (atomic.Int64.Load) — одна CPU-инструкция.
// Можно вызывать на каждый SET без overhead'а.
func (h *MHeap) UsedMemory() int64 {
	return h.usedBytes.Load()
}

// SetMaxMemory устанавливает лимит памяти в байтах.
// 0 = без лимита (по умолчанию).
func (h *MHeap) SetMaxMemory(bytes int64) {
	h.maxMemory = bytes
}

// MaxMemory возвращает установленный лимит памяти в байтах.
func (h *MHeap) MaxMemory() int64 {
	return h.maxMemory
}

// IsOOM проверяет, превышен ли лимит памяти.
// Если maxMemory == 0 — лимита нет, всегда false.
//
// Вызывается перед каждым SET в обработчике команд.
// Стоимость: один atomic Load (~1ns).
func (h *MHeap) IsOOM() bool {
	max := h.maxMemory
	if max == 0 {
		return false
	}
	return h.usedBytes.Load() >= max
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
