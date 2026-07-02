package main

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
)

// TestSnapshotIterate_TTLAndZSetSurviveCompaction — страж S1+S2.
//
// До фикса iterateAll эмитил только OpSet: TTL (OpExpire) и zset-деревья
// (OpZAdd) не переснимались, а компакция удаляет старые WAL → после рестарта
// ключи с TTL становились бессмертными (S1), а zset-деревья исчезали, оставляя
// __zidx-реверс-ключи → split-brain (S2). Тест гоняет полный цикл:
// snapshotIterate → WriteSnapshot → ReadFile → recovery в СВЕЖИЕ структуры и
// проверяет, что TTL и zset воскресли.
func TestSnapshotIterate_TTLAndZSetSurviveCompaction(t *testing.T) {
	dir := t.TempDir()

	// ── Источник: реальные store/ttl/zsetReg ──
	s := tcmalloc.NewTCMallocStore(2)
	defer s.Close()
	ttl := store.NewTTLManager(tcmalloc.NewEvictor(s))
	defer ttl.Stop()
	zsetReg := zset.New(s)

	// Обычный KV-ключ.
	s.Set(0, "plainkey", []byte("plainval"))

	// Ключ с TTL.
	s.Set(0, "sessionkey", []byte("sessionval"))
	ttl.Set("sessionkey", time.Hour)

	// Sorted set с двумя членами (через ZAdd — он пишет и дерево, и __zidx).
	zsetReg.ZAdd(0, "prices", 100.0, "apple")
	zsetReg.ZAdd(0, "prices", 250.0, "banana")

	// ── Собираем эмитируемые записи + прямые проверки эмита ──
	var entries []wal.Entry
	sawZidxOpSet := false
	snapshotIterate(s, ttl, zsetReg, func(op byte, key string, value []byte) {
		cp := append([]byte(nil), value...) // value указывает в span — копируем
		entries = append(entries, wal.Entry{Op: op, Key: key, Value: cp})
		if op == wal.OpSet && strings.HasPrefix(key, "__zidx:") {
			sawZidxOpSet = true
		}
	})

	if sawZidxOpSet {
		t.Fatal("__zidx-ключ эмитится как OpSet — на реплее ранний return ZAdd оставил бы дерево пустым (S2)")
	}
	assertOp(t, entries, wal.OpExpire, "sessionkey", "TTL не переснят (S1)")
	assertOp(t, entries, wal.OpZAdd, "prices", "zset-дерево не переснято (S2)")
	assertOp(t, entries, wal.OpSet, "plainkey", "обычный ключ пропал")

	// ── Пишем реальный snapshot и читаем обратно ──
	sw := wal.NewSnapshotWriter(dir)
	if err := sw.WriteSnapshot(42, func(fn func(byte, string, []byte)) {
		snapshotIterate(s, ttl, zsetReg, fn)
	}); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	_, restored, err := wal.ReadFile(dir + "/snapshot.wal")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// ── Recovery в СВЕЖИЕ структуры (зеркалит applyEntry в main) ──
	s2 := tcmalloc.NewTCMallocStore(2)
	defer s2.Close()
	ttl2 := store.NewTTLManager(tcmalloc.NewEvictor(s2))
	defer ttl2.Stop()
	zset2 := zset.New(s2)

	for _, e := range restored {
		switch e.Op {
		case wal.OpSet:
			s2.Set(0, e.Key, e.Value)
		case wal.OpExpire:
			expiresAt := time.Unix(0, int64(binary.BigEndian.Uint64(e.Value)))
			if rem := time.Until(expiresAt); rem > 0 {
				ttl2.Set(e.Key, rem)
			}
		case wal.OpZAdd:
			score, member := zset.DecodeZAddValue(e.Value)
			zset2.ZAdd(0, e.Key, score, member)
		}
	}

	// ── S1: TTL воскрес ──
	if d := ttl2.TTL("sessionkey"); d <= 0 || d > time.Hour {
		t.Fatalf("S1: TTL sessionkey после recovery = %v, ожидали (0, 1h] — TTL потерян/бессмертен", d)
	}

	// ── S2: zset-дерево воскресло (ZCard/ZRange, не только ZSCORE) ──
	if n := zset2.ZCard("prices"); n != 2 {
		t.Fatalf("S2: ZCard(prices) после recovery = %d, want 2 — дерево не восстановлено (split-brain)", n)
	}
	if got := zset2.ZRangeByScore("prices", 0, 1000); len(got) != 2 {
		t.Fatalf("S2: ZRangeByScore вернул %d членов, want 2", len(got))
	}
	if sc, ok := zset2.ZScore("prices", "banana"); !ok || sc != 250.0 {
		t.Fatalf("S2: ZScore(prices,banana) = %v,%v, want 250,true", sc, ok)
	}

	// Обычный ключ тоже на месте.
	if v, ok := s2.Get("plainkey"); !ok || string(v) != "plainval" {
		t.Fatalf("plainkey после recovery = %q,%v", v, ok)
	}
}

func assertOp(t *testing.T, entries []wal.Entry, op byte, key, msg string) {
	t.Helper()
	for _, e := range entries {
		if e.Op == op && e.Key == key {
			return
		}
	}
	t.Fatalf("%s: не найдена запись op=%d key=%q среди эмитированных", msg, op, key)
}
