package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckDataDirSupported — каталог, записанный с -encrypt-at-rest, не
// открывается: реплей конвертов как данных был бы тихой порчей.
func TestCheckDataDirSupported(t *testing.T) {
	dir := t.TempDir()
	if err := checkDataDirSupported(dir); err != nil {
		t.Fatalf("пустой каталог отклонён: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wal_1.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkDataDirSupported(dir); err != nil {
		t.Fatalf("обычный каталог отклонён: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, legacyKeyringFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkDataDirSupported(dir)
	if err == nil || !strings.Contains(err.Error(), "-encrypt-at-rest") {
		t.Fatalf("каталог с %s принят или ошибка не называет причину: %v", legacyKeyringFile, err)
	}
}
