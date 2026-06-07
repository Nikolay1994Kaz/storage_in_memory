//go:build tinygo.wasm

package main

import (
	"kvstore/kvstore/sdk"
)

// ─── Статические буферы ──────────────────────────────────
// Вся «рабочая» память — глобальные массивы фиксированного размера.
var numBuf [20]byte // буфер для itoa (макс 20 цифр для int64)

//export _initialize
func _initialize() {}

// export process вызывается Host-ом на каждый триггер.
//
//export process
func process(keyPtr, keyLen uint32) uint32 {
	// 1. Читаем value из Store (через zero-alloc GetViewBytes)
	keySlice := sdk.SliceAt(keyPtr, keyLen)
	val, ok := sdk.GetViewBytes(keySlice)
	if !ok {
		return 0
	}

	// 2. Парсим JSON поля (без аллокаций — работаем с []byte)
	amount := parseAmount(val)
	countryCode := parseCountryCode(val)

	// 3. Считаем fraud score
	score := 0
	if amount > 50000 {
		score = 100
	} else if amount > 10000 {
		score += 50
	} else if amount > 5000 {
		score += 30
	} else if amount > 1000 {
		score += 10
	}

	if countryCode == codeNK || countryCode == codeIR || countryCode == codeSY {
		score += 50
	}
	if score > 100 {
		score = 100
	}

	// 4. Определяем решение
	var decision []byte
	if score >= 80 {
		decision = []byte("BLOCKED")
	} else if score >= 50 {
		decision = []byte("REVIEW")
	} else {
		decision = []byte("OK")
	}

	// 5. Формируем JSON-результат прямо в OUTPUT_REGION
	out := sdk.SliceAt(sdk.OutputOffset, 4096)
	n := buildResult(out, score, amount, decision, countryCode)

	// 6. Записываем fraud-результат обратно в Store (zero-alloc)
	resultKey := buildResultKey(keySlice)
	sdk.SetBytes(resultKey, out[:n])

	// 7. Если BLOCKED — публикуем алерт (zero-alloc)
	if score >= 80 {
		alert := buildAlert(keySlice, score, countryCode)
		ch := []byte("fraud_alerts")
		sdk.PublishBytes(ch, alert)
	}

	return uint32(n)
}

// ─── Zero-alloc helpers ──────────────────────────────────

// parseAmount ищет "amount": в JSON и парсит число.
func parseAmount(data []byte) int {
	pat := []byte(`"amount":`)
	idx := findPattern(data, pat)
	if idx < 0 {
		return 0
	}
	pos := idx + len(pat)
	num := 0
	for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
		num = num*10 + int(data[pos]-'0')
		pos++
	}
	return num
}

// Двухбуквенные коды стран как uint16 — сравнение без строк.
const (
	codeNK uint16 = 'N'<<8 | 'K'
	codeIR uint16 = 'I'<<8 | 'R'
	codeSY uint16 = 'S'<<8 | 'Y'
)

// parseCountryCode возвращает 2-буквенный код как uint16.
func parseCountryCode(data []byte) uint16 {
	pat := []byte(`"country":"`)
	idx := findPattern(data, pat)
	if idx < 0 {
		return 0
	}
	pos := idx + len(pat)
	if pos+2 > len(data) {
		return 0
	}
	return uint16(data[pos])<<8 | uint16(data[pos+1])
}

func findPattern(data, pattern []byte) int {
	for i := 0; i <= len(data)-len(pattern); i++ {
		match := true
		for j := 0; j < len(pattern); j++ {
			if data[i+j] != pattern[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// itoa записывает число в numBuf и возвращает slice.
func itoa(n int) []byte {
	if n == 0 {
		numBuf[0] = '0'
		return numBuf[:1]
	}
	i := len(numBuf)
	for n > 0 {
		i--
		numBuf[i] = byte('0' + n%10)
		n /= 10
	}
	return numBuf[i:]
}

// buildResult пишет JSON прямо в destination slice.
func buildResult(dst []byte, score, amount int, decision []byte, country uint16) int {
	n := 0
	n += copyTo(dst[n:], []byte(`{"score":`))
	n += copyTo(dst[n:], itoa(score))
	n += copyTo(dst[n:], []byte(`,"decision":"`))
	n += copyTo(dst[n:], decision)
	n += copyTo(dst[n:], []byte(`","amount":`))
	n += copyTo(dst[n:], itoa(amount))
	n += copyTo(dst[n:], []byte(`,"country":"`))
	if country != 0 {
		dst[n] = byte(country >> 8)
		dst[n+1] = byte(country & 0xFF)
		n += 2
	}
	n += copyTo(dst[n:], []byte(`"}`))
	return n
}

// buildResultKey: "tx:1001" → "fraud:tx:1001"
func buildResultKey(key []byte) []byte {
	dst := sdk.SliceAt(sdk.OutputOffset+2048, uint32(6+len(key)))
	copy(dst, []byte("fraud:"))
	copy(dst[6:], key)
	return dst
}

// buildAlert формирует строку алерта.
func buildAlert(key []byte, score int, country uint16) []byte {
	dst := sdk.SliceAt(sdk.OutputOffset+3072, 1024)
	n := 0
	n += copyTo(dst[n:], []byte("BLOCKED: "))
	n += copyTo(dst[n:], key)
	n += copyTo(dst[n:], []byte(" score="))
	n += copyTo(dst[n:], itoa(score))
	n += copyTo(dst[n:], []byte(" country="))
	if country != 0 {
		dst[n] = byte(country >> 8)
		dst[n+1] = byte(country & 0xFF)
		n += 2
	}
	return dst[:n]
}

func copyTo(dst, src []byte) int {
	return copy(dst, src)
}

func main() {}
