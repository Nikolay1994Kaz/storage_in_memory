//go:build amd64

// 🚨Тег обязателен: euclideanSQ8AVX2/dotProductSQ8AVX2 объявлены в
// distance_sq_amd64.go, то есть существуют ТОЛЬКО на amd64. Без тега файл
// ломал сборку тестов всего пакета на arm64 — `go vet ./kvstore/vector/` под
// GOARCH=arm64 падал с `undefined: euclideanSQ8AVX2`, а значит на Apple
// Silicon и ARM-серверах тесты vector нельзя было запустить вовсе. CI этого
// не показывал: ubuntu-latest — amd64. Прод-код при этом собирался нормально,
// поэтому дефект жил только в тестах.

package vector

import (
	"testing"
	"time"

	"golang.org/x/sys/cpu"
)

// requireAVX2 — AVX2-функции зовутся здесь НАПРЯМУЮ, минуя диспетчер
// sq8EuclidImpl. На amd64 без AVX2 это не медленный путь, а SIGILL, поэтому
// пропуск обязан быть явным и с причиной: молчаливый скип уже однажды скрыл
// три неисполнявшихся теста вокруг вставки.
func requireAVX2(t *testing.T) {
	t.Helper()
	if !cpu.X86.HasAVX2 {
		t.Skip("CPU без AVX2: asm-функции вызываются напрямую и дали бы SIGILL")
	}
}

// TestSQ8_AVX2_vs_PureGo проверяет корректность AVX2 asm против Pure-Go.
//
// Устраняет риск subtle-багов в asm: VPMOVZXBD, VCVTDQ2PS, VFMADD, horizontal sum.
// Допуск — небольшая численная разница из-за порядка операций (FMA reassociation).
func TestSQ8_AVX2_vs_PureGo(t *testing.T) {
	requireAVX2(t)

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
	requireAVX2(t)
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

	// 🚨Было t.Logf("WARNING: …") — то есть единственная причина, по которой этот
	// asm вообще существует, не проверялась ничем. Порог на ОТНОШЕНИИ, а не на
	// абсолютном времени: посторонняя нагрузка тормозит обе руки разом и
	// отношение переживает, тогда как абсолютный порог падал бы от чужого
	// процесса. Запас велик — AVX2 обрабатывает 8 размерностей за итерацию,
	// ожидание кратное, порог всего 1.0.
	if speedup < 1.0 {
		t.Errorf("AVX2 не быстрее Pure-Go (speedup=%.2f) — asm не окупает своего существования", speedup)
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

// approxEqual / approxEqualRel живут в approx_test.go — вне тега amd64,
// потому что нужны и файлам, не связанным с asm.
