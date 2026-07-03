package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// attrEnv — минимальное окружение диспетчера с настраиваемым PartitionAttr
// (newExecEnv хардкодит конфиг без партиции, а тенант-роутинг нужен именно с ней).
type attrEnv struct {
	t  *testing.T
	do func(cmd string, args ...string) protocol.Value
}

func newAttrEnv(t *testing.T, partitionAttr string) *attrEnv {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "t.wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	bw := wal.NewBatchWAL(w)
	s := tcmalloc.NewTCMallocStore(1)
	ttl := store.NewTTLManager(tcmalloc.NewEvictor(s))
	hub := pubsub.NewHub(nil)
	vec := vector.NewLeveledVectorStore(vector.LeveledConfig{
		Distance:      vector.EuclideanDistance,
		Allocator:     s,
		NumBuilders:   1,
		PartitionAttr: partitionAttr,
	})
	zreg := zset.New(s)
	t.Cleanup(func() { ttl.Stop(); _ = bw.Close(); s.Close() })

	do := func(cmd string, args ...string) protocol.Value {
		t.Helper()
		rc := &recordingConn{}
		cs := &server.ConnState{Conn: rc, Buf: server.NewConnBuf(rc), WorkerID: 0, Authenticated: true}
		byteArgs := make([][]byte, len(args))
		for i, a := range args {
			byteArgs[i] = []byte(a)
		}
		executeCommand(s, bw, ttl, hub, nil, nopCompute{}, vec, zreg,
			nil, nil, func(func(byte, string, []byte)) {}, func() error { return nil },
			cs, cmd, byteArgs)
		if err := cs.Buf.Flush(); err != nil {
			t.Fatalf("Buf.Flush(%s): %v", cmd, err)
		}
		v, err := protocol.NewReader(bytes.NewReader(rc.snapshot())).Read()
		if err != nil {
			t.Fatalf("parse %s: %v", cmd, err)
		}
		return v
	}
	return &attrEnv{t: t, do: do}
}

// keysOf извлекает ключи из ответа VSIM.FILTER (массив [key, dist, ...]).
func keysOf(v protocol.Value) []string {
	var out []string
	for i := 0; i < len(v.Array); i += 2 {
		out = append(out, v.Array[i].Str)
	}
	return out
}

func wantOK(t *testing.T, v protocol.Value) {
	t.Helper()
	if v.Typ != '+' || v.Str != "OK" {
		t.Fatalf("ждали +OK, got typ=%q str=%q", v.Typ, v.Str)
	}
}

func wantErr(t *testing.T, v protocol.Value) {
	t.Helper()
	if v.Typ != '-' {
		t.Fatalf("ждали ошибку, got typ=%q str=%q", v.Typ, v.Str)
	}
}

// Базовый columnar-фильтр без партиции: EQ и RANGE отбирают по attr-колонкам.
func TestExec_VsimAddAttrAndFilter(t *testing.T) {
	e := newAttrEnv(t, "")
	wantOK(t, e.do("VSIM.ADDATTR", "a", "CAT", "cat", "books", "NUM", "price", "10", "VEC", "1", "0", "0"))
	wantOK(t, e.do("VSIM.ADDATTR", "b", "CAT", "cat", "toys", "NUM", "price", "50", "VEC", "0", "1", "0"))
	wantOK(t, e.do("VSIM.ADDATTR", "c", "CAT", "cat", "books", "NUM", "price", "90", "VEC", "0", "0", "1"))

	// EQ cat=books → только a, c.
	got := keysOf(e.do("VSIM.FILTER", "10", "EQ", "cat", "books", "VEC", "1", "0", "0"))
	if !sameSet(got, []string{"a", "c"}) {
		t.Fatalf("EQ cat=books: ждали {a,c}, got %v", got)
	}
	// EQ cat=books + RANGE price 0..50 → только a.
	got = keysOf(e.do("VSIM.FILTER", "10", "EQ", "cat", "books", "RANGE", "price", "0", "50", "VEC", "1", "0", "0"))
	if !sameSet(got, []string{"a"}) {
		t.Fatalf("EQ+RANGE: ждали {a}, got %v", got)
	}
}

// Tenant-изоляция через сеть (PartitionAttr): чужой тенант и БЕЗатрибутные
// векторы (голый VSIM.ADD) не должны утекать в тенант-запрос. Это сетевой вход
// в путь, где жил баг #2 tenant-leak.
func TestExec_VsimFilterTenantIsolation(t *testing.T) {
	e := newAttrEnv(t, "tenant")
	wantOK(t, e.do("VSIM.ADDATTR", "t1a", "CAT", "tenant", "t1", "VEC", "1", "0", "0"))
	wantOK(t, e.do("VSIM.ADDATTR", "t1b", "CAT", "tenant", "t1", "VEC", "0", "1", "0"))
	wantOK(t, e.do("VSIM.ADDATTR", "t2a", "CAT", "tenant", "t2", "VEC", "1", "0", "0"))
	// Безатрибутный вектор (голый VSIM.ADD) — без tenant.
	wantOK(t, e.do("VSIM.ADD", "noattr", "1", "0", "0"))
	e.do("VSIM.INFO") // flush в сегменты

	got := keysOf(e.do("VSIM.FILTER", "10", "EQ", "tenant", "t1", "VEC", "1", "0", "0"))
	for _, k := range got {
		if k == "t2a" {
			t.Fatalf("УТЕЧКА: тенант-запрос t1 вернул ключ t2 (%v)", got)
		}
		if k == "noattr" {
			t.Fatalf("УТЕЧКА: тенант-запрос t1 вернул безатрибутный ключ (%v)", got)
		}
	}
	// t1-ключи обязаны находиться.
	if !contains(got, "t1a") {
		t.Fatalf("тенант t1 не нашёл собственный ключ t1a, got %v", got)
	}
}

func TestExec_VsimAttrParseErrors(t *testing.T) {
	e := newAttrEnv(t, "")
	wantErr(t, e.do("VSIM.ADDATTR", "k", "CAT", "cat", "books")) // нет VEC
	wantErr(t, e.do("VSIM.ADDATTR", "k", "BOGUS", "x", "VEC", "1", "0", "0"))
	wantErr(t, e.do("VSIM.ADDATTR", "k", "NUM", "price", "notafloat", "VEC", "1", "0", "0"))
	wantErr(t, e.do("VSIM.FILTER", "10", "EQ", "cat"))          // EQ без значения
	wantErr(t, e.do("VSIM.FILTER", "10", "EQ", "cat", "books")) // нет VEC
	wantErr(t, e.do("VSIM.FILTER", "0", "VEC", "1", "0", "0"))  // K<=0
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range b {
		if !contains(a, x) {
			return false
		}
	}
	return true
}
