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

// QuarantineRemainder — сколько фактов ЭТОГО ЖЕ источника память всё ещё
// считает истиной ПОСЛЕ отзыва, и по какой причине предикат их не взял.
//
// ⭐ЗАЧЕМ. Число отозванных ≠ полнота лечения. Команда, отвечающая «отозвано
// 3», одинаково звучит и когда лжи больше нет, и когда двенадцать фактов того
// же канала остались нетронутыми за границей окна. Разницу знал только тот,
// кто считал её снаружи (scripts/revocation_limits.py), — то есть измерение
// жило в харнессе, а продукт молчал. Это единственное, чем ответ отличался от
// вопроса, который задаёт закупочный опросник: «как вы знаете, что устранение
// было полным».
//
// Разбивка — ТОЧНОЕ ЗЕРКАЛО условий приговора quarantineKeys, а не удобная
// категоризация рядом с ним: OutsideWindow считает ровно то, что приговор
// отвергает предикатом valid_from (включая факт БЕЗ valid_from — такой не
// отзывается ни при каком окне, легаси-аналог absent в VMEM.COVERAGE), а
// OverLimit — всё прочее, то есть подходящее под предикат и не влезшее в батч.
// Сумма причин равна StillTrusted по построению; ценность не в их сходимости,
// а в том, что StillTrusted ИЗМЕРЕН по состоянию, а не выведен из счётчиков.
type QuarantineRemainder struct {
	StillTrusted  int // живых, не отозванных фактов scope+source
	OutsideWindow int // из них: предикат valid_from их не берёт
	OverLimit     int // из них: под предикат подходят, ждут следующего вызова
}

// QuarantineResult — новые (отозванные) версии фактов: ровно то, что обязано
// уйти в WAL одной записью OpVSimAddDocBatch, плюс остаток по тому же
// источнику.
type QuarantineResult struct {
	Docs      []RememberedDoc
	Remainder QuarantineRemainder
}

// Quarantine — массовый отзыв по происхождению. Возвращает отозванные версии;
// пустой результат означает «под предикат никто не попал» и не является
// ошибкой (повторный карантин того же источника идемпотентен). Остаток
// считается ВСЕГДА, в том числе при пустом результате: именно там ответ «0»
// и нуждается в расшифровке больше всего.
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
	var res QuarantineResult
	if cands := lvs.collectBySource(req, now, limit); len(cands) > 0 {
		var err error
		if res, err = lvs.quarantineKeys(cands, req, now); err != nil {
			return QuarantineResult{}, err
		}
	}
	res.Remainder = lvs.sourceRemainder(req, now)
	return res, nil
}

// sourceRemainder — остаток по предикату происхождения, измеренный ПОЛНЫМ
// проходом по состоянию ПОСЛЕ приговора.
//
// 🚨ПОЧЕМУ НЕ АРИФМЕТИКА ПО ЧЕРНОВИКУ СКАНА, ХОТЯ ОНА БЕСПЛАТНА. collectBySource
// обрывается, как только батч набран: при сработавшем LIMIT он не досматривает
// стор до конца и попросту НЕ ВИДИТ хвост. Счётчик, снятый по дороге, назвал бы
// остаток нулём ровно в том случае, ради которого поле и заводится, — а поле
// это ответ на вопрос о полноте. Ошибка была бы в нашу пользу и незаметна
// снаружи: тот же класс, что «прибор считал не то, что показывал».
//
// Отсюда же — «после», а не «до»: остаток есть свойство ИСХОДА, и меряться он
// обязан по памяти, а не по намерению. Цена — полный скан на вызов, как у
// VMEM.COVERAGE; карантин это операция дня после инцидента, а не горячий путь.
func (lvs *LeveledVectorStore) sourceRemainder(req QuarantineRequest, now int64) QuarantineRemainder {
	// Ключи отдельным проходом по той же причине, что в QuarantinedFacts и
	// ProvenanceCoverage: ForEach держит lvs.mu.RLock, а чтение атрибутов
	// берёт его же — рекурсивный RLock с ждущим писателем даёт дедлок.
	var keys []string
	lvs.ForEach(func(key string, _ []float32) { keys = append(keys, key) })
	if len(keys) == 0 {
		return QuarantineRemainder{}
	}
	scopes := lvs.catForKeys(keys, vmemAttrScope)
	sources := lvs.catForKeys(keys, vmemAttrSource)
	nums := lvs.numsForKeys(keys, vmemAttrValidFrom, vmemAttrExpiresAt, vmemAttrQuarantinedAt)

	var r QuarantineRemainder
	for i := range keys {
		if scopes[i] != req.Scope || sources[i] != req.Source {
			continue
		}
		validFrom, expiresAt, quarantinedAt := nums[i][0], nums[i][1], nums[i][2]
		if !math.IsNaN(quarantinedAt) {
			continue // отозван — этот и есть результат лечения
		}
		// Истёкший по TTL невидим на чтении, значит истиной уже не считается;
		// приговор пропускает его по тому же основанию.
		if !math.IsNaN(expiresAt) && expiresAt <= float64(now) {
			continue
		}
		r.StillTrusted++
		if math.IsNaN(validFrom) || validFrom < float64(req.Since) {
			r.OutsideWindow++
		} else {
			r.OverLimit++
		}
	}
	return r
}

