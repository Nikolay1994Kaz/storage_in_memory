# 🗺️ Roadmap: High-Performance Storage Systems in Go

> Два проекта-тренажёра для глубокого понимания рантайма Go, системного программирования
> и внутреннего устройства баз данных.

---

## Порядок реализации

| # | Проект | Сложность | Фокус |
|---|--------|-----------|-------|
| 1 | **Zero-GC Sharded KV Store** | ⭐⭐⭐ | In-memory, сеть, GC, concurrency |
| 2 | **LSM-Tree Storage Engine** | ⭐⭐⭐⭐⭐ | Disk I/O, data structures, compaction |

Начинаем с KV Store — он проще, даёт быстрый видимый результат и обучает навыкам,
которые пригодятся во втором проекте.

---

# Проект 1: Zero-GC Sharded KV Store

> Сверхбыстрое in-memory хранилище — хардкорный аналог Redis.
> Цель: удерживать **10 000+ RPS** с миллионами записей и **GC pause < 100μs**.

## Структура проекта

```
kvstore/
├── cmd/
│   └── kvstore/
│       └── main.go              # Точка входа, запуск сервера
├── internal/
│   ├── protocol/
│   │   └── resp.go              # RESP-парсер (Уровень 0)
│   ├── store/
│   │   ├── naive.go             # Наивная реализация (Уровень 1)
│   │   ├── sharded.go           # Шардированное хранилище (Уровень 2)
│   │   └── arena.go             # Arena allocator + Zero-GC (Уровень 3)
│   ├── server/
│   │   ├── tcp.go               # Стандартный TCP-сервер
│   │   └── epoll.go             # Epoll Event Loop (Уровень 4)
│   └── wal/
│       └── wal.go               # Write-Ahead Log (Уровень 5)
├── bench/
│   ├── naive_test.go            # Бенчмарки Уровня 1
│   ├── sharded_test.go          # Бенчмарки Уровня 2
│   ├── arena_test.go            # Бенчмарки Уровня 3
│   └── server_test.go           # Бенчмарки Уровня 4
├── docs/
│   ├── profiles/                # Сохранённые pprof-профили
│   └── results/                 # Графики и результаты бенчмарков
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## Уровень 0: RESP-протокол

**Цель:** Реализовать парсер Redis Serialization Protocol (RESP2).

**Зачем:** Совместимость с `redis-cli` и `redis-benchmark`. Это даёт вам индустриальный
инструмент для тестирования, а не самодельные скрипты.

### Спецификация RESP2

```
Простые строки:  +OK\r\n
Ошибки:          -ERR unknown command\r\n
Целые числа:     :1000\r\n
Bulk-строки:     $5\r\nHello\r\n
Массивы:         *2\r\n$3\r\nGET\r\n$4\r\nname\r\n
NULL:             $-1\r\n
```

### Поддерживаемые команды (MVP)

| Команда | Описание |
|---------|----------|
| `SET key value` | Записать ключ |
| `GET key` | Прочитать ключ |
| `DEL key` | Удалить ключ |
| `PING` | Проверка жизни (ответ: `PONG`) |
| `DBSIZE` | Количество ключей |
| `FLUSHDB` | Очистить всё хранилище |

### Чему учит

- Работа с `bufio.Reader` для zero-copy парсинга
- Обработка бинарных протоколов на уровне байтов
- Написание state machine для парсинга потоковых данных

### Критерий успеха

```bash
# Запускаем наш сервер
./kvstore --port 6380

# Подключаемся стандартным клиентом Redis
redis-cli -p 6380
SET mykey "hello"    # → OK
GET mykey            # → "hello"
DEL mykey            # → (integer) 1
```

---

## Уровень 1: Наивная реализация

**Цель:** `map[string][]byte` + `sync.RWMutex` — как делают все.

### Что реализуем

```go
type NaiveStore struct {
    mu   sync.RWMutex
    data map[string][]byte
}
```

### Бенчмарки

Тестируем с тремя уровнями нагрузки:

| Горутин | Ожидание |
|---------|----------|
| 100 | Работает нормально |
| 1 000 | Начинаются просадки |
| 10 000 | Lock contention убивает throughput |

### Профилирование

```bash
# CPU-профиль
go test -bench=BenchmarkNaiveStore -cpuprofile=cpu.prof -count=1
go tool pprof cpu.prof

