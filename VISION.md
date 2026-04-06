# 🚀 VISION: KVStore → Molten — Programmable In-Memory Engine

> **Миссия:** Создать первый Redis-совместимый in-memory движок со встроенными
> вычислениями. Один бинарник. Ноль зависимостей. Данные и логика — в одном процессе.

> **Слоган:** *«The cache that thinks.»*

---

## Откуда мы идём и куда

```
БЫЛО (ROADMAP.md):                        БУДЕТ (VISION.md):
───────────────────                        ─────────────────
  Учебный KV Store                     →     Коммерческий продукт
  "Понять, как работает Redis"         →     "Заменить Redis + Kafka + микросервис"
  Один node, pet project               →     Production-ready engine + SaaS
  Бенчмарки для портфолио              →     Бенчмарки как маркетинговое оружие
```

---

## Что уже построено (Фундамент)

| Компонент | Файл | Статус | Что даёт продукту |
|:---|:---|:---:|:---|
| **Arena Allocator** | `store/arena.go` | ✅ | Zero-GC хранение. GC pause <100μs при 5M+ ключей |
| **WAL + Snapshots** | `wal/wal.go` | ✅ | Durability. Данные переживают рестарт |
| **Pub/Sub Hub** | `pubsub/pubsub.go` | ✅ | Real-time уведомления. sync.Pool, backpressure |
| **TTL / Expiration** | `store/ttl.go` | ✅ | Автоудаление. MinHeap + lazy + active |
| **RESP Protocol** | `protocol/resp.go` | ✅ | Redis-совместимость. `redis-cli` работает из коробки |
| **Кластеризация** | `cluster/` | ✅ | Gossip, slots, replication, migration |
| **Шардирование** | `store/sharded.go` | ✅ | 256 шардов, FNV-1a хеширование |

**Итог:** У нас есть State + Messaging + Durability. Не хватает **одного** — Compute.

---

## Что мы строим: Programmable In-Memory Engine

### Ключевая идея

В классической архитектуре высоконагруженной системы всегда есть боль:

```
Классический путь (5-15 ms на операцию):
┌────────┐     ┌──────────┐     ┌────────────┐     ┌─────────┐     ┌───────┐
│ Client │────►│ API/gRPC │────►│ Go Service │────►│  Redis  │────►│ Kafka │
│        │◄────│          │◄────│ (логика)   │◄────│ (кэш)   │     │(event)│
└────────┘     └──────────┘     └────────────┘     └─────────┘     └───────┘
                                      │
                              4 сетевых hop'а
                              3 отдельных процесса
                              Проблема консистентности
```

**Molten ломает этот шаблон:**

```
Molten путь (<100 μs на операцию):
┌────────┐     ┌─────────────────────────────────────────────────┐
│ Client │────►│               Molten (один процесс)           │
│  SET   │     │  ┌─────────┐    ┌──────────┐    ┌───────────┐  │
│        │◄────│  │ KV Store│◄──►│ WASM     │───►│ Pub/Sub   │  │
│        │     │  │ (Arena) │    │ (логика) │    │ (события) │  │
└────────┘     │  └─────────┘    └──────────┘    └───────────┘  │
               │         ▲              │              │         │
               │         └──── WAL ─────┴──────────────┘         │
               └─────────────────────────────────────────────────┘
                              0 сетевых hop'ов
                              1 процесс
                              Атомарная консистентность
```

### Как это работает для клиента

```bash
# 1. Загружаем WASM-модуль с бизнес-логикой
redis-cli -p 6380 WASM.LOAD fraud_scorer ./fraud_model.wasm

# 2. Ставим триггер: при каждом SET на tx:* — запускать scoring
redis-cli -p 6380 WASM.TRIGGER SET "tx:*" fraud_scorer score_transaction

# 3. Бизнес просто делает SET — всё остальное автоматически
redis-cli -p 6380 SET tx:12345 '{"amount":100,"user":"u789"}'
# → WASM-триггер активируется:
#   → GET user:profile:u789 (из KV, 0 network)
#   → Вычисляет fraud_score
#   → SET tx:12345:score 0.87 (в KV, 0 network)
#   → PUBLISH fraud_alerts "tx:12345 score=0.87" (в Pub/Sub, 0 network)
# → Ответ клиенту: OK
# Всё за < 100 μs
```

