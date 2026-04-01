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

## Уровень 5: WAL + Persistence ✅

**Цель:** Данные переживают рестарт процесса.

### Реализовано

- Binary WAL с CRC32 чексуммой для crash recovery
- `bufio.Writer` для батчинга записей (zero-syscall на уровне записи)
- `Syncer` — фоновый fsync каждые 100ms
- **Log Rotation** — мгновенное переключение WAL-файла (наносекунды под локом)
- **Background Snapshot** — неблокирующая компактизация в фоновой горутине
- **Auto-Compact** — автоматическая компактизация при размере WAL > 64MB
- `atomic.Bool` для безопасного флага компактизации
- `PutUint32` вместо `binary.Write` — zero-alloc, zero-reflect

### Восстановление при старте

```
1. Читаем snapshot.wal (если есть)
2. Читаем все wal_*.log по порядку
3. Применяем каждую запись к store
= Полное восстановление
```

---

## Уровень 6: TTL + Key Expiration

**Цель:** Ключи автоматически удаляются после заданного времени жизни.

### Новые команды

| Команда | Описание |
|---------|----------|
| `SET key value EX seconds` | Записать ключ с временем жизни |
| `EXPIRE key seconds` | Установить TTL на существующий ключ |
| `TTL key` | Сколько секунд осталось до удаления |
| `PERSIST key` | Убрать TTL (ключ живёт вечно) |

### Архитектура

```
                     ┌───────────────────────┐
                     │   MinHeap (по времени) │
                     │ ┌────────────────────┐│
                     │ │ 12:00:05 → key_1   ││ ← ближайший к удалению
                     │ │ 12:00:10 → key_2   ││
                     │ │ 12:01:00 → key_3   ││
                     │ └────────────────────┘│
                     └───────────┬───────────┘
                                │
          Фоновая горутина (каждые 100ms):
            while heap.top().expires_at < now:
                store.Del(heap.pop().key)
```

### Два подхода к удалению (как в Redis)

| Подход | Описание | Плюсы | Минусы |
|--------|----------|-------|--------|
| **Lazy** | Проверяем TTL при GET | Простой, без overhead | Мёртвые ключи занимают RAM |
| **Active** | Фоновая горутина + MinHeap | Чистит RAM | CPU overhead |

Реализуем **оба** (как Redis): lazy при каждом GET + active в фоне.

### Структура данных

```go
type TTLEntry struct {
    Key       string
    ExpiresAt time.Time
    Index     int        // позиция в heap (для O(log n) удаления)
}

type TTLHeap []*TTLEntry  // реализует container/heap.Interface
```

### Чему учит

- `container/heap` — приоритетная очередь в стандартной библиотеке Go
- Lazy vs Active expiration — trade-off CPU vs RAM
- Интеграция TTL с WAL (для persistence)

---

## Уровень 7: Pub/Sub (Publish/Subscribe)

**Цель:** Клиенты могут подписываться на каналы и получать сообщения в реальном времени.

### Новые команды

| Команда | Описание |
|---------|----------|
| `SUBSCRIBE channel [channel ...]` | Подписаться на канал(ы) |
| `UNSUBSCRIBE [channel ...]` | Отписаться |
| `PUBLISH channel message` | Отправить сообщение всем подписчикам |

### Архитектура

```
Publisher                         Subscribers
─────────                         ───────────

  PUBLISH "chat" "hello"
       │
       ▼
  ┌─────────────────────┐
  │     PubSub Hub      │
  │                     │
  │  channels:          │
  │   "chat" → [c1, c2] ──────► conn1: *3\r\n$7\r\nmessage\r\n...
  │   "logs" → [c3]     │       conn2: *3\r\n$7\r\nmessage\r\n...
  └─────────────────────┘
```

### Ключевые решения

```go
type PubSub struct {
    mu       sync.RWMutex
    channels map[string]map[*Subscriber]struct{} // channel → set of subscribers
}

type Subscriber struct {
    ch   chan protocol.Value  // буферизованный канал для сообщений
    conn net.Conn
}
```

### Особенности реализации

