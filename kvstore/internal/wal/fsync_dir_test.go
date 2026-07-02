package wal

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFsyncDir — страж durability атомарной замены файлов. FsyncDir делает
// последний rename/create в каталоге durable; без него power-loss мог откатить
// переименование snapshot.wal/graph_leveled.bin при уже удалённых старых WAL.
//
// Сам fsync каталога здесь не наблюдаем, но фиксируем контракт: для реального
// каталога — nil, для несуществующего пути — ошибка (не тихий no-op).
func TestFsyncDir(t *testing.T) {
	dir := t.TempDir()

	// Имитируем атомарную замену: temp → final, затем fsync каталога.
	tmp := filepath.Join(dir, "f.tmp")
	final := filepath.Join(dir, "f")
	if err := os.WriteFile(tmp, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := FsyncDir(dir); err != nil {
		t.Fatalf("FsyncDir на существующем каталоге вернул ошибку: %v", err)
	}

	if err := FsyncDir(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("FsyncDir на несуществующем пути должен вернуть ошибку, а не тихий nil")
	}
}
