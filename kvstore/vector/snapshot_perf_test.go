package vector

import (
	"bytes"
	"fmt"
	"math/rand"
	"runtime"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestSnapshotPerformance measures and prints precise execution times and memory allocations.
func TestSnapshotPerformance(t *testing.T) {
	// Инициализируем генератор случайных чисел фиксированным зерном для воспроизводимости
	rng := rand.New(rand.NewSource(42))

	const numVectors = 5000
	const dim = 128

	// Генерируем тестовый набор данных векторов
	vectors := make([][]float32, numVectors)
	for i := 0; i < numVectors; i++ {
		vec := make([]float32, dim)
		for j := 0; j < dim; j++ {
			vec[j] = rng.Float32()
		}
		vectors[i] = vec
	}

	fmt.Printf("\n=== БЕНЧМАРК ПРОИЗВОДИТЕЛЬНОСТИ (N = %d векторов, Dim = %d) ===\n\n", numVectors, dim)

	// ==========================================
	// 1. Построение HNSW-графа с нуля (Old way)
	// ==========================================
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	startTime := time.Now()
	storeOriginal := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	for i := 0; i < numVectors; i++ {
		key := fmt.Sprintf("key_%d", i)
		if err := storeOriginal.Add(key, vectors[i]); err != nil {
			t.Fatalf("Failed to add vector: %v", err)
		}
	}
	buildDuration := time.Since(startTime)

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	buildAlloc := memAfter.TotalAlloc - memBefore.TotalAlloc

	fmt.Printf("1. Построение HNSW-графа с нуля (N*Insert):\n")
	fmt.Printf("   - Время: %.2f ms (%.3f секунд)\n", float64(buildDuration.Microseconds())/1000.0, buildDuration.Seconds())
	fmt.Printf("   - Выделено памяти: %.2f MB\n", float64(buildAlloc)/(1024*1024))
	fmt.Printf("   - Скорость вставки: %.2f векторов/сек\n\n", float64(numVectors)/buildDuration.Seconds())

	// ==========================================
	// 2. Сохранение графа в бинарный поток (SaveBinary)
	// ==========================================
	var buf bytes.Buffer
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	startTime = time.Now()
	if err := storeOriginal.SaveBinary(&buf); err != nil {
		t.Fatalf("Failed to save binary snapshot: %v", err)
	}
	saveDuration := time.Since(startTime)
	runtime.ReadMemStats(&memAfter)
	saveAlloc := memAfter.TotalAlloc - memBefore.TotalAlloc

	snapshotSizeBytes := buf.Len()
	throughputSaveMB := (float64(snapshotSizeBytes) / (1024 * 1024)) / saveDuration.Seconds()

	fmt.Printf("2. Сохранение графа в бинарный снапшот (SaveBinary):\n")
	fmt.Printf("   - Время: %.2f ms (%.3f секунд)\n", float64(saveDuration.Microseconds())/1000.0, saveDuration.Seconds())
	fmt.Printf("   - Размер файла снапшота: %.2f MB (%d байт)\n", float64(snapshotSizeBytes)/(1024*1024), snapshotSizeBytes)
	fmt.Printf("   - Пропускная способность записи: %.2f MB/сек\n", throughputSaveMB)
	fmt.Printf("   - Дополнительно выделено памяти (GC pressure): %.2f KB\n\n", float64(saveAlloc)/1024.0)

	// ==========================================
	// 3. Загрузка графа из бинарного снапшота (LoadBinary)
	// ==========================================
	snapshotBytes := buf.Bytes()
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	startTime = time.Now()
	storeRestored := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	reader := bytes.NewReader(snapshotBytes)
	if err := storeRestored.LoadBinary(reader); err != nil {
		t.Fatalf("Failed to load binary snapshot: %v", err)
	}
	loadDuration := time.Since(startTime)
	runtime.ReadMemStats(&memAfter)
	loadAlloc := memAfter.TotalAlloc - memBefore.TotalAlloc

	throughputLoadMB := (float64(snapshotSizeBytes) / (1024 * 1024)) / loadDuration.Seconds()
	speedupFactor := buildDuration.Seconds() / loadDuration.Seconds()

	fmt.Printf("3. Загрузка графа из бинарного снапшота (LoadBinary):\n")
	fmt.Printf("   - Время: %.2f ms (%.3f секунд)\n", float64(loadDuration.Microseconds())/1000.0, loadDuration.Seconds())
	fmt.Printf("   - Пропускная способность чтения: %.2f MB/сек\n", throughputLoadMB)
	fmt.Printf("   - Дополнительно выделено памяти (GC pressure): %.2f KB\n", float64(loadAlloc)/1024.0)
	fmt.Printf("   - Ускорение старта по сравнению с ребилдом: %.1fx раз\n\n", speedupFactor)

	// ==========================================
	// 4. Проверка качества и скорости векторного поиска
	// ==========================================
	queryVec := make([]float32, dim)
	for j := 0; j < dim; j++ {
		queryVec[j] = rng.Float32()
	}

	// Поиск в оригинальном графе
	startTime = time.Now()
	resOriginal, err := storeOriginal.Search(queryVec, 10, nil)
	if err != nil {
		t.Fatalf("Original search failed: %v", err)
	}
	origSearchDur := time.Since(startTime)

	// Поиск в восстановленном графе
	startTime = time.Now()
	resRestored, err := storeRestored.Search(queryVec, 10, nil)
	if err != nil {
		t.Fatalf("Restored search failed: %v", err)
	}
	restSearchDur := time.Since(startTime)

	// Проверяем 100% идентичность результатов и дистанций
	if len(resOriginal) != len(resRestored) {
		t.Fatalf("Results count mismatch: got %d, want %d", len(resRestored), len(resOriginal))
	}

	for i := 0; i < len(resOriginal); i++ {
		if resOriginal[i].Key != resRestored[i].Key || resOriginal[i].Distance != resRestored[i].Distance {
			t.Fatalf("Mismatch at rank %d: Original={%s, %f}, Restored={%s, %f}",
				i, resOriginal[i].Key, resOriginal[i].Distance, resRestored[i].Key, resRestored[i].Distance)
		}
	}

	fmt.Printf("4. Сравнение производительности поиска (Search QPS):\n")
	fmt.Printf("   - Результаты поиска совпали на 100%% (полная идентичность графа)\n")
	fmt.Printf("   - Время поиска (оригинальный HNSW): %.3f ms\n", float64(origSearchDur.Nanoseconds())/1e6)
	fmt.Printf("   - Время поиска (восстановленный HNSW): %.3f ms\n", float64(restSearchDur.Nanoseconds())/1e6)
	fmt.Printf("===============================================================\n\n")
}