1. **Subscriber переходит в read-only режим** — после SUBSCRIBE клиент не может отправлять другие команды (как в Redis)
2. **Буферизованный канал** — если подписчик медленный, сообщения копятся в буфере
3. **Backpressure** — при переполнении буфера отключаем медленного подписчика
4. **Pub/Sub НЕ пишется в WAL** — сообщения ephemeral (как в Redis)

### Чему учит

- Go channels для межгорутинной коммуникации
- Fan-out паттерн (одно сообщение → множество получателей)
- Backpressure и обработка медленных потребителей
- Изменение протокола взаимодействия (push vs pull)

---

## Уровень 8: Кластеризация (Distributed KV Store)

**Цель:** Несколько нод работают как единое хранилище, данные распределены между ними.

### Архитектура кластера

```
                    ┌─────────── Client ───────────┐
                    │  SET user:1 → hash(user:1)   │
                    │             = slot 5649       │
                    │             → Node B          │
                    └──────────────┬────────────────┘
                                   │
           ┌───────────────────────┼───────────────────────┐
           ▼                       ▼                       ▼
     ┌──────────┐           ┌──────────┐           ┌──────────┐
     │  Node A  │           │  Node B  │           │  Node C  │
     │ slots    │           │ slots    │           │ slots    │
     │ 0-5460   │           │ 5461-10922│          │10923-16383│
     └──────────┘           └──────────┘           └──────────┘
```

### Consistent Hashing (16384 слотов)

```go
func KeySlot(key string) uint16 {
    return crc16(key) % 16384
}
```

Каждая нода отвечает за диапазон слотов (как в Redis Cluster).

### Межнодовое взаимодействие

| Компонент | Описание |
|-----------|----------|
| **Gossip Protocol** | Ноды обмениваются информацией о топологии |
| **MOVED redirect** | Если ключ на другой ноде — клиент получает `-MOVED slot host:port` |
| **Slot Migration** | Перенос слотов между нодами при масштабировании |

### Новые команды

| Команда | Описание |
|---------|----------|
| `CLUSTER NODES` | Показать все ноды кластера |
| `CLUSTER SLOTS` | Показать распределение слотов |
| `CLUSTER MEET host port` | Добавить ноду в кластер |

### Чему учит

- Распределённые системы и CAP-теорема
- Consistent hashing
- Gossip protocol
- Обработка network partition
- Leader election (упрощённый)

---

## Уровень 9: Tunable Guarantees (Переключаемые гарантии)

**Цель:** Клиент сам выбирает уровень гарантий для каждой операции.
По умолчанию — быстрый режим (как Redis). По запросу — транзакции с `fsync`.

> Паттерн из индустрии: Cassandra (`CONSISTENCY LEVEL`), DynamoDB (`ConsistentRead`),
> MongoDB (`writeConcern`), Redis (`WAIT`). Все дают клиенту "ручку" —
> "мне сейчас нужна скорость" или "мне сейчас нужна надёжность".

### Зачем

Один KVStore, два сценария:

```
Сценарий 1: Кэш / сессии (99% трафика)
  SET user:session "abc123"      → буфер WAL, fsync через 100ms
  Скорость: ~500K ops/sec
  Потеряли данные? Не страшно — перечитаем из основной БД.

Сценарий 2: Критичная операция (1% трафика)
  TXBEGIN
  SET account:1 "900"
  SET account:2 "1100"
  TXCOMMIT                       → atomарная запись + fsync СЕЙЧАС
  Скорость: ~10K ops/sec (fsync дорог)
  Гарантия: всё или ничего, данные на диске.
```

### Новые команды

| Команда | Описание |
|---------|----------|
| `TXBEGIN` | Начать транзакцию (команды буферизуются) |
| `TXCOMMIT` | Применить все команды атомарно + fsync |
| `TXDISCARD` | Отменить транзакцию (очистить буфер) |

### Архитектура

