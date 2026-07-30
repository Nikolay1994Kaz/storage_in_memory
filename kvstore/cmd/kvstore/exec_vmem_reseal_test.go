package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"kvstore/kvstore/internal/wal"
)

// Перешифровка легаси (П10б).
//
// ⭐ЧТО ЗДЕСЬ ПРОВЕРЯЕТСЯ НА САМОМ ДЕЛЕ. Не «команда проставила атрибут» — это
// проверка кода его же утверждением. Проверяется ФИЗИЧЕСКОЕ следствие: после
// перешифровки новая запись в журнале не содержит открытого текста, то есть
// уничтожение ключа до неё дотянется. Атрибут sealed без этого был бы просто
// вторым местом, где написано то же самое.

func TestReseal_RemovesPlaintextFromJournal(t *testing.T) {
	e := newExecEnv(t)

	// Фаза 1: пишем БЕЗ шифрования — легаси-корпус.
	const legacy = "старый факт, записанный до шифрования"
	id := e.do("VMEM.REMEMBER", "alice", "TEXT", legacy, "SOURCE", "agent-a").Str
	if id == "" {
		t.Fatal("факт не создан")
	}

	// ПАРНЫЙ КОНТРОЛЬ: до перешифровки текст в журнале ОТКРЫТ. Без этой
	// проверки тест ниже прошёл бы и на движке, который вообще ничего не пишет.
	//
	// ⚠Именно Close, а не Sync: BatchWAL пишет асинхронно, и Sync
	// синхронизирует ФАЙЛ, не дожидаясь, пока записи дойдут из канала батчера.
	// Эта грабля уже стоила теста, зелёного по неверной причине (см.
	// archiveWAL в exec_vmem_shred_test.go).
	if err := e.bw.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(e.dir, "t.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(legacy)) {
		t.Fatal("до перешифровки открытого текста в журнале нет — проверка ниже ничего не значит")
	}

	// Фаза 2: новый журнал, чтобы в нём лежало ТОЛЬКО то, что напишет
	// перешифровка. Иначе «в журнале нет открытого текста» пришлось бы
	// доказывать вычитанием, а вычитание легко проходит по неверной причине.
	w2, err := wal.Open(filepath.Join(e.dir, "t2.wal"))
	if err != nil {
		t.Fatal(err)
	}
	e.bw = wal.NewBatchWAL(w2)

	ring := enableKeyring(t, e.dir)
	sealingActive = true
	t.Cleanup(func() { sealingActive = false })

	v := e.do("VMEM.RESEAL", "alice")
	if v.Typ != '*' {
		t.Fatalf("RESEAL: %v", v.Str)
	}
	fields := fieldsOf(t, v)
	if fields["resealed"] != "1" {
		t.Fatalf("перешифровано %q фактов, ожидался 1", fields["resealed"])
	}
	if fields["sealed_share"] != "1.0000" {
		t.Fatalf("доля покрытия %q, ожидалась 1.0000", fields["sealed_share"])
	}
	// ⚠Квитанция ОБЯЗАНА говорить, что старые копии не покрыты.
	if fields["earlier_copies"] != "not_covered" {
		t.Fatalf("квитанция молчит о старых копиях: %+v", fields)
	}

	// Новая запись в журнале — под конвертом.
	if err := e.bw.Close(); err != nil {
		t.Fatal(err)
	}
	tail, err := os.ReadFile(filepath.Join(e.dir, "t2.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) == 0 {
		t.Fatal("перешифровка ничего не дописала в журнал")
	}
	if bytes.Contains(tail, []byte(legacy)) {
		t.Fatal("перешифрованная версия уехала в журнал открытым текстом")
	}
	if !ring.HasScope("alice") {
		t.Fatal("ключ скоупа не создан")
	}

	// ⭐ЯКОРЬ-ТЕКСТ ТОЖЕ ОБЯЗАН БЫТЬ ПЕРЕЗАПИСАН. Он лежит в KV отдельной
	// записью (vmem:<id>) и содержит факт ДОСЛОВНО — то есть самое читаемое из
	// всего, что есть. Перешифровка одних только векторов и термов подняла бы
	// покрытие до 1.0, оставив в журнале открытый текст; проверка «в хвосте нет
	// legacy» этого бы не заметила, потому что якорь просто не переписывался бы.
	_, entries, err := wal.ReadFile(filepath.Join(e.dir, "t2.wal"))
	if err != nil {
		t.Fatal(err)
	}
	var anchor, batch int
	for _, en := range entries {
		switch {
		case en.Op == wal.OpSet && en.Key == "vmem:"+id:
			anchor++
		case en.Op == wal.OpVSimAddDocBatch:
			batch++
		}
	}
	if batch == 0 {
		t.Error("перешифровка не переписала документ факта")
	}
	if anchor == 0 {
		t.Error("перешифровка не переписала якорь-текст — дословный факт остался в журнале открытым")
	}
}

// ⭐TestReseal_RefusedWithoutEncryption — без активного конверта перешифровка
// проставила бы sealed записям, которые не шифровались. Это не деградация, а
// ложь в отчёте о покрытии, поэтому команда обязана отказать.
func TestReseal_RefusedWithoutEncryption(t *testing.T) {
	e := newExecEnv(t)
	e.do("VMEM.REMEMBER", "alice", "TEXT", "факт", "SOURCE", "agent-a")

	v := e.do("VMEM.RESEAL", "alice")
	if v.Typ != '-' {
		t.Fatalf("RESEAL без -encrypt-at-rest не отвергнута: %v", v)
	}

	// И покрытие не сдвинулось: соврать не удалось даже частично.
	cov := e.do("VMEM.COVERAGE", "alice")
	f := fieldsOf(t, cov.Array[0])
	if f["sealed"] != "0" {
		t.Fatalf("покрытие выросло после отказавшей команды: %+v", f)
	}
}

// TestReseal_IsIdempotent — повторный запуск не должен переписывать уже
// покрытые факты: иначе администратор, запустивший команду дважды, удвоил бы
// объём журнала без единого изменения смысла.
func TestReseal_IsIdempotent(t *testing.T) {
	e := newExecEnv(t)
	enableKeyring(t, e.dir)
	e.do("VMEM.REMEMBER", "alice", "TEXT", "факт", "SOURCE", "agent-a")
	sealingActive = true
	t.Cleanup(func() { sealingActive = false })

	first := fieldsOf(t, e.do("VMEM.RESEAL", "alice"))
	if first["resealed"] != "1" {
		t.Fatalf("первый прогон перешифровал %q", first["resealed"])
	}
	second := fieldsOf(t, e.do("VMEM.RESEAL", "alice"))
	if second["resealed"] != "0" {
		t.Fatalf("повторный прогон перешифровал %q фактов, ожидался 0", second["resealed"])
	}
}

// TestReseal_DoesNotTouchOtherScopes — перешифровка через границу памяти так же
// недопустима, как стирание через неё.
func TestReseal_DoesNotTouchOtherScopes(t *testing.T) {
	e := newExecEnv(t)
	enableKeyring(t, e.dir)
	e.do("VMEM.REMEMBER", "alice", "TEXT", "факт алисы", "SOURCE", "agent-a")
	e.do("VMEM.REMEMBER", "bob", "TEXT", "факт боба", "SOURCE", "agent-a")
	sealingActive = true
	t.Cleanup(func() { sealingActive = false })

	if f := fieldsOf(t, e.do("VMEM.RESEAL", "alice")); f["resealed"] != "1" {
		t.Fatalf("перешифровано %q фактов алисы", f["resealed"])
	}
	for _, rep := range e.do("VMEM.COVERAGE", "bob").Array {
		f := fieldsOf(t, rep)
		if f["sealed"] != "0" {
			t.Fatalf("перешифровка алисы задела скоуп боба: %+v", f)
		}
	}
}

// TestReseal_RecordedInChain — сдвиг границы покрытия обязан быть в журнале:
// после него VMEM.SHRED обещает больше, чем обещал вчера.
func TestReseal_RecordedInChain(t *testing.T) {
	e := newExecEnv(t)
	dir := enableAuditChain(t)
	enableKeyring(t, e.dir)
	e.do("VMEM.REMEMBER", "alice", "TEXT", "факт", "SOURCE", "agent-a")
	sealingActive = true
	t.Cleanup(func() { sealingActive = false })

	e.do("VMEM.RESEAL", "alice")

	var found bool
	for _, l := range chainLeaves(t, dir) {
		if l.Type == 7 && l.Scope == "alice" { // EventReseal
			found = true
		}
	}
	if !found {
		t.Fatal("перешифровка не попала в цепь — граница покрытия сдвинулась молча")
	}
}