---

## Конкурентный ландшафт

### Почему существующие решения не закрывают эту нишу

```
                        Есть State (KV)
                             │
                    ┌────────┴────────┐
                    │                 │
              Есть Compute      Нет Compute
                    │                 │
            ┌───────┴──────┐    ┌─────┴─────┐
            │              │    │            │
        Есть Broker   Нет Broker│         NATS
            │              │    Redis     KeyDB
         Molten 🎯   Tarantool Memcached
         Fluvio (β)    (Lua only)
                             │
                        Нет State
                             │
                    ┌────────┴────────┐
                    │                 │
              Есть Compute      Нет Compute
                    │                 │
               Redpanda          Kafka
            (stateless only)    RabbitMQ
```

### Детальное сравнение

| Возможность | Redis | Tarantool | Redpanda | Fluvio | **Molten** |
|:---|:---:|:---:|:---:|:---:|:---:|
| In-Memory KV | ✅ | ✅ | ❌ | ⚠️ | ✅ |
| Zero-GC Architecture | ❌ | ❌ | N/A | N/A | ✅ |
| Message Broker | ⚠️ Streams | ❌ | ✅ | ✅ | ✅ |
| Programmable (WASM) | ❌ Lua/JS | ❌ Lua | ✅ stateless | ✅ beta | ✅ stateful |
| Redis-совместимость | ✅ | ❌ | ❌ | ❌ | ✅ |
| Single binary deploy | ❌ | ❌ | ✅ | ⚠️ | ✅ |
| Язык ядра | C | C+Lua | C++ | Rust | **Go** |
| State + Compute + Broker | ❌ | ⚠️ | ❌ | ⚠️ β | **✅** |

### Наши уникальные преимущества

1. **Redis-совместимость** — `redis-cli` работает. Zero learning curve.
2. **Go + Single Binary** — `curl | tar | ./molten`. Никакого JVM, Docker'а, Helm'а.
3. **Zero-GC Arena** — стабильная subsecond latency при миллионах ключей.
4. **WASM Isolation** — бизнес-логика в песочнице. Crash в WASM ≠ crash сервера.
5. **State + Compute + Broker в одном процессе** — нет аналогов на Go.

---

## Целевые рынки

### Tier 1: Основные (B2B)

| Индустрия | Боль | Как Molten решает |
|:---|:---|:---|
| **FinTech** | Fraud-скоринг за <5ms, консистентность балансов | WASM-триггер на каждую транзакцию, атомарное обновление |
| **AdTech** | Real-time bidding за <10ms | KV с профилем юзера + WASM scoring = ответ без сети |
| **GameDev** | Состояние игрока, game events, leaderboards | KV state + WASM game logic + Pub/Sub для мультиплеера |
| **IoT / Edge** | Агрегация телеметрии на edge-устройствах | Лёгкий бинарник (Go, ARM) + WASM алерты |

### Tier 2: Вертикальные SaaS (B2B/B2C)

| Продукт | Описание | Цена |
|:---|:---|:---|
| **Loyalty Engine SaaS** | API для программ лояльности. Правила → WASM → мгновенный расчёт | $49–$2499/мес |
| **Real-time Analytics API** | Подсчёт метрик в реальном времени (counters, percentiles) | $99–$999/мес |

---

## Размер рынка

```
                    In-Memory Computing
                    ┌────────────────────┐
                    │   $14B – $32B      │
                    │   CAGR: 15-20%     │
                    │                    │
                    │  ┌──────────────┐  │
                    │  │Event Stream  │  │     WASM Cloud
                    │  │$8B – $12B   │  │   ┌────────────┐
                    │  │CAGR: 20%    │  │   │ $1.8B      │
                    │  │             │  │   │ CAGR: >30%  │
                    │  │   ┌────────┼──┼───┤            │
                    │  │   │ Molten│  │   │            │
                    │  │   │ TAM:   │  │   │            │
                    │  │   │$500M-  │  │   │            │
                    │  │   │ $2B    │  │   │            │
                    │  │   └────────┼──┼───┤            │
                    │  │            │  │   └────────────┘
                    │  └────────────┘  │
                    └─────────────────-┘
```

