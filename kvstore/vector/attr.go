package vector

import "math"

// =============================================================================
// Колоночный слой атрибутов — uint/multi-attr (план valiant-spinning-kettle).
//
// Атрибуты едут с данными (AddWithAttrs) и хранятся в сегменте КОЛОНКАМИ,
// выровненными по frozen-индексу (codes[i]/vals[i] — атрибут вектора i в раскладке
// сегмента). Категориальные значения dict-кодируются в uint32 (рычаг #2: фильтр =
// O(1) индексация массива, без strconv/hash строки на каждый вектор). Числовые
// хранятся как float64 для range-фильтров (lo ≤ v ≤ hi).
//
// Dict — PER-SEGMENT (как internedKeys, [[vector-key-interning]]): коды назначаются
// при build, без глобального мьютекса на горячем Add. SearchFilter резолвит
// запрошенное значение в локальный код каждого сегмента; нет в dict → сегмент
// не даёт результатов по этому Eq (matchable=false).
//
// Тенант (партиционный атрибут cfg.PartitionAttr) — обычный категориальный атрибут;
// его код дополнительно служит ведущим ключом сортировки/каталога (Вещь 1).
// =============================================================================

// attrMissing — код категориального атрибута, отсутствующего у вектора. Не хранится
// в dict, поэтому никогда не равен резолву реального запрошенного значения.
const attrMissing = ^uint32(0)

// Attributes — атрибуты одного вектора на входе (AddWithAttrs) и при декоде (merge).
type Attributes struct {
	Cat map[string]string  // категориальные → dict-код uint32
	Num map[string]float64 // числовые → range-фильтры
}

// empty сообщает, что атрибутов нет вовсе (быстрый путь).
func (a Attributes) empty() bool { return len(a.Cat) == 0 && len(a.Num) == 0 }

// categoricalColumn — dict-коды атрибута, выровнены по frozen-индексу.
type categoricalColumn struct{ codes []uint32 }

// numericColumn — числовые значения атрибута, выровнены по frozen-индексу.
// NaN = атрибут отсутствует у вектора (range-проверка с NaN всегда ложна).
type numericColumn struct{ vals []float64 }

// attrDict — per-segment словарь категориального атрибута: значение ↔ код.
type attrDict struct {
	code map[string]uint32 // value → code
	vals []string          // vals[code] = value (обратный декод)
}

func newAttrDict() *attrDict { return &attrDict{code: make(map[string]uint32)} }

// intern возвращает код значения, назначая новый при первой встрече.
func (d *attrDict) intern(value string) uint32 {
	if c, ok := d.code[value]; ok {
		return c
	}
	c := uint32(len(d.vals))
	d.code[value] = c
	d.vals = append(d.vals, value)
	return c
}

// lookup резолвит значение в код без назначения. ok=false → значения нет в сегменте.
func (d *attrDict) lookup(value string) (uint32, bool) {
	c, ok := d.code[value]
	return c, ok
}

// segmentAttrs — колоночный слой одного сегмента.
type segmentAttrs struct {
	cat  map[string]*categoricalColumn
	num  map[string]*numericColumn
	dict map[string]*attrDict // per-attr словарь категориальных значений
}

// buildSegmentAttrs строит колонки по entries в ИХ ПОРЯДКЕ (после sortEntriesByTenant;
// frozen-индекс i = позиция entries[i]). Возвращает nil, если ни у одной записи нет
// атрибутов (тенант-режим без атрибут-слоя — zero overhead).
//
// presets — заранее построенные словари для отдельных атрибутов (партиционный attr:
// его коды уже использованы для сортировки и каталога, поэтому колонка обязана
// переиспользовать ТОТ ЖЕ dict, иначе коды каталога ≠ коды колонки).
func buildSegmentAttrs(entries []DeltaEntry, presets map[string]*attrDict) *segmentAttrs {
	n := len(entries)
	// Собрать множество имён атрибутов (категориальные / числовые могут различаться
	// между записями).
	catNames := make(map[string]struct{})
	numNames := make(map[string]struct{})
	for i := range entries {
		for name := range entries[i].Attrs.Cat {
			catNames[name] = struct{}{}
		}
		for name := range entries[i].Attrs.Num {
			numNames[name] = struct{}{}
		}
	}
	if len(catNames) == 0 && len(numNames) == 0 {
		return nil
	}

	sa := &segmentAttrs{
		cat:  make(map[string]*categoricalColumn, len(catNames)),
		num:  make(map[string]*numericColumn, len(numNames)),
		dict: make(map[string]*attrDict, len(catNames)),
	}
	for name := range catNames {
		codes := make([]uint32, n)
		dict := presets[name]
		if dict == nil {
			dict = newAttrDict()
		}
		for i := range entries {
			if v, ok := entries[i].Attrs.Cat[name]; ok {
				codes[i] = dict.intern(v)
			} else {
				codes[i] = attrMissing
			}
		}
		sa.cat[name] = &categoricalColumn{codes: codes}
		sa.dict[name] = dict
	}
	for name := range numNames {
		vals := make([]float64, n)
		for i := range entries {
			if v, ok := entries[i].Attrs.Num[name]; ok {
				vals[i] = v
			} else {
				vals[i] = math.NaN()
			}
		}
		sa.num[name] = &numericColumn{vals: vals}
	}
	return sa
}