# Mutex contention
go test -bench=BenchmarkNaiveStore -mutexprofile=mutex.prof
go tool pprof mutex.prof
```

### Метрики для сравнения

- `ops/sec` — операций в секунду
- `ns/op` — наносекунд на операцию
- `B/op` — аллокаций в байтах на операцию
- `allocs/op` — количество аллокаций на операцию

### Чему учит

- `sync.RWMutex` и его ограничения
- `go tool pprof` — чтение CPU и mutex профилей
- Flame graphs — визуальный анализ горячих путей

### Ощущение боли

В `pprof` вы увидите, что при 10 000 горутинах **>80% CPU** тратится на
`runtime.lock` и `sync.(*RWMutex).Lock`. Полезной работы — почти ноль.

---

## Уровень 2: Шардирование

**Цель:** Разбить одну глобальную блокировку на 256 независимых.

### Архитектура

```go
const numShards = 256

type Shard struct {
    mu   sync.RWMutex
    data map[string][]byte
}

type ShardedStore struct {
    shards [numShards]Shard
}

func (s *ShardedStore) getShard(key string) *Shard {
    h := fnv.New32a()
    h.Write([]byte(key))
    return &s.shards[h.Sum32()%numShards]
}
```

### Хэш-функции: сравнение

| Функция | Скорость | Качество распределения |
|---------|----------|----------------------|
| FNV-1a | Быстрая | Хорошее |
| xxHash | Очень быстрая | Отличное |
| CRC32 | Средняя | Хорошее |

Рекомендация: начать с `FNV-1a` (стандартная библиотека), потом попробовать `xxHash`
для сравнения.

### Бенчмарки

Сравниваем с Уровнем 1 на тех же нагрузках (100 / 1 000 / 10 000 горутин).

Ожидаемый результат:
- **5–15x** прирост throughput на 10 000 горутин
- Mutex contention в профиле **практически исчезает**

### Новое ощущение боли

Шардирование решает проблему блокировок. Но теперь загружаем **20 миллионов ключей**:

```bash
# Наблюдаем GC pauses
GODEBUG=gctrace=1 ./kvstore
```

Вы увидите в логах GC:
```
gc 42 @18.523s 11%: 0.12+45.3+0.08 ms clock, ...
```

**45 миллисекунд** GC pause! При реальной нагрузке это значит потерю запросов.

---

## Уровень 3: Zero-GC Arena Allocator

**Цель:** Убрать указатели из структуры данных, ослепить сборщик мусора.

### Проблема

```
map[string][]byte  →  каждый string — это (ptr, len)
                   →  каждый []byte — это (ptr, len, cap)
                   →  20M записей = 40M указателей для GC
```

### Решение: три компонента

#### 1. Arena Allocator (хранение значений)

```go
type Arena struct {
    buf    []byte // гигантский непрерывный кусок памяти
    offset uint32 // текущая позиция записи
}

func (a *Arena) Alloc(data []byte) (offset uint32, size uint32) {
    offset = a.offset
    size = uint32(len(data))
    copy(a.buf[offset:], data)
    a.offset += size
    return
}
```

**Улучшение (mmap):** Вместо `make([]byte, size)` используем `syscall.Mmap`:

```go
func NewMmapArena(size int) (*Arena, error) {
    buf, err := syscall.Mmap(
        -1, 0, size,
        syscall.PROT_READ|syscall.PROT_WRITE,
        syscall.MAP_ANON|syscall.MAP_PRIVATE,
    )
    if err != nil {
        return nil, err
    }
    return &Arena{buf: buf}, nil
}
```

Преимущества `mmap`:
- Память **полностью невидима для GC** (вне Go heap)
- Возможность **persistence** — данные переживают рестарт (с `MAP_SHARED` + файл)
- Ленивая аллокация — ОС выделяет физическую память только при обращении

#### 2. Числовые ключи (убираем string из map)

```go
// Было:  map[string][]byte    — полно указателей
// Стало: map[uint64]uint32    — ноль указателей!

func hashKey(key string) uint64 {
    h := xxhash.Sum64String(key)
    return h
}
```

`map[uint64]uint32` — GC видит эту структуру как массив чисел и **не сканирует её**.

#### 3. Метаданные вместо значений в map

```go
type Entry struct {
    Offset uint32 // Смещение в арене
    Size   uint32 // Длина данных
}

