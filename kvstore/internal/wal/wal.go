package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Типы операций
const (
	OpSet     byte = 1
	OpDel     byte = 2
	OpExpire  byte = 3 // TTL: Value = 8 байт unix nano (абсолютное время смерти)
	OpPersist byte = 4 // Убрать TTL: Value пустой
	OpVSimAdd byte = 5  // вектор добавлен в HNSW
	OpVSimDel byte = 6  // вектор удалён из HNSW
	// OpVSimAddAttrs: вектор + атрибуты (колоночный слой). Value кодируется
	// vector.SerializeVectorWithAttrs. Replay восстанавливает attrs/tenant через
	// AddWithAttrs (P0-4). Эмитится write-путём, когда AddWithAttrs выходит в RESP.
	OpVSimAddAttrs byte = 7
	OpZAdd    byte = 16 // sorted set: Key = setName, Value = [8B score LE float64][member string]
	OpZRem    byte = 17 // sorted set: Key = setName, Value = [member string]
)

// maxEntrySize — защита от мусорных данных при recovery.
// Если length > 64MB — это скорее всего corruption, а не настоящая запись.
const maxEntrySize = 64 * 1024 * 1024

// Entry — одна запись в WAL.
type Entry struct {
	Op    byte
	Key   string
	Value []byte
}

// WAL — Write-Ahead Log с поддержкой ротации.
//
// Durability trade-off (осознанный выбор, аналог Redis AOF everysec):
//
// Текущая архитектура — fire-and-forget: BatchWAL.Write() отправляет
// запись в канал и немедленно возвращает управление. Данные попадают на
// диск асинхронно (через flusher + Syncer fsync каждые 100ms).
//
// Это означает окно потери данных ≤100ms при crash. Для in-memory
// KV store это приемлемый компромисс: максимальный throughput (~1.2M ops/sec)
// ценой теоретической потери последних ~100ms данных.
//
// Для 100% durability в будущем планируется group commit (аналог PostgreSQL):
// flusher будет делать fsync после каждого batch и уведомлять writers.
// Ожидаемая деградация throughput: ~10-15% на NVMe.
type WAL struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	dir    string // директория, где лежат WAL-файлы
}

// Open открывает или создаёт WAL-файл.
func Open(path string) (*WAL, error) {
	dir := filepath.Dir(path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("wal open: %w", err)
	}
	return &WAL{
		file:   file,
		writer: bufio.NewWriter(file),
		dir:    dir,
	}, nil
}

// Write записывает одну операцию в WAL.
// Формат: [CRC32 4B][TotalLen 4B][Op 1B][KeyLen 4B][Key][Value]
func (w *WAL) Write(entry Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	payload := encodeEntry(entry)
	checksum := crc32.ChecksumIEEE(payload)

	// Создаем локальный массив на 8 байт (4 байта для CRC32 + 4 байта для длины).
	// Важно: так как размер фиксирован, Go выделит эту память на стеке,
	// а не в куче. Это значит — НОЛЬ нагрузки на сборщик мусора (GC)!
	var header [8]byte

	// Вручную раскладываем числа по байтам (без рефлексии, работает мгновенно)
	binary.LittleEndian.PutUint32(header[0:4], checksum)
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(payload)))

	// Пишем весь заголовок (8 байт) за один вызов
	if _, err := w.writer.Write(header[:]); err != nil {
		return fmt.Errorf("wal write header: %w", err)
	}

	// Пишем сами данные
	if _, err := w.writer.Write(payload); err != nil {
		return fmt.Errorf("wal write payload: %w", err)
	}

	return nil
}

// Sync сбрасывает буфер на диск (fsync).
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.writer.Flush(); err != nil {
		return err
	}
	return w.file.Sync()
}

// Rotate переключает WAL на новый файл.
// Старый файл закрывается и возвращается его путь.
// Эта операция МГНОВЕННАЯ — блокировка на наносекунды.
//
// Схема:
//  1. Создаём новый файл (wal_0002.log)
//  2. Lock → переключаем writer → Unlock
//  3. Возвращаем путь к старому файлу (wal_0001.log)
func (w *WAL) Rotate(newPath string) (oldPath string, err error) {
	// Создаём новый файл ДО блокировки — тяжёлая FS-операция вне лока
	newFile, err := os.OpenFile(newPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return "", fmt.Errorf("wal rotate: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Сбрасываем буфер старого WAL на диск
	if err := w.writer.Flush(); err != nil {
		newFile.Close()
		os.Remove(newPath)
		return "", fmt.Errorf("wal rotate flush: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		newFile.Close()
		os.Remove(newPath)
		return "", fmt.Errorf("wal rotate sync: %w", err)
	}

	oldPath = w.file.Name()
	if err := w.file.Close(); err != nil {
		// Данные уже synced — логируем, но продолжаем ротацию.
		log.Printf("WAL rotate: error closing old file %s: %v", oldPath, err)
	}

	w.file = newFile
	w.writer = bufio.NewWriter(newFile)

	return oldPath, nil
}

// Close закрывает WAL.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.writer.Flush(); err != nil {
		w.file.Close()
		return fmt.Errorf("wal flush on close: %w", err)
	}
	return w.file.Close()
}

// Path возвращает текущий путь WAL-файла.
func (w *WAL) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Name()
}

// --- Кодирование/декодирование ---

func encodeEntry(e Entry) []byte {
	size := 1 + 4 + len(e.Key) + len(e.Value)
	buf := make([]byte, size)

	offset := 0
	buf[offset] = e.Op
	offset++

	binary.LittleEndian.PutUint32(buf[offset:], uint32(len(e.Key)))
	offset += 4

	// copy из string напрямую — без промежуточного []byte(e.Key).
	copy(buf[offset:], e.Key)
	offset += len(e.Key)

	if len(e.Value) > 0 {
		copy(buf[offset:], e.Value)
	}

	return buf
}

