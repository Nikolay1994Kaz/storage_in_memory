package btree

import (
	"math/rand"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// countNodes считает достижимые из корня узлы дерева (leaf + internal).
// Утечка «btree не merge'ит листья» проявляется как число узлов, которое НЕ
// уменьшается после удаления элементов: узлы аллоцируются на split, но freeNode
// не вызывается нигде → дерево держит high-water-mark узлов навсегда.
func countNodes(t *BPTree) int {
	var walk func(h tcmalloc.Handle) int
	walk = func(h tcmalloc.Handle) int {
		if h == nilHandle {
			return 0
		}
		nd := resolveNode(t.store, h)
		n := 1
		if !nd.leaf {
			for i := int32(0); i <= nd.count; i++ {
				n += walk(nd.children[i])
			}
		}
		return n
	}
	return walk(t.loadRoot())
}

// TestLeafMerge_ReclaimsNodesAfterDeleteAll — измеряет утечку узлов.
//
// Вставляем N (много split'ов → много узлов), затем удаляем все. Здоровое
// дерево должно вернуться к одному пустому корню. С утечкой число узлов
// остаётся на пике.
func TestLeafMerge_ReclaimsNodesAfterDeleteAll(t *testing.T) {
	tree, _ := newTestTree()

	const n = 5000
	for i := range n {
		tree.Insert(0, float64(i), keyFor(i))
	}
	peak := countNodes(tree)
	t.Logf("узлов на пике (%d элементов): %d", n, peak)
	if peak < 10 {
		t.Fatalf("предусловие: ожидали дерево из многих узлов, got %d", peak)
	}

	for i := range n {
		tree.Delete(float64(i))
	}
	if tree.Len() != 0 {
		t.Fatalf("после удаления всех Len=%d, want 0", tree.Len())
	}
	after := countNodes(tree)
	t.Logf("узлов после удаления всех: %d", after)

	if after != 1 {
		t.Fatalf("space leak: после удаления всех элементов дерево держит %d узлов, want 1 (пустой корень)", after)
	}
}

func keyFor(i int) string {
	// стабильные разной длины ключи, уникальные по i
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 0, 8)
	x := i + 1
	for x > 0 {
		b = append(b, alphabet[x%len(alphabet)])
		x /= len(alphabet)
	}
	return string(b)
}

// TestLeafMerge_RandomizedChurnCorrectness — дифференциальный тест против
// эталонной map. Случайные Insert/Delete по уникальным score; после churn'а
// каждый живой ключ обязан находиться с ПРАВИЛЬНЫМ значением, удалённые —
// отсутствовать, Len совпадать. Ловит любую порчу разделителей/данных в
// borrow/merge. Плюс проверка утилизации: число узлов должно резко упасть
// относительно пика (иначе reclaim не работает).
func TestLeafMerge_RandomizedChurnCorrectness(t *testing.T) {
	tree, _ := newTestTree()
	ref := map[int]string{}
	rng := rand.New(rand.NewSource(0xC0FFEE))

	const ops = 40000
	const keyspace = 4000
	peak := 0
	for range ops {
		k := rng.Intn(keyspace)
		if _, live := ref[k]; live && rng.Intn(2) == 0 {
			tree.Delete(float64(k))
			delete(ref, k)
		} else {
			v := keyFor(k)
			tree.Insert(0, float64(k), v)
			ref[k] = v
		}
		if n := countNodes(tree); n > peak {
			peak = n
		}
	}

	if tree.Len() != len(ref) {
		t.Fatalf("Len=%d, эталон=%d", tree.Len(), len(ref))
	}
	for k, v := range ref {
		got, ok := tree.Search(float64(k))
		if !ok {
			t.Fatalf("живой ключ %d потерян после churn (порча borrow/merge)", k)
		}
		if got != v {
			t.Fatalf("ключ %d: got %q, want %q (порча данных)", k, got, v)
		}
	}
	for k := range keyspace {
		if _, live := ref[k]; !live {
			if _, found := tree.Search(float64(k)); found {
				t.Fatalf("удалённый ключ %d всё ещё находится", k)
			}
		}
	}

	nodes := countNodes(tree)
	t.Logf("живых=%d, узлов сейчас=%d, пик узлов=%d", len(ref), nodes, peak)
	// Утилизация ≥50% → листьев ≈ len/minKeys; узлов ≪ len/4. Утечка держала бы пик.
	if bound := len(ref)/4 + 20; nodes > bound {
		t.Fatalf("узлов=%d > %d — reclaim/утилизация не держится (leak?)", nodes, bound)
	}
}

// TestLeafMerge_CompositeChurnCorrectness — то же, но по composite-ключу
// (score, member) с ДУБЛИРУЮЩИМИСЯ score (как в реальном zset): ZADD/ZREM
// через DeleteMember. Полная сверка мультимножества через ForEach.
func TestLeafMerge_CompositeChurnCorrectness(t *testing.T) {
	tree, _ := newTestTree()
	ref := map[string]float64{} // member → score (члены уникальны)
	rng := rand.New(rand.NewSource(0xBEEF))

	const ops = 40000
	const members = 4000
	const scoreBuckets = 40 // много дубликатов score

	for range ops {
		m := keyFor(rng.Intn(members))
		if s, live := ref[m]; live && rng.Intn(2) == 0 {
			tree.DeleteMember(s, m)
			delete(ref, m)
		} else {
			if s, live := ref[m]; live {
				tree.DeleteMember(s, m) // ZADD поверх: снять старый score
			}
			s := float64(rng.Intn(scoreBuckets))
			tree.Insert(0, s, m)
			ref[m] = s
		}
	}

	if tree.Len() != len(ref) {
		t.Fatalf("Len=%d, эталон=%d", tree.Len(), len(ref))
	}
	got := map[string]float64{}
	dups := 0
	tree.ForEach(func(score float64, member string) {
		if _, seen := got[member]; seen {
			dups++
		}
		got[member] = score
	})
	if dups != 0 {
		t.Fatalf("%d дубликатов member в дереве (сирота от неатомарного RMW/порчи merge)", dups)
	}
	if len(got) != len(ref) {
		t.Fatalf("в дереве %d членов, эталон %d", len(got), len(ref))
	}
	for m, s := range ref {
		if got[m] != s {
			t.Fatalf("член %q: score %v в дереве, want %v", m, got[m], s)
		}
	}
}

// TestLeafMerge_RangeAfterChurn — RangeSearch остаётся корректным и
// отсортированным после массы merge/borrow.
func TestLeafMerge_RangeAfterChurn(t *testing.T) {
	tree, _ := newTestTree()
	const n = 6000
	for i := range n {
		tree.Insert(0, float64(i), keyFor(i))
	}
	// Удаляем каждый второй.
	live := map[int]bool{}
	for i := range n {
		if i%2 == 0 {
			tree.Delete(float64(i))
		} else {
			live[i] = true
		}
	}

	res := tree.RangeSearch(0, float64(n))
	if len(res) != len(live) {
		t.Fatalf("RangeSearch вернул %d, живых %d", len(res), len(live))
	}
	prev := -1.0
	for _, r := range res {
		if r.Score < prev {
			t.Fatalf("RangeSearch не отсортирован: %v после %v", r.Score, prev)
		}
		prev = r.Score
		if !live[int(r.Score)] {
			t.Fatalf("range содержит удалённый/призрачный score %v", r.Score)
		}
		if r.Member != keyFor(int(r.Score)) {
			t.Fatalf("score %v: member %q, want %q", r.Score, r.Member, keyFor(int(r.Score)))
		}
	}
}
