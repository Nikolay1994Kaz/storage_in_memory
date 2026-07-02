package wal

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// makeHeader собирает валидный по магии заголовок с заданной версией.
func makeHeader(ver byte) []byte {
	h := make([]byte, fileHeaderSize)
	copy(h[0:4], fileMagic[:])
	h[4] = ver
	binary.LittleEndian.PutUint64(h[5:fileHeaderSize], 0)
	return h
}

// TestReadFileHeader_VersionMismatchActionable — страж actionable-диагностики
// несовместимости формата (High из аудита оболочки).
//
// Раньше любая чужая версия давала одинаковое «unsupported version N». Оператор
// не понимал: данные из БУДУЩЕЙ сборки (обнови бинарь) или из СТАРОЙ (мигрируй).
// Контракт: NEWER и OLDER дают разные, направленные сообщения со ссылкой на
// docs/FORMAT_COMPAT.md.
func TestReadFileHeader_VersionMismatchActionable(t *testing.T) {
	t.Run("newer version", func(t *testing.T) {
		r := bufio.NewReader(bytes.NewReader(makeHeader(fileFormatVersion + 1)))
		_, err := readFileHeader(r)
		if err == nil {
			t.Fatal("ожидали ошибку на более новой версии формата")
		}
		msg := err.Error()
		if !strings.Contains(msg, "NEWER") || !strings.Contains(msg, "FORMAT_COMPAT.md") {
			t.Fatalf("сообщение не actionable для NEWER: %q", msg)
		}
	})

	t.Run("older version", func(t *testing.T) {
		// fileFormatVersion=1; версия 0 = «старее». Если формат дорастёт до 1
		// как минимума, тест продолжит проверять ветку OLDER для любого <текущей.
		if fileFormatVersion == 0 {
			t.Skip("нет версии старее текущей")
		}
		r := bufio.NewReader(bytes.NewReader(makeHeader(fileFormatVersion - 1)))
		_, err := readFileHeader(r)
		if err == nil {
			t.Fatal("ожидали ошибку на более старой версии формата")
		}
		msg := err.Error()
		if !strings.Contains(msg, "OLDER") || !strings.Contains(msg, "FORMAT_COMPAT.md") {
			t.Fatalf("сообщение не actionable для OLDER: %q", msg)
		}
	})

	t.Run("current version ok", func(t *testing.T) {
		r := bufio.NewReader(bytes.NewReader(makeHeader(fileFormatVersion)))
		if _, err := readFileHeader(r); err != nil {
			t.Fatalf("текущая версия должна читаться без ошибки: %v", err)
		}
	})
}
