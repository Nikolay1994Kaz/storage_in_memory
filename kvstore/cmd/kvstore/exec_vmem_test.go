package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// ─── VMEM.* — RESP-слой памяти агентов (шаг 7 VMEM_DESIGN) ─────────────────
//
// Семантика памяти (интервалы/erasure/скоринг) запинена оракулом на
// store-уровне (kvstore/vector/vmem_oracle_test.go, канон ×3 LSM). Здесь
// судится ИМЕННО RESP-слой: парсинг и проброс полей (порог 1 — ленты оракула
// через живой диспетчер против golden-ожиданий), durability (порог 2 —
// рестарт-реплей WAL), атомарность пары supersedes (порог 3 — торн-хвост),
// командные гейты (порог 4 — OOM/fail-stop).
//
// Часы: RESP-хендлер берёт now = time.Now().Unix(), а времена лент —
// абстрактные секунды. Поэтому через RESP гоняются только сценарии БЕЗ TTL
// (валидность иммунна: VALIDFROM/ASOF абсолютные и исторические; erasure-ось
// без TTL — сентинел, любой now проходит), а дефолт-запросы модели шлются
// явным ASOF now-ленты (контракт: дефолт ≡ AS_OF now — та же ветка
// recallFilter). TTL-семантика через RESP не воспроизводима без инъекции
// часов в диспетчер — осознанно оставлена store-уровню.

// Локальные копии JSON-структур сценариев (оригиналы — тестовый файл пакета
// vector, из cmd/kvstore недоступны).
type vmemRespOp struct {
	Op         string   `json:"op"`
	ID         string   `json:"id"`
	Scope      string   `json:"scope"`
	Text       string   `json:"text"`
	Type       string   `json:"type"`
	Importance *float64 `json:"importance"`
	At         int64    `json:"at"`
	TTL        int64    `json:"ttl"`
	Supersedes string   `json:"supersedes"`
}

type vmemRespQuery struct {
	ID          string   `json:"id"`
	Scope       string   `json:"scope"`
	Query       string   `json:"query"`
	Now         int64    `json:"now"`
	AsOf        *int64   `json:"as_of"`
	All         bool     `json:"all"`
	TypeEq      string   `json:"type_eq"`
	Expect      []string `json:"expect"`
	ExpectFirst string   `json:"expect_first"`
}

type vmemRespScenario struct {
	Name    string          `json:"name"`
	Ops     []vmemRespOp    `json:"ops"`
	Queries []vmemRespQuery `json:"queries"`
}

func loadVMEMScenariosRESP(t *testing.T) []vmemRespScenario {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "vector", "testdata", "vmem", "scenarios.json"))
	if err != nil {
		t.Fatalf("scenarios.json: %v", err)
	}
	var all struct {
		Scenarios []vmemRespScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("scenarios.json parse: %v", err)
	}
	return all.Scenarios
}

// vmemDo — REMEMBER одной операции ленты через диспетчер: времена ленты едут
// явным VALIDFROM (историческое, независимо от серверных часов).
func vmemDoRemember(e *execEnv, op vmemRespOp) protocol.Value {
	args := []string{op.Scope, "TEXT", op.Text, "ID", op.ID, "VALIDFROM", strconv.FormatInt(op.At, 10)}
	if op.Type != "" {
		args = append(args, "TYPE", op.Type)
	}
	if op.Importance != nil {
		args = append(args, "IMPORTANCE", strconv.FormatFloat(*op.Importance, 'f', -1, 64))
	}
	if op.Supersedes != "" {
		args = append(args, "SUPERSEDES", op.Supersedes)
	}
	return e.do("VMEM.REMEMBER", args...)
}

