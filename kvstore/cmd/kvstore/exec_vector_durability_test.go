package main

import (
	"path/filepath"
	"testing"

	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// 4b/4c. Гейтинг векторного эффекта WAL-записи по watermark. Раньше OpDel/OpExpire
// дёргали vecStore.Delete БЕЗУСЛОВНО → старый DEL (LSN ≤ watermark, уже в снапшоте)
// удалял вектор, воскрешённый более поздним re-add.
func TestShouldSkipVecReplay(t *testing.T) {
	cases := []struct {
		name         string
		lsn          uint64
		fromSnapshot bool
		graphLoaded  bool
		vecWatermark uint64
		wantSkip     bool
	}{
		{"snapshot.wal + graph loaded → skip", 5, true, true, 100, true},
		{"snapshot.wal + graph NOT loaded → apply", 5, true, false, 100, false},
		{"wal LSN ниже watermark → skip (уже в снапшоте)", 50, false, true, 100, true},
		{"wal LSN == watermark → skip (граница включительно)", 100, false, true, 100, true},
		{"wal LSN выше watermark → apply (после снапшота)", 101, false, true, 100, false},
		{"wal без снапшота (watermark=0) → apply", 1, false, false, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldSkipVecReplay(wal.Entry{LSN: c.lsn}, c.fromSnapshot, c.graphLoaded, c.vecWatermark)
			if got != c.wantSkip {
				t.Fatalf("shouldSkipVecReplay=%v, ожидали %v", got, c.wantSkip)
			}
		})
	}
}

// 4a. Add ДО bw.Write: провалившийся Add (напр. dim mismatch) НЕ должен оставлять
// запись в WAL — иначе отравление (реплей WARNING) и, главное, watermark мог бы
// «покрыть» LSN вектора, которого нет в сторе → потеря на рестарте. Проверяем
// прямое следствие переупорядочивания: WAL не содержит записи по провальному ключу.
func TestExec_VsimAddFailDoesNotWriteWAL(t *testing.T) {
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
		Distance: vector.EuclideanDistance, Allocator: s, NumBuilders: 1,
	})
	zreg := zset.New(s)
	defer func() {
		ttl.Stop()
		s.Close()
	}()

	do := func(cmd string, args ...string) {
		rc := &recordingConn{}
		cs := &server.ConnState{Conn: rc, Buf: server.NewConnBuf(rc), WorkerID: 0, Authenticated: true}
		byteArgs := make([][]byte, len(args))
		for i, a := range args {
			byteArgs[i] = []byte(a)
		}
		executeCommand(s, bw, ttl, hub, nil, nopCompute{}, vec, zreg,
			nil, nil, func(func(byte, string, []byte)) {}, func() error { return nil },
			cs, cmd, byteArgs)
		_ = cs.Buf.Flush()
	}

	// dim=3 — ок; dim=2 с тем же store → dimMismatch, Add провалится.
	do("VSIM.ADD", "good", "1", "2", "3")
	do("VSIM.ADD", "bad", "9", "9") // dimMismatch → ошибка, WAL писать нельзя

	// Флашим WAL (Close ждёт flusher) и читаем обратно.
	if err := bw.Close(); err != nil {
		t.Fatalf("bw.Close: %v", err)
	}
	_, entries, err := wal.ReadFile(filepath.Join(dir, "t.wal"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	sawGood, sawBad := false, false
	for _, e := range entries {
		if e.Op == wal.OpVSimAdd {
			switch e.Key {
			case "good":
				sawGood = true
			case "bad":
				sawBad = true
			}
		}
	}
	if !sawGood {
		t.Fatal("успешный VSIM.ADD 'good' не залогирован в WAL")
	}
	if sawBad {
		t.Fatal("провалившийся VSIM.ADD 'bad' попал в WAL (Add должен идти ДО bw.Write)")
	}
}