```
  Обычный SET (fast path):
  ─────────────────────────
  Client → SET key value → WAL.Write(entry) → Store.Set() → +OK
                            └── буфер, fsync позже (100ms)

  Транзакция (safe path):
  ─────────────────────────
  Client → TXBEGIN
         → SET key1 val1     → буфер (не применяется)
         → SET key2 val2     → буфер (не применяется)
         → TXCOMMIT
              │
              ▼
         WAL.WriteBatch([entry1, entry2])  ← одна запись в WAL
              │
              ▼
         WAL.Sync()                        ← fsync СЕЙЧАС
              │
              ▼
         Store.Set(key1, val1)             ← применяем ВСЕ
         Store.Set(key2, val2)             ← или НИЧЕГО
              │
              ▼
         +OK
```

### Ключевые структуры

```go
// Transaction — буфер операций одного клиента.
type Transaction struct {
    ops     []wal.Entry    // накопленные операции
    active  bool           // true после TXBEGIN
}

// WAL Batch — группа записей как один атомарный блок.
// При чтении: если batch не полный (crash) — пропускаем ВЕСЬ batch.
func (w *WAL) WriteBatch(entries []Entry) error {
    w.mu.Lock()
    defer w.mu.Unlock()

    // Один CRC32 на весь batch — если хоть один байт повреждён,
    // отбрасываем все операции.
    payload := encodeBatch(entries)
    checksum := crc32.ChecksumIEEE(payload)
    // ...
}
```

### Граница реализации (что НЕ делаем)

| Делаем ✅ | Не делаем ❌ |
|-----------|-------------|
| Атомарный batch write | ROLLBACK с восстановлением старых значений |
| fsync при TXCOMMIT | MVCC (это для LSM-Tree Проекта 2) |
| Буферизация до COMMIT | Deadlock detection между транзакциями |
| Batch CRC32 | Serializable isolation с конфликтами |

### Чему учит

- Tunable consistency — ключевой паттерн распределённых систем
- Batch WAL writes — амортизация стоимости fsync
- Per-connection state management (буфер транзакции на клиента)
- Trade-off: latency vs durability на уровне API
- Паттерны Cassandra/DynamoDB/MongoDB в своём проекте

---

## Observability (на каждом уровне)

### Метрики для сбора

```go
type Metrics struct {
    OpsTotal       uint64        // Всего операций
    OpsPerSecond   float64       // Текущий RPS
    LatencyP50     time.Duration // 50-й перцентиль
    LatencyP95     time.Duration // 95-й перцентиль
    LatencyP99     time.Duration // 99-й перцентиль
    GCPauseNs      uint64        // Последняя GC-пауза
    HeapAllocMB    float64       // Потребление heap
    GoroutineCount int           // Активные горутины
    ExpiredKeys    uint64        // Удалённых по TTL ключей
    PubSubChannels int           // Активных каналов
    PubSubClients  int           // Подписчиков
    ClusterNodes   int           // Нод в кластере
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
| ops/sec (10K горутин) | ~2.4M | ~11.2M | ~5.4M | 234K (network) |
| p99 latency | — | — | — | 155ms (10K conn) |
| GC pause (5M keys) | 54ms | 54ms | 833μs | 833μs |
| Память (5M ключей) | 461MB | 461MB | 1169MB | 1169MB |

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
Месяц 1-2: KV Store (Уровни 0-3) ✅
    → Парсер RESP
    → Наивная реализация + профилирование
    → Шардирование
    → Arena allocator + Zero-GC

Месяц 2-3: KV Store (Уровни 4-5) ✅
    → Epoll Event Loop (per-worker, Round Robin)
    → WAL + Log Rotation + Background Snapshot

Месяц 3: KV Store (Уровни 6-8) ← ТЫ ЗДЕСЬ
    → TTL + Key Expiration (MinHeap, lazy + active)
    → Pub/Sub (Fan-out, backpressure)
    → Кластеризация (Consistent Hashing, Gossip, MOVED)

Месяц 3-4: KV Store (Уровень 9)
    → Tunable Guarantees (TXBEGIN/TXCOMMIT/TXDISCARD)
    → WAL Batch writes + per-operation fsync
    → Bugfixes: WAL error handling, TTL persistence

Месяц 4-6: LSM-Tree (Уровни 1-4)
    → SkipList MemTable
    → SSTable (write + read)
    → Bloom Filter
    → Compaction

Месяц 6-7: LSM-Tree (Уровни 5-6)
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
