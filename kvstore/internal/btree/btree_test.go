package btree

import (
	"math"
	"math/rand"
	"slices"
	"strconv"
	"testing"
)

// check проверяет все инварианты дерева и возвращает его высоту:
//   - листья на одной глубине;
//   - некорневой узел держит от minItems до maxItems ключей;
//   - у внутреннего узла детей на одного больше, чем разделителей;
//   - ключи поддерева лежат в границах, заданных разделителями предков;
//   - цепочка листьев отдаёт все элементы строго по возрастанию и их ровно Len.
func check(t *testing.T, tr *BPTree) int {
	t.Helper()
	leafDepth := -1
	var walk func(n *node, depth int, lo, hi *Item, isRoot bool)
	walk = func(n *node, depth int, lo, hi *Item, isRoot bool) {
		if !isRoot && (len(n.items) < minItems || len(n.items) > maxItems) {
			t.Fatalf("узел на глубине %d держит %d ключей (допустимо %d..%d)", depth, len(n.items), minItems, maxItems)
		}
		for i, it := range n.items {
			if i > 0 && !n.items[i-1].less(it) {
				t.Fatalf("ключи узла не по возрастанию: %v, %v", n.items[i-1], it)
			}
			if lo != nil && it.less(*lo) {
				t.Fatalf("ключ %v меньше нижней границы %v", it, *lo)
			}
			if hi != nil && !it.less(*hi) {
				t.Fatalf("ключ %v не меньше верхней границы %v", it, *hi)
			}
		}
		if n.leaf() {
			if leafDepth == -1 {
				leafDepth = depth
			} else if depth != leafDepth {
				t.Fatalf("листья на разной глубине: %d и %d", leafDepth, depth)
			}
			return
		}
		if len(n.children) != len(n.items)+1 {
			t.Fatalf("детей %d при %d разделителях", len(n.children), len(n.items))
		}
		for i, c := range n.children {
			clo, chi := lo, hi
			if i > 0 {
				clo = &n.items[i-1]
			}
			if i < len(n.items) {
				chi = &n.items[i]
			}
			walk(c, depth+1, clo, chi, false)
		}
	}
	walk(tr.root, 1, nil, nil, true)

	var all []Item
	tr.ForEach(func(it Item) bool { all = append(all, it); return true })
	for i := 1; i < len(all); i++ {
		if !all[i-1].less(all[i]) {
			t.Fatalf("цепочка листьев не по возрастанию: %v, %v", all[i-1], all[i])
		}
	}
	if len(all) != tr.Len() {
		t.Fatalf("в цепочке листьев %d элементов, Len()=%d", len(all), tr.Len())
	}
	return leafDepth
}

func rangeOf(tr *BPTree, min, max float64) []Item {
	var out []Item
	tr.Range(min, max, func(it Item) bool { out = append(out, it); return true })
	return out
}

func TestInsertDuplicatePair(t *testing.T) {
	tr := New()
	if !tr.Insert(10, "old") || !tr.Insert(10, "new") {
		t.Fatal("разные member с одним score — два разных элемента")
	}
	if tr.Insert(10, "old") {
		t.Fatal("повторная вставка той же пары должна вернуть false")
	}
	if tr.Len() != 2 {
		t.Fatalf("Len=%d, ожидали 2", tr.Len())
	}
}

