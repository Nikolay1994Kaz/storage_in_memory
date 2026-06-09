# TCMalloc: полный путь аллокации на реальном коде

## Фаза 0: Инициализация

При старте `NewTCMallocStore(12)` создаётся вся иерархия:

```go
// store.go:104
func NewTCMallocStore(numWorkers int) *TCMallocStore {
    heap := NewMHeap()  // ← Фаза 0.1: глобальный аллокатор

    var centrals [numSizeClasses]*MCentral
    for i := 0; i < numSizeClasses; i++ {
        centrals[i] = NewMCentral(i, heap)  // ← Фаза 0.2: 8 центральных пулов
    }

    caches := make([]*MCache, numWorkers)
    for i := 0; i < numWorkers; i++ {
        caches[i] = NewMCache(centrals)  // ← Фаза 0.3: 12 per-worker кешей
    }
    ...
}
```

**MHeap** выделяет первый chunk 1MB сырой памяти:

```go
// mheap.go:137
func NewMHeap() *MHeap {
    h := &MHeap{
        chunks:   make([][]byte, 0, 16),
        registry: newSpanRegistry(),
        largeFree: make([]*Span, 0, 16),
    }
    h.chunks = append(h.chunks, make([]byte, chunkSize))  // ← 1MB сразу
    h.offset = 0
    return h
}
```

**MCentral** — пустые списки, ни одного span'а:

```go
// mcentral.go:42
func NewMCentral(sizeClass int, heap *MHeap) *MCentral {
    return &MCentral{
        sizeClass: sizeClass,
        partial:   make([]*Span, 0, 8),  // ← пусто
        full:      make([]*Span, 0, 8),  // ← пусто
        heap:      heap,
    }
}
```

**MCache** — все слоты nil, ни одного span'а:

```go
// mcache.go:53
func NewMCache(centrals [numSizeClasses]*MCentral) *MCache {
    return &MCache{
        centrals: centrals,  // ← только ссылки на centrals
        // alloc[0..7] = nil — span'ов ещё нет!
    }
}
```

> **Итог фазы 0**: MHeap имеет 1MB сырой памяти. MCentral пусты. MCache пусты. Ноль span'ов, ноль lock'ов.

---

## Фаза 1: Первый SET — Cold Path

```go
store.Set(0, "user:123", []byte("active"))
// размер записи: 4 + 8 + 4 + 6 = 22 байта
```

Входим в `Set`:

```go
// store.go:196
func (s *TCMallocStore) Set(workerID int, key string, value []byte) {
    size := encodeSize(key, value)       // = 22
    cache := s.caches[workerID]          // MCache[0]
    buf, handle := cache.Alloc(size)     // ← СЮДА
    ...
}
```

Внутри `MCache.Alloc` — определяем size class:

```go
// mcache.go:72
func (c *MCache) Alloc(size int) ([]byte, Handle) {
    sc := SizeClassForSize(size)  // 22 → class 0 (32B)
    ...
    s := c.alloc[sc]  // alloc[0] == nil ← ПЕРВЫЙ РАЗ!
```

`alloc[0]` — nil, span'а нет. Попадаем в **холодный путь**:

```go
    // mcache.go:102 — холодный путь
    c.RefillCount++
    s = c.centrals[sc].GetSpan()  // ← идём в MCentral
    c.alloc[sc] = s
```

MCentral тоже пуст — идём в MHeap:

```go
// mcentral.go:64
func (c *MCentral) GetSpan() *Span {
    c.mu.Lock()               // ← LOCK #1: MCentral mutex
    defer c.mu.Unlock()

    // partial пуст (первый вызов!)
    if n := len(c.partial); n > 0 { ... }  // n == 0, пропускаем

    // Просим новый span у MHeap
    c.totalSpansAllocated++
    s := c.heap.AllocSpan(c.sizeClass, c)  // ← идём в MHeap
    s.state = spanInCache                   // сразу отдаём в mcache
    return s
}
```

