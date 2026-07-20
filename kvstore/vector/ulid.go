package vector

import (
	"crypto/rand"
	"fmt"
)

// ULID — серверный id факта VMEM: 48 бит времени (мс, big-endian) + 80 бит
// crypto/rand, Crockford base32, 26 символов. Ключи лексикографически
// сортируются по времени создания — коррелируют с valid_from (отладка, сканы),
// durable-счётчик не нужен. Генерация ТОЛЬКО на ингесте (кухня REMEMBER):
// в WAL едет готовый ключ, реплей не генерит (дверь 1 VMEM_DESIGN).
// Зависимость oklog/ulid не тянем: нужная часть спеки — 30 строк.

// ulidAlphabet — Crockford base32: без I/L/O/U (визуальные двойники).
const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID возвращает ULID для момента unixMs (мс с эпохи).
func NewULID(unixMs int64) (string, error) {
	var b [16]byte
	b[0] = byte(unixMs >> 40)
	b[1] = byte(unixMs >> 32)
	b[2] = byte(unixMs >> 24)
	b[3] = byte(unixMs >> 16)
	b[4] = byte(unixMs >> 8)
	b[5] = byte(unixMs)
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("ulid entropy: %w", err)
	}
	return encodeULID(b), nil
}

// encodeULID кодирует 128 бит в 26 символов по 5 бит. Виртуальный поток —
// 130 бит с двумя нулевыми старшими (26*5=130): первый символ несёт только
// 3 старших бита времени, поэтому всегда ≤ '7' — как в спеке ULID.
func encodeULID(b [16]byte) string {
	out := make([]byte, 26)
	for i := 0; i < 26; i++ {
		v := 0
		for j := 0; j < 5; j++ {
			p := i*5 + j - 2 // позиция бита в 128-битном массиве
			v <<= 1
			if p >= 0 && b[p>>3]>>(7-p&7)&1 == 1 {
				v |= 1
			}
		}
		out[i] = ulidAlphabet[v]
	}
	return string(out)
}
