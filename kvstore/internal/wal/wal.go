package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
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
)

// Entry — одна запись в WAL.
type Entry struct {
	Op    byte
	Key   string
	Value []byte
}

// WAL — Write-Ahead Log с поддержкой ротации.
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

	// 1. Создаем локальный массив на 8 байт (4 байта для CRC32 + 4 байта для длины).
	// Важно: так как размер фиксирован, Go выделит эту память на стеке,
	// а не в куче. Это значит — НОЛЬ нагрузки на сборщик мусора (GC)!
	var header [8]byte

	// 2. Вручную раскладываем числа по байтам (без рефлексии, работает мгновенно)
	binary.LittleEndian.PutUint32(header[0:4], checksum)
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(payload)))

	// 3. Пишем весь заголовок (8 байт) за один вызов
	if _, err := w.writer.Write(header[:]); err != nil {
		return fmt.Errorf("wal write header: %w", err)
	}

	// 4. Пишем сами данные
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
	// --- Критическая секция ---

	// Сбрасываем буфер старого WAL на диск
	if err := w.writer.Flush(); err != nil {
		w.mu.Unlock()
		newFile.Close()
		os.Remove(newPath)
		return "", fmt.Errorf("wal rotate flush: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		w.mu.Unlock()
		newFile.Close()
		os.Remove(newPath)
		return "", fmt.Errorf("wal rotate sync: %w", err)
	}

	oldPath = w.file.Name()
	w.file.Close()

	w.file = newFile
	w.writer = bufio.NewWriter(newFile)
	// --- Конец критической секции ---
	w.mu.Unlock()

	return oldPath, nil
}

// Close закрывает WAL.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.writer.Flush()
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
	keyBytes := []byte(e.Key)
	size := 1 + 4 + len(keyBytes) + len(e.Value)
	buf := make([]byte, size)

	offset := 0
	buf[offset] = e.Op
	offset++

	binary.LittleEndian.PutUint32(buf[offset:], uint32(len(keyBytes)))
	offset += 4

	copy(buf[offset:], keyBytes)
	offset += len(keyBytes)

	if len(e.Value) > 0 {
		copy(buf[offset:], e.Value)
	}

	return buf
}

// ReadEntries читает все записи из одного WAL-файла.
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

	for {
		var checksum uint32
		if err := binary.Read(reader, binary.LittleEndian, &checksum); err != nil {
			if err == io.EOF {
				break
			}
			break
		}

		var length uint32
		if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
			break
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			break
		}

		if crc32.ChecksumIEEE(payload) != checksum {
			break
		}

		entry, err := decodeEntry(payload)
		if err != nil {
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
