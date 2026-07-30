package main

import (
	"encoding/json"
	"testing"

	"kvstore/kvstore/internal/auditchain"
)

// Подключение цепи аудита к командам VMEM.
//
// ⭐ЗДЕСЬ ПРОВЕРЯЕТСЯ ИНВАРИАНТ, А НЕ СПИСОК ТОЧЕК. Прошлая интеграция
// (конверты кейринга) сначала закрыла только REMEMBER, а QUARANTINE и BACKFILL
// уехали бы открытым текстом — и лечением было не «добавить ещё две проверки»,
// а тест, перебирающий ВСЕ пишущие команды и требующий записи для каждой.
// Тогда следующая забытая точка падает сама. Тот же приём здесь: таблица
// команд ниже и есть определение того, что цепь обязана фиксировать.

// enableAuditChain поднимает носитель на время теста и возвращает каталог.
func enableAuditChain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	c, err := auditchain.Open(dir)
	if err != nil {
		t.Fatalf("auditchain.Open: %v", err)
	}
	prev := auditChain
	auditChain = c
	t.Cleanup(func() {
		auditChain = prev
		c.Close()
	})
	return dir
}

// chainLeaves сбрасывает буфер и читает с диска ВСЕ листья цепи.
func chainLeaves(t *testing.T, dir string) []auditchain.Leaf {
	t.Helper()
	if _, err := auditChain.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	links, err := auditchain.ReadChain(dir)
	if err != nil {
		t.Fatalf("ReadChain: %v", err)
	}
	var out []auditchain.Leaf
	for i, l := range links {
		p, err := auditchain.DecodeBatchPayload(l.Payload)
		if err != nil {
			t.Fatalf("звено %d: %v", i, err)
		}
		leaves, err := auditchain.ReadLeaves(dir, p.FirstLeaf, int(p.Count))
		if err != nil {
			t.Fatalf("звено %d: %v", i, err)
		}
		// Заодно: корень каждого звена обязан сходиться с его листьями.
		if auditchain.MerkleRoot(leaves) != p.Root {
			t.Fatalf("звено %d: корень не сходится с листьями на диске", i)
		}
		out = append(out, leaves...)
	}
	return out
}

func countByType(leaves []auditchain.Leaf) map[auditchain.EventType]int {
	m := make(map[auditchain.EventType]int)
	for _, l := range leaves {
		m[l.Type]++
	}
	return m
}

// ⭐TestAuditChain_EveryWritingCommandIsRecorded — инвариант интеграции.
//
// Каждая команда, меняющая память, обязана оставить след в цепи. Забытая точка
// подключения роняет ИМЕННО ЭТОТ тест, без правки тестов.
func TestAuditChain_EveryWritingCommandIsRecorded(t *testing.T) {
	dir := enableAuditChain(t)
	e := newExecEnv(t)
	enableKeyring(t, e.dir)

	cases := []struct {
		name string
		want auditchain.EventType
		run  func() string // возвращает id факта, если команда его создаёт
	}{
		{"VMEM.REMEMBER", auditchain.EventRemember, func() string {
			v := e.do("VMEM.REMEMBER", "alice", "TEXT", "любит кофе без сахара", "SOURCE", "agent-a")
			if v.Typ != '$' {
				t.Fatalf("REMEMBER: %v", v.Str)
			}
			return v.Str
		}},
		{"VMEM.FORGET", auditchain.EventForget, func() string {
			v := e.do("VMEM.REMEMBER", "alice", "TEXT", "временный факт", "SOURCE", "agent-a")
			e.do("VMEM.FORGET", "alice", v.Str)
			return v.Str
		}},
		{"VMEM.QUARANTINE", auditchain.EventQuarantine, func() string {
			e.do("VMEM.REMEMBER", "bob", "TEXT", "факт из отравленного источника", "SOURCE", "bad-agent")
			e.do("VMEM.QUARANTINE", "bob", "SOURCE", "bad-agent")
			return ""
		}},
		{"VMEM.BACKFILL", auditchain.EventBackfill, func() string {
			// ⚠Легаси-факт нельзя создать через VMEM.REMEMBER: источник
			// штампуется ВСЕГДА, и мигрировать было бы нечего (первая версия
			// этого теста получала 0 и потому не проверяла ничего). Факт без
			// колонки source — это ровно то, что осталось от версий до
			// провенанса, и повторяется он только сырым ADDDOC.
			// Размерность — placeholder ступени 0 (vector.vmemPlaceholderDim,
			// из пакета main не видна). Несовпадение вылезет явной ошибкой
			// ADDDOC, поэтому её проверяем, а не молчим.
			args := []string{"legacy-1", "TEXT", "легаси-факт", "CAT", "scope", "carol", "VEC"}
			for i := 0; i < 32; i++ {
				args = append(args, "0")
			}
			if v := e.do("VSIM.ADDDOC", args...); v.Typ == '-' {
				t.Fatalf("ADDDOC: %v", v.Str)
			}
			v := e.do("VMEM.BACKFILL", "carol", "SOURCE", "imported-2024")
			if v.Typ != ':' || v.Num != 1 {
				t.Fatalf("BACKFILL мигрировал %d фактов, ожидался 1 — тест проверял бы пустую операцию", v.Num)
			}
			return ""
		}},
		{"VMEM.SHRED", auditchain.EventShred, func() string {
			e.do("VMEM.REMEMBER", "dave", "TEXT", "всё про дейва", "SOURCE", "agent-a")
			v := e.do("VMEM.SHRED", "dave")
			if v.Typ == '-' {
				t.Fatalf("SHRED: %v", v.Str)
			}
			return ""
		}},
	}

	for _, tc := range cases {
		before := countByType(chainLeaves(t, dir))
		tc.run()
		after := countByType(chainLeaves(t, dir))
		if after[tc.want] <= before[tc.want] {
			t.Errorf("%s не оставила следа в цепи (событий типа %d было %d, стало %d) — точка записи не подключена",
				tc.name, tc.want, before[tc.want], after[tc.want])
		}
	}
}

