package vector

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestBioLoadBenchmark — полноценный нагрузочный тест для биоинформатики.
// Имитирует базу данных белков (эмбеддинги ESM-2, размерность 640).
// Запуск: go test -v -run=TestBioLoadBenchmark ./vector/ -timeout 30m
func TestBioLoadBenchmark(t *testing.T) {
	const (
		numProteins = 10000 // Размер базы данных белков
		dim         = 640   // Размерность ESM-2 белкового вектора
		numQueries  = 1000  // Количество поисковых запросов в тесте
		numSubs     = 1000  // Количество семантических подписчиков
		numPubs     = 5000  // Количество публикуемых событий в секунду/тест
	)

	fmt.Printf("\n=== БЕНЧМАРК НАГРУЗКИ: БИОИНФОРМАТИКА (Dim=%d) ===\n", dim)
	
	// Выделяем память
	runtime.GC()
	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	rng := rand.New(rand.NewSource(42))
	allocator := tcmalloc.NewTCMallocStore(4)
	vs := NewVectorStore(DotProductDistance, allocator)
	vs.autoNormalize = true // Для быстрой косинусной близости через DotProduct

	// ---------------------------------------------------------
	// Имитация реальной структуры белковых семейств (кластеров)
	// ---------------------------------------------------------
	const numFamilies = 50
	families := make([][]float32, numFamilies)
	for i := 0; i < numFamilies; i++ {
		fam := make([]float32, dim)
		for j := range fam {
			fam[j] = rng.Float32()*2 - 1
		}
		Normalize(fam)
		families[i] = fam
	}

	// 1. Тест индексации (Ingestion Rate)
	// ---------------------------------------------------------
	fmt.Printf("1. Индексация %d белков (распределенных по %d семействам)...\n", numProteins, numFamilies)
	startInsert := time.Now()

	for i := 0; i < numProteins; i++ {
		famIdx := rng.Intn(numFamilies)
		fam := families[famIdx]

		vec := make([]float32, dim)
		// Добавляем шум (мутацию белка внутри семейства)
		for j := range vec {
			vec[j] = fam[j] + float32(rng.NormFloat64()*0.02)
		}
		Normalize(vec)
		
		key := fmt.Sprintf("protein:id:%d", i)
		if err := vs.Add(key, vec); err != nil {
			t.Fatalf("Add error at %d: %v", i, err)
		}
	}
	insertDuration := time.Since(startInsert)
	fmt.Printf("   - Время вставки: %v\n", insertDuration)
	fmt.Printf("   - Скорость: %.2f белков/сек\n", float64(numProteins)/insertDuration.Seconds())

	// Проверяем память
	runtime.GC()
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)
	memUsedMB := float64(mAfter.Alloc-mBefore.Alloc) / 1024 / 1024
	fmt.Printf("   - Память под граф: %.2f MB (в среднем %.2f KB на белок)\n", memUsedMB, memUsedMB*1024/float64(numProteins))

	// Генерируем тестовые поисковые векторы (привязанные к семействам белков)
	queries := make([][]float32, numQueries)
	for i := range queries {
		famIdx := rng.Intn(numFamilies)
		fam := families[famIdx]

		q := make([]float32, dim)
		for j := range q {
			q[j] = fam[j] + float32(rng.NormFloat64()*0.02)
		}
		Normalize(q)
		queries[i] = q
	}

	// ---------------------------------------------------------
	// 2. Тест параллельного поиска (Parallel Search QPS)
	// ---------------------------------------------------------
	fmt.Printf("2. Запуск параллельного поиска (%d запросов на всех ядрах)...\n", numQueries)
	
	concurrency := runtime.NumCPU()
	fmt.Printf("   - Активных ядер CPU: %d\n", concurrency)
	
	var wg sync.WaitGroup
	queriesPerThread := numQueries / concurrency
	
	startSearch := time.Now()
	for tId := 0; tId < concurrency; tId++ {
		wg.Add(1)
		go func(threadID int) {
			defer wg.Done()
			offset := threadID * queriesPerThread
			for i := 0; i < queriesPerThread; i++ {
				_, err := vs.Search(queries[offset+i], 10) // Ищем топ-10 похожих белков
				if err != nil {
					return
				}
			}
		}(tId)
	}
	wg.Wait()
	searchDuration := time.Since(startSearch)
	qps := float64(numQueries) / searchDuration.Seconds()
	fmt.Printf("   - Время поиска: %v\n", searchDuration)
	fmt.Printf("   - Производительность: %.2f запросов/сек (QPS)\n", qps)
	fmt.Printf("   - Среднее время на 1 поиск: %.3f мс\n", searchDuration.Seconds()*1000/float64(numQueries))

	// ---------------------------------------------------------
	// 3. Тест семантического Pub/Sub (Event Routing)
	// ---------------------------------------------------------
	fmt.Printf("3. Нагрузочный тест семантического Pub/Sub (%d подписчиков, %d событий)...\n", numSubs, numPubs)
	
	// Подготавливаем шину данных
	subIndex := NewVectorStoreCosine(allocator)
	
	// Имитируем сетевые соединения подписчиков
	type mockSub struct {
		ch      chan []byte
		done    chan struct{}
		msgRecv int32
	}
	
	subs := make([]*mockSub, numSubs)
	
	// Регистрируем подписчиков в индексе интересов (привязываем к семействам)
	for i := 0; i < numSubs; i++ {
		subs[i] = &mockSub{
			ch:   make(chan []byte, 100),
			done: make(chan struct{}),
		}
		
		famIdx := rng.Intn(numFamilies)
		fam := families[famIdx]

		interestVec := make([]float32, dim)
		for j := range interestVec {
			interestVec[j] = fam[j] + float32(rng.NormFloat64()*0.02)
		}
		Normalize(interestVec)
		
		key := fmt.Sprintf("sub:%d", i)
		if err := subIndex.Add(key, interestVec); err != nil {
			t.Fatalf("Sub register error: %v", err)
		}
	}

	// Запуск фоновых обработчиков доставки (имитация реальных клиентов)
	for i := 0; i < numSubs; i++ {
		go func(s *mockSub) {
			for {
				select {
				case <-s.ch:
					s.msgRecv++
				case <-s.done:
					return
				}
			}
		}(subs[i])
	}

	// Поток публикации событий
	pubStart := time.Now()
	deliveredTotal := 0
	
	for i := 0; i < numPubs; i++ {
		famIdx := rng.Intn(numFamilies)
		fam := families[famIdx]

		eventVec := make([]float32, dim)
		for j := range eventVec {
			eventVec[j] = fam[j] + float32(rng.NormFloat64()*0.02)
		}
		Normalize(eventVec)
		
		// Делаем семантический поиск интересов среди всех подписчиков
		// (Имитируем поведение Hub.SemanticPublish)
		results, err := subIndex.Search(eventVec, numSubs)
		if err != nil {
			t.Fatalf("Event search error: %v", err)
		}
		
		// Доставляем тем, кто проходит по порогу (threshold = 0.40)
		for _, r := range results {
			if r.Distance <= 0.40 {
				var subIdx int
				fmt.Sscanf(r.Key, "sub:%d", &subIdx)
				select {
				case subs[subIdx].ch <- []byte("new protein alert"):
					deliveredTotal++
				default:
					// Канал переполнен (медленный клиент)
				}
			}
		}
	}
	
	pubDuration := time.Since(pubStart)
	
	// Корректно завершаем горутины
	for i := 0; i < numSubs; i++ {
		close(subs[i].done)
	}

	fmt.Printf("   - Время обработки Pub/Sub: %v\n", pubDuration)
	fmt.Printf("   - Всего доставлено сообщений: %d\n", deliveredTotal)
	fmt.Printf("   - Скорость маршрутизации: %.2f публикаций/сек\n", float64(numPubs)/pubDuration.Seconds())
	fmt.Printf("   - Средняя задержка матчинга 1 события: %.3f мс\n", pubDuration.Seconds()*1000/float64(numPubs))
	fmt.Printf("===================================================\n\n")
}