// TestExecVMEM_OracleParityRESP — порог 1: ленты оракула (без TTL) через
// живой RESP-диспетчер ×3 LSM-состояния — множества и expect_first совпадают
// с golden, тексты в тройках ответа = дословный якорь последнего upsert'а.
func TestExecVMEM_OracleParityRESP(t *testing.T) {
	scenarios := loadVMEMScenariosRESP(t)
	states := []struct {
		name    string
		flushAt func(n int) int
	}{
		{"delta", func(int) int { return 0 }},
		{"flushed", func(n int) int { return n }},
		{"mixed", func(n int) int { return n / 2 }},
	}
	ran := 0
	for _, sc := range scenarios {
		withTTL := false
		for _, op := range sc.Ops {
			if op.TTL != 0 {
				withTTL = true
			}
		}
		if withTTL {
			continue // TTL-семантика через RESP не воспроизводима (см. шапку)
		}
		ran++
		for _, st := range states {
			t.Run(sc.Name+"/"+st.name, func(t *testing.T) {
				e := newExecEnv(t)
				lvs := e.vec.(*vector.LeveledVectorStore)
				flushAfter := st.flushAt(len(sc.Ops))
				texts := map[string]string{}  // id → дословный якорь последнего upsert
				scopes := map[string]string{} // id → scope (у forget-операций ленты scope не записан)
				for i, op := range sc.Ops {
					switch op.Op {
					case "remember":
						e.wantBulk(vmemDoRemember(e, op), op.ID)
						texts[op.ID] = op.Text
						scopes[op.ID] = op.Scope
					case "forget":
						e.wantInt(e.do("VMEM.FORGET", scopes[op.ID], op.ID), 1)
						delete(texts, op.ID)
					}
					if i+1 == flushAfter {
						lvs.FlushDeltaSync()
					}
				}
				for _, q := range sc.Queries {
					// Дефолт-запрос модели ≡ AS_OF now-ленты (контракт recallFilter):
					// явный ASOF делает исход независимым от серверных часов.
					args := []string{q.Scope, "50", q.Query}
					switch {
					case q.All:
						args = append(args, "ALL")
					case q.AsOf != nil:
						args = append(args, "ASOF", strconv.FormatInt(*q.AsOf, 10))
					default:
						args = append(args, "ASOF", strconv.FormatInt(q.Now, 10))
					}
					if q.TypeEq != "" {
						args = append(args, "TYPE", q.TypeEq)
					}
					v := e.do("VMEM.RECALL", args...)
					if v.Typ != '*' || len(v.Array)%3 != 0 {
						t.Fatalf("%s: ответ не массив троек: %+v", q.ID, v)
					}
					var got []string
					for i := 0; i < len(v.Array); i += 3 {
						id := v.Array[i].Str
						got = append(got, id)
						if text := v.Array[i+2].Str; text != texts[id] {
							t.Errorf("%s: текст %s = %q, якорь ленты %q", q.ID, id, text, texts[id])
						}
					}
					if q.ExpectFirst != "" && (len(got) == 0 || got[0] != q.ExpectFirst) {
						t.Errorf("%s: первый в выдаче %v, golden ожидает %q", q.ID, got, q.ExpectFirst)
					}
					want := append([]string(nil), q.Expect...)
					slices.Sort(got)
					slices.Sort(want)
					if !slices.Equal(got, want) {
						t.Errorf("%s: RESP-выдача %v, golden %v", q.ID, got, want)
					}
				}
			})
		}
	}
	if ran == 0 {
		t.Fatal("ни один сценарий не прогнан — фильтр TTL отфильтровал всё?")
	}
	t.Logf("через RESP прогнано %d/%d сценариев (без TTL) ×3 LSM", ran, len(scenarios))
}

// replayVMEMWAL — мини-реплей WAL-файла в свежую среду: зеркало applyEntry из
// main.go для опов, порождаемых VMEM.* (Set/Del/VSimDel/AddDoc/AddDocBatch).
// applyEntry — замыкание внутри main и из тестов недоступен; при дивергенции
// править ОБА места (свитч в main.go — источник истины).
func replayVMEMWAL(t *testing.T, path string, e *execEnv) {
	t.Helper()
	_, entries, err := wal.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	lvs := e.vec.(*vector.LeveledVectorStore)
	for _, entry := range entries {
		switch entry.Op {
		case wal.OpSet:
			e.s.Set(0, entry.Key, entry.Value)
		case wal.OpDel:
			e.s.Del(0, entry.Key)
			e.vec.Delete(entry.Key)
		case wal.OpVSimDel:
			e.vec.Delete(entry.Key)
		case wal.OpVSimAddDoc:
			vec, attrs, terms, err := vector.DeserializeVectorWithDoc(entry.Value)
			if err != nil {
				t.Fatalf("replay OpVSimAddDoc %s: %v", entry.Key, err)
			}
			if err := lvs.AddDocTerms(entry.Key, vec, attrs, terms); err != nil {
				t.Fatalf("replay AddDocTerms %s: %v", entry.Key, err)
			}
		case wal.OpVSimAddDocBatch:
			docs, err := vector.DeserializeDocBatch(entry.Value)
			if err != nil {
				t.Fatalf("replay OpVSimAddDocBatch %s: %v", entry.Key, err)
			}
			for _, d := range docs {
				if err := lvs.AddDocTerms(d.Key, d.Vec, d.Attrs, d.Terms); err != nil {
					t.Fatalf("replay batch AddDocTerms %s: %v", d.Key, err)
				}
			}
		}
	}
}

