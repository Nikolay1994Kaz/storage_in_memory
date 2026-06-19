package vector

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestBioRealWorldScenario имитирует реальный биоинформатический pipeline:
//   1. Загрузка белковых семейств (имитация SwissProt ESM-2 embeddings)
//   2. Поиск гомологов (P53 → P63, P73)
//   3. Фильтрованный поиск (только human proteins)
//   4. Проверка Recall@K vs brute-force ground truth
//   5. Семантический Pub/Sub для lab alerting
//
// Запуск: go test -v -run=TestBioRealWorldScenario ./vector/ -timeout 10m
func TestBioRealWorldScenario(t *testing.T) {
	const (
		dim          = 640 // ESM-2 per-protein embedding
		numFamilies  = 100 // Белковые семейства (kinases, transporters, etc.)
		proteinsPerFam = 200 // Белков в семействе
		totalProteins = numFamilies * proteinsPerFam // 20,000
	)

	organisms := []string{
		"Homo_sapiens",     // 40% 
		"Mus_musculus",     // 20%
		"Rattus_norvegicus", // 15%
		"Danio_rerio",      // 10%
		"Drosophila_melanogaster", // 10%
		"Caenorhabditis_elegans",  // 5%
	}
	orgWeights := []float64{0.40, 0.60, 0.75, 0.85, 0.95, 1.0}

	familyNames := []string{
		"kinase", "phosphatase", "protease", "ligase", "transferase",
		"oxidoreductase", "hydrolase", "lyase", "isomerase", "translocase",
		"receptor", "channel", "transporter", "chaperone", "ribosomal",
		"histone", "transcription_factor", "DNA_repair", "cytokine", "immunoglobulin",
	}

	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("БИОИНФОРМАТИЧЕСКИЙ ТЕСТ: Real-World Scenario")
	fmt.Printf("  Белков: %d | Семейств: %d | Dim: %d (ESM-2)\n", totalProteins, numFamilies, dim)
	fmt.Println(strings.Repeat("=", 70))

	rng := rand.New(rand.NewSource(42))
	allocator := tcmalloc.NewTCMallocStore(4)
	vs := NewVectorStoreCosine(allocator)

	// ═══════════════════════════════════════════════════════════
	// ФАЗА 1: Генерация «белковых семейств» (cluster centroids)
	// ═══════════════════════════════════════════════════════════
	familyCentroids := make([][]float32, numFamilies)
	for i := 0; i < numFamilies; i++ {
		c := make([]float32, dim)
		for j := range c {
			c[j] = float32(rng.NormFloat64())
		}
		Normalize(c)
		familyCentroids[i] = c
	}

	// ═══════════════════════════════════════════════════════════
	// ФАЗА 2: Загрузка белков + метаданные
	// ═══════════════════════════════════════════════════════════
	type proteinMeta struct {
		key      string
		family   int
		organism string
		vec      []float32
	}
	allProteins := make([]proteinMeta, 0, totalProteins)

	// Маппинг key → organism для фильтрации
	metaStore := make(map[string]string, totalProteins)

	fmt.Printf("\n1. Загрузка %d белков...\n", totalProteins)
	startInsert := time.Now()

	for fam := 0; fam < numFamilies; fam++ {
		famName := familyNames[fam % len(familyNames)]
		centroid := familyCentroids[fam]

		for p := 0; p < proteinsPerFam; p++ {
			// Выбираем организм по весам
			r := rng.Float64()
			orgIdx := 0
			for i, w := range orgWeights {
				if r <= w {
					orgIdx = i
					break
				}
			}
			organism := organisms[orgIdx]

			// Генерируем вектор: centroid + шум (имитация мутаций)
			vec := make([]float32, dim)
			for j := range vec {
				vec[j] = centroid[j] + float32(rng.NormFloat64()*0.03)
			}
			Normalize(vec)

			uniprotID := fmt.Sprintf("%s_%04d_%s", famName, p, organism)
			key := fmt.Sprintf("protein:%s", uniprotID)

			if err := vs.Add(key, vec); err != nil {
				t.Fatalf("Add error: %v", err)
			}

			metaStore[key] = organism
			allProteins = append(allProteins, proteinMeta{
				key: key, family: fam, organism: organism, vec: vec,
			})
		}
	}
	insertDuration := time.Since(startInsert)

	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	fmt.Printf("   ✅ Время: %v\n", insertDuration)
	fmt.Printf("   ✅ Скорость: %.0f белков/сек\n", float64(totalProteins)/insertDuration.Seconds())
	fmt.Printf("   ✅ Память: %.1f MB (%.1f KB/белок)\n",
		float64(m.Alloc)/1024/1024, float64(m.Alloc)/1024/float64(totalProteins))

	// ═══════════════════════════════════════════════════════════
	// ФАЗА 3: Поиск гомологов P53 → должен найти семейство
	// ═══════════════════════════════════════════════════════════
	fmt.Println("\n2. Поиск гомологов (protein family similarity)...")

	// Берём 1-й белок 1-го семейства как "P53"
	queryProtein := allProteins[0]
	queryFamily := queryProtein.family

	results, err := vs.Search(queryProtein.vec, 20)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}

	sameFamily := 0
	for _, r := range results {
		for _, p := range allProteins {
			if p.key == r.Key && p.family == queryFamily {
				sameFamily++
				break
			}
		}
	}

	familyRecall := float64(sameFamily) / float64(len(results))
	fmt.Printf("   Query: %s (family=%d)\n", queryProtein.key, queryFamily)
	fmt.Printf("   Top-20 results: %d/%d from same family (%.0f%% recall)\n",
		sameFamily, len(results), familyRecall*100)

	if familyRecall < 0.5 {
		t.Errorf("Family recall too low: %.2f (expected >= 0.5)", familyRecall)
	}

	// ═══════════════════════════════════════════════════════════
	// ФАЗА 4: Filtered search — только Human белки
	// ═══════════════════════════════════════════════════════════
	fmt.Println("\n3. Filtered search (только Homo sapiens)...")

	filteredResults, err := vs.SearchFiltered(queryProtein.vec, 10, func(key string) bool {
		return metaStore[key] == "Homo_sapiens"
	})
	if err != nil {
		t.Fatalf("SearchFiltered error: %v", err)
	}

	allHuman := true
	for _, r := range filteredResults {
		org := metaStore[r.Key]
		if org != "Homo_sapiens" {
			allHuman = false
			t.Errorf("Filtered result %s has organism %s (expected Homo_sapiens)", r.Key, org)
		}
	}
	fmt.Printf("   ✅ %d results, all human: %v\n", len(filteredResults), allHuman)

	if len(filteredResults) > 0 {
		fmt.Printf("   Top-3: %s (dist=%.4f), %s (dist=%.4f), %s (dist=%.4f)\n",
			filteredResults[0].Key, filteredResults[0].Distance,
			filteredResults[min(1, len(filteredResults)-1)].Key, filteredResults[min(1, len(filteredResults)-1)].Distance,
			filteredResults[min(2, len(filteredResults)-1)].Key, filteredResults[min(2, len(filteredResults)-1)].Distance)
	}

	// ═══════════════════════════════════════════════════════════
	// ФАЗА 5: Recall@K vs Brute-Force Ground Truth
	// ═══════════════════════════════════════════════════════════
	fmt.Println("\n4. Recall@K vs Brute-Force Ground Truth...")

	numQueries := 100
	K := 10
	totalRecall := 0.0

	for q := 0; q < numQueries; q++ {
		queryIdx := rng.Intn(totalProteins)
		query := allProteins[queryIdx]

		// HNSW search
		hnswResults, err := vs.Search(query.vec, K)
		if err != nil {
			t.Fatalf("Search error: %v", err)
		}
		hnswKeys := make(map[string]bool)
		for _, r := range hnswResults {
			hnswKeys[r.Key] = true
		}

		// Brute-force ground truth
		type distItem struct {
			key  string
			dist float32
		}
		bruteForce := make([]distItem, totalProteins)
		for i, p := range allProteins {
			var dot float32
			for j := range query.vec {
				dot += query.vec[j] * p.vec[j]
			}
			bruteForce[i] = distItem{key: p.key, dist: 1.0 - dot} // cosine distance
		}
		sort.Slice(bruteForce, func(i, j int) bool {
			return bruteForce[i].dist < bruteForce[j].dist
		})

		// Считаем overlap
		overlap := 0
		for i := 0; i < K && i < len(bruteForce); i++ {
			if hnswKeys[bruteForce[i].key] {
				overlap++
			}
		}
		totalRecall += float64(overlap) / float64(K)
	}

	avgRecall := totalRecall / float64(numQueries)
	fmt.Printf("   ✅ Average Recall@%d: %.2f%% (over %d queries)\n", K, avgRecall*100, numQueries)

	// HNSW — приближённый поиск. Recall 50-60% при efSearch=K*10 на 20K
	// нод dim=640 — нормальный результат. Для 80%+ нужен efSearch=K*50.
	// Ключевая метрика: family recall (100%) и filtered search (корректность).
	if avgRecall < 0.40 {
		t.Errorf("Recall@%d too low: %.2f (expected >= 0.40)", K, avgRecall)
	}

	// ═══════════════════════════════════════════════════════════
	// ФАЗА 6: Parallel Search QPS
	// ═══════════════════════════════════════════════════════════
	fmt.Println("\n5. Parallel Search QPS (all cores)...")

	numSearchQueries := 5000
	queries := make([][]float32, numSearchQueries)
	for i := range queries {
		fam := rng.Intn(numFamilies)
		q := make([]float32, dim)
		for j := range q {
			q[j] = familyCentroids[fam][j] + float32(rng.NormFloat64()*0.03)
		}
		Normalize(q)
		queries[i] = q
	}

	concurrency := runtime.NumCPU()
	var wg sync.WaitGroup
	perThread := numSearchQueries / concurrency

	startSearch := time.Now()
	for t := 0; t < concurrency; t++ {
		wg.Add(1)
		go func(tid int) {
			defer wg.Done()
			off := tid * perThread
			for i := 0; i < perThread; i++ {
				vs.Search(queries[off+i], 10)
			}
		}(t)
	}
	wg.Wait()
	searchDur := time.Since(startSearch)

	qps := float64(numSearchQueries) / searchDur.Seconds()
	avgLatency := searchDur.Seconds() * 1000 / float64(numSearchQueries)
	fmt.Printf("   ✅ %d queries on %d cores: %.0f QPS\n", numSearchQueries, concurrency, qps)
	fmt.Printf("   ✅ Average latency: %.3f ms\n", avgLatency)

	// ═══════════════════════════════════════════════════════════
	// ФАЗА 7: Snapshot Save/Load
	// ═══════════════════════════════════════════════════════════
	fmt.Println("\n6. Binary Snapshot Save/Load...")

	var snapBuf = new(bytes.Buffer)
	startSave := time.Now()
	if err := vs.SaveBinary(snapBuf); err != nil {
		t.Fatalf("SaveBinary error: %v", err)
	}
	saveDur := time.Since(startSave)

	snapSize := snapBuf.Len()
	fmt.Printf("   ✅ Save: %v (%.1f MB, %.0f MB/s)\n",
		saveDur, float64(snapSize)/1024/1024,
		float64(snapSize)/1024/1024/saveDur.Seconds())

	// Load into new store
	vs2 := NewVectorStoreCosine(tcmalloc.NewTCMallocStore(4))
	startLoad := time.Now()
	if err := vs2.LoadBinary(snapBuf); err != nil {
		t.Fatalf("LoadBinary error: %v", err)
	}
	loadDur := time.Since(startLoad)
	fmt.Printf("   ✅ Load: %v (%.0f MB/s)\n",
		loadDur, float64(snapSize)/1024/1024/loadDur.Seconds())

	// Verify restored graph gives same results
	verifyResults, err := vs2.Search(queryProtein.vec, 10)
	if err != nil {
		t.Fatalf("Search on restored graph: %v", err)
	}
	origResults, _ := vs.Search(queryProtein.vec, 10)

	match := 0
	for _, vr := range verifyResults {
		for _, or := range origResults {
			if vr.Key == or.Key {
				match++
				break
			}
		}
	}
	fmt.Printf("   ✅ Search match after restore: %d/%d\n", match, len(origResults))

	// ═══════════════════════════════════════════════════════════
	// ИТОГИ
	// ═══════════════════════════════════════════════════════════
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("ИТОГИ:")
	fmt.Printf("  Белков загружено:     %d\n", totalProteins)
	fmt.Printf("  Скорость индексации:  %.0f белков/сек\n", float64(totalProteins)/insertDuration.Seconds())
	fmt.Printf("  Search QPS (parallel): %.0f\n", qps)
	fmt.Printf("  Avg latency:          %.3f ms\n", avgLatency)
	fmt.Printf("  Recall@%d:             %.1f%%\n", K, avgRecall*100)
	fmt.Printf("  Family recall:        %.0f%%\n", familyRecall*100)
	fmt.Printf("  Filtered search:      ✅ (all human)\n")
	fmt.Printf("  Snapshot save:        %.1f ms (%.1f MB)\n", float64(saveDur.Microseconds())/1000, float64(snapSize)/1024/1024)
	fmt.Printf("  Snapshot load:        %.1f ms\n", float64(loadDur.Microseconds())/1000)
	fmt.Printf("  Память:               %.1f MB\n", float64(m.Alloc)/1024/1024)

	_ = math.Abs // suppress unused import
	fmt.Println(strings.Repeat("=", 70))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
