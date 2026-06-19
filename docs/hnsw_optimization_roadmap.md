# Roadmap оптимизаций HNSW: Память и Масштабирование

> **Статус:** Задокументировано по результатам глубокого анализа (июнь 2026).
> Каждая оптимизация подтверждена бенчмарками на Intel i7-9750H.

---

## Текущая архитектура (что работает и почему)

```
┌──────────────────────────────────────────────────────────────────┐
│                        HNSW Graph                                 │
│                                                                    │
│  []Node (Go slice, 40B/node)    — плоский массив, O(1) access     │
│  VectorArena ([]float32)        — плоский массив координат        │
│  TCMalloc (neighbors)           — per-node variable-size blocks   │
│                                                                    │
│  Три аллокатора — НЕ дублирование, а специализация:               │
│    • []Node: random access 1.4ns (vs 5.4ns TCMalloc Resolve)     │
│    • VectorArena: contiguous для batchDistance + AVX2             │
│    • TCMalloc: variable-size blocks + OOM tracking + deferred free│
└──────────────────────────────────────────────────────────────────┘
```

### Бенчмарки доступа (baseline)

```
Direct slice access:      0.35 ns/op
Node slice access [i%N]:  1.40 ns/op
TCMalloc Resolve:         5.40 ns/op  (×15 vs direct, ×4 vs node)

Search (10K, dim=128):    160 µs/op, 3 allocs, 912 B/op
Search (10K, dim=640):    ~40 µs/query (parallel, 12 cores)
```

### Бенчмарки layout (AoS vs SoA vs Compact)

```
Sequential (10K nodes):
  Current AoS 40B:  3,744 ns  ← ЛУЧШИЙ
  Compact AoS 16B:  3,970 ns  (+6%)
  SoA:             12,781 ns  (×3.4 хуже)

Random (4000 accesses):
  Current AoS 40B:  1,915 ns  ← ЛУЧШИЙ
  Compact AoS 16B:  3,754 ns  (×2.0 хуже)
  SoA:              3,612 ns  (×1.9 хуже)
```

**Вывод:** НЕ менять layout Node struct. 40B AoS — оптимален для HNSW access pattern.

---

## Оптимизация 1: OOM Tracking (приоритет: ВЫСОКИЙ)

**Проблема:** `heap.usedBytes` не учитывает VectorArena и []Node.
При dim=768, 100K нод → **235 MB невидимы** для IsOOM().

**Решение:** Подключить к единому atomic counter через `heap.MemoryCounter()`.

```go
// vector_arena.go — добавить поле
type VectorArena struct {
    data        []float32
    dim         int
    freeOffsets []uint32
    memCounter  *atomic.Int64  // shared с MHeap.usedBytes
}

// В Allocate, при append (не free list reuse):
func (va *VectorArena) Allocate(vec []float32) uint32 {
    // ... free list path (без Add — память уже учтена) ...
    
    // append path:
    offset := uint32(len(va.data))
    va.data = append(va.data, vec...)
    if va.memCounter != nil {
        va.memCounter.Add(int64(va.dim * 4))
    }
    return offset
}
```

**Стоимость:** 1 atomic.Add (~1ns) на Insert.
**Gain:** OOM лимит видит ВСЮ память.
**Сложность:** 5-10 строк.

---

## Оптимизация 2: Neighbor Memory Waste (приоритет: СРЕДНИЙ)

**Проблема:** TCMalloc size classes = степени двойки.
Level 0 neighbors = 264B → class 512B → **93.9% waste**.
На 1M нод = **360 MB чистых потерь**.

```
Level  Нужно   Выделено  Waste   % нод   Weighted
  0    264B    512B      248B    100%     248B × N
  1    400B    512B      112B     36%      40B × N
  2    536B    1024B     488B     13%      63B × N
```

**Варианты решения:**

### Вариант A: Добавить size class 288B
```go
var sizeClasses = [numSizeClasses+1]int{
    32, 64, 128, 256, 288, 512, 1024, 2048, 4096,
}
```
Waste level 0: 248B → 24B (−90%).
⚠️ Ломает snapshot Handle encoding → нужна миграция версии.

