package vector

import "strings"

// =============================================================================
// VMEM шаг 6 — erasure (docs/VMEM_DESIGN.md): FORGET по id + TTL-жнец.
//
// Корректность от жнеца НЕ зависит: истёкший факт скрыт на чтении с шага 3
// (Range по expires_at против настоящего now во всех режимах RECALL) — жнец
// лишь возвращает ресурсы. Поэтому он ленивый, батчевый и прерываемый: его
// запаздывание невидимо снаружи, а гонки решаются перепроверкой приговора.
//
// Две фазы:
//   1. collectExpired — скан кандидатов по всем источникам LSM под RLock;
//      список — черновик: в него попадают и затенённые старые копии ключа,
//      и жертвы, которых параллельный Remember успел освежить после скана.
//   2. reapKeys — приговор: под эксклюзивным lvs.mu.Lock перечитывается
//      expires_at СВЕЖАЙШЕЙ версии ключа, и только всё-ещё-истёкший
//      удаляется deleteLocked. «Поздний прав» (контракт шага 4): освежённый
//      факт побеждает жнеца.
//
// Дверь 1 (реплей не смотрит на часы): жнец — единственный потребитель часов,
// но его решение локально и в WAL не едет. После реплея истёкшие факты
// воскресают физически, остаются невидимыми (фильтр чтения) и будут пожаты
// заново на ближайшем простое — идемпотентная сходимость вместо журналирования.
//
// FORGET не ходит по цепочкам supersedes (контракт): valid_to соседей —
// физический атрибут их собственных доков, стирание звена ничего не
// воскрешает. Проверка принадлежности scope — вопрос командного слоя (шаг 7).
// =============================================================================

// vmemReapTickLimit — потолок жатвы за один idle-тик: фоновая работа обязана
// уступать (урок brownout), хвост дожнётся следующим тиком.
const vmemReapTickLimit = 4096

// Forget — внутренний путь VMEM.FORGET (RESP-команда — шаг 7): erasure по id,
// физически и без обхода цепочек supersedes. Синоним Delete по построению —
// весь смысл шага 6 в жнеце; отдельное имя фиксирует контрактную роль.
// Повторный FORGET того же id возвращает false (идемпотентность стирания).
func (lvs *LeveledVectorStore) Forget(id string) bool {
	return lvs.Delete(id)
}

// ReapExpired — TTL-жнец: стереть до limit фактов с expires_at <= now.
// Возвращает число пожатых. Вызывается idle-механикой (handleIdleTick) и
// напрямую тестами/оператором; безопасен конкурентно с писателями и чтением.
func (lvs *LeveledVectorStore) ReapExpired(now int64, limit int) int {
	if now <= 0 || limit <= 0 {
		return 0
	}
	cands := lvs.collectExpired(now, limit)
	if len(cands) == 0 {
		return 0
	}
	return lvs.reapKeys(cands, now)
}

// collectExpired — фаза скана: ключи с expires_at <= now по активной дельте,
// flushing-дельтам и колонкам сегментов. Дедуп между источниками; limit
// ограничивает батч. Черновик приговора — окончательное решение за reapKeys.
func (lvs *LeveledVectorStore) collectExpired(now int64, limit int) []string {
	hi := float64(now)
	seen := make(map[string]struct{})
	out := make([]string, 0)
	// Уже погребённые (tombstone) милуются ЗДЕСЬ, а не в фазе приговора:
	// их физические копии лежат в сегментах до консолидации, и без этой
	// проверки каждый холостой проход жнеца заново тащил бы их в reapKeys —
	// эксклюзивный замок на каждую, читатели голодают (порог 3 ловил 0.24×).
	var tombs map[string]struct{}
	if tp := lvs.tombstones.Load(); tp != nil {
		tombs = *tp
	}
	// key может быть zero-copy view поверх blob сегмента (валиден под RLock
	// скана): map-lookup и дедуп не аллоцируют, клон — только для кандидата.
	add := func(key string, safe bool) bool {
		if _, gone := tombs[key]; gone {
			return len(out) < limit
		}
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			if !safe {
				key = strings.Clone(key)
			}
			out = append(out, key)
		}
		return len(out) < limit
	}

	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	memtables := make([]*DeltaSegment, 0, 1+len(lvs.flushing))
	if lvs.delta != nil {
		memtables = append(memtables, lvs.delta)
	}
	memtables = append(memtables, lvs.flushing...)
	for _, mt := range memtables {
		for _, key := range mt.KeysNumLE(vmemAttrExpiresAt, hi) {
			if !add(key, true) {
				return out
			}
		}
	}
	for _, level := range lvs.levels {
		for _, seg := range level {
			switch s := seg.(type) {
			case *frozenSegment:
				if !scanExpiredColumn(s.attrs, s.fg.keys, hi, add) {
					return out
				}
			case *frozenSQSegment:
				if !scanExpiredColumn(s.attrs, s.fg.keys, hi, add) {
					return out
				}
			case *hnswSegment:
				s.mu.RLock()
				if s.attrs != nil {
					if col, ok := s.attrs.num[vmemAttrExpiresAt]; ok {
						for id, key := range s.keys {
							// NaN (не-VMEM док) не проходит сравнение.
							if s.g.nodes[id].Alive && col.vals[int(id)] <= hi {
								if !add(key, true) {
									s.mu.RUnlock()
									return out
								}
							}
						}
					}
				}
				s.mu.RUnlock()
			}
		}
	}
	return out
}

