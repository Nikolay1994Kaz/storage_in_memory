package vector

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Бюджет чтения снапшота: счётчик из файла не может заставить выделить больше,
// чем в файле осталось байт.
//
// CRC снапшота проверяется в конце чтения, а выделения идут по счётчикам по
// ходу: число векторов, tombstone-ключей, длина строки, размер слэба. Один
// испорченный байт в таком счётчике без проверки превращался в make на
// гигабайты, и рантайм падал с «out of memory» — это фатальная ошибка, её не
// перехватить. Сервер с таким снапшотом падал на каждом старте вместо
// штатного «снапшот битый, восстанавливаемся из WAL».

// ErrSnapshotCorrupt — поле снапшота противоречит размеру файла.
var ErrSnapshotCorrupt = errors.New("snapshot is corrupt")

// budgetReader считает прочитанные байты и знает, сколько осталось.
type budgetReader struct {
	r    io.Reader
	left int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// needBytes проверяет счётчик из файла перед выделением: count элементов,
// каждый из которых занимает на диске не меньше minBytes, обязаны поместиться
// в остаток файла. Без бюджета (источник неизвестного размера) — без проверки.
func needBytes(r io.Reader, count, minBytes int64) error {
	b, ok := r.(*budgetReader)
	if !ok {
		return nil
	}
	if minBytes < 1 {
		minBytes = 1
	}
	if count < 0 || count > b.left/minBytes {
		return fmt.Errorf("%w: field claims %d × %d bytes, %d bytes left", ErrSnapshotCorrupt, count, minBytes, b.left)
	}
	return nil
}

// sourceSize — сколько байт осталось в источнике, если это можно узнать:
// bytes.Reader и подобные знают остаток, у файла он берётся из Stat.
func sourceSize(src io.Reader) (int64, bool) {
	switch v := src.(type) {
	case interface{ Len() int }:
		return int64(v.Len()), true
	case *os.File:
		fi, err := v.Stat()
		if err != nil {
			return 0, false
		}
		off, err := v.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, false
		}
		return fi.Size() - off, true
	}
	return 0, false
}