// ⭐TestAuditChain_RememberStoresFingerprintNotContent — правило, ради
// которого цепь вообще совместима с криптостиранием: положи в append-only
// журнал текст факта, и SHRED перестанет что-либо значить — стёртое жило бы
// вечно в том, что нельзя переписать.
func TestAuditChain_RememberStoresFingerprintNotContent(t *testing.T) {
	dir := enableAuditChain(t)
	e := newExecEnv(t)

	const secret = "диагноз пациента — гипертония второй степени"
	id := e.do("VMEM.REMEMBER", "alice", "TEXT", secret, "SOURCE", "clinic").Str

	leaves := chainLeaves(t, dir)
	var found bool
	for _, l := range leaves {
		if l.Type != auditchain.EventRemember || l.Subject != id {
			continue
		}
		found = true
		// ПАРНЫЙ КОНТРОЛЬ: отпечаток обязан БЫТЬ, иначе проверка «текста нет»
		// прошла бы и на пустом листе.
		var p rememberPayload
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatalf("предмет листа не разбирается: %v", err)
		}
		if p.Hash == "" {
			t.Error("в листе нет отпечатка текста — подмену содержания доказать нечем")
		}
		if p.Source != "clinic" {
			t.Errorf("источник в листе %q, ожидался clinic — отзыв идёт по источнику", p.Source)
		}
	}
	if !found {
		t.Fatal("создание факта не попало в цепь")
	}

	// И главное: дословного текста в цепи нет НИГДЕ — ни в листьях, ни в
	// звеньях. Проверяется по всем байтам, а не по одному полю.
	for i, l := range leaves {
		if bytesContain(l.Payload, secret) || bytesContain([]byte(l.Subject), secret) || bytesContain([]byte(l.Scope), secret) {
			t.Fatalf("лист %d содержит дословный текст факта — append-only журнал пережил бы SHRED", i)
		}
	}
}

func bytesContain(b []byte, s string) bool {
	return len(s) > 0 && len(b) >= len(s) && stringIndex(string(b), s) >= 0
}

func stringIndex(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// ⭐TestAuditChain_ShredForcesFlush — квитанция обязана покрывать всё, что было
// раньше. Если события до SHRED остались в буфере, окно агрегации накрывает
// ровно тот момент, который продаётся как доказанный.
func TestAuditChain_ShredForcesFlush(t *testing.T) {
	dir := enableAuditChain(t)
	e := newExecEnv(t)
	enableKeyring(t, e.dir)

	e.do("VMEM.REMEMBER", "erin", "TEXT", "факт один", "SOURCE", "agent-a")
	e.do("VMEM.REMEMBER", "erin", "TEXT", "факт два", "SOURCE", "agent-a")
	if auditChain.Pending() == 0 {
		t.Fatal("рядовые REMEMBER ушли на диск сразу — батч не работает, и тест ниже проверял бы не то")
	}

	v := e.do("VMEM.SHRED", "erin")
	if v.Typ == '-' {
		t.Fatalf("SHRED: %v", v.Str)
	}
	if n := auditChain.Pending(); n != 0 {
		t.Fatalf("после SHRED в буфере осталось %d событий — окно недоказуемости накрыло квитанцию", n)
	}

	// Не флашим специально: всё обязано быть на диске уже сейчас.
	links, err := auditchain.ReadChain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) == 0 {
		t.Fatal("цепь пуста после SHRED")
	}
	p, err := auditchain.DecodeBatchPayload(links[len(links)-1].Payload)
	if err != nil {
		t.Fatal(err)
	}
	leaves, err := auditchain.ReadLeaves(dir, p.FirstLeaf, int(p.Count))
	if err != nil {
		t.Fatal(err)
	}
	types := countByType(leaves)
	if types[auditchain.EventRemember] != 2 || types[auditchain.EventShred] != 1 {
		t.Fatalf("звено с квитанцией накрыло %+v, ожидались 2 REMEMBER и 1 SHRED в одном батче", types)
	}
}

