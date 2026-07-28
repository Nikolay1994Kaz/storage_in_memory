package vector

// Криптостирание скоупа: половина в памяти.
//
// Уничтожение KEK (internal/keyring) делает нечитаемыми ВСЕ персистентные
// копии скоупа — WAL, снапшоты, отгруженные архивы и любое восстановление из
// них. Но живой процесс держит факты в памяти открытым текстом (осознанное
// ограничение, записано в docs/VMEM_DESIGN.md), поэтому без этой половины
// стёртое оставалось бы видимым до ближайшего перезапуска.
//
// Порядок операции задан и важен: СНАЧАЛА память, ПОТОМ ключ. Тогда в любой
// момент отказа выполняется «в памяти нет» ⊇ «на диске нет». Обратный порядок
// оставляет окно, в котором объявленное стёртым ещё отдаётся из RECALL.

// CollectScope — фаза СКАНА: id всех VMEM-фактов скоупа. Отдельным проходом,
// без удержания замка на время чтения атрибутов (ForEach держит RLock, а
// чтение атрибутов берёт его же — рекурсивный RLock с ждущим писателем даёт
// дедлок; тот же приём, что в ProvenanceCoverage).
//
// Вынесена отдельно от приговора намеренно: двухфазную операцию обязан
// проверять тест, вызывающий фазы РАЗДЕЛЬНО и меняющий состояние между ними —
// иначе проверяется только скан (урок VMEM.BACKFILL, 27.07).
func (lvs *LeveledVectorStore) CollectScope(scope string) []string {
	if scope == "" {
		return nil
	}
	var keys []string
	lvs.ForEach(func(key string, _ []float32) { keys = append(keys, key) })
	if len(keys) == 0 {
		return nil
	}
	scopes := lvs.catForKeys(keys, vmemAttrScope)
	out := make([]string, 0, 16)
	for i, sc := range scopes {
		if sc == scope {
			out = append(out, keys[i])
		}
	}
	return out
}

// ShredScopeKeys — фаза ПРИГОВОРА: удалить перечисленные факты, весь батч под
// одним замком. Возвращает id, которые действительно исчезли.
//
// Принадлежность скоупу перепроверяется ЗДЕСЬ, а не только в скане: между
// фазами факт мог быть перезаписан upsert'ом в другой скоуп, и удалить его по
// устаревшему приговору — значит стереть чужое по чужой команде.
func (lvs *LeveledVectorStore) ShredScopeKeys(scope string, keys []string) []string {
	if scope == "" || len(keys) == 0 {
		return nil
	}
	deleted := make([]string, 0, len(keys))
	lvs.mu.Lock()
	for _, key := range keys {
		sc, ok := lvs.factCatAttrLocked(key, vmemAttrScope)
		if !ok || sc != scope {
			continue // исчез или уехал в другой скоуп между фазами
		}
		if lvs.deleteLocked(key) {
			deleted = append(deleted, key)
		}
	}
	lvs.mu.Unlock()
	return deleted
}

// ShredScope — обе фазы подряд, обычный путь вызова из команды.
func (lvs *LeveledVectorStore) ShredScope(scope string) []string {
	return lvs.ShredScopeKeys(scope, lvs.CollectScope(scope))
}