// TestExecVMEM_WALRestart — порог 2: рестарт-реплей после обычного REMEMBER,
// пары supersedes и FORGET. Восстановленное состояние эквивалентно: наследник
// виден сейчас, закрытая цель — только через ASOF (интервал пережил реплей),
// стёртый факт и его якорь-текст не воскресли.
func TestExecVMEM_WALRestart(t *testing.T) {
	e1 := newExecEnv(t)
	e1.wantBulk(e1.do("VMEM.REMEMBER", "user:dana", "TEXT", "дедлайн проекта март", "ID", "f1", "VALIDFROM", "1000"), "f1")
	e1.wantBulk(e1.do("VMEM.REMEMBER", "user:dana", "TEXT", "дедлайн проекта май", "ID", "f2", "VALIDFROM", "2000", "SUPERSEDES", "f1"), "f2")
	e1.wantBulk(e1.do("VMEM.REMEMBER", "user:dana", "TEXT", "временный секрет", "ID", "f3", "VALIDFROM", "1000"), "f3")
	e1.wantInt(e1.do("VMEM.FORGET", "user:dana", "f3"), 1)
	if err := e1.bw.Close(); err != nil {
		t.Fatalf("bw.Close: %v", err)
	}

	e2 := newExecEnv(t)
	replayVMEMWAL(t, filepath.Join(e1.dir, "t.wal"), e2)

	recallIDs := func(args ...string) []string {
		v := e2.do("VMEM.RECALL", args...)
		if v.Typ != '*' {
			t.Fatalf("RECALL не массив: %+v", v)
		}
		var ids []string
		for i := 0; i < len(v.Array); i += 3 {
			ids = append(ids, v.Array[i].Str)
		}
		return ids
	}
	// Сейчас (ASOF 3000): только наследник — закрытие интервала пережило реплей
	// атомарной парой.
	if got := recallIDs("user:dana", "10", "дедлайн", "ASOF", "3000"); !slices.Equal(got, []string{"f2"}) {
		t.Fatalf("ASOF 3000: %v, ожидался [f2]", got)
	}
	// Машина времени (ASOF 1500): виден закрытый f1 — история жива.
	if got := recallIDs("user:dana", "10", "дедлайн", "ASOF", "1500"); !slices.Equal(got, []string{"f1"}) {
		t.Fatalf("ASOF 1500: %v, ожидался [f1]", got)
	}
	// Стёртый f3 не воскрес ни фактом, ни якорем.
	if got := recallIDs("user:dana", "10", "секрет", "ALL"); len(got) != 0 {
		t.Fatalf("FORGET не пережил реплей: %v", got)
	}
	if _, ok := e2.s.Get("vmem:f3"); ok {
		t.Fatal("якорь-текст стёртого факта воскрес после реплея")
	}
	// Якоря живых фактов восстановлены.
	if text, ok := e2.s.Get("vmem:f1"); !ok || string(text) != "дедлайн проекта март" {
		t.Fatalf("якорь f1 после реплея: %q, %v", text, ok)
	}
}

