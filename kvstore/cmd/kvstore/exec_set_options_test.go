package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/kv"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// TestSetRejectedOptionsWriteNothing — SET с неверными опциями отвечает
// ошибкой и НИЧЕГО не меняет: ни значение в памяти, ни срок жизни, ни журнал.
//
// Прежде значение записывалось в журнал и в стор ДО разбора EX, а ошибка
// приходила после: клиент получал «ERR invalid expire time», хотя ключ уже
// перезаписан — и со старым сроком жизни. Неизвестные опции (PX, NX, лишний
// хвост) молча игнорировались, и SET k v PX 100 отвечал OK, не ставя срок.
func TestSetRejectedOptionsWriteNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	bw := wal.NewBatchWAL(w)
	s := kv.New()
	ttl := store.NewTTLManager(s)
	defer ttl.Stop()
	vec := vector.NewLeveledVectorStore(vector.LeveledConfig{Distance: vector.EuclideanDistance, NumBuilders: 1})
	defer vec.Close()
	c := &commands{kv: s, wal: bw, ttl: ttl, hub: pubsub.NewHub(), vec: vec, zsets: zset.New(s.MemoryCounter()),
		iterateAll: func(func(byte, string, []byte)) {}, saveVectors: func() error { return nil }}

	do := func(args ...string) protocol.Value {
		t.Helper()
		rc := &recordingConn{}
		cs := &server.ConnState{Conn: rc, Buf: server.NewConnBuf(rc), Authenticated: true}
		byteArgs := make([][]byte, len(args)-1)
		for i, a := range args[1:] {
			byteArgs[i] = []byte(a)
		}
		c.execute(cs, args[0], byteArgs)
		if err := cs.Buf.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		v, err := protocol.NewReader(bytes.NewReader(rc.snapshot())).Read()
		if err != nil {
			t.Fatalf("parse reply: %v", err)
		}
		return v
	}

	if v := do("SET", "k", "old", "EX", "100"); v.Typ != '+' {
		t.Fatalf("SET k old EX 100: %+v", v)
	}
	for _, bad := range [][]string{
		{"SET", "k", "new", "EX", "abc"},
		{"SET", "k", "new", "EX", "0"},
		{"SET", "k", "new", "EX", "-5"},
		{"SET", "k", "new", "EX"},
		{"SET", "k", "new", "PX", "100"},
		{"SET", "k", "new", "NX"},
		{"SET", "k", "new", "EX", "10", "extra"},
		// Срок, не помещающийся в time.Duration: прежде умножение на секунду
		// переполнялось в отрицательный срок.
		{"SET", "k", "new", "EX", "9999999999999"},
		{"EXPIRE", "k", "9999999999999"},
	} {
		if v := do(bad...); v.Typ != '-' {
			t.Errorf("%v: ждали ошибку, получили %q %q", bad, v.Typ, v.Str)
		}
	}

	if got, ok := s.Get("k"); !ok || string(got) != "old" {
		t.Fatalf("после отклонённых SET значение %q, ожидалось old", got)
	}
	if d := ttl.TTL("k"); d < 90*time.Second {
		t.Fatalf("после отклонённых SET срок жизни %v, ожидался прежний ~100s", d)
	}

	if err := bw.Close(); err != nil {
		t.Fatalf("bw.Close: %v", err)
	}
	_, entries, err := wal.ReadFile(path)
	if err != nil {
		t.Fatalf("wal.ReadFile: %v", err)
	}
	for _, e := range entries {
		if e.Op == wal.OpSet && string(e.Value) == "new" {
			t.Fatal("отклонённый SET попал в журнал: после рестарта значение воскресло бы")
		}
		if e.Op == wal.OpPersist {
			t.Fatal("отклонённый SET снял срок жизни в журнале")
		}
	}
}
