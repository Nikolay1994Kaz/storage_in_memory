package vector

// =============================================================================
// VMEM — миграция легаси: проставить источник фактам, у которых колонки source
// НЕТ ФИЗИЧЕСКИ (записаны до появления провенанса).
//
// ЗАЧЕМ. `VMEM.COVERAGE` на настоящем сторе показал ноль: массовый отзыв
// отбирает ПО ИСТОЧНИКУ, а у старых фактов источника нет — и отобрать их
// нельзя ничем, потому что пустота нефильтруема. То есть весь слой
// восстановления над легаси-данными не работает, и апгрейд движка сам по себе
// этого не чинит. Миграция переводит такие факты в состояние, где отзыв хотя
// бы ВОЗМОЖЕН.
//
// ПОЧЕМУ ИМЕННО КОМАНДОЙ, А НЕ АВТОМАТОМ ПРИ СТАРТЕ. Это запись в данные
// пользователя, причём запись СМЫСЛА: «за этот факт расписался вот кто».
// Молча дописывать смысл в чужую память при апгрейде — ровно то, за что мы
// критикуем чужие решения. Оператор запускает это осознанно и сам отвечает за
// значение, которое ставит.
//
// ПОЧЕМУ НИКОГДА НЕ ПЕРЕЗАПИСЫВАЕТ УЖЕ ОБЪЯВЛЕННЫЙ ИСТОЧНИК. Провенанс — это
// улика. Команда, умеющая переписать существующий source, умеет уничтожить
// след того, кто именно наполнил память, — то есть ломает то самое свойство,
// ради которого слой существует. Поэтому предикат ровно один и неотключаемый:
// атрибут отсутствует. Повторный запуск идемпотентен по построению.
//
// ЗНАЧЕНИЕ ЗАДАЁТ ОПЕРАТОР, А НЕ МЫ. Обычный ответ — литеральный `unknown`
// («никто не расписался»), и он честен. Но оператор, знающий происхождение
// легаси-корпуса («это импорт из старого CRM»), вправе поставить его: провенанс
// — это ВХОД, утверждение владельца данных, а не наше суждение о факте.
// Соврать он может и здесь — но соврать он может и при обычной записи; новых
// возможностей команда не даёт.
//
// ЦЕНА. Полный скан живых ключей, как у COVERAGE: индекса «атрибут
// отсутствует» не существует, ради него же всё и затевается. Операция
// админская, разовая, идёт батчами под потолком LIMIT.
// =============================================================================

import (
	"errors"
	"maps"
)

var (
	// ErrVMEMBackfillSource — значение обязательно: команда, дописывающая
	// пустую строку, тихо не сделала бы ничего (атрибут остался бы «нет»).
	ErrVMEMBackfillSource = errors.New("vmem: backfill requires a source value")
)

// BackfillSourceRequest — вход VMEM.BACKFILL.
type BackfillSourceRequest struct {
	Scope  string // чья память; обязателен
	Source string // что проставить отсутствующим; обычно "unknown"
	Limit  int    // потолок батча; 0 → vmemQuarantineLimit
}

// BackfillSourceResult — новые версии фактов, ровно то, что обязано уйти в WAL.
type BackfillSourceResult struct {
	Docs []RememberedDoc
}

// BackfillSource — проставить source фактам scope, у которых атрибута нет.
// Пустой результат — не ошибка: значит, покрытие уже полное.
func (lvs *LeveledVectorStore) BackfillSource(req BackfillSourceRequest, now int64) (BackfillSourceResult, error) {
	if now <= 0 || now >= vmemOpenValidTo {
		return BackfillSourceResult{}, ErrVMEMNow
	}
	if req.Scope == "" {
		return BackfillSourceResult{}, ErrVMEMScope
	}
	if req.Source == "" {
		return BackfillSourceResult{}, ErrVMEMBackfillSource
	}
	limit := req.Limit
	if limit <= 0 || limit > vmemQuarantineLimit {
		limit = vmemQuarantineLimit
	}
	cands := lvs.collectSourceless(req.Scope, limit)
	if len(cands) == 0 {
		return BackfillSourceResult{}, nil
	}
	return lvs.backfillKeys(cands, req, now)
}

// collectSourceless — фаза скана. Отсутствие атрибута не выражается ни одним
// колоночным предикатом, поэтому идём полным обходом живых ключей (как
// ProvenanceCoverage) и проверяем свежайшую версию точечно. Черновик: приговор
// перепроверит всё под эксклюзивным замком.
func (lvs *LeveledVectorStore) collectSourceless(scope string, limit int) []string {
	var keys []string
	lvs.ForEach(func(key string, _ []float32) { keys = append(keys, key) })
	if len(keys) == 0 {
		return nil
	}
	scopes := lvs.catForKeys(keys, vmemAttrScope)
	sources := lvs.catForKeys(keys, vmemAttrSource)
	out := make([]string, 0, 16)
	for i, key := range keys {
		if scopes[i] != scope || sources[i] != "" {
			continue
		}
		out = append(out, key)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// backfillKeys — фаза приговора: батч под ОДНИМ эксклюзивным замком, как у
// карантина. Кандидат отсеивается, если свежайшая версия сменила scope, уже
// обзавелась источником (параллельный upsert), стёрта или истекла по TTL.
func (lvs *LeveledVectorStore) backfillKeys(keys []string, req BackfillSourceRequest, now int64) (BackfillSourceResult, error) {
	docs := make([]RememberedDoc, 0, len(keys))
	lvs.mu.Lock()
	for _, key := range keys {
		target, ok := lvs.getFactDocLocked(key)
		if !ok {
			continue
		}
		if target.Attrs.Cat[vmemAttrScope] != req.Scope {
			continue
		}
		// Единственный и неотключаемый предикат: источника нет.
		if _, has := target.Attrs.Cat[vmemAttrSource]; has {
			continue
		}
		// Истёкший по TTL факт уже невидим на чтении — мигрировать нечего
		// (иначе результат зависел бы от расписания жнеца, как у карантина).
		if exp, ok := target.Attrs.Num[vmemAttrExpiresAt]; ok && exp <= float64(now) {
			continue
		}
		docs = append(docs, backfillFactDoc(target, req.Source))
	}
	if len(docs) == 0 {
		lvs.mu.Unlock()
		return BackfillSourceResult{}, nil
	}
	if lvs.delta == nil {
		if err := lvs.initDeltaLocked(len(docs[0].Vec)); err != nil {
			lvs.mu.Unlock()
			return BackfillSourceResult{}, err
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
	return BackfillSourceResult{Docs: docs}, nil
}

// backfillFactDoc — новая версия факта: содержимое цели бит-в-бит плюс одна
// CAT-колонка. Прикладное время, importance, оси валидности и карантина не
// трогаются — миграция чинит провенанс, а не переписывает историю. Глубокая
// копия по той же причине, что у quarantineFactDoc: мапы делятся с исходной
// вставкой, а из flushing-дельты их параллельно читает build-горутина.
func backfillFactDoc(target DeltaEntry, source string) RememberedDoc {
	attrs := Attributes{
		Cat: make(map[string]string, len(target.Attrs.Cat)+1),
		Num: maps.Clone(target.Attrs.Num),
	}
	maps.Copy(attrs.Cat, target.Attrs.Cat)
	attrs.Cat[vmemAttrSource] = source
	return RememberedDoc{ID: target.Key, Vec: target.Vec, Attrs: attrs, Terms: target.Terms}
}
