# HNSW Vector Search: Эволюция оптимизаций

> Пошаговый лог всех оптимизаций HNSW-графа.
> Каждый шаг содержит: что было → что стало → почему → результат.
>
> CPU: Intel Core i7-9750H @ 2.60GHz
> Benchmark: `BenchmarkSearch_Full` (10000 vectors, dim=128, K=10, ef=100)

---

## Baseline (до оптимизаций)

```
BenchmarkSearch_Full-12    ~886K ns/op    2000 B/op    5 allocs/op
```

CPU профиль:
- `EuclideanDistance` — 61%
- `mapaccess1_fast64` (g.nodes map) — 17%
- `reflect.Swapper` (sort.Slice) — 8%
- `mapassign_fast64` (visited map) — 4%
- `make()` в pruneNeighbors — 5%
- `make()` в Insert — 3%
- `Map.Clear` (visited) — 1%

Аллокации:
- `sort.Slice` → `reflect.Swapper` — вызов рефлексии на каждый sort
- `pruneNeighbors` → `make([]item)` + `make([]uint64)` на каждый вызов
- `Insert` → `make([]uint64)` для обратных связей
- `searchLayer` → `make([]item)` для результатов
- `searchLayer(ef=1)` → sync.Pool + map + heap ради одного числа

---

## Шаг 1: `sort.Slice` → `slices.SortFunc`

**Файл:** `graph.go` — `pruneNeighbors`, `pruneNeighborsFromList`

**Было:**
```go
sort.Slice(items, func(i, j int) bool {
    return items[i].dist < items[j].dist
})
```

**Стало:**
```go
slices.SortFunc(items, func(a, b item) int {
    if a.dist < b.dist { return -1 }
    if a.dist > b.dist { return 1 }
    return 0
})
```

**Почему:** `sort.Slice` использует `reflect.Swapper` для создания swap-функции.
Это аллокация + вызов через интерфейс на КАЖДЫЙ sort. `slices.SortFunc`
из Go 1.21 — generics, компилируется в прямой код без рефлексии.

**Убрано:** ~29% аллокаций (`reflect.Swapper`)

---

## Шаг 2: Переиспользуемые буферы в `pruneNeighbors`

**Файл:** `graph.go` — struct `Graph`, `pruneNeighbors`, `pruneNeighborsFromList`

**Было:**
```go
func (g *Graph) pruneNeighbors(node *Node, level int, maxCount int) {
    items := make([]item, len(neighbors))    // ← аллокация на каждый вызов
    // ...
    pruned := make([]uint64, len(items))     // ← ещё одна аллокация
}
```

**Стало:**
```go
// В структуре Graph:
pruneBufItems []item   // capacity = M0+1, переиспользуется
pruneBufIDs   []uint64 // capacity = M0+1, переиспользуется

func (g *Graph) pruneNeighbors(node *Node, level int, maxCount int) {
    if cap(g.pruneBufItems) < len(neighbors) {
        g.pruneBufItems = make([]item, len(neighbors)) // grow only if needed
    }
    items := g.pruneBufItems[:len(neighbors)] // zero-alloc slice
    // ...
}
```

**Почему:** `pruneNeighbors` вызывается только из `Insert`/`Delete`, которые
защищены `mu.Lock()`. Буферы безопасны без дополнительной синхронизации.

**Убрано:** ~49% аллокаций

---

## Шаг 3: Переиспользуемый буфер в `Insert`

**Файл:** `graph.go` — struct `Graph`, `Insert`

**Было:**
```go
// В Insert, для обратных связей:
updated := append([]uint64{}, existing...)   // ← аллокация
updated = append(updated, id)
```

**Стало:**
```go
// В структуре Graph:
insertBuf []uint64 // capacity = M0+1

// В Insert:
updated := g.insertBuf[:len(existing)+1]
copy(updated, existing)
updated[len(existing)] = id
```

**Убрано:** ~17% аллокаций

---

## Шаг 4: `greedyClosest` для ef=1

**Файл:** `graph.go` — новый метод `greedyClosest`

**Было:**
```go
// На верхних слоях HNSW (ef=1):
for lc := g.maxLevel; lc > level; lc-- {
    results := g.searchLayer(vec, ep, 1, lc)  // ← pool + map + heap ради 1 числа
    if len(results) > 0 { ep = results[0].id }
}
```

