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
// =============================================================================

// KeyReport — покрытие ключом одного scope.
type KeyReport struct {
	Scope    string
	Total    int
	Sealed   int // записаны под конвертом → крипто-стираемы
	Unsealed int // атрибута нет: записаны до шифрования → НЕ стираемы
}

// SealedShare — доля фактов, до которых уничтожение ключа реально дотянется.
func (r KeyReport) SealedShare() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Sealed) / float64(r.Total)
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
		if sealed[i] == "1" {
			rep.Sealed++
		} else {
			rep.Unsealed++
		}
	}
	out := make([]KeyReport, 0, len(order))
	for _, sc := range order {
		out = append(out, *byScope[sc])
	}
	return out
}
