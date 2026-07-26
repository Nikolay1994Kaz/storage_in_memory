package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kvstore/kvstore/internal/wal"
)

// =============================================================================
// Восстановление на момент (примитив 3). Живой сквозной сценарий проверяется на
// настоящем бинаре (запуск с -restore-to-lsn); здесь — те части, которые могут
// тихо разъехаться при правках: честная граница достижимости, изоляция
// каталога данных и читаемость журнала.
// =============================================================================

// TestCheckRestoreReachable — граница честности: цель раньше любого снапшота
// недостижима, и отказ обязан назвать, откуда можно.
func TestCheckRestoreReachable(t *testing.T) {
	defer func(prev uint64) { restoreLSN = prev }(restoreLSN)

	cases := []struct {
		name         string
		lsn          uint64
		vec, snap    uint64
		wantErr      bool
		wantMentions string
	}{
		{"режим выключен — проверять нечего", 0, 100, 100, false, ""},
		{"цель позже обоих снапшотов", 150, 100, 120, false, ""},
		{"цель ровно на watermark достижима", 100, 100, 100, false, ""},
		{"цель раньше векторного снапшота", 50, 100, 0, true, "LSN 100"},
		{"цель раньше KV-снапшота", 50, 0, 120, true, "LSN 120"},
		{"самый ранний = максимум из двух", 50, 100, 120, true, "LSN 120"},
	}
	for _, tc := range cases {
		restoreLSN = tc.lsn
		err := checkRestoreReachable(tc.vec, tc.snap)
		if tc.wantErr != (err != nil) {
			t.Errorf("%s: err=%v, ожидалась ошибка=%v", tc.name, err, tc.wantErr)
			continue
		}
		if tc.wantErr && !strings.Contains(err.Error(), tc.wantMentions) {
			t.Errorf("%s: отказ обязан называть самый ранний достижимый LSN, а сказал: %v", tc.name, err)
		}
	}
}

// TestRestoreOutDirIsolation — в режиме восстановления процесс пишет НЕ в
// каталог данных: иначе расследование портит вещдок, по которому ведётся.
func TestRestoreOutDirIsolation(t *testing.T) {
	defer func(prev uint64) { restoreLSN = prev }(restoreLSN)
	data := t.TempDir()

	restoreLSN = 0
	out, cleanup, err := restoreOutDir(data)
	if err != nil {
		t.Fatalf("restoreOutDir: %v", err)
	}
	cleanup()
	if out != data {
		t.Errorf("обычный режим: out=%q, ожидался сам каталог данных %q", out, data)
	}

	restoreLSN = 42
	out, cleanup, err = restoreOutDir(data)
	if err != nil {
		t.Fatalf("restoreOutDir(restore): %v", err)
	}
	if out == data {
		t.Fatal("режим восстановления пишет в каталог данных — оригинал под угрозой")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("временный каталог не создан: %v", err)
	}
	cleanup()
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("временный каталог пережил уборку: %v", err)
	}
}

// TestInspectWAL — журнал читаем: без него -restore-to-lsn нечем пользоваться,
// потому что момент называется номером, а номер надо где-то увидеть.
func TestInspectWAL(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal_20260726_000000.log"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	w.Write(wal.Entry{Op: wal.OpSet, Key: "vmem:f1", Value: []byte("дедлайн март")})
	w.Write(wal.Entry{Op: wal.OpVSimAddDoc, Key: "f1", Value: []byte("doc")})
	w.Write(wal.Entry{Op: wal.OpVSimAddDocBatch, Key: "f2", Value: []byte("pair")})
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	var out bytes.Buffer
	if err := inspectWAL(dir, &out); err != nil {
		t.Fatalf("inspectWAL: %v", err)
	}
	got := out.String()
	for _, want := range []string{"SET", "VSIM.ADDDOC", "VSIM.ADDDOC.BATCH", "vmem:f1", "f2"} {
		if !strings.Contains(got, want) {
			t.Errorf("в выводе нет %q:\n%s", want, got)
		}
	}

	// Пустой каталог не паникует и говорит об этом словами.
	out.Reset()
	if err := inspectWAL(t.TempDir(), &out); err != nil {
		t.Fatalf("inspectWAL(empty): %v", err)
	}
	if !strings.Contains(out.String(), "no wal_") {
		t.Errorf("пустой каталог: %q", out.String())
	}
}
