package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

// Типы операций
const (
	OpSet byte = 1
	OpDel byte = 2
)

// Entry — одна запись в WAL.
type Entry struct {
	Op    byte // OpSet или OpDel
	Key   string
	Value []byte // пустой для OpDel
}

// WAL — Write-Ahead Log.
// Все мутирующие операции сначала пишутся сюда,
// потом применяются к in-memory store.
type WAL struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
}

// Open открывает или создаёт WAL-файл.
// Файл открывается в режиме append — новые записи добавляются в конец.
func Open(path string) (*WAL, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("wal open: %w", err)
	}
	return &WAL{
		file:   file,
		writer: bufio.NewWriter(file),
	}, nil
}

// Write записывает одну операцию в WAL.
// Формат: [CRC32 4B][TotalLen 4B][Op 1B][KeyLen 4B][Key][Value]
func (w *WAL) Write(entry Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 1. Собираем payload (всё после CRC и TotalLen)
	payload := encodeEntry(entry)

	// 2. Считаем CRC32 для защиты целостности
	checksum := crc32.ChecksumIEEE(payload)

	// 3. Пишем: CRC32 (4 байта)
	if err := binary.Write(w.writer, binary.LittleEndian, checksum); err != nil {
		return fmt.Errorf("wal write crc: %w", err)
	}

	// 4. Пишем: длина payload (4 байта)
	if err := binary.Write(w.writer, binary.LittleEndian, uint32(len(payload))); err != nil {
		return fmt.Errorf("wal write len: %w", err)
	}

	// 5. Пишем сам payload
	if _, err := w.writer.Write(payload); err != nil {
		return fmt.Errorf("wal write payload: %w", err)
	}

	return nil
}

// Sync сбрасывает буфер на диск (fsync).
// Дорогая операция! Поэтому вызываем её батчами, а не на каждую запись.
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.writer.Flush(); err != nil {
		return err
	}
	return w.file.Sync() // fsync — гарантия, что данные на диске
}

// Close закрывает WAL.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.writer.Flush()
	return w.file.Close()
}

// encodeEntry превращает Entry в байты (payload).
func encodeEntry(e Entry) []byte {
	// Op (1) + KeyLen (4) + Key + Value
	keyBytes := []byte(e.Key)
	size := 1 + 4 + len(keyBytes) + len(e.Value)
	buf := make([]byte, size)

	offset := 0

	// Op
	buf[offset] = e.Op
	offset++

	// KeyLen
	binary.LittleEndian.PutUint32(buf[offset:], uint32(len(keyBytes)))
	offset += 4

	// Key
	copy(buf[offset:], keyBytes)
	offset += len(keyBytes)

	// Value (только для SET)
	if len(e.Value) > 0 {
		copy(buf[offset:], e.Value)
	}

	return buf
}

// ReadAll читает все записи из WAL-файла.
// Используется при старте для восстановления состояния store.
// Повреждённые записи (неверный CRC) пропускаются.
func ReadAll(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Файл не существует — первый запуск
		}
		return nil, fmt.Errorf("wal read: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var entries []Entry

	for {
		// 1. Читаем CRC32
		var checksum uint32
		if err := binary.Read(reader, binary.LittleEndian, &checksum); err != nil {
			if err == io.EOF {
				break // Конец файла — нормальное завершение
			}
			break // Повреждённая запись — останавливаемся
		}

		// 2. Читаем длину payload
		var length uint32
		if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
			break
		}

		// 3. Читаем payload
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			break
		}

		// 4. Проверяем CRC — если не совпадает, запись повреждена
		if crc32.ChecksumIEEE(payload) != checksum {
			break // Данные повреждены — дальше читать нельзя
		}

		// 5. Декодируем Entry
		entry, err := decodeEntry(payload)
		if err != nil {
			break
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// decodeEntry разбирает payload обратно в Entry.
func decodeEntry(data []byte) (Entry, error) {
	if len(data) < 5 { // минимум: Op(1) + KeyLen(4)
		return Entry{}, fmt.Errorf("entry too short: %d bytes", len(data))
	}

	offset := 0

	// Op
	op := data[offset]
	offset++

	// KeyLen
	keyLen := binary.LittleEndian.Uint32(data[offset:])
	offset += 4

	// Key
	if offset+int(keyLen) > len(data) {
		return Entry{}, fmt.Errorf("invalid key length")
	}
	key := string(data[offset : offset+int(keyLen)])
	offset += int(keyLen)

	// Value (оставшиеся байты)
	var value []byte
	if offset < len(data) {
		value = make([]byte, len(data)-offset)
		copy(value, data[offset:])
	}

	return Entry{Op: op, Key: key, Value: value}, nil
}
