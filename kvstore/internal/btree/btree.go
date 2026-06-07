package btree

import (
	"sync"
	"unsafe"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// ============================================================================
// B+Tree на базе TCMalloc — zero-alloc, GC-free
// ============================================================================
//
// Все узлы дерева живут в TCMalloc (тот же аллокатор что для KV-данных).
// Ни одного *node указателя. Ни одного string. GC не видит структуру дерева.
//
// node хранит:
//   - score (float64)   — ключ сортировки
//   - member (Handle)   — ссылка на строку-member в TCMalloc
//   - children (Handle) — ссылки на дочерние узлы (тоже в TCMalloc)
//
// Доступ к узлу: store.Resolve(handle) → []byte → (*node)(unsafe.Pointer)
// Стоимость: ~5ns (lock-free Resolve).

// order — максимум ключей в узле.
// 32 ключа → высота дерева для 1М записей ≈ 4 (log32(1_000_000)).
const order = 32

// nilHandle — пустой Handle (аналог nil).
// В TCMalloc spanID=0 зарезервирован, поэтому Handle(0) невалиден.
const nilHandle tcmalloc.Handle = 0

// item — одна запись в листе.
//
// Только числа, никаких Go-указателей:
//   - score:  ключ сортировки (float64)
//   - member: Handle на []byte с именем member (в TCMalloc)
//
// GC не сканирует эту структуру.
type item struct {
	score  float64
	member tcmalloc.Handle // → store.Resolve(member) даёт []byte с именем
}

// node — узел B+Tree. Целиком живёт в TCMalloc Span.
//
// Нет ни одного Go-указателя (*node, string, []byte) — GC игнорирует.
// Доступ через unsafe.Pointer cast из []byte, полученного через Resolve.
//
// sizeof(node) ≈ 816 байт → попадает в size class 5 (1024B).
type node struct {
	items    [order + 1]item            // +1 = overflow-слот для split
	children [order + 2]tcmalloc.Handle // Handle на дочерние узлы
	next     tcmalloc.Handle            // → следующий лист (цепочка)
	count    int32                      // текущее кол-во ключей
	leaf     bool                       // лист или внутренний?
	_        [3]byte                    // padding для выравнивания
}

// nodeSize — размер узла в байтах (вычисляется один раз при старте).
var nodeSize = int(unsafe.Sizeof(node{}))

// BPTree — B+дерево, все узлы в TCMalloc.
type BPTree struct {
	mu       sync.RWMutex
	store    *tcmalloc.TCMallocStore
	workerID int
	root     tcmalloc.Handle
	len      int
}

// New создаёт пустое B+дерево.
//
// store — TCMallocStore (тот же что для KV-данных).
// workerID — ID epoll-воркера для lock-free аллокации через MCache.
func New(store *tcmalloc.TCMallocStore, workerID int) *BPTree {
	rootH := allocNode(store, workerID)
	root := resolveNode(store, rootH)
	root.leaf = true
	return &BPTree{
		store:    store,
		workerID: workerID,
		root:     rootH,
	}
}

// ── Аллокация узлов через TCMalloc ─────────────────────────

// allocNode выделяет узел в TCMalloc и возвращает Handle.
func allocNode(store *tcmalloc.TCMallocStore, workerID int) tcmalloc.Handle {
	_, h := store.Alloc(workerID, nodeSize)
	return h
}

// resolveNode — Handle → *node (zero-copy через unsafe.Pointer).
//
// Resolve возвращает []byte из Span. unsafe.Pointer приводит к *node.
// Безопасно потому что node содержит ТОЛЬКО числа (float64, uint64, int32, bool).
// Никаких Go-указателей → GC не нужно сканировать.
func resolveNode(store *tcmalloc.TCMallocStore, h tcmalloc.Handle) *node {
	buf := store.Resolve(h)
	return (*node)(unsafe.Pointer(&buf[0]))
}

// freeNode освобождает узел обратно в TCMalloc.
func freeNode(store *tcmalloc.TCMallocStore, workerID int, h tcmalloc.Handle) {
	store.Free(workerID, h)
}

// ── Хранение member-строк в TCMalloc ───────────────────────

// allocMember сохраняет строку member в TCMalloc и возвращает Handle.
//
// Формат: [4 байта длина (LE)][данные строки]
// Зачем? Alloc(5) для "fifty" вернёт 32-байтный буфер (size class 0).
// Resolve вернёт все 32 байта. Без длины мы не знаем где строка кончается.
func allocMember(store *tcmalloc.TCMallocStore, workerID int, member string) tcmalloc.Handle {
	size := 4 + len(member) // 4 байта на длину + данные
	buf, h := store.Alloc(workerID, size)
	// Записываем длину (little-endian)
	buf[0] = byte(len(member))
	buf[1] = byte(len(member) >> 8)
	buf[2] = byte(len(member) >> 16)
	buf[3] = byte(len(member) >> 24)
	copy(buf[4:], member)
	return h
}

// resolveMember читает строку member по Handle из TCMalloc.
func resolveMember(store *tcmalloc.TCMallocStore, h tcmalloc.Handle) string {
	buf := store.Resolve(h)
	// Читаем длину (little-endian)
	length := int(buf[0]) | int(buf[1])<<8 | int(buf[2])<<16 | int(buf[3])<<24
	return string(buf[4 : 4+length])
}

// freeMember освобождает память member-строки.
func freeMember(store *tcmalloc.TCMallocStore, workerID int, h tcmalloc.Handle) {
	store.Free(workerID, h)
}

// ── Public API ──────────────────────────────────────────────

// Len — количество элементов.
func (t *BPTree) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.len
}

