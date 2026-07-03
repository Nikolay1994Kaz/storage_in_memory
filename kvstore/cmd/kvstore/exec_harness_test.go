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

// ─── Тест-харнесс для executeCommand ───────────────────────────────────────
//
// executeCommand — центральный диспетчер команд сервера (~950 строк, все
// KV/TTL/zset/vector команды), но исторически имел 0% покрытия: логика жила
// в cmd/kvstore/main.go и дёргалась только через реальный сокет. Это чистая
// функция (все зависимости — параметрами, ответ пишется в cs.Buf), поэтому её
// можно гонять напрямую: собираем реальные хранилища, шлём команду, парсим
// RESP-ответ с провода.
//
// recordingConn/testAddr переиспользуются из m1_pipeline_writer_test.go
// (тот же пакет main).

// execEnv — собранное окружение диспетчера: реальные store/wal/ttl/zset/vector,
// один переиспользуемый ConnState. Всё чистится через t.Cleanup.
type execEnv struct {
	t    *testing.T
	s    *tcmalloc.TCMallocStore
	bw   *wal.BatchWAL
	ttl  *store.TTLManager
	hub  *pubsub.Hub
	wasm computeRuntime
	vec  vector.VectorIndex
	zset *zset.ZSetRegistry
	cs   *server.ConnState

	iterateAll  func(fn func(op byte, key string, value []byte))
	saveVectors func() error
}

func newExecEnv(t *testing.T) *execEnv {
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
		Distance:    vector.EuclideanDistance,
		Allocator:   s,
		NumBuilders: 1,
	})
	zreg := zset.New(s)

	e := &execEnv{
		t:           t,
		s:           s,
		bw:          bw,
		ttl:         ttl,
		hub:         hub,
		wasm:        nopCompute{},
		vec:         vec,
		zset:        zreg,
		iterateAll:  func(func(op byte, key string, value []byte)) {}, // COMPACT-тесты подменяют
		saveVectors: func() error { return nil },
	}

	t.Cleanup(func() {
		ttl.Stop()
		_ = bw.Close()
		s.Close()
	})
	return e
}

// do шлёт одну команду в диспетчер и возвращает распарсенный RESP-ответ.
//
// Каждый вызов получает свежий recordingConn+Buf (ответы не накапливаются между
// командами), но переиспользует остальное состояние окружения — так тесты видят
// накопленный эффект предыдущих команд (SET → GET).
func (e *execEnv) do(cmd string, args ...string) protocol.Value {
	e.t.Helper()
	rc := &recordingConn{}
	e.cs = &server.ConnState{Conn: rc, Buf: server.NewConnBuf(rc), WorkerID: 0, Authenticated: true}

	byteArgs := make([][]byte, len(args))
	for i, a := range args {
		byteArgs[i] = []byte(a)
	}

	executeCommand(e.s, e.bw, e.ttl, e.hub, nil, e.wasm, e.vec, e.zset,
		nil, nil, e.iterateAll, e.saveVectors, e.cs, cmd, byteArgs)

	if err := e.cs.Buf.Flush(); err != nil {
		e.t.Fatalf("Buf.Flush(%s): %v", cmd, err)
	}
	return e.parse(rc.snapshot())
}

// parse читает ровно один RESP-ответ из сырых байтов, снятых с провода.
func (e *execEnv) parse(raw []byte) protocol.Value {
	e.t.Helper()
	if len(raw) == 0 {
		e.t.Fatalf("пустой ответ на проводе")
	}
	v, err := protocol.NewReader(bytes.NewReader(raw)).Read()
	if err != nil {
		e.t.Fatalf("парс RESP-ответа %q: %v", raw, err)
	}
	return v
}

// ─── ассерт-хелперы (читаемость тестов) ────────────────────────────────────

func (e *execEnv) wantSimple(v protocol.Value, want string) {
	e.t.Helper()
	if v.Typ != '+' || v.Str != want {
		e.t.Fatalf("ждали +%s, got typ=%q str=%q", want, v.Typ, v.Str)
	}
}

func (e *execEnv) wantErrPrefix(v protocol.Value, prefix string) {
	e.t.Helper()
	if v.Typ != '-' {
		e.t.Fatalf("ждали ошибку с префиксом %q, got typ=%q str=%q", prefix, v.Typ, v.Str)
	}
	if !bytes.HasPrefix([]byte(v.Str), []byte(prefix)) {
		e.t.Fatalf("ждали ошибку с префиксом %q, got %q", prefix, v.Str)
	}
}

func (e *execEnv) wantInt(v protocol.Value, want int) {
	e.t.Helper()
	if v.Typ != ':' || v.Num != want {
		e.t.Fatalf("ждали :%d, got typ=%q num=%d", want, v.Typ, v.Num)
	}
}

func (e *execEnv) wantBulk(v protocol.Value, want string) {
	e.t.Helper()
	if v.Typ != '$' || v.Str != want {
		e.t.Fatalf("ждали bulk %q, got typ=%q str=%q", want, v.Typ, v.Str)
	}
}
