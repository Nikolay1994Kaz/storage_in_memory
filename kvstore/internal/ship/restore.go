package ship

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// RestoreSummary — что было восстановлено (для лога оператору).
type RestoreSummary struct {
	Gen      uint64
	Snapshot bool
	Graph    bool
	WALFiles int
	Bytes    int64
}

// Restore скачивает последнюю restore-точку (манифест) в каталог данных.
//
// Отказывается работать поверх существующего состояния: если в каталоге уже
// есть snapshot.wal / graph_leveled.bin / wal_*.log — это чьи-то данные, и
// молча смешать их с удалённой копией = коррупция. Оператор должен явно
// убрать старый каталог (как scripts/restore.sh отодвигает его в *.bak).
//
// После восстановления сервер стартует обычным путём: накатывает снапшот +
// WAL и продолжает работу. Хвост WAL, оборванный на границе доставки,
// обрезается replay'ем по CRC — тот же путь, что и при crash recovery.
func Restore(ctx context.Context, r Remote, dataDir string) (RestoreSummary, error) {
	var sum RestoreSummary

	if err := ensureEmptyState(dataDir); err != nil {
		return sum, err
	}

	m, err := loadLatestManifest(ctx, r)
	if err != nil {
		return sum, err
	}
	if m == nil {
		return sum, fmt.Errorf("ship: remote storage has no manifest (nothing was shipped yet)")
	}
	sum.Gen = m.Gen

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return sum, err
	}

	if m.Snapshot != nil {
		n, err := restoreObject(ctx, r, m.Snapshot.Key, filepath.Join(dataDir, snapshotFile))
		if err != nil {
			return sum, err
		}
		sum.Snapshot = true
		sum.Bytes += n
	}
	if m.Graph != nil {
		n, err := restoreObject(ctx, r, m.Graph.Key, filepath.Join(dataDir, graphFile))
		if err != nil {
			return sum, err
		}
		sum.Graph = true
		sum.Bytes += n
	}

	for _, w := range m.WALs {
		n, err := restoreWAL(ctx, r, w, dataDir)
		if err != nil {
			return sum, err
		}
		sum.WALFiles++
		sum.Bytes += n
	}

	// Durable-фиксация всего восстановленного набора одним fsync каталога.
	if err := syncDir(dataDir); err != nil {
		return sum, err
	}
	return sum, nil
}

// ensureEmptyState гарантирует, что в каталоге нет существующего состояния.
func ensureEmptyState(dataDir string) error {
	for _, name := range []string{snapshotFile, graphFile} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			return fmt.Errorf("ship: refusing to restore into %s: %s already exists "+
				"(переместите старый каталог данных, как это делает scripts/restore.sh)", dataDir, name)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dataDir, "wal_*.log"))
	if len(matches) > 0 {
		return fmt.Errorf("ship: refusing to restore into %s: %d wal_*.log files already exist "+
			"(переместите старый каталог данных)", dataDir, len(matches))
	}
	return nil
}

// restoreObject скачивает один объект в файл (через tmp+rename+fsync).
func restoreObject(ctx context.Context, r Remote, key, dst string) (int64, error) {
	data, err := r.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if err := writeFileDurable(dst, data); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// restoreWAL восстанавливает wal-файл конкатенацией кусков, проверяя их
// смежность: дырка между кусками = битый манифест, лучше громкий отказ,
// чем тихо склеенный лог со сдвинутыми записями.
func restoreWAL(ctx context.Context, r Remote, w WALFile, dataDir string) (int64, error) {
	dst := filepath.Join(dataDir, w.Name)
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}

	var written int64
	fail := func(err error) (int64, error) {
		f.Close()
		os.Remove(tmp)
		return written, err
	}

	var expectOffset int64
	for _, c := range w.Chunks {
		if c.Offset != expectOffset {
			return fail(fmt.Errorf("ship: %s: chunk gap at %d (expected offset %d) — manifest corrupt",
				w.Name, c.Offset, expectOffset))
		}
		data, err := r.Get(ctx, c.Key)
		if err != nil {
			return fail(err)
		}
		if int64(len(data)) != c.Size {
			return fail(fmt.Errorf("ship: %s: chunk %s size %d != manifest %d",
				w.Name, c.Key, len(data), c.Size))
		}
		if _, err := f.Write(data); err != nil {
			return fail(err)
		}
		written += int64(len(data))
		expectOffset += c.Size
	}
	if written != w.Size {
		return fail(fmt.Errorf("ship: %s: restored %d bytes, manifest says %d", w.Name, written, w.Size))
	}

	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return written, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return written, err
	}
	return written, nil
}

func writeFileDurable(dst string, data []byte) error {
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