MHeap нарезает span из chunk'а:

```go
// mheap.go:153
func (h *MHeap) AllocSpan(sizeClass int, central *MCentral) *Span {
    elemSize := sizeClasses[sizeClass]      // 32
    numObjects := objectsPerSpan[sizeClass] // 1024
    spanSize := elemSize * numObjects       // 32 × 1024 = 32KB

    h.mu.Lock()               // ← LOCK #2: MHeap mutex
    defer h.mu.Unlock()

    // offset=0, spanSize=32768, chunkSize=1MB — влезает
    if h.offset+spanSize > chunkSize { ... }  // нет, пропускаем

    chunkIdx := len(h.chunks) - 1                       // 0
    data := h.chunks[chunkIdx][h.offset : h.offset+spanSize]  // chunks[0][0:32768]
    h.offset += spanSize                                 // offset = 32768

    s := NewSpan(data, elemSize, sizeClass, central)
    s.spanID = uint32(h.registry.len())  // spanID = 0
    h.registry.append(s)
    return s
}
```

Span создаётся:

```go
// sizeclass.go:168
func NewSpan(data []byte, elemSize, sizeClass int, central *MCentral) *Span {
    cap := len(data) / elemSize  // 32768 / 32 = 1024 объекта!
    return &Span{
        data:       data,
        elemSize:   elemSize,     // 32
        capacity:   cap,          // 1024
        allocIndex: 0,            // bump pointer на нуле
        freeStack:  make([]int, 0, cap/4),
        state:      spanInCentral,
        central:    central,
    }
}
```

Вернулись в MCache с новым span'ом. Теперь аллоцируем из него:

```go
    // mcache.go:107 — продолжение Alloc
    buf, idx := s.Alloc()  // ← первый объект из span
```

Внутри span — bump pointer:

```go
// sizeclass.go:188
func (s *Span) Alloc() ([]byte, int) {
    // freeStack пуст — пропускаем
    if n := len(s.freeStack); n > 0 { ... }

    // Bump pointer — ОДНА инструкция
    if s.allocIndex < s.capacity {   // 0 < 1024
        idx := s.allocIndex          // 0
        s.allocIndex++               // → 1
        offset := idx * s.elemSize   // 0 × 32 = 0
        return s.data[offset : offset+s.elemSize], idx  // data[0:32], 0
    }
    ...
}
```

Вернулись в Set с буфером — записываем данные и сохраняем в индекс:

```go
// store.go:203 — продолжение Set
    encodeInto(buf, key, value)  // [4B keyLen][key][4B valLen][value] → в buf

    hash := hashStoreKey(key)
    sh := &s.shards[hash%numStoreShards]
    sh.mu.Lock()                  // ← LOCK #3: shard mutex
    t := sh.table.Load()
    t.Put(hash, uint64(handle))
    sh.mu.Unlock()
```

> **Итог фазы 1**: 3 lock'а (heap + central + shard). Создан span#0 на 1024 объекта. Использован 1 объект. Это САМЫЙ дорогой SET — дальше будет в 3 раза дешевле.

---

## Фаза 2: SET #2 — #1024 — Hot Path

Теперь `alloc[0]` уже не nil:

```go
// mcache.go:80
    s := c.alloc[sc]  // alloc[0] = span#0 ← есть!

    if s != nil {
        if buf, idx := s.Alloc(); buf != nil {  // ← bump pointer
            c.AllocCount++
            return buf, MakeHandle(s.spanID, idx)
        }
        ...
    }
```

`span.Alloc()` — просто bump pointer:

```go
// sizeclass.go:198
    idx := s.allocIndex    // 1, 2, 3, ... 1023
    s.allocIndex++         // → 2, 3, 4, ... 1024
    offset := idx * s.elemSize
    return s.data[offset : offset+s.elemSize], idx
```

