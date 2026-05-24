package vector

import "math"

// EuclideanDistance вычисляет квадрат евклидова расстояния между двумя векторами.
//
// Почему квадрат, а не обычное расстояние?
// Обычное = √((a₁-b₁)² + (a₂-b₂)² + ...)
// Квадрат =   (a₁-b₁)² + (a₂-b₂)² + ...
//
// Корень (√) — дорогая операция (~20 тактов CPU).
// А нам нужно только СРАВНИВАТЬ расстояния: "A ближе к B, или к C?"
// Если   dist(A,B) < dist(A,C)
// то и dist²(A,B) < dist²(A,C)  — порядок сохраняется!
// Поэтому корень не нужен. Это стандартная оптимизация во всех vector DB.
func EuclideanDistance(a, b []float32) float32 {
	var sum float32

	// Проходим по каждой паре чисел и считаем (a[i] - b[i])²
	for i := range a {
		diff := a[i] - b[i]
		sum += diff * diff
	}

	return sum
}

// CosineDistance вычисляет косинусное расстояние между двумя векторами.
//
// Косинусное расстояние отвечает на вопрос: "насколько два вектора НАПРАВЛЕНЫ в одну сторону?"
// Не важна длина (амплитуда), важно только НАПРАВЛЕНИЕ.
//
// Пример из жизни:
//
//	Пользователь A читает: [10 статей про Go, 2 про Python]   → направление: "Go-шник"
//	Пользователь B читает: [100 статей про Go, 20 про Python]  → направление: "Go-шник"
//	Пользователь C читает: [2 статьи про Go, 10 про Python]    → направление: "Python-ист"
//
//	A и B — разные "длины" (A прочитал 12, B прочитал 120), но НАПРАВЛЕНИЕ одинаковое.
//	Евклидово расстояние скажет "A и B далеко" (числа разные).
//	Косинусное расстояние скажет "A и B очень близки" (направление одно).
//
// Формула:
//
//	similarity = (a·b) / (|a| × |b|)
//	distance   = 1 - similarity
//
//	a·b    = a₁×b₁ + a₂×b₂ + ... + aₙ×bₙ  (скалярное произведение)
//	|a|    = √(a₁² + a₂² + ... + aₙ²)       (длина вектора)
func CosineDistance(a, b []float32) float32 {
	var dot, normA, normB float32

	for i := range a {
		dot += a[i] * b[i]   // скалярное произведение: сумма попарных умножений
		normA += a[i] * a[i] // квадрат длины вектора A
		normB += b[i] * b[i] // квадрат длины вектора B
	}

	// Защита от деления на ноль (если один из векторов = [0, 0, ..., 0])
	if normA == 0 || normB == 0 {
		return 1 // максимальное расстояние
	}

	// similarity = dot / (√normA × √normB)
	// distance = 1 - similarity
	similarity := dot / float32(math.Sqrt(float64(normA))*math.Sqrt(float64(normB)))

	return 1 - similarity
}

func Normalize(vec []float32) {
	var norm float32

	for _, v := range vec {
		norm += v * v

	}
	if norm == 0 {
		return
	}

	length := float32(math.Sqrt(float64(norm)))
	for i := range vec {
		vec[i] /= length
	}
}

// DotProductDistance — сверхбыстрое расстояние для PRE-NORMALIZED векторов.
//
// Если все векторы нормализованы (|v| = 1.0), то:
//
//	CosineDistance(a, b) = 1 - dot(a, b) / (|a| × |b|)
//	                     = 1 - dot(a, b) / (1.0 × 1.0)
//	                     = 1 - dot(a, b)
//
// Один цикл, ноль sqrt, ноль делений. ×3 быстрее CosineDistance.
//
// ⚠️ Используй ТОЛЬКО через NewVectorStoreCosine, который автоматически
// нормализует все вставляемые векторы и запросы.
func DotProductDistance(a, b []float32) float32 {
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return 1 - dot
}

// DistanceFunc — тип функции расстояния.
// Позволяет пользователю выбрать метрику при создании индекса.
//
// Зачем type для функции?
// Ты уже видел этот паттерн:
//
//	type Handler func(cs *ConnState, args []protocol.Value) protocol.Value
//
// В server.go — тот же приём. Вместо жёсткого вызова конкретной функции,
// мы передаём функцию как параметр. Хочешь Euclidean — передай Euclidean.
// Хочешь Cosine — передай Cosine.
type DistanceFunc func(a, b []float32) float32
