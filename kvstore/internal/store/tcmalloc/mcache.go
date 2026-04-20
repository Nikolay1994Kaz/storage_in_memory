package tcmalloc

// MCache — per-worker кеш аллокатора.
//
// Аналог runtime.mcache в Go runtime.
// Каждый epoll worker получает свой MCache.
//
// Аллокация из MCache — БЕЗ БЛОКИРОВОК:
//
//	span.Alloc() = bump pointer или pop из freeStack.
//	Это буквально одна-две инструкции CPU.
//
// Блокировка появляется ТОЛЬКО когда span заполнен
// и нужно получить новый из MCentral.
// При span capacity=64, это происходит каждые 64 аллокации.
//
// Формула эффективности:
//
//	ArenaStore:    1 mutex lock на КАЖДУЮ операцию
//	TCMallocStore: 1 mutex lock на КАЖДЫЕ 64 операции
//	Выигрыш: 64x меньше contention
type MCache struct {
	// Один активный span на каждый size class.
	// alloc[0] = span для 32B объектов
	// alloc[1] = span для 64B объектов
	// ...
	alloc [numSizeClasses]*Span

	// Указатели на центральные пулы (для refill).
	// НЕ копируем, только ссылки.
	centrals [numSizeClasses]*MCentral

	// Статистика для бенчмарков
	AllocCount  uint64 // сколько объектов выделено
	RefillCount uint64 // сколько раз ходили в mcentral
}

// NewMCache создаёт per-worker кеш.
//
// centrals — массив указателей на MCentral (по одному на size class).
// Все workers разделяют одни и те же MCentral'ы, но каждый
// имеет свой MCache с отдельными span'ами.
func NewMCache(centrals [numSizeClasses]*MCentral) *MCache {
	return &MCache{
		centrals: centrals,
	}
}

// Alloc выделяет блок памяти нужного размера.
//
// Возвращает:
//   - buf []byte — указатель на выделенный блок (НЕ копия, прямой доступ в span)
//   - handle Handle — идентификатор для index (spanID + objIndex)
//
// Горячий путь (99% вызовов): span.Alloc() → 0 locks, ~5ns
// Холодный путь (1% вызовов):  refill из MCentral → 1 mutex, ~100ns
//
// ZERO LOCKS на горячем пути!
func (c *MCache) Alloc(size int) ([]byte, Handle) {
	sc := SizeClassForSize(size)
	if sc < 0 {
		// Large object (> 4KB) — идёт напрямую в mheap
		// через mcentral.heap (для простоты прокидываем)
		return c.allocLarge(size)
	}

	s := c.alloc[sc]

	// ─── Горячий путь: span есть и не полон ───
	if s != nil {
		if buf, idx := s.Alloc(); buf != nil {
			c.AllocCount++
			return buf, MakeHandle(s.spanID, idx)
		}

		// Span полон → возвращаем в MCentral как full
		c.centrals[sc].ReturnSpan(s)
		c.alloc[sc] = nil
	}

	// ─── Холодный путь: нужен новый span из MCentral ───
	c.RefillCount++
	s = c.centrals[sc].GetSpan()
	c.alloc[sc] = s

	buf, idx := s.Alloc()
	c.AllocCount++
	return buf, MakeHandle(s.spanID, idx)
}

// Free освобождает объект по его Handle.
//
// Находит span по spanID, вызывает span.Free(objIndex).
//
// Важно: в нашей модели Free может вызывать ЛЮБОЙ worker
// (не обязательно тот, кто аллоцировал). Это нормально:
//   - Если span принадлежит нашему mcache → lock-free
//   - Если span принадлежит другому mcache → тоже ок, потому что
//     span.Free() — это просто append в freeStack
//
// В Go runtime это сложнее (GC), у нас — проще.
func (c *MCache) Free(heap *MHeap, handle Handle) {
	s := heap.GetSpan(handle.SpanID())
	if s == nil {
		return
	}
	s.Free(handle.ObjIndex())
}

// allocLarge — аллокация больших объектов через mheap.
func (c *MCache) allocLarge(size int) ([]byte, Handle) {
	// Для large objects используем mheap напрямую
	// (через central[0].heap — все centrals ведут к одному heap)
	buf, handle := c.centrals[0].heap.AllocLarge(size)
	c.AllocCount++
	return buf, handle
}

// Flush возвращает все span'ы обратно в MCentral.
//
// Вызывается когда worker завершается или при graceful shutdown.
// Без этого span'ы «застрянут» в mcache и не будут переиспользованы.
func (c *MCache) Flush() {
	for sc := 0; sc < numSizeClasses; sc++ {
		if s := c.alloc[sc]; s != nil {
			c.centrals[sc].ReturnSpan(s)
			c.alloc[sc] = nil
		}
	}
}
