package vector

import (
	"bytes"
	"testing"
)

// TestTombstone_ResurrectionAfterSnapshot — репро P0-1.
//
// Удаление вектора, уже находящегося во frozen-сегменте, ставит tombstone
// (hard-delete возможен только для дельты — сегменты иммутабельны). Tombstone
// хранится отдельной in-memory COW-мапой. Если SaveBinary её не сериализует,
// то после рестарта из снапшота (SaveBinary → LoadBinary) удалённый вектор
// возвращается «живым».
//
// Сценарий = эквивалент «удалил → снапшот до merge → старый WAL с OpVSimDel
// удалён → рестарт» на уровне движка:
//  1. Add вектор, FlushDeltaSync → он во frozen-сегменте.
//  2. Delete → tombstone (не hard-delete).
//  3. Sanity: в живом сторе Get его не видит.
//  4. SaveBinary → LoadBinary в свежий стор.
//  5. Если воскрес — диагноз P0-1 подтверждён.
func TestTombstone_ResurrectionAfterSnapshot(t *testing.T) {
	const dim = 8
	mk := func(seed float32) []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = seed + float32(i)
		}
		return v
	}

	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		EfConstruction: 64,
		EfSearch:       64,
	}

	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Close()

	if err := lvs.Add("victim", mk(1)); err != nil {
		t.Fatal(err)
	}
	if err := lvs.Add("keep", mk(100)); err != nil {
		t.Fatal(err)
	}

	// Заморозить дельту → теперь Delete идёт по tombstone-пути.
	lvs.FlushDeltaSync()

	if !lvs.Delete("victim") {
		t.Fatal("Delete(victim) == false — ожидали tombstone в сегменте (вектор не во frozen?)")
	}

	// Sanity: в живом сторе victim удалён.
	if _, ok := lvs.Get("victim"); ok {
		t.Fatal("victim виден в живом сторе сразу после Delete — это другой баг, не P0-1")
	}

	// Снапшот → «рестарт».
	var buf bytes.Buffer
	if err := lvs.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	restored := NewLeveledVectorStore(cfg)
	defer restored.Close()

	if err := restored.LoadBinary(&buf); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}

	// keep должен пережить рестарт (контроль, что снапшот вообще рабочий).
	if _, ok := restored.Get("keep"); !ok {
		t.Fatal("keep пропал после рестарта — снапшот сломан, тест невалиден")
	}

	if _, ok := restored.Get("victim"); ok {
		t.Fatal("РЕПРО P0-1: удалённый 'victim' ВОСКРЕС после SaveBinary→LoadBinary " +
			"(tombstone не попал в снапшот)")
	}

	// И в поиске tombstone тоже должен держаться: запрос ровно вектором victim
	// не имеет права вернуть victim среди результатов.
	res, err := restored.Search(mk(1), 5, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range res {
		if r.Key == "victim" {
			t.Fatal("РЕПРО P0-1: 'victim' воскрес и виден в Search после рестарта")
		}
	}
}
