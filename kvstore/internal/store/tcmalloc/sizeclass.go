// Package tcmalloc — кастомный аллокатор памяти по модели TCMalloc / Go Runtime.
//
// Иерархия:
//
//	mcache  → per-worker, lock-free
//	mcentral → per-size-class, mutex
//	mheap   → global, mutex, chunks по 1MB
//
// Вдохновлено:
//   - Go runtime: src/runtime/malloc.go, sizeclasses.go, mcache.go
//   - Google TCMalloc: https://google.github.io/tcmalloc/design.html
package tcmalloc

import "sync"

// ─── Size Classes ───────────────────────────────────────────
//
// Каждый аллоцируемый объект попадает в ближайший size class.
// Go runtime использует 67 классов (от 8B до 32KB).
// Мы упрощаем до 8 классов для KV-хранилища.
//
// Формула: каждый следующий класс = предыдущий × 2 (степени двойки).
// Это упрощает вычисление класса: class = bits.Len(size-1) - 4

const numSizeClasses = 8

// sizeClasses[i] = размер объекта в байтах для класса i.
//
//	Class 0:   32B  — короткие ключи ("cnt", "ok")
//	Class 1:   64B  — средние KV ("user:123" → "active")
//	Class 2:  128B  — JSON-поля, маленькие документы
//	Class 3:  256B  — средние документы
//	Class 4:  512B  — большие значения
//	Class 5: 1024B  — крупные записи
//	Class 6: 2048B  — очень большие объекты
//	Class 7: 4096B  — максимальный span-класс (одна страница ОС)
var sizeClasses = [numSizeClasses]int{
	32, 64, 128, 256, 512, 1024, 2048, 4096,
}

// objectsPerSpan[i] = сколько объектов помещается в один span для класса i.
//
// КЛЮЧЕВОЙ параметр производительности!
//
// Чем больше объектов в span — тем РЕЖЕ mcache ходит в mcentral за новым span.
// Каждый поход в mcentral = mutex lock. Наша цель — минимизировать refills.
//
// Go runtime использует span'ы по 8KB-32KB (сотни объектов).
// Мы подбираем span size ≈ 32KB-64KB:
//
//	32B  × 1024 = 32KB  → 1 refill на 1024 аллокации
//	64B  ×  512 = 32KB  → 1 refill на 512 аллокаций
//	128B ×  256 = 32KB  → 1 refill на 256 аллокаций
//	256B ×  128 = 32KB  → 1 refill на 128 аллокаций
//	512B ×   64 = 32KB  → 1 refill на 64 аллокации
//	1KB  ×   32 = 32KB  → 1 refill на 32 аллокации
//	2KB  ×   16 = 32KB  → 1 refill на 16 аллокаций
//	4KB  ×    8 = 32KB  → 1 refill на 8 аллокаций
var objectsPerSpan = [numSizeClasses]int{
	1024, 512, 256, 128, 64, 32, 16, 8,
}

// SizeClassForSize возвращает индекс size class для заданного размера.
//
// Пример: SizeClassForSize(21) → 0 (32B), потому что 32 ≥ 21.
// Пример: SizeClassForSize(65) → 2 (128B), потому что 128 ≥ 65.
//
// Если size > 4096 → возвращает -1 (large object, аллоцируется напрямую из mheap).
func SizeClassForSize(size int) int {
	for i, sc := range sizeClasses {
		if size <= sc {
			return i
		}
	}
	return -1 // large object
}

// ─── Span State ─────────────────────────────────────────────
//
// Span может находиться в двух состояниях:
//
//	spanInCache   — span принадлежит конкретному mcache.
//	                Alloc/Free вызываются ОДНИМ worker'ом → lock-free.
//
//	spanInCentral — span лежит в MCentral (в partial или full).
//	                Free может вызвать ЛЮБОЙ worker → нужен mutex.
//	                При Free, если span был в full → ReturnToPartial.
const (
	spanInCache   uint32 = 0
	spanInCentral uint32 = 1
)

// ─── Span ───────────────────────────────────────────────────
//
// Span — непрерывный блок памяти, разбитый на N объектов одного size class.
//
// Аналог runtime.mspan в Go runtime.
// Каждый span принадлежит ОДНОМУ mcache (→ lock-free доступ),
// или лежит в mcentral (→ mutex-protected).
//
// Жизненный цикл span:
//
//	mheap.AllocSpan() → mcentral (partial list) → mcache.alloc[class]
//	                                              ↕ (alloc/free)
//	                  ← mcentral (full list)    ← mcache (когда span полон)
//
// Ownership tracking:
//
//	state == spanInCache   → принадлежит mcache, lock-free доступ
//	state == spanInCentral → лежит в mcentral, Free() берёт mu
//
//	При Free() + state==spanInCentral:
//	  1. Берём s.mu (защита freeStack от параллельных Free)
//	  2. Если span был полон → вызываем central.ReturnToPartial(s)
type Span struct {
	// Память, из которой нарезаются объекты.
	// Это slice из chunk'а mheap (НЕ отдельная аллокация).
	data []byte

	// Метаданные
	elemSize  int // размер одного объекта (= sizeClasses[sizeClass])
	capacity  int // максимальное количество объектов в span
	sizeClass int // индекс size class (для back-reference на MCentral)

	// ─── Аллокатор: bump pointer + freelist ───
	//
	// Два пути выделения памяти:
	//
	// 1. Bump pointer (быстрый путь):
	//    Объекты 0..allocIndex-1 уже были выделены хотя бы раз.
	//    Следующий новый объект: data[allocIndex * elemSize]
	//    allocIndex++ → готово. Это ОДНА инструкция CPU.
	//
	// 2. Free stack (путь переиспользования):
	//    Когда объект освобождается (Free), его индекс
	//    помещается в freeStack. При следующем Alloc
	//    мы сначала проверяем freeStack — если не пуст,
	//    берём оттуда (переиспользуем память).
	//
	// Порядок при Alloc:
	//    freeStack не пуст? → pop   (переиспользование)
	//    allocIndex < cap?  → bump  (новый объект)
	//    Иначе → span полон, нужен новый из mcentral.

	allocIndex int   // bump pointer: следующий нетронутый индекс
	freeStack  []int // стек индексов освобождённых объектов

	spanID uint32

	// ─── Ownership tracking ───
	//
	// mu защищает freeStack когда span в состоянии spanInCentral.
	// Когда span в spanInCache — mu НЕ берётся (single-writer, lock-free).
	//
	// central — back-reference для вызова ReturnToPartial.
	// Устанавливается один раз при AllocSpan и не меняется.
	mu      sync.Mutex
	state   uint32    // spanInCache или spanInCentral
	central *MCentral // back-reference на «свой» MCentral
}

