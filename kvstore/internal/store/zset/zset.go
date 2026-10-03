package zset

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"

	"kvstore/kvstore/internal/btree"
)

// ============================================================================
// ZSetRegistry — реестр sorted sets
// ============================================================================
//
// Каждое множество — B+-дерево пар (score, member) плюс обратный индекс
// member → score в обычной map; оба под одним RWMutex множества:
//   ZADD/ZREM            — Lock (check-then-act атомарен: нет дублей в дереве)
//   ZSCORE/ZCARD/ZRANGE  — RLock (читатели одного множества не мешают друг другу)
//
// Прежде обратный индекс лежал в общем KV под ключами "__zidx:<set>:<member>",
// а узлы дерева — в арене самописного аллокатора. Отсюда три следствия, которых
// больше нет: DBSIZE считал служебные ключи, снапшот обязан был их вырезать,
// а память дерева нельзя было отдать сборщику мусора.
//
// Опустевшее множество снимается с реестра (как в Redis: ZREM последнего
// member удаляет ключ). Иначе каждое когда-либо созданное имя жило бы вечно.

// Оценки служебных байт для гейта maxmemory (учёт приближённый, как и у KV).
const (
	// memberOverhead — элемент в листе дерева (24 Б) с запасом на
	// недозаполнение узлов, слот обратного индекса (заголовок строки и float64)
	// и служебные байты таблицы. Сама строка member общая у дерева и индекса и
	// считается один раз.
	memberOverhead = 80
	// setOverhead — запись реестра, пустые дерево и map.
	setOverhead = 256
)

// zsetEntry — одно множество.
type zsetEntry struct {
	mu     sync.RWMutex
	tree   *btree.BPTree
	scores map[string]float64
	// dead — множество опустело и снято с реестра. Писатель, взявший запись
	// до снятия и дождавшийся лока, видит флаг и берёт свежую запись — иначе
	// его ZADD ушёл бы в запись, которой уже нет в реестре.
	dead bool
}

// ZSetRegistry — реестр sorted sets.
type ZSetRegistry struct {
	sets sync.Map // map[string]*zsetEntry
	mem  *atomic.Int64
}

// New создаёт реестр. mem — общий счётчик учтённой памяти (тот же, что у KV,
// чтобы ZADD упирался в тот же maxmemory); nil — без учёта.
func New(mem *atomic.Int64) *ZSetRegistry {
	return &ZSetRegistry{mem: mem}
}

func (r *ZSetRegistry) account(delta int64) {
	if r.mem != nil {
		r.mem.Add(delta)
	}
}

// EncodeZAddValue кодирует score + member для WAL Entry.Value.
// Формат: [8 bytes score LE float64][member string]
func EncodeZAddValue(score float64, member string) []byte {
	buf := make([]byte, 8+len(member))
	binary.LittleEndian.PutUint64(buf[:8], math.Float64bits(score))
	copy(buf[8:], member)
	return buf
}

// DecodeZAddValue декодирует score + member из WAL Entry.Value.
func DecodeZAddValue(data []byte) (float64, string) {
	score := math.Float64frombits(binary.LittleEndian.Uint64(data[:8]))
	member := string(data[8:])
	return score, member
}

// ── Public API ──────────────────────────────────────────────

func (r *ZSetRegistry) get(setName string) *zsetEntry {
	v, ok := r.sets.Load(setName)
	if !ok {
		return nil
	}
	return v.(*zsetEntry)
}

func (r *ZSetRegistry) getOrCreate(setName string) *zsetEntry {
	if e := r.get(setName); e != nil {
		return e
	}
	e := &zsetEntry{tree: btree.New(), scores: make(map[string]float64)}
	actual, loaded := r.sets.LoadOrStore(setName, e)
	if !loaded {
		r.account(setOverhead + int64(len(setName)))
	}
	return actual.(*zsetEntry)
}

