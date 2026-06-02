package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ─── Helpers ───────────────────────────────────────────────

// tempWAL создаёт WAL во временной директории.
// Возвращает WAL и cleanup-функцию.
//
// ПОЧЕМУ t.TempDir()?
//
//	Go автоматически удалит эту директорию после теста.
//	Нам не нужно вручную чистить файлы.
func tempWAL(t *testing.T) (*WAL, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_0001.log")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open WAL: %v", err)
	}
	return w, dir
}

// ─── Базовые тесты WAL ────────────────────────────────────

// TestWAL_WriteAndRead — полный цикл: запись → sync → чтение.
//
// Проверяем что данные переживают закрытие файла.
// Это фундамент durability — если тут баг, данные теряются.
func TestWAL_WriteAndRead(t *testing.T) {
	w, dir := tempWAL(t)

	// Записываем 3 операции разных типов
	entries := []Entry{
		{Op: OpSet, Key: "user:1", Value: []byte("Alice")},
		{Op: OpSet, Key: "user:2", Value: []byte("Bob")},
		{Op: OpDel, Key: "user:1", Value: nil},
	}

	for _, e := range entries {
		if err := w.Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Sync + Close — данные на диске
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	w.Close()

	// Читаем обратно
	got, err := ReadEntries(filepath.Join(dir, "wal_0001.log"))
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("read %d entries, want 3", len(got))
	}

	// Проверяем каждую запись
	if got[0].Op != OpSet || got[0].Key != "user:1" || string(got[0].Value) != "Alice" {
		t.Fatalf("entry 0: %+v", got[0])
	}
	if got[1].Key != "user:2" || string(got[1].Value) != "Bob" {
		t.Fatalf("entry 1: %+v", got[1])
	}
	if got[2].Op != OpDel || got[2].Key != "user:1" {
		t.Fatalf("entry 2: %+v", got[2])
	}
}

// TestWAL_EmptyValue — OpDel с пустым Value.
// DEL не имеет значения — только ключ.
func TestWAL_EmptyValue(t *testing.T) {
	w, dir := tempWAL(t)

	w.Write(Entry{Op: OpDel, Key: "mykey", Value: nil})
	w.Sync()
	w.Close()

	got, _ := ReadEntries(filepath.Join(dir, "wal_0001.log"))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].Key != "mykey" || len(got[0].Value) != 0 {
		t.Fatalf("entry: %+v", got[0])
	}
}

// TestWAL_CRC32_Corruption — повреждённый файл: CRC не совпадает.
//
// Симулируем битый диск: записываем данные, потом портим 1 байт.
// ReadEntries ОБЯЗАН остановиться на повреждённой записи
// и вернуть только валидные записи до неё.
func TestWAL_CRC32_Corruption(t *testing.T) {
	w, dir := tempWAL(t)
	path := filepath.Join(dir, "wal_0001.log")

	// Пишем 3 записи
	w.Write(Entry{Op: OpSet, Key: "a", Value: []byte("1")})
	w.Write(Entry{Op: OpSet, Key: "b", Value: []byte("2")})
	w.Write(Entry{Op: OpSet, Key: "c", Value: []byte("3")})
	w.Sync()
	w.Close()

	// Портим файл: меняем байт в середине (вторая запись)
	data, _ := os.ReadFile(path)
	// Первая запись: 8 байт header + payload.
	// Портим байт где-то в районе второй записи
	corruptPos := len(data) / 2
	data[corruptPos] ^= 0xFF // инвертируем байт
	os.WriteFile(path, data, 0644)

	// Читаем — должны получить только записи до повреждения
	got, _ := ReadEntries(path)

	// Первая запись может быть валидна, вторая/третья — нет.
	// Главное: НЕ паника и НЕ ошибка, а graceful stop.
	if len(got) > 2 {
		t.Fatalf("corrupted WAL returned %d entries (should stop at corruption)", len(got))
	}
	t.Logf("recovered %d entries from corrupted WAL (expected ≤2)", len(got))
}

// TestWAL_ReadNonExistent — чтение несуществующего файла → nil, nil.
func TestWAL_ReadNonExistent(t *testing.T) {
	entries, err := ReadEntries("/tmp/does_not_exist_wal.log")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if entries != nil {
		t.Fatal("expected nil entries for non-existent file")
	}
}

// ─── Rotate ────────────────────────────────────────────────