**Ноль lock'ов на аллокаторе!** Единственный lock — shard mutex при записи в индекс.

> **1024 аллокации — 1 обращение к MCentral. Выигрыш vs ArenaStore: 1024× меньше contention.**

---

## Фаза 3: SET #1025 — Span полон, Refill

Span#0 заполнен. `Alloc()` вернёт nil:

```go
// mcache.go:83
    if s != nil {
        if buf, idx := s.Alloc(); buf != nil { ... }
        // buf == nil → span полон!

        // Возвращаем полный span в MCentral
        c.centrals[sc].ReturnSpan(s)  // ← в full list
        c.alloc[sc] = nil
    }
```

`ReturnSpan` кладёт в full:

```go
// mcentral.go:92
func (c *MCentral) ReturnSpan(s *Span) {
    c.mu.Lock()
    defer c.mu.Unlock()

    s.state = spanInCentral  // теперь Free() будет брать mutex

    if s.IsFull() {
        c.full = append(c.full, s)  // ← полный → в full
    } else {
        c.partial = append(c.partial, s)
    }
}
```

Затем запрашиваем новый span через `GetSpan()` → MHeap нарезает span#1 из `chunks[0][32768:65536]`. Цикл повторяется.

**Развитие MHeap**: один 1MB chunk вмещает 31 span по 32KB. Это 31 × 1024 = **31,744 аллокации** до нового chunk'а.

---

## Фаза 4: DEL — два сценария Free

```go
// store.go:274
func (s *TCMallocStore) Del(workerID int, key string) bool {
    ...
    sh.mu.Lock()
    t := sh.table.Load()
    rawHandle, ok := t.Delete(hash)  // удаляем из индекса
    sh.mu.Unlock()

    // Освобождаем блок обратно в аллокатор
    s.caches[workerID].Free(s.heap, Handle(rawHandle))
    return true
}
```

MCache.Free определяет тип span'а:

```go
// mcache.go:123
func (c *MCache) Free(heap *MHeap, handle Handle) {
    s := heap.GetSpan(handle.SpanID())  // ← LOCK-FREE: registry.get()

    if s.sizeClass < 0 {
        heap.FreeLarge(s)  // large object → в пул
        return
    }

    s.Free(handle.ObjIndex())  // ← поведение зависит от state
}
```

**Сценарий A** — span в нашем MCache (`spanInCache`):

```go
// sizeclass.go:224
func (s *Span) Free(objIndex int) {
    if s.state == spanInCache {
        // Наш span → single writer → LOCK-FREE!
        s.freeStack = append(s.freeStack, objIndex)
        return
    }
    ...
}
```

**Ноль lock'ов.** Просто push в стек.

**Сценарий B** — span уже в MCentral (`spanInCentral`), Worker 3 удаляет ключ, который записал Worker 0:

```go
// sizeclass.go:232
    // Span в MCentral → нужен mutex
    s.mu.Lock()
    wasFull := s.IsFull()                       // ДО добавления
    s.freeStack = append(s.freeStack, objIndex)
    s.mu.Unlock()

    // Если span БЫЛ полон, а теперь есть место → переводим в partial
    if wasFull && s.central != nil {
        s.central.ReturnToPartial(s)
    }
```

`ReturnToPartial` — ключевой механизм recycling'а:

```go
// mcentral.go:111
func (c *MCentral) ReturnToPartial(s *Span) {
    c.mu.Lock()
    defer c.mu.Unlock()

    // Убираем из full
    for i, fs := range c.full {
        if fs == s {
            c.full[i] = c.full[len(c.full)-1]
            c.full = c.full[:len(c.full)-1]
            break
        }
    }

    // Добавляем в partial — теперь GetSpan() отдаст его
    c.partial = append(c.partial, s)
}
```

> Без ReturnToPartial full-span'ы копились бы мёртвым грузом: место появилось, но никто об этом не знает. Этот метод переводит span из `full → partial`, делая его доступным для `GetSpan()`.

