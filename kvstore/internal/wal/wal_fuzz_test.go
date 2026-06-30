package wal

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Fuzz-тестирование WAL парсера
//
// Цель: гарантировать что повреждённые или произвольные данные
// НИКОГДА не вызовут panic при старте сервера (crash recovery).
//
// Запуск:
//   go test ./kvstore/internal/wal/ -fuzz FuzzDecodeEntry -fuzztime=30s
//   go test ./kvstore/internal/wal/ -fuzz FuzzReadEntriesFromBytes -fuzztime=30s
//   go test ./kvstore/internal/wal/ -fuzz FuzzEncodeDecodeRoundtrip -fuzztime=30s
//
// При нахождении crash-input Go сохраняет его в testdata/fuzz/<TestName>/
// и автоматически прогоняет при каждом go test.
// ═══════════════════════════════════════════════════════════════════════════════

// FuzzDecodeEntry — фаззинг парсера одной WAL-записи.
//
// decodeEntry(data) получает сырые байты и парсит Op, Key, Value.
// Контракт: ЛЮБОЙ вход — либо валидный Entry, либо error.
// Паника = баг. Зависание = баг.
func FuzzDecodeEntry(f *testing.F) {
	// Seed corpus: валидные записи, от которых фаззер отталкивается
	f.Add(encodeEntry(Entry{Op: OpSet, Key: "hello", Value: []byte("world")}))
	f.Add(encodeEntry(Entry{Op: OpDel, Key: "x", Value: nil}))
	f.Add(encodeEntry(Entry{Op: OpExpire, Key: "ttl", Value: []byte{1, 2, 3, 4, 5, 6, 7, 8}}))
	f.Add(encodeEntry(Entry{Op: OpPersist, Key: "persist-key", Value: nil}))
	f.Add(encodeEntry(Entry{Op: OpVSimAdd, Key: "vec:1", Value: []byte("vector-data")}))
	f.Add(encodeEntry(Entry{Op: OpVSimDel, Key: "vec:2", Value: nil}))
	f.Add(encodeEntry(Entry{Op: OpSet, Key: "", Value: []byte("empty-key")}))
	f.Add(encodeEntry(Entry{Op: OpSet, Key: "empty-val", Value: nil}))

	// Граничные случаи
	f.Add([]byte{})                                       // пустой вход
	f.Add([]byte{0})                                      // 1 байт
	f.Add([]byte{0, 0, 0, 0, 0})                          // минимальный размер (Op + keyLen=0)
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF})           // максимальные значения
	f.Add([]byte{1, 0xFF, 0xFF, 0xFF, 0xFF})              // Op=1, keyLen=MaxUint32
	f.Add([]byte{1, 0, 0, 0, 0})                          // Op=1, keyLen=0, no value
	f.Add([]byte{1, 3, 0, 0, 0, 'a', 'b', 'c', 'd', 'e'}) // key="abc", value="de"

	f.Fuzz(func(t *testing.T, data []byte) {
		// Контракт: не паниковать. Ошибка — допустима.
		_, _ = decodeEntry(data)
	})
}

// FuzzReadEntriesFromBytes — фаззинг полного пайплайна чтения WAL-файла.
//
// Симулирует ситуацию: сервер упал (kill -9 / отключение питания),
// WAL-файл оказался повреждён. При следующем старте ReadEntries
// читает этот файл. Паника = сервер не запустится = катастрофа.
//
// Проверяем:
//   - Нет panic на произвольных данных
//   - Нет бесконечного цикла (timeout фаззера поймает)
//   - Нет OOM (maxEntrySize guard должен сработать)
func FuzzReadEntriesFromBytes(f *testing.F) {
	// Seed 1: валидный WAL-файл с 3 записями
	f.Add(buildValidWALFile())

	// Seed 2: пустой файл
	f.Add([]byte{})

	// Seed 3: только header без payload (truncated write)
	f.Add([]byte{0, 0, 0, 0, 5, 0, 0, 0})

	// Seed 4: header + частичный payload (crash mid-write)
	f.Add([]byte{0, 0, 0, 0, 10, 0, 0, 0, 1, 2, 3})

	// Seed 5: валидная запись + мусор в конце
	valid := buildSingleEntryWAL(Entry{Op: OpSet, Key: "ok", Value: []byte("v")})
	withGarbage := append(valid, 0xFF, 0xFE, 0xFD, 0xFC, 0xFB)
	f.Add(withGarbage)

	// Seed 6: length = 0 (пустой payload)
	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header[0:4], crc32.ChecksumIEEE([]byte{}))
	binary.LittleEndian.PutUint32(header[4:8], 0)
	f.Add(header)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Записываем произвольные байты в файл → парсим как WAL
		dir := t.TempDir()
		path := filepath.Join(dir, "fuzz.wal")
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Skip("cannot write temp file")
		}

		// Контракт: не паниковать. Любой результат допустим.
		_, _ = ReadEntries(path)
	})
}

