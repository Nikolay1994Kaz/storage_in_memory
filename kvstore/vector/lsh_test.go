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
		hashes: make([]uint64, n),
	}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		idx.hashes[i] = rng.Uint64()
	}
	queryHash := rng.Uint64()

	b.ResetTimer()
	b.ReportAllocs()
	var candidates []uint64
	for i := 0; i < b.N; i++ {
		candidates = idx.FindCandidates(queryHash, 10, candidates)
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
		results, _ := vs.Search(query, K, nil)
		_ = results
	}
}

func benchmarkLSHSearch(b *testing.B, n, dim, K, threshold int) {
	vs := makeTestStore(n, dim)
	query := makeQuery(dim, 999)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		results, _ := vs.SearchWithLSH(query, K, threshold, nil)
		_ = results
	}
}

// ─────────────────────────────────────────────
// Test: Корректность LSH (recall)
// ─────────────────────────────────────────────

func TestLSH_Recall(t *testing.T) {
	if testing.Short() {
		t.Skip("тяжёлый recall-репорт (строит HNSW на 10k); запуск без -short")
	}
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
						groundTruth, _ := vs.Search(query, K, nil)
						groundSet := make(map[string]bool)
						for _, r := range groundTruth {
							groundSet[r.Key] = true
						}

						// LSH-ускоренный поиск
						lshResults, _ := vs.SearchWithLSH(query, K, threshold, nil)

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
	candidates := idx.FindCandidates(hash0, 0, nil)
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
	candidates = idx.FindCandidates(hash0, 5, nil)
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

	t.Logf("Hamming(base, similar) = %d bits (замерено 4)", hammingSimilar)
	t.Logf("Hamming(base, distant) = %d bits (замерено 29, теория ~32)", hammingDistant)

	// 🚨Раньше все три проверки ниже были t.Logf("WARNING: …"), то есть тест не мог
	// упасть НИКОГДА. Проверено мутацией: с `ComputeHash → 0` (хеш выродился в
	// константу, LSH сломан полностью) этот тест оставался зелёным, и TestLSH_Recall
	// тоже — 100% recall во всех 12 конфигурациях, потому что SearchWithLSH
	// расширяет threshold до 32 и уходит в полный перебор. LSH при этом живёт в
	// проде (store.go, leveled_store.go, snapshot_binary.go, main.go).
	// Пороги взяты с кратным запасом от замера, а не подогнаны под него.

	// Фундаментальный инвариант SimHash: близкое ближе далёкого. Ловит вырождение
	// хеша в константу (тогда обе дистанции 0 и условие срабатывает).
	if hammingSimilar >= hammingDistant {
		t.Errorf("похожий вектор дальше далёкого: similar=%d >= distant=%d — хеш не разделяет направления",
			hammingSimilar, hammingDistant)
	}
	// Замерено 4 из 64 при отклонении ~0.05 на координату. Порог 20 — пятикратный запас.
	if hammingSimilar > 20 {
		t.Errorf("похожий вектор отличается на %d бит из 64 (порог 20) — хеш слишком чувствителен",
			hammingSimilar)
	}
	// Случайные векторы обязаны расходиться примерно на половину бит. Замерено 29,
	// теория 32. Окно 12..52 ловит и вырождение (0), и «инвертированный» хеш (64).
	if hammingDistant < 12 || hammingDistant > 52 {
		t.Errorf("случайные векторы разошлись на %d бит из 64 (ожидалось 12..52) — хеш вырожден",
			hammingDistant)
	}
}

// TestLSH_CandidatePruning — LSH обязан ОТСЕИВАТЬ, и это единственное, ради чего
// он существует: сузить перебор, не потеряв соседа.
//
// ⭐Почему отдельным тестом, а не порогом внутри TestLSH_Recall: тот меряет
// recall через SearchWithLSH, а там адаптивный цикл поднимает threshold до 32 и
// при нехватке кандидатов уходит в searchNoLSH. То есть полный перебор
// возвращает верный ответ и с мёртвым хешем — recall слеп к качеству LSH по
// построению. Здесь проверяется сам индекс, где подмены нет.
func TestLSH_CandidatePruning(t *testing.T) {
	const (
		n         = 2000
		dim       = 128
		threshold = 8
	)
	idx := NewLSHIndex(dim, 42)
	rng := rand.New(rand.NewSource(20260802))

	vecs := make([][]float32, n)
	for i := range vecs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()*2 - 1
		}
		vecs[i] = v
		idx.Insert(uint32(i), v)
	}

	// Запрос — сосед вектора 0 со сдвигом много меньше разброса самих координат.
	query := make([]float32, dim)
	copy(query, vecs[0])
	for j := range query {
		query[j] += (rng.Float32()*2 - 1) * 0.02
	}
	qHash := idx.ComputeHash(query)

	got := idx.FindCandidates(qHash, threshold, nil)
	t.Logf("кандидатов при threshold=%d: %d из %d", threshold, len(got), n)

	// 1. Сосед обязан остаться. Отсев, теряющий цель, — не отсев, а потеря данных.
	found := false
	for _, id := range got {
		if id == 0 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("истинный сосед отсеян при threshold=%d — LSH теряет цель", threshold)
	}
	// 2. И обязан отсеять большинство. При хеше-константе сюда попадут ВСЕ n.
	if len(got) > n/2 {
		t.Errorf("кандидатов %d из %d — LSH не сужает перебор (мёртвый хеш даёт все %d)",
			len(got), n, n)
	}
	// 3. ПАРНЫЙ КОНТРОЛЬ: при максимальном пороге обязаны вернуться все. Без него
	// проверка выше прошла бы и на индексе, который просто ничего не находит.
	if all := idx.FindCandidates(qHash, 64, nil); len(all) != n {
		t.Errorf("при threshold=64 кандидатов %d, ожидались все %d — индекс неполон", len(all), n)
	}
}
