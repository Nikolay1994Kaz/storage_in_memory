package vector

import "math"

// Сравнение чисел с допуском — общие хелперы тестов пакета.
//
// ⚠Лежат ОТДЕЛЬНО от distance_sq_test.go намеренно: тот файл закрыт тегом
// `//go:build amd64`, потому что зовёт asm-функции AVX2 напрямую. Пока эти две
// функции жили там, они уезжали за тег вместе с ним, и adc_brute_microbench_test.go
// переставал компилироваться на arm64 — ошибка выглядела как `undefined:
// approxEqualRel`, то есть указывала не туда, где причина.

// approxEqual — абсолютный допуск. Годится, когда масштаб величин известен.
func approxEqual(a, b, tol float32) bool {
	return math.Abs(float64(a-b)) < float64(tol)
}

// approxEqualRel — относительный допуск: |a-b| / (|a|+|b|). Нужен там, где
// величины отличаются на порядки и абсолютный допуск теряет смысл.
func approxEqualRel(a, b, tol float32) bool {
	if a == 0 && b == 0 {
		return true
	}
	denom := math.Abs(float64(a)) + math.Abs(float64(b))
	if denom == 0 {
		return true
	}
	return math.Abs(float64(a-b))/denom < float64(tol)
}
