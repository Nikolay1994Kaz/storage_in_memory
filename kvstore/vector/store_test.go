package vector

import (
	"math"
	"os"
	"testing"

	"kvstore/kvstore/internal/compute"
)

func TestVectorStore_AddAndSearch(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance)

	vs.Add(0, "cat", []float32{1, 0, 0})
	vs.Add(0, "dog", []float32{0.9, 0.1, 0})
	vs.Add(0, "car", []float32{0, 0, 1})

	results, err := vs.Search(0, []float32{1, 0, 0}, 2)
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
	err := vs.Add(0, "a", []float32{1, 2, 3})
	if err != nil {
		t.Fatalf("first add should succeed: %v", err)
	}

	// Второй вектор — dim=2 → должна быть ошибка
	err = vs.Add(0, "b", []float32{1, 2})
	if err == nil {
		t.Fatal("expected dimension mismatch error, got nil")
	}

	// Search с неправильной размерностью
	_, err = vs.Search(0, []float32{1, 2}, 1)
	if err == nil {
		t.Fatal("expected search dimension mismatch error, got nil")
	}
}

func TestVectorStore_Upsert(t *testing.T) {
	vs := NewVectorStore(EuclideanDistance)

	vs.Add(0, "cat", []float32{1, 0, 0})
	vs.Add(0, "cat", []float32{0, 0, 1})
	nodeCount, _, _ := vs.Info()
	if nodeCount != 1 {
		t.Fatalf("expected 1 node after upsert, got %d", nodeCount)
	}
	results, err := vs.Search(0, []float32{0, 0, 0.9}, 1)
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

	vs.Add(0, "a", []float32{1, 0})
	vs.Add(0, "b", []float32{0, 1})
	vs.Add(0, "c", []float32{1, 1})

	// Удаляем "b"
	ok := vs.Delete(0, "b")
	if !ok {
		t.Fatal("Delete returned false for existing key")
	}

	// Повторное удаление — false
	ok = vs.Delete(0, "b")
	if ok {
		t.Fatal("Delete returned true for already-deleted key")
	}

	// Удаление несуществующего — false
	ok = vs.Delete(0, "zzz")
	if ok {
		t.Fatal("Delete returned true for non-existent key")
	}

	// Проверяем количество
	nodeCount, _, _ := vs.Info()
	if nodeCount != 2 {
		t.Fatalf("expected 2 nodes after delete, got %d", nodeCount)
	}

	// Поиск не должен возвращать "b"
	results, _ := vs.Search(0, []float32{0, 1}, 3)
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
	vs.Add(0, "x", []float32{1, 2, 3, 4, 5})
	count, dim, _ = vs.Info()
	if count != 1 {
		t.Errorf("expected count=1, got %d", count)
	}
	if dim != 5 {
		t.Errorf("expected dim=5, got %d", dim)
	}

	// После удаления
	vs.Delete(0, "x")
	count, _, _ = vs.Info()
	if count != 0 {
		t.Errorf("expected count=0 after delete, got %d", count)
	}
}