// Финальная структура шарда:
type Shard struct {
    mu    sync.RWMutex
    index map[uint64]Entry  // Только числа — GC не трогает
    arena *Arena             // Один непрерывный буфер
}
```

### Бенчмарки

```bash
# Загружаем 20M ключей, сравниваем GC pause
GODEBUG=gctrace=1 go test -bench=BenchmarkArenaStore -benchtime=30s
```

Ожидаемый результат:
- GC pause: **45ms → <100μs** (сокращение в 500x)
- Throughput при 20M ключей: **стабильный**, без фризов

### Обработка коллизий хэшей

`uint64` может давать коллизии (хоть и редко: ~1 на 4 миллиарда при равномерном
распределении). Варианты:
1. **Игнорировать** — для кэша допустимо (как в memcached)
2. **Хранить оригинальный ключ в арене** и проверять при GET — надёжнее, но медленнее

### Чему учит

- `unsafe.Pointer` и работа с сырой памятью
- `syscall.Mmap` — системные вызовы Linux
- Memory layout и то, как GC сканирует heap
- Понимание, что "знание — это выбранный trade-off"

---

## Уровень 4: Epoll Event Loop (Reactor Pattern)

**Цель:** Обрабатывать 100 000+ одновременных соединений без 100 000 горутин.

### Проблема стандартного `net` пакета

```
1 соединение = 1 горутина = ~8KB стека
100 000 соединений = 100 000 горутин ≈ 800MB только на стеки
```

### Решение: Reactor Pattern

```
                    ┌─────────────┐
                    │   epoll_wait │  ← Один системный вызов
                    └──────┬──────┘
                           │
              ┌────────────┼────────────┐
              ▼            ▼            ▼
         ┌────────┐  ┌────────┐  ┌────────┐
         │Worker 1│  │Worker 2│  │Worker N│  ← Фиксированный пул
         └────────┘  └────────┘  └────────┘     (GOMAXPROCS штук)
              │            │            │
         ┌────┴────┐  ┌───┴───┐  ┌────┴────┐
         │conn 1-33│  │conn 34│  │conn 67-N│  ← Тысячи соединений
         │   ...   │  │ - 66  │  │   ...   │     на горутину
         └─────────┘  └───────┘  └─────────┘
```

### Ключевые системные вызовы

```go
// Создаём epoll instance
epfd, _ := syscall.EpollCreate1(0)

// Регистрируем файловый дескриптор сокета
syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{
    Events: syscall.EPOLLIN | syscall.EPOLLET,  // Edge-Triggered!
    Fd:     int32(fd),
})

