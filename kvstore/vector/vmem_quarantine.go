package vector

// =============================================================================
// VMEM — карантин: массовый отзыв убеждений по происхождению (примитив 2 слоя
// восстановления, 26.07). Операция дня после инцидента: «всё, что записал этот
// канал начиная с такого-то момента, больше не считать истиной».
//
// ПОЧЕМУ ОТДЕЛЬНАЯ ОСЬ, А НЕ valid_to. Развилка выглядела как выбор из двух:
// писать valid_to = момент карантина или valid_to = valid_from. Оба ответа
// неверны, и это видно, если назвать, что означает каждая ось.
//   - valid_from/valid_to — время ПРИКЛАДНОЕ: «истинно с … по …». Отравленный
//     факт не был истинным никогда, поэтому valid_to = момент карантина —
//     прямая ложь в терминах самой оси.
//   - valid_to = valid_from («не был истинным никогда») терминологически
//     честнее, но стирает след того, что агент в это ВЕРИЛ. А продаём мы
//     ровно это: «что агент знал в 14:32». Ответ «ничего не знал» на вопрос,
//     заданный про момент, когда он уже действовал по подсунутому факту, —
//     уничтожение улики.
// Поэтому карантин получает СВОЮ ось: quarantined_at = «убеждение отозвано в
// этот момент». Прикладное время не трогается вовсе, история веры цела:
// AS_OF до отзыва честно показывает факт, AS_OF после — не показывает.
// Это же — первый маленький шаг к битемпоральности, которая и так записана
// открытой дверью (VMEM_DESIGN), а не костыль поперёк неё.
//
// ПОЧЕМУ АТРИБУТ НЕ ШТАМПУЕТСЯ ВСЕМ (в отличие от valid_to и от source).
// Отсутствие quarantined_at означает ровно одно — «не карантинился», и это
// подавляющий обычный случай. Для valid_to сентинел нужен потому, что по нему
// идёт Range-предикат пре-фильтра, а отсутствующий атрибут Range не проходит;
// карантин же судится в ранговом слое по проекции numsForKeys, где NaN значит
// «атрибута нет» и обрабатывается явно. Следствие важнее эстетики: факты,
// записанные ДО появления карантина, продолжают читаться без миграции —
// апгрейд не прячет данные, которые пользователь уже положил.
// (Сравнить с source: там отсутствие создавало слепое пятно для операции
// отбора, и потому потребовало явного unknown. Здесь отсутствие само по себе
// однозначно и безопасно.)
//
// Механика — та же двухфазность, что у TTL-жнеца: скан кандидатов под RLock
// (черновик) и приговор под эксклюзивным замком с перепроверкой свежайшей
// версии. Отличие в исходе: жнец удаляет, карантин дописывает новую версию
// факта с проставленной осью — данные не теряются, отзыв обратим по устройству.
// =============================================================================

import (
	"errors"
	"maps"
	"math"
	"strings"
)

// vmemAttrQuarantinedAt — NUM-атрибут «убеждение отозвано в этот момент»
// (unix-секунды). Отсутствие = не карантинился.
const vmemAttrQuarantinedAt = "quarantined_at"

// vmemQuarantineLimit — потолок фактов за один вызов. Карантин идёт ОДНОЙ
// атомарной WAL-записью (полу-карантин после краша неотличим от «часть лжи
// пережила отзыв»), поэтому размер батча ограничен сверху; хвост берётся
// повторным вызовом, операция идемпотентна по построению (уже отозванные
// кандидаты отсеиваются приговором).
const vmemQuarantineLimit = 4096

var (
	// ErrVMEMQuarantineSource — источник обязателен: карантин без предиката
	// происхождения означал бы «отозвать всё подряд», и такая команда не
	// должна существовать по недосмотру парсера.
	ErrVMEMQuarantineSource = errors.New("vmem: quarantine requires a source")
	// ErrVMEMQuarantineSince — граница since вне (0, сентинел).
	ErrVMEMQuarantineSince = errors.New("vmem: quarantine since must be positive unix seconds below the sentinel")
)

