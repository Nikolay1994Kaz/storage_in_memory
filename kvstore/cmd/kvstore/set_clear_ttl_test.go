package main

import (
	"path/filepath"
	"testing"
	"time"

	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/internal/wal"
)

// TestSetClearsTTL — страж zset-M: голый SET снимает прежний TTL (Redis без
// KEEPTTL). До фикса ttl оставался → новое значение умирало по старому таймеру.
//
// Проверяем ОБА эффекта setClearTTL (вызывается в else-ветке SET-обработчика):
//  1. runtime: TTL снят;
//  2. durability: OpPersist записан в WAL (иначе прежний OpExpire воскресит TTL
//     при реплее/компакции) — но ТОЛЬКО когда TTL реально был (без WAL-спама).
func TestSetClearsTTL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	bw := wal.NewBatchWAL(w)

	s := tcmalloc.NewTCMallocStore(1)
	defer s.Close()
	ttl := store.NewTTLManager(tcmalloc.NewEvictor(s))
	defer ttl.Stop()

	// Ключ с TTL — как после SET k v EX 100.
	ttl.Set("withttl", time.Hour)
	if ttl.TTL("withttl") <= 0 {
		t.Fatal("предусловие: TTL не выставлен")
	}

	// Голый SET поверх ключа с TTL → снять таймер.
	setClearTTL(ttl, bw, "withttl")
	if d := ttl.TTL("withttl"); d > 0 {
		t.Fatalf("zset-M: голый SET не снял TTL: осталось %v", d)
	}

	// Голый SET по ключу БЕЗ TTL → не должен писать OpPersist (без WAL-спама).
	setClearTTL(ttl, bw, "notimer")

	if err := bw.Close(); err != nil {
		t.Fatalf("bw.Close: %v", err)
	}

	_, entries, err := wal.ReadFile(path)
	if err != nil {
		t.Fatalf("wal.ReadFile: %v", err)
	}

	persistFor := map[string]int{}
	for _, e := range entries {
		if e.Op == wal.OpPersist {
			persistFor[e.Key]++
		}
	}
	if persistFor["withttl"] != 1 {
		t.Fatalf("zset-M durability: OpPersist('withttl')=%d, want 1 "+
			"(после рестарта прежний OpExpire воскресит TTL)", persistFor["withttl"])
	}
	if persistFor["notimer"] != 0 {
		t.Fatalf("zset-M: лишний OpPersist('notimer')=%d — WAL-спам на SET без TTL",
			persistFor["notimer"])
	}
}