// Регресс: равные score, растянутые на несколько листов. Прежнее дерево
// спускалось к началу диапазона в САМЫЙ ПРАВЫЙ лист с этим score и теряло
// все левые: из 300 member с score=5 ZRANGEBYSCORE 5 5 отдавал 1.
func TestRangeEqualScoresAcrossLeaves(t *testing.T) {
	tr := New()
	for i := 0; i < 300; i++ {
		tr.Insert(5, "m"+strconv.Itoa(1000+i))
	}
	for i := 0; i < 50; i++ {
		tr.Insert(1, "low"+strconv.Itoa(i))
		tr.Insert(9, "high"+strconv.Itoa(i))
	}
	if h := check(t, tr); h < 2 {
		t.Fatalf("высота %d: тест обязан растянуть score=5 на несколько листов", h)
	}
	cases := []struct {
		min, max float64
		want     int
	}{
		{5, 5, 300},
		{5, 9, 350},
		{1, 5, 350},
		{math.Inf(-1), math.Inf(1), 400},
		{2, 4, 0},
		{9, 1, 0}, // min > max
	}
	for _, c := range cases {
		if got := len(rangeOf(tr, c.min, c.max)); got != c.want {
			t.Errorf("Range(%v, %v) = %d, ожидали %d", c.min, c.max, got, c.want)
		}
	}
	// Внутри одного score — алфавитный порядок member, как в Redis.
	got := rangeOf(tr, 5, 5)
	if !slices.IsSortedFunc(got, func(a, b Item) int { return compareStr(a.Member, b.Member) }) {
		t.Fatal("member с равным score не в алфавитном порядке")
	}
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestRangeEarlyStop(t *testing.T) {
	tr := New()
	for i := 0; i < 1000; i++ {
		tr.Insert(float64(i), "m")
	}
	n := 0
	tr.Range(0, 999, func(Item) bool { n++; return n < 10 })
	if n != 10 {
		t.Fatalf("fn вызвана %d раз после false, ожидали остановку на 10", n)
	}
}

func TestRangeNaNBoundIsEmpty(t *testing.T) {
	tr := New()
	tr.Insert(1, "a")
	if len(rangeOf(tr, math.NaN(), 10)) != 0 || len(rangeOf(tr, 0, math.NaN())) != 0 {
		t.Fatal("диапазон с NaN-границей должен быть пустым")
	}
}

// Вставка по возрастанию и по убыванию — два худших порядка для делений.
func TestSequentialInsertsAndDeleteAll(t *testing.T) {
	for _, desc := range []bool{false, true} {
		tr := New()
		const n = 20_000
		for i := 0; i < n; i++ {
			s := i
			if desc {
				s = n - 1 - i
			}
			tr.Insert(float64(s), "m"+strconv.Itoa(s))
		}
		if h := check(t, tr); h < 3 {
			t.Fatalf("desc=%v: высота %d для %d элементов — деления не происходят", desc, h, n)
		}
		for i := 0; i < n; i++ {
			if !tr.Delete(float64(i), "m"+strconv.Itoa(i)) {
				t.Fatalf("desc=%v: Delete(%d) не нашёл элемент", desc, i)
			}
			if i%997 == 0 {
				check(t, tr)
			}
		}
		// Слияния обязаны вернуть дерево к одному пустому листу: прежде узлы
		// не освобождались вовсе, и дерево держало пиковый объём навсегда.
		if tr.Len() != 0 || !tr.root.leaf() || len(tr.root.items) != 0 {
			t.Fatalf("desc=%v: после удаления всего Len=%d, корень-лист=%v, ключей в корне %d",
				desc, tr.Len(), tr.root.leaf(), len(tr.root.items))
		}
	}
}

func TestDeleteMissing(t *testing.T) {
	tr := New()
	tr.Insert(1, "a")
	if tr.Delete(1, "b") || tr.Delete(2, "a") {
		t.Fatal("удаление отсутствующей пары должно вернуть false")
	}
	if tr.Len() != 1 {
		t.Fatalf("Len=%d", tr.Len())
	}
}

// Модельный тест: случайные вставки, удаления и диапазоны против оракула —
// отсортированного среза. Мало различных score, чтобы равные score постоянно
// растягивались по листьям — тот самый режим, где падало прежнее дерево.
func TestRandomizedAgainstOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	tr := New()
	var oracle []Item
	cmp := func(a, b Item) int {
		if a.less(b) {
			return -1
		}
		if b.less(a) {
			return 1
		}
		return 0
	}
	steps := 60_000
	if testing.Short() {
		steps = 20_000
	}
	for step := 0; step < steps; step++ {
		it := Item{Score: float64(rng.Intn(40)), Member: "m" + strconv.Itoa(rng.Intn(3000))}
		pos, found := slices.BinarySearchFunc(oracle, it, cmp)
		switch op := rng.Intn(10); {
		case op < 6:
			if got := tr.Insert(it.Score, it.Member); got != !found {
				t.Fatalf("шаг %d: Insert(%v)=%v, оракул: новый=%v", step, it, got, !found)
			}
			if !found {
				oracle = slices.Insert(oracle, pos, it)
			}
		case op < 9:
			if got := tr.Delete(it.Score, it.Member); got != found {
				t.Fatalf("шаг %d: Delete(%v)=%v, оракул: был=%v", step, it, got, found)
			}
			if found {
				oracle = slices.Delete(oracle, pos, pos+1)
			}
		default:
			lo := float64(rng.Intn(40))
			hi := lo + float64(rng.Intn(5))
			got := rangeOf(tr, lo, hi)
			var want []Item
			for _, o := range oracle {
				if o.Score >= lo && o.Score <= hi {
					want = append(want, o)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("шаг %d: Range(%v,%v) дал %d элементов, оракул %d", step, lo, hi, len(got), len(want))
			}
		}
		if step%5000 == 0 {
			check(t, tr)
		}
	}
	check(t, tr)
	var all []Item
	tr.ForEach(func(it Item) bool { all = append(all, it); return true })
	if !slices.Equal(all, oracle) {
		t.Fatalf("итог разошёлся с оракулом: %d против %d", len(all), len(oracle))
	}
}

func BenchmarkInsert(b *testing.B) {
	tr := New()
	members := make([]string, 4096)
	for i := range members {
		members[i] = "member:" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Insert(float64(i), members[i&4095])
	}
}

func BenchmarkRange100(b *testing.B) {
	tr := New()
	for i := 0; i < 100_000; i++ {
		tr.Insert(float64(i), "member:"+strconv.Itoa(i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lo := float64(i % 99_900)
		n := 0
		tr.Range(lo, lo+99, func(Item) bool { n++; return true })
		if n != 100 {
			b.Fatalf("n=%d", n)
		}
	}
}
