package main

// Применение записей журнала к состоянию — БОЕВОЙ путь восстановления.
//
// Вынесен из main отдельным типом не ради красоты. Пока это было замыкание
// внутри main(), проверить его можно было только ЗЕРКАЛОМ в тесте, а зеркало
// расходится с оригиналом молча — и расходится
// оно в коде, который работает ровно один раз: при восстановлении после
// аварии, когда проверять уже поздно. Теперь main и тесты зовут один и тот же
// код.
//
// Порядок применения и все гейты сохранены дословно; смысловых изменений при
// выносе не делалось.

import (
	"encoding/binary"
	"log/slog"
	"time"

	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/kv"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// walApplier — состояние, к которому применяются записи, плюс счётчики
// восстановления. Живёт ровно один старт.
type walApplier struct {
	s       *kv.Store
	ttl     *store.TTLManager
	vec     *vector.LeveledVectorStore
	zsetReg *zset.ZSetRegistry

	// graphLoaded/vecWatermark — вход shouldSkipVecReplay: покрыт ли
	// векторный эффект записи уже загруженным бинарным снапшотом.
	graphLoaded  bool
	vecWatermark uint64

	restored    int
	vecRestored int
}

// skipVec решает, пропустить ли ВЕКТОРНЫЙ эффект записи как уже вошедший в
// снапшот. Применяется и к OpVSim*, и к каскадным OpDel/OpExpire.
func (a *walApplier) skipVec(entry wal.Entry, isFromSnapshot bool) bool {
	return shouldSkipVecReplay(entry, isFromSnapshot, a.graphLoaded, a.vecWatermark)
}

// apply применяет одну запись журнала или снапшота.
func (a *walApplier) apply(entry wal.Entry, isFromSnapshot bool) {
	switch entry.Op {
	case wal.OpSet:
		a.s.Set(entry.Key, entry.Value)
		a.restored++
	case wal.OpDel:
		a.s.Del(entry.Key)
		// Векторный эффект гейтуем watermark'ом: старый DEL (LSN ≤ watermark),
		// уже отражённый в снапшоте, не должен удалять вектор, воскрешённый
		// более поздним re-add (который тоже в снапшоте).
		if !a.skipVec(entry, isFromSnapshot) {
			a.vec.Delete(entry.Key)
		}
		a.ttl.OnDelete(entry.Key)
		a.restored++
	case wal.OpExpire:
		if len(entry.Value) == 8 {
			expiresAt := time.Unix(0, int64(binary.BigEndian.Uint64(entry.Value)))
			remaining := time.Until(expiresAt)
			if remaining > 0 {
				a.ttl.Set(entry.Key, remaining)
			} else {
				a.s.Del(entry.Key)
				if !a.skipVec(entry, isFromSnapshot) {
					a.vec.Delete(entry.Key) // Также удаляем вектор, если ключ просрочен в оффлайне
				}
				a.ttl.OnDelete(entry.Key)
			}
		}
		a.restored++
	case wal.OpPersist:
		a.ttl.Remove(entry.Key)
		a.restored++
	case wal.OpVSimAdd:
		// Пропускаем, если операция уже в снапшоте (snapshot.wal при graphLoaded
		// или wal_*.log с LSN ≤ watermark) — иначе дубль вектора после рестарта.
		if a.skipVec(entry, isFromSnapshot) {
			return
		}
		vec := vector.DeserializeVector(entry.Value)
		if err := a.vec.Add(entry.Key, vec); err != nil {
			slog.Warn("failed to restore vector", "key", entry.Key, "err", err)
		}
		a.vecRestored++
		a.restored++
	case wal.OpVSimAddAttrs:
		// Вектор + атрибуты (P0-4): attrs/tenant восстанавливаются через
		// AddWithAttrs, а не теряются как при голом Add.
		if a.skipVec(entry, isFromSnapshot) {
			return
		}
		vec, attrs, err := vector.DeserializeVectorWithAttrs(entry.Value)
		if err != nil {
			slog.Warn("failed to decode vector+attrs", "key", entry.Key, "err", err)
			return
		}
		if err := a.vec.AddWithAttrs(entry.Key, vec, attrs); err != nil {
			slog.Warn("failed to restore vector+attrs", "key", entry.Key, "err", err)
		}
		a.vecRestored++
		a.restored++
	case wal.OpVSimAddDoc:
		// Вектор + атрибуты + термы текста (BM25, шаг 5). Реплей через
		// AddDocTerms: в журнале УЖЕ готовые термы, перетокенизация запрещена
		// (бит-в-бит воспроизводимость независимо от версии стеммера).
		if a.skipVec(entry, isFromSnapshot) {
			return
		}
		vec, attrs, terms, err := vector.DeserializeVectorWithDoc(entry.Value)
		if err != nil {
			slog.Warn("failed to decode vector+doc", "key", entry.Key, "err", err)
			return
		}
		if err := a.vec.AddDocTerms(entry.Key, vec, attrs, terms); err != nil {
			slog.Warn("failed to restore vector+doc", "key", entry.Key, "err", err)
		}
		a.vecRestored++
		a.restored++
	case wal.OpVSimAddDocBatch:
		// Пакет доков одной записью (журналы прежних версий): один CRC на
		// всю запись — DeserializeDocBatch либо отдаёт все доки, либо ошибку;
		// частичное применение невозможно по построению.
		if a.skipVec(entry, isFromSnapshot) {
			return
		}
		docs, err := vector.DeserializeDocBatch(entry.Value)
		if err != nil {
			slog.Warn("failed to decode doc batch", "key", entry.Key, "err", err)
			return
		}
		for _, d := range docs {
			if err := a.vec.AddDocTerms(d.Key, d.Vec, d.Attrs, d.Terms); err != nil {
				slog.Warn("failed to restore doc from batch", "key", d.Key, "err", err)
			}
		}
		a.vecRestored += len(docs)
		a.restored++
	case wal.OpVSimDel:
		if a.skipVec(entry, isFromSnapshot) {
			return
		}
		a.vec.Delete(entry.Key)
		a.restored++
	case wal.OpZAdd:
		if len(entry.Value) >= 8 {
			score, member := zset.DecodeZAddValue(entry.Value)
			a.zsetReg.ZAdd(entry.Key, score, member)
			a.restored++
		}
	case wal.OpZRem:
		member := string(entry.Value)
		a.zsetReg.ZRem(entry.Key, member)
		a.restored++
	}
}

// shouldSkipVecReplay решает, пропустить ли ВЕКТОРНЫЙ эффект WAL-записи при
// recovery как уже отражённый в снапшоте graph_leveled.bin. Из snapshot.wal —
// пропускаем, если граф загружен из бинарного снапшота; из wal_*.log — если
// LSN ≤ vecWatermark (запись уже в снапшоте). Применяется КО ВСЕМ записям,
// меняющим вектор: OpVSimAdd/Attrs/Doc/Del И каскадным OpDel/OpExpire (KV-DEL, что
// также удаляет вектор) — иначе старый DEL с LSN ≤ watermark удалил бы вектор,
// воскрешённый более поздним re-add, уже присутствующий в снапшоте.
func shouldSkipVecReplay(entry wal.Entry, isFromSnapshot, graphLoaded bool, vecWatermark uint64) bool {
	if isFromSnapshot {
		return graphLoaded
	}
	return entry.LSN <= vecWatermark
}
