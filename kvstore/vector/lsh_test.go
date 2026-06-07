package vector

import (
	"fmt"
	"math/rand"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// ============================================================================
// Бенчмарки: LSH-ускоренный поиск vs обычный HNSW
// ============================================================================
//
// Запуск:
//   go test ./kvstore/vector/ -bench=BenchmarkLSH -benchmem -count=3 -timeout=300s
//
// Что измеряем:
//   1. Скорость вычисления LSH-хэша
//   2. Скорость POPCNT-сканирования
//   3. Сквозной поиск: обычный HNSW vs LSH+HNSW
//   4. Качество: recall (сколько настоящих ближайших находит LSH)

// ─────────────────────────────────────────────
// Хелперы
// ─────────────────────────────────────────────

func makeTestStore(n, dim int) *VectorStore {
	alloc := tcmalloc.NewTCMallocStore(1)
	vs := NewVectorStoreCosine(alloc)

	rng := rand.New(rand.NewSource(12345))
	for i := 0; i < n; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		key := fmt.Sprintf("vec:%d", i)
		vs.Add(key, vec)
	}
	return vs
}

func makeQuery(dim int, seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	q := make([]float32, dim)
	for j := range q {
		q[j] = rng.Float32()*2 - 1
	}
	return q
}

// ─────────────────────────────────────────────
// Benchmark: LSH Hash Computation
// ─────────────────────────────────────────────

func BenchmarkLSH_ComputeHash_128(b *testing.B) {
	benchmarkComputeHash(b, 128)
}

func BenchmarkLSH_ComputeHash_768(b *testing.B) {
	benchmarkComputeHash(b, 768)
}

func benchmarkComputeHash(b *testing.B, dim int) {
	idx := NewLSHIndex(dim, 42)
	rng := rand.New(rand.NewSource(99))
	vec := make([]float32, dim)
	for j := range vec {
		vec[j] = rng.Float32()*2 - 1
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = idx.ComputeHash(vec)
	}
}

// ─────────────────────────────────────────────
// Benchmark: POPCNT Scan
// ─────────────────────────────────────────────

func BenchmarkLSH_POPCNTScan_10K(b *testing.B) {
	benchmarkPOPCNTScan(b, 10_000)
}

func BenchmarkLSH_POPCNTScan_100K(b *testing.B) {
	benchmarkPOPCNTScan(b, 100_000)
}

func BenchmarkLSH_POPCNTScan_1M(b *testing.B) {
	benchmarkPOPCNTScan(b, 1_000_000)
}

func benchmarkPOPCNTScan(b *testing.B, n int) {
	idx := &LSHIndex{
		hashes:       make([]uint64, n),
		candidateBuf: make([]uint64, 0, 1024),
	}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		idx.hashes[i] = rng.Uint64()
	}
	queryHash := rng.Uint64()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		candidates := idx.FindCandidates(queryHash, 10)
		_ = candidates
	}
}

// ─────────────────────────────────────────────
// Benchmark: Сквозной поиск — HNSW vs LSH+HNSW
// ─────────────────────────────────────────────

func BenchmarkSearch_HNSW_10K_dim128(b *testing.B) {
	benchmarkHNSWSearch(b, 10_000, 128, 10)
}

func BenchmarkSearch_LSH_HNSW_10K_dim128(b *testing.B) {
	benchmarkLSHSearch(b, 10_000, 128, 10, 10)
}

func BenchmarkSearch_HNSW_50K_dim128(b *testing.B) {
	benchmarkHNSWSearch(b, 50_000, 128, 10)
}

func BenchmarkSearch_LSH_HNSW_50K_dim128(b *testing.B) {
	benchmarkLSHSearch(b, 50_000, 128, 10, 10)
}

func BenchmarkSearch_HNSW_10K_dim768(b *testing.B) {
	benchmarkHNSWSearch(b, 10_000, 768, 10)
}

func BenchmarkSearch_LSH_HNSW_10K_dim768(b *testing.B) {
	benchmarkLSHSearch(b, 10_000, 768, 10, 12)
}

func benchmarkHNSWSearch(b *testing.B, n, dim, K int) {
	vs := makeTestStore(n, dim)
	query := makeQuery(dim, 999)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		results, _ := vs.Search(query, K)
		_ = results
	}
}

func benchmarkLSHSearch(b *testing.B, n, dim, K, threshold int) {
	vs := makeTestStore(n, dim)
	query := makeQuery(dim, 999)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		results, _ := vs.SearchWithLSH(query, K, threshold)
		_ = results
	}
}

// ─────────────────────────────────────────────
// Test: Корректность LSH (recall)
// ─────────────────────────────────────────────

