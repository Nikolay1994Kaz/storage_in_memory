package vector

import (
	"context"
	"math/rand"
	"os"
	"strings"
	"testing"
)

// TestWasmEngine_InvalidBytes проверяет, что при передаче битых байт движок возвращает ошибку компиляции.
func TestWasmEngine_InvalidBytes(t *testing.T) {
	ctx := context.Background()
	badBytes := []byte{0x00, 0x00, 0x00, 0x00} // Невалидный заголовок WASM

	_, err := NewWasmEngine(ctx, badBytes, 128)
	if err == nil {
		t.Fatal("expected error when instantiating bad WASM bytes, got nil")
	}
}

// TestWasmEngine_MissingExports проверяет, что минимально валидный WASM модуль без нужных экспортов возвращает ошибку валидации.
func TestWasmEngine_MissingExports(t *testing.T) {
	ctx := context.Background()

	// Минимально валидный заголовок WASM модуля (8 байт: magic + version)
	minimalWasm := []byte{
		0x00, 0x61, 0x73, 0x6d, // Magic: "\x00asm"
		0x01, 0x00, 0x00, 0x00, // Version: 1
	}

	_, err := NewWasmEngine(ctx, minimalWasm, 128)
	if err == nil {
		t.Fatal("expected error due to missing exports, got nil")
	}

	expectedErr := "simd_euclidean_distance not exported"
	if !strings.Contains(err.Error(), expectedErr) {
		t.Fatalf("expected error containing %q, got: %v", expectedErr, err)
	}
	t.Logf("Successfully caught expected error: %v", err)
}

// TestWasmEngine_EndToEnd загружает НАСТОЯЩИЙ скомпилированный WASM файл,
// проводит расчеты расстояний с SIMD и сверяет результаты с эталонными Go-функциями.
func TestWasmEngine_EndToEnd(t *testing.T) {
	// Путь к скомпилированному WASM
	wasmPath := "../../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm"

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Skipf("Skipping end-to-end test because compiled WASM was not found at %s: %v", wasmPath, err)
	}

	const dim = 128
	ctx := context.Background()

	// 1. Инициализируем настоящий WASM-движок
	engine, err := NewWasmEngine(ctx, wasmBytes, dim)
	if err != nil {
		t.Fatalf("failed to initialize WASM engine: %v", err)
	}
	defer engine.Close(ctx)

	// 2. Генерируем тестовые векторы
	rng := rand.New(rand.NewSource(42))
	vecA := make([]float32, dim)
	vecB := make([]float32, dim)
	for i := 0; i < dim; i++ {
		vecA[i] = rng.Float32() * 100.0
		vecB[i] = rng.Float32() * 100.0
	}

	// 3. Вычисляем Евклидово расстояние
	goDistE := EuclideanDistance(vecA, vecB)
	wasmDistE := engine.EuclideanDistanceFunc()(vecA, vecB)

	// Проверяем относительную погрешность (из-за разного порядка суммирования в SIMD)
	diffE := abs(goDistE - wasmDistE)
	relativeDiffE := diffE / goDistE
	if relativeDiffE > 1e-5 {
		t.Errorf("Euclidean distance mismatch: Go=%f, WASM=%f (relDiff=%e)", goDistE, wasmDistE, relativeDiffE)
	} else {
		t.Logf("PASS: Euclidean Distances match. Go=%f, WASM=%f (relDiff=%e)", goDistE, wasmDistE, relativeDiffE)
	}

	// 4. Вычисляем Косинусное расстояние
	goDistC := CosineDistance(vecA, vecB)
	wasmDistC := engine.CosineDistanceFunc()(vecA, vecB)

	diffC := abs(goDistC - wasmDistC)
	if diffC > 1e-4 {
		t.Errorf("Cosine distance mismatch: Go=%f, WASM=%f (diff=%f)", goDistC, wasmDistC, diffC)
	} else {
		t.Logf("PASS: Cosine Distances match. Go=%f, WASM=%f (diff=%e)", goDistC, wasmDistC, diffC)
	}
}

func abs(val float32) float32 {
	if val < 0 {
		return -val
	}
	return val
}

