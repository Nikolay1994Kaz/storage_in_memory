package vector

import (
	"testing"
)

func TestVectorStore_AddAndSearch(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance)

	vs.Add("cat", []float32{1, 0, 0})
	vs.Add("dog", []float32{0.9, 0.1, 0})
	vs.Add("car", []float32{0, 0, 1})

	results, err := vs.Search([]float32{1, 0, 0}, 2)
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
	vs := NewVectorStore(EuclideanDistance)

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
	_, err = vs.Search([]float32{1, 2}, 1)
	if err == nil {
		t.Fatal("expected search dimension mismatch error, got nil")
	}
}

func TestVectorStore_Upsert(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance)

	vs.Add("cat", []float32{1, 0, 0})
	vs.Add("cat", []float32{0, 0, 1})
	nodeCount, _, _ := vs.Info()
	if nodeCount != 1 {
		t.Fatalf("expected 1 node after upsert, got %d", nodeCount)
	}
	results, err := vs.Search([]float32{0, 0, 0.9}, 1)
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
	vs := NewVectorStore(EuclideanDistance)

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
	results, _ := vs.Search([]float32{0, 1}, 3)
	for _, r := range results {
		if r.Key == "b" {
			t.Error("deleted key 'b' found in search results")
		}
	}
}

func TestVectorStore_Info(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance)

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
	vs := NewVectorStoreCosine()

	// A и B — одно направление, но РАЗНЫЕ длины
	// C — перпендикулярно
	vs.Add("right_short", []float32{1, 0})
	vs.Add("right_long", []float32{100, 0})
	vs.Add("up", []float32{0, 1})

	// Запрос «вправо» — A и B должны быть ближайшими
	results, err := vs.Search([]float32{5, 0}, 2)
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
	vsCosine := NewVectorStore(CosineDistance)
	// Вариант 2: pre-normalized DotProduct
	vsDot := NewVectorStoreCosine()

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

	resCosine, _ := vsCosine.Search(query, 3)
	resDot, _ := vsDot.Search(query, 3)

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
