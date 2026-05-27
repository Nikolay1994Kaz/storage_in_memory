# KVStore: Pipeline + Ring Buffer + TCMalloc — Итоги сессии

## Что было на старте

Сервер работал на **ArenaStore** + **bufio.Reader/Writer** + **BatchWAL**.
Baseline без pipeline: **~65-77K RPS**.

---

## Фаза 1: Pipeline Support (bufio.Writer + Buffered())

### Что сделали
1. **`protocol.Writer`** — заменили прямой `io.Writer` на `bufio.Writer`, добавили `Flush()`
2. **`protocol.Reader.Buffered()`** — метод для проверки наличия данных в буфере bufio
3. **`server.handleConn`** — добавили `for cs.Reader.Buffered() > 0` цикл для обработки всех команд из TCP-пакета перед единственным `Flush()`

### Результат

| Тест | До | После | Прирост |
|:-----|:---|:------|:--------|
| SET без pipeline | 65K | 65K | — |
| GET P=16 | 65K | 878K | **13.5x** |
| GET P=64 | 65K | 1.96M | **30x** |
| GET P=128 200c | 65K | 2.42M | **37x** |

### Ключевой вывод
Три строки кода (`Buffered()`, `for` loop, `Flush()`) дали **37x** прирост. Pipeline убирает TCP round-trip — главный bottleneck.

---

## Фаза 2: TCMallocStore вместо ArenaStore

### Что сделали
1. **Добавили `WorkerID` в `ConnState`** — каждое соединение знает какой epoll-воркер его обслуживает
2. **Переключили `main.go`** — `store.NewArenaStore()` → `tcmalloc.NewTCMallocStore(runtime.NumCPU())`
3. **Создали `TCMallocEvictor`** — адаптер для TTLManager (bridge между `Del(key)` и `Del(workerID, key)`)
4. **Добавили `GetKeysInSlot`** в TCMallocStore — для кластерной миграции
5. **Все Set/Del вызовы** получили `workerID` — hot path использует per-worker MCache (lock-free), cold path (TTL, WASM, replication) использует worker 0

### Результат

| Тест | ArenaStore | TCMallocStore | Прирост |
|:-----|:-----------|:--------------|:--------|
| SET без pipeline | 65K | **77K** | **+18%** |
| SET P=16 | 655K | **922K** | **+41%** |
| SET P=64 | 1.14M | **1.35M** | **+19%** |
| GET P=128 200c | 2.42M | **2.41M** | ~0% |

### Ключевой вывод
SET выиграл больше всего (+19-41%) — TCMalloc убирает mutex на аллокацию. GET P=128 не вырос — bottleneck уже в TCP/epoll/syscall, не в store.

---

## Фаза 3: Ring Buffer + Zero-Alloc Parser

### Проблемы, которые решали

> [!IMPORTANT]
> **Проблема №1: Syscalls** — `SetReadDeadline` + `bufio.Read` + `bufio.Write` + `Flush` = 4+ syscall на команду.
>
> **Проблема №2: Копирование** — kernel → bufio(4KB) → make([]byte) → string() → Value → Marshal() → bufio → kernel = 6 копирований.
>
> **Проблема №3: GC** — парсер создавал `make([]byte)` + `string()` + `[]Value` = 3+ аллокации × 77K RPS = 230K мусорных объектов/сек.

### Что сделали

#### Файл: `internal/server/connbuf.go` (новый)
- **Read Buffer** — `[]byte` 64KB, линейный с компакцией (как Redis `c->querybuf`)
- **Zero-alloc RESP Parser** — `ParseCommand()` возвращает `[][]byte` — слайсы ВНУТРЬ read buffer. Ноль аллокаций
- **`parseIntBytes()`** — zero-alloc parseInt из `[]byte` (вместо `strconv.Atoi(string(...))` — убрали 2 heap-аллокации на команду)
- **`peekLine()`** — `bytes.IndexByte` (SIMD/AVX2) вместо ручного цикла
- **Write Buffer** — `[]byte` append-only. `WriteSimpleString/WriteBulk/WriteInt/WriteNull/WriteArrayHeader` — прямая RESP-кодировка без `protocol.Value` и `Marshal()`
- **`Flush()`** — один `conn.Write(wbuf)` для всех ответов
- **`TryRead()`** — non-blocking read через `RawConn.Read()` + `syscall.Read()` для greedy drain

#### Файл: `internal/server/server.go` (переписан)
- **`ConnState`** — `Buf *ConnBuf` вместо `Reader + Writer`
- **`Handler`** — `func(cs *ConnState, args [][]byte)` вместо `func(cs, args []Value) Value`. Handler больше не возвращает Value — пишет ответ прямо в `cs.Buf`
- **`handleConn`** — greedy drain loop: `ReadFromConn()` → parse all → `TryRead()` → parse more → `Flush()`
- **Убрали `SetReadDeadline`** из hot path — это был лишний syscall (+120% без pipeline)

