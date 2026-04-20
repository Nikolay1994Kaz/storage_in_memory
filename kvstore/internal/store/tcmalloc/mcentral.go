package tcmalloc

import (
	"sync"
)

// MCentral — центральный распределитель span'ов для одного size class.
//
// Аналог runtime.mcentral в Go runtime.
//
// Логика:
//
//	mcache просит span → MCentral отдаёт partial (есть место)
//	mcache возвращает полный span → MCentral складывает в full
//	mcache возвращает partial span → складывает обратно в partial
//
// MUTEX: один sync.Mutex per MCentral.
// Поскольку у нас 8 size classes → 8 отдельных mutex'ов.
// Это НАМНОГО лучше чем один глобальный mutex:
//   - worker пишущий 64B не блокирует worker пишущий 256B
//   - contention падает в 8 раз по сравнению с глобальным lock'ом
type MCentral struct {
	mu        sync.Mutex
	sizeClass int

	// partial — span'ы, в которых есть свободные объекты.
	// mcache берёт span'ы отсюда.
	partial []*Span

	// full — span'ы, в которых ВСЕ объекты заняты.
	// Когда в span освобождается объект — он переезжает обратно в partial.
	full []*Span

	// heap — глобальный аллокатор для создания новых span'ов.
	heap *MHeap

	// Статистика
	totalSpansAllocated int
}

// NewMCentral создаёт центральный распределитель для указанного size class.
func NewMCentral(sizeClass int, heap *MHeap) *MCentral {
	return &MCentral{
		sizeClass: sizeClass,
		partial:   make([]*Span, 0, 8),
		full:      make([]*Span, 0, 8),
		heap:      heap,
	}
}

// GetSpan отдаёт span с свободными объектами для mcache.
//
// Порядок:
//  1. Есть partial span? → отдаём его (дёшево, переиспользование)
//  2. Нет partial? → аллоцируем НОВЫЙ span из mheap (дорого, редко)
//
// MUTEX: берёт c.mu на время операции.
// Но mcache вызывает GetSpan РЕДКО — только когда его текущий
// span полностью заполнен (каждые 64-128 аллокаций).
func (c *MCentral) GetSpan() *Span {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Путь 1: забираем существующий partial span
	if n := len(c.partial); n > 0 {
		// Берём последний (O(1), без сдвига массива)
		s := c.partial[n-1]
		c.partial = c.partial[:n-1]
		return s
	}

	// Путь 2: partial пуст — просим новый span у mheap
	c.totalSpansAllocated++
	return c.heap.AllocSpan(c.sizeClass)
}

// ReturnSpan возвращает span обратно в mcentral.
//
// Вызывается из mcache когда:
//   - span полон → кладём в full
//   - worker завершается → возвращаем текущий span (partial или full)
func (c *MCentral) ReturnSpan(s *Span) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if s.IsFull() {
		c.full = append(c.full, s)
	} else {
		c.partial = append(c.partial, s)
	}
}

// ReturnToPartial перемещает span из full в partial.
//
// Вызывается когда в «полном» span'е освободился объект.
// В нашей упрощённой модели Free() всегда идёт через mcache,
// который владеет span'ом, поэтому этот метод — для корректности:
// когда mcache отдаёт span назад после Free().
func (c *MCentral) ReturnToPartial(s *Span) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Убираем из full (если он там есть)
	for i, fs := range c.full {
		if fs == s {
			c.full[i] = c.full[len(c.full)-1]
			c.full = c.full[:len(c.full)-1]
			break
		}
	}

	// Добавляем в partial
	c.partial = append(c.partial, s)
}

// Stats возвращает статистику mcentral.
func (c *MCentral) Stats() (partialCount, fullCount, totalAllocated int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.partial), len(c.full), c.totalSpansAllocated
}
