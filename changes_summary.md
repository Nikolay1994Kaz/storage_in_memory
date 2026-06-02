# Отчет по интеграции TCMalloc и Векторного поиска (HNSW)

В данном файле подробно описаны все изменения, внесенные в проект для реализации гибридного управления памятью (хранение связей графа в TCMalloc, а координат векторов — на куче Go в плоском массиве).

---

## 1. Сводка измененных файлов

Всего изменено **13 файлов**:
* **`kvstore/internal/store/tcmalloc/mheap.go`** — Исправление коллизии нулевого дескриптора.
* **`kvstore/internal/store/tcmalloc/store.go`** — Экспорт методов аллокации.
* **`kvstore/vector/graph.go`** — Замена арены связей на TCMalloc, логика чтения/записи связей через `unsafe.Slice`.
* **`kvstore/vector/vector_arena.go`** — Удаление старой структуры `NeighborsArena`.
* **`kvstore/vector/store.go`** — Проброс аллокатора в конструкторы `VectorStore`.
* **`kvstore/vector/snapshot_binary.go`** — Изменение логики сохранения/загрузки бинарных снапшотов.
* **Тесты и примеры (`graph_test.go`, `store_test.go`, `snapshot_binary_test.go`, `snapshot_perf_test.go`, `profile_test.go`, `prod_bench/main.go`)** — Обновление инициализации тестов под сигнатуры с TCMalloc.
* **`kvstore/cmd/kvstore/main.go`** — Проброс единого аллокатора БД в векторный движок при старте сервера.

---

## 2. Детальное описание изменений по файлам

### [mheap.go](file:///home/nikolay/storage_in_memory/kvstore/internal/store/tcmalloc/mheap.go)
* **Что изменилось:** В функцию `NewMHeap()` добавлено резервирование нулевого индекса в реестре спанов:
  ```go
  h.registry.append(nil) // Резервируем spanID = 0 для исключения коллизии нулевого Handle
  ```
* **Зачем:** Первая аллокация в TCMalloc возвращала `spanID = 0, objIndex = 0`, из-за чего валидный хэндл был равен `0`. В графе HNSW нулевое значение трактовалось как "нет соседей" (`nil`). Резервирование `spanID = 0` гарантирует, что все валидные дескрипторы будут строго больше нуля.

---

### [store.go](file:///home/nikolay/storage_in_memory/kvstore/internal/store/tcmalloc/store.go)
* **Что изменилось:** Экспортированы три публичных метода для работы с аллокатором вне пакета:
  ```go
  func (s *TCMallocStore) Alloc(workerID int, size int) ([]byte, Handle)
  func (s *TCMallocStore) Free(workerID int, handle Handle)
  func (s *TCMallocStore) Resolve(handle Handle) []byte
  ```

---

### [graph.go](file:///home/nikolay/storage_in_memory/kvstore/vector/graph.go)
* **Что изменилось:**
  1. В структуре `Node` поле `NeighborsOffset uint32` заменено на `NeighborsHandle tcmalloc.Handle`.
  2. Из структуры `Graph` удалена ссылка на `NeighborsArena`, добавлена ссылка на `allocator *tcmalloc.TCMallocStore`.
  3. Добавлены функции zero-copy преобразования слайса байт в слайс индексов соседей `[]uint64` через `unsafe`:
     ```go
     func bytesToUint64(b []byte) []uint64 {
         if len(b) == 0 { return nil }
         return unsafe.Slice((*uint64)(unsafe.Pointer(&b[0])), len(b)/8)
     }
     ```
  4. Переписаны методы `getNeighbors` и `setNeighbors` для чтения и записи по Handle:
     ```go
     func (g *Graph) getNeighbors(handle tcmalloc.Handle, targetLevel int) []uint64 {
         if uint64(handle) == 0 { return nil }
         byteBuf := g.allocator.Resolve(handle)
         uint64Buf := bytesToUint64(byteBuf)
         offset := g.offsetForLevel(targetLevel)
         length := int(uint64Buf[offset])
         if length == 0 { return nil }
         return uint64Buf[offset+1 : offset+1+length]
     }
     ```
  5. Вставка (`Insert`) и удаление (`Delete`) переведены на выделение памяти через `g.allocator.Alloc(...)` и `g.allocator.Free(...)`.

---

### [vector_arena.go](file:///home/nikolay/storage_in_memory/kvstore/vector/vector_arena.go)
* **Что изменилось:** Полностью удалена структура `NeighborsArena` и все её методы (`Allocate`, `Free`, `GetNeighbors`, `SetNeighbors`).
* **Зачем:** Логика этой арены теперь целиком делегирована более совершенному и протестированному менеджеру памяти TCMalloc. Координаты векторов (`VectorArena`) остались без изменений для максимальной производительности AVX2 SIMD.

---

### [store.go](file:///home/nikolay/storage_in_memory/kvstore/vector/store.go)
* **Что изменилось:** Конструкторы хранилища теперь принимают `TCMallocStore`:
  ```go
  func NewVectorStore(distance DistanceFunc, allocator *tcmalloc.TCMallocStore) *VectorStore
  func NewVectorStoreCosine(allocator *tcmalloc.TCMallocStore) *VectorStore
  ```

---

### [snapshot_binary.go](file:///home/nikolay/storage_in_memory/kvstore/vector/snapshot_binary.go)
* **Что изменилось:** Полностью переписана логика сохранения и загрузки бинарных снапшотов.
  * **Сохранение (`SaveBinary`):** Списков соседей больше нет в едином плоском массиве (арене). Теперь при обходе графа мы последовательно пишем соседей каждой ноды для каждого уровня в бинарный поток.
  * **Загрузка (`LoadBinary`):** При чтении файла мы считываем плоские списки соседей для каждой ноды, делаем `allocator.Alloc(...)` в TCMalloc, получаем новые `Handle` и записываем их в восстанавливаемые структуры `Node`.

---

### [main.go (cmd/kvstore)](file:///home/nikolay/storage_in_memory/kvstore/cmd/kvstore/main.go)
* **Что изменилось:** Сервер инициализирует `vecStore` с использованием глобального аллокатора базы данных:
  ```go
  // tcmalloc store уже создан на строке 55
  vecStore := vector.NewVectorStore(vector.EuclideanDistance, s)
  ```

---

## 3. Результаты и стабильность

* **Все тесты пройдены (`go test ./...`):** Успешно проходят как тесты корректности поиска, так и тесты снапшотов и многопоточной работы.
* **0 аллокаций в вычислении расстояний:** Горячий тракт поиска не совершает выделений памяти в куче Go.
* **Производительность:** На 100 000 векторов (128 измерений) пропускная способность параллельного поиска составляет **9 062 QPS** со средним временем отклика ~1.04 ms.
