package vector

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// ─────────────────────────────────────────────────
// Тест 1: Базовый — 5 точек, 2D
// ─────────────────────────────────────────────────

func TestBasicInsertAndSearch(t *testing.T) {
	g := NewGraph(EuclideanDistance)

	g.Insert(1, []float32{0, 0})
	g.Insert(2, []float32{0, 10})
	g.Insert(3, []float32{5, 5})
	g.Insert(4, []float32{10, 10})
	g.Insert(5, []float32{10, 0})

	results := g.Search([]float32{1, 1}, 3, 10)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Ближайший к (1,1) должен быть A(0,0) с distance²=2
	if results[0].ID != 1 {
		t.Errorf("expected nearest ID=1, got ID=%d", results[0].ID)
	}

	// Проверяем сортировку: расстояния должны расти
	for i := 1; i < len(results); i++ {
		if results[i].Distance < results[i-1].Distance {
			t.Errorf("results not sorted: [%d].dist=%f < [%d].dist=%f",
				i, results[i].Distance, i-1, results[i-1].Distance)
		}
	}
}

// ─────────────────────────────────────────────────
// Тест 2: Пустой граф и крайние случаи
// ─────────────────────────────────────────────────

func TestEdgeCases(t *testing.T) {
	g := NewGraph(EuclideanDistance)

	// Поиск в пустом графе — не должен паниковать
	results := g.Search([]float32{1, 2, 3}, 5, 10)
	if len(results) != 0 {
		t.Errorf("empty graph: expected 0 results, got %d", len(results))
	}

	// Одна нода
	g.Insert(1, []float32{1, 2, 3})
	results = g.Search([]float32{1, 2, 3}, 5, 10)
	if len(results) != 1 {
		t.Errorf("single node: expected 1 result, got %d", len(results))
	}

	// K больше чем нод в графе
	g.Insert(2, []float32{4, 5, 6})
	results = g.Search([]float32{0, 0, 0}, 100, 200)
	if len(results) != 2 {
		t.Errorf("K > nodes: expected 2 results, got %d", len(results))
	}
}

// ─────────────────────────────────────────────────
// Тест 3: Recall — точность поиска на 1000 точках
// ─────────────────────────────────────────────────
//
// Recall = "какой процент НАСТОЯЩИХ ближайших HNSW нашёл?"
//
// HNSW — приближённый алгоритм. Он может не найти идеальный ответ.
// Recall 0.95 = нашёл 95% настоящих ближайших. Это отличный результат.
// Recall < 0.80 = алгоритм работает плохо, что-то сломано.

func TestRecall(t *testing.T) {
	const (
		numVectors = 1000
		dim        = 32
		K          = 10
		efSearch   = 100
		numQueries = 50
	)

	rng := rand.New(rand.NewSource(42)) // фиксированный seed для воспроизводимости

	// 1. Генерируем случайные векторы
	vectors := make([][]float32, numVectors)
	for i := range vectors {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()
		}
		vectors[i] = vec
	}

	// 2. Вставляем в HNSW
	g := NewGraph(EuclideanDistance)
	for i, vec := range vectors {
		g.Insert(uint64(i), vec)
	}

	// 3. Для каждого запроса сравниваем HNSW и brute-force
	totalRecall := 0.0

	for q := 0; q < numQueries; q++ {
		query := make([]float32, dim)
		for j := range query {
			query[j] = rng.Float32()
		}

		// HNSW результат
		hnswResults := g.Search(query, K, efSearch)
		hnswIDs := make(map[uint64]bool)
		for _, r := range hnswResults {
			hnswIDs[r.ID] = true
		}

		// Brute-force: настоящие ближайшие (проверяем ВСЕ точки)
		type bruteItem struct {
			id   uint64
			dist float32
		}
		brute := make([]bruteItem, numVectors)
		for i, vec := range vectors {
			brute[i] = bruteItem{
				id:   uint64(i),
				dist: EuclideanDistance(query, vec),
			}
		}
		sort.Slice(brute, func(i, j int) bool {
			return brute[i].dist < brute[j].dist
		})

		// Считаем recall: сколько из top-K brute-force нашёл HNSW?
		hits := 0
		for i := 0; i < K && i < len(brute); i++ {
			if hnswIDs[brute[i].id] {
				hits++
			}
		}
		recall := float64(hits) / float64(K)
		totalRecall += recall
	}

	avgRecall := totalRecall / float64(numQueries)
	t.Logf("Recall@%d (ef=%d, %d vectors, %d dims): %.2f%%",
		K, efSearch, numVectors, dim, avgRecall*100)

	// Recall должен быть >= 80%. Для хорошего HNSW обычно 95%+
	if avgRecall < 0.80 {
		t.Errorf("recall too low: %.2f%% (expected >= 80%%)", avgRecall*100)
	}
}

