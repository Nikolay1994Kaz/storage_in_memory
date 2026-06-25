package vector

import (
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

func TestVectorStore_AddAndSearch(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	vs.Add("cat", []float32{1, 0, 0})
	vs.Add("dog", []float32{0.9, 0.1, 0})
	vs.Add("car", []float32{0, 0, 1})

	results, err := vs.Search([]float32{1, 0, 0}, 2, nil)
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got: %d", len(results))
	}
	if results[0].Key != "cat" {
		t.Errorf("expected first result 'cat', got '%s'", results[0].Key)
	}
	// Второй — "dog" (ближе к cat, чем car)
	if results[1].Key != "dog" {
		t.Errorf("expected second result 'dog', got '%s'", results[1].Key)
	}

	if results[0].Distance != 0 {
		t.Errorf("exptected distance 0 for exact match, got %f", results[0].Distance)
	}
}

func TestVectorStore_DimensionMismatch(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	// Первый вектор — dim=3
	err := vs.Add("a", []float32{1, 2, 3})
	if err != nil {
		t.Fatalf("first add should succeed: %v", err)
	}

	// Второй вектор — dim=2 → должна быть ошибка
	err = vs.Add("b", []float32{1, 2})
	if err == nil {
		t.Fatal("expected dimension mismatch error, got nil")
	}

	// Search с неправильной размерностью
	_, err = vs.Search([]float32{1, 2}, 1, nil)
	if err == nil {
		t.Fatal("expected search dimension mismatch error, got nil")
	}
}

func TestVectorStore_Upsert(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	vs.Add("cat", []float32{1, 0, 0})
	vs.Add("cat", []float32{0, 0, 1})
	nodeCount, _, _ := vs.Info()
	if nodeCount != 1 {
		t.Fatalf("expected 1 node after upsert, got %d", nodeCount)
	}
	results, err := vs.Search([]float32{0, 0, 0.9}, 1, nil)
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Key != "cat" {
		t.Errorf("expected 'cat', got '%s'", results[0].Key)
	}
	if results[0].Distance > 0.1 {
		t.Errorf("cat should be near (0,0,1), but distance = %f (old vector leaked?)",
			results[0].Distance)
	}
}

func TestVectorStore_Delete(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	vs.Add("a", []float32{1, 0})
	vs.Add("b", []float32{0, 1})
	vs.Add("c", []float32{1, 1})

	// Удаляем "b"
	ok := vs.Delete("b")
	if !ok {
		t.Fatal("Delete returned false for existing key")
	}

	// Повторное удаление — false
	ok = vs.Delete("b")
	if ok {
		t.Fatal("Delete returned true for already-deleted key")
	}

	// Удаление несуществующего — false
	ok = vs.Delete("zzz")
	if ok {
		t.Fatal("Delete returned true for non-existent key")
	}

	// Проверяем количество
	nodeCount, _, _ := vs.Info()
	if nodeCount != 2 {
		t.Fatalf("expected 2 nodes after delete, got %d", nodeCount)
	}

	// Поиск не должен возвращать "b"
	results, _ := vs.Search([]float32{0, 1}, 3, nil)
	for _, r := range results {
		if r.Key == "b" {
			t.Error("deleted key 'b' found in search results")
		}
	}
}

func TestVectorStore_Info(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	// Пустой стор
	count, dim, _ := vs.Info()
	if count != 0 || dim != 0 {
		t.Errorf("empty store: expected count=0 dim=0, got count=%d dim=%d", count, dim)
	}

	// После добавления
	vs.Add("x", []float32{1, 2, 3, 4, 5})
	count, dim, _ = vs.Info()
	if count != 1 {
		t.Errorf("expected count=1, got %d", count)
	}
	if dim != 5 {
		t.Errorf("expected dim=5, got %d", dim)
	}

	// После удаления
	vs.Delete("x")
	count, _, _ = vs.Info()
	if count != 0 {
		t.Errorf("expected count=0 after delete, got %d", count)
	}
}

// ─────────────────────────────────────────────────
// Тест: Pre-normalization Cosine
// ─────────────────────────────────────────────────
//
// Проверяем что NewVectorStoreCosine:
// 1. Автоматически нормализует вектора при вставке
// 2. Автоматически нормализует запрос при поиске
// 3. Даёт тот же порядок результатов, что и CosineDistance
// 4. Игнорирует длину вектора (только направление важно)