// QuarantineRequest — вход VMEM.QUARANTINE.
type QuarantineRequest struct {
	Scope  string // чья память; обязателен
	Source string // происхождение, подлежащее отзыву; обязателен
	Since  int64  // отзывать факты с valid_from >= Since; 0 → без нижней границы
	Limit  int    // потолок батча; 0 → vmemQuarantineLimit
}

// QuarantineResult — новые (отозванные) версии фактов: ровно то, что обязано
// уйти в WAL одной записью OpVSimAddDocBatch.
type QuarantineResult struct {
	Docs []RememberedDoc
}

// Quarantine — массовый отзыв по происхождению. Возвращает отозванные версии;
// пустой результат означает «под предикат никто не попал» и не является
// ошибкой (повторный карантин того же источника идемпотентен).
func (lvs *LeveledVectorStore) Quarantine(req QuarantineRequest, now int64) (QuarantineResult, error) {
	if now <= 0 || now >= vmemOpenValidTo {
		return QuarantineResult{}, ErrVMEMNow
	}
	if req.Scope == "" {
		return QuarantineResult{}, ErrVMEMScope
	}
	if req.Source == "" {
		return QuarantineResult{}, ErrVMEMQuarantineSource
	}
	if req.Since < 0 || req.Since >= vmemOpenValidTo {
		return QuarantineResult{}, ErrVMEMQuarantineSince
	}
	limit := req.Limit
	if limit <= 0 || limit > vmemQuarantineLimit {
		limit = vmemQuarantineLimit
	}
	cands := lvs.collectBySource(req.Source, limit)
	if len(cands) == 0 {
		return QuarantineResult{}, nil
	}
	return lvs.quarantineKeys(cands, req, now)
}

