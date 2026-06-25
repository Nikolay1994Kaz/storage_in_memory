package vector

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// =============================================================================
// SCALE BENCHMARK: Float32 vs SQ8 vs PQ на dim=768
//
// Цель: доказать математически и через замер QPS, что при dim=768:
// 1. Float32 убивает L3 кэш (память раздувается).
// 2. SQ8 (наш текущий метод) тоже слишком толстый для L3.
// 3. PQ решает проблему (сжимает вектор до 32 байт), ускоряя вычисления.
// =============================================================================

func TestBench_Dim768_Float32_vs_SQ8_vs_PQ(t *testing.T) {
	if testing.Short() {
		t.Skip("Dim 768 scale benchmark is slow")
	}

	const n = 20000    // 20k векторов для быстрого теста
	const dim = 768    // Целевая размерность (BERT, sentence-transformers)
	const testSecs = 3 * time.Second

	// 1. Генерируем реальные по структуре векторы (нормальное распределение)
	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = make([]float32, dim)
		for d := range vecs[i] {
			vecs[i][d] = float32(rng.NormFloat64() * 10) // среднее 0, отклонение 10
		}
	}

	queries := make([][]float32, 100)
	for i := range queries {
		queries[i] = make([]float32, dim)
		for d := range queries[i] {
			queries[i][d] = float32(rng.NormFloat64() * 10)
		}
	}

	// 2. Подготавливаем структуры данных
	// SQ8: симулируем деквантование (как делает наш AVX2)
	sqMin := make([]float32, dim)
	sqScale := make([]float32, dim)
	for d := 0; d < dim; d++ {
		sqMin[d] = -30
		sqScale[d] = 60.0 / 255.0
	}
	sq8Codes := make([][]uint8, n)
	for i, v := range vecs {
		sq8Codes[i] = make([]uint8, dim)
		for d := 0; d < dim; d++ {
			q := (v[d] - sqMin[d]) / sqScale[d]
			if q < 0 {
				q = 0
			} else if q > 255 {
				q = 255
			}
			sq8Codes[i][d] = uint8(q)
		}
	}

	// PQ: M=32 (32 байта на вектор, 24 под-вектора по 32 dim)
	pq := trainPQ(vecs, 32)

	// 3. Считаем память на 1 вектор
	memFloat32 := dim * 4     // 3072 байта
	memSQ8 := dim             // 768 байт
	memPQ := pq.M             // 32 байта

	// 4. Запускаем честные вычисления дистанций (ядро поиска HNSW)
	// Float32 (AVX2 в production)
	qpsFloat := measureQPSBatch(func(q []float32) {
		var minDist float32 = 1e9
		for _, v := range vecs {
			d := EuclideanDistance(q, v) // здесь AVX2
			if d < minDist {
				minDist = d
			}
		}
	}, queries, 12, testSecs)

	// SQ8 (Деквантование + AVX2 в production, Pure-Go для теста)
	qpsSQ8 := measureQPSBatch(func(q []float32) {
		var minDist float32 = 1e9
		rankBuf := make([]float32, dim)
		for _, codes := range sq8Codes {
			for d := 0; d < dim; d++ {
				rankBuf[d] = sqMin[d] + float32(codes[d])*sqScale[d]
			}
			d := EuclideanDistance(q, rankBuf)
			if d < minDist {
				minDist = d
			}
		}
	}, queries, 12, testSecs)

	// PQ (ADC table lookup)
	qpsPQ := measureQPSBatch(func(q []float32) {
		var minDist float32 = 1e9
		for _, codes := range pq.codes {
			d := pq.adcDistance(q, codes)
			if d < minDist {
				minDist = d
			}
		}
	}, queries, 12, testSecs)

	// 5. Вывод результатов
	fmt.Printf("\n=== DIM 768 BENCHMARK (n=%d) ===\n", n)
	fmt.Printf("│ Method  │ Bytes/Vec │ Mem for 500k  │ L3 (12MB)? │ QPS (Distance Calc) │\n")
	fmt.Printf("│---------|-----------|---------------|------------|---------------------│\n")
	fmt.Printf("│ Float32 │ %4d B   │ %10.1f MB │ %s  │ %19.0f │\n", memFloat32, float64(500000*memFloat32)/1e6, l3StatusStr(float64(500000*memFloat32)/1e6/12), qpsFloat)
	fmt.Printf("│ SQ8     │ %4d B   │ %10.1f MB │ %s  │ %19.0f │\n", memSQ8, float64(500000*memSQ8)/1e6, l3StatusStr(float64(500000*memSQ8)/1e6/12), qpsSQ8)
	fmt.Printf("│ PQ      │ %4d B   │ %10.1f MB  │ %s  │ %19.0f │\n", memPQ, float64(500000*memPQ)/1e6, l3StatusStr(float64(500000*memPQ)/1e6/12), qpsPQ)
}