**Наш TAM:** ~$500M – $2B (пересечение In-Memory + Streaming + WASM).
Достаточно для компании с $50M+ ARR.

---

## Архитектура продукта

### Итоговая структура проекта

```
kvstore/
├── cmd/
│   └── kvstore/
│       └── main.go                    # Точка входа
├── internal/
│   ├── protocol/
│   │   └── resp.go                    # ✅ RESP-парсер
│   ├── store/
│   │   ├── arena.go                   # ✅ Zero-GC Arena Allocator
│   │   ├── sharded.go                 # ✅ 256-shard storage
│   │   └── ttl.go                     # ✅ TTL / Expiration
│   ├── server/
│   │   ├── tcp.go                     # ✅ TCP-сервер
│   │   └── epoll.go                   # ✅ Epoll Event Loop
│   ├── wal/
│   │   └── wal.go                     # ✅ WAL + Rotation + Snapshot
│   ├── pubsub/
│   │   └── pubsub.go                  # ✅ Pub/Sub Hub
│   ├── cluster/
│   │   ├── cluster.go                 # ✅ Cluster manager
│   │   ├── gossip.go                  # ✅ Gossip protocol
│   │   ├── replication.go             # ✅ Master-replica sync
│   │   ├── migration.go              # ✅ Slot migration
│   │   └── slots.go                   # ✅ Hash slots
│   │
│   │   ─── НОВОЕ: WASM Compute Layer ───
│   │
│   ├── compute/                       # 🔜 WASM Runtime
│   │   ├── runtime.go                 # Обёртка над wazero
│   │   ├── host_functions.go          # Host API: kv_get, kv_set, publish
│   │   ├── trigger.go                 # Система триггеров ON_SET/ON_DEL/ON_EXPIRE
│   │   └── module_store.go            # Хранилище WASM-модулей
│   │
│   │   ─── НОВОЕ: Transactions ───
│   │
│   └── tx/                            # 🔜 Транзакции
│       └── transaction.go             # TXBEGIN/TXCOMMIT/TXDISCARD
│
└── examples/
    ├── fraud_scorer/                  # 🔜 Пример: fraud scoring WASM module
    │   ├── main.go                    #   (компилируется в WASM)
    │   └── README.md
    └── loyalty_engine/                # 🔜 Пример: loyalty cashback calculator
        ├── main.go
        └── README.md
```

### WASM Host Function API

Это **контракт** между Molten и загруженными WASM-модулями.
WASM-модуль может вызывать эти функции для взаимодействия с данными:

```
┌──────────────────────────────────────────────────┐
│              WASM Module (песочница)              │
│                                                  │
│  func score_transaction(key []byte) {            │
│      profile := host.kv_get("user:" + userID)    │  ◄── Читает из KV
│      score := calculate(profile, txData)         │  ◄── Бизнес-логика
│      host.kv_set("tx:" + txID + ":score", score) │  ◄── Пишет в KV
│      host.publish("alerts", alertMsg)            │  ◄── Шлёт в Pub/Sub
│      host.log("scored tx " + txID)               │  ◄── Логирование
│  }                                               │
│                                                  │
├──────────────────────────────────────────────────┤
│              Host Functions (Go)                 │
│──────────────────────────────────────────────────│
│  kv_get(key_ptr, key_len) → (val_ptr, val_len)  │
│  kv_set(key_ptr, key_len, val_ptr, val_len) → i32│
│  kv_del(key_ptr, key_len) → i32                 │
│  publish(chan_ptr, chan_len, msg_ptr, msg_len)→i32│
│  log_info(msg_ptr, msg_len)                      │
│  log_error(msg_ptr, msg_len)                     │
│  current_time_ms() → i64                         │
├──────────────────────────────────────────────────┤
│              Molten Engine                      │
│  ┌──────────┐  ┌──────────┐  ┌────────────────┐ │
│  │ ArenaStore│  │ Pub/Sub  │  │ WAL + Snapshot │ │
│  └──────────┘  └──────────┘  └────────────────┘ │
└──────────────────────────────────────────────────┘
```