#### Файл: `cmd/kvstore/main.go` (переписан)
- Все команды (SET, GET, DEL, EXPIRE, TTL, PERSIST, PubSub, WASM, VSIM) переписаны с `protocol.Value` на `[][]byte` + `cs.Buf.Write*()`
- **`writeValue()` helper** — мост для cluster API (`CheckKey`, `MigrateKey`), которые ещё возвращают `protocol.Value`
- Транзакции (MULTI/EXEC) — `args` копируются в `TxQueue` (ring buffer будет перезаписан)

### Результат

| Тест | bufio (Phase 2) | Ring Buffer Hybrid | Δ |
|:-----|:-----------------|:-------------------|:--|
| SET без pipeline | 77K | 66K | -14% |
| GET без pipeline | 78K | 66K | -15% |
| SET P=16 | 922K | 874K | -5% |
| GET P=16 | 976K | 890K | -9% |
| **SET P=64** | 1.35M | **1.49M** | **+10%** |
| GET P=64 | 2.17M | 2.2M | +1% |
| **GET P=128 200c** | 2.41M | **3.06M** | **+27% 🔥** |

### Ключевой вывод
Без pipeline -14% (TCP round-trip доминирует, Ring Buffer overhead не окупается). Pipeline P=128 **+27%** (3.06M RPS) — ring buffer + zero-alloc окупается под нагрузкой. Production = pipeline. **3M GET RPS — production-grade.**

---

## Разобранные концепции

### Почему аллокатор показывал миллионы, а сервер — 66K?
Аллокатор занимает **0.05%** от полного пути команды. 95% времени — TCP round-trip (ядро ОС, сетевой стек, epoll wakeup). Бенчмарк аллокатора мерил одну шестерёнку, а не весь конвейер.

### Pipeline vs без Pipeline
- Без pipeline: 1 команда = 1 round-trip (~100μs). Потолок: ~10K RPS/соединение
- С pipeline P=64: 64 команды = 1 round-trip. Выигрыш: 64x меньше round-trip'ов
- **Все production Redis-клиенты используют pipeline**: go-redis, Jedis, ioredis, Lettuce, redis-py

### 66K без pipeline — нормально?
Да. Redis (C) = 80-110K. Dragonfly (C++) = 80-100K. Наш Go = 66K. Разница — Go runtime overhead (GC checks, goroutine scheduler, string copy, net.Conn abstraction).

---

## Файлы проекта — что изменено

| Файл | Что изменено |
|:-----|:-------------|
| [connbuf.go](file:///home/nikolay/storage_in_memory/kvstore/internal/server/connbuf.go) | **НОВЫЙ** — Ring Buffer + Zero-alloc Parser + Direct Writer + TryRead |
| [server.go](file:///home/nikolay/storage_in_memory/kvstore/internal/server/server.go) | ConnState → ConnBuf, Handler → `[][]byte`, handleConn greedy drain |
| [main.go](file:///home/nikolay/storage_in_memory/kvstore/cmd/kvstore/main.go) | ArenaStore → TCMallocStore, все handlers на `[][]byte` + `cs.Buf.Write*()` |
| [evictor.go](file:///home/nikolay/storage_in_memory/kvstore/internal/store/tcmalloc/evictor.go) | **НОВЫЙ** — адаптер TTLManager для TCMallocStore |
| [store.go](file:///home/nikolay/storage_in_memory/kvstore/internal/store/tcmalloc/store.go) | +`GetKeysInSlot()`, +`DelSimple()` |

---

## Планы на будущее

### 1. SIMD EuclideanDistance (Vector Search)
В предыдущих CPU-профилях `EuclideanDistance` занимал **~49% CPU**. Текущая реализация — скалярный Go-цикл. План: использовать SIMD (AVX2/AVX-512) через Go assembly или CGo для 8-16x ускорения similarity search.

### 2. io_uring (Linux 5.1+)
Позволяет делать read/write **без syscall** через shared ring buffer с ядром. Может поднять non-pipeline RPS до ~100K+. Но в Go нет нативной поддержки — нужна C-библиотека или asm.

### 3. Custom Writer (вместо conn.Write)
Заменить `conn.Write(wbuf)` на прямой `syscall.Write(fd, wbuf)` для write path — убрать Go net.Conn overhead.

### 4. Prometheus метрики
Инструментировать pipeline depth, commands/syscall, greedy drain hit rate для fine-tuning буферов.

### 5. Connection-level Slowloris protection
Вернуть `SetReadDeadline` но не per-command, а per-connection (при Accept) — одна установка вместо двух syscall на каждую команду.
