//go:build tinygo.wasm

package main

import "unsafe"

// ─── Host-функции (импорт из KVStore) ─────────────────────
//
//go:wasmimport env kv_get
func kvGet(keyPtr, keyLen uint32) uint32

//go:wasmimport env kv_set
func kvSet(keyPtr, keyLen, valPtr, valLen uint32) uint32

//go:wasmimport env publish
func kvPublish(chanPtr, chanLen, msgPtr, msgLen uint32) uint32

//go:wasmimport env tx_begin
func txBegin() uint32

//go:wasmimport env tx_commit
func txCommit() uint32

// ─── Memory Layout (договор с Host) ──────────────────────
//
//  Offset 0..1023     — INPUT_REGION  (Host пишет key сюда)
//  Offset 1024..5119  — VALUE_REGION  (kv_get записывает результат)
//  Offset 5120..9215  — OUTPUT_REGION (Guest пишет результат для Host)
//
// Ни Guest, ни Host НИКОГДА не аллоцируют память динамически.
// Всё общение — через эти три фиксированных окна.

const (
	inputOffset  = 16384
	valueOffset  = 20480
	outputOffset = 24576
)

// ─── Статические буферы ──────────────────────────────────
// Вся «рабочая» память — глобальные массивы фиксированного размера.
// TinyGo выделит их один раз при _initialize. Потом — ноль аллокаций.

var numBuf [20]byte // буфер для itoa (макс 20 цифр для int64)

// ─── Reactor entry points ────────────────────────────────

// _initialize вызывается Host-ом ОДИН раз сразу после InstantiateModule.
// Здесь можно сделать одноразовую инициализацию.
// В нашем случае буферы уже готовы (глобальные), делать нечего.
//
//export _initialize
func _initialize() {}

// process — основная функция. Host вызывает её на КАЖДЫЙ триггер.
// Один и тот же инстанс, миллионы вызовов, ноль аллокаций.
//
// Аргументы:
//
//	keyPtr — offset в linear memory где лежит key (Host записал туда)
//	keyLen — длина key в байтах
//
// Возвращает:
//
//	длину результата, записанного в OUTPUT_REGION (offset 5120)
//	Host прочитает output оттуда.
//
//export process
func process(keyPtr, keyLen uint32) uint32 {
	// 1. Читаем value из Store через host-функцию
	//    kvGet сам запишет результат в valueOffset (1024)
	valLen := kvGet(keyPtr, keyLen)
	if valLen == 0 {
		return 0
	}

	// 2. Получаем slice НА ТУ ЖЕ ПАМЯТЬ (zero-copy, без make)
	val := ptrToSlice(valueOffset, valLen)

	// 3. Парсим JSON поля (без аллокаций — работаем с []byte)
	amount := parseAmount(val)
	countryCode := parseCountryCode(val)

	// 4. Считаем fraud score
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

	// 5. Определяем решение
	var decision []byte
	if score >= 80 {
		decision = []byte("BLOCKED")
	} else if score >= 50 {
		decision = []byte("REVIEW")
	} else {
		decision = []byte("OK")
	}

	// 6. Формируем JSON-результат прямо в OUTPUT_REGION
	out := ptrToSlice(outputOffset, 4096)
	n := buildResult(out, score, amount, decision, countryCode)

	// 7. Записываем fraud-результат обратно в Store
	keySlice := ptrToSlice(keyPtr, keyLen)
	resultKey := buildResultKey(keySlice)
	kvSet(ptr(resultKey), uint32(len(resultKey)), uint32(outputOffset), uint32(n))

	// 8. Если BLOCKED — публикуем алерт
	if score >= 80 {
		alert := buildAlert(keySlice, score, countryCode)
		ch := []byte("fraud_alerts")
		kvPublish(ptr(ch), uint32(len(ch)), ptr(alert), uint32(len(alert)))
	}

	return uint32(n)
}

// ─── Zero-alloc helpers ──────────────────────────────────

// ptrToSlice — создаёт Go slice, указывающий на linear memory.
// НЕ копирует данные. НЕ аллоцирует. Это unsafe.Slice.
func ptrToSlice(offset, length uint32) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(offset))), int(length))
}

func ptr(b []byte) uint32 {
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

// parseAmount ищет "amount": в JSON и парсит число.
// Работает с []byte, не создаёт string.
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
// Никаких string, никаких аллокаций.
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
// Переиспользует глобальный буфер — zero alloc.
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
// Формат: {"score":50,"decision":"REVIEW","amount":15000,"country":"NK"}
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
// Пишет в OUTPUT_REGION + 256 (вторая половина output буфера).
func buildResultKey(key []byte) []byte {
	// Используем область output+2048 как scratch для ключа
	dst := ptrToSlice(outputOffset+2048, uint32(6+len(key)))
	copy(dst, []byte("fraud:"))
	copy(dst[6:], key)
	return dst
}

// buildAlert формирует строку алерта.
func buildAlert(key []byte, score int, country uint16) []byte {
	// Используем область output+3072 как scratch для алерта
	dst := ptrToSlice(outputOffset+3072, 1024)
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

// main() нужен для TinyGo, но ПУСТОЙ.
// С -scheduler=none и reactor pattern, _start НЕ вызывается Host-ом.
// Мы явно вызываем _initialize вместо этого.
func main() {}
