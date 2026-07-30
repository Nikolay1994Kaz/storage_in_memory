package vector

import "maps"

// =============================================================================
// VMEM — ПЕРЕШИФРОВКА ЛЕГАСИ: сделать крипто-стираемым то, что записано до
// шифрования.
//
// ЗАЧЕМ. KeyCoverage измеряет долю фактов, до которых уничтожение ключа реально
// дотянется. На корпусе, накопленном до -encrypt-at-rest, эта доля равна нулю:
// VMEM.SHRED отработает, квитанция будет честной («ключ K уничтожен»), а факты
// останутся читаемы в журнале и архивах. Перешифровка — единственный способ
// сдвинуть это число, потому что догнать уже записанные копии удалением нельзя
// в принципе.
//
// ⭐ЧЕМ ЭТО ПРИНЦИПИАЛЬНО ОТЛИЧАЕТСЯ ОТ BACKFILL ПРОВЕНАНСА, И ПОЧЕМУ ЗДЕСЬ
// ШТАМП СТАВИТЬ МОЖНО. Провенанс объявляет ОПЕРАТОР: source — его утверждение о
// прошлом, поэтому проставлять его задним числом можно только там, где значения
// нет вовсе. Атрибут sealed — не утверждение, а ФИЗИЧЕСКИЙ ФАКТ: ушли байты под
// конвертом или нет. Поставить sealed записи, чьи байты не шифровались, значит
// соврать (и по этой причине бэкфилла для этой оси не существует). Здесь же
// факт ПЕРЕЗАПИСЫВАЕТСЯ заново, и новая запись действительно уходит под
// конвертом — штамп отражает то, что произошло с байтами прямо сейчас.
//
// ⚠ЧЕГО ПЕРЕШИФРОВКА НЕ ДЕЛАЕТ, И ЭТО ОБЯЗАНО ЕХАТЬ В КВИТАНЦИИ. Она НЕ
// достаёт старые копии. Сегменты WAL, снапшоты и архивы, снятые ДО неё, как
// лежали открытым текстом, так и лежат; уничтожение ключа их не коснётся.
// Перешифровка переводит факт в стираемое состояние НАЧИНАЯ С ЭТОГО МОМЕНТА —
// и ровно так о ней надо говорить, иначе выросшее до 100% покрытие будет
// прочитано как «всё стираемо», что неправда для всего, что уже уехало.
// =============================================================================

// ResealRequest — заявка на перешифровку скоупа.
type ResealRequest struct {
	Scope string
	Limit int
}

// ResealResult — новые версии фактов, которые обязаны уйти в WAL под конвертом.
type ResealResult struct {
	Docs []RememberedDoc
}

// Reseal — перевести факты скоупа, записанные без конверта, в шифруемое
// состояние.
//
// Пустой результат — не ошибка: значит, покрытие ключом уже полное.
func (lvs *LeveledVectorStore) Reseal(req ResealRequest, now int64) (ResealResult, error) {
	if now <= 0 || now >= vmemOpenValidTo {
		return ResealResult{}, ErrVMEMNow
	}
	if req.Scope == "" {
		return ResealResult{}, ErrVMEMScope
	}
	limit := req.Limit
	if limit <= 0 || limit > vmemQuarantineLimit {
		limit = vmemQuarantineLimit
	}
	cands := lvs.collectUnsealed(req.Scope, limit)
	if len(cands) == 0 {
		return ResealResult{}, nil
	}
	return lvs.resealKeys(cands, req, now)
}

// collectUnsealed — фаза скана. Как и у провенанса, отсутствие атрибута не
// выражается колоночным предикатом, поэтому идём полным обходом живых ключей.
func (lvs *LeveledVectorStore) collectUnsealed(scope string, limit int) []string {
	var keys []string
	lvs.ForEach(func(key string, _ []float32) { keys = append(keys, key) })
	if len(keys) == 0 {
		return nil
	}
	scopes := lvs.catForKeys(keys, vmemAttrScope)
	sealed := lvs.catForKeys(keys, vmemAttrSealed)
	out := make([]string, 0, 16)
	for i, key := range keys {
		if scopes[i] != scope || sealed[i] == "1" {
			continue
		}
		out = append(out, key)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// resealKeys — фаза приговора под ОДНИМ эксклюзивным замком, как у карантина и
// бэкфилла.
func (lvs *LeveledVectorStore) resealKeys(keys []string, req ResealRequest, now int64) (ResealResult, error) {
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
		// Единственный предикат: конверта не было. Параллельный upsert мог
		// успеть записать факт заново уже под конвертом.
		if target.Attrs.Cat[vmemAttrSealed] == "1" {
			continue
		}
		// Истёкший по TTL факт перешифровывать нечего: он уже невидим на
		// чтении, а физически его снимет жнец. Иначе результат зависел бы от
		// расписания жнеца — та же оговорка, что у бэкфилла.
		if exp, ok := target.Attrs.Num[vmemAttrExpiresAt]; ok && exp <= float64(now) {
			continue
		}
		docs = append(docs, resealFactDoc(target))
	}
	if len(docs) == 0 {
		lvs.mu.Unlock()
		return ResealResult{}, nil
	}
	if lvs.delta == nil {
		if err := lvs.initDeltaLocked(len(docs[0].Vec)); err != nil {
			lvs.mu.Unlock()
			return ResealResult{}, err
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
	return ResealResult{Docs: docs}, nil
}

// resealFactDoc — новая версия факта: содержимое цели бит-в-бит плюс штамп
// sealed. Глубокая копия по той же причине, что у quarantineFactDoc: мапы
// делятся с исходной вставкой, а из flushing-дельты их параллельно читает
// build-горутина.
//
// ⚠Штамп ставится ЗДЕСЬ, а командный слой обязан записать эту версию под
// конвертом. Если он этого не сделает, атрибут соврёт — поэтому в тестах
// проверяется не «Reseal проставил sealed», а что в журнале лежит шифротекст.
func resealFactDoc(target DeltaEntry) RememberedDoc {
	attrs := Attributes{
		Cat: make(map[string]string, len(target.Attrs.Cat)+1),
		Num: maps.Clone(target.Attrs.Num),
	}
	maps.Copy(attrs.Cat, target.Attrs.Cat)
	attrs.Cat[vmemAttrSealed] = "1"
	return RememberedDoc{ID: target.Key, Vec: target.Vec, Attrs: attrs, Terms: target.Terms}
}