---

## Фаза 5: Recycling — переиспользование через freeStack

Worker вызывает `GetSpan()`, получает partial span с 500 свободными слотами:

```go
// mcentral.go:69
    if n := len(c.partial); n > 0 {
        s := c.partial[n-1]       // берём partial span
        c.partial = c.partial[:n-1]
        s.state = spanInCache     // передаём владение
        return s
    }
```

Теперь `Alloc()` берёт из freeStack вместо bump pointer:

```go
// sizeclass.go:190
func (s *Span) Alloc() ([]byte, int) {
    // Путь 1: ПЕРЕИСПОЛЬЗОВАНИЕ — freeStack не пуст!
    if n := len(s.freeStack); n > 0 {
        idx := s.freeStack[n-1]          // pop
        s.freeStack = s.freeStack[:n-1]
        offset := idx * s.elemSize
        return s.data[offset : offset+s.elemSize], idx
    }
    // Путь 2 (bump) уже не нужен — bump pointer дошёл до конца
    ...
}
```

**Новая память НЕ выделяется** — переиспользуем старые слоты.

---

## Фаза 6: Self-Sweep — возврат пустых span'ов

Каждые 4096 аллокаций:

```go
// mcache.go:88
    if c.AllocCount&(sweepInterval-1) == 0 {
        c.sweep()
    }
```

```go
// mcache.go:161
func (c *MCache) sweep() {
    for sc := 0; sc < numSizeClasses; sc++ {
        s := c.alloc[sc]
        if s == nil { continue }

        // Полностью пуст: ВСЕ слоты выделены И ВСЕ освобождены
        if s.allocIndex >= s.capacity && len(s.freeStack) >= s.capacity {
            c.centrals[sc].ReturnSpan(s)
            c.alloc[sc] = nil
            // Span вернулся в MCentral → другие workers получат его
        }
    }
}
```

> **Зачем**: Worker 0 получил span, записал 1024 ключа, все удалили (DEL). Span пуст, но лежит в alloc[] Worker'а 0. Worker 1 не может его получить → идёт в MHeap за НОВОЙ памятью. Sweep возвращает пустые span'ы → Worker 1 получит их через GetSpan().

---

## Фаза 7: Large Objects (> 4KB)

SizeClassForSize возвращает -1:

```go
// mcache.go:74
    if sc < 0 {
        return c.allocLarge(size)  // → прямо в MHeap
    }
```

```go
// mcache.go:140
func (c *MCache) allocLarge(size int) ([]byte, Handle) {
    buf, handle := c.centrals[0].heap.AllocLarge(size)
    c.AllocCount++
    return buf, handle
}
```

MHeap сначала ищет в пуле переиспользуемых:

```go
// mheap.go:208
func (h *MHeap) AllocLarge(size int) ([]byte, Handle) {
    h.mu.Lock()
    defer h.mu.Unlock()

    // Best-fit из largeFree пула
    bestIdx := -1
    bestSize := int(^uint(0) >> 1)
    for i, s := range h.largeFree {
        if s.elemSize >= size && s.elemSize < bestSize {
            bestIdx = i
            bestSize = s.elemSize
        }
    }
    if bestIdx >= 0 {
        // ПЕРЕИСПОЛЬЗОВАНИЕ — новая память не нужна
        s := h.largeFree[bestIdx]
        h.largeFree[bestIdx] = h.largeFree[len(h.largeFree)-1]
        h.largeFree = h.largeFree[:len(h.largeFree)-1]
        s.allocIndex = 1
        s.freeStack = s.freeStack[:0]
        return s.data[:size], MakeHandle(s.spanID, 0)
    }

    // Нет подходящего → нарезаем из chunk'а (или новый chunk если > 1MB)
    ...
}
```

FreeLarge возвращает в пул:

