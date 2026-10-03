package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kvstore/kvstore/vector"
)

func newTestVectorStore(t *testing.T) *vector.LeveledVectorStore {
	t.Helper()
	vec := vector.NewLeveledVectorStore(vector.LeveledConfig{Distance: vector.EuclideanDistance, NumBuilders: 1})
	t.Cleanup(vec.Close)
	return vec
}

// TestLoadVectorSnapshot — снапшот векторов, который есть, но не читается,
// останавливает старт: после компакции это единственная копия старых векторов.
// Прежде сервер писал «will rebuild from WAL» и стартовал без них (проверено
// живьём: 50 векторов до COMPACT, порча одного байта — после рестарта в
// индексе один вектор, записанный уже после компакции).
func TestLoadVectorSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graph_leveled.bin")

	// Файла нет — чистый старт, не ошибка.
	if loaded, err := loadVectorSnapshot(newTestVectorStore(t), path); loaded || err != nil {
		t.Fatalf("нет файла: loaded=%v err=%v, ожидалось false, nil", loaded, err)
	}

	src := newTestVectorStore(t)
	for i := 0; i < 20; i++ {
		if err := src.Add("v"+string(rune('a'+i)), []float32{float32(i), 1, 2, 3}); err != nil {
			t.Fatal(err)
		}
	}
	src.FlushDeltaSync()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.SaveBinary(f); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := newTestVectorStore(t)
	if loaded, err := loadVectorSnapshot(dst, path); !loaded || err != nil {
		t.Fatalf("целый файл: loaded=%v err=%v", loaded, err)
	}
	if n, _, _ := dst.Info(); n != 20 {
		t.Fatalf("загружено %d векторов, ожидалось 20", n)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadVectorSnapshot(newTestVectorStore(t), path)
	if loaded || err == nil {
		t.Fatalf("испорченный файл: loaded=%v err=%v — старт без старых векторов молча потерял бы их", loaded, err)
	}
	if !strings.Contains(err.Error(), "only copy") {
		t.Fatalf("ошибка не объясняет, почему старт остановлен: %v", err)
	}
}
