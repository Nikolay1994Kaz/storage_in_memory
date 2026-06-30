package vector

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// ─────────────────────────────────────────────────
// Тест 1: Базовый — 5 точек, 2D
// ─────────────────────────────────────────────────

func TestBasicInsertAndSearch(t *testing.T) {
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	idA := g.Insert([]float32{0, 0})
	g.Insert([]float32{0, 10})
	g.Insert([]float32{5, 5})
	g.Insert([]float32{10, 10})
	g.Insert([]float32{10, 0})

	results := g.Search([]float32{1, 1}, 3, 10)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Ближайший к (1,1) должен быть A(0,0) с distance²=2
	if results[0].ID != uint64(idA) {
		t.Errorf("expected nearest ID=%d, got ID=%d", idA, results[0].ID)
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
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	// Поиск в пустом графе — не должен паниковать
	results := g.Search([]float32{1, 2, 3}, 5, 10)
	if len(results) != 0 {
		t.Errorf("empty graph: expected 0 results, got %d", len(results))
	}

	// Одна нода
	g.Insert([]float32{1, 2, 3})
	results = g.Search([]float32{1, 2, 3}, 5, 10)
	if len(results) != 1 {
		t.Errorf("single node: expected 1 result, got %d", len(results))
	}

	// K больше чем нод в графе
	g.Insert([]float32{4, 5, 6})
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
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	for _, vec := range vectors {
		g.Insert(vec)
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
	g := NewGraph(CosineDistance, tcmalloc.NewTCMallocStore(1))

	// Три вектора с РАЗНЫМИ длинами, но одинаковым направлением
	// A и B смотрят в одну сторону (Cosine distance ≈ 0)
	// C смотрит в другую (Cosine distance ≈ 2)
	g.Insert([]float32{1, 0})        // A: вправо
	g.Insert([]float32{100, 0})      // B: тоже вправо, но "длиннее"
	idC := g.Insert([]float32{0, 1}) // C: вверх (перпендикулярно)

	// Запрос: вектор "вправо" — A и B должны быть ближайшими
	results := g.Search([]float32{5, 0}, 2, 10)

	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Оба результата (A и B) должны иметь distance ≈ 0
	// C (перпендикулярный) должен иметь distance ≈ 1
	for _, r := range results {
		if r.ID == uint64(idC) {
			t.Errorf("perpendicular vector (ID=%d) should not be in top-2 for cosine", idC)
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
	gCosine := NewGraph(CosineDistance, tcmalloc.NewTCMallocStore(1))
	for _, vec := range vectors {
		gCosine.Insert(vec)
	}
	cosineResults := gCosine.Search(query, K, 50)

	// Вариант 2: Euclidean distance (с нормализацией)
	gEuclid := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	for _, vec := range vectors {
		normalized := make([]float32, len(vec))
		copy(normalized, vec)
		Normalize(normalized)
		gEuclid.Insert(normalized)
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

	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	for _, vec := range vectors {
		g.Insert(vec)
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
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	const n = 500
	rng := rand.New(rand.NewSource(77))

	for i := 0; i < n; i++ {
		vec := []float32{rng.Float32(), rng.Float32()}
		g.Insert(vec)
	}

	// Проверяем: максимальный уровень > 0 (при 500 нодах почти наверняка)
	if g.maxLevel == 0 {
		t.Errorf("expected maxLevel > 0 with %d nodes, got 0", n)
	}

	// Проверяем: entry point существует и жив
	if g.entryPointID >= uint64(len(g.nodes)) || !g.nodes[g.entryPointID].Alive {
		t.Errorf("entry point ID=%d not found or not alive", g.entryPointID)
	}

	// Проверяем: количество нод правильное
	if g.nodeCount != n {
		t.Errorf("expected %d nodes, got %d", n, g.nodeCount)
	}

	// Проверяем: ни у одной ноды нет соседей больше M/M0
	for i := range g.nodes {
		node := &g.nodes[i]
		if !node.Alive {
			continue
		}
		for level := 0; level <= node.Level; level++ {
			neighbors := g.getNeighbors(node.NeighborsHandle, level)
			maxN := g.maxNeighbors(level)
			if len(neighbors) > maxN {
				t.Errorf("node %d level %d: %d neighbors > max %d",
					i, level, len(neighbors), maxN)
			}
		}
	}

	t.Logf("Graph: %d nodes, maxLevel=%d, entryPoint=%d",
		g.nodeCount, g.maxLevel, g.entryPointID)

	// Распределение нод по слоям
	levelCounts := make(map[int]int)
	for i := range g.nodes {
		node := &g.nodes[i]
		if !node.Alive {
			continue
		}
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

	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()
		}
		g.Insert(vec)
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

// ─────────────────────────────────────────────────
// Тест 9: Delete — базовое удаление
// ─────────────────────────────────────────────────

func TestDelete_Basic(t *testing.T) {
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	g.Insert([]float32{0, 0})
	g.Insert([]float32{1, 0})
	idC := g.Insert([]float32{2, 0})
	g.Insert([]float32{3, 0})
	g.Insert([]float32{4, 0})

	if g.Len() != 5 {
		t.Fatalf("expected 5 nodes, got %d", g.Len())
	}

	// Удаляем ноду C (средняя)
	ok := g.Delete(uint64(idC))
	if !ok {
		t.Fatal("Delete returned false for existing node")
	}
	if g.Len() != 4 {
		t.Fatalf("expected 4 nodes after delete, got %d", g.Len())
	}

	// Повторное удаление — должно вернуть false
	ok = g.Delete(uint64(idC))
	if ok {
		t.Fatal("Delete returned true for already-deleted node")
	}

	// Поиск должен найти только оставшиеся ноды
	results := g.Search([]float32{2, 0}, 5, 10)
	for _, r := range results {
		if r.ID == uint64(idC) {
			t.Errorf("deleted node ID=%d found in search results", idC)
		}
	}

	// Ни у кого не должно быть ссылки на удалённую ноду
	for i := range g.nodes {
		node := &g.nodes[i]
		if !node.Alive {
			continue
		}
		for level := 0; level <= node.Level; level++ {
			neighbors := g.getNeighbors(node.NeighborsHandle, level)
			for _, nid := range neighbors {
				if nid == uint64(idC) {
					t.Errorf("node %d level %d still references deleted node %d", i, level, idC)
				}
			}
		}
	}
}

// ─────────────────────────────────────────────────
// Тест 10: Delete entry point
// ─────────────────────────────────────────────────

func TestDelete_EntryPoint(t *testing.T) {
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	// Вставляем 100 нод
	rng := rand.New(rand.NewSource(55))
	for i := 0; i < 100; i++ {
		vec := []float32{rng.Float32(), rng.Float32()}
		g.Insert(vec)
	}

	// Удаляем entry point
	ep := g.entryPointID
	g.Delete(ep)

	// Entry point должен смениться
	if g.entryPointID == ep {
		t.Error("entry point should have changed after deletion")
	}

	// Новый entry point должен существовать и быть живым
	if g.entryPointID >= uint64(len(g.nodes)) || !g.nodes[g.entryPointID].Alive {
		t.Error("new entry point does not exist or is not alive")
	}

	// Поиск всё ещё работает
	results := g.Search([]float32{0.5, 0.5}, 5, 50)
	if len(results) == 0 {
		t.Error("search returned 0 results after entry point deletion")
	}
}

// ─────────────────────────────────────────────────
// Тест 11: Удалить все, потом снова вставить
// ─────────────────────────────────────────────────

func TestDelete_AllAndRebuild(t *testing.T) {
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	// Вставляем 10 нод
	var ids []uint32
	for i := 0; i < 10; i++ {
		idx := g.Insert([]float32{float32(i), 0})
		ids = append(ids, idx)
	}

	// Удаляем все
	for _, idx := range ids {
		g.Delete(uint64(idx))
	}

	if g.Len() != 0 {
		t.Fatalf("expected 0 nodes, got %d", g.Len())
	}

	// Вставляем снова — не должно паниковать
	for i := 100; i < 105; i++ {
		g.Insert([]float32{float32(i), 0})
	}

	if g.Len() != 5 {
		t.Fatalf("expected 5 nodes after rebuild, got %d", g.Len())
	}

	// Поиск работает
	results := g.Search([]float32{102, 0}, 3, 10)
	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}
}

// ─────────────────────────────────────────────────
// Тест 12: Recall после массового удаления
// ─────────────────────────────────────────────────
//
// Вставляем 500 нод, удаляем 200, проверяем что поиск
// по оставшимся 300 всё ещё имеет хороший recall.

func TestDelete_RecallAfterMassDelete(t *testing.T) {
	const (
		totalNodes  = 500
		deleteCount = 200
		dim         = 16
		K           = 10
		efSearch    = 100
	)

	rng := rand.New(rand.NewSource(42))

	// Вставляем
	g := NewGraph(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	vectors := make(map[uint64][]float32)
	for i := 0; i < totalNodes; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()
		}
		idx := g.Insert(vec)
		vectors[uint64(idx)] = vec
	}

	// Удаляем первые deleteCount
	for i := 0; i < deleteCount; i++ {
		g.Delete(uint64(i))
		delete(vectors, uint64(i))
	}

	if g.Len() != totalNodes-deleteCount {
		t.Fatalf("expected %d nodes, got %d", totalNodes-deleteCount, g.Len())
	}

	// Генерируем запрос
	query := make([]float32, dim)
	for j := range query {
		query[j] = rng.Float32()
	}

	// HNSW поиск
	results := g.Search(query, K, efSearch)
	hnswIDs := make(map[uint64]bool)
	for _, r := range results {
		hnswIDs[r.ID] = true
		// Убеждаемся что удалённые ноды не возвращаются
		if r.ID < uint64(deleteCount) {
			t.Errorf("deleted node ID=%d found in results", r.ID)
		}
	}

	// Brute-force по оставшимся
	type bi struct {
		id   uint64
		dist float32
	}
	var brute []bi
	for id, vec := range vectors {
		brute = append(brute, bi{id, EuclideanDistance(query, vec)})
	}
	sort.Slice(brute, func(i, j int) bool {
		return brute[i].dist < brute[j].dist
	})

	// Recall
	hits := 0
	for i := 0; i < K && i < len(brute); i++ {
		if hnswIDs[brute[i].id] {
			hits++
		}
	}
	recall := float64(hits) / float64(K)
	t.Logf("Recall after deleting %d/%d nodes: %.0f%%", deleteCount, totalNodes, recall*100)

	if recall < 0.60 {
		t.Errorf("recall too low after mass delete: %.0f%%", recall*100)
	}
}

// Тест: Проверка работы searchState (bitset: isVisited, setVisited и сброс/аллокация в acquire)
// ──────────────────────────────────────────────────────────────────────────────────────────
func TestSearchState_VisitedAndAcquire(t *testing.T) {
	// 1. Тестируем базовые функции isVisited и setVisited для разных ID
	state := &searchState{
		visited: make([]uint64, 4), // 4 * 64 = 256 бит
	}

	testIDs := []uint64{0, 5, 63, 64, 127, 128, 255}
	for _, id := range testIDs {
		if state.isVisited(id) {
			t.Errorf("node ID=%d is visited initially, expected false", id)
		}
		state.setVisited(id)
		if !state.isVisited(id) {
			t.Errorf("node ID=%d is not visited after setVisited, expected true", id)
		}
	}

	// Убедимся, что соседние невыбранные ноды остались ложными
	unvisitedIDs := []uint64{1, 4, 62, 65, 126, 129, 254}
	for _, id := range unvisitedIDs {
		if state.isVisited(id) {
			t.Errorf("node ID=%d is visited, expected false (should not be affected by neighbor set)", id)
		}
	}

	// 2. Тестируем acquire — сброс и сохранение емкости
	// Настроим state с какими-то данными в кучах и буферах
	state.candidates = append(state.candidates, item{id: 10, dist: 1.5})
	state.results = append(state.results, item{id: 20, dist: 2.5})
	state.collected = append(state.collected, item{id: 30, dist: 3.5})

	// Вызываем acquire для маленького размера графа (nodeSlots = 100 -> needed = 2 uint64)
	// Должно занулиться visited, но capacity остаться >= 2
	state.acquire(100)

	// Проверяем, что все ранее установленные биты занулились (только для ID < 100, так как длина visited теперь 2 uint64, то есть 128 бит)
	for _, id := range testIDs {
		if id < 100 {
			if state.isVisited(id) {
				t.Errorf("node ID=%d is still visited after acquire, expected reset", id)
			}
		}
	}

	// Проверяем сброс длин куч и буферов при сохранении capacity
	if len(state.candidates) != 0 || cap(state.candidates) < 1 {
		t.Errorf("candidates heap length not reset to 0 or capacity lost")
	}
	if len(state.results) != 0 || cap(state.results) < 1 {
		t.Errorf("results heap length not reset to 0 or capacity lost")
	}
	if len(state.collected) != 0 || cap(state.collected) < 1 {
		t.Errorf("collected slice length not reset to 0 or capacity lost")
	}

	// 3. Тестируем автоматическое расширение (grow) в acquire при большом nodeSlots
	largeNodeSlots := 20000 // needed = (20000+63)/64 = 313 uint64
	state.acquire(largeNodeSlots)

	// Проверяем, что длина visited увеличилась до необходимой
	expectedNeeded := (largeNodeSlots + 63) / 64
	if len(state.visited) != expectedNeeded {
		t.Errorf("expected visited slice length %d, got %d", expectedNeeded, len(state.visited))
	}

	// Проверяем, что все биты в новом расширенном массиве равны 0
	for i := uint64(0); i < uint64(largeNodeSlots); i += 100 {
		if state.isVisited(i) {
			t.Errorf("node ID=%d is visited in freshly allocated visited slice, expected false", i)
		}
	}
}