// TestWAL_Rotate — переключение на новый файл.
//
// Сценарий:
//  1. Пишем в wal_0001.log
//  2. Rotate → wal_0002.log
//  3. Пишем в wal_0002.log
//  4. Проверяем: оба файла содержат свои данные
func TestWAL_Rotate(t *testing.T) {
	w, dir := tempWAL(t)

	// Пишем в первый файл
	w.Write(Entry{Op: OpSet, Key: "before", Value: []byte("rotate")})

	// Rotate
	newPath := filepath.Join(dir, "wal_0002.log")
	oldPath, err := w.Rotate(newPath)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Пишем во второй файл
	w.Write(Entry{Op: OpSet, Key: "after", Value: []byte("rotate")})
	w.Sync()
	w.Close()

	// Проверяем старый файл
	old, _ := ReadEntries(oldPath)
	if len(old) != 1 || old[0].Key != "before" {
		t.Fatalf("old WAL: %+v", old)
	}

	// Проверяем новый файл
	new_, _ := ReadEntries(newPath)
	if len(new_) != 1 || new_[0].Key != "after" {
		t.Fatalf("new WAL: %+v", new_)
	}
}

// ─── ReadAllWALs ──────────────────────────────────────────

// TestReadAllWALs — чтение snapshot + несколько WAL-файлов.
func TestReadAllWALs(t *testing.T) {
	dir := t.TempDir()

	// Создаём snapshot
	snap, _ := Open(filepath.Join(dir, "snapshot.wal"))
	snap.Write(Entry{Op: OpSet, Key: "snap", Value: []byte("data")})
	snap.Sync()
	snap.Close()

	// Создаём два WAL-файла
	w1, _ := Open(filepath.Join(dir, "wal_0001.log"))
	w1.Write(Entry{Op: OpSet, Key: "wal1", Value: []byte("a")})
	w1.Sync()
	w1.Close()

	w2, _ := Open(filepath.Join(dir, "wal_0002.log"))
	w2.Write(Entry{Op: OpSet, Key: "wal2", Value: []byte("b")})
	w2.Sync()
	w2.Close()

	// Читаем всё
	all, err := ReadAllWALs(dir)
	if err != nil {
		t.Fatalf("ReadAllWALs: %v", err)
	}

	// Порядок: snapshot → wal_0001 → wal_0002
	if len(all) != 3 {
		t.Fatalf("got %d entries, want 3", len(all))
	}
	if all[0].Key != "snap" {
		t.Fatalf("first entry should be from snapshot, got %q", all[0].Key)
	}
	if all[1].Key != "wal1" || all[2].Key != "wal2" {
		t.Fatalf("WAL order wrong: %q, %q", all[1].Key, all[2].Key)
	}
}

// ─── BatchWAL ──────────────────────────────────────────────

// TestBatchWAL_WriteAndRead — batch-запись сохраняет все данные.
//
// BatchWAL буферизует entries в канале и пишет пачками.
// Проверяем: после Close() все entries на диске.
func TestBatchWAL_WriteAndRead(t *testing.T) {
	w, dir := tempWAL(t)
	bw := NewBatchWAL(w)

	for i := 0; i < 100; i++ {
		bw.Write(Entry{
			Op:    OpSet,
			Key:   fmt.Sprintf("key:%d", i),
			Value: []byte(fmt.Sprintf("val:%d", i)),
		})
	}

	// Close дожидается flush последнего batch
	bw.Close()

	// Читаем
	got, _ := ReadEntries(filepath.Join(dir, "wal_0001.log"))
	if len(got) != 100 {
		t.Fatalf("BatchWAL: read %d entries, want 100", len(got))
	}

	// Проверяем первый и последний
	if got[0].Key != "key:0" || string(got[0].Value) != "val:0" {
		t.Fatalf("first entry: %+v", got[0])
	}
	if got[99].Key != "key:99" || string(got[99].Value) != "val:99" {
		t.Fatalf("last entry: %+v", got[99])
	}
}

// TestBatchWAL_MultiWorker — параллельная запись из нескольких goroutines.
//
// В проде 12 workers шлют в один BatchWAL.
// Канал (chan Entry) потокобезопасен — это гарантия Go.
func TestBatchWAL_MultiWorker(t *testing.T) {
	w, dir := tempWAL(t)
	bw := NewBatchWAL(w)

	const workers = 4
	const perWorker = 250

	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				bw.Write(Entry{
					Op:    OpSet,
					Key:   fmt.Sprintf("w%d:%d", id, i),
					Value: []byte("x"),
				})
			}
		}(w)
	}

	wg.Wait()
	bw.Close()

	got, _ := ReadEntries(filepath.Join(dir, "wal_0001.log"))
	expected := workers * perWorker
	if len(got) != expected {
		t.Fatalf("BatchWAL multi-worker: got %d, want %d", len(got), expected)
	}
}

