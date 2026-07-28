package main

// Сквозная проверка второй половины криптостирания: бинарный снапшот,
// снятый ДО VMEM.SHRED, после уничтожения ключа факт не воскрешает.
//
// Тест ходит через НАСТОЯЩИЙ кейринг и через ту же snapshotCryptoFor, что
// зовёт main — не через зеркало. Зеркало разошлось бы с оригиналом молча.

import (
	"bytes"
	"strings"
	"testing"

	"kvstore/kvstore/vector"
)

func TestSnapshotBinary_ShredDoesNotResurrectFacts(t *testing.T) {
	e := newExecEnv(t)
	ring := enableKeyring(t, e.dir)
	lvs := e.vec.(*vector.LeveledVectorStore)
	lvs.SetSnapshotCrypto(snapshotCryptoFor(ring, true))

	const aliceText = "aurora contract signed with the steering committee"
	const bobText = "standup happens every morning"
	e.do("VMEM.REMEMBER", "alice", "TEXT", aliceText)
	e.do("VMEM.REMEMBER", "bob", "TEXT", bobText)
	lvs.FlushDeltaSync()

	var snap bytes.Buffer
	if err := lvs.SaveBinary(&snap); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	// Содержания факта в снапшоте быть не должно — ни термов, ни текста.
	raw := snap.String()
	for _, secret := range []string{"aurora", "steering", "standup"} {
		if strings.Contains(raw, secret) {
			t.Errorf("терм %q найден в бинарном снапшоте открытым текстом", secret)
		}
	}
	// ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ: снапшот не пуст и содержит документы —
	// иначе проверка выше была бы верна и на пустом файле.
	aliceIDs := recallIDs(t, e, "alice", aliceText)
	if len(aliceIDs) == 0 {
		t.Fatal("факт не найден в живом сторе — проверять нечего")
	}
	if !strings.Contains(raw, aliceIDs[0]) {
		t.Fatal("ключ факта не найден в снапшоте — снапшот пуст, проверка прошла бы по неверной причине")
	}

	// ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ ВОССТАНОВЛЕНИЯ: до уничтожения ключа снапшот
	// отдаёт факты полностью. Без этого «не воскрес» проходило бы и в случае,
	// когда снапшот битый.
	before := vector.NewLeveledVectorStore(vector.LeveledConfig{
		Distance: vector.EuclideanDistance, NumBuilders: 1,
	})
	before.SetSnapshotCrypto(snapshotCryptoFor(ring, true))
	defer before.Clear()
	if err := before.LoadBinary(bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("LoadBinary до стирания: %v", err)
	}
	if _, ok := before.Get(aliceIDs[0]); !ok {
		t.Fatal("факт не восстановился из снапшота ДО стирания — дальше проверять нечего")
	}

	// VMEM.SHRED: память чистится, ключ уничтожается.
	if v := e.do("VMEM.SHRED", "alice"); v.Typ == '-' {
		t.Fatalf("VMEM.SHRED: %v", v.Str)
	}

	after := vector.NewLeveledVectorStore(vector.LeveledConfig{
		Distance: vector.EuclideanDistance, NumBuilders: 1,
	})
	after.SetSnapshotCrypto(snapshotCryptoFor(ring, true))
	defer after.Clear()
	if err := after.LoadBinary(bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("LoadBinary после стирания: %v", err)
	}

	for _, id := range aliceIDs {
		if _, ok := after.Get(id); ok {
			t.Errorf("факт стёртого скоупа %s воскрес из бинарного снапшота", id)
		}
	}

	// Соседний скоуп обязан уцелеть: иначе это потеря данных, а не стирание.
	bobIDs := recallIDs(t, e, "bob", bobText)
	if len(bobIDs) == 0 {
		t.Fatal("факт соседнего скоупа исчез из живого стора после чужого SHRED")
	}
	for _, id := range bobIDs {
		if _, ok := after.Get(id); !ok {
			t.Errorf("факт соседнего скоупа %s не пережил стирание", id)
		}
	}
}
