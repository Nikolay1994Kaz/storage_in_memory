# Semantic Pub/Sub — Векторная маршрутизация сообщений

## Содержание

1. [Концепция](#концепция)
2. [Архитектура системы](#архитектура-системы)
3. [Классический Pub/Sub vs Semantic Pub/Sub](#классический-pubsub-vs-semantic-pubsub)
4. [Компоненты и взаимодействие](#компоненты-и-взаимодействие)
5. [Подробный data flow](#подробный-data-flow)
6. [Внутренние структуры данных](#внутренние-структуры-данных)
7. [RESP протокол и команды](#resp-протокол-и-команды)
8. [Concurrency модель](#concurrency-модель)
9. [Производительность](#производительность)
10. [Интеграция в main.go](#интеграция-в-maingo)
11. [Примеры использования](#примеры-использования)

---

## Концепция

### Проблема классического Pub/Sub

В классическом Pub/Sub маршрутизация работает по **точному совпадению имени канала**:

```
SUBSCRIBE "ml-news"     → получает ВСЕ сообщения из канала "ml-news"
SUBSCRIBE "cooking"     → получает ВСЕ сообщения из канала "cooking"
```

Это бинарный выбор: получаешь ВСЁ из канала или НИЧЕГО. Нет понятия "похожести" или "близости" между темами.

### Решение: маршрутизация по семантической близости

Semantic Pub/Sub заменяет строковые имена каналов на **вектора**:

```
VSIM.SUBSCRIBE 0.5 1.0 0.0 0.0    → интерес: "машинное обучение" (вектор)
VSIM.PUBLISH "GPT-5!" 0.9 0.1 0.0 → сообщение с вектором темы
```

Вместо точного совпадения строк — **поиск ближайших соседей в HNSW-графе**.
Подписчик получает сообщение, если расстояние между его вектором интересов и вектором сообщения **≤ threshold**.

### Ключевое решение: клиент предоставляет вектора

Вектора вычисляются **на стороне клиента** (через Ollama, OpenAI, или любой embedding model).
Сервер **не вызывает LLM** на горячем пути — он только сравнивает готовые вектора через HNSW.

Это критически важно для производительности:
- Ollama embedding: ~10-50ms на запрос
- HNSW Search: ~1-30µs на запрос
- Разница: **1000×**

---

## Архитектура системы

```
┌─────────────────────────────────────────────────────────────────┐
│                        KVStore Server                            │
│                                                                  │
│  ┌────────────┐     ┌─────────────────────────────────────┐     │
│  │ Classic Hub │     │          SemanticHub                 │     │
│  │             │     │                                     │     │
│  │ channels:   │     │  ┌─────────────┐  ┌─────────────┐  │     │
│  │  "news" → ──┤     │  │ HNSW Index  │  │ Subscribers  │  │     │
│  │   [sub1,2]  │     │  │ (VectorStore)│  │  subs map   │  │     │
│  │  "chat" → ──┤     │  │             │  │  conns map  │  │     │
│  │   [sub3]    │     │  │ Vec1 ──→Sub1│  │             │  │     │
│  │             │     │  │ Vec2 ──→Sub2│  │ Sub1{conn,  │  │     │
│  │ Маршрутиз.: │     │  │ Vec3 ──→Sub3│  │   vec,      │  │     │
│  │ СТРОКА==    │     │  │             │  │   threshold} │  │     │
│  │ СТРОКА      │     │  │ Маршрутиз.: │  │             │  │     │
│  │             │     │  │ HNSW K-NN + │  │             │  │     │
│  │ O(1) lookup │     │  │ threshold   │  │             │  │     │
│  └────────────┘     │  └─────────────┘  └─────────────┘  │     │
│                      └─────────────────────────────────────┘     │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐     │
│  │                    TCMallocStore                          │     │
│  │  Shared allocator: KV data + HNSW nodes + neighbors      │     │
│  └─────────────────────────────────────────────────────────┘     │
└─────────────────────────────────────────────────────────────────┘
```

Система состоит из двух независимых подсистем Pub/Sub, которые сосуществуют в одном сервере:

| | Classic Hub | SemanticHub |
|---|---|---|
| Маршрутизация | Точное совпадение строки | K-NN поиск в HNSW + threshold |
| Подписка | `SUBSCRIBE channel1 channel2` | `VSIM.SUBSCRIBE 0.5 v1 v2 ... vN` |
| Публикация | `PUBLISH channel message` | `VSIM.PUBLISH message v1 v2 ... vN` |
| Сложность Publish | O(1) — lookup в map | O(log N) — HNSW search |
| Allocator | — | TCMallocStore (shared с KV) |

---

## Классический Pub/Sub vs Semantic Pub/Sub

### Classic Hub: строковая маршрутизация

```
                PUBLISH "ml-news" "GPT-5 released!"
                         │
                         ▼
              ┌──────────────────┐
              │    Hub.channels   │
              │                  │
              │ "ml-news" → map{ │
              │   sub1: struct{},│──────→ sub1.ch ← message
              │   sub2: struct{} │──────→ sub2.ch ← message
              │ }                │
              │                  │
              │ "cooking" → map{ │
              │   sub3: struct{} │       (НЕ получает — другой канал)
              │ }                │
              └──────────────────┘
```

Алгоритм:
1. `h.channels["ml-news"]` → O(1) lookup в map
2. Итерация по подписчикам канала → O(K) где K = кол-во подписчиков
3. Non-blocking send в канал каждого подписчика
4. Общая сложность: **O(K)**

### SemanticHub: векторная маршрутизация

```
        VSIM.PUBLISH "GPT-5 released!" 0.9 0.1 0.0 0.0
                         │
                         ▼
              ┌──────────────────────────────────┐
              │     SemanticHub                   │
              │                                   │
              │  1. HNSW Search([0.9,0.1,0,0], K)│
              │     ┌────────────────────┐       │
              │     │   HNSW Index       │       │
              │     │                    │       │
              │     │  ●[1,0,0,0]  sub1  │←─ dist=0.02 ≤ 0.5 ✅
              │     │  ●[0,0,0,1]  sub2  │←─ dist=0.98 > 0.5 ❌
              │     │  ●[0.5,.5,.5,.5] s3│←─ dist=0.42 ≤ 2.0 ✅
              │     └────────────────────┘       │
              │                                   │
              │  2. Фильтр: dist ≤ sub.threshold │
              │     sub1: 0.02 ≤ 0.5 → ✅ deliver│
              │     sub2: 0.98 > 0.5 → ❌ skip   │
              │     sub3: 0.42 ≤ 2.0 → ✅ deliver│
              │                                   │
              │  3. Доставка: sub1.ch, sub3.ch    │
              └──────────────────────────────────┘
```

Алгоритм:
1. HNSW Search по индексу подписчиков → O(log N × M) где N = подписчики, M = параметр HNSW
2. Фильтрация по threshold каждого подписчика → O(K)
3. Non-blocking send → O(K)
4. Общая сложность: **O(log N × M + K)**

---

## Компоненты и взаимодействие

### Слои системы

```
┌───────────────────────────────────────────────────────────┐
│ Слой 1: RESP Protocol (main.go)                           │
│ Парсинг команд VSIM.SUBSCRIBE / VSIM.PUBLISH             │
│ Конвертация [][]byte → []float32 + string                 │
├───────────────────────────────────────────────────────────┤
│ Слой 2: SemanticHub (semantic.go)                         │
│ Управление подписчиками, маршрутизация, доставка          │
│ RWMutex для concurrent publish + exclusive subscribe      │
├───────────────────────────────────────────────────────────┤
│ Слой 3: VectorStore (store.go)                            │
│ Обёртка над HNSW: Add/Delete/Search с string ключами      │
│ Cosine distance через pre-normalization + DotProduct      │
├───────────────────────────────────────────────────────────┤
│ Слой 4: HNSW Graph (graph.go)                             │
│ Навигационный граф: searchLayer, greedyClosest, Insert     │
│ AVX2+FMA distance compute, batch processing, sync.Pool    │
├───────────────────────────────────────────────────────────┤
│ Слой 5: TCMallocStore                                     │
│ Управление памятью: арена для векторов, handles для связей │
│ Lock-free GET, per-worker MCache, deferred free            │
└───────────────────────────────────────────────────────────┘
```

### Зависимости между компонентами

```
main.go
  │
  ├── hub = pubsub.NewHub()                    // Классический Pub/Sub
  │
  ├── s = tcmalloc.NewTCMallocStore(NumCPU)    // Аллокатор памяти
  │     │
  │     └── semanticIndex = vector.NewVectorStoreCosine(s)
  │           │
  │           ├── graph = NewGraph(DotProductDistance, s)
  │           │     │
  │           │     ├── arena = VectorArena       // Хранение float32 данных
  │           │     └── allocator = s             // TCMalloc для neighbors
  │           │
  │           └── lsh = NewLSHIndex(dim, 42)      // LSH для dim≥256
  │
  └── semHub = pubsub.NewSemanticHub(semanticIndex)
        │
        ├── index = semanticIndex     // ← тот же VectorStore
        ├── subs = map[string]*SemanticSub
        └── conns = map[net.Conn]*SemanticSub
```

**Ключевой момент**: `SemanticHub` использует **отдельный** `VectorStore` (semanticIndex),
не общий `vecStore` для данных пользователя. Это изолирует подписки от пользовательских
векторов и позволяет использовать Cosine distance для подписок, даже если основной
индекс использует Euclidean.

---

## Подробный data flow

### VSIM.SUBSCRIBE — регистрация подписчика

```
Клиент → TCP → RESP Parser → main.go handler → SemanticHub.Subscribe()

Шаг за шагом:

1. Клиент отправляет:
   *4\r\n$14\r\nVSIM.SUBSCRIBE\r\n$3\r\n0.5\r\n$3\r\n1.0\r\n$3\r\n0.0\r\n

2. RESP Parser (protocol/) декодирует в:
   args = [[]byte("VSIM.SUBSCRIBE"), []byte("0.5"), []byte("1.0"), []byte("0.0")]

3. main.go handler:
   cmd = "VSIM.SUBSCRIBE"
   threshold = 0.5
   vec = []float32{1.0, 0.0}

4. semHub.Subscribe(cs.Conn, vec, threshold):

   4a. sh.mu.Lock()                          // Exclusive lock
   
   4b. Если conn уже подписан → removeLocked(old)
       ├── sh.index.Delete(old.key)          // Удалить вектор из HNSW
       ├── close(old.done)                   // Остановить writePump
       ├── delete(sh.subs, old.key)
       └── delete(sh.conns, old.conn)
   
   4c. id = sh.nextID.Add(1)                 // Атомарный счётчик
       key = "__sem:42"                      // Уникальный ключ в HNSW
   
   4d. sh.index.Add(key, vec):               // VectorStore.Add()
       ├── vs.mu.Lock()
       ├── Normalize(vec)                    // Pre-normalization (cosine)
       ├── graph.Insert(normalizedVec):      // HNSW Insert
       │   ├── level = randomLevel()         // Случайный уровень (0-4)
       │   ├── arena.Allocate(vec)           // Сохранить вектор в арене
       │   ├── allocator.Alloc(blockSize)    // TCMalloc: блок для neighbors
       │   ├── greedyClosest(верхние слои)   // Навигация к нужному региону
       │   └── searchLayer + connect(слой 0) // Подключение к соседям
       ├── vs.ids[key] = nodeID
       ├── vs.keys[nodeID] = key
       └── vs.mu.Unlock()
   
   4e. sub = &SemanticSub{
           ch:        make(chan protocol.Value, 256),
           conn:      conn,
           done:      make(chan struct{}),
           key:       "__sem:42",
           threshold: 0.5,
       }
   
   4f. sh.subs["__sem:42"] = sub
       sh.conns[conn] = sub
   
   4g. go sub.writePump()                    // Запуск горутины записи
   
   4h. sub.ch <- confirm                     // Подтверждение подписки
   
   4i. sh.mu.Unlock()

5. writePump() отправляет confirm в TCP:
   *3\r\n$18\r\nsemantic-subscribe\r\n$2\r\nOK\r\n:1\r\n
```

### VSIM.PUBLISH — публикация с маршрутизацией

```
Клиент → TCP → RESP Parser → main.go handler → SemanticHub.Publish()

Шаг за шагом:

1. Клиент отправляет:
   VSIM.PUBLISH "GPT-5 released!" 0.9 0.1

2. main.go handler:
   message = "GPT-5 released!"
   queryVec = []float32{0.9, 0.1}

3. semHub.Publish(queryVec, message):

   ╔═══════════════════════════════════════════╗
   ║ ФАЗА 1: Поиск подписчиков (под RLock)    ║
   ╚═══════════════════════════════════════════╝
   
   3a. sh.mu.RLock()                          // Shared lock (параллельно с другими Publish)
   
   3b. subCount = len(sh.subs)                // Быстрая проверка: есть ли подписчики?
       if subCount == 0 → return 0
   
   3c. sh.index.Search(queryVec, subCount):   // VectorStore.Search()
       ├── vs.mu.RLock()
       ├── Normalize(queryVec)                // Pre-normalization
       ├── graph.Search(query, K, efSearch):  // HNSW Search
       │   │
       │   │  ★ ЭТО ГОРЯЧИЙ ПУТЬ — 99% ВРЕМЕНИ ЗДЕСЬ ★
       │   │
       │   ├── greedyClosest(верхние слои)    // Навигация по express-слоям
       │   │   └── for each level > 0:
       │   │       └── follow best neighbor (no pool/heap)
       │   │
       │   └── searchLayer(слой 0, efSearch): // Полный поиск на слое 0
       │       ├── searchState = pool.Get()   // sync.Pool: zero-alloc
       │       ├── visited = bitset           // Bitset visited nodes
       │       ├── candidates = minHeap       // Очередь кандидатов
       │       ├── results = maxHeap          // K лучших результатов
       │       │
       │       └── while candidates not empty:
       │           ├── closest = candidates.pop()
       │           ├── if closest.dist > results.peek() → break
       │           ├── neighbors = getNeighbors(closest, level=0)
       │           │   └── allocator.Resolve(handle) → []uint64
       │           │
       │           ├── batch collect unvisited offsets
       │           │
       │           ├── batchDistance(query, offsets, dists):
       │           │   └── for each offset:
       │           │       vec = arena.Get(offset)
       │           │       ┌─────────────────────────────────┐
       │           │       │  DotProductDistance(query, vec)  │
       │           │       │  = 1 - dotProductAVX2(q, v)     │
       │           │       │                                 │
       │           │       │  AVX2+FMA Assembly:              │
       │           │       │  loop:                          │
       │           │       │    VMOVUPS (AX)(SI*4), Y1       │
       │           │       │    VMOVUPS (CX)(SI*4), Y2       │
       │           │       │    VFMADD231PS Y2, Y1, Y0       │
       │           │       │    ADDQ $8, SI                  │
       │           │       │    JMP loop                     │
       │           │       └─────────────────────────────────┘
       │           │
       │           └── for each neighbor:
       │               if dist < results.peek() || results.Len() < ef:
       │                   candidates.push(neighbor)
       │                   results.push(neighbor)
       │
       ├── results → []VSearchResult{Key, Distance}
       └── vs.mu.RUnlock()
   
   3d. Фильтрация по threshold:
       for r in results:
           sub = sh.subs[r.Key]              // "__sem:42" → SemanticSub
           if r.Distance <= sub.threshold:
               targets = append(targets, sub)
   
   3e. sh.mu.RUnlock()                       // Снимаем shared lock
   
   ╔═══════════════════════════════════════════╗
   ║ ФАЗА 2: Доставка (БЕЗ лока)             ║
   ╚═══════════════════════════════════════════╝
   
   3f. Формирование RESP-сообщения:
       msg = protocol.Value{
           Typ: '*',
           Array: [
               {Typ: '$', Str: "semantic-message"},
               {Typ: '$', Str: "GPT-5 released!"},
           ],
       }
   
   3g. for each target:
       select {
       case sub.ch <- msg:                   // Non-blocking send
           delivered++
       default:
           sh.disconnectSlow(sub)            // Канал полный → отключить
       }
   
   3h. return delivered

4. Каждый writePump() подписчика:
   msg = <-sub.ch
   writer.Write(msg) → TCP → клиент получает:
   *2\r\n$16\r\nsemantic-message\r\n$15\r\nGPT-5 released!\r\n
```

### VSIM.UNSUBSCRIBE — отписка

```
1. main.go: semHub.Unsubscribe(cs.Conn)

2. SemanticHub.Unsubscribe(conn):
   ├── sh.mu.Lock()
   ├── sub = sh.conns[conn]
   ├── removeLocked(sub):
   │   ├── sh.index.Delete("__sem:42")      // Удалить вектор из HNSW
   │   │   ├── graph.Delete(nodeID)          // Ремонт связей графа
   │   │   └── delete maps (ids, keys)
   │   ├── close(sub.done)                   // Сигнал writePump → выход
   │   ├── delete(sh.subs, "__sem:42")
   │   └── delete(sh.conns, conn)
   └── sh.mu.Unlock()

3. writePump() горутина:
   case <-sub.done: return                   // Горутина завершается
```

---

## Внутренние структуры данных

### SemanticHub

```go
type SemanticHub struct {
    mu    sync.RWMutex           // Защита subs/conns maps
    index *vector.VectorStore    // HNSW-индекс интересов подписчиков (cosine)

    subs  map[string]*SemanticSub   // "__sem:42" → подписчик
    conns map[net.Conn]*SemanticSub // TCP conn → подписчик (для Unsubscribe)

    nextID atomic.Uint64            // Генератор уникальных ID (lock-free)
}
```

**Зачем два map?**

- `subs` — нужен для Publish: HNSW Search возвращает ключ `"__sem:42"`, нам нужно найти подписчика по этому ключу.
- `conns` — нужен для Unsubscribe/RemoveConn: клиент отключается, у нас есть только `net.Conn`, нужно найти и удалить подписчика.

### SemanticSub

```go
type SemanticSub struct {
    ch        chan protocol.Value   // Буферизованный канал (256 сообщений)
    conn      net.Conn             // TCP-соединение подписчика
    done      chan struct{}         // Сигнал завершения writePump
    key       string               // Ключ в HNSW: "__sem:42"
    threshold float32              // Порог: 0.0=exact, 0.5=похожие, 2.0=всё
}
```

### Как ключи связывают SemanticHub ↔ VectorStore ↔ HNSW Graph

```
SemanticHub                VectorStore              Graph
═══════════                ═══════════              ═════

subs map:                  ids map:                 nodes array:
"__sem:1" → sub1           "__sem:1" → 0            [0] Node{vec=[1,0,0,0]}
"__sem:2" → sub2           "__sem:2" → 1            [1] Node{vec=[0,0,0,1]}
"__sem:3" → sub3           "__sem:3" → 2            [2] Node{vec=[0.5,0.5,...]}

                           keys map (reverse):
                           0 → "__sem:1"
                           1 → "__sem:2"
                           2 → "__sem:3"

Publish([0.9,0.1,0,0]):
  Graph.Search → [{ID:0, Dist:0.02}, {ID:2, Dist:0.42}]
  keys[0] = "__sem:1"  →  subs["__sem:1"] = sub1  →  sub1.threshold=0.5, 0.02≤0.5 ✅
  keys[2] = "__sem:3"  →  subs["__sem:3"] = sub3  →  sub3.threshold=2.0, 0.42≤2.0 ✅
```

---

## RESP протокол и команды

### VSIM.SUBSCRIBE

```
Формат:  VSIM.SUBSCRIBE <threshold> <v1> <v2> ... <vN>

Пример:  VSIM.SUBSCRIBE 0.5 1.0 0.0 0.0 0.0

         threshold=0.5 — получать сообщения с distance ≤ 0.5
         [1.0, 0.0, 0.0, 0.0] — вектор интересов (e.g. "ML")

Ответ:   *3
         $18
         semantic-subscribe
         $2
         OK
         :1

Ошибки:  -ERR usage: VSIM.SUBSCRIBE <threshold> <v1> <v2> ... <vN>
         -ERR invalid threshold (must be non-negative float)
         -ERR dimension mismatch: expected 128, got 4
```

### VSIM.PUBLISH

```
Формат:  VSIM.PUBLISH <message> <v1> <v2> ... <vN>

Пример:  VSIM.PUBLISH "GPT-5 released!" 0.9 0.1 0.0 0.0

         message = "GPT-5 released!"
         [0.9, 0.1, 0.0, 0.0] — вектор темы сообщения

Ответ:   :2                          (количество получателей)
         :0                          (никто не подписан / не совпало)

Ошибки:  -ERR usage: VSIM.PUBLISH <message> <v1> <v2> ... <vN>
```

### VSIM.UNSUBSCRIBE

```
Формат:  VSIM.UNSUBSCRIBE

Ответ:   +OK                         (всегда OK, idempotent)
```

### Сообщения подписчику (push)

```
Подтверждение подписки:
*3\r\n$18\r\nsemantic-subscribe\r\n$2\r\nOK\r\n:1\r\n

Входящее сообщение:
*2\r\n$16\r\nsemantic-message\r\n$15\r\nGPT-5 released!\r\n
```

---

## Concurrency модель

### Блокировки и гарантии

```
Операция          Лок SemanticHub    Лок VectorStore    Горутина
──────────────    ────────────────   ────────────────   ─────────
Subscribe         mu.Lock() ■        vs.mu.Lock() ■     creates writePump
Unsubscribe       mu.Lock() ■        vs.mu.Lock() ■     closes done
Publish           mu.RLock() □       vs.mu.RLock() □     —
Publish           mu.RLock() □       vs.mu.RLock() □     —
disconnectSlow    mu.Lock() ■        vs.mu.Lock() ■     closes conn

■ = exclusive     □ = shared (concurrent)
```

**Гарантии:**
- Несколько Publish могут выполняться **параллельно** (RLock)
- Subscribe/Unsubscribe блокирует Publish (Lock vs RLock)
- Subscribe/Unsubscribe — редкие операции (раз в жизнь соединения)
- Publish — частая операция (тысячи в секунду)

### writePump — горутина подписчика

```
Каждый подписчик имеет свою горутину writePump:

  ┌─────────┐     ch (buffer=256)     ┌──────────┐
  │ Publish  │ ──── non-blocking ────→ │writePump │ ──→ TCP conn → клиент
  │ горутина │     send                │ горутина │
  └─────────┘                         └──────────┘
                                           │
                                      <-done (close)
                                           │
                                       return (exit)

Если ch полный (клиент не читает):
  - Publish делает non-blocking send → default branch
  - Вызывается disconnectSlow()
  - close(done) → writePump завершается
  - conn.Close() → TCP-соединение закрывается
```

### Порядок блокировок (deadlock prevention)

```
Всегда: SemanticHub.mu → VectorStore.mu

  Subscribe:   sh.mu.Lock() → sh.index.Add() → vs.mu.Lock()
  Publish:     sh.mu.RLock() → sh.index.Search() → vs.mu.RLock()

Никогда наоборот: VectorStore не вызывает SemanticHub.
Это гарантирует отсутствие deadlock.
```

---

## Производительность

### Бенчмарки (Intel i7-9750H, AVX2+FMA)

```
Semantic Pub/Sub Publish:
──────────────────────────────────────────────────────────
Подписчики    Размерность    Latency       Allocs
──────────────────────────────────────────────────────────
10            128D           1,745 ns      4 allocs/op
100           128D           19,389 ns     4 allocs/op
1,000         128D           ~480 µs       4 allocs/op
10            768D           ~84 µs        5 allocs/op
100           768D           ~125 µs       5 allocs/op
──────────────────────────────────────────────────────────

Для сравнения — классический Hub.Publish:
  O(1) map lookup + O(K) send ≈ 50-200ns
```

### Где тратится время

```
SemanticHub.Publish() — профиль:

  HNSW Search (searchLayer + greedyClosest)    ████████████████████  96-99%
    └─ batchDistance (AVX2+FMA dotProduct)      ████████████████     ~85%
    └─ heap operations (push/pop)              ██                   ~8%
    └─ getNeighbors (TCMalloc resolve)         █                    ~5%
  
  Map lookups (subs[key])                      ▏                    <1%
  RWMutex (RLock/RUnlock)                      ▏                    <0.5%
  Channel send (sub.ch <- msg)                 ▏                    <0.5%
```

### Оптимизации на горячем пути

1. **AVX2 + FMA3 SIMD** — distance compute: 8 float32 за такт, fused multiply-add
2. **Pre-normalization** — cosine через dot product (3× быстрее полного cosine)
3. **Batch distance** — cache locality: все offsets собраны в буфере
4. **sync.Pool** — zero-alloc searchState (visited bitset, heaps, buffers)
5. **greedyClosest** — верхние слои без pool/heap/map (tight loop)
6. **Publish вне лока** — сообщения отправляются после RUnlock

---

## Интеграция в main.go

### Инициализация (строки 261-266)

```go
// === 5. Pub/Sub Hub ===
hub := pubsub.NewHub()

// === 5.1. Semantic Pub/Sub (Vector-routed) ===
semanticIndex := vector.NewVectorStoreCosine(s)  // Отдельный HNSW (cosine)
semHub := pubsub.NewSemanticHub(semanticIndex)
```

`semanticIndex` использует тот же `TCMallocStore` (`s`) что и основной KV-store,
но это **отдельный VectorStore** от `vecStore` (который используется для VSIM.ADD/SEARCH).

### Передача в executeCommand (строка 577)

```go
func executeCommand(s *tcmalloc.TCMallocStore, bw *wal.BatchWAL, ttl *store.TTLManager,
    hub *pubsub.Hub, semHub *pubsub.SemanticHub, cl *cluster.Cluster, ...)
```

### Обработка команд (строки 1022-1079)

```go
case "VSIM.SUBSCRIBE":
    // Парсинг threshold и вектора из args
    threshold, _ := strconv.ParseFloat(string(args[0]), 32)
    vec := parseFloatArgs(args[1:])
    semHub.Subscribe(cs.Conn, vec, float32(threshold))

case "VSIM.UNSUBSCRIBE":
    semHub.Unsubscribe(cs.Conn)
    buf.WriteSimpleString("OK")

case "VSIM.PUBLISH":
    message := string(args[0])
    vec := parseFloatArgs(args[1:])
    count := semHub.Publish(vec, message)
    buf.WriteInt(count)
```

---

## Примеры использования

### Пример 1: Новостной фильтр

```
# Клиент A: интересуется технологиями
> VSIM.SUBSCRIBE 0.3 0.9 0.05 0.05 0.0 0.0

# Клиент B: интересуется спортом
> VSIM.SUBSCRIBE 0.3 0.0 0.0 0.0 0.9 0.1

# Клиент C: интересуется всем
> VSIM.SUBSCRIBE 2.0 0.5 0.5 0.5 0.5 0.5

# Издатель: tech-новость
> VSIM.PUBLISH "Apple Vision Pro 2" 0.85 0.1 0.0 0.05 0.0
# → Клиент A получает (dist≈0.08 ≤ 0.3)  ✅
# → Клиент B НЕ получает (dist≈0.95 > 0.3) ❌
# → Клиент C получает (dist≈0.4 ≤ 2.0)   ✅
# Ответ: :2

# Издатель: спортивная новость
> VSIM.PUBLISH "Champions League Final" 0.0 0.0 0.05 0.85 0.1
# → Клиент A НЕ получает ❌
# → Клиент B получает ✅
# → Клиент C получает ✅
# Ответ: :2
```

### Пример 2: IoT-сенсоры

```
# Сенсор температуры: подписка на аномалии похожего типа
> VSIM.SUBSCRIBE 0.2 0.0 1.0 0.0

# Сенсор давления: подписка на свои аномалии
> VSIM.SUBSCRIBE 0.2 0.0 0.0 1.0

# Алерт: аномалия температуры
> VSIM.PUBLISH "temp spike 95°C" 0.05 0.9 0.05
# → Сенсор температуры получает (close to [0,1,0]) ✅
# → Сенсор давления НЕ получает (far from [0,0,1]) ❌
```

### Пример 3: Рекомендательная система

```
# Пользователь с embedding его профиля (768-dim вектор из BERT/OpenAI)
> VSIM.SUBSCRIBE 0.4 0.123 -0.456 0.789 ... (768 значений)

# Новый контент: embedding текста статьи
> VSIM.PUBLISH "Как готовить пасту карбонара" 0.234 -0.567 0.890 ...
# → Доставляется пользователям с похожими интересами
```

### Пример 4: Программный клиент (Go)

```go
// Подписчик
conn, _ := net.Dial("tcp", "localhost:6380")
fmt.Fprintf(conn, "*5\r\n$14\r\nVSIM.SUBSCRIBE\r\n$3\r\n0.5\r\n$3\r\n1.0\r\n$3\r\n0.0\r\n$3\r\n0.0\r\n")

// Читаем сообщения в цикле
reader := bufio.NewReader(conn)
for {
    line, _ := reader.ReadString('\n')
    // Парсим RESP → получаем semantic-message + payload
}

// Издатель
pub, _ := net.Dial("tcp", "localhost:6380")
fmt.Fprintf(pub, "*4\r\n$12\r\nVSIM.PUBLISH\r\n$11\r\nHello World\r\n$3\r\n0.9\r\n$3\r\n0.1\r\n")
```
