package main

// Тесты шага 7 BM25-спринта: RESP-поверхность текстового пути —
// VSIM.ADDDOC / VSIM.SEARCHTEXT / VSIM.HYBRID. Механика ядра (BM25-оракул,
// RRF, durability термов) покрыта в пакете vector; здесь пиннится сама
// плёнка: парсинг, маршрут до store и write-site WAL (журнал везёт ТЕРМЫ,
// а не сырой текст — реплей не перетокенизирует).

import (
	"path/filepath"
	"reflect"
	"testing"

	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// Живой путь через команды: ADDDOC → SEARCHTEXT, upsert-семантика текста
// (замена и снятие пустым TEXT) — та же, что у attrs.
func TestExec_VsimAddDocAndSearchText(t *testing.T) {
	e := newAttrEnv(t, "")
	wantOK(t, e.do("VSIM.ADDDOC", "d1", "TEXT", "чай зелёный", "VEC", "1", "0", "0"))
	wantOK(t, e.do("VSIM.ADDDOC", "d2", "TEXT", "чай чай чёрный", "VEC", "0", "1", "0"))
	wantOK(t, e.do("VSIM.ADDDOC", "d3", "TEXT", "кофе", "VEC", "0", "0", "1"))

	// d2 бьёт d1 по tf-насыщению; d3 не матчится.
	got := keysOf(e.do("VSIM.SEARCHTEXT", "10", "чай"))
	if !reflect.DeepEqual(got, []string{"d2", "d1"}) {
		t.Fatalf("SEARCHTEXT: ждали [d2 d1], got %v", got)
	}

	// Upsert полностью заменяет текст: d2 теперь про кофе.
	wantOK(t, e.do("VSIM.ADDDOC", "d2", "TEXT", "кофе", "VEC", "0", "1", "0"))
	got = keysOf(e.do("VSIM.SEARCHTEXT", "10", "чай"))
	if !reflect.DeepEqual(got, []string{"d1"}) {
		t.Fatalf("SEARCHTEXT после upsert: ждали [d1], got %v", got)
	}

	// Пустой TEXT снимает текст дока.
	wantOK(t, e.do("VSIM.ADDDOC", "d1", "TEXT", "", "VEC", "1", "0", "0"))
	got = keysOf(e.do("VSIM.SEARCHTEXT", "10", "чай"))
	if len(got) != 0 {
		t.Fatalf("SEARCHTEXT после снятия текста: ждали пусто, got %v", got)
	}
}

// TITLE-буст через команду: терм в заголовке бьёт тот же терм в тексте,
// даже когда в тексте он встречается чаще (tf×3 бедного BM25F).
func TestExec_VsimAddDocTitleBoost(t *testing.T) {
	e := newAttrEnv(t, "")
	wantOK(t, e.do("VSIM.ADDDOC", "titled", "TEXT", "напиток из листьев", "TITLE", "чай", "VEC", "1", "0", "0"))
	wantOK(t, e.do("VSIM.ADDDOC", "plain", "TEXT", "чай чай напиток из листьев", "VEC", "0", "1", "0"))
	got := keysOf(e.do("VSIM.SEARCHTEXT", "10", "чай"))
	if len(got) != 2 || got[0] != "titled" {
		t.Fatalf("SEARCHTEXT: ждали titled первым, got %v", got)
	}
}

// Гибрид через команду: сценарий консенсуса из bm25_hybrid_test.go —
// векторный победитель без текстового матча (z) проигрывает докам,
// найденным обоими путями.
func TestExec_VsimHybrid(t *testing.T) {
	e := newAttrEnv(t, "")
	wantOK(t, e.do("VSIM.ADDDOC", "x", "TEXT", "чай напиток", "VEC", "2", "2", "2"))
	wantOK(t, e.do("VSIM.ADDDOC", "y", "TEXT", "чай чай чай", "VEC", "3", "3", "3"))
	wantOK(t, e.do("VSIM.ADDDOC", "z", "TEXT", "молоко", "VEC", "1", "1", "1"))

	got := keysOf(e.do("VSIM.HYBRID", "10", "TEXT", "чай", "VEC", "0", "0", "0"))
	if !reflect.DeepEqual(got, []string{"y", "x", "z"}) {
		t.Fatalf("HYBRID: ждали [y x z], got %v", got)
	}
}

// Write-site пиннит контракт durability: WAL-запись ADDDOC несёт РЕЗУЛЬТАТ
// токенизации (те же термы, что ушли в дельту), а не сырой текст — реплей
// обязан быть чистой функцией байтов журнала (бит-в-бит независимо от
// версии стеммера).
func TestExec_VsimAddDocWALCarriesTerms(t *testing.T) {
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

	const text = "чай зелёный чай"
	do("VSIM.ADDDOC", "doc", "TEXT", text, "CAT", "lang", "ru", "VEC", "1", "2", "3")

	if err := bw.Close(); err != nil {
		t.Fatalf("bw.Close: %v", err)
	}
	_, entries, err := wal.ReadFile(filepath.Join(dir, "t.wal"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Op != wal.OpVSimAddDoc || e.Key != "doc" {
			continue
		}
		found = true
		gotVec, gotAttrs, gotTerms, err := vector.DeserializeVectorWithDoc(e.Value)
		if err != nil {
			t.Fatalf("DeserializeVectorWithDoc: %v", err)
		}
		if !reflect.DeepEqual(gotVec, []float32{1, 2, 3}) {
			t.Fatalf("vec в WAL: %v", gotVec)
		}
		if gotAttrs.Cat["lang"] != "ru" {
			t.Fatalf("attrs в WAL: %+v", gotAttrs)
		}
		if want := vector.TokenizeDoc(text); !reflect.DeepEqual(gotTerms, want) {
			t.Fatalf("термы в WAL: %+v, ждали %+v", gotTerms, want)
		}
	}
	if !found {
		t.Fatal("в WAL нет OpVSimAddDoc для ключа doc")
	}
}

// Ошибки парсинга поверхности: команда обязана падать ДО каких-либо мутаций.
func TestExec_VsimTextParseErrors(t *testing.T) {
	e := newAttrEnv(t, "")
	wantErr(t, e.do("VSIM.ADDDOC", "k", "VEC", "1", "0", "0"))            // нет TEXT
	wantErr(t, e.do("VSIM.ADDDOC", "k", "TEXT"))                          // TEXT без значения
	wantErr(t, e.do("VSIM.ADDDOC", "k", "TEXT", "чай"))                   // нет VEC
	wantErr(t, e.do("VSIM.ADDDOC", "k", "TEXT", "чай", "TITLE"))          // TITLE без значения
	wantErr(t, e.do("VSIM.ADDDOC", "k", "BOGUS", "x", "VEC", "1"))        // чужой токен
	wantErr(t, e.do("VSIM.ADDDOC", "k", "TEXT", "чай", "VEC", "хи"))      // не float
	wantErr(t, e.do("VSIM.SEARCHTEXT", "0", "чай"))                       // K вне диапазона
	wantErr(t, e.do("VSIM.SEARCHTEXT", "10"))                             // нет запроса
	wantErr(t, e.do("VSIM.HYBRID", "10", "чай", "VEC", "0", "0", "0"))    // нет маркера TEXT
	wantErr(t, e.do("VSIM.HYBRID", "10", "TEXT", "чай", "0", "0", "0"))   // нет маркера VEC
	wantErr(t, e.do("VSIM.HYBRID", "-1", "TEXT", "чай", "VEC", "0", "0")) // K вне диапазона
}