### Новые RESP-команды

```
# Управление модулями
WASM.LOAD <module_name> <binary>           # Загрузить WASM-модуль
WASM.DROP <module_name>                    # Удалить WASM-модуль
WASM.LIST                                  # Список загруженных модулей
WASM.INFO <module_name>                    # Метаданные модуля

# Триггеры (реактивное программирование)
WASM.TRIGGER SET "tx:*" fraud_scorer score_transaction
WASM.TRIGGER DEL "session:*" cleanup cleanup_session
WASM.TRIGGER EXPIRE "cache:*" analytics on_cache_miss
WASM.TRIGGERS                              # Список всех триггеров
WASM.UNTRIGGER <trigger_id>                # Удалить триггер

# Прямой вызов (императивное программирование)
WASM.EXEC <module_name> <func_name> [args...]  # Вызвать функцию
WASM.EVAL <module_name> <func_name> <key>       # Вызвать с ключом как контекст
```

---

## Монетизация

### Стратегия: Open-Core + Vertical SaaS

```
                          Revenue Streams
                               │
                 ┌─────────────┼─────────────┐
                 │             │             │
           Open Source    Enterprise     SaaS Products
           (бесплатно)    License       (вертикали)
                 │             │             │
           Community      $500-2K/        $49-2.5K/
           adoption       мес/нода         мес/клиент
                 │             │             │
           ─────────     ─────────      ─────────
           KV + Pub/Sub  Clustering     Loyalty API
           WASM engine   Raft consensus Analytics API
           WAL/Snapshot  SSO/LDAP       Fraud API  
           Single node   Dashboard
           RESP proto    Connectors
                         SLA support
```

### Open Source (Community Edition)

Всё, что нужно для development и small-scale production:
- ✅ KV Store + Arena + TTL
- ✅ Pub/Sub
- ✅ WAL + Snapshots
- ✅ WASM Runtime + Triggers
- ✅ Single-node deployment
- ✅ RESP-протокол
- ✅ Community support (GitHub Issues)

### Enterprise Edition ($500–$2000/мес за ноду)

Фичи для серьёзного production:
- 🔒 Multi-node кластер с Raft-консенсусом
- 🔒 Шардирование данных между нодами
- 🔒 SSO/LDAP интеграция
- 🔒 Web Dashboard: мониторинг WASM-модулей, latency, throughput
- 🔒 Коннекторы: WAL → ClickHouse, WAL → S3, WAL → PostgreSQL
- 🔒 Role-Based Access Control (RBAC)
- 🔒 Priority support + SLA

### SaaS: Loyalty Engine API (Первый вертикальный продукт)

| Tier | RPS | Профилей | Хранилище | Цена/мес |
|:---|:---|:---|:---|:---|
| **Starter** | до 100 | 10K | 1 GB | $49 |
| **Business** | до 1K | 100K | 10 GB | $299 |
| **Pro** | до 10K | 1M | 100 GB | $999 |
| **Enterprise** | до 50K+ | 10M+ | Custom | $2,499+ |

**Unit Economics:**
- Стоимость инфраструктуры для Pro-клиента: ~$50/мес (VPS + мониторинг)
- Цена Pro: $999/мес
- **Маржинальность: ~95%** (благодаря Zero-GC in-memory архитектуре)

---

## Execution Roadmap

### Phase 0: WASM Foundation (Недели 1–3)

> **Цель:** WASM-модуль может читать/писать KV и публиковать в Pub/Sub