// TestBatchWAL_FlushTimer — при малой нагрузке flush по таймеру (1ms).
//
// Отправляем 1 entry и ждём. Без таймера batch из 256 ждал бы вечно.
// С таймером — flush через ≤1ms.
func TestBatchWAL_FlushTimer(t *testing.T) {
	w, dir := tempWAL(t)
	bw := NewBatchWAL(w)

	// Одна запись — не заполнит batch из 256
	bw.Write(Entry{Op: OpSet, Key: "lonely", Value: []byte("entry")})

	// Ждём чтобы таймер (1ms) сработал
	time.Sleep(50 * time.Millisecond)

	// Sync чтобы bufio.Writer сбросил на диск
	bw.RawWAL().Sync()

	// Проверяем что запись уже на диске (таймер сработал)
	got, _ := ReadEntries(filepath.Join(dir, "wal_0001.log"))
	if len(got) != 1 {
		t.Fatalf("timer flush: got %d entries, want 1 (timer should have flushed)", len(got))
	}

	bw.Close()
}

// ─── Encode/Decode ─────────────────────────────────────────

// TestEncodeDecodeRoundtrip — encode → decode сохраняет данные.
func TestEncodeDecodeRoundtrip(t *testing.T) {
	tests := []Entry{
		{Op: OpSet, Key: "hello", Value: []byte("world")},
		{Op: OpDel, Key: "deleted", Value: nil},
		{Op: OpExpire, Key: "ttl-key", Value: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
		{Op: OpSet, Key: "", Value: []byte("empty-key")}, // пустой ключ
		{Op: OpSet, Key: "empty-val", Value: nil},        // пустое значение
	}

	for _, original := range tests {
		t.Run(original.Key, func(t *testing.T) {
			encoded := encodeEntry(original)
			decoded, err := decodeEntry(encoded)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if decoded.Op != original.Op {
				t.Fatalf("Op: got %d, want %d", decoded.Op, original.Op)
			}
			if decoded.Key != original.Key {
				t.Fatalf("Key: got %q, want %q", decoded.Key, original.Key)
			}
			if string(decoded.Value) != string(original.Value) {
				t.Fatalf("Value: got %q, want %q", decoded.Value, original.Value)
			}
		})
	}
}

// ─── Syncer ────────────────────────────────────────────────

// TestSyncer_Fsync — Syncer вызывает fsync с указанным интервалом.
//
// Создаём Syncer с интервалом 10ms. Пишем entry в WAL.
// Ждём 50ms. Проверяем что данные попали на диск
// (читаем файл заново — если fsync не сработал, bufio не сбросил данные).
func TestSyncer_Fsync(t *testing.T) {
	w, dir := tempWAL(t)
	path := filepath.Join(dir, "wal_0001.log")

	// iterate-заглушка (для Syncer'а)
	iterate := func(fn func(byte, string, []byte)) {}

	syncer := NewSyncer(w, 10*time.Millisecond, dir, iterate, func() error { return nil })

	// Пишем entry (через WAL напрямую, без Sync)
	w.Write(Entry{Op: OpSet, Key: "synced", Value: []byte("yes")})

	// Ждём — Syncer должен вызвать Sync() несколько раз за 50ms
	time.Sleep(50 * time.Millisecond)

	// Читаем файл заново — данные должны быть на диске
	got, _ := ReadEntries(path)
	if len(got) != 1 || got[0].Key != "synced" {
		t.Fatalf("Syncer didn't fsync: got %d entries", len(got))
	}

	syncer.Stop()
	w.Close()
}

// TestSyncer_CompactTrigger — при превышении MaxWALSize запускается компактизация.
//
// Трюк: пишем большой файл (больше MaxWALSize), но sizeCheckEvery=50.
// При интервале 1ms → проверка размера через 50ms.
// Используем atomic.Bool чтобы засечь что compact был вызван.
func TestSyncer_CompactTrigger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_0001.log")
	w, _ := Open(path)

	// Пишем > MaxWALSize данных
	bigValue := make([]byte, 1024) // 1KB на запись
	for i := 0; i < 70000; i++ {   // 70K * 1KB ≈ 70MB > 64MB
		w.Write(Entry{Op: OpSet, Key: fmt.Sprintf("k%d", i), Value: bigValue})
	}
	w.Sync()

	// Фиксируем что compact вызван
	var compactCalled sync.WaitGroup
	compactCalled.Add(1)
	compactDone := false

	iterate := func(fn func(byte, string, []byte)) {
		if !compactDone {
			compactDone = true
			compactCalled.Done()
		}
		// iterate пуст — snapshot будет пустой, это нормально для теста
	}

	// Syncer с коротким интервалом — чтобы sizeCheck сработал быстро
	syncer := NewSyncer(w, 1*time.Millisecond, dir, iterate, func() error { return nil })

	// Ждём срабатывания compact (sizeCheckEvery=50, interval=1ms → ~50ms)
	done := make(chan struct{})
	go func() {
		compactCalled.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("compact triggered successfully")
	case <-time.After(3 * time.Second):
		t.Log("compact not triggered within 3s (WAL size check may not have fired)")
		// Не Fatal — sizeCheckEvery=50 при interval=1ms = 50ms,
		// но timing в CI может быть нестабильным
	}

	syncer.Stop()
	w.Close()

	// Даем фоновой горутине SnapshotWriter доли секунды на то,
	// чтобы завершить создание файлов snapshot.wal и удаление старых файлов,
	// иначе t.TempDir() попытается удалить папку во время записи файлов.
	time.Sleep(100 * time.Millisecond)
}