```go
// mheap.go:276
func (h *MHeap) FreeLarge(s *Span) {
    h.mu.Lock()
    s.allocIndex = 0
    s.freeStack = s.freeStack[:0]
    h.largeFree = append(h.largeFree, s)
    h.usedBytes.Add(-int64(s.elemSize))
    h.mu.Unlock()
}
```

> Large objects **минуют MCentral**. Путь: MCache → MHeap напрямую. Маркер: `sizeClass == -1`.

---

## GET — полностью lock-free

```go
// store.go:243
func (s *TCMallocStore) Get(key string) ([]byte, bool) {
    hash := hashStoreKey(key)
    sh := &s.shards[hash%numStoreShards]

    t := sh.table.Load()           // atomic.Pointer.Load() — БЕЗ LOCK'а
    rawHandle, ok := t.Get(hash)   // linear probing по atomic слотам
    if !ok { return nil, false }

    handle := Handle(rawHandle)
    buf := s.heap.Resolve(handle)  // registry.get() — lock-free
    storedKey, value := decodeFrom(buf)

    if storedKey != key { return nil, false }  // коллизия хешей
    return value, true
}
```

```go
// mheap.go:193
func (h *MHeap) Resolve(handle Handle) []byte {
    s := h.registry.get(handle.SpanID())  // два array access, LOCK-FREE
    idx := handle.ObjIndex()
    offset := idx * s.elemSize
    return s.data[offset : offset+s.elemSize]
}
```

```go
// mheap.go:91 — registry lock-free read
func (r *spanRegistry) get(spanID uint32) *Span {
    chunks := *r.chunks.Load()            // atomic.Pointer
    ci := spanID >> regChunkBits           // chunk index
    return chunks[ci][spanID&regChunkMask] // slot index
}
```

> **GET: ноль mutex, ноль RWMutex, ноль atomic CAS. Только atomic Load'ы.**

---

## Сводка: стоимость каждой операции

| Операция | Аллокатор locks | Индекс locks | Частота |
|----------|----------------|--------------|---------|
| SET (hot) | **0** — span.Alloc() bump | 1 (shard.mu) | 99.9% |
| SET (refill) | 2 — central.mu + heap.mu | 1 (shard.mu) | 0.1% |
| GET | **0** | **0** | 100% |
| DEL (own span) | **0** — freeStack push | 1 (shard.mu) | ~50% |
| DEL (foreign) | 1-2 — span.mu + central.mu | 1 (shard.mu) | ~50% |
| SET (large) | 1 — heap.mu | 1 (shard.mu) | rare |

При 1M ключей × 12 workers: **99.9% аллокаций без mutex на аллокаторе**.

---

## Дополнительные архитектурные детали реализации

### 1. Архитектурный гибрид `TCMallocStore`
`TCMallocStore` в кодовой базе выполняет две совмещенные роли:
1. **Интерфейс базы данных (Key-Value Store):** Предоставляет высокоуровневые операции `Set`, `Get` и `Del` по строковым ключам. Для этого он содержит шардированный индекс `shards [256]indexShard`, преобразующий хэш ключа в `Handle`.
2. **Системный аллокатор памяти:** Предоставляет низкоуровневый интерфейс выделения памяти через методы `Alloc`, `Free` и `Resolve`. Например, HNSW-граф векторного поиска (`vector/graph.go`) использует `TCMallocStore` в качестве аллокатора для списков связей вершин (`NeighborsHandle`). При этом самому графу шардированный индекс (`shards`) не нужен — он работает с аллокатором напрямую по дескрипторам `Handle`.

---