// quarantineEligibleLocked — ТОТ ЖЕ предикат, что у приговора, но по атрибутам
// свежайшей версии и под RLock.
//
// ⚠НЕ АВТОРИТЕТЕН И НЕ ПЫТАЕТСЯ БЫТЬ. Между сканом и приговором факт может
// измениться (гонка, закрытая a5d43ed), поэтому последнее слово остаётся за
// quarantineKeys, который перепроверяет всё то же самое под эксклюзивным
// замком. Здесь предикат нужен для ДРУГОГО: чтобы ЛИМИТ ПАРТИИ тратился на
// факты, которые действительно будут отозваны.
//
// ⭐Почему без этого было сломано. Лимит применялся к ЧЕРНОВИКУ: скан набирал
// первые limit ключей источника, а приговор потом отбрасывал уже отозванные.
// Повторный вызов набирал ТЕХ ЖЕ и возвращал 0 при неотозванном остатке —
// 20 фактов, отозвано 5, в памяти 15, и «возьмите хвост следующим вызовом»
// (docs/COMMANDS.md) не работало никогда. Ноль означал две разные вещи:
// «всё чисто» и «первая партия уже отозвана, остальное не тронуто».
//
// Отсеивать надо ВСЕ постоянные причины отказа, а не только «уже отозван»:
// чужой scope, факт раньше SINCE и истёкший по TTL точно так же навсегда
// съедали бы бюджет партии, и повторный вызов снова упирался бы в них.
func (lvs *LeveledVectorStore) quarantineEligibleLocked(key string, req QuarantineRequest, now int64) bool {
	attrs, ok := lvs.factAttrsLocked(key)
	if !ok {
		return false
	}
	if attrs.Cat[vmemAttrScope] != req.Scope || attrs.Cat[vmemAttrSource] != req.Source {
		return false
	}
	if vf, ok := attrs.Num[vmemAttrValidFrom]; !ok || vf < float64(req.Since) {
		return false
	}
	if exp, ok := attrs.Num[vmemAttrExpiresAt]; ok && exp <= float64(now) {
		return false
	}
	if _, already := attrs.Num[vmemAttrQuarantinedAt]; already {
		return false
	}
	return true
}

// collectBySource — фаза скана: ключи с CAT source == req.Source по активной
// дельте, flushing-дельтам и колонкам сегментов. Дешёвый отбор по самому
// селективному предикату идёт по колонке, а остальные условия карантина
// проверяет quarantineEligibleLocked — НЕ ради авторитетности (она у
// приговора), а чтобы limit считал ОТЗЫВЫ, а не кандидатов.
func (lvs *LeveledVectorStore) collectBySource(req QuarantineRequest, now int64, limit int) []string {
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
		if _, dup := seen[key]; dup {
			return len(out) < limit
		}
		// В seen кладём ДО проверки предиката: ключ встречается в нескольких
		// сегментах (старые версии), и перепроверять его на каждой — это
		// полный обход LSM на каждую версию.
		seen[key] = struct{}{}
		if !lvs.quarantineEligibleLocked(key, req, now) {
			return len(out) < limit
		}
		if !safe {
			key = strings.Clone(key)
		}
		out = append(out, key)
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
		for _, key := range mt.KeysCatEq(vmemAttrSource, req.Source) {
			if !add(key, true) {
				return out
			}
		}
	}
	for _, level := range lvs.levels {
		for _, seg := range level {
			switch s := seg.(type) {
			case *frozenSegment:
				if !scanCatEqColumn(s.attrs, s.fg.keys, vmemAttrSource, req.Source, add) {
					return out
				}
			case *frozenSQSegment:
				if !scanCatEqColumn(s.attrs, s.fg.keys, vmemAttrSource, req.Source, add) {
					return out
				}
			case *hnswSegment:
				// 🚨ДВЕ ФАЗЫ, И ЭТО НЕ СТИЛЬ. Совпадения снимаются под s.mu, а
				// add вызывается ПОСЛЕ RUnlock, потому что add → предикат →
				// factAttrsLocked берёт s.mu.RLock ТОГО ЖЕ сегмента (свежайшая
				// версия часто лежит именно здесь). Рекурсивный RLock на
				// sync.RWMutex — не безобидность: писатель, вставший между
				// двумя захватами, вешает обе стороны насмерть.
				var matched []string
				s.mu.RLock()
				if code, col, ok := catCodeOf(s.attrs, vmemAttrSource, req.Source); ok {
					for id, key := range s.keys {
						if s.g.nodes[id].Alive && col.codes[int(id)] == code {
							matched = append(matched, key)
						}
					}
				}
				s.mu.RUnlock()
				for _, key := range matched {
					if !add(key, true) {
						return out
					}
				}
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

// QuarantinedFacts — множество фактов, у которых проставлена ось
// quarantined_at, то есть отозванных и намеренно оставленных в памяти.
//
// ⭐ЗАЧЕМ ОТДЕЛЬНЫМ МЕТОДОМ. Сверка состояния с журналом обязана отличать
// ОТОЗВАННЫЙ факт от ВОСКРЕСШЕГО, и отличие между ними ровно одно: у
// отозванного эта ось есть. Без неё «карантин сработал» и «отзыв не сработал»
// выглядят одинаково — факт записан в журнал как снятый и при этом лежит в
// памяти, — и самый тяжёлый класс тревоги срабатывал бы на КАЖДОМ успешном
// массовом отзыве, то есть ровно в том сценарии, ради которого сверка нужна.
func (lvs *LeveledVectorStore) QuarantinedFacts() map[string]bool {
	// Ключи собираем ОТДЕЛЬНЫМ проходом по той же причине, что FactScopes и
	// ProvenanceCoverage: ForEach держит lvs.mu.RLock, а чтение атрибутов
	// берёт его же — рекурсивный RLock с ждущим писателем даёт дедлок.
	var keys []string
	lvs.ForEach(func(key string, _ []float32) { keys = append(keys, key) })
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]bool, len(keys))
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	for _, key := range keys {
		if _, ok := lvs.factNumAttrLocked(key, vmemAttrQuarantinedAt); ok {
			out[key] = true
		}
	}
	return out
}