// ─────────────────────────────────────────────────────────────────────────────
// Бенчмарки производительности и пропускной способности (RPS / Operations/sec)
// ─────────────────────────────────────────────────────────────────────────────

const benchDim = 128

func BenchmarkEuclidean_PureGo(b *testing.B) {
	rng := rand.New(rand.NewSource(42))
	vecA := make([]float32, benchDim)
	vecB := make([]float32, benchDim)
	for i := 0; i < benchDim; i++ {
		vecA[i] = rng.Float32()
		vecB[i] = rng.Float32()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EuclideanDistance(vecA, vecB)
	}
}

func BenchmarkEuclidean_WasmSIMD(b *testing.B) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile("../../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm")
	if err != nil {
		b.Fatalf("failed to read WASM: %v", err)
	}
	engine, err := NewWasmEngine(ctx, wasmBytes, benchDim)
	if err != nil {
		b.Fatalf("failed to create WASM engine: %v", err)
	}
	defer engine.Close(ctx)

	rng := rand.New(rand.NewSource(42))
	vecA := make([]float32, benchDim)
	vecB := make([]float32, benchDim)
	for i := 0; i < benchDim; i++ {
		vecA[i] = rng.Float32()
		vecB[i] = rng.Float32()
	}

	fn := engine.EuclideanDistanceFunc()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fn(vecA, vecB)
	}
}

func BenchmarkEuclidean_WasmSIMD_Parallel(b *testing.B) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile("../../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm")
	if err != nil {
		b.Fatalf("failed to read WASM: %v", err)
	}
	engine, err := NewWasmEngine(ctx, wasmBytes, benchDim)
	if err != nil {
		b.Fatalf("failed to create WASM engine: %v", err)
	}
	defer engine.Close(ctx)

	rng := rand.New(rand.NewSource(42))
	vecA := make([]float32, benchDim)
	vecB := make([]float32, benchDim)
	for i := 0; i < benchDim; i++ {
		vecA[i] = rng.Float32()
		vecB[i] = rng.Float32()
	}

	fn := engine.EuclideanDistanceFunc()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = fn(vecA, vecB)
		}
	})
}

func BenchmarkCosine_PureGo(b *testing.B) {
	rng := rand.New(rand.NewSource(42))
	vecA := make([]float32, benchDim)
	vecB := make([]float32, benchDim)
	for i := 0; i < benchDim; i++ {
		vecA[i] = rng.Float32()
		vecB[i] = rng.Float32()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CosineDistance(vecA, vecB)
	}
}

func BenchmarkCosine_WasmSIMD(b *testing.B) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile("../../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm")
	if err != nil {
		b.Fatalf("failed to read WASM: %v", err)
	}
	engine, err := NewWasmEngine(ctx, wasmBytes, benchDim)
	if err != nil {
		b.Fatalf("failed to create WASM engine: %v", err)
	}
	defer engine.Close(ctx)

	rng := rand.New(rand.NewSource(42))
	vecA := make([]float32, benchDim)
	vecB := make([]float32, benchDim)
	for i := 0; i < benchDim; i++ {
		vecA[i] = rng.Float32()
		vecB[i] = rng.Float32()
	}

	fn := engine.CosineDistanceFunc()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fn(vecA, vecB)
	}
}

func BenchmarkCosine_WasmSIMD_Parallel(b *testing.B) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile("../../rust_src/target/wasm32-unknown-unknown/release/vector_math_wasm.wasm")
	if err != nil {
		b.Fatalf("failed to read WASM: %v", err)
	}
	engine, err := NewWasmEngine(ctx, wasmBytes, benchDim)
	if err != nil {
		b.Fatalf("failed to create WASM engine: %v", err)
	}
	defer engine.Close(ctx)

	rng := rand.New(rand.NewSource(42))
	vecA := make([]float32, benchDim)
	vecB := make([]float32, benchDim)
	for i := 0; i < benchDim; i++ {
		vecA[i] = rng.Float32()
		vecB[i] = rng.Float32()
	}

	fn := engine.CosineDistanceFunc()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = fn(vecA, vecB)
		}
	})
}