// NewSpan создаёт span над существующим блоком памяти.
//
// data — slice из chunk'а mheap (не копируется, не аллоцируется).
// elemSize — размер одного объекта.
// sizeClass — индекс size class (для back-reference).
// central — MCentral, которому принадлежит span (для ReturnToPartial).
func NewSpan(data []byte, elemSize, sizeClass int, central *MCentral) *Span {
	cap := len(data) / elemSize
	return &Span{
		data:       data,
		elemSize:   elemSize,
		sizeClass:  sizeClass,
		capacity:   cap,
		allocIndex: 0,
		freeStack:  make([]int, 0, cap/4),
		state:      spanInCentral, // рождается в central (до выдачи в mcache)
		central:    central,
	}
}

// Alloc выделяет один объект из span. Возвращает slice + индекс объекта.
//
// ZERO LOCKS — этот метод вызывается только из mcache,
// который принадлежит одному воркеру.
//
// Возвращает nil, -1 если span полон.
func (s *Span) Alloc() ([]byte, int) {
	// Путь 1: переиспользуем освобождённый объект
	if n := len(s.freeStack); n > 0 {
		idx := s.freeStack[n-1]
		s.freeStack = s.freeStack[:n-1]
		offset := idx * s.elemSize
		return s.data[offset : offset+s.elemSize], idx
	}

	// Путь 2: bump pointer — берём следующий нетронутый
	if s.allocIndex < s.capacity {
		idx := s.allocIndex
		s.allocIndex++
		offset := idx * s.elemSize
		return s.data[offset : offset+s.elemSize], idx
	}

	// Span полон
	return nil, -1
}

// Free возвращает объект по индексу обратно в span.
//
// Поведение зависит от состояния span'а:
//
//	state == spanInCache:
//	  Lock-free. Вызывается только владельцем-mcache.
//	  Просто push в freeStack.
//
//	state == spanInCentral:
//	  Берёт s.mu (защита от параллельных Free из разных workers).
//	  Если span БЫЛ полон (wasFull) → вызывает central.ReturnToPartial(s),
//	  чтобы вернуть span в оборот.
//
// Примечание: НЕ обнуляем память. Данные будут перезаписаны
// при следующем Alloc+encodeInto. Обнуление стоило 10% CPU (pprof).
func (s *Span) Free(objIndex int) {
	if s.state == spanInCache {
		// Span принадлежит нашему mcache → single writer, lock-free.
		s.freeStack = append(s.freeStack, objIndex)
		return
	}

	// Span в MCentral → нужен mutex (любой worker может вызвать Free).
	s.mu.Lock()
	wasFull := s.IsFull() // проверяем ДО добавления в freeStack
	s.freeStack = append(s.freeStack, objIndex)
	s.mu.Unlock()

	// Если span был полностью заполнен, а теперь появилось место →
	// переводим из full в partial, чтобы другие mcache могли его получить.
	if wasFull && s.central != nil {
		s.central.ReturnToPartial(s)
	}
}

// IsFull возвращает true если в span нет свободных объектов.
func (s *Span) IsFull() bool {
	return len(s.freeStack) == 0 && s.allocIndex >= s.capacity
}

// FreeCount возвращает количество доступных для аллокации объектов.
func (s *Span) FreeCount() int {
	return len(s.freeStack) + (s.capacity - s.allocIndex)
}

// ─── Handle ─────────────────────────────────────────────────
//
// Handle — адрес объекта в аллокаторе.
//
// Старый дизайн: (chunkID, offset) — хватало только для чтения.
// Новый дизайн:  (spanID, objIndex) — хватает для чтения И удаления.
//
// Формат (uint64):
//
//	bits 63..32: spanID   (uint32 → до 4 млрд span'ов)
//	bits 31..0:  objIndex (uint32 → до 4 млрд объектов)
//
// Для нашей реальности:
//
//	64K span'ов × 128 obj/span × 32B = 256MB (минимум)
//	64K span'ов × 128 obj/span × 4KB = 32GB (максимум)
//	Более чем достаточно для in-memory БД.
type Handle uint64

func MakeHandle(spanID uint32, objIndex int) Handle {
	return Handle(uint64(spanID)<<32 | uint64(objIndex))
}

func (h Handle) SpanID() uint32 { return uint32(h >> 32) }
func (h Handle) ObjIndex() int  { return int(uint32(h)) }