// Ждём событий (неблокирующий мультиплексор)
events := make([]syscall.EpollEvent, 1024)
n, _ := syscall.EpollWait(epfd, events, -1)
for i := 0; i < n; i++ {
    handleConnection(events[i].Fd)
}
```

### Level-Triggered vs Edge-Triggered

| Режим | Поведение | Применение |
|-------|-----------|------------|
| Level-Triggered | epoll сигнализирует пока данные есть | Проще, но больше syscalls |
| Edge-Triggered | Сигнал только при **изменении** состояния | Эффективнее, но нужно читать до `EAGAIN` |

Рекомендация: начать с **Level-Triggered**, потом оптимизировать в **Edge-Triggered**.

### Бенчмарки

```bash
# Сравниваем стандартный net vs epoll
# 100 000 одновременных подключений
redis-benchmark -p 6380 -c 100000 -n 1000000 -t SET,GET
```

Ожидаемый результат:
- Потребление памяти: **800MB → ~50MB** (16x экономия)
- Latency p99: значительно стабильнее

---

## Уровень 5: WAL + Persistence (Бонус)

**Цель:** Данные переживают рестарт процесса.

### Write-Ahead Log

```
┌──────────────────────────────────────────┐
│              WAL File (.wal)             │
├──────┬──────┬───────┬──────┬─────────────┤
│ CRC  │ Len  │  Op   │ Key  │   Value     │
│ 4B   │ 4B   │  1B   │ var  │   var       │
├──────┴──────┴───────┴──────┴─────────────┤
│ Entry 1 │ Entry 2 │ Entry 3 │ ...        │
└──────────────────────────────────────────┘
```

### Батчинг записей (критическая оптимизация)

`fsync` — дорогая операция (~1-10ms на HDD, ~50-200μs на SSD).
Батчим записи: копим за 1ms, потом один `fsync` на всю пачку.

```go
type WAL struct {
    file      *os.File
    mu        sync.Mutex
    buf       *bufio.Writer
    syncEvery time.Duration  // например, 1ms
}
```

### Восстановление после падения

```
1. Открыть WAL-файл
2. Читать записи последовательно
3. Для каждой записи: проверить CRC → применить операцию к store
4. Продолжить работу
```

---

## Observability (на каждом уровне)

### Метрики для сбора

```go
type Metrics struct {
    OpsTotal      uint64        // Всего операций
    OpsPerSecond  float64       // Текущий RPS
    LatencyP50    time.Duration // 50-й перцентиль
    LatencyP95    time.Duration // 95-й перцентиль
    LatencyP99    time.Duration // 99-й перцентиль
    GCPauseNs     uint64        // Последняя GC-пауза
    HeapAllocMB   float64       // Потребление heap
    GoroutineCount int          // Активные горутины
}
```

### Инструменты

| Инструмент | Что показывает |
|------------|---------------|
| `go tool pprof` (CPU) | Где тратится процессорное время |
| `go tool pprof` (mutex) | Contention на блокировках |
| `go tool pprof` (heap) | Кто аллоцирует память |
| `GODEBUG=gctrace=1` | Длительность GC пауз |
| `perf stat` | Кэш-промахи CPU (L1/L2/L3) |
| Flame Graph | Визуальный стек вызовов |

### Таблица результатов (заполняем по мере прохождения уровней)

| Метрика | Уровень 1 | Уровень 2 | Уровень 3 | Уровень 4 |
|---------|-----------|-----------|-----------|-----------|
| ops/sec (10K горутин) | ? | ? | ? | ? |
| p99 latency | ? | ? | ? | ? |
| GC pause | ? | ? | ? | ? |
| Память (20M ключей) | ? | ? | ? | ? |

---

---

# Проект 2: LSM-Tree Storage Engine

> Полноценный storage engine для embeddable базы данных — свой аналог LevelDB/RocksDB.
> Цель: понять, как **на самом деле** работают 90% современных баз данных.

## Почему это сложнее KV Store?

| Аспект | KV Store | LSM-Tree Engine |
|--------|----------|----------------|
| Хранение | Только RAM | RAM + диск |
| Структуры данных | HashMap | SkipList + SSTable + Bloom Filter |
| Consictency | Простая | WAL + MVCC |
| Compaction | Нет | Merge-sort SSTables |
| Range queries | Нет | Полная поддержка |
| Сложность | ⭐⭐⭐ | ⭐⭐⭐⭐⭐ |

## Архитектура LSM-Tree

```
                     Write Path                          Read Path
                         │                                   │
                         ▼                                   ▼
                   ┌──────────┐                        ┌──────────┐
                   │   WAL    │                        │ MemTable │──── Hit? → Return
                   └────┬─────┘                        └────┬─────┘
                        │                                   │ Miss
                        ▼                                   ▼
                   ┌──────────┐                    ┌────────────────┐
                   │ MemTable │ ── Full? ──►       │ Bloom Filter   │──── No? → Skip
                   │(SkipList)│    Flush            └───────┬────────┘
                   └──────────┘      │                      │ Maybe
                                     ▼                      ▼
                              ┌─────────────┐       ┌─────────────┐
                              │  SSTable L0 │       │  SSTable L0 │──── Binary Search
                              └──────┬──────┘       └──────┬──────┘
                                     │                      │ Miss
                                     ▼                      ▼
                              ┌─────────────┐       ┌─────────────┐
                              │  SSTable L1 │       │  SSTable L1 │
                              └──────┬──────┘       └──────┬──────┘
                                     │                      │
                              Compaction               ... и так далее
```

## Уровни реализации

### Уровень 1: MemTable (Skip List)

**Skip List** — вероятностная структура данных, альтернатива сбалансированным деревьям.

```
Level 3: ──────────────────────────► 50 ──────────────────────► NIL
Level 2: ──────► 10 ──────────────► 50 ──────────────────────► NIL
Level 1: ──────► 10 ──► 25 ──────► 50 ──────► 70 ──────────► NIL
Level 0: 5 ──► 10 ──► 25 ──► 30 ► 50 ──► 60 ► 70 ──► 90 ──► NIL
```

Преимущества перед `map`:
- **Упорядоченность** — O(log n) для range queries
- **Lock-free возможность** — concurrent skiplist без мьютексов
- **Предсказуемость** — нет resize как у hash map

### Уровень 2: SSTable (Sorted String Table)

Immutable файл на диске с отсортированными парами key-value.

```
┌─────────────────────────────────────────────┐
│                SSTable File                  │
├─────────────┬────────────┬──────────────────┤
│ Data Block  │ Data Block │   Index Block    │
│  (4KB)      │  (4KB)     │  (key→offset)   │
├─────────────┴────────────┴──────────────────┤
│              Footer (magic, offsets)         │
└─────────────────────────────────────────────┘
```

### Уровень 3: Bloom Filter

Вероятностная структура, отвечающая на вопрос "элемент **точно нет** в множестве"
с false positive rate ~1%.

```go
type BloomFilter struct {
    bits    []uint64
    numHash uint
}