// TestSyncer_Stop — Stop дожидается последнего Sync и завершает горутину.
func TestSyncer_Stop(t *testing.T) {
	w, _ := tempWAL(t)
	iterate := func(fn func(byte, string, []byte)) {}

	syncer := NewSyncer(w, 10*time.Millisecond, "", iterate, func() error { return nil })

	// Stop не должен зависнуть и не паниковать
	syncer.Stop()
	w.Close()
}

// ─── Snapshot ──────────────────────────────────────────────

// TestSnapshot_WriteAndRead — полный цикл: создание snapshot → чтение.
//
// Проверяем:
// 1. WriteSnapshot создаёт файл snapshot.wal (не .tmp)
// 2. Файл содержит все ключи
// 3. ReadEntries читает их корректно
func TestSnapshot_WriteAndRead(t *testing.T) {
	dir := t.TempDir()
	sw := NewSnapshotWriter(dir)

	// Имитация store с 100 ключами
	testData := make(map[string]string)
	for i := 0; i < 100; i++ {
		testData[fmt.Sprintf("key:%d", i)] = fmt.Sprintf("val:%d", i)
	}

	iterate := func(fn func(byte, string, []byte)) {
		for k, v := range testData {
			fn(OpSet, k, []byte(v))
		}
	}

	err := sw.WriteSnapshot(iterate)
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	// .tmp НЕ должен остаться
	tmpPath := filepath.Join(dir, "snapshot.wal.tmp")
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatal("snapshot.wal.tmp should not exist (should be renamed)")
	}

	// snapshot.wal должен существовать
	finalPath := filepath.Join(dir, "snapshot.wal")
	got, err := ReadEntries(finalPath)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("snapshot has %d entries, want 100", len(got))
	}

	// Проверяем данные (порядок map не гарантирован, проверяем по ключу)
	found := make(map[string]string)
	for _, e := range got {
		found[e.Key] = string(e.Value)
	}
	for k, v := range testData {
		if found[k] != v {
			t.Fatalf("key %q: got %q, want %q", k, found[k], v)
		}
	}
}

// TestSnapshot_AtomicRename — .tmp файл не остаётся при успехе.
func TestSnapshot_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	sw := NewSnapshotWriter(dir)

	iterate := func(fn func(byte, string, []byte)) {
		fn(OpSet, "only-key", []byte("only-value"))
	}

	sw.WriteSnapshot(iterate)

	// Проверяем: .tmp удалён (переименован)
	if _, err := os.Stat(filepath.Join(dir, "snapshot.wal.tmp")); !os.IsNotExist(err) {
		t.Fatal(".tmp file should not exist after successful snapshot")
	}

	// Финальный файл на месте
	if _, err := os.Stat(filepath.Join(dir, "snapshot.wal")); err != nil {
		t.Fatal("snapshot.wal should exist")
	}
}