// ZAdd добавляет member или меняет его score. true — member новый.
// NaN не принимается (false, ничего не меняется): Redis отвечает на него
// ошибкой, а в упорядоченном индексе NaN ломает сравнение. Обработчик ZADD
// отсекает NaN раньше; здесь — защита реплея журнала, записанного версией,
// которая NaN пропускала.
func (r *ZSetRegistry) ZAdd(setName string, score float64, member string) bool {
	if math.IsNaN(score) {
		return false
	}
	for {
		e := r.getOrCreate(setName)
		e.mu.Lock()
		if e.dead {
			e.mu.Unlock()
			continue
		}
		old, existed := e.scores[member]
		if existed && old == score {
			e.mu.Unlock()
			return false
		}
		if existed {
			e.tree.Delete(old, member)
		}
		e.tree.Insert(score, member)
		e.scores[member] = score
		e.mu.Unlock()
		if !existed {
			r.account(memberOverhead + int64(len(member)))
		}
		return !existed
	}
}

// ZScore возвращает score member.
func (r *ZSetRegistry) ZScore(setName, member string) (float64, bool) {
	e := r.get(setName)
	if e == nil {
		return 0, false
	}
	e.mu.RLock()
	score, ok := e.scores[member]
	e.mu.RUnlock()
	return score, ok
}

// ZRem удаляет member. true — member был.
func (r *ZSetRegistry) ZRem(setName, member string) bool {
	e := r.get(setName)
	if e == nil {
		return false
	}
	e.mu.Lock()
	score, ok := e.scores[member]
	if !ok {
		e.mu.Unlock()
		return false
	}
	delete(e.scores, member)
	e.tree.Delete(score, member)
	emptied := len(e.scores) == 0
	if emptied {
		e.dead = true
		r.sets.CompareAndDelete(setName, e)
	}
	e.mu.Unlock()
	r.account(-(memberOverhead + int64(len(member))))
	if emptied {
		r.account(-(setOverhead + int64(len(setName))))
	}
	return true
}

// RangeResult — результат диапазонного поиска.
type RangeResult struct {
	Score  float64
	Member string
}

// ZRangeByScore возвращает member с minScore ≤ score ≤ maxScore по порядку
// (score, затем member).
func (r *ZSetRegistry) ZRangeByScore(setName string, minScore, maxScore float64) []RangeResult {
	e := r.get(setName)
	if e == nil {
		return nil
	}
	var out []RangeResult
	e.mu.RLock()
	e.tree.Range(minScore, maxScore, func(it btree.Item) bool {
		out = append(out, RangeResult{Score: it.Score, Member: it.Member})
		return true
	})
	e.mu.RUnlock()
	return out
}

// ZCard возвращает число member в множестве.
func (r *ZSetRegistry) ZCard(setName string) int {
	e := r.get(setName)
	if e == nil {
		return 0
	}
	e.mu.RLock()
	n := len(e.scores)
	e.mu.RUnlock()
	return n
}

// MembersInRange возвращает множество member со score в [minScore, maxScore]
// (nil — пусто). Нужен VSIM.SEARCHRANGE как фильтр кандидатов HNSW. Строки
// member не копируются: map держит те же строки, что и дерево. Прежде здесь
// было множество FNV-хешей member — ради того, чтобы не копировать строки из
// арены, ценой ложных совпадений при коллизии хеша.
func (r *ZSetRegistry) MembersInRange(setName string, minScore, maxScore float64) map[string]struct{} {
	e := r.get(setName)
	if e == nil {
		return nil
	}
	var out map[string]struct{}
	e.mu.RLock()
	e.tree.Range(minScore, maxScore, func(it btree.Item) bool {
		if out == nil {
			out = make(map[string]struct{})
		}
		out[it.Member] = struct{}{}
		return true
	})
	e.mu.RUnlock()
	return out
}

// ForEach обходит множество по порядку — для снапшота. Элементы копируются под
// RLock, а fn вызывается уже без лока: запись снапшота на диск не держит
// писателей этого множества.
func (r *ZSetRegistry) ForEach(setName string, fn func(score float64, member string)) {
	e := r.get(setName)
	if e == nil {
		return
	}
	e.mu.RLock()
	items := make([]btree.Item, 0, e.tree.Len())
	e.tree.ForEach(func(it btree.Item) bool {
		items = append(items, it)
		return true
	})
	e.mu.RUnlock()
	for _, it := range items {
		fn(it.Score, it.Member)
	}
}

// ForEachSet обходит имена всех множеств — для снапшота.
func (r *ZSetRegistry) ForEachSet(fn func(setName string)) {
	r.sets.Range(func(key, _ any) bool {
		fn(key.(string))
		return true
	})
}