// ReadEntries читает все записи из одного WAL-файла.
//
// Стратегия recovery (аналог PostgreSQL):
//   - Читаем записи последовательно до первой невалидной
//   - Truncated header/payload → нормально при crash recovery, логируем
//   - CRC mismatch → corruption, логируем и останавливаемся
//   - Реальная I/O ошибка → возвращаем error
//   - Всё что прочитано до ошибки — валидные записи
func ReadEntries(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("wal read: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var entries []Entry
	baseName := filepath.Base(path)

	for {
		// 1. Читаем header: [CRC32 4B][Length 4B]
		var header [8]byte
		_, err := io.ReadFull(reader, header[:])
		if err != nil {
			if err == io.EOF {
				break // Нормальный конец файла
			}
			if err == io.ErrUnexpectedEOF {
				// Truncated header — ожидаемо после crash
				log.Printf("WAL %s: truncated header at entry %d (crash recovery), %d entries recovered",
					baseName, len(entries), len(entries))
				break
			}
			// Реальная I/O ошибка — возвращаем что есть + error
			return entries, fmt.Errorf("wal read header %s: %w", baseName, err)
		}

		checksum := binary.LittleEndian.Uint32(header[0:4])
		length := binary.LittleEndian.Uint32(header[4:8])

		// Защита от мусорных данных: если length > maxEntrySize — это corruption
		if length > maxEntrySize {
			log.Printf("WAL %s: suspicious entry length %d at entry %d, stopping recovery (%d entries recovered)",
				baseName, length, len(entries), len(entries))
			break
		}

		// 2. Читаем payload
		payload := make([]byte, length)
		_, err = io.ReadFull(reader, payload)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				log.Printf("WAL %s: truncated payload at entry %d (crash recovery), %d entries recovered",
					baseName, len(entries), len(entries))
				break
			}
			return entries, fmt.Errorf("wal read payload %s: %w", baseName, err)
		}

		// 3. CRC проверка
		if crc32.ChecksumIEEE(payload) != checksum {
			log.Printf("WAL %s: CRC mismatch at entry %d, stopping recovery (%d entries recovered)",
				baseName, len(entries), len(entries))
			break
		}

		// 4. Декодируем
		entry, err := decodeEntry(payload)
		if err != nil {
			log.Printf("WAL %s: decode error at entry %d: %v, stopping recovery (%d entries recovered)",
				baseName, len(entries), err, len(entries))
			break
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

func decodeEntry(data []byte) (Entry, error) {
	if len(data) < 5 {
		return Entry{}, fmt.Errorf("entry too short: %d bytes", len(data))
	}

	offset := 0
	op := data[offset]
	offset++

	keyLen := binary.LittleEndian.Uint32(data[offset:])
	offset += 4

	if offset+int(keyLen) > len(data) {
		return Entry{}, fmt.Errorf("invalid key length")
	}
	key := string(data[offset : offset+int(keyLen)])
	offset += int(keyLen)

	var value []byte
	if offset < len(data) {
		value = make([]byte, len(data)-offset)
		copy(value, data[offset:])
	}

	return Entry{Op: op, Key: key, Value: value}, nil
}

// ReadAllWALs читает snapshot + все WAL-файлы в правильном порядке.
// Порядок: snapshot.wal (если есть) → wal_0001.log → wal_0002.log → ...
func ReadAllWALs(dir string) ([]Entry, error) {
	var allEntries []Entry

	// 1. Сначала snapshot
	snapshotPath := filepath.Join(dir, "snapshot.wal")
	if entries, err := ReadEntries(snapshotPath); err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	} else if entries != nil {
		allEntries = append(allEntries, entries...)
	}

	// 2. Потом все WAL-файлы по порядку
	matches, _ := filepath.Glob(filepath.Join(dir, "wal_*.log"))
	sort.Strings(matches) // wal_0001.log, wal_0002.log, ...

	for _, path := range matches {
		entries, err := ReadEntries(path)
		if err != nil {
			return nil, fmt.Errorf("read wal %s: %w", path, err)
		}
		allEntries = append(allEntries, entries...)
	}

	return allEntries, nil
}

// CleanupOldWALs удаляет WAL-файлы, которые старше snapshot.
// Вызывается после успешного создания snapshot.
func CleanupOldWALs(dir string, keepPath string) error {
	matches, _ := filepath.Glob(filepath.Join(dir, "wal_*.log"))
	for _, path := range matches {
		if path == keepPath {
			continue // не удаляем текущий WAL
		}
		// Удаляем только если имя файла "меньше" текущего (более старый)
		if strings.Compare(filepath.Base(path), filepath.Base(keepPath)) < 0 {
			os.Remove(path)
		}
	}
	return nil
}

// WriteBatch записывает pre-encoded batch за одну блокировку.
//
// buf уже содержит все entries в формате [CRC32][Len][Payload]...
// Вызывается из BatchWAL.flushBatch — один раз на batch.
//
// Стоимость: 1 mutex lock + 1 bufio write.
// Сравни с Write(): 1 mutex lock + encode + CRC + 2 writes PER ENTRY.
func (w *WAL) WriteBatch(buf []byte) error {
	if len(buf) == 0 {
		return nil
	}

	w.mu.Lock()
	_, err := w.writer.Write(buf)
	w.mu.Unlock()

	if err != nil {
		return fmt.Errorf("wal write batch: %w", err)
	}
	return nil
}