// TestAuditChain_ShredReceiptCarriesChainSeq — квитанция должна давать то, по
// чему её потом ищут в цепи.
func TestAuditChain_ShredReceiptCarriesChainSeq(t *testing.T) {
	enableAuditChain(t)
	e := newExecEnv(t)
	enableKeyring(t, e.dir)
	e.do("VMEM.REMEMBER", "frank", "TEXT", "факт", "SOURCE", "agent-a")

	v := e.do("VMEM.SHRED", "frank")
	if v.Typ != '*' {
		t.Fatalf("SHRED: %v", v.Str)
	}
	fields := map[string]string{}
	for i := 0; i+1 < len(v.Array); i += 2 {
		fields[v.Array[i].Str] = v.Array[i+1].Str
	}
	seq, ok := fields["chain_seq"]
	if !ok {
		t.Fatal("в квитанции нет chain_seq — предъявить её в цепи нечем")
	}
	if seq == "off" || seq == "unrecorded" || seq == "0" {
		t.Fatalf("chain_seq = %q при поднятой цепи", seq)
	}
}

// TestAuditChain_OffByDefault — цепь платит вторым файлом и вторым fsync;
// сервер без флага не должен платить ничего, а квитанция обязана честно
// говорить, что предъявлять её негде.
func TestAuditChain_OffByDefault(t *testing.T) {
	e := newExecEnv(t)
	enableKeyring(t, e.dir)
	if auditChain != nil {
		t.Fatal("цепь включена без флага")
	}
	e.do("VMEM.REMEMBER", "gina", "TEXT", "факт", "SOURCE", "agent-a")

	v := e.do("VMEM.SHRED", "gina")
	if v.Typ != '*' {
		t.Fatalf("SHRED: %v", v.Str)
	}
	for i := 0; i+1 < len(v.Array); i += 2 {
		if v.Array[i].Str == "chain_seq" && v.Array[i+1].Str != "off" {
			t.Fatalf("без цепи chain_seq = %q, ожидалось off", v.Array[i+1].Str)
		}
	}
}

// TestAuditChain_QuarantineRecordsEveryFactByName — сводка «отозвано N по
// источнику S» доказывала бы объём, но не состав, а отзыв оспаривают пофактно.
func TestAuditChain_QuarantineRecordsEveryFactByName(t *testing.T) {
	dir := enableAuditChain(t)
	e := newExecEnv(t)

	var ids []string
	for _, txt := range []string{"первый", "второй", "третий"} {
		ids = append(ids, e.do("VMEM.REMEMBER", "hank", "TEXT", txt, "SOURCE", "bad-agent").Str)
	}
	e.do("VMEM.REMEMBER", "hank", "TEXT", "чистый факт", "SOURCE", "good-agent")

	v := e.do("VMEM.QUARANTINE", "hank", "SOURCE", "bad-agent")
	if v.Typ != ':' || v.Num != 3 {
		t.Fatalf("QUARANTINE вернула %v/%d, ожидалось :3", string(v.Typ), v.Num)
	}

	named := map[string]bool{}
	var summary *quarantinePayload
	for _, l := range chainLeaves(t, dir) {
		if l.Type != auditchain.EventQuarantine {
			continue
		}
		if l.Subject != "" {
			named[l.Subject] = true
			continue
		}
		var p quarantinePayload
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatalf("сводка карантина не разбирается: %v", err)
		}
		summary = &p
	}
	for _, id := range ids {
		if !named[id] {
			t.Errorf("факт %s отозван, но поимённо в цепь не попал", id)
		}
	}
	if summary == nil {
		t.Fatal("в цепи нет сводки карантина")
	}
	if summary.Facts != 3 || summary.Source != "bad-agent" {
		t.Errorf("сводка карантина %+v, ожидалось 3 факта по bad-agent", *summary)
	}
}

// TestAuditChain_ChainVerifiesAfterMixedTraffic — цепь обязана сходиться после
// обычной смеси команд, иначе всё выше проверяет отдельные листья, а не журнал.
func TestAuditChain_ChainVerifiesAfterMixedTraffic(t *testing.T) {
	dir := enableAuditChain(t)
	e := newExecEnv(t)
	enableKeyring(t, e.dir)

	for i := 0; i < 20; i++ {
		id := e.do("VMEM.REMEMBER", "ivan", "TEXT", "факт", "SOURCE", "agent-a").Str
		if i%3 == 0 {
			e.do("VMEM.FORGET", "ivan", id)
		}
		if i%7 == 0 {
			if _, err := auditChain.Flush(); err != nil { // тик посреди потока
				t.Fatal(err)
			}
		}
	}
	e.do("VMEM.SHRED", "ivan")

	head := auditChain.Head()
	links, err := auditchain.ReadChain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auditchain.Verify(links, &head); err != nil {
		t.Fatalf("цепь не сходится с головой после смешанного потока: %v", err)
	}
}
