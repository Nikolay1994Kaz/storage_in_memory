package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════
// LSN (Log Sequence Number) — курсор для будущей резюмируемой репликации.
//
// Инварианты, которые стерегут эти тесты:
//  1. LSN монотонно растут в порядке записи внутри файла.
//  2. Нумерация непрерывна сквозь ротацию WAL (не сбрасывается, без дыр).
//  3. Watermark в snapshot защищает nextLSN от отката после компакции + краша
//     до появления новых записей (иначе номера переиспользуются → реплика ломается).
//  4. Чужой/несовместимый файл отвергается громкой ошибкой (магия + версия).
//  5. Обрезанный хвост при краше даёт валидный префикс с корректными LSN.
// ═══════════════════════════════════════════════════════════════════════════

// waitFor опрашивает cond до истины или таймаута.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// TestLSN_MonotonicInFile — LSN идут 1,2,3,... в порядке записи.
func TestLSN_MonotonicInFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_0001.log")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const n = 10
	for i := 0; i < n; i++ {
		if err := w.Write(Entry{Op: OpSet, Key: fmt.Sprintf("k%d", i), Value: []byte("v")}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	w.Sync()
	w.Close()

	base, entries, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if base != walBaseLSN {
		t.Fatalf("WAL baseLSN: got %d, want %d", base, walBaseLSN)
	}
	if len(entries) != n {
		t.Fatalf("got %d entries, want %d", len(entries), n)
	}
	for i, e := range entries {
		if e.LSN != uint64(i+1) {
			t.Fatalf("entry %d: LSN=%d, want %d", i, e.LSN, i+1)
		}
	}
}

// TestLSN_ContinuousThroughRotate — LSN не сбрасывается и не прыгает при ротации.
func TestLSN_ContinuousThroughRotate(t *testing.T) {
	dir := t.TempDir()
	path1 := filepath.Join(dir, "wal_0001.log")
	w, err := Open(path1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Первые 5 записей → LSN 1..5
	for i := 0; i < 5; i++ {
		w.Write(Entry{Op: OpSet, Key: fmt.Sprintf("a%d", i), Value: []byte("v")})
	}

	path2 := filepath.Join(dir, "wal_0002.log")
	oldPath, err := w.Rotate(path2)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if oldPath != path1 {
		t.Fatalf("Rotate returned %q, want %q", oldPath, path1)
	}

	// Ещё 5 записей → LSN 6..10 в новом файле
	for i := 0; i < 5; i++ {
		w.Write(Entry{Op: OpSet, Key: fmt.Sprintf("b%d", i), Value: []byte("v")})
	}
	w.Sync()
	w.Close()

	_, e1, err := ReadFile(path1)
	if err != nil {
		t.Fatalf("ReadFile wal1: %v", err)
	}
	_, e2, err := ReadFile(path2)
	if err != nil {
		t.Fatalf("ReadFile wal2: %v", err)
	}

	all := append(append([]Entry{}, e1...), e2...)
	if len(all) != 10 {
		t.Fatalf("got %d entries across both files, want 10", len(all))
	}
	for i, e := range all {
		if e.LSN != uint64(i+1) {
			t.Fatalf("entry %d: LSN=%d, want %d (continuity broken at rotation)", i, e.LSN, i+1)
		}
	}
}

// TestLSN_WatermarkSurvivesCompaction — ГЛАВНЫЙ страж.
//
// Сценарий: пишем 100 записей (LSN 1..100) → компакция (rotate + snapshot,
// удаление старого WAL) → новый WAL пуст. Симулируем recovery: без watermark
// nextLSN восстановился бы в 1 и переиспользовал номера 1..100. С watermark
// в заголовке snapshot recovery видит «покрыто до 100» → nextLSN=101.
func TestLSN_WatermarkSurvivesCompaction(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "wal_0001.log"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const n = 100
	for i := 0; i < n; i++ {
		w.Write(Entry{Op: OpSet, Key: fmt.Sprintf("k%d", i), Value: []byte("v")})
	}
	w.Sync()
	lastLSN := w.LastLSN()
	if lastLSN != n {
		t.Fatalf("LastLSN before compaction: got %d, want %d", lastLSN, n)
	}

	iterate := func(fn func(op byte, key string, value []byte)) {
		for i := 0; i < n; i++ {
			fn(OpSet, fmt.Sprintf("k%d", i), []byte("v"))
		}
	}
	BackgroundCompact(w, dir, iterate, nil)

	// Ждём завершения фонового snapshot + очистки старого WAL.
	waitFor(t, 3*time.Second, func() bool {
		if _, err := os.Stat(filepath.Join(dir, "snapshot.wal")); err != nil {
			return false
		}
		if _, err := os.Stat(filepath.Join(dir, "wal_0001.log")); !os.IsNotExist(err) {
			return false // старый WAL ещё не удалён
		}
		return true
	})
	w.Close()

	// Симулируем recovery-пересчёт maxLSN (как в cmd/kvstore/main.go).
	var maxLSN uint64
	bump := func(v uint64) {
		if v > maxLSN {
			maxLSN = v
		}
	}
	snapBase, _, err := ReadFile(filepath.Join(dir, "snapshot.wal"))
	if err != nil {
		t.Fatalf("ReadFile snapshot: %v", err)
	}
	bump(snapBase)

	matches, _ := filepath.Glob(filepath.Join(dir, "wal_*.log"))
	sawEmptyNewWAL := false
	for _, p := range matches {
		b, entries, err := ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", p, err)
		}
		bump(b)
		if len(entries) == 0 {
			sawEmptyNewWAL = true
		}
		for _, e := range entries {
			bump(e.LSN)
		}
	}
	if !sawEmptyNewWAL {
		t.Fatal("expected an empty post-compaction WAL in the scenario")
	}

	if maxLSN != lastLSN {
		t.Fatalf("watermark lost: recovered maxLSN=%d, want %d (nextLSN would reuse numbers)", maxLSN, lastLSN)
	}
	// nextLSN = maxLSN+1 = 101 — номера 1..100 не переиспользуются.
}

// TestFileHeader_RejectsBadMagic — чужой файл отвергается, а не парсится как мусор.
func TestFileHeader_RejectsBadMagic(t *testing.T) {
	dir := t.TempDir()

	badMagic := filepath.Join(dir, "alien.wal")
	// 13+ байт с неправильной магией.
	if err := os.WriteFile(badMagic, []byte("XXXXX\x01\x00\x00\x00\x00\x00\x00\x00extra"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFile(badMagic); err == nil {
		t.Fatal("expected error on bad magic, got nil")
	}

	// Правильная магия, но незнакомая версия.
	badVer := filepath.Join(dir, "future.wal")
	buf := make([]byte, fileHeaderSize)
	copy(buf[0:4], fileMagic[:])
	buf[4] = fileFormatVersion + 99
	if err := os.WriteFile(badVer, buf, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFile(badVer); err == nil {
		t.Fatal("expected error on unsupported version, got nil")
	}
}

// TestFileHeader_EmptyAndHeaderOnly — пустой и «только заголовок» файлы = 0 записей, не ошибка.
func TestFileHeader_EmptyAndHeaderOnly(t *testing.T) {
	dir := t.TempDir()

	// 0-байтовый файл (свежесозданный, заголовок ещё не долетел до диска).
	empty := filepath.Join(dir, "empty.wal")
	os.WriteFile(empty, nil, 0644)
	if base, entries, err := ReadFile(empty); err != nil || base != 0 || len(entries) != 0 {
		t.Fatalf("empty file: base=%d entries=%d err=%v; want 0/0/nil", base, len(entries), err)
	}

	// Только заголовок, без записей (Open+Sync+Close без Write).
	hdrOnly := filepath.Join(dir, "wal_0001.log")
	w, _ := Open(hdrOnly)
	w.Sync()
	w.Close()
	if base, entries, err := ReadFile(hdrOnly); err != nil || base != 0 || len(entries) != 0 {
		t.Fatalf("header-only file: base=%d entries=%d err=%v; want 0/0/nil", base, len(entries), err)
	}
}

// TestLSN_TruncatedTail — обрезанный при краше хвост даёт валидный префикс с LSN.
func TestLSN_TruncatedTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_0001.log")
	w, _ := Open(path)
	for i := 0; i < 5; i++ {
		w.Write(Entry{Op: OpSet, Key: fmt.Sprintf("k%d", i), Value: []byte("val")})
	}
	w.Sync()
	w.Close()

	// Дописываем мусорный «полузаписанный» хвост (crash mid-write).
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	f.Write([]byte{0xDE, 0xAD, 0xBE}) // обрезанный header записи
	f.Close()

	_, entries, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5 (valid prefix before truncated tail)", len(entries))
	}
	for i, e := range entries {
		if e.LSN != uint64(i+1) {
			t.Fatalf("entry %d: LSN=%d, want %d", i, e.LSN, i+1)
		}
	}
}

// TestLSN_SetNextLSNResumesNumbering — recovery ставит nextLSN, новые записи продолжают.
func TestLSN_SetNextLSNResumesNumbering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_0001.log")
	w, _ := Open(path)

	// Симулируем «после recovery увидели maxLSN=1000».
	w.SetNextLSN(1001)
	w.Write(Entry{Op: OpSet, Key: "resume", Value: []byte("v")})
	w.Sync()
	w.Close()

	_, entries, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(entries) != 1 || entries[0].LSN != 1001 {
		t.Fatalf("resumed LSN: got %v, want single entry LSN=1001", entries)
	}
}