func TestVectorStore_RustWasmEngineCorrectness(t *testing.T) {
	// 1. Создаем хранилище в гибридном Go режиме
	vs := NewVectorStore(EuclideanDistance)

	// Добавляем тестовые векторы в Go
	vectors := map[string][]float32{
		"vec1": {1.0, 0.0, 0.0, 0.5},
		"vec2": {0.0, 1.0, 0.0, 0.5},
		"vec3": {0.0, 0.0, 0.9, 0.5},
		"vec4": {0.5, 0.5, 0.5, 0.5},
		"vec5": {1.0, 1.0, 1.0, 1.0},
	}

	for k, v := range vectors {
		if err := vs.Add(0, k, v); err != nil {
			t.Fatalf("failed to add vector %s: %v", k, err)
		}
	}

	// 2. Делаем тестовый поиск в Go
	query := []float32{0.9, 0.1, 0.1, 0.5}
	resultsGo, err := vs.Search(0, query, 3)
	if err != nil {
		t.Fatalf("Go search failed: %v", err)
	}

	// 3. Инициализируем WASM движок и прогреваем его
	wasmBytes, err := os.ReadFile("../../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm")
	if err != nil {
		t.Skip("skipping WASM test because compiled vector_math_wasm.wasm not found; compile it first")
	}

	e := compute.NewEngine()
	defer e.Close()

	wle := compute.NewWorkerLocalEngine(e)
	defer wle.Close()

	if err := wle.WarmUpFromBytes("vector_math", wasmBytes, compute.TierPinned); err != nil {
		t.Fatalf("failed to warm up WASM module: %v", err)
	}

	vs.SetWorkerLocalEngine(wle)

	if err := vs.SetEngine(1); err != nil {
		t.Fatalf("failed to switch to WASM engine: %v", err)
	}

	if vs.Engine() != 1 {
		t.Fatal("engine should be 1 (WASM)")
	}

	// Проверяем Info()
	count, dim, maxLvl := vs.Info()
	if count != 5 {
		t.Errorf("WASM info: expected count 5, got %d", count)
	}
	if dim != 4 {
		t.Errorf("WASM info: expected dim 4, got %d", dim)
	}
	t.Logf("WASM active. Count: %d, Dim: %d, MaxLevel: %d", count, dim, maxLvl)

	// 4. Делаем поиск с использованием WASM
	resultsWasm, err := vs.Search(0, query, 3)
	if err != nil {
		t.Fatalf("WASM search failed: %v", err)
	}

	// 5. Проверяем идентичность результатов Go vs WASM
	if len(resultsGo) != len(resultsWasm) {
		t.Fatalf("search result len mismatch: Go=%d, WASM=%d", len(resultsGo), len(resultsWasm))
	}

	for i := range resultsGo {
		rGo := resultsGo[i]
		rWasm := resultsWasm[i]
		if rGo.Key != rWasm.Key {
			t.Errorf("result %d key mismatch: Go=%q, WASM=%q", i, rGo.Key, rWasm.Key)
		}
		// Проверяем расстояние с учетом погрешности float32
		diff := math.Abs(float64(rGo.Distance - rWasm.Distance))
		if diff > 1e-4 {
			t.Errorf("result %d distance mismatch: Go=%f, WASM=%f (diff=%f)", i, rGo.Distance, rWasm.Distance, diff)
		}
	}

	// 6. Проверяем вставку нового вектора напрямую при включенном WASM
	newVec := []float32{0.8, 0.2, 0.0, 0.4}
	if err := vs.Add(0, "vec6", newVec); err != nil {
		t.Fatalf("failed to add vector in WASM mode: %v", err)
	}

	// Проверяем, что count увеличился
	count, _, _ = vs.Info()
	if count != 6 {
		t.Errorf("expected 6 vectors, got %d", count)
	}

	// 7. Проверяем удаление вектора в WASM режиме
	deleted := vs.Delete(0, "vec3")
	if !deleted {
		t.Error("expected delete to return true")
	}

	count, _, _ = vs.Info()
	if count != 5 {
		t.Errorf("expected 5 vectors after delete, got %d", count)
	}

	// 8. Переключаемся обратно на Go движок (откат) и проверяем, что все работает
	if err := vs.SetEngine(0); err != nil {
		t.Fatalf("failed to switch back to Go engine: %v", err)
	}

	if vs.Engine() != 0 {
		t.Fatal("engine should be 0 (Go)")
	}

	resultsGoBack, err := vs.Search(0, query, 3)
	if err != nil {
		t.Fatalf("Go search after rollback failed: %v", err)
	}

	// Проверяем, что удаленный в WASM режиме vec3 отсутствует и в Go, а добавленный vec6 присутствует!
	for _, r := range resultsGoBack {
		if r.Key == "vec3" {
			t.Error("vec3 was deleted in WASM mode, but is still present in Go mode (sync failed!)")
		}
	}

	foundVec6 := false
	for _, r := range resultsGoBack {
		if r.Key == "vec6" {
			foundVec6 = true
			break
		}
	}
	if !foundVec6 {
		t.Error("vec6 was added in WASM mode, but is missing in Go mode after rollback (sync failed!)")
	}

	t.Log("PASS: Rust/WASM distance calculator verified successfully, 100% correct parity with Go engine!")
}