// TestExecVMEM_PairAtomicity — порог 3: пара supersedes в WAL неделима.
// Полный файл реплеится в «закрытая цель + наследник»; обрезка хвоста на
// ЛЮБОЙ длине даёт либо пару целиком, либо ничего — состояние «закрыт без
// наследника» / «полтора дока» невоспроизводимо по построению (один CRC).
func TestExecVMEM_PairAtomicity(t *testing.T) {
	e := newExecEnv(t)
	e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "исходный факт", "ID", "f1", "VALIDFROM", "1000"), "f1")
	e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "факт замена", "ID", "f2", "VALIDFROM", "2000", "SUPERSEDES", "f1"), "f2")
	if err := e.bw.Close(); err != nil {
		t.Fatalf("bw.Close: %v", err)
	}
	path := filepath.Join(e.dir, "t.wal")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// pairState: 0 = пары нет, 2 = пара целиком; всё прочее — ложь реплея.
	pairState := func(p string) int {
		_, entries, _ := wal.ReadFile(p) // торн-хвост легален: читаем сколько цело
		n := 0
		for _, entry := range entries {
			if entry.Op != wal.OpVSimAddDocBatch {
				continue
			}
			docs, err := vector.DeserializeDocBatch(entry.Value)
			if err != nil {
				t.Fatalf("целая по CRC запись не декодится: %v", err)
			}
			n += len(docs)
		}
		return n
	}
	if got := pairState(path); got != 2 {
		t.Fatalf("полный WAL: в батче %d доков, ожидалась пара", got)
	}

	tmp := filepath.Join(t.TempDir(), "torn.wal")
	sawZero := false
	for cut := len(raw) - 1; cut >= 0; cut-- {
		if err := os.WriteFile(tmp, raw[:cut], 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		switch got := pairState(tmp); got {
		case 2:
		case 0:
			sawZero = true
		default:
			t.Fatalf("обрезка на %d байт: реплей дал %d док(а) из пары — полуправда", cut, got)
		}
	}
	if !sawZero {
		t.Fatal("ни одна обрезка не удалила пару — тест не задел запись")
	}
}

// TestExecVMEM_Gates — порог 4: OOM-гейт режет REMEMBER (растит память), но
// пропускает RECALL и FORGET (выход из OOM — освобождение); durability
// fail-stop режет оба пишущих (REMEMBER и FORGET), но не RECALL.
func TestExecVMEM_Gates(t *testing.T) {
	t.Run("oom", func(t *testing.T) {
		e := newExecEnv(t)
		e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "живучий факт", "ID", "f1"), "f1")
		e.s.SetMaxMemory(1)
		e.wantErrPrefix(e.do("VMEM.REMEMBER", "s", "TEXT", "не влезет"), "OOM")
		if v := e.do("VMEM.RECALL", "s", "10", "живучий"); v.Typ != '*' || len(v.Array) != 3 {
			t.Fatalf("RECALL под OOM обязан работать: %+v", v)
		}
		e.wantInt(e.do("VMEM.FORGET", "s", "f1"), 1) // удаление — путь наружу из OOM
	})
	t.Run("failstop", func(t *testing.T) {
		e := newExecEnv(t)
		e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "живучий факт", "ID", "f1"), "f1")
		raw := e.bw.RawWAL()
		_ = raw.Close()
		_ = raw.Sync()
		if e.bw.Failed() == nil {
			t.Fatal("предусловие: WAL не в fail-stop")
		}
		e.wantErrPrefix(e.do("VMEM.REMEMBER", "s", "TEXT", "нельзя"), "WAL persistence failed")
		e.wantErrPrefix(e.do("VMEM.FORGET", "s", "f1"), "WAL persistence failed")
		if v := e.do("VMEM.RECALL", "s", "10", "живучий"); v.Typ != '*' || len(v.Array) != 3 {
			t.Fatalf("RECALL под fail-stop обязан работать: %+v", v)
		}
	})
}

