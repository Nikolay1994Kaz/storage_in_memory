// Package btree — B+-дерево для sorted set: упорядоченное множество пар
// (Score, Member) с обходом диапазона по цепочке листьев.
//
// Порядок как в Redis: по Score, при равных Score — по Member побайтово.
// Пара целиком и есть ключ, поэтому одинаковые score у разных member не
// конфликтуют, а ZRANGEBYSCORE по равным score отдаёт member в алфавитном
// порядке.
//
// Конкурентность: дерево НЕ потокобезопасно, его защищает вызывающий (у zset —
// RWMutex на одно множество: писатели Lock, читатели RLock).
//
// Прежняя версия жила в арене самописного аллокатора: узлы без Go-указателей,
// хендлы вместо ссылок, seqlock для чтения без локов и QSBR, чтобы читатель
// не прочёл освобождённый узел. Её спуск к началу диапазона шёл по правилу
// «score разделителя ≤ min → вправо» и пропускал левые листья, когда
// одинаковые score занимали несколько листьев: из 300 member с одним score
// ZRANGEBYSCORE возвращал 1. Здесь спуск идёт по нижней границе (min, "") —
// самому маленькому ключу с этим score.
package btree

import "sort"

// Item — элемент множества и одновременно ключ дерева.
type Item struct {
	Score  float64
	Member string
}

// less — строгий порядок (Score, Member). NaN в дерево не попадает: его
// отсекает вызывающий (ZADD отвергает NaN, как Redis), иначе сравнение
// перестаёт быть порядком.
func (a Item) less(b Item) bool {
	if a.Score != b.Score {
		return a.Score < b.Score
	}
	return a.Member < b.Member
}

const (
	// maxItems — потолок ключей в узле. 64 Item по 24 байта — полтора
	// килобайта на лист: двоичный поиск внутри узла дешевле лишнего уровня.
	maxItems = 64
	// minItems — нижний порог для некорневого узла; меньше — заём у соседа
	// или слияние с ним. Слияние двух узлов на пороге влезает в maxItems.
	minItems = maxItems / 2
)

// node — узел дерева.
//
// Лист: items — элементы по возрастанию, children == nil, next — следующий
// лист (цепочка для обхода диапазона).
//
// Внутренний узел: items — разделители, len(children) == len(items)+1.
// Инвариант: все ключи в children[i] < items[i] ≤ все ключи в children[i+1].
// Разделитель может не совпадать ни с одним живым элементом (элемент удалён) —
// для B+-дерева это нормально, неравенства от этого не ломаются.
type node struct {
	items    []Item
	children []*node
	next     *node
}

func (n *node) leaf() bool { return n.children == nil }

// lowerBound — первая позиция i, где items[i] ≥ it (или len(items)).
func lowerBound(items []Item, it Item) int {
	return sort.Search(len(items), func(i int) bool { return !items[i].less(it) })
}

// childIndex — в какого ребёнка спускаться за ключом it: число разделителей ≤ it.
func (n *node) childIndex(it Item) int {
	return sort.Search(len(n.items), func(i int) bool { return it.less(n.items[i]) })
}

// BPTree — B+-дерево. Нулевое значение не годится, создавать через New.
type BPTree struct {
	root *node
	len  int
}

// New создаёт пустое дерево (корень — пустой лист).
func New() *BPTree {
	return &BPTree{root: &node{}}
}

// Len — число элементов.
func (t *BPTree) Len() int { return t.len }

// Insert добавляет элемент. false — такая пара (Score, Member) уже есть.
func (t *BPTree) Insert(score float64, member string) bool {
	it := Item{Score: score, Member: member}
	inserted, sep, right := t.insert(t.root, it)
	if !inserted {
		return false
	}
	if right != nil {
		t.root = &node{items: []Item{sep}, children: []*node{t.root, right}}
	}
	t.len++
	return true
}

// insert вставляет it в поддерево n. При переполнении n делится пополам:
// возвращается разделитель и новый правый узел, их вставит родитель.
func (t *BPTree) insert(n *node, it Item) (inserted bool, sep Item, right *node) {
	if n.leaf() {
		i := lowerBound(n.items, it)
		if i < len(n.items) && n.items[i] == it {
			return false, Item{}, nil
		}
		n.items = insertItem(n.items, i, it)
		if len(n.items) <= maxItems {
			return true, Item{}, nil
		}
		mid := len(n.items) / 2
		r := &node{items: append(make([]Item, 0, maxItems+1), n.items[mid:]...), next: n.next}
		clear(n.items[mid:]) // не держать строки member в хвосте ёмкости
		n.items = n.items[:mid]
		n.next = r
		return true, r.items[0], r
	}

	ci := n.childIndex(it)
	inserted, sep, right = t.insert(n.children[ci], it)
	if right == nil {
		return inserted, Item{}, nil
	}
	n.items = insertItem(n.items, ci, sep)
	n.children = insertChild(n.children, ci+1, right)
	if len(n.items) <= maxItems {
		return true, Item{}, nil
	}
	// Делим внутренний узел: средний разделитель уходит вверх.
	mid := len(n.items) / 2
	up := n.items[mid]
	r := &node{
		items:    append(make([]Item, 0, maxItems+1), n.items[mid+1:]...),
		children: append(make([]*node, 0, maxItems+2), n.children[mid+1:]...),
	}
	clear(n.items[mid:])
	clear(n.children[mid+1:])
	n.items = n.items[:mid]
	n.children = n.children[:mid+1]
	return true, up, r
}