// FuzzEncodeDecodeRoundtrip — property-based тест: encode(decode(x)) == x.
//
// Для ЛЮБОГО валидного Entry:
//
//	entry → encodeEntry → decodeEntry → entry'
//	entry' ДОЛЖЕН быть идентичен entry.
//
// Это проверяет что encode/decode — обратные операции,
// и данные не теряются/не искажаются при сериализации.
func FuzzEncodeDecodeRoundtrip(f *testing.F) {
	f.Add(byte(OpSet), "user:1", []byte("Alice"))
	f.Add(byte(OpDel), "deleted-key", []byte{})
	f.Add(byte(OpExpire), "ttl-key", []byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add(byte(OpPersist), "persist", []byte{})
	f.Add(byte(OpVSimAdd), "vec:embedding", []byte("float32-data-here"))
	f.Add(byte(OpVSimDel), "vec:old", []byte{})
	f.Add(byte(0), "", []byte{})          // пустой ключ и значение
	f.Add(byte(255), "max-op", []byte{0}) // несуществующий Op

	f.Fuzz(func(t *testing.T, op byte, key string, value []byte) {
		original := Entry{Op: op, Key: key, Value: value}

		// Encode
		encoded := encodeEntry(original)

		// Decode
		decoded, err := decodeEntry(encoded)
		if err != nil {
			t.Fatalf("decodeEntry failed on encodeEntry output: %v", err)
		}

		// Проверка: Op
		if decoded.Op != original.Op {
			t.Fatalf("Op mismatch: got %d, want %d", decoded.Op, original.Op)
		}

		// Проверка: Key
		if decoded.Key != original.Key {
			t.Fatalf("Key mismatch: got %q, want %q", decoded.Key, original.Key)
		}

		// Проверка: Value
		// Нюанс: nil и []byte{} оба валидны как "пустое значение"
		if len(original.Value) == 0 {
			if len(decoded.Value) != 0 {
				t.Fatalf("Value should be empty, got %d bytes", len(decoded.Value))
			}
		} else {
			if !bytes.Equal(decoded.Value, original.Value) {
				t.Fatalf("Value mismatch: got %x, want %x", decoded.Value, original.Value)
			}
		}
	})
}

// ─── Helpers для seed corpus ──────────────────────────────

// buildValidWALFile создаёт валидный WAL-файл с 3 записями в памяти.
func buildValidWALFile() []byte {
	entries := []Entry{
		{Op: OpSet, Key: "user:1", Value: []byte("Alice")},
		{Op: OpSet, Key: "user:2", Value: []byte("Bob")},
		{Op: OpDel, Key: "user:1", Value: nil},
	}

	var buf []byte
	for _, e := range entries {
		buf = append(buf, buildSingleEntryWAL(e)...)
	}
	return buf
}

// buildSingleEntryWAL кодирует одну Entry в формат WAL: [CRC32][Length][Payload].
func buildSingleEntryWAL(e Entry) []byte {
	payload := encodeEntry(e)
	checksum := crc32.ChecksumIEEE(payload)

	var header [8]byte
	binary.LittleEndian.PutUint32(header[0:4], checksum)
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(payload)))

	var buf []byte
	buf = append(buf, header[:]...)
	buf = append(buf, payload...)
	return buf
}
