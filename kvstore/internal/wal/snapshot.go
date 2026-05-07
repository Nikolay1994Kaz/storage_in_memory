package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
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
//
// Оптимизация: пишет напрямую в файл через bufio (256KB буфер)
// с pre-allocated encode buffer. Без промежуточного WAL.Write() —
// нет mutex lock и аллокаций на каждый ключ.
func (sw *SnapshotWriter) WriteSnapshot(iterate func(fn func(key string, value []byte))) error {
	tmpPath := filepath.Join(sw.dir, "snapshot.wal.tmp")
	finalPath := filepath.Join(sw.dir, "snapshot.wal")

	// Открываем временный файл напрямую (без WAL-обёртки)
	file, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("snapshot create: %w", err)
	}

	writer := bufio.NewWriterSize(file, 256*1024) // 256KB буфер
	encodeBuf := make([]byte, 0, 64*1024)         // pre-allocated encode buffer

	count := 0
	var writeErr error
	iterate(func(key string, value []byte) {
		if writeErr != nil {
			return // предыдущая запись упала — пропускаем остальные
		}

		// Кодируем entry прямо в encodeBuf (zero-alloc если capacity хватает)
		payloadSize := 1 + 4 + len(key) + len(value)
		totalSize := 8 + payloadSize

		// Grow буфер если нужно
		if cap(encodeBuf) < totalSize {
			encodeBuf = make([]byte, totalSize)
		}
		encodeBuf = encodeBuf[:totalSize]

		// Заполняем payload: [Op 1B][KeyLen 4B][Key][Value]
		off := 8
		encodeBuf[off] = OpSet
		off++

		binary.LittleEndian.PutUint32(encodeBuf[off:], uint32(len(key)))
		off += 4

		copy(encodeBuf[off:], key)
		off += len(key)

		if len(value) > 0 {
			copy(encodeBuf[off:], value)
		}

		// CRC32 по payload
		payload := encodeBuf[8 : 8+payloadSize]
		checksum := crc32.ChecksumIEEE(payload)

		// Header: [CRC32 4B][PayloadLen 4B]
		binary.LittleEndian.PutUint32(encodeBuf[0:4], checksum)
		binary.LittleEndian.PutUint32(encodeBuf[4:8], uint32(payloadSize))

		if _, err := writer.Write(encodeBuf[:totalSize]); err != nil {
			writeErr = err
			return
		}
		count++
	})

	// Если хоть одна запись не прошла — snapshot битый, не используем
	if writeErr != nil {
		file.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("snapshot write: %w", writeErr)
	}

	// Flush + Sync — гарантируем, что данные на диске
	if err := writer.Flush(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("snapshot flush: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("snapshot sync: %w", err)
	}
	file.Close()

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
// 3. Удалить старые WAL (ТОЛЬКО после успешного snapshot!)
func BackgroundCompact(w *WAL, dir string, iterate func(fn func(key string, value []byte))) {
	// 1. Ротация — переключаем WAL на новый файл.
	// Наносекундная точность в имени предотвращает коллизии при быстрой ротации.
	now := time.Now()
	newWALPath := filepath.Join(dir, fmt.Sprintf("wal_%s_%09d.log",
		now.Format("20060102_150405"), now.Nanosecond()))

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
			// НЕ удаляем старые WAL — snapshot не прошёл,
			// старые WAL нужны для recovery!
			return
		}

		// 3. Удаляем старые WAL-файлы ТОЛЬКО после успешного snapshot.
		// Snapshot уже содержит их данные + атомарно переименован.
		CleanupOldWALs(dir, newWALPath)
		log.Printf("Old WAL files cleaned up")
	}()
}