// Может сказать "возможно есть" (нужна проверка) или "точно нет" (пропускаем SSTable)
```

Экономит **90%+ дисковых чтений** при point queries.

### Уровень 4: Compaction

Merge-sort SSTables из уровня L0 → L1 → L2 → ...

```
Compaction: соединяем и пересортировываем SSTables

L0: [SST-1] [SST-2] [SST-3]   ← Могут перекрываться
          ╲    │    ╱
           Merge Sort
              │
              ▼
L1: [    SST-merged    ]       ← Не перекрываются
```

Стратегии:
- **Size-Tiered** (Cassandra): проще, больше write amplification
- **Leveled** (LevelDB): сложнее, лучше read amplification

### Уровень 5: MVCC (Multi-Version Concurrency Control)

Каждая запись имеет версию (timestamp). Это позволяет:
- Читать без блокировок (snapshot isolation)
- Поддерживать транзакции
- Делать point-in-time recovery

### Уровень 6: Transactions

```go
tx := db.Begin()
tx.Set("account:1", balance1)
tx.Set("account:2", balance2)
tx.Commit()  // Атомарная запись всех изменений
```

## Структура проекта

```
lsmdb/
├── cmd/
│   └── lsmdb/
│       └── main.go
├── internal/
│   ├── memtable/
│   │   └── skiplist.go         # Skip List
│   ├── sstable/
│   │   ├── writer.go           # Запись SSTable на диск
│   │   ├── reader.go           # Чтение SSTable с диска
│   │   └── block.go            # Data Block encoding
│   ├── bloom/
│   │   └── filter.go           # Bloom Filter
│   ├── wal/
│   │   └── wal.go              # Write-Ahead Log
│   ├── compaction/
│   │   ├── leveled.go          # Leveled compaction strategy
│   │   └── manager.go          # Compaction scheduler
│   ├── manifest/
│   │   └── manifest.go         # Метаданные о файлах SSTables
│   └── db/
│       ├── db.go               # Главный API: Open, Get, Set, Delete
│       ├── iterator.go         # Range scans
│       └── snapshot.go         # MVCC snapshots
├── bench/
├── go.mod
└── README.md
```

## Навыки, которые даёт этот проект

| Навык | Где применяется |
|-------|----------------|
| Структуры данных (SkipList, BloomFilter, B-Tree) | Движки БД, поисковые системы |
| Disk I/O оптимизация (page alignment, buffered writes) | Любая система с persistence |
| Compaction и merge-sort на файлах | Hadoop, Spark, Cassandra |
| MVCC и snapshot isolation | PostgreSQL, CockroachDB |
| Binary encoding (varint, block format) | Протоколы, сериализация |
| Crash recovery (WAL replay) | Любая надёжная система |

---

# Рекомендуемый порядок работы

```
Месяц 1-2: KV Store (Уровни 0-3)
    → Парсер RESP
    → Наивная реализация + профилирование
    → Шардирование
    → Arena allocator + Zero-GC

Месяц 2-3: KV Store (Уровни 4-5)
    → Epoll Event Loop
    → WAL + Persistence

Месяц 3-5: LSM-Tree (Уровни 1-4)
    → SkipList MemTable
    → SSTable (write + read)
    → Bloom Filter
    → Compaction

Месяц 5-6: LSM-Tree (Уровни 5-6)
    → MVCC
    → Transactions
```

---

# Полезные ресурсы

## KV Store
- [Redis Protocol (RESP)](https://redis.io/docs/reference/protocol-spec/)
- [Go pprof guide](https://go.dev/blog/pprof)
- [GopherCon: Manual Memory Management in Go](https://www.youtube.com/watch?v=EceLLh1FUHI)
- [go-memdb](https://github.com/hashicorp/go-memdb) — пример in-memory DB
- [epollet example](https://man7.org/linux/man-pages/man7/epoll.7.html)

## LSM-Tree
- [LevelDB Implementation Notes](https://github.com/google/leveldb/blob/main/doc/impl.md)
- [WiscKey: Separating Keys from Values](https://www.usenix.org/system/files/conference/fast16/fast16-papers-lu.pdf)
- [Designing Data-Intensive Applications](https://dataintensive.net/) — глава 3 (Storage & Retrieval)
- [badger](https://github.com/dgraph-io/badger) — LSM на Go (эталон для изучения)
- [pebble](https://github.com/cockroachdb/pebble) — LSM от CockroachDB

---

> **Начинаем с Проекта 1, Уровень 0 (RESP-протокол).**
