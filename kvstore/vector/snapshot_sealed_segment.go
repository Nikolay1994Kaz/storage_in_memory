package vector

// Врезка документной секции в бинарный снапшот (формат v8).
//
// Как устроен v8 у frozen-сегмента:
//
//	[segType][граф — векторы фактов ЗАНУЛЕНЫ][segMeta без фактов][segText без
//	фактов][документная секция: факты группами по скоупу под конвертом]
//
// То есть форматы графа, колонок и текстового слоя НЕ меняются — меняется их
// наполнение: всё, что принадлежит скоупу, из них изъято и уехало в секцию.
// Так правка не трогает разбор трёх устоявшихся форматов (там ломается тихо),
// а стирание получает то, что ему нужно: байты факта лежат в блоке, который
// без ключа скоупа не открывается.
//
// На загрузке слои пересобираются из объединённого набора документов теми же
// buildSegmentAttrs/buildSegmentText, что зовёт флаш дельты. Цена измерена до
// кода: 18–20 мс на 10k фактов (snapshot_rebuild_cost_bench_test).

// frozenEntries разворачивает frozen-сегмент в документы — тем же способом,
// каким это делает merge (leveled_store.go, mergeSegmentsWithAllocator):
// вектор из слэба по позиции, атрибуты через decodeAt, термы через
// decodeTerms. Позиция сохраняется: дыры (удалённые ключи) остаются дырами,
// потому что все слои адресуются позицией.
func frozenEntries(s *frozenSegment) []DeltaEntry {
	fg := s.fg
	decTerms := s.text.decodeTerms()
	entries := make([]DeltaEntry, fg.n)
	for i := 0; i < fg.n; i++ {
		key := fg.keys.view(i)
		if key == "" {
			continue // дыра: нода удалена
		}
		var terms []TermTF
		if i < len(decTerms) {
			terms = decTerms[i]
		}
		entries[i] = DeltaEntry{
			Key:   fg.keys.clone(i),
			Vec:   fg.data[i*fg.dim : (i+1)*fg.dim],
			Attrs: s.attrs.decodeAt(i),
			Terms: terms,
		}
	}
	return entries
}

// splitBySealed делит документы на те, что уезжают под конверт (есть scope),
// и публичный остаток. Возвращает маску запечатанных позиций и КОПИЮ набора,
// в которой запечатанные документы обнулены, — из неё строятся публичные слои.
//
// Ключ в публичном наборе СОХРАНЯЕТСЯ: без него граф на загрузке не знал бы,
// какая позиция кому принадлежит, а мёртвый документ нечем было бы пометить
// в tombstones.
func splitBySealed(entries []DeltaEntry, scopeOf func(DeltaEntry) string) (mask []bool, public []DeltaEntry, anySealed bool) {
	mask = make([]bool, len(entries))
	public = make([]DeltaEntry, len(entries))
	for i, e := range entries {
		if e.Key == "" || scopeOf(e) == "" {
			public[i] = e
			continue
		}
		mask[i] = true
		anySealed = true
		public[i] = DeltaEntry{Key: e.Key} // позиция и ключ остаются, содержание — нет
	}
	return mask, public, anySealed
}

// sealedOnly оставляет только запечатываемые документы: остальные позиции
// обнуляются, и документная секция их пропустит (она пропускает пустой ключ).
func sealedOnly(entries []DeltaEntry, mask []bool) []DeltaEntry {
	out := make([]DeltaEntry, len(entries))
	for i := range entries {
		if i < len(mask) && mask[i] {
			out[i] = entries[i]
		}
	}
	return out
}

// vmemScopeOf — правило принадлежности документа скоупу. Единственная точка,
// где снапшот решает, что считать фактом памяти: атрибут scope. Не VMEM-доки
// его не имеют, шифровать их нечем и незачем.
func vmemScopeOf(e DeltaEntry) string {
	return e.Attrs.Cat[vmemAttrScope]
}

// mergeSealedIntoEntries вписывает восстановленные из секции документы обратно
// по их позициям и возвращает объединённый набор для пересборки слоёв.
//
// Мёртвые (скоуп стёрт) не восстанавливаются вовсе: их позиции остаются
// пустыми, вектор в графе — нулевым, а ключи вызывающий кладёт в tombstones.
func mergeSealedIntoEntries(public, sealed []DeltaEntry) []DeltaEntry {
	out := make([]DeltaEntry, len(public))
	copy(out, public)
	for i := range sealed {
		if i >= len(out) || sealed[i].Key == "" {
			continue
		}
		out[i] = sealed[i]
	}
	return out
}

// restoreSealedVectors вписывает векторы восстановленных документов обратно в
// слэб графа: на диске они были занулены. Позиции стёртых остаются нулевыми.
func restoreSealedVectors(fg *FrozenGraph, entries []DeltaEntry) {
	for i, e := range entries {
		if e.Key == "" || len(e.Vec) != fg.dim || i >= fg.n {
			continue
		}
		copy(fg.data[i*fg.dim:(i+1)*fg.dim], e.Vec)
	}
}

// hasAnyDoc — есть ли в наборе хоть один восстановленный документ. Пустой
// набор означает «секции не было или в ней ничего не осталось»: пересобирать
// слои не нужно, и лишняя работа на загрузке не делается.
func hasAnyDoc(entries []DeltaEntry) bool {
	for i := range entries {
		if entries[i].Key != "" {
			return true
		}
	}
	return false
}

// SetSnapshotCrypto подключает шифрование бинарного снапшота. Ставится один
// раз при старте, до загрузки и до приёма запросов: снапшот пишется и
// читается уже с ним, а менять его на живом сторе значило бы получить файл,
// половина которого под одним режимом, половина под другим.
//
// nil — снапшот пишется как раньше (движок без -encrypt-at-rest).
func (lvs *LeveledVectorStore) SetSnapshotCrypto(c *SnapshotCrypto) {
	lvs.snapshotCrypto = c
}