### Вариант B: Neighbor Arena (вместо TCMalloc для связей)
Выделить один плоский `[]uint64` для всех neighbor lists,
фиксированный размер на ноду = `(1+M0) + maxLevel*(1+M)`.
Доступ как VectorArena — по offset, O(1), без Resolve.

Плюс: waste=0, latency 0.35ns вместо 5.4ns.
Минус: фиксированный max level, нет variable-size → перерасход для level 0 нод
если max level высокий.

### Вариант C (рекомендуется при >100K нод):
Гибрид — отдельная Level0Arena (fixed 264B per node, flat array)
+ TCMalloc для upper levels (variable, rare).

```
Level 0: 100% нод → Level0Arena (flat []uint64, zero waste, 0.35ns access)
Level 1+: 36% нод → TCMalloc (variable size, 5.4ns access, acceptable)
```

**Когда делать:** при росте до 100K+ нод с ощутимым memory pressure.

---

## Оптимизация 3: Chunked VectorArena (приоритет: при масштабировании)

**Проблема:** `append(va.data, vec...)` при overflow копирует ВЕСЬ массив.

```
 N нод     dim    VectorArena     Grow cost (copy)     Pause
 10K       128    4.9 MB          ~3 MB                <1ms  ✅
 100K      128    49 MB           ~32 MB               ~10ms ⚠️
 100K      768    293 MB          ~195 MB              ~60ms ❌
 1M        768    2.9 GB          ~1.9 GB              ~500ms ❌
```

**Решение:** Chunked арена с чанками от MHeap (1MB каждый).

```go
type ChunkedVectorArena struct {
    chunks     [][]float32    // массив 1MB чанков
    dim        int
    slotsPerChunk int         // = 1MB / (dim * 4)
    totalSlots int
    freeSlots  []uint32
    memCounter *atomic.Int64
}

func (ca *ChunkedVectorArena) Get(slotID uint32) []float32 {
    ci := slotID / uint32(ca.slotsPerChunk)
    si := slotID % uint32(ca.slotsPerChunk)
    off := si * uint32(ca.dim)
    return ca.chunks[ci][off : off+uint32(ca.dim)]
}
```

**Характеристики:**
- Grow: выделить 1 чанк (1MB) вместо copy всего массива
- Access: +1 div + 1 mod ≈ +1ns per Get → +4µs per Search (+2.5%)
- Cache locality: НЕ теряется (Search и так random access по VectorOffset)
- Snapshot: пишем чанки последовательно, формат совместим

**Когда делать:** при планах на 100K+ нод с dim ≥ 256.
**НЕ делать** если датасет ≤ 50K нод — текущий append справляется.

---

## Оптимизация 4: Node.ID removal (приоритет: НИЗКИЙ)

**Факт:** `Node.ID` всегда равен индексу в массиве (graph.go:422).
Убрав его, Node = 32B (−20%). Но бенчмарки показали что 40B Node
быстрее для всех access patterns. **НЕ делать ради скорости.**

Делать только если нужна экономия памяти (8B × N нод) при очень большом N.
⚠️ Ломает snapshot формат (Section 2 = raw bytes Node struct).

---

## Матрица приоритетов

| # | Оптимизация | Gain | Effort | Когда |
|---|-------------|------|--------|-------|
| 1 | OOM Tracking | Корректный MAXMEMORY | 5 строк | **Сейчас** |
| 2C | Level0Arena | −360MB waste на 1M нод | 100 строк | >100K нод |
| 3 | Chunked VectorArena | −500ms pause при grow | 150 строк | >100K нод, dim≥256 |
| 2A | Size class 288B | −90% waste level 0 | 20 строк + миграция | Альтернатива 2C |
| 4 | Node.ID removal | −8B per node | 50 строк + миграция | Только при давлении RAM |

---

## Чего НЕ делать (подтверждено бенчмарками)

| Идея | Почему НЕТ |
|------|-----------|
| Перевести VectorArena на TCMalloc | **−50% Search** (cache miss на фрагментированных span'ах) |
| Перевести []Node на TCMalloc | +5.4ns access, сломает snapshot (−10× save, −14× load) |
| SoA layout для Node | **×3.4 медленнее** sequential, ×1.9 медленнее random |
| Compact Node (16B) | **×2.0 медленнее** random access (бенчмарк подтвердил) |
| Убрать TCMalloc для neighbors | Variable-size blocks → нужен dynamic alloc |
