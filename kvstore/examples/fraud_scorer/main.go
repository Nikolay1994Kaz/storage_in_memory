//go:build tinygo.wasm

package main

import "unsafe"

// ─── Импорт host-функций из KVStore ───
// TinyGo использует //go:wasmimport для связывания с host.
// Эти функции реализованы в host_functions.go нашего сервера.

//go:wasmimport env kv_get
func kvGet(keyPtr uint32, keyLen uint32) uint32

//go:wasmimport env kv_set
func kvSet(keyPtr uint32, keyLen uint32, valPtr uint32, valLen uint32) uint32

//go:wasmimport env publish
func kvPublish(chanPtr uint32, chanLen uint32, msgPtr uint32, msgLen uint32) uint32

//go:wasmimport env log_info
func logInfo(msgPtr uint32, msgLen uint32)

//go:wasmimport env current_time_ms
func currentTimeMs() uint64

//go:wasmimport env tx_begin
func txBegin() uint32

//go:wasmimport env tx_commit
func txCommit() uint32

// ─── Вспомогательные функции ───

// ptr возвращает uint32-указатель на данные слайса.
func ptr(b []byte) uint32 {
	if len(b) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

// sptr возвращает указатель на строку.
func sptr(s string) uint32 {
	b := []byte(s)
	return ptr(b)
}

// logStr логирует строку через host-функцию.
func logStr(msg string) {
	b := []byte(msg)
	logInfo(ptr(b), uint32(len(b)))
}

// getValue читает значение из Store по ключу.
// Возвращает слайс байтов из WASM-памяти (offset 1024).
func getValue(keyPtr, keyLen uint32) []byte {
	valLen := kvGet(keyPtr, keyLen)
	if valLen == 0 {
		return nil
	}
	// Host-функция записала значение в memory[1024..1024+valLen]
	// Читаем из нашей же памяти через unsafe
	return readMemory(1024, valLen)
}

// readMemory читает данные из линейной памяти WASM по заданному offset.
func readMemory(offset, length uint32) []byte {
	result := make([]byte, length)
	src := (*[1 << 20]byte)(unsafe.Pointer(uintptr(offset)))
	copy(result, src[:length])
	return result
}

// setValue записывает ключ-значение через host-функцию.
func setValue(key, value string) {
	k := []byte(key)
	v := []byte(value)
	kvSet(ptr(k), uint32(len(k)), ptr(v), uint32(len(v)))
}

// pub публикует сообщение в канал.
func pub(channel, message string) {
	ch := []byte(channel)
	msg := []byte(message)
	kvPublish(ptr(ch), uint32(len(ch)), ptr(msg), uint32(len(msg)))
}

// ─── Бизнес-логика: Fraud Scorer ───

// parseAmount извлекает значение amount из простого JSON.
// Пример: {"amount":5000,"country":"NK"} → 5000
// Простой ручной парсер — не тянем зависимости.
func parseAmount(data []byte) int {
	// Ищем "amount":
	pattern := []byte(`"amount":`)
	idx := -1
	for i := 0; i <= len(data)-len(pattern); i++ {
		match := true
		for j := 0; j < len(pattern); j++ {
			if data[i+j] != pattern[j] {
				match = false
				break
			}
		}
		if match {
			idx = i + len(pattern)
			break
		}
	}
	if idx == -1 {
		return 0
	}

	// Парсим число
	num := 0
	for idx < len(data) && data[idx] >= '0' && data[idx] <= '9' {
		num = num*10 + int(data[idx]-'0')
		idx++
	}
	return num
}

// parseCountry извлекает country из JSON.
// Пример: {"amount":5000,"country":"NK"} → "NK"
func parseCountry(data []byte) string {
	pattern := []byte(`"country":"`)
	idx := -1
	for i := 0; i <= len(data)-len(pattern); i++ {
		match := true
		for j := 0; j < len(pattern); j++ {
			if data[i+j] != pattern[j] {
				match = false
				break
			}
		}
		if match {
			idx = i + len(pattern)
			break
		}
	}
	if idx == -1 {
		return ""
	}

	// Читаем до закрывающей кавычки
	end := idx
	for end < len(data) && data[end] != '"' {
		end++
	}
	return string(data[idx:end])
}

// itoa — простая int → string конвертация.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	// Reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// ─── Экспортируемая функция (вызывается триггером) ───

//export process
func process(keyPtr uint32, keyLen uint32) {
	// 1. Читаем ключ из памяти
	keyBytes := readMemory(keyPtr, keyLen)
	key := string(keyBytes)
	logStr("processing key: " + key)

	// 2. Получаем значение (JSON) из Store
	val := getValue(keyPtr, keyLen)
	if val == nil {
		logStr("key not found: " + key)
		return
	}
	logStr("value: " + string(val))

	// 3. ★ БИЗНЕС-ЛОГИКА: Подсчёт fraud score ★
	amount := parseAmount(val)
	country := parseCountry(val)

	score := 0

	// Правило 1: Большая сумма
	if amount > 10000 {
		score += 50
	} else if amount > 5000 {
		score += 30
	} else if amount > 1000 {
		score += 10
	}

	// Правило 2: Подозрительная страна
	if country == "NK" || country == "IR" || country == "SY" {
		score += 50
	}

	// Правило 3: Очень большая сумма = автоблок
	if amount > 50000 {
		score = 100
	}

	// 4. Определяем решение
	var decision string
	if score >= 80 {
		decision = "BLOCKED"
	} else if score >= 50 {
		decision = "REVIEW"
	} else {
		decision = "OK"
	}

	// 5. Записываем результат
	resultKey := "fraud:" + key
	resultValue := `{"score":` + itoa(score) +
		`,"decision":"` + decision +
		`","amount":` + itoa(amount) +
		`,"country":"` + country + `"}`

	txBegin() // <<< НАЧИНАЕМ ТРАНЗАКЦИЮ
	setValue(resultKey, resultValue)
	logStr("fraud check: " + key + " → score=" + itoa(score) + " " + decision)

	// 6. Публикуем событие если заблокировано
	if score >= 80 {
		pub("fraud_alerts", "BLOCKED: "+key+" score="+itoa(score)+" country="+country)
	}
	txCommit() // <<< ВЫПОЛНЯЕМ ВСЁ РАЗОМ

}

// main нужен для TinyGo, но ничего не делает.
// Вся логика — в export-функции process().
func main() {}