// TestExecVMEM_ForgetScopeAndErrors — контракт границ: FORGET через чужой
// scope — ошибка (факт жив), несуществующий id → :0 (идемпотентность),
// SUPERSEDES несуществующей цели — ошибка, невалидные аргументы — usage.
func TestExecVMEM_ForgetScopeAndErrors(t *testing.T) {
	e := newExecEnv(t)
	e.wantBulk(e.do("VMEM.REMEMBER", "user:dana", "TEXT", "приватный факт", "ID", "f1"), "f1")
	e.wantErrPrefix(e.do("VMEM.FORGET", "user:boris", "f1"), "ERR vmem: forget target belongs to a different scope")
	if v := e.do("VMEM.RECALL", "user:dana", "10", "приватный"); len(v.Array) != 3 {
		t.Fatalf("факт обязан пережить чужой FORGET: %+v", v)
	}
	e.wantInt(e.do("VMEM.FORGET", "user:dana", "no-such"), 0)
	e.wantErrPrefix(e.do("VMEM.REMEMBER", "s", "TEXT", "x", "SUPERSEDES", "ghost"), "ERR vmem: supersedes target does not exist")
	e.wantErrPrefix(e.do("VMEM.REMEMBER", "s"), "ERR usage:")
	e.wantErrPrefix(e.do("VMEM.REMEMBER", "s", "ID", "f9"), "ERR TEXT is required")
	e.wantErrPrefix(e.do("VMEM.REMEMBER", "s", "TEXT", "x", "IMPORTANCE", "kek"), "ERR IMPORTANCE not a float")
	e.wantErrPrefix(e.do("VMEM.RECALL", "s", "0", "q"), "ERR invalid K")
	e.wantErrPrefix(e.do("VMEM.RECALL", "s", "10", "q", "ASOF", "10", "ALL"), "ERR vmem: as_of")
	e.wantErrPrefix(e.do("VMEM.FORGET", "s"), "ERR usage:")
}

// TestExecVMEM_TxClassification — VMEM-команды разрешены в MULTI/EXEC
// (в отличие от subscribe-семейства, ломающего RESP-кадр): классификация
// forbiddenInTx + живой прогон очереди через execQueuedTx (путь EXEC).
func TestExecVMEM_TxClassification(t *testing.T) {
	for _, cmd := range []string{"VMEM.REMEMBER", "VMEM.RECALL", "VMEM.FORGET"} {
		if forbiddenInTx(cmd) {
			t.Fatalf("%s не должен быть запрещён в MULTI/EXEC", cmd)
		}
	}
	e := newExecEnv(t)
	queue := [][][]byte{
		{[]byte("VMEM.REMEMBER"), []byte("s"), []byte("TEXT"), []byte("факт из транзакции"), []byte("ID"), []byte("f1")},
		{[]byte("VMEM.FORGET"), []byte("s"), []byte("f1")},
	}
	var replies []protocol.Value
	execQueuedTx(queue, func(int) {}, func(qCmd string, qArgs [][]byte) {
		args := make([]string, len(qArgs))
		for i, a := range qArgs {
			args[i] = string(a)
		}
		replies = append(replies, e.do(qCmd, args...))
	})
	if len(replies) != 2 {
		t.Fatalf("EXEC исполнил %d команд из 2", len(replies))
	}
	e.wantBulk(replies[0], "f1")
	e.wantInt(replies[1], 1)
}

// TestExecVMEM_ServerTTLAndAnchor — TTL через RESP на серверных часах:
// факт с TTL жив сразу после записи, якорь-текст получил таймер (TTL > 0),
// upsert без TTL снимает таймер якоря. Семантика истечения — store-уровень.
func TestExecVMEM_ServerTTLAndAnchor(t *testing.T) {
	e := newExecEnv(t)
	e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "недолговечный факт", "ID", "f1", "TTL", "3600"), "f1")
	if v := e.do("VMEM.RECALL", "s", "10", "недолговечный"); len(v.Array) != 3 || v.Array[2].Str != "недолговечный факт" {
		t.Fatalf("свежий TTL-факт обязан быть виден с якорем: %+v", v)
	}
	if v := e.do("TTL", "vmem:f1"); v.Typ != ':' || v.Num <= 0 {
		t.Fatalf("якорь TTL-факта обязан иметь таймер: %+v", v)
	}
	e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "недолговечный факт", "ID", "f1"), "f1")
	if v := e.do("TTL", "vmem:f1"); v.Typ != ':' || v.Num != -1 {
		t.Fatalf("upsert без TTL обязан снять таймер якоря: %+v", v)
	}
}

