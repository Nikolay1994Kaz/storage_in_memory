package btree

import (
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// ============================================================================
// B+Tree на базе TCMalloc — zero-alloc, GC-free, lock-free Search
// ============================================================================
//
// Все узлы дерева живут в TCMalloc (тот же аллокатор что для KV-данных).
// Ни одного *node указателя. Ни одного string. GC не видит структуру дерева.
//
// Модель конкурентности (аналог TCMalloc Store):
//
//	Search   — ★ LOCK-FREE ★ через seqlock (как Get в TCMalloc store)
//	Len      — ★ LOCK-FREE ★ через atomic.Int64
//	Insert   — mu.Lock (exclusive между writers)
//	Delete   — mu.Lock (exclusive между writers)
//	ForEach  — mu.RLock (shared, блокируется writers)
//	Range    — mu.RLock (shared, блокируется writers)
//	Min/Max  — mu.RLock (shared, блокируется writers)
//
// Seqlock (per-node version counter):
//   - Writer: version++ (нечётный = "пишу") → modify → version++ (чётный = "готово")
//   - Reader: v1 = load → read → v2 = load → if v1 != v2 || v1&1 → retry
//
// DeferFree (QSBR reclamation):
//   - Delete/Insert(duplicate) вызывают store.DeferFree(memberH) вместо Free
//   - Это гарантирует что in-flight lock-free Search не прочитает freed memory
//   - Тот же механизм что в TCMalloc store Del: слот освобождается не по
//     таймеру, а по кворуму quiescent-состояний воркеров (см. tcmalloc/reclaim.go)
//
// Composite keys (score, memberHash):
//   - Sorted sets допускают дублирующиеся score (два товара по $9.99).
//   - Для уникальности используем (score, FNV-1a hash of member) как составной ключ.
//   - Коллизия хешей: ~1 на 2^64 — пренебрежимо мала.

// order — максимум ключей в узле.
// 32 ключа → высота дерева для 1М записей ≈ 4 (log32(1_000_000)).
const order = 32

// nilHandle — пустой Handle (аналог nil).
// В TCMalloc spanID=0 зарезервирован, поэтому Handle(0) невалиден.
const nilHandle tcmalloc.Handle = 0

// item — одна запись в листе.
//
// Составной ключ (score, memberHash) для поддержки дублирующихся score:
//   - score:      ключ сортировки (float64)
//   - memberHash: FNV-1a хеш member-строки (uint64) — tiebreaker при equal scores
//   - member:     Handle на []byte с именем member (в TCMalloc)
//
// GC не сканирует эту структуру — только числа.
type item struct {
	score      float64
	memberHash uint64          // FNV-1a hash для сортировки при одинаковых score
	member     tcmalloc.Handle // → store.Resolve(member) даёт []byte с именем
}

// node — узел B+Tree. Целиком живёт в TCMalloc Span.
//
// Нет ни одного Go-указателя (*node, string, []byte) — GC игнорирует.
// Доступ через unsafe.Pointer cast из []byte, полученного через Resolve.
//
// Поле version — seqlock counter:
//   - Чётное значение = узел стабилен, можно читать lock-free
//   - Нечётное значение = writer модифицирует узел, reader должен retry
type node struct {
	items    [order + 1]item            // +1 = overflow-слот для split
	children [order + 2]tcmalloc.Handle // Handle на дочерние узлы
	next     tcmalloc.Handle            // → следующий лист (цепочка)
	count    int32                      // текущее кол-во ключей
	version  uint32                     // seqlock: чёт=стабильный, нечёт=пишут
	leaf     bool                       // лист или внутренний?
	_        [3]byte                    // padding для выравнивания
}

// nodeSize — размер узла в байтах (вычисляется один раз при старте).
var nodeSize = int(unsafe.Sizeof(node{}))

// BPTree — B+дерево, все узлы в TCMalloc.
//
// root и len — atomic для lock-free Search и Len.
// mu — RWMutex: writers берут Lock, ForEach/Range/Min/Max берут RLock.
// Search НЕ берёт никаких мьютексов (seqlock).
type BPTree struct {
	mu    sync.RWMutex
	store *tcmalloc.TCMallocStore
	root  atomic.Uint64 // Handle корня (atomic для lock-free Search)
	len   atomic.Int64  // количество элементов (atomic для lock-free Len)
}

// New создаёт пустое B+дерево.
//
// store — TCMallocStore (тот же что для KV-данных).
// workerID — ID epoll-воркера ВЫЗЫВАЮЩЕГО (для аллокации корня из его MCache).
//
// ВАЖНО: workerID НЕ сохраняется в дереве. Каждая последующая мутация (Insert)
// аллоцирует из кэша СВОЕГО вызывающего воркера — иначе все деревья писали бы в
// один caches[0] из произвольных epoll-горутин (data race: MCache — single-writer).
func New(store *tcmalloc.TCMallocStore, workerID int) *BPTree {
	rootH := allocNode(store, workerID)
	root := resolveNode(store, rootH)
	root.leaf = true
	t := &BPTree{
		store: store,
	}
	t.root.Store(uint64(rootH))
	return t
}

// loadRoot атомарно читает Handle корня (для lock-free Search).
func (t *BPTree) loadRoot() tcmalloc.Handle {
	return tcmalloc.Handle(t.root.Load())
}

// storeRoot атомарно записывает Handle корня.
// Вызывается из Insert под mu.Lock.
func (t *BPTree) storeRoot(h tcmalloc.Handle) {
	t.root.Store(uint64(h))
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

// ── Hash function ──────────────────────────────────────────

// hashMember — FNV-1a hash, fully inlineable, zero-alloc.
// Используется как tiebreaker при одинаковых score в sorted sets.
func hashMember(member string) uint64 {
	h := uint64(14695981039346656037) // FNV offset basis
	for i := 0; i < len(member); i++ {
		h ^= uint64(member[i])
		h *= 1099511628211 // FNV prime
	}
	return h
}

// ── Public API ──────────────────────────────────────────────

// Len — количество элементов. ★ LOCK-FREE ★ через atomic.
func (t *BPTree) Len() int {
	return int(t.len.Load())
}

// Search ищет member по score.
//
// ★ LOCK-FREE ★ — не берёт никаких mutex/RWMutex.
// Использует seqlock (per-node version counter) для консистентного чтения.
// Аналог lock-free Get в TCMalloc Store.
//
// Возвращает ЛЮБОЙ member с данным score (первый найденный в листе).
// Для точного поиска (member → score) используйте KV обратный индекс.
func (t *BPTree) Search(score float64) (string, bool) {
	for {
		// Оптимистичный спуск от root к leaf (seqlock на каждом уровне).
		// Score-only навигация гарантирует попадание в правильный лист.
		h, ok := t.findLeafOptimisticByScore(score)
		if !ok {
			runtime.Gosched()
			continue
		}

		nd := resolveNode(t.store, h)

		// Seqlock: читаем версию ПЕРЕД чтением данных
		v1 := atomic.LoadUint32(&nd.version)
		if v1&1 != 0 {
			// Writer работает прямо сейчас → retry
			runtime.Gosched()
			continue
		}

		// Score-only binary search (ищем первый item с score >= target)
		lo, hi := int32(0), nd.count
		for lo < hi {
			mid := (lo + hi) / 2
			if nd.items[mid].score < score {
				lo = mid + 1
			} else {
				hi = mid
			}
		}

		var memberH tcmalloc.Handle
		found := lo < nd.count && nd.items[lo].score == score
		if found {
			memberH = nd.items[lo].member
		}

		// Seqlock: проверяем версию ПОСЛЕ чтения
		if atomic.LoadUint32(&nd.version) != v1 {
			// Writer вмешался между v1 и v2 → данные могут быть inconsistent → retry
			continue
		}

		// Версия не изменилась → данные консистентны.
		// memberH защищён DeferFree: не освобождается, пока читатель (этот
		// воркер) не пройдёт quiescent-состояние (см. tcmalloc/reclaim.go).
		if found {
			return resolveMember(t.store, memberH), true
		}
		return "", false
	}
}

// Insert вставляет (score, member), аллоцируя из кэша ВЫЗЫВАЮЩЕГО воркера
// (workerID). Если (score, member) уже есть — обновляет member (no-op для
// sorted sets). Если score есть с другим member — добавляет отдельным элементом.
//
// Единственный аллоцирующий путь дерева (Delete/DeleteMember только DeferFree,
// без Alloc), поэтому только он получает workerID.
func (t *BPTree) Insert(workerID int, score float64, member string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	mhash := hashMember(member)
	memberH := allocMember(t.store, workerID, member)
	it := item{score: score, memberHash: mhash, member: memberH}

	splitKey, splitMHash, splitH := t.insertRec(workerID, t.loadRoot(), it)

	if splitH != nilHandle {
		newRootH := allocNode(t.store, workerID)
		newRoot := resolveNode(t.store, newRootH)
		newRoot.items[0] = item{score: splitKey, memberHash: splitMHash}
		newRoot.children[0] = t.loadRoot()
		newRoot.children[1] = splitH
		newRoot.count = 1
		newRoot.leaf = false
		// Atomic store — lock-free Search увидит новый root
		t.storeRoot(newRootH)
	}
}

// minKeys — минимум ключей в НЕ-корневом узле. При count < minKeys узел
// underflow'ит: восстанавливаем баланс borrow (занять у соседа) или merge
// (слить с соседом, освободив узел). Гарантирует утилизацию ≥50% и, главное,
// возврат узлов в аллокатор — без этого freeNode не звался нигде и дерево
// держало high-water-mark узлов навсегда (space leak на churn/ZREM).
const minKeys = order / 2

// Delete удаляет первый элемент с данным score. Возвращает true если существовал.
//
// Member и опустевшие узлы освобождаются через DeferFree (QSBR), а НЕ через Free:
// in-flight lock-free Search может держать указатель в арену. Освобождённый узел
// НЕ затирается — устаревший читатель видит валидный снимок. Тот же механизм что
// в TCMalloc store Del.
func (t *BPTree) Delete(score float64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.deleteRec(t.loadRoot(), score, 0, true) {
		return false
	}
	t.len.Add(-1)
	t.collapseRoot()
	return true
}

// DeleteMember удаляет конкретный элемент по (score, member). Для sorted sets: ZREM.
//
// В отличие от Delete(score), находит точный элемент по composite key (score, memberHash).
// Это позволяет корректно удалять из sorted set с дублирующимися score.
func (t *BPTree) DeleteMember(score float64, member string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	mhash := hashMember(member)
	if !t.deleteRec(t.loadRoot(), score, mhash, false) {
		return false
	}
	t.len.Add(-1)
	t.collapseRoot()
	return true
}

// deleteRec спускается к листу с целевым ключом, удаляет его и на обратном пути
// ребалансирует underflow'нувших детей. Возвращает found. Все мутации под
// mu.Lock (writers сериализованы), каждая обёрнута seqlock-версией для lock-free
// Search. scoreOnly=true → навигация и удаление по score (первый с этим score);
// иначе — по composite (score, memberHash).
func (t *BPTree) deleteRec(h tcmalloc.Handle, score float64, mhash uint64, scoreOnly bool) bool {
	nd := resolveNode(t.store, h)
	if nd.leaf {
		return t.removeFromLeaf(h, score, mhash, scoreOnly)
	}

	var ci int
	if scoreOnly {
		ci = nd.childIndexByScore(score)
	} else {
		ci = nd.childIndex(score, mhash)
	}
	childH := nd.children[ci]
	if !t.deleteRec(childH, score, mhash, scoreOnly) {
		return false
	}
	if resolveNode(t.store, childH).count < minKeys {
		t.rebalanceChild(h, ci)
	}
	return true
}

// removeFromLeaf удаляет item из листа (seqlock + DeferFree member).
func (t *BPTree) removeFromLeaf(h tcmalloc.Handle, score float64, mhash uint64, scoreOnly bool) bool {
	nd := resolveNode(t.store, h)

	var idx int
	if scoreOnly {
		lo, hi := int32(0), nd.count
		for lo < hi {
			mid := (lo + hi) / 2
			if nd.items[mid].score < score {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if lo >= nd.count || nd.items[lo].score != score {
			return false
		}
		idx = int(lo)
	} else {
		i, found := nd.keyIndex(score, mhash)
		if !found {
			return false
		}
		idx = i
	}

	memberH := nd.items[idx].member

	atomic.AddUint32(&nd.version, 1) // odd → "пишу"
	for i := idx; i < int(nd.count)-1; i++ {
		nd.items[i] = nd.items[i+1]
	}
	nd.count--
	atomic.AddUint32(&nd.version, 1) // even → "готово"

	t.store.DeferFree(memberH)
	return true
}

// collapseRoot схлопывает корень, оставшийся с 0 ключей после merge его детей:
// internal-корень с count=0 имеет ровно 1 ребёнка — делаем его новым корнем.
// Атомарная подмена root (Search увидит старый ИЛИ новый; старый через DeferFree
// живёт до quiescence). Цикл — на случай нескольких уровней вырождения.
func (t *BPTree) collapseRoot() {
	for {
		rootH := t.loadRoot()
		root := resolveNode(t.store, rootH)
		if root.leaf || root.count > 0 {
			return
		}
		newRootH := root.children[0]
		t.storeRoot(newRootH)
		t.store.DeferFree(rootH)
	}
}

// rebalanceChild восстанавливает баланс underflow'нувшего children[ci]:
// borrow у соседа с запасом (> minKeys), иначе merge с соседом.
func (t *BPTree) rebalanceChild(parentH tcmalloc.Handle, ci int) {
	parent := resolveNode(t.store, parentH)

	if ci > 0 && resolveNode(t.store, parent.children[ci-1]).count > minKeys {
		t.borrowFromLeft(parentH, ci)
		return
	}
	if ci < int(parent.count) && resolveNode(t.store, parent.children[ci+1]).count > minKeys {
		t.borrowFromRight(parentH, ci)
		return
	}
	// Сосед впритык (== minKeys) — merge. Слияние двух узлов ≤ order (см. minKeys).
	if ci > 0 {
		t.mergeChildren(parentH, ci-1) // ci сливается В левого соседа
	} else {
		t.mergeChildren(parentH, ci) // правый сосед сливается В ci
	}
}

// mergeChildren сливает children[i] (left) и children[i+1] (right) в left,
// используя разделитель parent.items[i]. Порядок операций важен для lock-free
// Search: сперва достраиваем survivor (left), ПОТОМ убираем разделитель+ссылку
// из parent, и лишь затем DeferFree правого — чтобы читатель, ведомый ещё старым
// parent, всегда попадал в узел, где ключ ЕСТЬ (в left, уже дополненном, либо в
// right, ещё не освобождённом).
func (t *BPTree) mergeChildren(parentH tcmalloc.Handle, i int) {
	parent := resolveNode(t.store, parentH)
	leftH := parent.children[i]
	rightH := parent.children[i+1]
	left := resolveNode(t.store, leftH)
	right := resolveNode(t.store, rightH)

	// 1) Достраиваем left.
	atomic.AddUint32(&left.version, 1) // odd
	if left.leaf {
		base := left.count
		for j := int32(0); j < right.count; j++ {
			left.items[base+j] = right.items[j]
		}
		left.count += right.count
		left.next = right.next
	} else {
		// Разделитель спускается вниз ключом между поддеревьями.
		left.items[left.count] = item{score: parent.items[i].score, memberHash: parent.items[i].memberHash}
		base := left.count + 1
		for j := int32(0); j < right.count; j++ {
			left.items[base+j] = right.items[j]
		}
		for j := int32(0); j <= right.count; j++ {
			left.children[base+j] = right.children[j]
		}
		left.count += 1 + right.count
	}
	atomic.AddUint32(&left.version, 1) // even

	// 2) Убираем разделитель items[i] и ссылку children[i+1] из parent.
	atomic.AddUint32(&parent.version, 1) // odd
	for j := i; j < int(parent.count)-1; j++ {
		parent.items[j] = parent.items[j+1]
	}
	for j := i + 1; j < int(parent.count); j++ {
		parent.children[j] = parent.children[j+1]
	}
	parent.count--
	atomic.AddUint32(&parent.version, 1) // even

	// 3) Освобождаем поглощённый узел (DeferFree: устаревший Search мог зайти в него).
	t.store.DeferFree(rightH)
}

// borrowFromLeft переносит последний элемент левого соседа в начало
// underflow'нувшего children[ci], обновляя разделитель parent.items[ci-1].
// Порядок: дополняем child → правим разделитель → урезаем соседа (читатель,
// ведомый старым разделителем, находит ключ у соседа, пока тот не урезан).
func (t *BPTree) borrowFromLeft(parentH tcmalloc.Handle, ci int) {
	parent := resolveNode(t.store, parentH)
	child := resolveNode(t.store, parent.children[ci])
	ls := resolveNode(t.store, parent.children[ci-1])

	atomic.AddUint32(&child.version, 1) // odd
	if child.leaf {
		for i := child.count; i > 0; i-- {
			child.items[i] = child.items[i-1]
		}
		child.items[0] = ls.items[ls.count-1]
		child.count++
	} else {
		for i := child.count; i > 0; i-- {
			child.items[i] = child.items[i-1]
		}
		for i := child.count + 1; i > 0; i-- {
			child.children[i] = child.children[i-1]
		}
		// Разделитель спускается в child, последний ключ соседа поднимается вверх.
		child.items[0] = item{score: parent.items[ci-1].score, memberHash: parent.items[ci-1].memberHash}
		child.children[0] = ls.children[ls.count]
		child.count++
	}
	atomic.AddUint32(&child.version, 1) // even

	// Новый разделитель.
	var sep item
	if child.leaf {
		sep = child.items[0]
	} else {
		sep = ls.items[ls.count-1]
	}
	atomic.AddUint32(&parent.version, 1)
	parent.items[ci-1] = item{score: sep.score, memberHash: sep.memberHash}
	atomic.AddUint32(&parent.version, 1)

	atomic.AddUint32(&ls.version, 1)
	ls.count--
	atomic.AddUint32(&ls.version, 1)
}

// borrowFromRight переносит первый элемент правого соседа в конец
// underflow'нувшего children[ci], обновляя разделитель parent.items[ci].
func (t *BPTree) borrowFromRight(parentH tcmalloc.Handle, ci int) {
	parent := resolveNode(t.store, parentH)
	child := resolveNode(t.store, parent.children[ci])
	rs := resolveNode(t.store, parent.children[ci+1])

	atomic.AddUint32(&child.version, 1) // odd
	if child.leaf {
		child.items[child.count] = rs.items[0]
		child.count++
	} else {
		child.items[child.count] = item{score: parent.items[ci].score, memberHash: parent.items[ci].memberHash}
		child.children[child.count+1] = rs.children[0]
		child.count++
	}
	atomic.AddUint32(&child.version, 1) // even

	// Новый разделитель = ключ, поднимающийся из соседа.
	var sep item
	if child.leaf {
		sep = rs.items[1] // после снятия items[0] первым станет items[1]
	} else {
		sep = rs.items[0]
	}
	atomic.AddUint32(&parent.version, 1)
	parent.items[ci] = item{score: sep.score, memberHash: sep.memberHash}
	atomic.AddUint32(&parent.version, 1)

	// Урезаем соседа: сдвигаем влево items (и children для internal).
	atomic.AddUint32(&rs.version, 1) // odd
	for i := int32(0); i < rs.count-1; i++ {
		rs.items[i] = rs.items[i+1]
	}
	if !rs.leaf {
		for i := int32(0); i < rs.count; i++ {
			rs.children[i] = rs.children[i+1]
		}
	}
	rs.count--
	atomic.AddUint32(&rs.version, 1) // even
}

// RangeSearch — все элементы где minScore ≤ score ≤ maxScore.
// Возвращает пары (score, member string).
func (t *BPTree) RangeSearch(minScore, maxScore float64) []struct {
	Score  float64
	Member string
} {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Score-only навигация для range search
	h := t.findLeafByScore(minScore)
	nd := resolveNode(t.store, h)

	// Score-only binary search для позиции начала
	lo, hi := int32(0), nd.count
	for lo < hi {
		mid := (lo + hi) / 2
		if nd.items[mid].score < minScore {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	idx := int(lo)

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

// RangeCollectHashes — оптимизированный range search для фильтрации.
//
// Вместо копирования string'ов из TCMalloc (N allocs) возвращает
// множество memberHash'ей (1 alloc на map).
//
// Используется в VSIM.SEARCHRANGE: HNSW кандидат проверяется через
// hashMember(candidate) → set[hash] — без копирования строк.
//
// Сложность: O(log n + k) время, O(k) память (8 байт на элемент вместо ~30).
func (t *BPTree) RangeCollectHashes(minScore, maxScore float64) map[uint64]struct{} {
	t.mu.RLock()
	defer t.mu.RUnlock()

	h := t.findLeafByScore(minScore)
	nd := resolveNode(t.store, h)

	// Score-only binary search для позиции начала
	lo, hi := int32(0), nd.count
	for lo < hi {
		mid := (lo + hi) / 2
		if nd.items[mid].score < minScore {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	idx := int(lo)

	// Pre-allocate map с оценкой размера (order как начальный)
	result := make(map[uint64]struct{}, order)

	for h != nilHandle {
		nd = resolveNode(t.store, h)
		for i := idx; i < int(nd.count); i++ {
			if nd.items[i].score > maxScore {
				return result
			}
			result[nd.items[i].memberHash] = struct{}{}
		}
		h = nd.next
		idx = 0
	}
	return result
}

// HashMember — экспортированный FNV-1a hash для использования в фильтрах.
// VSIM.SEARCHRANGE: кандидат проверяется через HashMember(key) ∈ set.
func HashMember(member string) uint64 {
	return hashMember(member)
}

// RangeForEach — callback-style range iteration. Zero allocs на саму итерацию.
// fn возвращает false для прекращения итерации (early stop).
func (t *BPTree) RangeForEach(minScore, maxScore float64, fn func(score float64, memberHash uint64, memberH tcmalloc.Handle) bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	h := t.findLeafByScore(minScore)
	nd := resolveNode(t.store, h)

	lo, hi := int32(0), nd.count
	for lo < hi {
		mid := (lo + hi) / 2
		if nd.items[mid].score < minScore {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	idx := int(lo)

	for h != nilHandle {
		nd = resolveNode(t.store, h)
		for i := idx; i < int(nd.count); i++ {
			if nd.items[i].score > maxScore {
				return
			}
			if !fn(nd.items[i].score, nd.items[i].memberHash, nd.items[i].member) {
				return
			}
		}
		h = nd.next
		idx = 0
	}
}

// ForEach вызывает fn для каждого элемента по возрастанию score.
func (t *BPTree) ForEach(fn func(score float64, member string)) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Спуск до самого левого листа
	h := t.loadRoot()
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

	if t.len.Load() == 0 {
		return 0, "", false
	}
	h := t.loadRoot()
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

	if t.len.Load() == 0 {
		return 0, "", false
	}
	h := t.loadRoot()
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

// childIndex — composite binary search для internal nodes.
// Сравнивает по (score, memberHash): сначала score, при равенстве — memberHash.
func (nd *node) childIndex(score float64, mhash uint64) int {
	lo, hi := int32(0), nd.count
	for lo < hi {
		mid := (lo + hi) / 2
		ms := nd.items[mid].score
		mh := nd.items[mid].memberHash
		if ms < score || (ms == score && mh <= mhash) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return int(lo)
}

// childIndexByScore — score-only binary search для internal nodes.
// Используется для Search(score) и RangeSearch: нам нужен лист,
// в котором ТОЧНО содержатся все items с данным score.
// При равных score всегда идём ВПРАВО (lo = mid + 1), чтобы не пропустить
// ни одного элемента — это гарантирует что мы найдём самый правый лист
// для данного score. Элементы с меньшим memberHash окажутся в предыдущих листах,
// но в текущем или предыдущем точно будут.
//
// Для Search это безопасно: мы делаем score-only binary search в листе
// и если не находим — проверяем предыдущие через цепочку next нет возможности,
// поэтому идём влево: ≤ вместо <.
func (nd *node) childIndexByScore(score float64) int {
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

// keyIndex — composite binary search для leaf nodes.
// Ищет точное совпадение по (score, memberHash).
func (nd *node) keyIndex(score float64, mhash uint64) (int, bool) {
	lo, hi := int32(0), nd.count
	for lo < hi {
		mid := (lo + hi) / 2
		ms := nd.items[mid].score
		mh := nd.items[mid].memberHash
		if ms < score || (ms == score && mh < mhash) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < nd.count && nd.items[lo].score == score && nd.items[lo].memberHash == mhash {
		return int(lo), true
	}
	return int(lo), false
}

// findLeaf — спуск к листу под mu.Lock (для writers: Insert/Delete).
// Writers не конкурируют друг с другом → seqlock не нужен.
func (t *BPTree) findLeaf(score float64, mhash uint64) tcmalloc.Handle {
	h := t.loadRoot()
	nd := resolveNode(t.store, h)
	for !nd.leaf {
		i := nd.childIndex(score, mhash)
		h = nd.children[i]
		nd = resolveNode(t.store, h)
	}
	return h
}

// findLeafByScore — спуск к листу по score-only под mu.Lock.
// Для Delete(score) и RangeSearch — находит лист, содержащий первые элементы с данным score.
func (t *BPTree) findLeafByScore(score float64) tcmalloc.Handle {
	h := t.loadRoot()
	nd := resolveNode(t.store, h)
	for !nd.leaf {
		i := nd.childIndexByScore(score)
		h = nd.children[i]
		nd = resolveNode(t.store, h)
	}
	return h
}

// findLeafOptimistic — lock-free спуск к листу с seqlock-валидацией.
//
// На каждом уровне: читаем version → childIndex → children[i] → проверяем version.
// Если version изменился (writer вмешался) → возвращаем false → Search retry от root.
//
// Стоимость при отсутствии конфликтов: дополнительные 2 atomic.Load на уровень (~2ns).
func (t *BPTree) findLeafOptimistic(score float64, mhash uint64) (tcmalloc.Handle, bool) {
	h := t.loadRoot()
	nd := resolveNode(t.store, h)

	for !nd.leaf {
		v1 := atomic.LoadUint32(&nd.version)
		if v1&1 != 0 {
			return 0, false // writer работает → retry
		}

		i := nd.childIndex(score, mhash)
		childH := nd.children[i]

		if atomic.LoadUint32(&nd.version) != v1 {
			return 0, false // version changed → retry
		}

		h = childH
		nd = resolveNode(t.store, h)
	}

	return h, true
}

// findLeafOptimisticByScore — lock-free спуск по score-only.
// Для Search(score): гарантирует нахождение листа с элементами данного score.
func (t *BPTree) findLeafOptimisticByScore(score float64) (tcmalloc.Handle, bool) {
	h := t.loadRoot()
	nd := resolveNode(t.store, h)

	for !nd.leaf {
		v1 := atomic.LoadUint32(&nd.version)
		if v1&1 != 0 {
			return 0, false
		}

		i := nd.childIndexByScore(score)
		childH := nd.children[i]

		if atomic.LoadUint32(&nd.version) != v1 {
			return 0, false
		}

		h = childH
		nd = resolveNode(t.store, h)
	}

	return h, true
}

func (t *BPTree) insertRec(workerID int, h tcmalloc.Handle, it item) (float64, uint64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	if nd.leaf {
		return t.insertLeaf(workerID, h, it)
	}

	i := nd.childIndex(it.score, it.memberHash)
	childH := nd.children[i]
	splitKey, splitMHash, splitH := t.insertRec(workerID, childH, it)

	if splitH == nilHandle {
		return 0, 0, nilHandle
	}

	return t.insertInternal(workerID, h, splitKey, splitMHash, splitH)
}

func (t *BPTree) insertLeaf(workerID int, h tcmalloc.Handle, it item) (float64, uint64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	idx, found := nd.keyIndex(it.score, it.memberHash)

	if found {
		// Тот же (score, memberHash) → обновляем member
		oldMember := nd.items[idx].member

		// Seqlock: version odd → "пишу" (lock-free Search увидит и retry)
		atomic.AddUint32(&nd.version, 1)
		nd.items[idx].member = it.member
		atomic.AddUint32(&nd.version, 1)
		// Seqlock: version even → "готово"

		// Старый member → DeferFree (in-flight Search может его читать)
		t.store.DeferFree(oldMember)
		return 0, 0, nilHandle
	}

	// Seqlock: version odd → "пишу"
	atomic.AddUint32(&nd.version, 1)

	// Сдвигаем вправо
	for i := int(nd.count); i > idx; i-- {
		nd.items[i] = nd.items[i-1]
	}
	nd.items[idx] = it
	nd.count++

	// Seqlock: version even → "готово"
	atomic.AddUint32(&nd.version, 1)

	t.len.Add(1)

	if nd.count <= order {
		return 0, 0, nilHandle
	}

	return t.splitLeaf(workerID, h)
}

func (t *BPTree) splitLeaf(workerID int, h tcmalloc.Handle) (float64, uint64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	mid := nd.count / 2

	rightH := allocNode(t.store, workerID)
	nd = resolveNode(t.store, h)
	right := resolveNode(t.store, rightH)

	// Копируем правую половину в новый узел
	rightCount := nd.count - mid
	for i := int32(0); i < rightCount; i++ {
		right.items[i] = nd.items[mid+i]
	}
	right.count = rightCount
	right.leaf = true

	// Seqlock: version odd → модифицируем count и next
	atomic.AddUint32(&nd.version, 1)
	nd.count = mid
	right.next = nd.next
	nd.next = rightH
	atomic.AddUint32(&nd.version, 1)
	// Seqlock: version even → "готово"

	return right.items[0].score, right.items[0].memberHash, rightH
}

func (t *BPTree) insertInternal(workerID int, h tcmalloc.Handle, score float64, mhash uint64, childH tcmalloc.Handle) (float64, uint64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	idx, _ := nd.keyIndex(score, mhash)

	// Seqlock: version odd → "пишу"
	atomic.AddUint32(&nd.version, 1)

	// Сдвигаем вправо
	for i := int(nd.count); i > idx; i-- {
		nd.items[i] = nd.items[i-1]
	}
	for i := int(nd.count) + 1; i > idx+1; i-- {
		nd.children[i] = nd.children[i-1]
	}

	nd.items[idx] = item{score: score, memberHash: mhash}
	nd.children[idx+1] = childH
	nd.count++

	// Seqlock: version even → "готово"
	atomic.AddUint32(&nd.version, 1)

	if nd.count <= order {
		return 0, 0, nilHandle
	}

	return t.splitInternal(workerID, h)
}

func (t *BPTree) splitInternal(workerID int, h tcmalloc.Handle) (float64, uint64, tcmalloc.Handle) {
	nd := resolveNode(t.store, h)
	mid := nd.count / 2
	upScore := nd.items[mid].score
	upMHash := nd.items[mid].memberHash

	rightH := allocNode(t.store, workerID)
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

	// Seqlock: version odd → модифицируем count
	atomic.AddUint32(&nd.version, 1)
	nd.count = mid
	atomic.AddUint32(&nd.version, 1)
	// Seqlock: version even → "готово"

	return upScore, upMHash, rightH
}
