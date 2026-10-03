package main

// Состояние, которое уходит в журнал и снапшот помимо обработчиков команд:
// снимок KV-состояния для компакции, TTL-эвиктор и загрузка снапшота векторов
// при старте.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
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

// loadVectorSnapshot загружает graph_leveled.bin, если он есть.
//
// Файл, который есть, но не читается, — отказ старта. После компакции это
// единственная копия векторов старше неё: их записи в WAL удалены, а
// snapshot.wal векторов не хранит. Прежде сервер писал «will rebuild from WAL»
// и стартовал без них — то есть молча терял данные, а следующая компакция
// перезаписывала файл неполным состоянием.
func loadVectorSnapshot(vec *vector.LeveledVectorStore, path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("vector snapshot %s: %w", path, err)
	}
	defer f.Close()
	slog.Info("loading vector snapshot", "path", path)
	if err := vec.LoadBinary(f); err != nil {
		return false, fmt.Errorf("vector snapshot %s is unreadable (%w). It is the only copy of vectors older than "+
			"the last compaction — their WAL records were deleted after it — so starting without it would silently "+
			"lose them. Restore the data directory from a backup or with -ship-restore; to start anyway and accept "+
			"the loss, move the file away", path, err)
	}
	slog.Info("vector snapshot loaded")
	return true, nil
}
