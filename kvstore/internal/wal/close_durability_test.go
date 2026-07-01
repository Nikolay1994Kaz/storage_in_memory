package wal

import (
	"path/filepath"
	"testing"
)

// TestBatchWAL_CloseFlushesLastBatch — страж durability на закрытии.
//
// Репро дефекта: WAL.Close() делал только writer.Flush() (bufio → page cache),
// без file.Sync(). Последний батч, отправленный в BatchWAL перед штатной
// остановкой, оставался лишь в page cache — потеря питания сразу после shutdown
// теряла уже подтверждённые клиенту записи.
//
// Здесь мы не можем наблюдать сам fsync, но проверяем сквозной контракт: после
// bw.Close() все записи (включая самую последнюю, не добравшую batchSize)
// присутствуют в файле и читаются обратно. Заодно Close() не должен возвращать
// ошибку из добавленного пути Sync().
func TestBatchWAL_CloseFlushesLastBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_close.log")

	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	bw := NewBatchWAL(w)

	// Пишем меньше batchSize записей — последний батч заведомо неполный и
	// сбрасывается только на Close(). Если Close не флашит/синкает — потеряются.
	const n = 5
	for i := 0; i < n; i++ {
		bw.Write(Entry{Op: OpSet, Key: "k" + string(rune('0'+i)), Value: []byte("v")})
	}

	if err := bw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("после Close прочитано %d записей, ожидали %d — последний батч потерян", len(entries), n)
	}
}