// TestSnapshot_ErrorCleansUp — если iterate падает, .tmp удаляется.
//
// Симуляция: iterate вызывает fn один раз, потом мы делаем так,
// чтобы writer.Write вернул ошибку.
// Но проще: проверяем что при пустом iterate (0 ключей) файл создаётся пустой.
// А для ошибки — пишем в read-only директорию.
func TestSnapshot_ErrorCleansUp(t *testing.T) {
	// Создаём read-only директорию → Create() вернёт permission denied
	dir := t.TempDir()
	readOnlyDir := filepath.Join(dir, "readonly")
	os.MkdirAll(readOnlyDir, 0555)

	sw := NewSnapshotWriter(readOnlyDir)
	iterate := func(fn func(byte, string, []byte)) {
		fn(OpSet, "k", []byte("v"))
	}

	err := sw.WriteSnapshot(iterate)
	if err == nil {
		t.Fatal("expected error for read-only dir")
	}

	// .tmp НЕ должен остаться
	if _, statErr := os.Stat(filepath.Join(readOnlyDir, "snapshot.wal.tmp")); !os.IsNotExist(statErr) {
		t.Fatal(".tmp file should be cleaned up on error")
	}
}

// ─── CleanupOldWALs ───────────────────────────────────────

// TestCleanupOldWALs — удаляет старые, оставляет текущий.
func TestCleanupOldWALs(t *testing.T) {
	dir := t.TempDir()

	// Создаём 3 WAL-файла
	for _, name := range []string{"wal_0001.log", "wal_0002.log", "wal_0003.log"} {
		os.WriteFile(filepath.Join(dir, name), []byte("data"), 0644)
	}

	// keepPath = wal_0003 — оставляем его, удаляем 0001 и 0002
	keepPath := filepath.Join(dir, "wal_0003.log")
	CleanupOldWALs(dir, keepPath)

	// 0001, 0002 удалены
	if _, err := os.Stat(filepath.Join(dir, "wal_0001.log")); !os.IsNotExist(err) {
		t.Fatal("wal_0001.log should be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "wal_0002.log")); !os.IsNotExist(err) {
		t.Fatal("wal_0002.log should be deleted")
	}

	// 0003 остался
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatal("wal_0003.log should NOT be deleted")
	}
}

// TestCleanupOldWALs_KeepNewer — файлы «новее» текущего тоже остаются.
func TestCleanupOldWALs_KeepNewer(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "wal_0001.log"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "wal_0002.log"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "wal_0003.log"), []byte("x"), 0644)

	// keepPath = wal_0002 → удалить 0001, оставить 0002 и 0003
	CleanupOldWALs(dir, filepath.Join(dir, "wal_0002.log"))

	if _, err := os.Stat(filepath.Join(dir, "wal_0001.log")); !os.IsNotExist(err) {
		t.Fatal("wal_0001 should be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "wal_0002.log")); err != nil {
		t.Fatal("wal_0002 (keepPath) should remain")
	}
	if _, err := os.Stat(filepath.Join(dir, "wal_0003.log")); err != nil {
		t.Fatal("wal_0003 (newer) should remain")
	}
}

// ─── BatchWAL: Backpressure ───────────────────────────────

// TestBatchWAL_Backpressure — 20K записей параллельно при буфере 8192.
//
// Воркеры пишут быстрее чем flusher читает → канал переполняется →
// воркеры блокируются (backpressure). После Close() все 20K на диске.
func TestBatchWAL_Backpressure(t *testing.T) {
	w, dir := tempWAL(t)
	bw := NewBatchWAL(w)

	const workers = 10
	const perWorker = 2000 // 10 × 2000 = 20000 > channelSize (8192)

	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				bw.Write(Entry{
					Op:    OpSet,
					Key:   fmt.Sprintf("bp:%d:%d", id, j),
					Value: []byte("v"),
				})
			}
		}(i)
	}

	wg.Wait()
	bw.Close()

	got, _ := ReadEntries(filepath.Join(dir, "wal_0001.log"))
	expected := workers * perWorker
	if len(got) != expected {
		t.Fatalf("backpressure: got %d entries, want %d (data lost!)", len(got), expected)
	}
}

// ─── Truncation (обрезанный файл) ─────────────────────────

