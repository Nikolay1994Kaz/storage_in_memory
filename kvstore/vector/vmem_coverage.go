package vector

// =============================================================================
// VMEM — покрытие провенансом: какая доля фактов вообще имеет объявленный
// источник. Метрика честности, а не удобства.
//
// ЗАЧЕМ. Провенанс — фундамент отзыва: карантин отбирает факты ПО ИСТОЧНИКУ,
// и если источник объявлен у меньшинства, вся конструкция восстановления
// декоративна. Заявлять «мы умеем отзывать по происхождению», не зная своей
// доли покрытия, — продавать механизм, который в реальном сторе может не за
// что зацепиться. Поэтому число измеряется, а не подразумевается.
//
// ПОЧЕМУ ЭТО НЕ ВЫРАЖАЕТСЯ ЧЕРЕЗ RECALL. Во-первых, RECALL — это поиск, а не
// скан: он отвечает про запрос, а покрытие — свойство всего scope. Во-вторых,
// у фактов есть ТРИ разных состояния источника, и предикатами доступны лишь
// два: конкретный источник и литеральный "unknown". Третье — атрибута нет
// физически (факты, записанные ДО появления провенанса), — не выражается
// никаким Eq, потому что пустота нефильтруема. Именно эта дыра важнее всех:
// такие факты невидимы для массового отзыва, и знать их долю обязательно.
//
// ЦЕНА. Обход всех живых ключей: O(N) + лукап атрибутов на ключ. Это
// админская/форензическая операция, не горячий путь; на большом сторе она
// стоит как полный скан, и звать её на каждый чих не нужно.
// =============================================================================

// ProvenanceReport — покрытие одного scope.
type ProvenanceReport struct {
	Scope string
	Total int
	// BySource — сколько фактов у каждого источника. Два особых ключа:
	//   vmemSourceUnknown ("unknown") — источник не объявлен при записи, но
	//     штамп есть: такие факты ОТЗЫВАЕМЫ (по этому же значению);
	//   "" — атрибута нет вовсе (записаны до провенанса): НЕ отзываемы
	//     массово, это и есть слепое пятно.
	BySource map[string]int
}

// Declared — доля фактов с объявленным источником (не unknown и не пусто).
func (r ProvenanceReport) Declared() float64 {
	if r.Total == 0 {
		return 0
	}
	declared := r.Total - r.BySource[vmemSourceUnknown] - r.BySource[""]
	return float64(declared) / float64(r.Total)
}

// Revocable — доля фактов, которые массовый отзыв в принципе способен отобрать
// (то есть всё, кроме фактов без атрибута). Это и есть потолок восстановления
// для конкретного стора.
func (r ProvenanceReport) Revocable() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Total-r.BySource[""]) / float64(r.Total)
}

// ProvenanceCoverage — покрытие по каждому scope. scopeEq != "" сужает до
// одного scope. Считаются только VMEM-факты (доки с атрибутом scope);
// посторонние вектора в том же сторе к памяти отношения не имеют.
func (lvs *LeveledVectorStore) ProvenanceCoverage(scopeEq string) []ProvenanceReport {
	// Ключи собираем ОТДЕЛЬНЫМ проходом: ForEach держит lvs.mu.RLock, а чтение
	// атрибутов берёт его же — рекурсивный RLock на RWMutex с ждущим писателем
	// даёт дедлок.
	var keys []string
	lvs.ForEach(func(key string, _ []float32) { keys = append(keys, key) })
	if len(keys) == 0 {
		return nil
	}
	scopes := lvs.catForKeys(keys, vmemAttrScope)
	sources := lvs.catForKeys(keys, vmemAttrSource)

	byScope := make(map[string]*ProvenanceReport)
	order := make([]string, 0, 8)
	for i, sc := range scopes {
		if sc == "" || (scopeEq != "" && sc != scopeEq) {
			continue // не VMEM-факт либо другой scope
		}
		rep, ok := byScope[sc]
		if !ok {
			rep = &ProvenanceReport{Scope: sc, BySource: map[string]int{}}
			byScope[sc] = rep
			order = append(order, sc)
		}
		rep.Total++
		rep.BySource[sources[i]]++
	}
	out := make([]ProvenanceReport, 0, len(order))
	for _, sc := range order {
		out = append(out, *byScope[sc])
	}
	return out
}
