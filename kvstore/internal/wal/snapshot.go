package wal

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// SnapshotWriter создаёт snapshot из текущего состояния store.
// Работает в фоне — не блокирует клиентов.
type SnapshotWriter struct {
	dir string
}

func NewSnapshotWriter(dir string) *SnapshotWriter {
	return &SnapshotWriter{dir: dir}
}

// WriteSnapshot записывает текущее состояние в snapshot.wal.
// Принимает функцию iterate, которая обходит все ключи store.
// Вызывается в фоновой горутине.
func (sw *SnapshotWriter) WriteSnapshot(iterate func(fn func(key string, value []byte))) error {
	tmpPath := filepath.Join(sw.dir, "snapshot.wal.tmp")
	finalPath := filepath.Join(sw.dir, "snapshot.wal")

	// Создаём временный файл
	w, err := Open(tmpPath)
	if err != nil {
		return fmt.Errorf("snapshot open: %w", err)
	}

	count := 0
	var writeErr error
	iterate(func(key string, value []byte) {
		if writeErr != nil {
			return // предыдущая запись упала — пропускаем остальные
		}
		if err := w.Write(Entry{Op: OpSet, Key: key, Value: value}); err != nil {
			writeErr = err
			return
		}
		count++
	})

	// Если хоть одна запись не прошла — snapshot битый, не используем
	if writeErr != nil {
		w.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("snapshot write: %w", writeErr)
	}

	// Sync — гарантируем, что данные на диске
	if err := w.Sync(); err != nil {
		w.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("snapshot sync: %w", err)
	}
	w.Close()

	// Атомарная замена: tmp → snapshot.wal
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("snapshot rename: %w", err)
	}

	log.Printf("Snapshot complete: %d keys written", count)
	return nil
}

// BackgroundCompact выполняет полный цикл компактизации:
// 1. Rotate WAL (мгновенно)
// 2. Записать snapshot (фон)
// 3. Удалить старые WAL
func BackgroundCompact(w *WAL, dir string, iterate func(fn func(key string, value []byte))) {
	// 1. Ротация — переключаем WAL на новый файл
	newWALPath := filepath.Join(dir, fmt.Sprintf("wal_%s.log", time.Now().Format("20060102_150405")))
	oldPath, err := w.Rotate(newWALPath)
	if err != nil {
		log.Printf("Compact rotate failed: %v", err)
		return
	}
	log.Printf("WAL rotated: %s → %s", filepath.Base(oldPath), filepath.Base(newWALPath))

	// 2. Фоновый snapshot — клиенты продолжают работать!
	go func() {
		sw := NewSnapshotWriter(dir)
		if err := sw.WriteSnapshot(iterate); err != nil {
			log.Printf("Snapshot failed: %v", err)
			return
		}

		// 3. Удаляем старые WAL-файлы (snapshot уже содержит их данные)
		CleanupOldWALs(dir, newWALPath)
		log.Printf("Old WAL files cleaned up")
	}()
}