```
Неделя 1:
  [ ] Добавить зависимость wazero в go.mod
  [ ] Создать internal/compute/runtime.go
      - Загрузка WASM-модуля из байтов
      - Compilation + instantiation через wazero
      - Управление lifecycle модулей
  [ ] Создать internal/compute/module_store.go
      - Хранилище модулей в памяти (map[string]*CompiledModule)

Неделя 2:
  [ ] Создать internal/compute/host_functions.go
      - kv_get: читает из ArenaStore через линейную память WASM
      - kv_set: пишет в ArenaStore + WAL
      - kv_del: удаляет из ArenaStore + WAL
      - publish: вызывает Hub.Publish()
      - log_info / log_error
      - current_time_ms
  [ ] Создать тестовый WASM-модуль на Go (→ WASM)
      - Простой echo: получает ключ, читает значение, пишет обратно с префиксом

Неделя 3:
  [ ] Создать internal/compute/trigger.go
      - Система паттерн-матчинга: "tx:*" → glob match
      - Hook в ArenaStore.Set() / Del() → вызов WASM
      - Timeout для WASM-вызовов (max 10ms по умолчанию)
  [ ] Добавить RESP-команды: WASM.LOAD, WASM.EXEC, WASM.TRIGGER
  [ ] Интегрировать в server/tcp.go
```

### Phase 1: Benchmarks + PoC (Неделя 4)

> **Цель:** Доказать преимущество цифрами. Это — маркетинговое оружие.

```
  [ ] Написать бенчмарк: "Классический путь" vs "Molten + WASM"
      Сценарий: 1M транзакций, каждая требует:
        - Чтение профиля пользователя
        - Вычисление (fraud score / loyalty points)
        - Запись результата
        - Отправка события
      
      Baseline: Go HTTP → Redis GET → compute → Redis SET → Kafka PUBLISH
      Molten:  SET → WASM trigger → kv_get → compute → kv_set → publish
      
  [ ] Целевые показатели:
      - Latency:    < 100μs (Molten) vs 2-5ms (baseline) → 20-50x
      - Throughput:  500K ops/sec vs 50K ops/sec           → 10x
      - Memory:     Стабильная (Zero-GC) vs пилообразная
  
  [ ] Создать examples/fraud_scorer/ — полный рабочий пример
  [ ] README с графиками latency, throughput, GC pauses
```

### Phase 2: Production Hardening (Недели 5–7)

> **Цель:** Стабильность для реальных нагрузок

```
Неделя 5: Arena Compaction
  [ ] Отслеживание "мёртвых" записей в арене (fragmentation ratio)
  [ ] Copy-compact: создать новую арену, скопировать живые записи
  [ ] Пороговый триггер: compact при fragmentation > 30%
  [ ] Бенчмарк: throughput во время compaction

Неделя 6: WAL Group Commit + Batch CRC
  [ ] Batch-write: несколько Entry → один WriteBatch() с единым CRC32
  [ ] Group commit: накапливать записи за 1ms → один fsync
  [ ] Для WASM-триггеров: все kv_set внутри одного вызова → один WAL batch

Неделя 7: Persisted Pub/Sub Streams
  [ ] Опциональный mode: SUBSCRIBE channel PERSIST
  [ ] При включении — сообщения пишутся в WAL
  [ ] При рестарте — replay последних N сообщений подписчикам
  [ ] Retention policy: по времени или по количеству
```

### Phase 3: Observability + DX (Неделя 8)

> **Цель:** Продукт, которым приятно пользоваться

```
  [ ] Prometheus endpoint: /metrics
      - molten_ops_total{op="SET|GET|DEL"}
      - molten_wasm_executions_total{module="...", func="..."}
      - molten_wasm_execution_duration_seconds
      - molten_wasm_errors_total
      - molten_arena_fragmentation_ratio
      - molten_pubsub_messages_total
      - molten_wal_sync_duration_seconds
      - molten_gc_pause_seconds
  [ ] WASM.INFO <module> — показывает: загружен когда, вызовов, avg latency, errors
  [ ] Structured logging (JSON формат для production)
  [ ] Graceful shutdown + drain WASM triggers
```

### Phase 4: Open Source Launch (Неделя 9)

> **Цель:** Первые звёзды на GitHub, первые пользователи

