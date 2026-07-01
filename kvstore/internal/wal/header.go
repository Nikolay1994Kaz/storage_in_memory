package wal

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// Заголовок файла — единый для WAL и snapshot.
//
// Формат: [MAGIC 4B][VER 1B][baseLSN 8B] = 13 байт.
//
// Читатель ВСЕГДА снимает фиксированные 13 байт без ветвлений по типу файла.
// Магия ловит «это не мой файл» (чужой/обрезанный/zero-filled) громкой ошибкой,
// а не тихой интерпретацией мусора как версии+LSN. Версия-байт оставляет
// дверь для будущей эволюции формата записи внутри того же семейства.
//
// Политика совместимости: смена формата ДО прода = чистый старт (старый data/
// сносится). Поэтому dual-format читателя нет — незнакомая магия/версия = ошибка.
var fileMagic = [4]byte{'V', 'K', 'V', '1'} // Vector-KV, формат v1

const (
	fileFormatVersion byte = 1
	// fileHeaderSize — magic(4) + version(1) + baseLSN(8).
	fileHeaderSize = 4 + 1 + 8

	// baseLSN в заголовке WAL-файла не несёт watermark (записи носят
	// собственный LSN) — пишем 0. В snapshot baseLSN = watermark «покрываю до
	// LSN N», защищающий nextLSN от сброса после компакции с последующим крашем.
	walBaseLSN uint64 = 0
)

// writeFileHeader пишет [MAGIC][VER][baseLSN] в начало файла.
func writeFileHeader(w io.Writer, baseLSN uint64) error {
	var h [fileHeaderSize]byte
	copy(h[0:4], fileMagic[:])
	h[4] = fileFormatVersion
	binary.LittleEndian.PutUint64(h[5:fileHeaderSize], baseLSN)
	if _, err := w.Write(h[:]); err != nil {
		return fmt.Errorf("wal write header: %w", err)
	}
	return nil
}

// readFileHeader читает и валидирует заголовок, возвращает baseLSN (watermark).
//
// Особый случай: полностью пустой файл (0 байт) — это НЕ ошибка. Так выглядит
// свежесозданный WAL, чей заголовок ещё завис в bufio и не долетел до диска
// перед крашем: данных в нём нет, восстанавливать нечего. Возвращаем io.EOF,
// вызывающий трактует как «нет записей».
func readFileHeader(r *bufio.Reader) (baseLSN uint64, err error) {
	var h [fileHeaderSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		if err == io.EOF {
			return 0, io.EOF // пустой файл — нормально
		}
		// Обрезанный заголовок (io.ErrUnexpectedEOF) — файл записан частично
		// и толком не начался; тоже нечего восстанавливать.
		if err == io.ErrUnexpectedEOF {
			return 0, io.EOF
		}
		return 0, fmt.Errorf("wal read header: %w", err)
	}
	if !bytes.Equal(h[0:4], fileMagic[:]) {
		return 0, fmt.Errorf("wal: bad magic %q (not a %s file)", h[0:4], fileMagic)
	}
	if h[4] != fileFormatVersion {
		return 0, fmt.Errorf("wal: unsupported format version %d (expected %d)", h[4], fileFormatVersion)
	}
	return binary.LittleEndian.Uint64(h[5:fileHeaderSize]), nil
}