func TestVectorStore_CosinePreNormalization(t *testing.T) {
	vs := NewVectorStoreCosine(tcmalloc.NewTCMallocStore(1))

	// A и B — одно направление, но РАЗНЫЕ длины
	// C — перпендикулярно
	vs.Add("right_short", []float32{1, 0})
	vs.Add("right_long", []float32{100, 0})
	vs.Add("up", []float32{0, 1})

	// Запрос «вправо» — A и B должны быть ближайшими
	results, err := vs.Search([]float32{5, 0}, 2, nil)
	if err != nil {
		t.Fatalf("search error: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Оба результата — «вправо». «up» не должен быть в top-2.
	for _, r := range results {
		if r.Key == "up" {
			t.Errorf("perpendicular vector 'up' should not be in top-2 cosine results")
		}
	}

	// Distance для одинакового направления должен быть ≈ 0
	for _, r := range results {
		if r.Distance > 0.01 {
			t.Errorf("same direction: expected distance ≈ 0, got %f for key=%s",
				r.Distance, r.Key)
		}
	}

	t.Logf("Pre-normalization cosine results: %+v", results)
}

// ─────────────────────────────────────────────────
// Тест: Pre-normalization vs CosineDistance — одинаковые результаты
// ─────────────────────────────────────────────────

func TestVectorStore_CosineVsDotProduct(t *testing.T) {
	// Вариант 1: «классический» CosineDistance
	vsCosine := NewVectorStore(CosineDistance, tcmalloc.NewTCMallocStore(1))
	// Вариант 2: pre-normalized DotProduct
	vsDot := NewVectorStoreCosine(tcmalloc.NewTCMallocStore(1))

	vecs := []struct {
		key string
		vec []float32
	}{
		{"a", []float32{3, 4, 0}},
		{"b", []float32{1, 0, 0}},
		{"c", []float32{0, 0, 7}},
		{"d", []float32{1, 1, 1}},
		{"e", []float32{-1, 2, 3}},
	}

	for _, v := range vecs {
		vsCosine.Add(v.key, v.vec)
		vsDot.Add(v.key, v.vec)
	}

	query := []float32{2, 3, 0}

	resCosine, _ := vsCosine.Search(query, 3, nil)
	resDot, _ := vsDot.Search(query, 3, nil)

	if len(resCosine) != len(resDot) {
		t.Fatalf("result count mismatch: cosine=%d, dot=%d", len(resCosine), len(resDot))
	}

	// Порядок ключей должен совпадать
	for i := range resCosine {
		if resCosine[i].Key != resDot[i].Key {
			t.Errorf("rank %d: cosine=%s, dot=%s", i, resCosine[i].Key, resDot[i].Key)
		}
	}

	t.Logf("Cosine results: %+v", resCosine)
	t.Logf("DotProduct results: %+v", resDot)
}

// ─────────────────────────────────────────────────
// Тесты: SearchFiltered (Single-Stage Filtering)
// ─────────────────────────────────────────────────

func TestVectorStore_SearchFiltered_Basic(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	// Добавляем 6 векторов: 3 "животных" и 3 "транспорта"
	// Животные — кластер вокруг (1,0,0)
	vs.Add("animal:cat", []float32{1, 0.1, 0})
	vs.Add("animal:dog", []float32{0.9, 0.2, 0})
	vs.Add("animal:bird", []float32{0.8, 0.15, 0.05})

	// Транспорт — кластер вокруг (0,0,1)
	vs.Add("vehicle:car", []float32{0, 0.1, 1})
	vs.Add("vehicle:bus", []float32{0.1, 0, 0.9})
	vs.Add("vehicle:bike", []float32{0.05, 0.05, 0.85})

	// Запрос ближе к животным: (0.95, 0.1, 0)
	query := []float32{0.95, 0.1, 0}

	// Без фильтра — top-3 должны быть животные
	allResults, err := vs.Search(query, 3, nil)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(allResults) != 3 {
		t.Fatalf("expected 3 results, got %d", len(allResults))
	}

	// С фильтром PREFIX "vehicle:" — должны вернуться только транспорт
	filteredResults, err := vs.SearchFiltered(query, 3, func(key string) bool {
		return len(key) > 8 && key[:8] == "vehicle:"
	}, nil)
	if err != nil {
		t.Fatalf("SearchFiltered error: %v", err)
	}

	// Все результаты должны быть vehicle:*
	for _, r := range filteredResults {
		if len(r.Key) < 8 || r.Key[:8] != "vehicle:" {
			t.Errorf("expected vehicle key, got '%s'", r.Key)
		}
	}

	// Должно быть 3 результата (все vehicle)
	if len(filteredResults) != 3 {
		t.Fatalf("expected 3 filtered results, got %d", len(filteredResults))
	}

	t.Logf("Unfiltered: %+v", allResults)
	t.Logf("Filtered (vehicle only): %+v", filteredResults)
}

func TestVectorStore_SearchFiltered_NoMatch(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	vs.Add("a", []float32{1, 0})
	vs.Add("b", []float32{0, 1})
	vs.Add("c", []float32{1, 1})

	// Фильтр не пропускает никого
	results, err := vs.SearchFiltered([]float32{1, 0}, 3, func(key string) bool {
		return false
	}, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results with reject-all filter, got %d", len(results))
	}
}

func TestVectorStore_SearchFiltered_AllMatch(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	vs.Add("a", []float32{1, 0, 0})
	vs.Add("b", []float32{0.9, 0.1, 0})
	vs.Add("c", []float32{0, 0, 1})

	// Фильтр пропускает всех — должен быть идентичен обычному Search
	filtered, err := vs.SearchFiltered([]float32{1, 0, 0}, 2, func(key string) bool {
		return true
	}, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	normal, _ := vs.Search([]float32{1, 0, 0}, 2, nil)

	if len(filtered) != len(normal) {
		t.Fatalf("pass-all filter: expected %d results, got %d", len(normal), len(filtered))
	}

	// Порядок должен совпадать
	for i := range filtered {
		if filtered[i].Key != normal[i].Key {
			t.Errorf("rank %d: filtered=%s, normal=%s", i, filtered[i].Key, normal[i].Key)
		}
	}
}

func TestVectorStore_SearchFiltered_EmptyGraph(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	results, err := vs.SearchFiltered([]float32{1, 0}, 3, func(key string) bool {
		return true
	}, nil)
	if err != nil {
		t.Fatalf("error on empty graph: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil on empty graph, got %v", results)
	}
}

func TestVectorStore_SearchFiltered_HighSelectivity(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

	// 100 векторов, только 1 проходит фильтр
	for i := 0; i < 100; i++ {
		key := "other"
		if i == 42 {
			key = "target"
		}
		vec := []float32{float32(i) / 100.0, float32(100-i) / 100.0}
		vs.Add(key+":"+string(rune('A'+i%26))+string(rune('0'+i/26)), vec)
	}
	// Переименуем: добавим target-вектор с уникальным ключом
	vs.Add("target:special", []float32{0.42, 0.58})

	// Фильтр: только ключи начинающиеся с "target"
	results, err := vs.SearchFiltered([]float32{0.5, 0.5}, 10, func(key string) bool {
		return len(key) >= 6 && key[:6] == "target"
	}, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	// Должен найти target-вектор(ы) несмотря на высокую селективность
	if len(results) == 0 {
		t.Error("expected at least 1 result with high selectivity filter, got 0")
	}

	for _, r := range results {
		if len(r.Key) < 6 || r.Key[:6] != "target" {
			t.Errorf("non-target key in results: %s", r.Key)
		}
	}
	t.Logf("High selectivity (1 of 101): found %d target results", len(results))
}

func TestVectorStore_SearchFiltered_DimensionMismatch(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	vs.Add("a", []float32{1, 2, 3})

	_, err := vs.SearchFiltered([]float32{1, 2}, 1, func(key string) bool {
		return true
	}, nil)
	if err == nil {
		t.Fatal("expected dimension mismatch error, got nil")
	}
}

func TestVectorStore_DeserializeVectorZeroCopy(t *testing.T) {
	original := []float32{1.0, -2.5, 3.14, 0.0, 999.9}
	data := SerializeVector(original)

	// Verify that the zero-copy deserializer returns the exact same float values.
	deserialized := DeserializeVectorZeroCopy(data)
	if len(deserialized) != len(original) {
		t.Fatalf("length mismatch: expected %d, got %d", len(original), len(deserialized))
	}

	for i := range original {
		if deserialized[i] != original[i] {
			t.Errorf("value mismatch at index %d: expected %f, got %f", i, original[i], deserialized[i])
		}
	}

	// Verify empty slice behavior
	if empty := DeserializeVectorZeroCopy(nil); empty != nil {
		t.Errorf("expected nil for nil input, got %v", empty)
	}
}