// TestExecVMEM_Provenance — провенанс через RESP: SOURCE едет в контракт,
// фильтр RECALL отбирает по нему выборочно, необъявленный источник
// первоклассно ищется как "unknown", и всё это переживает рестарт-реплей
// (провенанс, не доживающий до перезапуска, форензике бесполезен).
func TestExecVMEM_Provenance(t *testing.T) {
	e1 := newExecEnv(t)
	e1.wantBulk(e1.do("VMEM.REMEMBER", "s", "TEXT", "дедлайн проекта март", "ID", "f1", "SOURCE", "web-scraper"), "f1")
	e1.wantBulk(e1.do("VMEM.REMEMBER", "s", "TEXT", "дедлайн проекта апрель", "ID", "f2", "SOURCE", "email-agent"), "f2")
	e1.wantBulk(e1.do("VMEM.REMEMBER", "s", "TEXT", "дедлайн проекта май", "ID", "f3"), "f3")

	recallIDs := func(e *execEnv, args ...string) []string {
		v := e.do("VMEM.RECALL", args...)
		if v.Typ != '*' {
			t.Fatalf("RECALL не массив: %+v", v)
		}
		var ids []string
		for i := 0; i < len(v.Array); i += 3 {
			ids = append(ids, v.Array[i].Str)
		}
		slices.Sort(ids)
		return ids
	}
	cases := []struct {
		source string
		want   []string
	}{
		{"web-scraper", []string{"f1"}},
		{"email-agent", []string{"f2"}},
		{"unknown", []string{"f3"}},
	}
	for _, tc := range cases {
		if got := recallIDs(e1, "s", "10", "дедлайн", "SOURCE", tc.source); !slices.Equal(got, tc.want) {
			t.Errorf("SOURCE=%s: %v, ожидалось %v", tc.source, got, tc.want)
		}
	}
	// Без фильтра видны все три — фильтр отбирает, а не прячет.
	if got := recallIDs(e1, "s", "10", "дедлайн"); len(got) != 3 {
		t.Errorf("без SOURCE: %v, ожидались все три факта", got)
	}
	// SOURCE без значения — ошибка разбора, а не молчаливый пропуск фильтра.
	e1.wantErrPrefix(e1.do("VMEM.RECALL", "s", "10", "дедлайн", "SOURCE"), "ERR SOURCE requires")
	e1.wantErrPrefix(e1.do("VMEM.REMEMBER", "s", "TEXT", "факт", "SOURCE"), "ERR SOURCE requires")

	if err := e1.bw.Close(); err != nil {
		t.Fatalf("bw.Close: %v", err)
	}
	e2 := newExecEnv(t)
	replayVMEMWAL(t, filepath.Join(e1.dir, "t.wal"), e2)
	for _, tc := range cases {
		if got := recallIDs(e2, "s", "10", "дедлайн", "SOURCE", tc.source); !slices.Equal(got, tc.want) {
			t.Errorf("после реплея SOURCE=%s: %v, ожидалось %v — провенанс не пережил рестарт", tc.source, got, tc.want)
		}
	}
}

// wantReceipt — сверка КВИТАНЦИИ по названным полям. Отсутствующее поле —
// провал, а не пропуск: молча не найденный ключ превратил бы проверку в
// «ответ вообще есть», что и так видно.
func wantReceipt(t *testing.T, v protocol.Value, want map[string]string) {
	t.Helper()
	if v.Typ != '*' {
		t.Fatalf("квитанция не массив: typ=%q str=%q", v.Typ, v.Str)
	}
	got := fieldsOf(t, v)
	for k, exp := range want {
		switch actual, ok := got[k]; {
		case !ok:
			t.Errorf("в квитанции нет поля %q (есть: %v)", k, got)
		case actual != exp:
			t.Errorf("квитанция.%s = %q, ожидалось %q", k, actual, exp)
		}
	}
}