**Стало:**
```go
func (g *Graph) greedyClosest(query []float32, entryID uint64, level int) uint64 {
    bestID := entryID
    bestDist := g.Distance(query, g.arena.Get(g.nodes[entryID].VectorOffset))
    improved := true
    for improved {
        improved = false
        node := &g.nodes[bestID]
        for _, neighborID := range g.neighborsArena.GetNeighbors(node.NeighborsOffset, level) {
            dist := g.Distance(query, g.arena.Get(g.nodes[neighborID].VectorOffset))
            if dist < bestDist {
                bestID = neighborID
                bestDist = dist
                improved = true
            }
        }
    }
    return bestID
}
```

**Почему:** На верхних слоях нам нужна ровно одна ближайшая нода.
`searchLayer` для ef=1 тянет за собой: sync.Pool.Get + map alloc +
heap push/pop + Pool.Put. `greedyClosest` — тесный цикл с Distance,
0 аллокаций, 0 overhead.

---

## Шаг 5: Переиспользуемый `searchResultBuf`

**Файл:** `graph.go` — struct `Graph`, `searchLayer`

**Было:**
```go
func (g *Graph) searchLayer(...) []item {
    // ... поиск ...
    result := make([]item, len(collected))  // ← аллокация на каждый вызов
    copy(result, collected)
    return result
}
```

**Стало:**
```go
// В структуре Graph:
searchResultBuf []item

func (g *Graph) searchLayer(...) []item {
    // ... поиск ...
    g.searchResultBuf = g.searchResultBuf[:0]
    g.searchResultBuf = append(g.searchResultBuf, state.collected...)
    return g.searchResultBuf  // вызывающий использует до следующего searchLayer
}
```

### Результат после шагов 1-5:

```
BenchmarkSearch_Full-12    ~636K ns/op    160 B/op    1 alloc/op
```

| Метрика         | Baseline | После шагов 1-5 |
|-----------------|----------|------------------|
| ns/op           | 886K     | 636K (−28%)      |
| B/op            | 2000     | 160 (−92%)       |
| allocs/op       | 5        | 1 (−80%)         |

---

## Шаг 6: `map[uint64]*Node` → `[]Node` (плоский массив)

**Файлы:** `graph.go`, `store.go`

Это самое крупное структурное изменение. Заменяем хэш-таблицу нод на
контигуальный массив с прямым доступом по индексу.

### 6a. Структура `Node`

**Было:**
```go
type Node struct {
    ID              uint64
    VectorOffset    uint32
    NeighborsOffset uint32
    Level           int
}
```

**Стало:**
```go
type Node struct {
    ID              uint64
    VectorOffset    uint32
    NeighborsOffset uint32
    Level           int
    Alive           bool   // tombstone маркер для Delete
}
```

### 6b. Структура `Graph`

**Было:**
```go
nodes map[uint64]*Node  // хэш-таблица, каждая *Node — отдельный объект в куче
```

**Стало:**
```go
nodes     []Node    // плоский массив, индекс = ID ноды, один объект для GC
nodeCount int       // количество живых нод (без дыр)
freeIDs   []uint32  // стек свободных индексов (от Delete)
```

### 6c. `Insert`

**Было:** принимает `id uint64` снаружи, пишет в map.
**Стало:** возвращает `uint32` индекс, использует free list.

```go
func (g *Graph) Insert(vec []float32) uint32 {
    var idx uint32
    if len(g.freeIDs) > 0 {
        idx = g.freeIDs[len(g.freeIDs)-1]       // переиспользуем ячейку
        g.freeIDs = g.freeIDs[:len(g.freeIDs)-1]
    } else {
        idx = uint32(len(g.nodes))               // новая ячейка
        g.nodes = append(g.nodes, Node{})
    }
    g.nodes[idx] = Node{..., Alive: true}
    g.nodeCount++
    return idx
}
```

### 6d. `Delete`

**Было:** `delete(g.nodes, id)` — удаление из map.
**Стало:** tombstone + free list.

```go
node.Alive = false
g.freeIDs = append(g.freeIDs, uint32(id))
g.nodeCount--
```

### 6e. Все lookup-ы

**Было:** `g.nodes[id]` → hash(id) → найти бакет → разыменовать *Node (~15ns)
**Стало:** `&g.nodes[id]` → base + id×sizeof(Node) → одна инструкция LEA (~1ns)

### 6f. `store.go`

- Убран `nextID atomic.Uint64` — Graph сам управляет индексами
- `Add()` получает индекс от `graph.Insert(vec)`

