# 🗺️ Карта Кластерного Кода — Шпаргалка

> Открой этот файл когда думаешь «а где это было?» — найдёшь за 30 секунд.

---

## 1. Файлы → Функции (что где лежит)

### 📄 [slots.go](https://github.com/Nikolay1994Kaz/storage_in_memory/blob/master/kvstore/internal/cluster/slots.go) — САМЫЙ МАЛЕНЬКИЙ (41 строка)

```
crc16Table  [256]uint16          ← предвычисленная таблица (init)
TotalSlots  = 16384              ← константа
CRC16(data string) → uint16     ← хеш строки
KeySlot(key string) → uint16    ← CRC16(key) % 16384
```

---

### 📄 [node.go](https://github.com/Nikolay1994Kaz/storage_in_memory/blob/master/kvstore/internal/cluster/node.go) — СТРУКТУРЫ ДАННЫХ (273 строки)

```
┌─ ТИПЫ ────────────────────────────────────────────────────┐
│  NodeState    int    (NodeOnline=0, NodePFail=1, NodeFail=2)│
│  NodeRole     int    (RoleMaster=0, RoleReplica=1)         │
│  SlotMigration struct (не используется активно)            │
└────────────────────────────────────────────────────────────┘

┌─ Node (структура одной ноды) ─────────────────────────────┐
│  ID          string         "a1b2c3d4"                     │
│  Addr        string         "127.0.0.1:6380"               │
│  GossipPort  int            6381                           │
│  State       NodeState      NodeOnline                     │
│  Slots       []bool         [16384]bool — bitmap слотов    │
│  LastPong    time.Time      когда последний раз ответила   │
│  Role        NodeRole       RoleMaster                     │
│  MasterID    string         "" (или ID мастера для реплик) │
├────────────────────────────────────────────────────────────┤
│  МЕТОДЫ:                                                   │
│  NewNode(id, addr, port) → *Node                           │
│  AssignSlots(start, end)      ← Slots[i] = true           │
│  OwnsSlot(slot) → bool        ← Slots[slot]               │
│  SlotCount() → int            ← count(true в Slots)       │
│  SlotPairs() → [][2]int       ← [true,true,false] → [[0,1]]│
│  SlotRanges() → string        ← "0-5460,10923-16383"      │
└────────────────────────────────────────────────────────────┘

┌─ ClusterState (что знает нода обо всём кластере) ─────────┐
│  mu         sync.RWMutex                 🔒 ГЛАВНЫЙ ЛОК   │
│  Self       *Node                        ← указатель на себя│
│  Nodes      map[string]*Node             ← ID → нода       │
│  SlotTable  [16384]*Node                 ← слот → владелец │
│  Migrating  map[uint16]string            ← слот → куда     │
│  Importing  map[uint16]string            ← слот → откуда   │
├────────────────────────────────────────────────────────────┤
│  МЕТОДЫ:                                                   │
│  NewClusterState(self) → *ClusterState                     │
│  RebuildSlotTable()    🔒mu.Lock   обнуляет+заполняет      │
│  LookupSlot(slot) → *Node  🔒mu.RLock  SlotTable[slot]    │
│  IsMySlot(slot) → bool  🔒mu.RLock  SlotTable[slot]==Self  │
│  AddNode(node)    🔒mu.Lock + RebuildSlotTable             │
└────────────────────────────────────────────────────────────┘
```

---

### 📄 [cluster.go](https://github.com/Nikolay1994Kaz/storage_in_memory/blob/master/kvstore/internal/cluster/cluster.go) — ЯДРО (503 строки)

