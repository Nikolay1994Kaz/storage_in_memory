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
// Устанавливает span.state = spanInCache перед выдачей.
// Это гарантирует, что Free() не будет брать mutex на freeStack
// пока span в mcache (lock-free путь).
//
// MUTEX: берёт c.mu на время операции.
// Но mcache вызывает GetSpan РЕДКО — только когда его текущий
// span полностью заполнен (каждые 64-1024 аллокации).
func (c *MCentral) GetSpan() *Span {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Путь 1: забираем существующий partial span
	if n := len(c.partial); n > 0 {
		// Берём последний (O(1), без сдвига массива)
		s := c.partial[n-1]
		c.partial = c.partial[:n-1]
		s.state = spanInCache // передаём владение mcache
		return s
	}

	// Путь 2: partial пуст — просим новый span у mheap
	c.totalSpansAllocated++
	s := c.heap.AllocSpan(c.sizeClass, c)
	s.state = spanInCache // сразу отдаём в mcache
	return s
}

// ReturnSpan возвращает span обратно в mcentral.
//
// Устанавливает span.state = spanInCentral.
// После этого Free() на этом span'е будет брать mutex.
//
// Вызывается из mcache когда:
//   - span полон → кладём в full
//   - worker завершается → возвращаем текущий span (partial или full)
func (c *MCentral) ReturnSpan(s *Span) {
	c.mu.Lock()
	defer c.mu.Unlock()

	s.state = spanInCentral // теперь span принадлежит central

	if s.IsFull() {
		c.full = append(c.full, s)
		s.inFull.Store(true)
	} else {
		c.partial = append(c.partial, s)
		s.inFull.Store(false)
	}
}

// notifyMaybeFull перемещает span из full в partial, если в нём освободился
// объект (через remote-free очередь Span.Free).
//
// Вызывается ТОЛЬКО когда s.inFull == true (гейт в Span.Free), т.е. спан
// действительно лежит в c.full и никто им не владеет. Это исключает опасный
// сценарий двойного владения (append живого spanInCache-спана в partial):
// активный у воркера спан имеет inFull == false и сюда не попадает.
//
// Идемпотентна: если спан уже кем-то перемещён/забран — просто чистит флаг.
func (c *MCentral) notifyMaybeFull(s *Span) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, fs := range c.full {
		if fs == s {
			c.full[i] = c.full[len(c.full)-1]
			c.full = c.full[:len(c.full)-1]
			c.partial = append(c.partial, s)
			s.inFull.Store(false)
			return
		}
	}
	// Уже не в full (забран GetSpan / перемещён) — синхронизируем флаг.
	s.inFull.Store(false)
}

// Stats возвращает статистику mcentral.
func (c *MCentral) Stats() (partialCount, fullCount, totalAllocated int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.partial), len(c.full), c.totalSpansAllocated
}