// decodeAt восстанавливает Attributes вектора i из колонок (для реконструкции entries
// при merge — атрибуты обязаны пережить мердж).
func (sa *segmentAttrs) decodeAt(i int) Attributes {
	if sa == nil {
		return Attributes{}
	}
	var a Attributes
	for name, col := range sa.cat {
		c := col.codes[i]
		if c == attrMissing {
			continue
		}
		if a.Cat == nil {
			a.Cat = make(map[string]string, len(sa.cat))
		}
		a.Cat[name] = sa.dict[name].vals[c]
	}
	for name, col := range sa.num {
		v := col.vals[i]
		if math.IsNaN(v) {
			continue
		}
		if a.Num == nil {
			a.Num = make(map[string]float64, len(sa.num))
		}
		a.Num[name] = v
	}
	return a
}

// Filter — спецификация мульти-атрибутного фильтра поиска.
type Filter struct {
	Eq    map[string]string     // категориальное равенство: attr = value
	Range map[string][2]float64 // числовой инклюзивный диапазон: lo ≤ attr ≤ hi
}

// empty — фильтр без условий.
func (f Filter) empty() bool { return len(f.Eq) == 0 && len(f.Range) == 0 }

// compile превращает фильтр в предикат по frozen-индексу (O(1) на вектор, без строк).
// matchable=false → в этом сегменте фильтр заведомо никого не пропустит (нет колонки
// или значение не встречалось) → сегмент можно пропустить целиком.
//
// skipAttr (если не "") исключается из предиката: это партиционный атрибут, по которому
// уже выбран блок тенанта — внутри блока он у всех одинаков, проверять избыточно.
func (sa *segmentAttrs) compile(f Filter, skipAttr string) (predIdx func(int) bool, matchable bool) {
	type catCheck struct {
		codes []uint32
		code  uint32
	}
	type numCheck struct {
		vals   []float64
		lo, hi float64
	}
	var cats []catCheck
	var nums []numCheck

	for name, val := range f.Eq {
		if name == skipAttr {
			continue
		}
		if sa == nil {
			return nil, false
		}
		col := sa.cat[name]
		dict := sa.dict[name]
		if col == nil || dict == nil {
			return nil, false // нет такого категориального атрибута в сегменте
		}
		code, ok := dict.lookup(val)
		if !ok {
			return nil, false // значение не встречалось в сегменте
		}
		cats = append(cats, catCheck{codes: col.codes, code: code})
	}
	for name, rng := range f.Range {
		if name == skipAttr {
			continue
		}
		if sa == nil {
			return nil, false
		}
		col := sa.num[name]
		if col == nil {
			return nil, false
		}
		nums = append(nums, numCheck{vals: col.vals, lo: rng[0], hi: rng[1]})
	}

	if len(cats) == 0 && len(nums) == 0 {
		return func(int) bool { return true }, true
	}
	return func(i int) bool {
		for _, c := range cats {
			if c.codes[i] != c.code {
				return false
			}
		}
		for _, n := range nums {
			v := n.vals[i]
			if !(v >= n.lo && v <= n.hi) { // NaN (отсутствует) → false
				return false
			}
		}
		return true
	}, true
}

// matchAttrs проверяет Attributes против фильтра напрямую (delta-путь, без колонок).
// skipAttr исключается (как в compile). Дельта мала → строковое сравнение приемлемо.
func matchAttrs(a Attributes, f Filter, skipAttr string) bool {
	for name, val := range f.Eq {
		if name == skipAttr {
			continue
		}
		if a.Cat[name] != val {
			return false
		}
	}
	for name, rng := range f.Range {
		if name == skipAttr {
			continue
		}
		v, ok := a.Num[name]
		if !ok || !(v >= rng[0] && v <= rng[1]) {
			return false
		}
	}
	return true
}