```
┌─ Cluster (главная структура) ─────────────────────────────┐
│  State              *ClusterState                          │
│  gossipListener     net.Listener           TCP для gossip  │
│  stopCh             chan struct{}           сигнал "стоп"   │
│  wg                 sync.WaitGroup         ждём горутины   │
│  GetKeysInSlotFunc  func(slot, count)→[]string  ← callback│
│  MigrateGetFunc     func(key)→([]byte, bool)    ← callback│
│  MigrateDelFunc     func(key)                   ← callback│
│  MigrateSetRemoteFunc func(addr,key,val)→error  ← callback│
│  Repl               *ReplicationManager                    │
└────────────────────────────────────────────────────────────┘

ФУНКЦИИ:
┌──────────────────────┬──────────┬──────────────────────────┐
│ Функция              │ Лок?     │ Что делает               │
├──────────────────────┼──────────┼──────────────────────────┤
│ New(addr, gossipPort)│ нет      │ Создаёт Cluster          │
│ generateNodeID()     │ нет      │ 4 rand байта → hex       │
│ CheckKey(key)        │ RLock×2  │ Мой слот? → nil/MOVED    │
│ CheckKeyAsk(key)     │ RLock×2  │ Мигрирует? → nil/ASK     │
│ HandleClusterCommand │ нет      │ switch по подкомандам     │
│ clusterInfo()        │ RLock    │ CLUSTER INFO текст       │
│ clusterNodes()       │ RLock    │ CLUSTER NODES текст      │
│ clusterSlots()       │ RLock    │ CLUSTER SLOTS RESP-массив│
│ clusterMeet(args)    │ RLock    │ Добавить новую ноду      │
│ clusterReplicate     │ Lock     │ Стать репликой            │
│ clusterSetSlot       │ → migr.  │ Делегирует в migration.go│
│ clusterGetKeysInSlot │ → migr.  │ Делегирует в migration.go│
│ splitAddr(addr)      │ нет      │ "host:port" → host, port │
└──────────────────────┴──────────┴──────────────────────────┘
```

---

### 📄 [gossip.go](https://github.com/Nikolay1994Kaz/storage_in_memory/blob/master/kvstore/internal/cluster/gossip.go) — ОБЩЕНИЕ (475 строк)

```
СТРУКТУРЫ ДЛЯ СЕТИ:
┌─ NodeInfo (лёгкая копия Node для JSON) ───────────────────┐
│  ID, Addr, GossipPort, State string, Slots [][2]int        │
└────────────────────────────────────────────────────────────┘
┌─ GossipMessage ───────────────────────────────────────────┐
│  Type   string      "PING" или "PONG"                     │
│  Sender NodeInfo    информация о себе                      │
│  Nodes  []NodeInfo  сплетни о других                       │
└────────────────────────────────────────────────────────────┘

ФУНКЦИИ:
┌───────────────────────┬──────────┬─────────────────────────┐
│ Функция               │ Лок?     │ Что делает              │
├───────────────────────┼──────────┼─────────────────────────┤
│ nodeToInfo(n)         │ нет      │ Node → NodeInfo (JSON)  │
│ applyNodeInfo(info)   │ Lock     │ Обновить/создать ноду   │
│ StartGossip()         │ нет      │ 3 горутины: accept,     │
│                       │          │ ticker, failure         │
│ StopGossip()          │ нет      │ close(stopCh)+Wait      │
│ handleGossipConn(conn)│ Lock×2   │ Принять PING→ответить   │
│                       │          │ PONG                    │
│ gossipTicker()        │ нет      │ for+select каждые 2 сек │
│ pingRandomNode()      │ RLock    │ Выбрать→dial→PING→PONG  │
│ buildMessage(type)    │ RLock    │ Собрать JSON-сообщение  │
│ failureDetector()     │ нет      │ for+select каждые 5 сек │
│ checkNodeHealth()     │ Lock     │ Проверить LastPong всех │
│ promoteToMaster(dead) │ —        │ Реплика→мастер (под     │
│                       │          │ Lock из checkNodeHealth)│
│ extractHost(addr)     │ нет      │ "host:port" → "host"   │
└───────────────────────┴──────────┴─────────────────────────┘
```

---

### 📄 [migration.go](https://github.com/Nikolay1994Kaz/storage_in_memory/blob/master/kvstore/internal/cluster/migration.go) — ПЕРЕНОС (176 строк)

```
┌───────────────────────┬──────────┬─────────────────────────┐
│ Функция               │ Лок?     │ Что делает              │
├───────────────────────┼──────────┼─────────────────────────┤
│ clusterSetSlot(args)  │ Lock     │ MIGRATING/IMPORTING/NODE│
│ clusterGetKeysInSlot  │ нет      │ callback→Store          │
│ MigrateKey(host,port, │ нет      │ Get→Send→Del ключ      │
│   key)                │          │                         │
│ SendSetToNode(addr,   │ нет      │ TCP→RESP SET→читать OK  │
│   key, value)         │          │                         │
└───────────────────────┴──────────┴─────────────────────────┘
```

---

### 📄 [replication.go](https://github.com/Nikolay1994Kaz/storage_in_memory/blob/master/kvstore/internal/cluster/replication.go) — КОПИЯ (168 строк)