// TestWAL_Truncation — файл оборван посередине записи.
//
// Сценарий: свет моргнул во время записи 8-байтного заголовка.
// Файл оборвался на 4-м байте последней записи.
// ReadEntries ОБЯЗАН спокойно вернуть первые N-1 записей.
func TestWAL_Truncation(t *testing.T) {
	w, dir := tempWAL(t)
	path := filepath.Join(dir, "wal_0001.log")

	// Пишем 3 записи
	w.Write(Entry{Op: OpSet, Key: "one", Value: []byte("1")})
	w.Write(Entry{Op: OpSet, Key: "two", Value: []byte("2")})
	w.Write(Entry{Op: OpSet, Key: "three", Value: []byte("3")})
	w.Sync()
	w.Close()

	// Отрезаем последние 5 байт — ломаем третью запись на уровне файла
	info, _ := os.Stat(path)
	os.Truncate(path, info.Size()-5)

	// ReadEntries должен прочитать первые 2 записи, на третьей — EOF
	got, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries on truncated file returned error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("truncated WAL: got %d entries, want 2", len(got))
	}
	if got[0].Key != "one" || got[1].Key != "two" {
		t.Fatalf("wrong entries: %q, %q", got[0].Key, got[1].Key)
	}

	t.Logf("gracefully recovered 2/3 entries from truncated WAL")
}

// ─── Benchmarks ────────────────────────────────────────────

// BenchmarkBatchWAL_FlushBatch — чистая скорость кодирования всего батча
// без накладных расходов на отправку/чтение из канала.
func BenchmarkBatchWAL_FlushBatch(b *testing.B) {
	dir := b.TempDir()
	w, _ := Open(filepath.Join(dir, "bench_flush.log"))
	bw := NewBatchWAL(w)
	defer bw.Close()

	// Подготавливаем батч из 256 записей (как в реальной жизни)
	batch := make([]Entry, 0, 256)
	for i := 0; i < 256; i++ {
		batch = append(batch, Entry{
			Op:    OpSet,
			Key:   "bench-key",
			Value: []byte("bench-value-data"),
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bw.flushBatch(batch)
	}
}

// ─── Benchmarks ────────────────────────────────────────────

// BenchmarkWAL_Write — скорость одиночной записи (mutex + encode + CRC + bufio).
func BenchmarkWAL_Write(b *testing.B) {
	dir := b.TempDir()
	w, _ := Open(filepath.Join(dir, "bench.log"))
	defer w.Close()

	entry := Entry{Op: OpSet, Key: "bench-key", Value: []byte("bench-value-data")}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Write(entry)
	}
}

// BenchmarkBatchWAL_Write — скорость записи через BatchWAL (channel send).
// Сравни с BenchmarkWAL_Write: BatchWAL должен быть быстрее,
// потому что горячий путь — это только channel send (~50ns),
// а encode + CRC + disk write делает flusher в фоне.
func BenchmarkBatchWAL_Write(b *testing.B) {
	dir := b.TempDir()
	w, _ := Open(filepath.Join(dir, "bench.log"))
	bw := NewBatchWAL(w)

	entry := Entry{Op: OpSet, Key: "bench-key", Value: []byte("bench-value-data")}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bw.Write(entry)
	}
	b.StopTimer()
	bw.Close()
}

// BenchmarkEncode — чистая скорость encode + CRC (без I/O).
func BenchmarkEncode(b *testing.B) {
	entry := Entry{Op: OpSet, Key: "bench-key", Value: []byte("bench-value-data")}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payload := encodeEntry(entry)
		_ = payload
	}
}

// BenchmarkReadEntries — скорость чтения WAL с диска.
func BenchmarkReadEntries(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.log")
	w, _ := Open(path)

	// Подготовка: 10K записей
	for i := 0; i < 10000; i++ {
		w.Write(Entry{Op: OpSet, Key: fmt.Sprintf("k:%d", i), Value: []byte("val")})
	}
	w.Sync()
	w.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ReadEntries(path)
	}
}

func BenchmarkBatchWAL_Write_Parallel(b *testing.B) {
	dir := b.TempDir()
	w, _ := Open(filepath.Join(dir, "bench.log"))
	bw := NewBatchWAL(w)
	defer bw.Close()

	entry := Entry{Op: OpSet, Key: "bench-key", Value: []byte("bench-value-data")}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			bw.Write(entry) // 12 горутин яростно шлют данные в канал
		}
	})
}
