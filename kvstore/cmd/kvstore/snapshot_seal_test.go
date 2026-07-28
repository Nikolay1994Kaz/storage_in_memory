package main

// Снапшот и криптостирание. Половина границы персистентности, которая до сих
// пор оставалась открытой: WAL уезжал под конвертом, а snapshot.wal — нет,
// потому что пишется обходом состояния В ПАМЯТИ, где факты лежат открытым
// текстом. Снапшот, снятый ДО VMEM.SHRED, хранил дословные якоря скоупа и
// уезжал шиппером в архив, куда удаление не дотягивается в принципе.
//
// Проверяется это двумя способами, и оба обязательны. Первый — ИНВАРИАНТ по
// всему снапшоту (а не точечная проверка одной записи): следующая забытая
// точка должна падать сама, как это уже случилось с QUARANTINE и BACKFILL.
// Второй — сквозной: после уничтожения ключа факт не воскресает из того
// самого снапшота.

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"kvstore/kvstore/internal/keyring"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// snapshotEntriesOf снимает снапшот боевым путём (тем же, что подставлен в
// iterateAll в main) и возвращает его записи.
func snapshotEntriesOf(t *testing.T, e *execEnv) []wal.Entry {
	t.Helper()
	lvs, ok := e.vec.(*vector.LeveledVectorStore)
	if !ok {
		t.Fatal("окружение без LeveledVectorStore")
	}
	var entries []wal.Entry
	snapshotIterateSealed(e.s, e.ttl, e.zset, lvs.FactScopes(), func(op byte, key string, value []byte) {
		entries = append(entries, wal.Entry{Op: op, Key: key, Value: append([]byte(nil), value...)})
	})
	return entries
}

// TestSnapshotSealsVMEMAnchors — инвариант: в снапшоте нет открытого текста
// фактов, и запечатан ровно тот, у кого есть ключ.
func TestSnapshotSealsVMEMAnchors(t *testing.T) {
	e := newExecEnv(t)
	ring := enableKeyring(t, e.dir)

	const aliceText = "Alice signed the Aurora contract on July 3"
	const bobText = "Bob prefers morning standups"
	e.do("VMEM.REMEMBER", "alice", "TEXT", aliceText)
	e.do("VMEM.REMEMBER", "bob", "TEXT", bobText)
	// Посторонний KV-ключ: у него нет скоупа, значит нет и ключа шифрования.
	e.s.Set(0, "plainkey", []byte("not a fact"))

	entries := snapshotEntriesOf(t, e)

	anchors := 0
	for _, en := range entries {
		if en.Op != wal.OpSet || !strings.HasPrefix(en.Key, vmemAnchorPrefix) {
			continue
		}
		anchors++
		if !keyring.IsEnvelope(en.Value) {
			t.Errorf("якорь %s уехал в снапшот открытым текстом", en.Key)
		}
	}

	// ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ к инварианту выше. Без него тест был бы зелёным
	// и на пустом снапшоте: «ни один якорь не открыт» верно и когда якорей нет
	// вовсе. Ровно этот класс ошибки ловил нас трижды за два дня.
	if anchors != 2 {
		t.Fatalf("в снапшоте %d якорей, ожидалось 2 — проверка прошла бы по неверной причине", anchors)
	}

	// Открытого текста не должно быть НИГДЕ в снапшоте, включая записи,
	// про которые мы не подумали.
	for _, en := range entries {
		for _, secret := range []string{aliceText, bobText} {
			if bytes.Contains(en.Value, []byte(secret)) {
				t.Errorf("текст факта найден открытым в записи %s (op=%d)", en.Key, en.Op)
			}
		}
	}

	// Посторонний ключ запечатывать НЕЧЕМ, и делать этого нельзя: у него нет
	// скоупа, а значит нет ключа, под которым его потом откроют.
	for _, en := range entries {
		if en.Key == "plainkey" && keyring.IsEnvelope(en.Value) {
			t.Error("не-VMEM ключ запечатан — под каким скоупом его открывать?")
		}
	}

	// ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ содержимого: конверт должен нести ИМЕННО факт, а
	// не что попало. Проверка «текста нет в байтах» одна прошла бы и если бы
	// мы записали в снапшот мусор.
	opened := 0
	for _, en := range entries {
		if !keyring.IsEnvelope(en.Value) {
			continue
		}
		plain, err := ring.Unseal(en.Value)
		if err != nil {
			t.Fatalf("Unseal(%s): %v", en.Key, err)
		}
		if string(plain) == aliceText || string(plain) == bobText {
			opened++
		}
	}
	if opened != 2 {
		t.Errorf("развернулось %d фактов, ожидалось 2 — конверт несёт не то, что должен", opened)
	}
}

