package main

// Состояние, которое уходит в журнал и снапшот помимо обработчиков команд:
// снимок KV-состояния для компакции и TTL-эвиктор.

import (
	"encoding/binary"
	"strings"

	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/kv"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// snapshotIterate переснимает всё KV-состояние, живущее только в реплее WAL,
// в snapshot.wal при компакции. Вынесено из iterateAll ради тестируемости
// (стражи S1/S2): компакция удаляет старые WAL, поэтому всё, что не попадёт
// сюда, теряется после рестарта.
//
//   - обычные KV-ключи → OpSet;
//   - ключи "__zidx:" ПРОПУСКАЕМ — это пространство имён прежнего обратного
//     индекса zset, когда он жил в общем KV. Теперь индекс внутри реестра и
//     такие ключи не создаются, но снапшот, снятый до фикса S2, мог их
//     принести: после первой компакции они выметаются;
//   - TTL → OpExpire с абсолютным временем смерти (S1): иначе после компакции
//     ключи с TTL становятся бессмертными (correctness + утечка памяти);
//   - zset → OpZAdd (score+member) (S2): реплей восстанавливает и дерево, и
//     обратный индекс member → score.
func snapshotIterate(
	s *kv.Store,
	ttl *store.TTLManager,
	zsetReg *zset.ZSetRegistry,
	fn func(op byte, key string, value []byte),
) {
	s.ForEach(func(key string, value []byte) {
		if strings.HasPrefix(key, "__zidx:") {
			return
		}
		fn(wal.OpSet, key, value)
	})

	ttl.ForEach(func(key string, expiresAtUnixNano int64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(expiresAtUnixNano))
		fn(wal.OpExpire, key, b[:])
	})

	zsetReg.ForEachSet(func(setName string) {
		zsetReg.ForEach(setName, func(score float64, member string) {
			fn(wal.OpZAdd, setName, zset.EncodeZAddValue(score, member))
		})
	})
}

// ═══════════════════════════════════════════════════
// compositeEvictor — TTL-expire удаляет и KV, и вектор
// ═══════════════════════════════════════════════════
//
// Когда TTLManager обнаруживает просроченный ключ, он вызывает Del(key).
// compositeEvictor выполняет три действия:
//
//  1. Удаляет значение из KV Store
//  2. Если ключ имеет ассоциированный вектор в HNSW графе — удаляет его,
//     освобождая память в VectorArena и TCMalloc (neighbors блок)
//  3. Записывает OpVSimDel в WAL, чтобы удаление пережило рестарт
//
// Без этого векторы с TTL «утекали» — KV-ключ удалялся,
// а вектор оставался навечно в графе HNSW, занимая RAM.
type compositeEvictor struct {
	kvs *kv.Store
	vec vector.VectorIndex
	wal *wal.BatchWAL
}

func (e *compositeEvictor) Del(key string) bool {
	kvDeleted := e.kvs.Del(key)

	vecDeleted := e.vec.Delete(key)
	if vecDeleted {
		e.wal.Write(wal.Entry{Op: wal.OpVSimDel, Key: key})
	}

	return kvDeleted || vecDeleted
}
