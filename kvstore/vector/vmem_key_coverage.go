package vector

// =============================================================================
// VMEM — покрытие КЛЮЧОМ: какая доля фактов вообще может быть крипто-стёрта.
//
// ЗАЧЕМ. VMEM.SHRED уничтожает ключ скоупа, и все копии, записанные ПОД ним,
// становятся нечитаемы. Но факты, записанные до того, как шифрование включили,
// лежат в журнале и архивах открытым текстом — уничтожение ключа их не
// касается. Квитанция, выданная без этого числа, позволяет принять нулевое
// покрытие за полное: аудитор увидит «ключ уничтожен» и решит, что данных
// больше нет. Поэтому число измеряется и предъявляется рядом.
//
// Это ровно та же болезнь, что лечил ProvenanceCoverage для отзыва по
// источнику, и лечится она так же — измерением, а не обещанием. На реальном
// сторе покрытие провенансом оказалось НОЛЬ; предполагать, что с ключами
// иначе, оснований нет.
//
// ПОЧЕМУ ПО АТРИБУТУ, А НЕ ПО КЕЙРИНГУ. Наличие KEK у скоупа говорит лишь, что
// ключ есть СЕЙЧАС, — а не что каждая персистентная копия под ним. Скоуп,
// половина фактов которого записана до шифрования, показал бы «покрыт», то
// есть отчёт соврал бы в НАШУ пользу. Атрибут ставится в момент записи и
// отражает то, что действительно произошло с байтами.
//
// ⭐ЭТОГО ОКАЗАЛОСЬ МАЛО. Рассуждение выше верно и всё равно врёт в нашу пользу
// — слоем ниже, чем его вели. Атрибут фиксирует НАМЕРЕНИЕ в момент записи, а
// между намерением и байтами на диске лежит весь конвейер: заморозка дельты,
// выбор типа сегмента, merge-каскад, запись снапшота. Конвейер может изменить
// ответ — и изменяет: запечатанный путь v8 реализован ТОЛЬКО для
// frozenSegment. Факт, записанный под шифрованием, но осевший в
// frozenSQSegment или hnswSegment, уезжает в снапшот открытым, а отчёт
// показывал 1.0000.
//
// Отсюда правило, которым живёт этот файл: метрика покрытия, читающая флаг,
// проставленный кем-то другим, — это ДЕКЛАРАЦИЯ, а не измерение. Поэтому к
// намерению добавлена вторая ось — умеет ли МЕСТО, где факт сейчас лежит,
// вообще писать его под конверт (segmentSealsScopes). Покрытым считается
// только пересечение.
// =============================================================================

// KeyReport — покрытие ключом одного scope.
//
// Total = Sealed + Exposed + Unsealed. Два вида непокрытия разделены намеренно:
// у них разное лечение. Unsealed чинится VMEM.RESEAL (перезаписать под
// конвертом), Exposed командой не чинится вовсе — это дефект движка, и до
// починки формата такие факты стираются только вместе с сегментом.
type KeyReport struct {
	Scope    string
	Total    int
	Sealed   int // под конвертом И в типе сегмента, который его пишет → крипто-стираемы
	Exposed  int // намерение было, но место хранения конверта не пишет → НЕ стираемы
	Unsealed int // атрибута нет: записаны до шифрования → НЕ стираемы
}

// SealedShare — доля фактов, до которых уничтожение ключа реально дотянется.
func (r KeyReport) SealedShare() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Sealed) / float64(r.Total)
}

// segmentSealsScopes — умеет ли ТИП сегмента писать факты скоупа под конверт.
//
// БЕЛЫЙ СПИСОК, А НЕ ЧЁРНЫЙ, и это главное здесь. Новый тип сегмента
// добавляется правкой одного case в общем switch записи — а такая правка
// никогда не ломает соседние case: компилятор молчит, чужие тесты зелёные,
// ревью видит цельную работу. Ровно так и проехали эти два. Значит умолчание
// обязано быть «не умеет»: незнакомый тип считается непокрытым, и отчёт
// падает ниже 1.0000 сам, без чьего-либо участия.
//
// Умеют все три известных типа — но каждый научился этому отдельной правкой, и
// именно поэтому список белый: frozenSegment получил документную секцию в v8,
// frozenSQSegment и hnswSegment — только в v9, а между этими версиями отчёт
// показывал по ним 1.0000. Четвёртый тип, добавленный так же незаметно, обязан
// провалиться в default и опустить долю, а не унаследовать чужое доверие.
func segmentSealsScopes(seg segment) bool {
	switch seg.(type) {
	case *frozenSegment, *frozenSQSegment, *hnswSegment:
		return true
	default:
		return false
	}
}

