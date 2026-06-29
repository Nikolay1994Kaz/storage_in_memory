package vector

import (
	"math"
	"unsafe"
)

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

// Metric — first-class идентификатор метрики расстояния.
//
// Зачем enum, а не сравнение указателей DistanceFunc: SQ8-обходу нужно знать,
// какой ADC применять (euclidean-L2² или dot/1-dot). Раньше это выводилось
// сравнением funcval-указателя через unsafe.Pointer (isDotDistance) — хрупкий
// приём: любая обёртка/замыкание над метрикой ломала сравнение МОЛЧА (ровно
// этот класс багов укусил нас в e4aa4e5, где сравнивали с внутренним impl).
// Теперь метрика — явные данные: компилятор ловит незакрытые case в switch,
// а dotMode выводится из значения, а не из адреса функции.
type Metric uint8

const (
	// MetricAuto — метрика не задана явно; выводится из сконфигурированной
	// DistanceFunc через inferMetric (legacy-мост). Нулевое значение, чтобы
	// существующие вызовы, задающие только Distance, продолжали работать.
	MetricAuto Metric = iota
	// MetricEuclidean — квадрат евклидова расстояния (L2²).
	MetricEuclidean
	// MetricDot — 1 - dot product; контракт: pre-normalized векторы.
	MetricDot
	// MetricCosine — косинусное расстояние (1 - cos); rerank нормализует сам.
	MetricCosine
)

// DistanceFunc возвращает каноническую функцию расстояния для метрики.
// MetricAuto не имеет канонической функции — должен быть разрешён до вызова.
func (m Metric) DistanceFunc() DistanceFunc {
	switch m {
	case MetricEuclidean:
		return EuclideanDistance
	case MetricDot:
		return DotProductDistance
	case MetricCosine:
		return CosineDistance
	default:
		return EuclideanDistance
	}
}

// IsDot сообщает, нужен ли SQ8-обходу dot-ADC (а не euclidean-ADC).
// Истинно для dot и cosine (обе работают через dot product над codes).
func (m Metric) IsDot() bool {
	return m == MetricDot || m == MetricCosine
}

// inferMetric — ЕДИНСТВЕННЫЙ оставшийся compat-мост: выводит Metric из
// DistanceFunc сравнением funcval-указателей. Используется ровно один раз при
// инициализации стора, когда метрика не задана явно (MetricAuto). Новый код
// должен задавать LeveledConfig.Metric напрямую и не полагаться на вывод.
// Неизвестная функция трактуется как euclidean (исторический дефолт).
func inferMetric(fn DistanceFunc) Metric {
	if fn == nil {
		return MetricEuclidean
	}
	fnPtr := *(*uintptr)(unsafe.Pointer(&fn))
	dot := DistanceFunc(DotProductDistance)
	cos := DistanceFunc(CosineDistance)
	switch fnPtr {
	case *(*uintptr)(unsafe.Pointer(&dot)):
		return MetricDot
	case *(*uintptr)(unsafe.Pointer(&cos)):
		return MetricCosine
	default:
		return MetricEuclidean
	}
}

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