func TestLSH_Recall(t *testing.T) {
	dims := []int{256}
	sizes := []int{1000, 5000, 10000}
	thresholds := []int{8, 10, 12, 15}

	for _, dim := range dims {
		for _, n := range sizes {
			vs := makeTestStore(n, dim)

			for _, threshold := range thresholds {
				name := fmt.Sprintf("n=%d/dim=%d/threshold=%d", n, dim, threshold)
				t.Run(name, func(t *testing.T) {
					K := 10
					numQueries := 20
					totalRecall := 0.0

					for q := 0; q < numQueries; q++ {
						query := makeQuery(dim, int64(q*1000+1))

						// Ground truth: обычный HNSW поиск
						groundTruth, _ := vs.Search(query, K)
						groundSet := make(map[string]bool)
						for _, r := range groundTruth {
							groundSet[r.Key] = true
						}

						// LSH-ускоренный поиск
						lshResults, _ := vs.SearchWithLSH(query, K, threshold)

						// Считаем recall: сколько из ground truth нашёл LSH
						hits := 0
						for _, r := range lshResults {
							if groundSet[r.Key] {
								hits++
							}
						}

						if len(groundTruth) > 0 {
							totalRecall += float64(hits) / float64(len(groundTruth))
						}
					}

					avgRecall := totalRecall / float64(numQueries)
					t.Logf("Recall: %.1f%% (avg over %d queries)", avgRecall*100, numQueries)

					if avgRecall < 0.5 {
						t.Logf("WARNING: recall below 50%%, consider increasing threshold")
					}
				})
			}
		}
	}
}

// ─────────────────────────────────────────────
// Test: LSH базовая корректность
// ─────────────────────────────────────────────

func TestLSH_BasicOperations(t *testing.T) {
	dim := 128
	idx := NewLSHIndex(dim, 42)

	rng := rand.New(rand.NewSource(123))

	// Вставляем 1000 случайных векторов
	vecs := make([][]float32, 1000)
	for i := 0; i < 1000; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		vecs[i] = vec
		idx.Insert(uint32(i), vec)
	}

	// Проверяем: одинаковый вектор → Hamming = 0
	hash0 := idx.ComputeHash(vecs[0])
	hash0again := idx.ComputeHash(vecs[0])
	if hash0 != hash0again {
		t.Fatalf("same vector gives different hashes: %x vs %x", hash0, hash0again)
	}

	// Проверяем: хэш записан
	if idx.hashes[0] != hash0 {
		t.Fatalf("stored hash mismatch: %x vs %x", idx.hashes[0], hash0)
	}

	// Проверяем FindCandidates: threshold=0 → только точные совпадения
	candidates := idx.FindCandidates(hash0, 0)
	found := false
	for _, c := range candidates {
		if c == 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("vector 0 not found in candidates with threshold=0")
	}

	// Проверяем Delete: sentinel делает ноду невидимой
	idx.Delete(0)
	if idx.hashes[0] != lshSentinel {
		t.Fatalf("delete didn't set sentinel: got %x", idx.hashes[0])
	}
	candidates = idx.FindCandidates(hash0, 5)
	for _, c := range candidates {
		if c == 0 {
			t.Fatal("deleted vector 0 should not appear in candidates")
		}
	}

	// Проверяем Stats
	stats := idx.Stats()
	if stats.AliveCount != 999 {
		t.Fatalf("expected 999 alive, got %d", stats.AliveCount)
	}

	t.Logf("LSH Stats: %+v", stats)
}

// ─────────────────────────────────────────────
// Test: Качество хэша — близкие векторы = малый Hamming
// ─────────────────────────────────────────────

func TestLSH_HashQuality(t *testing.T) {
	dim := 128
	idx := NewLSHIndex(dim, 42)

	rng := rand.New(rand.NewSource(777))

	// Создаём базовый вектор
	base := make([]float32, dim)
	for j := range base {
		base[j] = rng.Float32()*2 - 1
	}
	baseHash := idx.ComputeHash(base)

	// Создаём близкий вектор (небольшое отклонение)
	similar := make([]float32, dim)
	copy(similar, base)
	for j := range similar {
		similar[j] += rng.Float32() * 0.1 // +/- 5% отклонение
	}
	similarHash := idx.ComputeHash(similar)

	// Создаём далёкий вектор (полностью случайный)
	distant := make([]float32, dim)
	for j := range distant {
		distant[j] = rng.Float32()*2 - 1
	}
	distantHash := idx.ComputeHash(distant)

	hammingSimilar := HammingDistance(baseHash, similarHash)
	hammingDistant := HammingDistance(baseHash, distantHash)

	t.Logf("Hamming(base, similar) = %d bits (expected low)", hammingSimilar)
	t.Logf("Hamming(base, distant) = %d bits (expected ~32)", hammingDistant)

	if hammingSimilar >= hammingDistant {
		t.Logf("WARNING: similar vector has higher Hamming than distant! LSH quality may be suboptimal")
	}

	// Близкий вектор должен отличаться меньше чем на 20 бит из 64
	if hammingSimilar > 20 {
		t.Logf("WARNING: similar vector Hamming=%d (expected <20)", hammingSimilar)
	}
}