// KeyCoverage — покрытие ключом по каждому scope. scopeEq != "" сужает до
// одного. Считаются только VMEM-факты (доки с атрибутом scope). Полный скан:
// операция админская, не горячий путь.
func (lvs *LeveledVectorStore) KeyCoverage(scopeEq string) []KeyReport {
	// Ключи отдельным проходом — ForEach держит RLock, а чтение атрибутов
	// берёт его же (тот же приём, что в ProvenanceCoverage и CollectScope).
	var keys []string
	lvs.ForEach(func(key string, _ []float32) { keys = append(keys, key) })
	if len(keys) == 0 {
		return nil
	}
	scopes := lvs.catForKeys(keys, vmemAttrScope)
	sealed := lvs.catForKeys(keys, vmemAttrSealed)
	sealable := lvs.sealablePlaceForKeys(keys)

	byScope := make(map[string]*KeyReport)
	order := make([]string, 0, 8)
	for i, sc := range scopes {
		if sc == "" || (scopeEq != "" && sc != scopeEq) {
			continue // не VMEM-факт либо другой scope
		}
		rep, ok := byScope[sc]
		if !ok {
			rep = &KeyReport{Scope: sc}
			byScope[sc] = rep
			order = append(order, sc)
		}
		rep.Total++
		switch {
		case sealed[i] != "1":
			rep.Unsealed++ // намерения не было: запись до шифрования
		case !sealable[i]:
			rep.Exposed++ // намерение было, место хранения конверта не пишет
		default:
			rep.Sealed++
		}
	}
	out := make([]KeyReport, 0, len(order))
	for _, sc := range order {
		out = append(out, *byScope[sc])
	}
	return out
}

// sealablePlaceForKeys — умеет ли место, где лежит свежайшая версия каждого
// ключа, писать его под конвертом. Один RLock на весь набор (тот же приём, что
// в catForKeys: поштучный захват на скане всего стора стоил бы дороже самой
// работы).
func (lvs *LeveledVectorStore) sealablePlaceForKeys(keys []string) []bool {
	out := make([]bool, len(keys))
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	for i, key := range keys {
		out[i] = lvs.factSealablePlaceLocked(key)
	}
	return out
}

// factSealablePlaceLocked — умеет ли МЕСТО хранения свежайшей версии ключа
// писать его под конверт. Обход повторяет провенанс factCatAttrLocked (активная
// дельта → tombstone-маска → flushing новее-первым → сегменты свежее-первым):
// спрашивать надо у той версии, которую вернёт чтение, иначе отчёт говорил бы о
// перекрытой копии. Вызывать под lvs.mu.
//
// Дельта и flushing → true, и это не поблажка: SaveBinary их не пишет вовсе
// («Delta НЕ сохраняется», leveled_store.go:2198), а единственная их
// персистентная форма — журнал, который запечатывается sealValue на границе
// записи независимо от того, в какой тип сегмента факт осядет позже. То есть
// прямо сейчас байты такого факта лежат под конвертом. Риск для них не
// текущий, а будущий: он материализуется на заморозке, и ровно тогда этот же
// отчёт его и покажет.
//
// Ключ не найден (стёрт, никогда не существовал) → false: отчёт о том, чего
// нет, не должен добавлять покрытия.
func (lvs *LeveledVectorStore) factSealablePlaceLocked(key string) bool {
	if lvs.delta != nil {
		if _, ok := lvs.delta.GetAttrs(key); ok {
			return true
		}
	}
	if t := lvs.tombstones.Load(); t != nil {
		if _, deleted := (*t)[key]; deleted {
			return false
		}
	}
	for i := len(lvs.flushing) - 1; i >= 0; i-- {
		if _, ok := lvs.flushing[i].GetAttrs(key); ok {
			return true
		}
	}
	for _, level := range lvs.levels {
		for pos := len(level) - 1; pos >= 0; pos-- {
			seg := level[pos]
			switch s := seg.(type) {
			case *frozenSegment:
				if _, ok := s.fg.keys.find(key); ok {
					return segmentSealsScopes(seg)
				}
			case *frozenSQSegment:
				if _, ok := s.fg.keys.find(key); ok {
					return segmentSealsScopes(seg)
				}
			case *hnswSegment:
				s.mu.RLock()
				for id, k := range s.keys {
					if k == key && s.g.nodes[id].Alive {
						s.mu.RUnlock()
						return segmentSealsScopes(seg)
					}
				}
				s.mu.RUnlock()
			default:
				// Незнакомый тип: найти в нём ключ мы не умеем, значит и
				// поручиться за него не можем. Молчаливое «покрыт» здесь было
				// бы тем самым fail-open, против которого весь файл.
				return false
			}
		}
	}
	return false
}