// TestExecVMEM_Quarantine — карантин через RESP: выборочный отзыв по
// происхождению, законные факты целы, история веры доступна через ASOF, и всё
// это переживает рестарт-реплей (батч едет одной записью — краш не может
// оставить половину лжи отозванной).
func TestExecVMEM_Quarantine(t *testing.T) {
	e1 := newExecEnv(t)
	e1.wantBulk(e1.do("VMEM.REMEMBER", "s", "TEXT", "дедлайн проекта март", "ID", "bad1", "SOURCE", "web-scraper", "VALIDFROM", "1000"), "bad1")
	e1.wantBulk(e1.do("VMEM.REMEMBER", "s", "TEXT", "дедлайн проекта апрель", "ID", "bad2", "SOURCE", "web-scraper", "VALIDFROM", "1000"), "bad2")
	e1.wantBulk(e1.do("VMEM.REMEMBER", "s", "TEXT", "дедлайн проекта май", "ID", "ok1", "SOURCE", "email-agent", "VALIDFROM", "1000"), "ok1")

	recallIDs := func(e *execEnv, args ...string) []string {
		v := e.do("VMEM.RECALL", args...)
		if v.Typ != '*' {
			t.Fatalf("RECALL не массив: %+v", v)
		}
		var ids []string
		for i := 0; i < len(v.Array); i += 3 {
			ids = append(ids, v.Array[i].Str)
		}
		slices.Sort(ids)
		return ids
	}
	// «Отозвать всё» не должно быть выразимо ни одной формой команды:
	// слишком мало аргументов, висящий SOURCE и полностью отсутствующий
	// источник — три разных пути, все обязаны отказать.
	e1.wantErrPrefix(e1.do("VMEM.QUARANTINE", "s", "SOURCE"), "ERR usage: VMEM.QUARANTINE")
	e1.wantErrPrefix(e1.do("VMEM.QUARANTINE", "s", "SINCE", "1000", "SOURCE"), "ERR SOURCE requires")
	e1.wantErrPrefix(e1.do("VMEM.QUARANTINE", "s", "SINCE", "1000", "LIMIT", "5"), "ERR vmem: quarantine requires a source")
	wantReceipt(t, e1.do("VMEM.QUARANTINE", "s", "SOURCE", "web-scraper"), map[string]string{
		"scope": "s", "source": "web-scraper", "since": "none",
		"revoked": "2", "still_trusted": "0", "outside_window": "0", "over_limit": "0",
		// Оговорка обязана быть в КАЖДОЙ квитанции, а не только в тревожной:
		// ноль в still_trusted иначе читается как «инцидент закрыт», хотя
		// он про один источник (ложь другим каналом — L1/L4/L5, измерены).
		"other_origins": "not_covered",
	})
	// Идемпотентно — и теперь ноль означает ОДНО: отзывать нечего и не
	// осталось ничего. До квитанции этот же `:0` не отличался от «первая
	// партия снята, остальное не тронуто».
	wantReceipt(t, e1.do("VMEM.QUARANTINE", "s", "SOURCE", "web-scraper"), map[string]string{
		"revoked": "0", "still_trusted": "0", "outside_window": "0", "over_limit": "0",
	})

	if got := recallIDs(e1, "s", "10", "дедлайн"); !slices.Equal(got, []string{"ok1"}) {
		t.Fatalf("после карантина: %v, ожидался [ok1]", got)
	}
	if got := recallIDs(e1, "s", "10", "дедлайн", "ALL"); len(got) != 3 {
		t.Fatalf("ALL: %v, ожидались все три (форензический режим)", got)
	}
	if got := recallIDs(e1, "s", "10", "дедлайн", "ASOF", "1500"); len(got) != 3 {
		t.Fatalf("ASOF до отзыва: %v, ожидались все три — история веры стёрта", got)
	}

	if err := e1.bw.Close(); err != nil {
		t.Fatalf("bw.Close: %v", err)
	}
	e2 := newExecEnv(t)
	replayVMEMWAL(t, filepath.Join(e1.dir, "t.wal"), e2)
	if got := recallIDs(e2, "s", "10", "дедлайн"); !slices.Equal(got, []string{"ok1"}) {
		t.Fatalf("после реплея: %v, ожидался [ok1] — карантин не пережил рестарт", got)
	}
	if got := recallIDs(e2, "s", "10", "дедлайн", "ASOF", "1500"); len(got) != 3 {
		t.Fatalf("после реплея ASOF: %v, ожидались все три — улика потеряна при рестарте", got)
	}
}

