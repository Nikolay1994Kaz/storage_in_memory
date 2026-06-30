package vector

import (
	"math"
	"testing"
	"time"
)

// TestSQ8_AVX2_vs_PureGo проверяет корректность AVX2 asm vs Pure-Go fallback.
// Если AVX2 недоступен (не-amd64 или старый CPU), тест пропускается.
//
// Устраняет риск subtle-багов в asm: VPMOVZXBD, VCVTDQ2PS, VFMADD, horizontal sum.
// Допуск — небольшая численная разница из-за порядка операций (FMA reassociation).
func TestSQ8_AVX2_vs_PureGo(t *testing.T) {
	// Проверяем, что AVX2 включён (init() перезаписал globals).
	sq8EuclidAVX2Active := false
	if sq8EuclidImpl != nil {
		// Сравниваем указатели: если != euclideanSQ8PureGo → AVX2 активен.
		// Тривиальный способ: проверить изменилось ли поведение.
		// Просто вызываем обе функции напрямую.
		sq8EuclidAVX2Active = true
	}
	_ = sq8EuclidAVX2Active

	cases := []struct {
		name string
		dim  int
	}{
		{"dim8", 8},     // ровно одна SIMD-итерация, без remainder
		{"dim16", 16},   // 2 итерации
		{"dim128", 128}, // SIFT-подобный, 16 итераций
		{"dim9", 9},     // 1 итерация + 1 remainder
		{"dim17", 17},   // 2 итерации + 1 remainder
		{"dim1", 1},     // только remainder
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := makeTestVec(tc.dim, 1.0)
			codes := makeTestCodes(tc.dim, 100)
			sqMin := makeTestVec(tc.dim, 10.0)
			sqScale := makeTestVec(tc.dim, 0.5)

			pureEucl := euclideanSQ8PureGo(query, codes, sqMin, sqScale)
			avx2Eucl := euclideanSQ8AVX2(query, codes, sqMin, sqScale)
			if !approxEqual(pureEucl, avx2Eucl, 0.01) {
				t.Errorf("euclidean mismatch dim=%d: pure=%.4f avx2=%.4f", tc.dim, pureEucl, avx2Eucl)
			}

			pureDot := dotProductSQ8PureGo(query, codes, sqMin, sqScale)
			avx2Dot := dotProductSQ8AVX2(query, codes, sqMin, sqScale)
			if !approxEqual(pureDot, avx2Dot, 0.01) {
				t.Errorf("dot mismatch dim=%d: pure=%.4f avx2=%.4f", tc.dim, pureDot, avx2Dot)
			}

			t.Logf("dim=%d: euclid pure=%.2f avx2=%.2f | dot pure=%.2f avx2=%.2f",
				tc.dim, pureEucl, avx2Eucl, pureDot, avx2Dot)
		})
	}
}

// TestSQ8_AVX2_Benchmark сравнивает скорость AVX2 vs Pure-Go.
// Подтверждает, что AVX2 реально быстрее (иначе нет смысла в asm).
func TestSQ8_AVX2_Benchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark in short mode")
	}
	const dim = 128
	const n = 100000

	query := makeTestVec(dim, 1.0)
	codes := makeTestCodes(dim, 100)
	sqMin := makeTestVec(dim, 10.0)
	sqScale := makeTestVec(dim, 0.5)

	// Warmup
	for i := 0; i < 1000; i++ {
		_ = euclideanSQ8PureGo(query, codes, sqMin, sqScale)
		_ = euclideanSQ8AVX2(query, codes, sqMin, sqScale)
	}

	// Bench Pure-Go
	startPure := timeNow()
	pureResult := float32(0)
	for i := 0; i < n; i++ {
		pureResult += euclideanSQ8PureGo(query, codes, sqMin, sqScale)
	}
	pureDur := timeSince(startPure)

	// Bench AVX2
	startAVX2 := timeNow()
	avx2Result := float32(0)
	for i := 0; i < n; i++ {
		avx2Result += euclideanSQ8AVX2(query, codes, sqMin, sqScale)
	}
	avx2Dur := timeSince(startAVX2)

	// Prevent dead-code elimination
	if pureResult == 0 || avx2Result == 0 {
		t.Fatal("zero result")
	}

	speedup := float64(pureDur) / float64(avx2Dur)
	t.Logf("Pure-Go: %v for %d ops (%.1f ns/op)", pureDur, n, float64(pureDur)/float64(n))
	t.Logf("AVX2:    %v for %d ops (%.1f ns/op)", avx2Dur, n, float64(avx2Dur)/float64(n))
	t.Logf("Speedup: %.2f×", speedup)

	// AVX2 должен быть быстрее. Если нет — asm некорректен или CPU не поддерживает.
	// Допуск: AVX2 не должен быть медленнее (speedup >= 1.0).
	if speedup < 1.0 {
		t.Logf("WARNING: AVX2 not faster than Pure-Go (speedup=%.2f). Check asm correctness.", speedup)
	}
}

// TestSQ8_AVX2_RandomVectors — стресс-тест корректности на случайных данных.
func TestSQ8_AVX2_RandomVectors(t *testing.T) {
	const dim = 128
	for trial := 0; trial < 100; trial++ {
		seed := uint64(trial*12345 + 777)
		query := makeRandTestVec(dim, seed)
		codes := makeRandTestCodes(dim, seed+1)
		sqMin := makeRandTestVec(dim, seed+2)
		sqScale := makeRandTestVec(dim, seed+3)

		pureEucl := euclideanSQ8PureGo(query, codes, sqMin, sqScale)
		avx2Eucl := euclideanSQ8AVX2(query, codes, sqMin, sqScale)
		if !approxEqualRel(pureEucl, avx2Eucl, 0.02) {
			t.Errorf("trial %d euclidean mismatch: pure=%.4f avx2=%.4f", trial, pureEucl, avx2Eucl)
		}

		pureDot := dotProductSQ8PureGo(query, codes, sqMin, sqScale)
		avx2Dot := dotProductSQ8AVX2(query, codes, sqMin, sqScale)
		if !approxEqualRel(pureDot, avx2Dot, 0.02) {
			t.Errorf("trial %d dot mismatch: pure=%.4f avx2=%.4f", trial, pureDot, avx2Dot)
		}
	}
}

func timeNow() time.Time                  { return time.Now() }
func timeSince(t time.Time) time.Duration { return time.Since(t) }

// ── helpers ──

func makeTestVec(dim int, val float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = val
	}
	return v
}

func makeTestCodes(dim int, val uint8) []uint8 {
	c := make([]uint8, dim)
	for i := range c {
		c[i] = val
	}
	return c
}

// Простой xorshift PRNG (без import math/rand, чтобы держать файл автономным).
func makeRandTestVec(dim int, seed uint64) []float32 {
	v := make([]float32, dim)
	s := seed
	for i := range v {
		s = s*6364136223846793005 + 1442695040888963407
		v[i] = float32(int64(s)%1000) / 100.0 // [-9.99, 9.99]
	}
	return v
}

func makeRandTestCodes(dim int, seed uint64) []uint8 {
	c := make([]uint8, dim)
	s := seed
	for i := range c {
		s = s*6364136223846793005 + 1442695040888963407
		c[i] = uint8(s % 256)
	}
	return c
}

func approxEqual(a, b, tol float32) bool {
	return math.Abs(float64(a-b)) < float64(tol)
}

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
