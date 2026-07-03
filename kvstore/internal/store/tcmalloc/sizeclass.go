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

import (
	"sync"
	"sync/atomic"
)

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
// Span может находиться в двух состояниях (документируют владение;
// Alloc пишет freeStack только у владельца-mcache):
//
//	spanInCache   — span принадлежит конкретному mcache.
//	                Alloc вызывает ОДИН worker (владелец) → freeStack lock-free.
//
//	spanInCentral — span лежит в MCentral (в partial или full), владельца нет.
//
// Free() может вызвать ЛЮБОЙ worker в ЛЮБОМ состоянии (deferred-free,
// cross-worker) → он НИКОГДА не пишет freeStack напрямую, а кладёт индекс в
// remoteFree под mu; владелец сольёт его в freeStack при Alloc (см. Span.Free).
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
//	state == spanInCache   → принадлежит mcache, Alloc lock-free
//	state == spanInCentral → лежит в mcentral, владельца нет
//
//	Free() (из любого потока) кладёт индекс в remoteFree под mu; владелец
//	сливает remoteFree → freeStack в Alloc. Если спан был в full — Free через
//	inFull-гейт зовёт central.notifyMaybeFull, возвращая его в partial.
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
	// mu защищает remoteFree (и переход remoteFree → freeStack при дренаже).
	// freeStack — приватный для владельца (Alloc): пишется под mu ТОЛЬКО во
	// время дренажа самим владельцем, поэтому pop в Alloc остаётся lock-free.
	//
	// central — back-reference на «свой» MCentral. Устанавливается один раз
	// при AllocSpan и не меняется.
	mu      sync.Mutex
	state   uint32    // spanInCache или spanInCentral (документирует владение)
	central *MCentral // back-reference на «свой» MCentral

	// ─── Remote free queue (защита от T1) ───
	//
	// Free() может вызвать НЕ владелец спана (deferred-free из store.go,
	// cross-worker Free в vector). Писать в freeStack напрямую нельзя — это
	// гонка с lock-free Alloc() владельца → потеря обновлений → один objIndex
	// двум ключам → тихая порча данных.
	//
	// Поэтому не-владелец кладёт индекс в remoteFree под mu. Владелец сливает
	// remoteFree → freeStack в начале Alloc(), проверяя lock-free счётчик
	// remotePend на горячем пути (дренаж происходит только когда есть что сливать).
	remoteFree []int        // возвраты не от владельца; под mu
	remotePend atomic.Int32 // len(remoteFree): lock-free сигнал к дренажу
	inFull     atomic.Bool  // спан лежит в MCentral.full (гейт для notifyMaybeFull)
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
	// Путь 0: дренаж remote-free от не-владельцев (редко).
	// Гейт lock-free счётчиком: на горячем пути — один atomic.Load.
	// Только владелец (этот вызов Alloc) пишет freeStack, поэтому после
	// дренажа под mu pop ниже остаётся безопасным без лока.
	if s.remotePend.Load() != 0 {
		s.mu.Lock()
		s.freeStack = append(s.freeStack, s.remoteFree...)
		s.remoteFree = s.remoteFree[:0]
		s.remotePend.Store(0)
		s.mu.Unlock()
	}

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
// Free может вызвать ЛЮБОЙ воркер/горутина, НЕ обязательно владелец спана
// (deferred-free из store.go, cross-worker Free в vector). Поэтому индекс
// НИКОГДА не пишется в freeStack напрямую (это гонка с lock-free Alloc()
// владельца → порча данных, баг T1). Вместо этого — remote-free очередь:
// кладём в remoteFree под mu, владелец сольёт её в freeStack при Alloc().
//
// Примечание: НЕ обнуляем память. Данные будут перезаписаны
// при следующем Alloc+encodeInto. Обнуление стоило 10% CPU (pprof).
func (s *Span) Free(objIndex int) {
	s.mu.Lock()
	s.remoteFree = append(s.remoteFree, objIndex)
	s.remotePend.Store(int32(len(s.remoteFree)))
	s.mu.Unlock()

	// Если спан лежит в MCentral.full — вернуть его в partial: появилось место,
	// он снова выдаваем. Гейт inFull держит путь lock-free, пока спан активен
	// у владельца (обычный случай — владелец сольёт remoteFree сам).
	if s.central != nil && s.inFull.Load() {
		s.central.notifyMaybeFull(s)
	}
}

// IsFull возвращает true если в span нет свободных объектов.
//
// Учитывает ещё не слитую remote-free очередь (remotePend, atomic) — иначе
// спан с возвращёнными, но не дренированными слотами ошибочно считался бы
// полным и застревал в MCentral.full.
func (s *Span) IsFull() bool {
	return len(s.freeStack) == 0 && s.remotePend.Load() == 0 && s.allocIndex >= s.capacity
}

// FreeCount возвращает количество доступных для аллокации объектов
// (freeStack + ещё не слитая remote-free очередь + непройденный bump-хвост).
func (s *Span) FreeCount() int {
	return len(s.freeStack) + int(s.remotePend.Load()) + (s.capacity - s.allocIndex)
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
