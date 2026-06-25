//go:build amd64

package vector

import "golang.org/x/sys/cpu"

// AVX2-реализации SQ8 distance (ADC — Asymmetric Distance Computation).
// asm-файл: distance_sq_amd64.s
//
// Алгоритм (8 размерностей за итерацию):
//  1. VPMOVZXBD — 8 uint8 bytes → 8 int32 (zero-extend)
//  2. VCVTDQ2PS — 8 int32 → 8 float32
//  3. VMULPS code, scale → VADDPS min → approx (деквантование)
//  4. VSUBPS query, approx → diff
//  5. VFMADD231PS sum += diff*diff (euclidean) ИЛИ sum += query*approx (dot)

//go:noescape
func euclideanSQ8AVX2(query []float32, codes []uint8, sqMin, sqScale []float32) float32

//go:noescape
func dotProductSQ8AVX2(query []float32, codes []uint8, sqMin, sqScale []float32) float32

func init() {
	if cpu.X86.HasAVX2 {
		sq8EuclidImpl = euclideanSQ8AVX2
		sq8DotImpl = dotProductSQ8AVX2
	}
}