// Delete удаляет пару (Score, Member). false — её не было.
func (t *BPTree) Delete(score float64, member string) bool {
	if !t.delete(t.root, Item{Score: score, Member: member}) {
		return false
	}
	t.len--
	// Корень, у которого слияние забрало последний разделитель, держит одного
	// ребёнка — он и становится корнем; так дерево теряет уровень.
	if !t.root.leaf() && len(t.root.items) == 0 {
		t.root = t.root.children[0]
	}
	return true
}

func (t *BPTree) delete(n *node, it Item) bool {
	if n.leaf() {
		i := lowerBound(n.items, it)
		if i == len(n.items) || n.items[i] != it {
			return false
		}
		n.items = removeItem(n.items, i)
		return true
	}
	ci := n.childIndex(it)
	if !t.delete(n.children[ci], it) {
		return false
	}
	if len(n.children[ci].items) < minItems {
		rebalance(n, ci)
	}
	return true
}

// rebalance чинит недобор в ребёнке p.children[ci]: заём у соседа, у которого
// есть запас, иначе слияние с соседом (родитель теряет разделитель и ссылку).
func rebalance(p *node, ci int) {
	child := p.children[ci]
	if ci > 0 && len(p.children[ci-1].items) > minItems {
		left := p.children[ci-1]
		if child.leaf() {
			last := len(left.items) - 1
			child.items = insertItem(child.items, 0, left.items[last])
			left.items = removeItem(left.items, last)
			p.items[ci-1] = child.items[0]
		} else {
			last := len(left.items) - 1
			child.items = insertItem(child.items, 0, p.items[ci-1])
			child.children = insertChild(child.children, 0, left.children[last+1])
			p.items[ci-1] = left.items[last]
			left.items = removeItem(left.items, last)
			left.children = removeChild(left.children, last+1)
		}
		return
	}
	if ci < len(p.children)-1 && len(p.children[ci+1].items) > minItems {
		right := p.children[ci+1]
		if child.leaf() {
			child.items = append(child.items, right.items[0])
			right.items = removeItem(right.items, 0)
			p.items[ci] = right.items[0]
		} else {
			child.items = append(child.items, p.items[ci])
			child.children = append(child.children, right.children[0])
			p.items[ci] = right.items[0]
			right.items = removeItem(right.items, 0)
			right.children = removeChild(right.children, 0)
		}
		return
	}
	if ci > 0 {
		merge(p, ci-1)
	} else {
		merge(p, ci)
	}
}

// merge вливает p.children[i+1] в p.children[i] и убирает разделитель p.items[i].
func merge(p *node, i int) {
	left, right := p.children[i], p.children[i+1]
	if left.leaf() {
		left.items = append(left.items, right.items...)
		left.next = right.next
	} else {
		left.items = append(left.items, p.items[i])
		left.items = append(left.items, right.items...)
		left.children = append(left.children, right.children...)
	}
	p.items = removeItem(p.items, i)
	p.children = removeChild(p.children, i+1)
}

// Range вызывает fn для элементов с min ≤ Score ≤ max по возрастанию, пока fn
// возвращает true. Границы включительные, как в ZRANGEBYSCORE без «(».
func (t *BPTree) Range(min, max float64, fn func(Item) bool) {
	if !(min <= max) { // ловит и NaN
		return
	}
	low := Item{Score: min} // Member "" — наименьший ключ с этим score
	n := t.root
	for !n.leaf() {
		n = n.children[n.childIndex(low)]
	}
	for i := lowerBound(n.items, low); n != nil; n, i = n.next, 0 {
		for ; i < len(n.items); i++ {
			if n.items[i].Score > max {
				return
			}
			if !fn(n.items[i]) {
				return
			}
		}
	}
}

// ForEach обходит все элементы по возрастанию, пока fn возвращает true.
func (t *BPTree) ForEach(fn func(Item) bool) {
	n := t.root
	for !n.leaf() {
		n = n.children[0]
	}
	for ; n != nil; n = n.next {
		for _, it := range n.items {
			if !fn(it) {
				return
			}
		}
	}
}

// ── срезы ───────────────────────────────────────────────────

func insertItem(s []Item, i int, it Item) []Item {
	s = append(s, Item{})
	copy(s[i+1:], s[i:])
	s[i] = it
	return s
}

func removeItem(s []Item, i int) []Item {
	copy(s[i:], s[i+1:])
	s[len(s)-1] = Item{} // не держать строку member удалённого элемента
	return s[:len(s)-1]
}

func insertChild(s []*node, i int, c *node) []*node {
	s = append(s, nil)
	copy(s[i+1:], s[i:])
	s[i] = c
	return s
}

func removeChild(s []*node, i int) []*node {
	copy(s[i:], s[i+1:])
	s[len(s)-1] = nil // не держать слитый узел
	return s[:len(s)-1]
}
