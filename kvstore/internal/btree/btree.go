package btree

// ============================================================================
// B+Tree — Сбалансированное дерево для Sorted Sets (ZADD/ZRANGE)
// ============================================================================
//
// Отличия от B-Tree:
//  1. Данные ТОЛЬКО в листьях. Внутренние узлы — навигация.
//  2. Листья связаны цепочкой (next) → range scan без подъёмов по дереву.
//  3. Ключ-разделитель ДУБЛИРУЕТСЯ: он есть и в родителе, и в правом листе.
//
// Визуализация (order=4):
//
//	                  [30 | 60]                ← внутренний (только ключи)
//	                 /    |    \
//	        [10 20]   [30 40 50]   [60 70 80]  ← листья (данные + цепочка)
//	           next→      next→
//
//	Поиск 45:  30≤45<60 → средний ребёнок → лист [30,40,50] → нашли 45? нет.
//	Range 25..50: найти 25 → лист [10,20] → idx=2 (нет) → next → [30,40,50] → стоп на 50.
//
// order — максимум ключей в узле.
// 32 ключа × 8 байт = 256 байт ≈ 4 cache-line процессора.
// 1 млн ключей → высота дерева ≈ 4 (log32(1_000_000) ≈ 3.9).
// Каждый поиск = 4 чтения памяти. Не 20 (как в AVL), а 4.
const order = 32

// item — одна запись. В терминах Redis:
//
//	ZADD leaderboard 1500 "alice" → item{key: 1500.0, value: "alice"}
type item struct {
	key   float64 // score
	value string  // member
}

// node — узел дерева. Бывает двух типов:
//
//	leaf=true:  items = данные, next = указатель на следующий лист
//	leaf=false: items = ключи-разделители, children = указатели на детей
//
// Массивы на +1 больше order — это слот для ВРЕМЕННОГО переполнения.
// Когда count > order → узел разделяется (split).
type node struct {
	items    [order + 1]item  // +1 = overflow-слот
	children [order + 2]*node // у N ключей N+1 детей, +1 для overflow
	count    int              // текущее количество ключей (0..order+1)
	leaf     bool             // лист или внутренний?
	next     *node            // → следующий лист (nil у внутренних)
}

// BPTree — B+дерево.
type BPTree struct {
	root *node
	len  int // общее количество элементов во всех листьях
}

// New создаёт пустое дерево. Корень — пустой лист.
func New() *BPTree {
	return &BPTree{
		root: &node{leaf: true},
	}
}

// Len возвращает количество элементов.
func (t *BPTree) Len() int { return t.len }

func (n *node) childIndex(key float64) int {
	lo, hi := 0, n.count
	for lo < hi {
		mid := (lo + hi) / 2
		if n.items[mid].key <= key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