```
┌─ ReplicationManager ──────────────────────────────────────┐
│  cluster     *Cluster                                      │
│  replicas    map[string]net.Conn    🔒mu    ID→соединение  │
│  mu          sync.RWMutex                                  │
│  masterConn  net.Conn               соединение к мастеру   │
│  stopCh      chan struct{}                                  │
│  StoreForEach func(fn)              ← callback             │
│  StoreSet     func(key, value)      ← callback             │
│  StoreDel     func(key)             ← callback             │
└────────────────────────────────────────────────────────────┘

┌───────────────────────────┬───────┬────────────────────────┐
│ Функция                   │ Лок?  │ Что делает             │
├───────────────────────────┼───────┼────────────────────────┤
│ HandlePsync(conn, id)     │ Lock  │ FULLSYNC→все ключи→    │
│                           │       │ DONE→сохранить conn    │
│ ForwardWrite(cmd)         │ RLock │ Fprintf всем репликам  │
│ ConnectToMaster(addr)     │ нет   │ Dial→PSYNC→scanner     │
│ applyReplicationCommand   │ нет   │ Parse "SET k v"→Store  │
│ Stop()                    │ нет   │ close(stopCh)+conn     │
└───────────────────────────┴───────┴────────────────────────┘
```

---

## 2. 🔗 Граф вызовов — кто кого вызывает

```
main.go
  │
  ├─ cluster.New(addr, gossipPort)
  │     ├─ generateNodeID()
  │     ├─ NewNode()                        ← node.go
  │     ├─ NewClusterState(self)            ← node.go
  │     └─ NewReplicationManager(c)         ← replication.go
  │
  ├─ cl.State.Self.AssignSlots(start, end)  ← node.go
  ├─ cl.State.RebuildSlotTable()            ← node.go
  │
  ├─ cl.StartGossip()                      ← gossip.go
  │     ├─ горутина: listener.Accept() → handleGossipConn()
  │     │     ├─ applyNodeInfo(msg.Sender)
  │     │     ├─ applyNodeInfo(msg.Nodes[i])
  │     │     └─ buildMessage("PONG") → Encode
  │     ├─ горутина: gossipTicker() → pingRandomNode()
  │     │     ├─ buildMessage("PING") → Encode
  │     │     ├─ Decode(pong)
  │     │     ├─ applyNodeInfo(pong.Sender)
  │     │     └─ applyNodeInfo(pong.Nodes[i])
  │     └─ горутина: failureDetector() → checkNodeHealth()
  │           └─ promoteToMaster(deadNode)
  │
  ├─── executeCommand() ─── для каждой команды клиента ───
  │     │
  │     ├─ "CLUSTER" → cl.HandleClusterCommand(args)
  │     │     ├─ "INFO"   → clusterInfo()
  │     │     ├─ "NODES"  → clusterNodes()
  │     │     ├─ "SLOTS"  → clusterSlots()
  │     │     ├─ "MEET"   → clusterMeet()
  │     │     │                └─ NewNode() + AddNode()
  │     │     ├─ "REPLICATE" → clusterReplicate()
  │     │     │                  └─ go Repl.ConnectToMaster()
  │     │     ├─ "SETSLOT" → clusterSetSlot()      ← migration.go
  │     │     └─ "GETKEYSINSLOT" → clusterGetKeysInSlot()
  │     │
  │     ├─ "MIGRATE" → cl.MigrateKey(host, port, key)
  │     │     ├─ MigrateGetFunc(key)        ← callback → Store.Get
  │     │     ├─ MigrateSetRemoteFunc()     → SendSetToNode()
  │     │     │     └─ net.Dial → RESP SET → read OK
  │     │     └─ MigrateDelFunc(key)        ← callback → Store.Del
  │     │
  │     ├─ "PSYNC" → cl.Repl.HandlePsync(conn, id)
  │     │     ├─ StoreForEach(fn)           ← callback
  │     │     └─ replicas[id] = conn
  │     │
  │     ├─ "SET" → cl.CheckKey(key)         ← nil или MOVED
  │     │     ├─ KeySlot(key)               ← slots.go
  │     │     ├─ IsMySlot(slot)             ← node.go
  │     │     └─ LookupSlot(slot)           ← node.go
  │     │   после SET:
  │     │     └─ cl.Repl.ForwardWrite("SET k v")
  │     │
  │     └─ "GET" → cl.CheckKey(key)         ← если не реплика
  │           если ключ не найден:
  │           └─ cl.CheckKeyAsk(key)        ← nil или ASK
```