// ─────────────────────────────────────────────────
// Тест 4: Cosine расстояние + нормализация
// ─────────────────────────────────────────────────

func TestCosineDistance(t *testing.T) {
	g := NewGraph(CosineDistance)

	// Три вектора с РАЗНЫМИ длинами, но одинаковым направлением
	// A и B смотрят в одну сторону (Cosine distance ≈ 0)
	// C смотрит в другую (Cosine distance ≈ 2)
	g.Insert(1, []float32{1, 0})   // A: вправо
	g.Insert(2, []float32{100, 0}) // B: тоже вправо, но "длиннее"
	g.Insert(3, []float32{0, 1})   // C: вверх (перпендикулярно)

	// Запрос: вектор "вправо" — A и B должны быть ближайшими
	results := g.Search([]float32{5, 0}, 2, 10)

	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Оба результата (A и B) должны иметь distance ≈ 0
	// C (перпендикулярный) должен иметь distance ≈ 1
	for _, r := range results {
		if r.ID == 3 {
			t.Errorf("perpendicular vector (ID=3) should not be in top-2 for cosine")
		}
	}
}

// ─────────────────────────────────────────────────
// Тест 5: Normalize + Euclidean = то же что Cosine
// ─────────────────────────────────────────────────
//
// Проверяем твой инсайт: после нормализации Euclidean даёт
// тот же порядок, что и Cosine.

func TestNormalizeEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(123))

	const dim = 16
	const numVecs = 100
	const K = 5

	// Генерируем векторы
	vectors := make([][]float32, numVecs)
	for i := range vectors {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*10 - 5 // от -5 до +5
		}
		vectors[i] = vec
	}

	query := make([]float32, dim)
	for j := range query {
		query[j] = rng.Float32()*10 - 5
	}

	// Вариант 1: Cosine distance (без нормализации)
	gCosine := NewGraph(CosineDistance)
	for i, vec := range vectors {
		gCosine.Insert(uint64(i), vec)
	}
	cosineResults := gCosine.Search(query, K, 50)

	// Вариант 2: Euclidean distance (с нормализацией)
	gEuclid := NewGraph(EuclideanDistance)
	for i, vec := range vectors {
		normalized := make([]float32, len(vec))
		copy(normalized, vec)
		Normalize(normalized)
		gEuclid.Insert(uint64(i), normalized)
	}
	normalizedQuery := make([]float32, len(query))
	copy(normalizedQuery, query)
	Normalize(normalizedQuery)
	euclidResults := gEuclid.Search(normalizedQuery, K, 50)

	// Сравниваем: оба должны вернуть одинаковые ID
	// (порядок может чуть отличаться из-за приближённости HNSW,
	//  поэтому проверяем пересечение множеств)
	cosineIDs := make(map[uint64]bool)
	for _, r := range cosineResults {
		cosineIDs[r.ID] = true
	}

	overlap := 0
	for _, r := range euclidResults {
		if cosineIDs[r.ID] {
			overlap++
		}
	}

	overlapRatio := float64(overlap) / float64(K)
	t.Logf("Cosine vs Normalized+Euclidean overlap: %d/%d (%.0f%%)",
		overlap, K, overlapRatio*100)

	// Минимум 60% совпадения (HNSW рандомизированный, идеала не будет)
	if overlapRatio < 0.6 {
		t.Errorf("low overlap: %.0f%% (expected >= 60%%)", overlapRatio*100)
	}
}

// ─────────────────────────────────────────────────
// Тест 6: efSearch влияет на recall
// ─────────────────────────────────────────────────
//
// Больше efSearch → лучше recall (но медленнее).
// Это главный tradeoff HNSW.