// TestSnapshotAnchorDoesNotSurviveShred — то, ради чего всё делается: снапшот
// снят ДО стирания, но после уничтожения ключа факт из него не воскресает.
// Соседний скоуп при этом обязан уцелеть — иначе «стирание» окажется просто
// потерей данных.
func TestSnapshotAnchorDoesNotSurviveShred(t *testing.T) {
	e := newExecEnv(t)
	ring := enableKeyring(t, e.dir)

	const secret = "Project Aurora cancelled by the steering committee"
	const keep = "Erik prefers morning standups"
	e.do("VMEM.REMEMBER", "alice", "TEXT", secret)
	e.do("VMEM.REMEMBER", "bob", "TEXT", keep)

	// Снимаем снапшот ДО стирания — это и есть та копия, которую удаление
	// догнать не может: она уже уехала бы в архив.
	dir := t.TempDir()
	sw := wal.NewSnapshotWriter(dir)
	lvs := e.vec.(*vector.LeveledVectorStore)
	if err := sw.WriteSnapshot(1, func(fn func(byte, string, []byte)) {
		snapshotIterateSealed(e.s, e.ttl, e.zset, lvs.FactScopes(), fn)
	}); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	snapPath := filepath.Join(dir, "snapshot.wal")

	// ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ: до уничтожения ключа снапшот факт ВОССТАНАВЛИВАЕТ.
	// Без этого шага проверка ниже проходила бы и в случае, когда снапшот
	// пуст или битый, — то есть по неверной причине.
	before := newExecEnv(t)
	replaySealedWAL(t, snapPath, before, ring)
	if _, ok := before.s.Get(anchorKeyOf(t, e, "alice", secret)); !ok {
		t.Fatal("факт не восстановился из снапшота ДО стирания — дальше проверять нечего")
	}

	if _, _, err := ring.Destroy("alice"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	after := newExecEnv(t)
	skipped := replaySealedWAL(t, snapPath, after, ring)
	if skipped == 0 {
		t.Error("реплей снапшота не пропустил ни одной записи — ключ уничтожен, а стирание не сработало")
	}

	for _, en := range mustReadEntries(t, snapPath) {
		if !strings.HasPrefix(en.Key, vmemAnchorPrefix) {
			continue
		}
		if v, ok := after.s.Get(en.Key); ok && bytes.Contains(v, []byte(secret)) {
			t.Errorf("стёртый факт воскрес из снапшота под ключом %s", en.Key)
		}
	}

	// Соседний скоуп цел: стирание обязано быть выборочным.
	survived := false
	for _, en := range mustReadEntries(t, snapPath) {
		if v, ok := after.s.Get(en.Key); ok && bytes.Contains(v, []byte(keep)) {
			survived = true
		}
	}
	if !survived {
		t.Error("факт соседнего скоупа не пережил стирание — это потеря данных, а не erasure")
	}
}

func mustReadEntries(t *testing.T, path string) []wal.Entry {
	t.Helper()
	_, entries, err := wal.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return entries
}

// anchorKeyOf находит KV-ключ якоря по тексту факта в живом сторе.
func anchorKeyOf(t *testing.T, e *execEnv, scope, text string) string {
	t.Helper()
	for _, id := range recallIDs(t, e, scope, text) {
		key := vmemAnchorPrefix + id
		if v, ok := e.s.Get(key); ok && string(v) == text {
			return key
		}
	}
	t.Fatalf("якорь факта %q в скоупе %s не найден", text, scope)
	return ""
}