---

## 3. 🔒 Карта блокировок — где берётся и зачем

### ClusterState.mu (sync.RWMutex) — ОДИН ЛОК на весь кластер

```
🔒 mu.Lock() (эксклюзивный — ЗАПИСЬ):
─────────────────────────────────────────────────────────────
applyNodeInfo()       │ Обновить/создать ноду + слоты + SlotTable
RebuildSlotTable()    │ Обнулить + заполнить всю SlotTable
AddNode()             │ Nodes[id] = node  (потом RebuildSlotTable)
handleGossipConn()    │ Обновить LastPong + State ноды
pingRandomNode()      │ Обновить LastPong + State ноды
checkNodeHealth()     │ Проверить все ноды + promoteToMaster
clusterSetSlot()      │ Migrating/Importing/SlotTable
clusterMeet()         │ RLock для проверки → потом AddNode(Lock)
clusterReplicate()    │ Nodes[masterID] + Self.Role

🔓 mu.RLock() (shared — ЧТЕНИЕ):
─────────────────────────────────────────────────────────────
LookupSlot()          │ return SlotTable[slot]
IsMySlot()            │ return SlotTable[slot] == Self
CheckKey()            │ IsMySlot + Migrating check + LookupSlot
CheckKeyAsk()         │ Migrating[slot] + Nodes[targetID]
buildMessage()        │ Читает Self + все Nodes для JSON
pingRandomNode()      │ Собирает candidates (все кроме Self)
clusterInfo()         │ Читает SlotTable + Nodes
clusterNodes()        │ Читает все Nodes
clusterSlots()        │ Читает SlotTable
clusterMeet()         │ Проверяет: уже есть такой Addr?
```

### ReplicationManager.mu (отдельный RWMutex)

```
🔒 rm.mu.Lock():    HandlePsync()     │ replicas[id] = conn
🔓 rm.mu.RLock():   ForwardWrite()    │ range replicas → Fprintf
```

---

## 4. 📦 Структуры данных — шпаргалка

```
ЧТО                        ТИП                     ГДЕ ЖИВЁТ
──────────────────────────  ──────────────────────── ─────────────
Все известные ноды          map[string]*Node         State.Nodes
  ключ = ID ноды              └─ "a1b2c3d4" → *Node

Маршрутизация слотов        [16384]*Node             State.SlotTable
  индекс = номер слота         └─ SlotTable[5000] = *Node

Слоты ноды (bitmap)         []bool (len=16384)       Node.Slots
                               └─ Slots[5000] = true/false

Мигрирующие слоты           map[uint16]string        State.Migrating
  ключ = номер слота           └─ 5000 → "e5f6g7h8" (целевой ID)

Импортируемые слоты         map[uint16]string        State.Importing
  ключ = номер слота           └─ 5000 → "a1b2c3d4" (источник ID)

CRC16 lookup таблица        [256]uint16              crc16Table (pkg-level)

Реплики (на мастере)        map[string]net.Conn      Repl.replicas
  ключ = ID реплики            └─ "e5f6g7h8" → net.Conn

Сигнал остановки            chan struct{}             Cluster.stopCh
  close(stopCh) → все горутины видят и выходят

WaitGroup для горутин       sync.WaitGroup           Cluster.wg
  wg.Add(1) при старте, wg.Done() при выходе, wg.Wait() при стопе
```

---

## 5. 🏃 Горутины — кто работает в фоне

