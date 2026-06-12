# Unified Pub/Sub — Единый Hub с векторной маршрутизацией

## Содержание

1. [Концепция](#концепция)
2. [Архитектура единого Hub](#архитектура-единого-hub)
3. [Classic vs Semantic маршрутизация](#classic-vs-semantic-маршрутизация)
4. [Компоненты и зависимости](#компоненты-и-зависимости)
5. [Внутренние структуры данных](#внутренние-структуры-данных)
6. [Подробный data flow — Subscribe](#подробный-data-flow--subscribe)
7. [Подробный data flow — Publish](#подробный-data-flow--publish)
8. [Подробный data flow — SemanticSubscribe](#подробный-data-flow--semanticsubscribe)
9. [Подробный data flow — SemanticPublish](#подробный-data-flow--semanticpublish)
10. [Подробный data flow — RemoveConn](#подробный-data-flow--removeconn)
11. [Смешанные подписки (Mixed)](#смешанные-подписки-mixed)
12. [Concurrency модель](#concurrency-модель)
13. [RESP протокол и команды](#resp-протокол-и-команды)
14. [Производительность и бенчмарки](#производительность-и-бенчмарки)
15. [Интеграция в main.go](#интеграция-в-maingo)
16. [Примеры использования](#примеры-использования)
17. [Решение об объединении Hub](#решение-об-объединении-hub)

---

## Концепция

### Проблема двух Hub'ов

Изначально система имела **два раздельных** механизма Pub/Sub:
- `Hub` — классический (строковые каналы)
- `SemanticHub` — векторный (HNSW-поиск)

Проблемы такого подхода:

1. **Data race на TCP-сокете** — если клиент подписывается на оба механизма,
   две горутины `writePump` пишут в один `net.Conn` **без синхронизации**.
2. **Дублирование кода** — `writePump`, `disconnectSlow`, `ch`/`conn`/`done` поля
   идентичны в обоих реализациях (~80 строк дублей).
3. **Два lifecycle** — отключение клиента требует вызова `hub.RemoveConn()` **и**
   `semHub.RemoveConn()` — легко забыть одну из очисток.

### Решение: единый Hub

```
       ┌──────────────────────────────────────────────┐
       │                   Hub                        │
       │                                              │
       │  subscribers: map[net.Conn]*Subscriber       │
       │  ┌─────────────────┐  ┌──────────────────┐  │
       │  │ Classic Routing  │  │ Semantic Routing  │  │
       │  │ channels map     │  │ semIndex (HNSW)   │  │
       │  │ string → [subs]  │  │ semSubs map       │  │
       │  └─────────────────┘  └──────────────────┘  │
       │                                              │
       │  Один conn → Один Subscriber → Один writePump│
       └──────────────────────────────────────────────┘
```

Принцип: **один conn = один Subscriber = один writePump = один канал доставки**.

Classic `Publish` и `SemanticPublish` оба пишут в **тот же `sub.ch`**.
`writePump` не знает откуда пришло сообщение — просто дренит канал.

---

## Архитектура единого Hub

```
┌─────────────────────────────────────────────────────────────────┐
│                        KVStore Server                            │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                      Hub (unified)                        │   │
│  │                                                           │   │
│  │  subscribers: map[net.Conn]*Subscriber  ← ЕДИНЫЙ реестр   │   │
│  │                                                           │   │
│  │  ┌─── Classic Routing ──────┐ ┌─── Semantic Routing ───┐ │   │
│  │  │                          │ │                         │ │   │
│  │  │ channels:                │ │ semIndex:               │ │   │
│  │  │   "news" → {sub1, sub3} │ │   VectorStore (HNSW)    │ │   │
│  │  │   "chat" → {sub2}       │ │   cosine distance       │ │   │
│  │  │                          │ │                         │ │   │
│  │  │ Маршрутизация:           │ │ semSubs:                │ │   │
│  │  │   O(1) map lookup        │ │   "__sem:1" → sub1      │ │   │
│  │  │                          │ │   "__sem:2" → sub3      │ │   │
│  │  │                          │ │                         │ │   │
│  │  │                          │ │ Маршрутизация:          │ │   │
│  │  │                          │ │   O(log N) HNSW search  │ │   │
│  │  │                          │ │   + threshold filter    │ │   │
│  │  └──────────────────────────┘ └─────────────────────────┘ │   │
│  │                                                           │   │
│  │  Subscriber struct:                                       │   │
│  │    ch        chan protocol.Value   ← ОДИН канал для ВСЕХ  │   │
│  │    conn      net.Conn                                     │   │
│  │    done      chan struct{}                                 │   │
│  │    channels  map[string]struct{}   ← classic подписки     │   │
│  │    vecKey    string                ← semantic ключ в HNSW │   │
│  │    threshold float32              ← semantic порог        │   │
│  │                                                           │   │
│  │  writePump: ONE goroutine per connection                  │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                    TCMallocStore                            │   │
│  │  Shared allocator: KV data + HNSW nodes + neighbors        │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

---

## Classic vs Semantic маршрутизация

### Сравнительная таблица

| | Classic | Semantic |
|---|---|---|
| **Подписка** | `SUBSCRIBE channel1 channel2` | `VSIM.SUBSCRIBE 0.5 v1 v2 ... vN` |
| **Публикация** | `PUBLISH channel message` | `VSIM.PUBLISH message v1 v2 ... vN` |
| **Отписка** | `UNSUBSCRIBE [channels]` | `VSIM.UNSUBSCRIBE` |
| **Маршрутизация** | Точное совпадение строки | K-NN поиск в HNSW + threshold |
| **Сложность Publish** | O(K) — K подписчиков канала | O(log N × M + K) — HNSW search |
| **Index** | `map[string]map[*Subscriber]struct{}` | VectorStore (HNSW + optional LSH) |
| **Множественность** | Несколько каналов на conn | Один вектор на conn |
| **Замена** | Аддитивная (новые каналы) | Замещающая (новый вектор) |

### Classic: строковая маршрутизация

```
                PUBLISH "news" "Breaking!"
                         │
                         ▼
              ┌──────────────────┐
              │   hub.channels   │
              │                  │
              │ "news" → map{    │
              │   sub1: {},      │──────→ sub1.ch ← msg
              │   sub3: {}       │──────→ sub3.ch ← msg
              │ }                │
              │                  │
              │ "chat" → map{    │
              │   sub2: {}       │       (НЕ получает)
              │ }                │
              └──────────────────┘
```

### Semantic: векторная маршрутизация

```
        VSIM.PUBLISH "GPT-5!" 0.9 0.1 0.0 0.0
                         │
                         ▼
              ┌──────────────────────────────────┐
              │          hub.semIndex             │
              │                                   │
              │  HNSW Search([0.9,0.1,0,0], K)   │
              │     ┌────────────────────┐       │
              │     │  ●[1,0,0,0]  sub1  │← dist=0.02 ≤ 0.5 ✅
              │     │  ●[0,0,0,1]  sub2  │← dist=0.98 > 0.5 ❌
              │     │  ●[.5,.5,.5,.5] s3 │← dist=0.42 ≤ 2.0 ✅
              │     └────────────────────┘       │
              │                                   │
              │  Фильтр по threshold → {sub1,sub3}│
              │  sub1.ch ← msg                    │
              │  sub3.ch ← msg                    │
              └──────────────────────────────────┘
```

---

## Компоненты и зависимости

### Дерево зависимостей (инициализация)

```
main.go
  │
  ├── s = tcmalloc.NewTCMallocStore(NumCPU)     // Аллокатор памяти
  │     │
  │     └── semanticIndex = vector.NewVectorStoreCosine(s)
  │           │
  │           ├── graph = NewGraph(DotProductDistance, s)
  │           │     ├── arena = VectorArena
  │           │     └── allocator = s (TCMalloc)
  │           │
  │           └── lsh = NewLSHIndex(dim, 42)   // LSH для dim≥256
  │
  └── hub = pubsub.NewHub(semanticIndex)       // ← ОДИН Hub
        │
        ├── subscribers = map[net.Conn]*Subscriber
        ├── channels = map[string]map[*Subscriber]struct{}
        │
        ├── semIndex = semanticIndex  // ← тот же VectorStore
        └── semSubs = map[string]*Subscriber
```

### Слои системы

```
┌───────────────────────────────────────────────────────────┐
│ Слой 1: RESP Protocol (main.go)                           │
│ Парсинг SUBSCRIBE, PUBLISH, VSIM.SUBSCRIBE, VSIM.PUBLISH │
│ Конвертация args → strings или []float32                  │
├───────────────────────────────────────────────────────────┤
│ Слой 2: Hub (pubsub.go) — ЕДИНЫЙ диспетчер               │
│ Classic: Subscribe/Publish/Unsubscribe                    │
│ Semantic: SemanticSubscribe/SemanticPublish/Unsubscribe   │
│ Shared: getOrCreateSub, writePump, disconnectSlow         │
│ RWMutex для concurrent publish + exclusive subscribe      │
├───────────────────────────────────────────────────────────┤
│ Слой 3: VectorStore (store.go) — только для Semantic      │
│ Обёртка над HNSW: Add/Delete/Search с string ключами      │
│ Cosine distance через pre-normalization + DotProduct      │
├───────────────────────────────────────────────────────────┤
│ Слой 4: HNSW Graph (graph.go)                             │
│ Навигационный граф: searchLayer, greedyClosest            │
│ AVX2+FMA distance compute, sync.Pool для searchState      │
├───────────────────────────────────────────────────────────┤
│ Слой 5: TCMallocStore                                     │
│ Arena для векторов, handles для neighbors                  │
│ Lock-free GET, per-worker MCache                          │
└───────────────────────────────────────────────────────────┘
```

---

## Внутренние структуры данных

### Hub struct

```go
type Hub struct {
    mu          sync.RWMutex              // Защита subscribers/channels/semSubs
    subscribers map[net.Conn]*Subscriber  // ЕДИНЫЙ реестр подписчиков

    // Classic routing
    channels map[string]map[*Subscriber]struct{}  // канал → множество подписчиков

    // Semantic routing (опционально, nil если не сконфигурировано)
    semIndex  *vector.VectorStore    // HNSW-индекс интересов подписчиков
    semSubs   map[string]*Subscriber // HNSW key ("__sem:42") → подписчик
    nextSemID atomic.Uint64          // Генератор ID (lock-free)
}
```

### Subscriber struct

```go
type Subscriber struct {
    // ═══ Транспортный слой (ОБЩИЙ для обоих типов) ═══
    ch   chan protocol.Value   // Буферизованный канал (256 сообщений)
    conn net.Conn             // TCP-соединение
    done chan struct{}         // Сигнал завершения writePump

    // ═══ Classic routing (опционально) ═══
    channels map[string]struct{}  // Набор строковых каналов: {"news", "chat"}

    // ═══ Semantic routing (опционально) ═══
    vecKey    string    // Ключ в HNSW: "__sem:42" ("" = нет семантической подписки)
    threshold float32   // Порог: 0.0=exact match, 0.5=похожие, 2.0=всё
}
```

### Как ключи связывают Hub ↔ VectorStore ↔ Graph

```
Hub                        VectorStore              Graph
═══                        ═══════════              ═════

subscribers:               ids map:                 nodes array:
  conn1 → sub1              "__sem:1" → 0            [0] Node{vec=[1,0,0,0]}
  conn2 → sub2              "__sem:2" → 1            [1] Node{vec=[0,0,0,1]}
  conn3 → sub3              "__sem:3" → 2            [2] Node{vec=[.5,.5,.5,.5]}

semSubs:                   keys map (reverse):
  "__sem:1" → sub1           0 → "__sem:1"
  "__sem:2" → sub2           1 → "__sem:2"
  "__sem:3" → sub3           2 → "__sem:3"

channels:
  "news" → {sub1, sub3}
  "chat" → {sub2}

SemanticPublish([0.9,0.1,0,0]):
  Graph.Search → [{ID:0, Dist:0.02}, {ID:2, Dist:0.42}]
  VectorStore.keys[0] = "__sem:1"  →  semSubs["__sem:1"] = sub1
  VectorStore.keys[2] = "__sem:3"  →  semSubs["__sem:3"] = sub3
  sub1.threshold=0.5, 0.02≤0.5 ✅ deliver
  sub3.threshold=2.0, 0.42≤2.0 ✅ deliver
```

---

## Подробный data flow — Subscribe

```
Клиент → TCP → RESP Parser → main.go → hub.Subscribe(conn, channels)

1. Клиент:  SUBSCRIBE news chat

2. main.go:
   channels = ["news", "chat"]
   hub.Subscribe(cs.Conn, channels)

3. Hub.Subscribe():

   3a. h.mu.Lock()                              // Exclusive lock

   3b. sub = h.getOrCreateSub(conn):
       │ Проверяет h.subscribers[conn]
       │ Если conn УЖЕ есть (например, уже имеет semantic подписку):
       │   → возвращает СУЩЕСТВУЮЩИЙ Subscriber
       │   → writePump уже запущен, не создаёт новый
       │ Если conn НОВЫЙ:
       │   → создаёт Subscriber{ch, conn, done, channels}
       │   → h.subscribers[conn] = sub
       │   → go sub.writePump()  // Запуск горутины
       └─→ sub

   3c. Для каждого канала:
       h.channels["news"][sub] = struct{}{}      // Добавить в роутинг
       sub.channels["news"] = struct{}{}         // Трекинг в подписчике

   3d. Формирование confirmations (RESP-ответы)

   3e. h.mu.Unlock()

   3f. Отправка confirmations ВНЕ лока:
       sub.ch <- confirm_news
       sub.ch <- confirm_chat

4. writePump():
   msg = <-sub.ch
   writer.Write(msg) → TCP → клиент получает:
   *3\r\n$9\r\nsubscribe\r\n$4\r\nnews\r\n:1\r\n
   *3\r\n$9\r\nsubscribe\r\n$4\r\nchat\r\n:2\r\n
```

---

## Подробный data flow — Publish

```
Клиент → TCP → RESP Parser → main.go → hub.Publish(channel, message)

1. Клиент:  PUBLISH news "Breaking news!"

2. Hub.Publish("news", "Breaking news!"):

   2a. h.mu.RLock()                         // Shared lock (parallel с другими Publish)

   2b. subs = h.channels["news"]            // O(1) map lookup
       Если нет подписчиков → RUnlock, return 0

   2c. recipients = pool.Get()              // sync.Pool — zero-alloc
       for sub in subs:
           recipients = append(recipients, sub)

   2d. h.mu.RUnlock()                       // Снимаем lock ДО отправки

   2e. msg = RESP{["message", "news", "Breaking news!"]}

   2f. for each recipient:
       select {
       case sub.ch <- msg:                  // Non-blocking send
           delivered++
       default:
           h.disconnectSlow(sub)            // Канал полный → отключить
       }

   2g. Pool cleanup and return

3. writePump(sub):
   msg = <-sub.ch
   writer.Write → TCP → клиент получает:
   *3\r\n$7\r\nmessage\r\n$4\r\nnews\r\n$14\r\nBreaking news!\r\n
```

---

## Подробный data flow — SemanticSubscribe

```
Клиент → TCP → RESP Parser → main.go → hub.SemanticSubscribe(conn, vec, threshold)

1. Клиент:  VSIM.SUBSCRIBE 0.5 1.0 0.0 0.0 0.0

2. main.go:
   threshold = 0.5
   vec = []float32{1.0, 0.0, 0.0, 0.0}

3. Hub.SemanticSubscribe(conn, vec, 0.5):

   3a. h.mu.Lock()

   3b. sub = h.getOrCreateSub(conn):
       │ Если conn уже имеет classic подписки:
       │   → возвращает ТОТ ЖЕ Subscriber (writePump уже бежит)
       │   → classic подписки СОХРАНЯЮТСЯ
       │ Если conn новый:
       │   → создаёт Subscriber, запускает writePump
       └─→ sub

   3c. Если sub.vecKey != "" (уже есть semantic подписка):
       h.semIndex.Delete(old_key)            // Удалить старый вектор из HNSW
       delete(h.semSubs, old_key)

   3d. id = h.nextSemID.Add(1)               // Атомарный счётчик (65→66)
       key = "__sem:66"

   3e. h.semIndex.Add(key, vec):              // VectorStore.Add()
       ├── vs.mu.Lock()
       ├── Normalize(vec)                    // Pre-normalization (cosine)
       ├── graph.Insert(normalizedVec):
       │   ├── level = randomLevel()
       │   ├── arena.Allocate(vec)           // Сохранить float32 данные
       │   ├── allocator.Alloc(blockSize)    // TCMalloc: блок для neighbors
       │   ├── greedyClosest(верхние слои)
       │   └── searchLayer + connect(слой 0)
       ├── vs.ids["__sem:66"] = nodeID
       ├── vs.keys[nodeID] = "__sem:66"
       └── vs.mu.Unlock()

   3f. sub.vecKey = "__sem:66"
       sub.threshold = 0.5
       h.semSubs["__sem:66"] = sub

   3g. sub.ch <- confirm                     // Подтверждение

   3h. h.mu.Unlock()

4. writePump():
   msg = <-sub.ch → TCP → клиент:
   *3\r\n$18\r\nsemantic-subscribe\r\n$2\r\nOK\r\n:1\r\n
```

---

## Подробный data flow — SemanticPublish

```
Клиент → TCP → RESP Parser → main.go → hub.SemanticPublish(queryVec, message)

1. Клиент:  VSIM.PUBLISH "GPT-5!" 0.9 0.1 0.0 0.0

2. Hub.SemanticPublish(queryVec, message):

   ╔═══════════════════════════════════════════╗
   ║ ФАЗА 1: HNSW Search (под RLock)          ║
   ╚═══════════════════════════════════════════╝

   2a. h.mu.RLock()

   2b. subCount = len(h.semSubs)              // Быстрая проверка
       if subCount == 0 → RUnlock, return 0

   2c. h.semIndex.Search(queryVec, subCount):
       ├── vs.mu.RLock()
       ├── Normalize(queryVec)
       ├── graph.Search(query, K, efSearch):
       │   │
       │   │  ★ ГОРЯЧИЙ ПУТЬ — 99% ВРЕМЕНИ ЗДЕСЬ ★
       │   │
       │   ├── greedyClosest(верхние слои):   // Express-навигация
       │   │   └── for each level > 0:
       │   │       └── follow best neighbor
       │   │
       │   └── searchLayer(слой 0, efSearch): // Полный поиск
       │       ├── searchState = pool.Get()   // sync.Pool: zero-alloc
       │       ├── visited = bitset
       │       ├── candidates = minHeap
       │       ├── results = maxHeap
       │       │
       │       └── while candidates not empty:
       │           ├── closest = candidates.pop()
       │           ├── if closest.dist > results.peek() → break
       │           │
       │           ├── batchDistance(query, neighbors):
       │           │   └── for each neighbor:
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
       │           └── update candidates + results heaps
       │
       ├── results → []VSearchResult{Key, Distance}
       └── vs.mu.RUnlock()

   ╔═══════════════════════════════════════════╗
   ║ ФАЗА 2: Threshold Filter (под RLock)     ║
   ╚═══════════════════════════════════════════╝

   2d. for r in results:
       sub = h.semSubs[r.Key]
       if r.Distance <= sub.threshold:
           targets = append(targets, sub)

   2e. h.mu.RUnlock()

   ╔═══════════════════════════════════════════╗
   ║ ФАЗА 3: Доставка (БЕЗ лока)             ║
   ╚═══════════════════════════════════════════╝

   2f. msg = RESP{["semantic-message", "GPT-5!"]}

   2g. for each target:
       select {
       case sub.ch <- msg:                   // Non-blocking send
           delivered++
       default:
           h.disconnectSlow(sub)
       }

   2h. return delivered

3. writePump() каждого подписчика:
   msg = <-sub.ch → TCP → клиент:
   *2\r\n$16\r\nsemantic-message\r\n$5\r\nGPT-5!\r\n
```

---

## Подробный data flow — RemoveConn

```
Сервер: клиент отключился → hub.RemoveConn(conn)

Hub.RemoveConn(conn):

  1. h.mu.Lock()

  2. sub = h.subscribers[conn]
     if not exists → return

  3. Очистка CLASSIC подписок:
     for channel in sub.channels:
         delete(h.channels[channel], sub)
         if len(h.channels[channel]) == 0:
             delete(h.channels, channel)     // GC пустого канала

  4. Очистка SEMANTIC подписки:
     if sub.vecKey != "":
         h.semIndex.Delete(sub.vecKey)       // Удалить из HNSW
         delete(h.semSubs, sub.vecKey)

  5. h.closeSub(sub):
     close(sub.done)                          // Сигнал writePump → выход
     delete(h.subscribers, sub.conn)

  6. h.mu.Unlock()

  7. writePump():
     case <-sub.done: return                  // Горутина завершается
```

---

## Смешанные подписки (Mixed)

### Сценарий: один клиент использует ОБА типа

```
Клиент conn1:
  SUBSCRIBE news                    → sub1.channels = {"news"}
  VSIM.SUBSCRIBE 0.5 1.0 0.0 0.0   → sub1.vecKey = "__sem:42"

Результат:
  h.subscribers[conn1] = sub1
  h.channels["news"] = {sub1}
  h.semSubs["__sem:42"] = sub1

  sub1.channels = {"news"}
  sub1.vecKey = "__sem:42"
  sub1.ch ← [все сообщения из обоих типов]

  ОДНА горутина writePump → ОДИН TCP conn
```

### Порядок отписки

```
Сценарий 1: Unsubscribe от classic → semantic остаётся
  UNSUBSCRIBE                        // отписка от всех classic каналов
  sub.channels = {}
  sub.vecKey = "__sem:42"            // ← всё ещё активен
  len(channels)==0 && vecKey!="" → subscriber НЕ удаляется

Сценарий 2: SemanticUnsubscribe → classic остаётся
  VSIM.UNSUBSCRIBE
  sub.vecKey = ""
  sub.channels = {"news"}            // ← всё ещё активен
  channels!={} && vecKey=="" → subscriber НЕ удаляется

Сценарий 3: Оба отписались → subscriber удаляется
  UNSUBSCRIBE
  VSIM.UNSUBSCRIBE
  sub.channels = {}
  sub.vecKey = ""
  len(channels)==0 && vecKey=="" → closeSub(sub) ← УДАЛЕНИЕ

Сценарий 4: RemoveConn → удаляется ВСЁ сразу
  // Вызывается при отключении клиента
  hub.RemoveConn(conn)               // очищает classic + semantic + closeSub
```

### Тесты смешанных подписок

```go
func TestHub_MixedSubscriptions(t *testing.T) {
    hub := newTestHub()
    _, conn := net.Pipe()

    // Classic подписка
    hub.Subscribe(conn, []string{"news"})

    // Semantic подписка (тот же conn!)
    hub.SemanticSubscribe(conn, []float32{1, 0, 0, 0}, 0.5)

    // Classic publish → доставляется
    hub.Publish("news", "breaking") // delivered=1 ✅

    // Semantic publish → тоже доставляется
    hub.SemanticPublish([]float32{0.95, 0.05, 0, 0}, "ML news") // delivered=1 ✅

    // Отписка от classic — semantic остаётся
    hub.Unsubscribe(conn, nil)
    hub.IsSubscriber(conn) // true (semantic ещё активен)

    // Отписка от semantic — subscriber удаляется
    hub.SemanticUnsubscribe(conn)
    hub.IsSubscriber(conn) // false
}
```

---

## Concurrency модель

### Таблица блокировок

```
Операция              Лок Hub       Лок VectorStore    Горутина
────────────────────  ──────────    ────────────────   ─────────
Subscribe             mu.Lock() ■   —                  may start writePump
Unsubscribe           mu.Lock() ■   —                  may stop writePump
Publish               mu.RLock() □  —                  —
SemanticSubscribe     mu.Lock() ■   vs.mu.Lock() ■     may start writePump
SemanticUnsubscribe   mu.Lock() ■   vs.mu.Lock() ■     may stop writePump
SemanticPublish       mu.RLock() □  vs.mu.RLock() □    —
RemoveConn            mu.Lock() ■   vs.mu.Lock() ■     stops writePump
disconnectSlow        mu.Lock() ■   vs.mu.Lock() ■     stops writePump

■ = exclusive     □ = shared (concurrent)
```

### Гарантии

- Несколько `Publish` / `SemanticPublish` могут выполняться **параллельно** (RLock)
- `Subscribe` / `Unsubscribe` блокируют `Publish` (Lock vs RLock)
- Subscribe/Unsubscribe — **редкие** (раз в жизнь соединения)
- Publish — **частая** операция (тысячи в секунду)

### Порядок блокировок (deadlock prevention)

```
Всегда: Hub.mu → VectorStore.mu

  SemanticSubscribe:  h.mu.Lock() → h.semIndex.Add() → vs.mu.Lock()
  SemanticPublish:    h.mu.RLock() → h.semIndex.Search() → vs.mu.RLock()

Никогда наоборот: VectorStore НЕ вызывает Hub.
Это гарантирует отсутствие deadlock.
```

### writePump — единственная горутина на conn

```
  ┌──────────┐     ch (buffer=256)     ┌──────────┐
  │ Publish   │ ────── send ──────────→ │writePump │ ──→ TCP → клиент
  │ (classic) │                        │ горутина │
  └──────────┘                         │          │
  ┌──────────┐                         │   ОДНА   │
  │ Semantic  │ ────── send ──────────→ │   на     │
  │ Publish   │                        │   conn   │
  └──────────┘                         └──────────┘
                                           │
                                      <-done (close)
                                           │
                                       return (exit)
```

---

## RESP протокол и команды

### Classic Pub/Sub

```
SUBSCRIBE channel1 channel2
Ответ: *3\r\n$9\r\nsubscribe\r\n$8\r\nchannel1\r\n:1\r\n
       *3\r\n$9\r\nsubscribe\r\n$8\r\nchannel2\r\n:2\r\n

UNSUBSCRIBE [channel1]
Ответ: +OK\r\n

PUBLISH channel1 "hello"
Ответ: :2\r\n  (количество получателей)

Push-сообщение подписчику:
*3\r\n$7\r\nmessage\r\n$8\r\nchannel1\r\n$5\r\nhello\r\n
```

### Semantic Pub/Sub

```
VSIM.SUBSCRIBE <threshold> <v1> <v2> ... <vN>
Ответ (push): *3\r\n$18\r\nsemantic-subscribe\r\n$2\r\nOK\r\n:1\r\n

VSIM.UNSUBSCRIBE
Ответ: +OK\r\n

VSIM.PUBLISH <message> <v1> <v2> ... <vN>
Ответ: :2\r\n  (количество получателей)

Push-сообщение подписчику:
*2\r\n$16\r\nsemantic-message\r\n$5\r\nGPT-5!\r\n
```

---

## Производительность и бенчмарки

### Результаты (Intel i7-9750H, AVX2+FMA)

```
Classic Hub.Publish:
──────────────────────────────────────────────────────────
Подписчики    Latency       Allocs      Throughput
──────────────────────────────────────────────────────────
1             ~50 ns        0 allocs    ~20M msg/sec
10            ~200 ns       0 allocs    ~5M msg/sec
100           ~2 µs         0 allocs    ~500K msg/sec
──────────────────────────────────────────────────────────

Semantic Hub.SemanticPublish:
──────────────────────────────────────────────────────────
Подписчики    Размерность    Latency       Allocs
──────────────────────────────────────────────────────────
10            128D           1,745 ns      4 allocs/op
100           128D           19,389 ns     4 allocs/op
1,000         128D           ~480 µs       4 allocs/op
10            768D           ~84 µs        5 allocs/op
100           768D           ~125 µs       5 allocs/op
──────────────────────────────────────────────────────────
```

### Где тратится время в SemanticPublish

```
HNSW Search (searchLayer + greedyClosest)    ████████████████████  96-99%
  └─ batchDistance (AVX2+FMA dotProduct)     ████████████████     ~85%
  └─ heap operations (push/pop)             ██                   ~8%
  └─ getNeighbors (TCMalloc resolve)        █                    ~5%

Map lookups (semSubs[key])                   ▏                   <1%
RWMutex (RLock/RUnlock)                      ▏                   <0.5%
Channel send (sub.ch <- msg)                 ▏                   <0.5%
```

### Оптимизации на горячем пути

1. **AVX2 + FMA3 SIMD** — 8 float32 за такт, fused multiply-add
2. **Pre-normalization** — cosine → dot product (3× быстрее)
3. **Batch distance** — cache locality
4. **sync.Pool** — zero-alloc searchState
5. **Publish вне лока** — сообщения отправляются после RUnlock
6. **subscriberSlicePool** — Pool для среза recipients (classic)

---

## Интеграция в main.go

### Инициализация (строки 260-262)

```go
// === 5. Pub/Sub Hub (Classic + Semantic) ===
semanticIndex := vector.NewVectorStoreCosine(s)
hub := pubsub.NewHub(semanticIndex)   // ОДИН Hub для всего
```

### Передача в executeCommand (строка 582)

```go
func executeCommand(s *tcmalloc.TCMallocStore, bw *wal.BatchWAL, ttl *store.TTLManager,
    hub *pubsub.Hub, cl *cluster.Cluster, wasm *compute.Engine, ...)
//  ^^^^^^^^^^^^^^ — ОДИН Hub, без semHub
```

### Обработка команд

```go
// Classic Pub/Sub
case "SUBSCRIBE":
    hub.Subscribe(cs.Conn, channels)

case "UNSUBSCRIBE":
    hub.Unsubscribe(cs.Conn, channels)

case "PUBLISH":
    count := hub.Publish(string(args[0]), string(args[1]))

// Semantic Pub/Sub (тот же hub!)
case "VSIM.SUBSCRIBE":
    hub.SemanticSubscribe(cs.Conn, vec, float32(threshold))

case "VSIM.UNSUBSCRIBE":
    hub.SemanticUnsubscribe(cs.Conn)

case "VSIM.PUBLISH":
    count := hub.SemanticPublish(vec, message)
```

### Очистка при отключении

```go
// Раньше: ДВА вызова
hub.RemoveConn(cs.Conn)
semHub.RemoveConn(cs.Conn)  // ← легко забыть

// Теперь: ОДИН вызов
hub.RemoveConn(cs.Conn)      // ← очищает ОБА типа
```

---

## Примеры использования

### Пример 1: Клиент с обоими типами подписок

```
# Подписка на classic канал "alerts"
> SUBSCRIBE alerts
< *3 subscribe alerts 1

# Подписка на semantic (ML-тематика)
> VSIM.SUBSCRIBE 0.5 1.0 0.0 0.0 0.0
< *3 semantic-subscribe OK 1

# Получаем classic сообщения:
< *3 message alerts "Server CPU > 95%"

# И semantic сообщения через тот же TCP:
< *2 semantic-message "New GPT model released"
```

### Пример 2: IoT сценарий

```
# Датчик: подписка на алерты по string-каналу
> SUBSCRIBE device:temp:alerts

# И одновременно на похожие аномалии по вектору
> VSIM.SUBSCRIBE 0.2 0.0 1.0 0.0

# Получает оба типа уведомлений через одно соединение
```

### Пример 3: Рекомендательная система

```
# Пользователь: classic подписка на системные уведомления
> SUBSCRIBE system:notifications

# + semantic подписка на контент по его embedding
> VSIM.SUBSCRIBE 0.4 0.123 -0.456 0.789 ... (768 значений)

# Получает и системные нотификации, и персонализированный контент
```

---

## Решение об объединении Hub

### Было (два Hub):
```
                     ┌──────────┐
  SUBSCRIBE ────────→│  Hub     │──→ writePump #1 ──→ conn
                     └──────────┘

                     ┌──────────────┐
  VSIM.SUBSCRIBE ──→│ SemanticHub  │──→ writePump #2 ──→ conn  ← DATA RACE!
                     └──────────────┘
```

### Стало (один Hub):
```
                     ┌──────────────────────┐
  SUBSCRIBE ────────→│                      │
                     │   Hub (unified)      │──→ writePump ──→ conn  ✅
  VSIM.SUBSCRIBE ──→│                      │
                     └──────────────────────┘
```

### Что изменилось

| Метрика | Было | Стало |
|---------|------|-------|
| Файлы | pubsub.go + semantic.go | pubsub.go |
| Строк кода | 247 + 253 = 500 | 378 (**−24%**) |
| Типы | Hub + SemanticHub + Subscriber + SemanticSub | Hub + Subscriber |
| writePump на conn | 1-2 (data race!) | 1 (гарантировано) |
| RemoveConn | 2 вызова (легко забыть) | 1 вызов |
| Тесты | 18 + 15 = 33 | 37 (+2 mixed теста, +2 для coverage) |

### Что НЕ изменилось

- API обратно совместим (Classic методы те же: Subscribe/Publish/Unsubscribe)
- Производительность идентична (те же алгоритмы внутри)
- RESP-формат сообщений не изменился
- `NewHub(nil)` работает как classic-only (обратная совместимость с тестами)