### 2. Двухуровневый реестр спанов (`spanRegistry`)
Для безопасного lock-free резолвинга дескрипторов `Handle` в куски памяти спанов используется двухуровневый `spanRegistry`:
* **Уровень 1 (L1):** Слайс указателей `[]*spanChunk` (тип L1). Изначально выделяется вместимостью на 16 элементов (`make([]*spanChunk, 0, 16)`). Указатель на этот слайс хранится в `atomic.Pointer`. При добавлении чанков L2 выполняется Copy-On-Write (COW) на L1 (копируются только указатели на чанки, а не сами спаны).
* **Уровень 2 (L2):** Фиксированные массивы `spanChunk [1024]*Span` (1024 указателя на спаны).
* **Почему это безопасно:** Массивы `spanChunk` аллоцируются один раз и **никогда не перемещаются в памяти**. Это гарантирует, что параллельно читающие потоки в `Get` всегда обращаются по валидным адресам L2 чанков, даже если L1 слайс в этот момент переаллоцируется в другом потоке.

---

### 3. Детальный разбор холодного пути (Cold Path)
Когда воркер выполняет первую вставку (Set), свободного спана в `MCache` еще нет. Происходит следующее:
1. Вычисляется размер записи: `size = encodeSize(key, value)`.
2. Запрашивается аллокация в `MCache` для соответствующего size class.
3. Поскольку локальный спан в `MCache` равен `nil`, управление передается в `MCentral` (`GetSpan`).
4. В `MCentral` списки свободных спанов тоже пусты. Управление уходит в глобальный `MHeap.AllocSpan`.
5. `MHeap` нарезает физическую память из текущего чанка 1 МБ и создает объект `Span`.
6. Спан регистрируется в реестре `spanRegistry.append` под уникальным `spanID` (длина реестра).
7. Если для `spanID` требуется новый чанк (индекс чанка $\ge$ длина L1), реестр аллоцирует новый `spanChunk [1024]*Span` и атомарно подменяет L1 указатель через `Store()`.
8. Указатель на спан сохраняется в чанк: `chunks[ci][si] = s`.
9. Спан возвращается по цепочке: `MHeap` $\to$ `MCentral` $\to$ `MCache`.
10. `MCache` делает `s.Alloc()`, получая буфер `buf` и локальный индекс объекта `idx`.
11. В `TCMallocStore.Set` возвращается `buf` и `Handle`, сгенерированный как `(spanID << 8) | idx`.

---

### 4. Кастомная открыто-адресуемая `HashTable`
Каждый шард индекса хранит данные в своей независимой `HashTable` с линейным исследованием (linear probing):
* **Оптимизация степени двойки:** Размер таблицы `size` всегда округляется до ближайшей степени 2. Это позволяет заменить медленную операцию остатка от деления `%` на быструю побитовую маску: `idx = hash & (size - 1)`.
* **Sentinel-значения:** Значение `emptyHash = 0` указывает на свободный слот, а `tombstoneHash = 0xFFFFFFFFFFFFFFFF` — на удаленный элемент.
* **sanitizeHash:** Чтобы реальный хэш ключа не пересекся с маркерами, хэши `0` и `MAX_UINT64` сдвигаются на 1.
* **Масштабирование:**
  - Если заполнение (живые элементы + надгробия) превышает 70%, таблица увеличивается в 2 раза (`Grow`).
  - Если количество надгробий превышает 25%, таблица пересобирается (`Rebuild`) без увеличения размера, полностью очищаясь от надгробий.
  - Изменение и подмена таблицы выполняются атомарно через `atomic.Pointer`.

---

### 5. Конкурентность и Use-After-Free (ABA) в `Get`
В реальном коде `Get` берет `RLock` на шарде перед чтением. Это критически необходимо из-за специфики ручного управления памятью:
* Без блокировки читатель может извлечь `Handle` из хеш-таблицы, но перед тем, как он выполнит `Resolve` и прочитает данные, другой поток может сделать `Del` для этого ключа.
* При `Del` память незамедлительно возвращается в аллокатор (`MCache.Free`), где она тут же переиспользуется под новый ключ.
* В результате читатель прочитал бы поврежденные или чужие данные.
* `RLock` в `Get` и `Lock` в `Del`/`Set` гарантируют, что память не будет освобождена и переиспользована, пока читатель не завершит разбор данных.
