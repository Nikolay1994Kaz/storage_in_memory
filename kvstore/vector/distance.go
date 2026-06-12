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