// collectBySource — фаза скана: ключи с CAT source == want по активной дельте,
// flushing-дельтам и колонкам сегментов. Черновик: scope, нижняя граница и
// «уже отозван» проверяются приговором по СВЕЖАЙШЕЙ версии — здесь дешёвый
// отбор по самому селективному предикату.
func (lvs *LeveledVectorStore) collectBySource(source string, limit int) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	var tombs map[string]struct{}
	if tp := lvs.tombstones.Load(); tp != nil {
		tombs = *tp
	}
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
		for _, key := range mt.KeysCatEq(vmemAttrSource, source) {
			if !add(key, true) {
				return out
			}
		}
	}
	for _, level := range lvs.levels {
		for _, seg := range level {
			switch s := seg.(type) {
			case *frozenSegment:
				if !scanCatEqColumn(s.attrs, s.fg.keys, vmemAttrSource, source, add) {
					return out
				}
			case *frozenSQSegment:
				if !scanCatEqColumn(s.attrs, s.fg.keys, vmemAttrSource, source, add) {
					return out
				}
			case *hnswSegment:
				s.mu.RLock()
				if code, col, ok := catCodeOf(s.attrs, vmemAttrSource, source); ok {
					for id, key := range s.keys {
						if s.g.nodes[id].Alive && col.codes[int(id)] == code {
							if !add(key, true) {
								s.mu.RUnlock()
								return out
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

// catCodeOf — код значения want в словаре CAT-колонки сегмента. ok=false, если
// колонки нет или значение в этом сегменте не встречалось: тогда сегмент можно
// пропустить целиком, не сравнивая строки построчно (весь смысл словаря).
func catCodeOf(sa *segmentAttrs, name, want string) (uint32, *categoricalColumn, bool) {
	if sa == nil {
		return 0, nil, false
	}
	col, ok := sa.cat[name]
	if !ok {
		return 0, nil, false
	}
	d, ok := sa.dict[name]
	if !ok {
		return 0, nil, false
	}
	code, ok := d.lookup(want)
	if !ok {
		return 0, nil, false
	}
	return code, col, true
}

// scanCatEqColumn — скан CAT-колонки frozen/SQ-сегмента по коду значения.
// Пустой слот ключа = tombstone. В add уходит zero-copy view (клон делается
// только для реального кандидата). false от add — батч полон, прервать обход.
func scanCatEqColumn(sa *segmentAttrs, keys internedKeys, name, want string, add func(string, bool) bool) bool {
	code, col, ok := catCodeOf(sa, name, want)
	if !ok {
		return true
	}
	for i := 0; i < keys.count(); i++ {
		if col.codes[i] == code {
			if k := keys.view(i); k != "" && !add(k, false) {
				return false
			}
		}
	}
	return true
}

// quarantineKeys — фаза приговора: весь батч под ОДНИМ эксклюзивным замком
// (единица уступки — батч, как у жнеца: перепроверка O(log n) на ключ).
// Кандидат отсеивается, если свежайшая версия сменила scope или источник
// (upsert после скана), лежит раньше нижней границы, уже отозвана, стёрта или
// истекла по TTL. Отозванная версия — полная копия свежайшей плюс одна ось:
// текст, вектор, термы и прикладное время не трогаются.
func (lvs *LeveledVectorStore) quarantineKeys(keys []string, req QuarantineRequest, now int64) (QuarantineResult, error) {
	docs := make([]RememberedDoc, 0, len(keys))
	lvs.mu.Lock()
	for _, key := range keys {
		target, ok := lvs.getFactDocLocked(key)
		if !ok {
			continue
		}
		if target.Attrs.Cat[vmemAttrScope] != req.Scope || target.Attrs.Cat[vmemAttrSource] != req.Source {
			continue
		}
		if vf, ok := target.Attrs.Num[vmemAttrValidFrom]; !ok || vf < float64(req.Since) {
			continue
		}
		// Истёкший по TTL факт уже невидим на чтении — отзывать нечего
		// (иначе успех карантина зависел бы от расписания жнеца).
		if exp, ok := target.Attrs.Num[vmemAttrExpiresAt]; ok && exp <= float64(now) {
			continue
		}
		// Идемпотентность: повторный карантин не переписывает момент отзыва
		// (иначе он «уезжал» бы вперёд, ломая AS_OF-ответы про прошлое).
		if _, already := target.Attrs.Num[vmemAttrQuarantinedAt]; already {
			continue
		}
		docs = append(docs, quarantineFactDoc(target, float64(now)))
	}
	if len(docs) == 0 {
		lvs.mu.Unlock()
		return QuarantineResult{}, nil
	}
	if lvs.delta == nil {
		if err := lvs.initDeltaLocked(len(docs[0].Vec)); err != nil {
			lvs.mu.Unlock()
			return QuarantineResult{}, err
		}
	}
	full := false
	for _, d := range docs {
		if lvs.delta.AppendDoc(d.ID, d.Vec, d.Attrs, d.Terms) {
			full = true
		}
		lvs.removeTombstone(d.ID)
	}
	lvs.touchMutation()
	lvs.mu.Unlock()
	if full {
		lvs.triggerCompact()
	}
	return QuarantineResult{Docs: docs}, nil
}

// quarantineFactDoc — отозванная версия факта: содержимое цели бит-в-бит плюс
// quarantined_at. Attrs — глубокая копия по той же причине, что у closeFactDoc:
// мапы делятся с исходной вставкой, а из flushing-дельты их параллельно читает
// build-горутина.
func quarantineFactDoc(target DeltaEntry, at float64) RememberedDoc {
	attrs := Attributes{Num: make(map[string]float64, len(target.Attrs.Num)+1)}
	if target.Attrs.Cat != nil {
		attrs.Cat = maps.Clone(target.Attrs.Cat)
	}
	maps.Copy(attrs.Num, target.Attrs.Num)
	attrs.Num[vmemAttrQuarantinedAt] = at
	return RememberedDoc{ID: target.Key, Vec: target.Vec, Attrs: attrs, Terms: target.Terms}
}

// vmemQuarantinedAt — момент отзыва у свежайшей версии факта (для тестов и
// будущего EXPLAIN). NaN → не карантинился.
func (lvs *LeveledVectorStore) vmemQuarantinedAt(key string) float64 {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	if v, ok := lvs.factNumAttrLocked(key, vmemAttrQuarantinedAt); ok {
		return v
	}
	return math.NaN()
}