// Insert вставляет (score, member). Если score есть — обновляет member.
func (t *BPTree) Insert(score float64, member string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	memberH := allocMember(t.store, t.workerID, member)
	it := item{score: score, member: memberH}

	splitKey, splitH := t.insertRec(t.root, it)

	if splitH != nilHandle {
		newRootH := allocNode(t.store, t.workerID)
		newRoot := resolveNode(t.store, newRootH)
		newRoot.items[0] = item{score: splitKey}
		newRoot.children[0] = t.root
		newRoot.children[1] = splitH
		newRoot.count = 1
		newRoot.leaf = false
		t.root = newRootH
	}
}

// Search ищет member по score.
func (t *BPTree) Search(score float64) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	h := t.findLeaf(score)
	nd := resolveNode(t.store, h)
	idx, found := nd.keyIndex(score)
	if found {
		return resolveMember(t.store, nd.items[idx].member), true
	}
	return "", false
}

// Delete удаляет элемент по score. Возвращает true если существовал.
func (t *BPTree) Delete(score float64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	h := t.findLeaf(score)
	nd := resolveNode(t.store, h)
	idx, found := nd.keyIndex(score)
	if !found {
		return false
	}

	// Освобождаем память member-строки
	freeMember(t.store, t.workerID, nd.items[idx].member)

	// Сдвигаем влево
	for i := int(idx); i < int(nd.count)-1; i++ {
		nd.items[i] = nd.items[i+1]
	}
	nd.count--
	t.len--
	return true
}

// RangeSearch — все элементы где minScore ≤ score ≤ maxScore.
// Возвращает пары (score, member string).
func (t *BPTree) RangeSearch(minScore, maxScore float64) []struct {
	Score  float64
	Member string
} {
	t.mu.RLock()
	defer t.mu.RUnlock()

	h := t.findLeaf(minScore)
	nd := resolveNode(t.store, h)
	idx, _ := nd.keyIndex(minScore)

	var results []struct {
		Score  float64
		Member string
	}

	for h != nilHandle {
		nd = resolveNode(t.store, h)
		for i := idx; i < int(nd.count); i++ {
			if nd.items[i].score > maxScore {
				return results
			}
			results = append(results, struct {
				Score  float64
				Member string
			}{
				Score:  nd.items[i].score,
				Member: resolveMember(t.store, nd.items[i].member),
			})
		}
		h = nd.next
		idx = 0
	}
	return results
}

// ForEach вызывает fn для каждого элемента по возрастанию score.
func (t *BPTree) ForEach(fn func(score float64, member string)) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Спуск до самого левого листа
	h := t.root
	nd := resolveNode(t.store, h)
	for !nd.leaf {
		h = nd.children[0]
		nd = resolveNode(t.store, h)
	}
	// Прогулка по цепочке
	for h != nilHandle {
		nd = resolveNode(t.store, h)
		for i := int32(0); i < nd.count; i++ {
			fn(nd.items[i].score, resolveMember(t.store, nd.items[i].member))
		}
		h = nd.next
	}
}

// Min — элемент с минимальным score.
func (t *BPTree) Min() (float64, string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.len == 0 {
		return 0, "", false
	}
	h := t.root
	nd := resolveNode(t.store, h)
	for !nd.leaf {
		h = nd.children[0]
		nd = resolveNode(t.store, h)
	}
	return nd.items[0].score, resolveMember(t.store, nd.items[0].member), true
}