// TestExecVMEM_QuarantineRemainderOutsideWindow — то, ради чего ответ перестал
// быть числом.
//
// 🚨СЦЕНАРИЙ ВОСПРОИЗВОДИТ ИЗМЕРЕННУЮ ДЫРУ (L3, scripts/revocation_limits.py):
// окно вычисляется из valid_from ЗАМЕЧЕННОЙ подсадки — оператор знает ровно
// это, — а подсаженное раньше уходит из-под окна, и злого умысла для этого не
// нужно: «бюджет заморожен с начала месяца» датируется началом месяца. Отзыв
// отрабатывает честно и снимает ровно одно. Раньше он отвечал бодрым `:1`, и
// ДВЕ пережившие лечение лжи не были названы ничем. Теперь их называет сам
// движок, а не скрипт рядом с ним.
func TestExecVMEM_QuarantineRemainderOutsideWindow(t *testing.T) {
	e := newExecEnv(t)
	e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "бюджет заморожен", "ID", "old1", "SOURCE", "email-channel", "VALIDFROM", "1000"), "old1")
	e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "закупки остановлены", "ID", "old2", "SOURCE", "email-channel", "VALIDFROM", "1000"), "old2")
	e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "подрядчик сменился", "ID", "seen", "SOURCE", "email-channel", "VALIDFROM", "3000"), "seen")
	// Сосед: другой источник в счёт остатка попадать не имеет права, иначе
	// still_trusted мерил бы размер памяти, а не полноту лечения.
	e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "встреча в четверг", "ID", "human1", "SOURCE", "human", "VALIDFROM", "1000"), "human1")

	wantReceipt(t, e.do("VMEM.QUARANTINE", "s", "SOURCE", "email-channel", "SINCE", "3000"), map[string]string{
		"since": "3000", "revoked": "1",
		"still_trusted": "2", "outside_window": "2", "over_limit": "0",
	})
	// Тот же отзыв без окна: остаток снимается, и цена этого выбора видна
	// рядом — обе точки компромисса теперь предъявлены числом.
	wantReceipt(t, e.do("VMEM.QUARANTINE", "s", "SOURCE", "email-channel"), map[string]string{
		"since": "none", "revoked": "2",
		"still_trusted": "0", "outside_window": "0", "over_limit": "0",
	})
}

// TestExecVMEM_QuarantineRemainderOverLimit — вторая причина остатка: партия
// ограничена сверху, и хвост до сих пор был неотличим снаружи. Число
// отозванных совпадает с LIMIT и в случае «набрали ровно партию, есть ещё», и
// в случае «столько и было» — а разница между ними это разница между
// «повторите вызов» и «работа закончена».
func TestExecVMEM_QuarantineRemainderOverLimit(t *testing.T) {
	e := newExecEnv(t)
	for i := 0; i < 5; i++ {
		id := "f" + strconv.Itoa(i)
		e.wantBulk(e.do("VMEM.REMEMBER", "s", "TEXT", "подсадка "+id, "ID", id, "SOURCE", "bad", "VALIDFROM", "1000"), id)
	}
	// Партия по 2: остаток обязан таять ровно на снятое, а не «пересчитаться»
	// заново, — поэтому ожидания записаны последовательностью, а не одним
	// финальным нулём.
	for _, want := range []map[string]string{
		{"revoked": "2", "still_trusted": "3", "over_limit": "3", "outside_window": "0"},
		{"revoked": "2", "still_trusted": "1", "over_limit": "1", "outside_window": "0"},
		{"revoked": "1", "still_trusted": "0", "over_limit": "0", "outside_window": "0"},
	} {
		wantReceipt(t, e.do("VMEM.QUARANTINE", "s", "SOURCE", "bad", "LIMIT", "2"), want)
	}
	// Контроль диагноза: последний вызов уже вернул ноль по обоим счётчикам —
	// если бы остаток считался по ЧЕРНОВИКУ скана (он обрывается на LIMIT),
	// первый же вызов показал бы over_limit=0 и проверки выше прошли бы мимо.
	wantReceipt(t, e.do("VMEM.QUARANTINE", "s", "SOURCE", "bad", "LIMIT", "2"), map[string]string{
		"revoked": "0", "still_trusted": "0", "over_limit": "0",
	})
}