```
┌──────────────────────────────────────────────────────────────┐
│                     ПРОЦЕСС НОДЫ                             │
│                                                              │
│  ┌─ Клиентский сервер (:6380) ──────────────────────────┐   │
│  │  acceptLoop → round-robin → N epoll workers          │   │
│  │  handleConn → executeCommand(... cl ...)              │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌─ Gossip сервер (:6381) ──────────────────────────────┐   │
│  │  Г1: listener.Accept() → go handleGossipConn(conn)   │   │
│  │      ↓                                                │   │
│  │      Для каждого входящего PING создаётся              │   │
│  │      короткоживущая горутина (обработала → умерла)     │   │
│  │                                                        │   │
│  │  Г2: gossipTicker (каждые 2 сек)                      │   │
│  │      → pingRandomNode()                               │   │
│  │      → dial → PING → read PONG → close conn          │   │
│  │                                                        │   │
│  │  Г3: failureDetector (каждые 5 сек)                   │   │
│  │      → checkNodeHealth()                              │   │
│  │      → если мастер FAIL и я реплика → promote         │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌─ Репликация (если реплика) ──────────────────────────┐   │
│  │  Г4: ConnectToMaster() — бесконечный scanner.Scan()  │   │
│  │      читает SET/DEL от мастера, применяет к Store     │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌─ Остановка ──────────────────────────────────────────┐   │
│  │  close(stopCh) → Г2,Г3 выходят из select             │   │
│  │  listener.Close() → Г1 Accept() возвращает error      │   │
│  │  wg.Wait() → ждём все 3                               │   │
│  └──────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────┘
```

---

## 6. 🎯 Быстрый поиск: «Где это?»

| Ищу... | Ответ |
|--------|-------|
| Как ключ → слот? | `slots.go:38` → `KeySlot(key)` → `CRC16(key) % 16384` |
| Где проверка «мой ли слот»? | `cluster.go:102` → `CheckKey()` → `IsMySlot()` |
| Где формируется MOVED? | `cluster.go:135` → `fmt.Sprintf("MOVED %d %s", slot, owner.Addr)` |
| Где формируется ASK? | `cluster.go:158` → `CheckKeyAsk()` |
| Как ноды знакомятся? | `cluster.go:447` → `clusterMeet()` → `AddNode()` |
| Где PING формируется? | `gossip.go:352` → `buildMessage("PING")` |
| Где PING отправляется? | `gossip.go:306-310` → `pingRandomNode()` → `encoder.Encode(ping)` |
| Где PING принимается? | `gossip.go:210-214` → `handleGossipConn()` → `decoder.Decode(&msg)` |
| Где обновляются данные? | `gossip.go:76` → `applyNodeInfo()` |
| Где обнаруживается сбой? | `gossip.go:405` → `checkNodeHealth()` |
| Где реплика→мастер? | `gossip.go:440` → `promoteToMaster()` |
| Где SET мигрирует? | `migration.go:122` → `MigrateKey()` → `SendSetToNode()` |
| Где ставится MIGRATING? | `migration.go:28-46` → `clusterSetSlot()` case "MIGRATING" |
| Где ставится IMPORTING? | `migration.go:48-62` → `clusterSetSlot()` case "IMPORTING" |
| Где миграция завершается? | `migration.go:64-91` → `clusterSetSlot()` case "NODE" |
| Где реплика подключается? | `replication.go:84` → `ConnectToMaster()` |
| Где мастер отдаёт данные? | `replication.go:43` → `HandlePsync()` |
| Где SET пересылается репликам? | `replication.go:69` → `ForwardWrite()` |
| Где callback-и подключаются? | `main.go:124-144` — все `cl.XXXFunc = func(...)` |

---

## 7. 🧠 Мнемоника — как запомнить

```
🏠 node.go      = ДАННЫЕ (структуры, типы, «что нода знает о себе»)
🧭 cluster.go   = МОЗГ   (маршрутизация CheckKey, команды CLUSTER *)
💬 gossip.go    = РОТ    (PING/PONG, сплетни, обнаружение сбоев)
📦 migration.go = РУКИ   (физический перенос ключей между нодами)
📋 replication.go = КОПИРКА (full sync + incremental)
#️⃣ slots.go     = КАЛЬКУЛЯТОР (CRC16 → номер слота)
```

**Правило для блокировок:**
- **Читаешь** SlotTable/Nodes/Migrating → **RLock** (много горутин одновременно ОК)
- **Пишешь** в них → **Lock** (эксклюзивно, один за раз)
- `clusterInfo/Nodes/Slots/CheckKey/buildMessage/pingRandom(candidates)` → RLock
- `applyNodeInfo/checkNodeHealth/clusterSetSlot/AddNode/Replicate` → Lock

**Правило для callbacks:**
- Кластер **не знает** про Store, WAL, TTL
- Все связи — через `func(...)` из main.go
- Если видишь `c.MigrateGetFunc(key)` — это `s.Get(key)` из main.go

**Правило для портов:**
- **port** = клиенты (SET/GET/MIGRATE/PSYNC приходят сюда)
- **port+1** = gossip (PING/PONG JSON между нодами)
