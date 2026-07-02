package wal

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestWAL_LatchesFailStopOnSyncError — страж durability fail-stop.
//
// Репро дефекта: на полном диске (ENOSPC) Sync() возвращал ошибку, а Syncer
// её только логировал и продолжал работу. BatchWAL.Write (fire-and-forget)
// продолжал подтверждать клиенту записи, которые уже не попадали на диск —
// БД тихо теряла подтверждённые данные (durability-грех №1).
//
// Контракт после фикса: первый провалившийся flush/fsync ЛАТЧИТ WAL
// (Failed()!=nil), и это состояние — сигнал write-пути отклонять новые мутации.
// Здесь симулируем отказ диска, закрыв fd под WAL: любой последующий
// flush/fsync проваливается, как на полном диске.
func TestWAL_LatchesFailStopOnSyncError(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Свежий WAL здоров — fail-stop не взведён.
	if w.Failed() != nil {
		t.Fatalf("свежий WAL не должен быть в fail-stop: %v", w.Failed())
	}

	// Буферизуем запись (осела в bufio, ещё не на диске).
	if err := w.Write(Entry{Op: OpSet, Key: "k", Value: []byte("v")}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Симулируем отказ диска: закрываем fd под WAL.
	if err := w.file.Close(); err != nil {
		t.Fatalf("close fd: %v", err)
	}

	// Sync обязан вернуть ошибку И залатчить fail-stop.
	if err := w.Sync(); err == nil {
		t.Fatal("Sync на сломанном диске вернул nil — ошибка проглочена")
	}
	if w.Failed() == nil {
		t.Fatal("после провала Sync WAL НЕ в fail-stop — БД продолжит ACK'ать потерянные записи")
	}
}

// TestWAL_FailLatchKeepsFirstCause — латч фиксирует первопричину, а не эхо.
// Оператору важна первая ошибка (напр. ENOSPC), а не производные сбои после неё.
func TestWAL_FailLatchKeepsFirstCause(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	first := errors.New("ENOSPC: no space left on device")
	w.fail(first)
	w.fail(errors.New("later echo error"))

	if got := w.Failed(); !errors.Is(got, first) {
		t.Fatalf("Failed() отдал %v, ожидали первопричину %v", got, first)
	}
}

// TestBatchWAL_FailedDelegates — BatchWAL.Failed отражает латч нижележащего WAL
// (write-путь сервера видит fail-stop именно через BatchWAL).
func TestBatchWAL_FailedDelegates(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	bw := NewBatchWAL(w)
	defer bw.Close()

	if bw.Failed() != nil {
		t.Fatalf("здоровый BatchWAL не должен быть в fail-stop: %v", bw.Failed())
	}

	w.fail(errors.New("enospc"))

	if bw.Failed() == nil {
		t.Fatal("BatchWAL.Failed не делегирует латч нижележащего WAL")
	}
}