```
  [ ] Репозиторий: github.com/NikolayKaz/molten
  [ ] README.md:
      - Hero banner с диаграммой архитектуры
      - "Get started in 30 seconds" (curl + run + redis-cli)
      - Benchmark charts (vs Redis + external service)
      - "Your first WASM trigger" — tutorial
  [ ] Примеры в /examples:
      - fraud_scorer (Go → WASM)
      - loyalty_calculator (Go → WASM)
      - rate_limiter (Go → WASM)
  [ ] Docker image: ghcr.io/nikolaykaz/molten:latest
  [ ] Landing page (опционально)
  [ ] Посты:
      - Hacker News: "Show HN: Molten — Redis with built-in WASM compute"
      - Reddit r/golang: "I built an in-memory engine with WASM triggers in Go"
      - lobste.rs
      - Dev.to / Habr
```

### Phase 5: Монетизация (Недели 10–16)

```
Путь A — Enterprise License:
  [ ] Raft-консенсус для multi-node
  [ ] Web dashboard (React/Vue)
  [ ] RBAC + Auth
  [ ] Лицензионный сервер

Путь B — SaaS Loyalty Engine:
  [ ] API Gateway + auth (API keys)
  [ ] Web UI: визуальный редактор правил → WASM compilation
  [ ] Billing (Stripe)
  [ ] Multi-tenant deployment
```

---

## Целевые бенчмарки (v1.0)

| Метрика | Цель | Для сравнения: Redis |
|:---|:---|:---|
| SET ops/sec (single node) | > 500K | ~400K |
| GET ops/sec (single node) | > 800K | ~500K |
| SET + WASM trigger ops/sec | > 200K | N/A (нет аналога) |
| P99 latency (SET) | < 200μs | ~150μs |
| P99 latency (SET + WASM) | < 500μs | N/A |
| GC pause (10M keys) | < 100μs | N/A (C, нет GC) |
| Memory (10M keys, 100B values) | < 3 GB | ~2.5 GB |
| Cold start | < 100ms | ~50ms |
| WASM module load time | < 10ms | N/A |
| Binary size | < 15 MB | ~8 MB |

---

## Принципы разработки

### 1. Performance is a Feature
Каждый commit проверяется бенчмарком. Regression в latency = bug severity P0.

### 2. Zero-Allocation Hot Path
На горячем пути (SET/GET) — ноль аллокаций. `sync.Pool`, stack-allocated arrays, arena.

### 3. Single Binary, Zero Dependencies
`go build` → один файл. Работает на VPS за $5/мес.

### 4. Redis-Compatible First
Если `redis-cli` не работает — это баг. Adoption > инновация в протоколе.

### 5. WASM = Isolation + Portability
WASM-модули запускаются в песочнице. Crash WASM ≠ crash сервера. Timeout обязателен.

### 6. Benchmarks are Marketing
Каждый Pull Request с перформанс-улучшением = пост в блоге = звёзды на GitHub.

---

## Связь с ROADMAP.md

Этот документ **продолжает** ROADMAP.md:

```
ROADMAP.md (Уровни 0-9):
  Уровень 0: RESP ────────────── ✅ Готово
  Уровень 1: Naive Store ─────── ✅ Готово
  Уровень 2: Sharding ────────── ✅ Готово
  Уровень 3: Arena Zero-GC ──── ✅ Готово
  Уровень 4: Epoll ───────────── ✅ Готово
  Уровень 5: WAL ─────────────── ✅ Готово
  Уровень 6: TTL ─────────────── ✅ Готово
  Уровень 7: Pub/Sub ─────────── ✅ Готово
  Уровень 8: Кластеризация ──── ✅ Готово
  Уровень 9: Tunable Guarantees  ⬜ В планах

VISION.md (следующие уровни — продуктовые):
  Уровень 10: WASM Compute ──── 🔜 Phase 0    ← ВОТ ТУТ МЫ
  Уровень 11: Arena Compaction ─ 🔜 Phase 2
  Уровень 12: Persisted Streams  🔜 Phase 2
  Уровень 13: Observability ──── 🔜 Phase 3
  Уровень 14: OSS Launch ─────── 🔜 Phase 4
  Уровень 15: Enterprise/SaaS ── 🔜 Phase 5
```

---

> **Это больше не учебный проект. Это ядро продукта, который занимает пустующую нишу
> между Redis, Kafka и серверными фреймворками. Один бинарник. Данные + логика + события.
> Ничего лишнего.**