**Почему:**
- `map[uint64]*Node` — N отдельных объектов в куче, GC сканирует каждый
- `[]Node` — один контигуальный блок, GC видит один объект без указателей
- Прямой доступ по индексу vs hash chain traversal
- Cache locality: соседние ноды лежат рядом в памяти

### Результат CPU профиля:

```
mapaccess1_fast64 (g.nodes):  17% → 0%  ← ПОЛНОСТЬЮ ИСЧЕЗЛО
mapassign_fast64 (g.nodes):   4% → 0%  ← ПОЛНОСТЬЮ ИСЧЕЗЛО
```

---

## Шаг 7: `visited map[uint64]bool` → bitset `[]uint64`

**Файл:** `graph.go` — `searchState`, `searchLayer`

Последний map в hot path. После перехода на `[]Node` все ID стали
плотными индексами (0, 1, 2, ...) — идеально для bitset.

### 7a. Структура `searchState`

**Было:**
```go
type searchState struct {
    visited map[uint64]bool  // hash table: ~15ns на проверку, ~20ns на запись
}
```

**Стало:**
```go
type searchState struct {
    visited []uint64  // bitset: 1 бит на ноду
}

func (s *searchState) isVisited(id uint64) bool {
    return s.visited[id/64] & (1 << (id%64)) != 0  // одна инструкция AND
}

func (s *searchState) setVisited(id uint64) {
    s.visited[id/64] |= 1 << (id % 64)  // одна инструкция OR
}
```

### 7b. `acquire`

**Было:**
```go
func (s *searchState) acquire() {
    clear(s.visited)  // обход всех бакетов map
}
```

**Стало:**
```go
func (s *searchState) acquire(nodeSlots int) {
    needed := (nodeSlots + 63) / 64
    if cap(s.visited) < needed {
        s.visited = make([]uint64, needed)
    } else {
        s.visited = s.visited[:needed]
        for i := range s.visited { s.visited[i] = 0 }  // memclr intrinsic
    }
}
```

### 7c. `searchLayer`

**Было:**
```go
state.visited[entryID] = true
if state.visited[neighborID] { continue }
state.visited[neighborID] = true
```

**Стало:**
```go
state.setVisited(entryID)
if state.isVisited(neighborID) { continue }
state.setVisited(neighborID)
```

**Почему:**
- map lookup: hash(id) + bucket chain + pointer chase = ~15ns
- bitset check: `array[id/64] & mask` = ~1ns (одна инструкция AND)
- map insert: hash + assign = ~20ns
- bitset set: `array[id/64] |= mask` = ~1ns (одна инструкция OR)
- map clear: обход всех бакетов = ~500ns для 10K нод
- bitset clear: `memclr` 1.2KB = ~50ns

**Размер bitset для 10000 нод:**
```
ceil(10000 / 64) × 8 = 1256 байт (~1.2 КБ)
```

### Результат CPU профиля:

```
mapaccess1_fast64 (visited): 10.5% → 0%   ← ПОЛНОСТЬЮ ИСЧЕЗЛО
mapassign_fast64 (visited):   1.7% → 0%   ← ПОЛНОСТЬЮ ИСЧЕЗЛО
memhash64:                    1.8% → 0%
h2 (map internals):           2.1% → 0%
matchH2:                      1.2% → 0%
Map.Clear:                    0.8% → 0%

isVisited (bitset, inline):   — → 0.68%   ← ЗАМЕНА
```

---

## Финальный результат

```
Baseline:     886,000 ns/op    2000 B/op    5 allocs/op
Финал:        422,000 ns/op     160 B/op    1 alloc/op
```

### Итого:

| Метрика     | Baseline | Финал    | Улучшение          |
|-------------|----------|----------|--------------------|
| Latency     | 886K ns  | 422K ns  | **2.1× быстрее**  |
| Memory      | 2000 B   | 160 B    | **12.5× меньше**   |
| Allocations | 5/op     | 1/op     | **5× меньше**      |

### CPU профиль — эволюция:

```
                          Baseline  →  Финал
EuclideanDistance:           61%    →   82%   (потолок без SIMD)
map operations (все):       31%    →    0%   (полностью убраны)
Heap (minHeap/maxHeap):      5%    →    7%
VectorArena.Get:             3%    →    2%
sort/slices:                 8%    →    2%
bitset (isVisited):          —     →   0.7%
```