// Max — элемент с максимальным score.
func (t *BPTree) Max() (float64, string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.len == 0 {
		return 0, "", false
	}
	h := t.root
	nd := resolveNode(t.store, h)
	for !nd.leaf {
		h = nd.children[int(nd.count)]
		nd = resolveNode(t.store, h)
	}
	return nd.items[nd.count-1].score,
		resolveMember(t.store, nd.items[nd.count-1].member), true
}

// ============================================================================
// Internal — навигация и split
// ============================================================================

func (nd *node) childIndex(score float64) int {
	lo, hi := int32(0), nd.count
	for lo < hi {
		mid := (lo + hi) / 2
		if nd.items[mid].score <= score {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return int(lo)
}

func (nd *node) keyIndex(score float64) (int, bool) {
	lo, hi := int32(0), nd.count
	for lo < hi {
		mid := (lo + hi) / 2
		if nd.items[mid].score < score {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < nd.count && nd.items[lo].score == score {
		return int(lo), true
	}
	return int(lo), false
}

func (t *BPTree) findLeaf(score float64) tcmalloc.Handle {
	h := t.root
	nd := resolveNode(t.store, h)
	for !nd.leaf {
		i := nd.childIndex(score)
		h = nd.children[i]
		nd = resolveNode(t.store, h)
	}
	return h
}

func (t *BPTree) insertRec(h tcmalloc.Handle, it item) (float64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	if nd.leaf {
		return t.insertLeaf(h, it)
	}

	i := nd.childIndex(it.score)
	childH := nd.children[i]
	splitKey, splitH := t.insertRec(childH, it)

	if splitH == nilHandle {
		return 0, nilHandle
	}

	return t.insertInternal(h, splitKey, splitH)
}

func (t *BPTree) insertLeaf(h tcmalloc.Handle, it item) (float64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	idx, found := nd.keyIndex(it.score)

	if found {
		// Ключ есть — освобождаем старый member, обновляем
		freeMember(t.store, t.workerID, nd.items[idx].member)
		nd.items[idx].member = it.member
		return 0, nilHandle
	}

	// Сдвигаем вправо
	for i := int(nd.count); i > idx; i-- {
		nd.items[i] = nd.items[i-1]
	}
	nd.items[idx] = it
	nd.count++
	t.len++

	if nd.count <= order {
		return 0, nilHandle
	}

	return t.splitLeaf(h)
}

func (t *BPTree) splitLeaf(h tcmalloc.Handle) (float64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	mid := nd.count / 2

	rightH := allocNode(t.store, t.workerID)
	// После Alloc nd может протухнуть — берём заново
	nd = resolveNode(t.store, h)
	right := resolveNode(t.store, rightH)

	// Копируем правую половину
	rightCount := nd.count - mid
	for i := int32(0); i < rightCount; i++ {
		right.items[i] = nd.items[mid+i]
	}
	right.count = rightCount
	right.leaf = true
	nd.count = mid

	// Цепочка: left → right → old_next
	right.next = nd.next
	nd.next = rightH

	return right.items[0].score, rightH
}

func (t *BPTree) insertInternal(h tcmalloc.Handle, score float64, childH tcmalloc.Handle) (float64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	idx, _ := nd.keyIndex(score)

	// Сдвигаем вправо
	for i := int(nd.count); i > idx; i-- {
		nd.items[i] = nd.items[i-1]
	}
	for i := int(nd.count) + 1; i > idx+1; i-- {
		nd.children[i] = nd.children[i-1]
	}

	nd.items[idx] = item{score: score}
	nd.children[idx+1] = childH
	nd.count++

	if nd.count <= order {
		return 0, nilHandle
	}

	return t.splitInternal(h)
}

func (t *BPTree) splitInternal(h tcmalloc.Handle) (float64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	mid := nd.count / 2
	upKey := nd.items[mid].score

	rightH := allocNode(t.store, t.workerID)
	// После Alloc nd может протухнуть — берём заново
	nd = resolveNode(t.store, h)
	right := resolveNode(t.store, rightH)

	rightCount := nd.count - mid - 1
	for i := int32(0); i < rightCount; i++ {
		right.items[i] = nd.items[mid+1+i]
	}
	right.count = rightCount
	right.leaf = false

	for i := int32(0); i <= rightCount; i++ {
		right.children[i] = nd.children[mid+1+i]
	}

	nd.count = mid

	return upKey, rightH
}