// scanExpiredColumn — скан колонки expires_at frozen/SQ-сегмента: пустой слот
// ключа = tombstone, NaN и сентинел не проходят сравнение. В add уходит
// zero-copy view (safe=false → клон только для реального кандидата).
// false от add — батч полон, прервать обход.
func scanExpiredColumn(sa *segmentAttrs, keys internedKeys, hi float64, add func(string, bool) bool) bool {
	if sa == nil {
		return true
	}
	col, ok := sa.num[vmemAttrExpiresAt]
	if !ok {
		return true
	}
	for i := 0; i < keys.count(); i++ {
		if col.vals[i] <= hi {
			if k := keys.view(i); k != "" && !add(k, false) {
				return false
			}
		}
	}
	return true
}

// reapKeys — фаза приговора: весь батч под ОДНИМ замком, tombstones — одной
// COW-копией карты (per-key замок+копия давали O(n²) и голодание читателей —
// порог 3 ловил 0.14×). Единица уступки жатвы — батч (limit вызова), не ключ:
// перепроверка и классификация дешёвые (O(log n) на ключ), замок держится
// ~миллисекунды. Кандидат без expires_at у свежайшей версии (перезаписан
// не-VMEM upsert'ом) тоже помилован.
func (lvs *LeveledVectorStore) reapKeys(keys []string, now int64) int {
	n := 0
	var tombs []string
	lvs.mu.Lock()
	for _, key := range keys {
		if v, ok := lvs.factNumAttrLocked(key, vmemAttrExpiresAt); !ok || v > float64(now) {
			continue
		}
		deleted, needTomb := lvs.deleteClassifyLocked(key)
		if deleted {
			n++
		}
		if needTomb {
			tombs = append(tombs, key)
		}
	}
	addTombstones(&lvs.tombstones, tombs)
	lvs.mu.Unlock()
	return n
}

// factNumAttrLocked — NUM-атрибут СВЕЖАЙШЕЙ версии ключа (провенанс Get:
// активная дельта → tombstone-маска → flushing новее-первым → сегменты
// свежее-первым). Атрибуты-only зеркало getFactDocLocked: приговору жнеца не
// нужны вектор и термы. Вызывать под lvs.mu (любой стороны: чтение провенанса
// не мутирует). ok=false — ключа нет, стёрт или у его свежайшей версии нет
// такого атрибута.
func (lvs *LeveledVectorStore) factNumAttrLocked(key, name string) (float64, bool) {
	numOf := func(a Attributes) (float64, bool) {
		v, ok := a.Num[name]
		return v, ok
	}
	if lvs.delta != nil {
		if a, ok := lvs.delta.GetAttrs(key); ok {
			return numOf(a)
		}
	}
	if t := lvs.tombstones.Load(); t != nil {
		if _, deleted := (*t)[key]; deleted {
			return 0, false
		}
	}
	for i := len(lvs.flushing) - 1; i >= 0; i-- {
		if a, ok := lvs.flushing[i].GetAttrs(key); ok {
			return numOf(a)
		}
	}
	segNum := func(sa *segmentAttrs, idx int) (float64, bool) {
		if sa == nil {
			return 0, false
		}
		col, ok := sa.num[name]
		if !ok {
			return 0, false
		}
		v := col.vals[idx]
		return v, v == v // NaN = атрибута нет у дока
	}
	for _, level := range lvs.levels {
		for pos := len(level) - 1; pos >= 0; pos-- {
			switch s := level[pos].(type) {
			case *frozenSegment:
				if i, ok := s.fg.keys.find(key); ok {
					return segNum(s.attrs, i)
				}
			case *frozenSQSegment:
				if i, ok := s.fg.keys.find(key); ok {
					return segNum(s.attrs, i)
				}
			case *hnswSegment:
				s.mu.RLock()
				for id, k := range s.keys {
					if k == key && s.g.nodes[id].Alive {
						v, ok := segNum(s.attrs, int(id))
						s.mu.RUnlock()
						return v, ok
					}
				}
				s.mu.RUnlock()
			}
		}
	}
	return 0, false
}
