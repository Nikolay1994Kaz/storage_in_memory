package vector

import "math"

// Глобальные указатели на реализации функций расстояния.
// На amd64-системах с поддержкой AVX2 они будут перезаписаны в init()
// функциями из ассемблерного файла.
var (
	euclideanImpl  = EuclideanPureGo
	dotProductImpl = DotProductPureGo
)

// EuclideanDistance вычисляет квадрат евклидова расстояния между двумя векторами.
func EuclideanDistance(a, b []float32) float32 {
	return euclideanImpl(a, b)
}

func EuclideanPureGo(a, b []float32) float32 {
	var sum float32
	for i := range a {
		diff := a[i] - b[i]
		sum += diff * diff
	}
	return sum
}

// CosineDistance вычисляет косинусное расстояние между двумя векторами.
func CosineDistance(a, b []float32) float32 {
	dot := dotProductImpl(a, b)
	normA := dotProductImpl(a, a)
	normB := dotProductImpl(b, b)

	if normA == 0 || normB == 0 {
		return 1 // максимальное расстояние
	}

	similarity := dot / float32(math.Sqrt(float64(normA))*math.Sqrt(float64(normB)))
	return 1 - similarity
}

func Normalize(vec []float32) {
	norm := dotProductImpl(vec, vec)
	if norm == 0 {
		return
	}

	length := float32(math.Sqrt(float64(norm)))
	for i := range vec {
		vec[i] /= length
	}
}

// DotProductDistance — расстояние для pre-normalized векторов (1 - dot_product).
func DotProductDistance(a, b []float32) float32 {
	return 1 - dotProductImpl(a, b)
}

func DotProductPureGo(a, b []float32) float32 {
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return dot
}

// DistanceFunc — тип функции расстояния.
type DistanceFunc func(a, b []float32) float32

// =============================================================================
// SQ8 (Scalar Quantization, int8) distance functions.
//
// Asymmetric Distance Computation (ADC): query в исходном float32, codes в int8.
// Деквантование: approx = sqMin[d] + float32(code[d]) * sqScale[d].
// Асимметричный вариант точнее симметричного (query не теряет точность) —
// стандарт для всех промышленных систем (FAISS, Qdrant, Milvus).
//
// Глобальные указатели sq8EuclidImpl/sq8DotImpl перезаписываются AVX2-реализацией
// в distance_amd64.go init() (Шаг 5). По умолчанию — Pure-Go.
// =============================================================================

var (
	sq8EuclidImpl = euclideanSQ8PureGo
	sq8DotImpl    = dotProductSQ8PureGo
)

// euclideanSQ8PureGo — квадрат евклидова расстояния, ADC over uint8 codes.
// query — исходный float32 (len=dim), codes — uint8 slab (len=dim, значения 0..255),
// sqMin/sqScale — per-dimension деквантование params (len=dim).
func euclideanSQ8PureGo(query []float32, codes []uint8, sqMin, sqScale []float32) float32 {
	var sum float32
	for d := 0; d < len(query); d++ {
		approx := sqMin[d] + float32(codes[d])*sqScale[d]
		diff := query[d] - approx
		sum += diff * diff
	}
	return sum
}

// dotProductSQ8PureGo — dot product (для cosine/dot distance), ADC over uint8 codes.
func dotProductSQ8PureGo(query []float32, codes []uint8, sqMin, sqScale []float32) float32 {
	var sum float32
	for d := 0; d < len(query); d++ {
		approx := sqMin[d] + float32(codes[d])*sqScale[d]
		sum += query[d] * approx
	}
	return sum
}