func TestEfSearchAffectsRecall(t *testing.T) {
	const (
		numVectors = 500
		dim        = 32
		K          = 10
	)

	rng := rand.New(rand.NewSource(99))

	vectors := make([][]float32, numVectors)
	for i := range vectors {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()
		}
		vectors[i] = vec
	}

	g := NewGraph(EuclideanDistance)
	for i, vec := range vectors {
		g.Insert(uint64(i), vec)
	}

	query := make([]float32, dim)
	for j := range query {
		query[j] = rng.Float32()
	}

	// Brute-force (истина)
	type bi struct {
		id   uint64
		dist float32
	}
	brute := make([]bi, numVectors)
	for i, vec := range vectors {
		brute[i] = bi{uint64(i), EuclideanDistance(query, vec)}
	}
	sort.Slice(brute, func(i, j int) bool {
		return brute[i].dist < brute[j].dist
	})
	trueTopK := make(map[uint64]bool)
	for i := 0; i < K; i++ {
		trueTopK[brute[i].id] = true
	}

	// Проверяем: ef=10 (маленький) vs ef=200 (большой)
	calcRecall := func(ef int) float64 {
		results := g.Search(query, K, ef)
		hits := 0
		for _, r := range results {
			if trueTopK[r.ID] {
				hits++
			}
		}
		return float64(hits) / float64(K)
	}

	recallLow := calcRecall(K)    // ef = K (минимально возможный)
	recallHigh := calcRecall(200) // ef = 200 (широкий поиск)

	t.Logf("Recall with ef=%d: %.0f%%", K, recallLow*100)
	t.Logf("Recall with ef=200: %.0f%%", recallHigh*100)

	// Recall при ef=200 должен быть >= recall при ef=K
	if recallHigh < recallLow {
		t.Errorf("higher ef should give better or equal recall")
	}
}

// ─────────────────────────────────────────────────
// Тест 7: Normalize — проверяем что длина = 1
// ─────────────────────────────────────────────────

func TestNormalize(t *testing.T) {
	vec := []float32{3, 4} // длина = √(9+16) = 5
	Normalize(vec)

	// После нормализации: [0.6, 0.8], длина = √(0.36+0.64) = 1.0
	length := float32(math.Sqrt(float64(vec[0]*vec[0] + vec[1]*vec[1])))

	if math.Abs(float64(length-1.0)) > 0.001 {
		t.Errorf("expected length 1.0, got %f (vec=%v)", length, vec)
	}

	// Нулевой вектор — не должен паниковать
	zero := []float32{0, 0, 0}
	Normalize(zero) // не должен падать

	if zero[0] != 0 || zero[1] != 0 || zero[2] != 0 {
		t.Errorf("zero vector should stay zero after normalize")
	}
}

// ─────────────────────────────────────────────────
// Тест 8: Структура графа — проверяем слои
// ─────────────────────────────────────────────────

func TestGraphStructure(t *testing.T) {
	g := NewGraph(EuclideanDistance)

	const n = 500
	rng := rand.New(rand.NewSource(77))

	for i := 0; i < n; i++ {
		vec := []float32{rng.Float32(), rng.Float32()}
		g.Insert(uint64(i), vec)
	}

	// Проверяем: максимальный уровень > 0 (при 500 нодах почти наверняка)
	if g.maxLevel == 0 {
		t.Errorf("expected maxLevel > 0 with %d nodes, got 0", n)
	}

	// Проверяем: entry point существует
	if _, ok := g.nodes[g.entryPointID]; !ok {
		t.Errorf("entry point ID=%d not found in nodes", g.entryPointID)
	}

	// Проверяем: количество нод правильное
	if len(g.nodes) != n {
		t.Errorf("expected %d nodes, got %d", n, len(g.nodes))
	}

	// Проверяем: ни у одной ноды нет соседей больше M/M0
	for id, node := range g.nodes {
		for level, neighbors := range node.Neighbors {
			maxN := g.maxNeighbors(level)
			if len(neighbors) > maxN {
				t.Errorf("node %d level %d: %d neighbors > max %d",
					id, level, len(neighbors), maxN)
			}
		}
	}

	t.Logf("Graph: %d nodes, maxLevel=%d, entryPoint=%d",
		len(g.nodes), g.maxLevel, g.entryPointID)

	// Распределение нод по слоям
	levelCounts := make(map[int]int)
	for _, node := range g.nodes {
		for l := 0; l <= node.Level; l++ {
			levelCounts[l]++
		}
	}
	for l := 0; l <= g.maxLevel; l++ {
		pct := float64(levelCounts[l]) / float64(n) * 100
		t.Logf("  Layer %d: %d nodes (%.1f%%)", l, levelCounts[l], pct)
	}
}

// Подавляем "imported and not used" для fmt
var _ = fmt.Sprintf

func BenchmarkSearch(b *testing.B) {
	const (
		numVectors = 5000
		dim        = 128
		k          = 10
		efSearch   = 1000
	)

	rng := rand.New(rand.NewSource(42))

	g := NewGraph(EuclideanDistance)
	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()
		}
		g.Insert(uint64(i), vec)
	}

	queries := make([][]float32, 1000)
	for i := range queries {
		q := make([]float32, dim)
		for j := range q {
			q[j] = rng.Float32()
		}
		queries[i] = q
	}
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		g.Search(queries[i%len(queries)], k, efSearch)
	}
}