**82% CPU уходит на чистую математику расстояний** — это кремниевый потолок.
Дальнейшее ускорение возможно только через SIMD (AVX2/AVX-512).

---

## Хронология файлов

| Шаг | Файлы | Суть изменения |
|-----|-------|----------------|
| 1   | `graph.go` | `sort.Slice` → `slices.SortFunc` |
| 2   | `graph.go` | `pruneBufItems`, `pruneBufIDs` в Graph |
| 3   | `graph.go` | `insertBuf` в Graph |
| 4   | `graph.go` | новый метод `greedyClosest` |
| 5   | `graph.go` | `searchResultBuf` в Graph |
| 6   | `graph.go`, `store.go` | `map[uint64]*Node` → `[]Node` + free list |
| 7   | `graph.go` | `visited map` → bitset `[]uint64` |
| 8   | `distance.go`, `store.go` | Pre-normalization: `CosineDistance` → `DotProductDistance` |

---

## Шаг 8: Pre-normalization для Cosine Distance

**Файлы:** `distance.go`, `store.go`, `store_test.go`

### Проблема

`CosineDistance` на каждый вызов делает:
- **3 цикла** по всем элементам (dot, normA, normB)
- **2 вызова `math.Sqrt`** (~20-40 тактов каждый)
- **1 деление** float64 (~20 тактов)

```go
// 3 цикла + 2 sqrt + деление = 193 ns/op (dim=128)
func CosineDistance(a, b []float32) float32 {
    var dot, normA, normB float32
    for i := range a {
        dot   += a[i] * b[i]
        normA += a[i] * a[i]
        normB += b[i] * b[i]
    }
    return 1 - dot / float32(math.Sqrt(float64(normA)) * math.Sqrt(float64(normB)))
}
```

### Решение

Если все векторы нормализованы (|v| = 1.0), формула упрощается:

```
CosineDistance(a, b) = 1 - dot(a,b) / (|a| × |b|)
                     = 1 - dot(a,b) / (1.0 × 1.0)
                     = 1 - dot(a,b)
```

### 8a. Новая distance функция

```go
// 1 цикл, 0 sqrt, 0 делений = 100 ns/op (dim=128)
func DotProductDistance(a, b []float32) float32 {
    var dot float32
    for i := range a {
        dot += a[i] * b[i]
    }
    return 1 - dot
}
```

### 8b. `NewVectorStoreCosine`

```go
func NewVectorStoreCosine() *VectorStore {
    vs := &VectorStore{
        keys:          make(map[uint64]string),
        ids:           make(map[string]uint64),
        autoNormalize: true,                      // ← авто-нормализация
    }
    vs.graph = NewGraph(DotProductDistance)        // ← быстрая distance
    return vs
}
```

### 8c. Автоматическая нормализация

В `Add()`:
```go
if vs.autoNormalize {
    normalized := make([]float32, len(vec))
    copy(normalized, vec)    // не трогаем оригинал пользователя
    Normalize(normalized)
    insertVec = normalized
}
```

В `Search()`:
```go
if vs.autoNormalize {
    normalized := make([]float32, len(query))
    copy(normalized, query)
    Normalize(normalized)
    searchQuery = normalized
}
```

### Результат — изолированный бенчмарк distance (dim=128):

```
EuclideanDistance:     68 ns/op   — 1 цикл, 0 sqrt (baseline)
CosineDistance:       193 ns/op   — 3 цикла + 2 sqrt + деление
DotProductDistance:   100 ns/op   — 1 цикл, 0 sqrt
```

**CosineDistance → DotProductDistance: ускорение ×1.93** на самой функции.

Это стандартная оптимизация Milvus, Qdrant, Pinecone.
API для пользователя не меняется — нормализация прозрачна.

---

## Ключевые принципы

1. **Профилируй, не гадай** — каждое изменение начиналось с `go tool pprof`
2. **Zero-alloc hot path** — аллокации убиты переиспользуемыми буферами
3. **Специализация** — `greedyClosest` для ef=1 вместо общего `searchLayer`
4. **Data locality** — `[]Node` вместо `map[uint64]*Node` для cache hits
5. **Bit-level operations** — bitset вместо hash map для boolean множеств
6. **Concurrency-aware** — буферы в Graph безопасны под `mu.Lock()`, bitset в searchState безопасен через `sync.Pool`
7. **Mathematical reduction** — pre-normalization превращает 3 цикла + 2 sqrt в 1 цикл

