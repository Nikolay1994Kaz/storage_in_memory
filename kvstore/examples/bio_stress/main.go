package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

func main() {
	const (
		duration = 6 * time.Hour // Время работы теста
		dim      = 640           // Размерность векторов (ESM-2 белки)
		initSize = 10000         // Начальный размер базы
		numSubs  = 1000          // Количество подписчиков
		logFreq  = 1 * time.Minute
	)

	fmt.Println("====================================================")
	fmt.Println("🧬 STARTING 6-HOUR BIOINFORMATICS STRESS TEST 🧬")
	fmt.Printf("   - Duration: %v\n", duration)
	fmt.Printf("   - Dimension: %d (ESM-2)\n", dim)
	fmt.Printf("   - Database Size: %d proteins\n", initSize)
	fmt.Printf("   - Subscribers: %d\n", numSubs)
	fmt.Println("====================================================")

	rng := rand.New(rand.NewSource(42))
	allocator := tcmalloc.NewTCMallocStore(runtime.NumCPU())
	vs := vector.NewVectorStoreCosine(allocator) // Косинусная близость

	// 0. Инициализация WAL (Write-Ahead Log) для обеспечения durability
	walDir := "./wal_bio_data"
	if err := os.MkdirAll(walDir, 0755); err != nil {
		panic(fmt.Errorf("failed to create WAL directory: %w", err))
	}
	walPath := filepath.Join(walDir, "bio_stress.wal")
	rawWAL, err := wal.Open(walPath)
	if err != nil {
		panic(fmt.Errorf("failed to open WAL: %w", err))
	}
	bw := wal.NewBatchWAL(rawWAL)
	defer func() {
		bw.Close()
		os.RemoveAll(walDir) // Очищаем временную папку логов после закрытия
	}()

	// Счётчики операций
	var searchCount uint64
	var pubCount uint64
	var writeCount uint64
	var deliveredCount uint64
	var walCount uint64 // Счётчик операций в WAL

	// Helper-функция для кодирования вектора и записи в WAL
	writeToWAL := func(op byte, key string, vec []float32) {
		var valBytes []byte
		if len(vec) > 0 {
			valBytes = make([]byte, len(vec)*4)
			for i, f := range vec {
				binary.LittleEndian.PutUint32(valBytes[i*4:], math.Float32bits(f))
			}
		}
		bw.Write(wal.Entry{
			Op:    op,
			Key:   key,
			Value: valBytes,
		})
		atomic.AddUint64(&walCount, 1)
	}

	// 1. Генерируем центроиды семейств белков
	const numFamilies = 50
	families := make([][]float32, numFamilies)
	for i := 0; i < numFamilies; i++ {
		fam := make([]float32, dim)
		for j := range fam {
			fam[j] = rng.Float32()*2 - 1
		}
		vector.Normalize(fam)
		families[i] = fam
	}

	// Helper для создания белкового вектора из семейства
	genProtein := func(r *rand.Rand) []float32 {
		famIdx := r.Intn(numFamilies)
		fam := families[famIdx]
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = fam[j] + float32(r.NormFloat64()*0.02)
		}
		vector.Normalize(vec)
		return vec
	}

	// 2. Первоначальное наполнение базы
	fmt.Println("⏳ Pre-populating protein database...")
	for i := 0; i < initSize; i++ {
		vec := genProtein(rng)
		key := fmt.Sprintf("protein:id:%d", i)
		if err := vs.Add(key, vec); err != nil {
			panic(err)
		}
		// Дублируем запись в WAL
		writeToWAL(wal.OpVSimAdd, key, vec)
	}
	fmt.Println("✅ Base populated.")

	// 3. Регистрация семантических подписчиков
	type mockSub struct {
		ch   chan []byte
		done chan struct{}
	}
	subs := make([]*mockSub, numSubs)
	subIndex := vector.NewVectorStoreCosine(allocator)

	fmt.Println("⏳ Registering subscribers...")
	for i := 0; i < numSubs; i++ {
		subs[i] = &mockSub{
			ch:   make(chan []byte, 100),
			done: make(chan struct{}),
		}
		interest := genProtein(rng)
		key := fmt.Sprintf("sub:%d", i)
		if err := subIndex.Add(key, interest); err != nil {
			panic(err)
		}
		// Запись о подписчике также дублируется в WAL
		writeToWAL(wal.OpVSimAdd, key, interest)
	}

	// Фоновые обработчики каналов подписчиков (вычищают сообщения, чтоб не было блока)
	for i := 0; i < numSubs; i++ {
		go func(s *mockSub) {
			for {
				select {
				case <-s.ch:
				case <-s.done:
					return
				}
			}
		}(subs[i])
	}
	fmt.Println("✅ Subscribers ready.")

	var stop uint32
	var wg sync.WaitGroup

	startTime := time.Now()
	endTime := startTime.Add(duration)

	// 4. Запуск потоков нагрузки
	numCPU := runtime.NumCPU()
	fmt.Printf("⏳ Launching %d search worker threads...\n", numCPU)
	for w := 0; w < numCPU; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			localRng := rand.New(rand.NewSource(int64(100 + workerID)))
			for atomic.LoadUint32(&stop) == 0 {
				q := genProtein(localRng)
				_, _ = vs.Search(q, 10)
				atomic.AddUint64(&searchCount, 1)
			}
		}(w)
	}

	fmt.Println("⏳ Launching background event publisher and mutations loop...")
	wg.Add(1)
	go func() {
		defer wg.Done()
		localRng := rand.New(rand.NewSource(2026))
		
		// Будем держать ID вставляемых белков в диапазоне, чтоб база не росла бесконечно
		insertID := initSize

		for atomic.LoadUint32(&stop) == 0 {
			// А. Публикация событий (Pub/Sub)
			eventVec := genProtein(localRng)
			results, err := subIndex.Search(eventVec, numSubs)
			if err == nil {
				for _, r := range results {
					if r.Distance <= 0.40 {
						var subIdx int
						fmt.Sscanf(r.Key, "sub:%d", &subIdx)
						select {
						case subs[subIdx].ch <- []byte("alert"):
							atomic.AddUint64(&deliveredCount, 1)
						default:
						}
					}
				}
			}
			atomic.AddUint64(&pubCount, 1)

			// Б. Мутации базы данных (10 удалений + 10 добавлений)
			for i := 0; i < 10; i++ {
				// Случайно удаляем старый
				delIdx := localRng.Intn(initSize)
				delKey := fmt.Sprintf("protein:id:%d", delIdx)
				vs.Delete(delKey)
				writeToWAL(wal.OpVSimDel, delKey, nil)

				// Вставляем новый с тем же ключом
				newVec := genProtein(localRng)
				vs.Add(delKey, newVec)
				writeToWAL(wal.OpVSimAdd, delKey, newVec)
			}

			// Вставляем абсолютно новые белки
			for i := 0; i < 10; i++ {
				newVec := genProtein(localRng)
				newKey := fmt.Sprintf("protein:id:%d", insertID)
				vs.Add(newKey, newVec)
				writeToWAL(wal.OpVSimAdd, newKey, newVec)
				
				// И сразу удаляем самый старый из добавленных, сохраняя размер около 10k
				oldKey := fmt.Sprintf("protein:id:%d", insertID-initSize)
				vs.Delete(oldKey)
				writeToWAL(wal.OpVSimDel, oldKey, nil)
				
				insertID++
			}
			atomic.AddUint64(&writeCount, 20)

			// Небольшая пауза в цикле мутаций, чтобы дать больше ресурсов поиску
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// 5. Логирование и таймер завершения
	logTicker := time.NewTicker(logFreq)
	defer logTicker.Stop()

	fmt.Println("🚀 Stress test running. Check metrics in stdout.")
	
	for {
		select {
		case <-logTicker.C:
			elapsed := time.Since(startTime)
			remaining := time.Until(endTime)
			
			// Собираем метрики памяти
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			heapAlloc := float64(mem.Alloc) / (1024 * 1024)
			sysMem := float64(mem.Sys) / (1024 * 1024)

			// Собираем размер файла WAL
			walSizeMB := float64(0)
			if fi, err := os.Stat(walPath); err == nil {
				walSizeMB = float64(fi.Size()) / (1024 * 1024)
			}
			
			fmt.Printf("[%s] Elapsed: %s | Remaining: %s\n", 
				time.Now().Format("15:04:05"), 
				elapsed.Round(time.Second), 
				remaining.Round(time.Second),
			)
			fmt.Printf("   - Searches executed:  %d (%.2f QPS)\n", 
				atomic.LoadUint64(&searchCount), 
				float64(atomic.LoadUint64(&searchCount))/elapsed.Seconds(),
			)
			fmt.Printf("   - Events published:   %d\n", atomic.LoadUint64(&pubCount))
			fmt.Printf("   - Messages delivered: %d\n", atomic.LoadUint64(&deliveredCount))
			fmt.Printf("   - DB Mutations (ops): %d (%.2f wps)\n", 
				atomic.LoadUint64(&writeCount),
				float64(atomic.LoadUint64(&writeCount))/elapsed.Seconds(),
			)
			fmt.Printf("   - WAL Entries Written:%d (%.2f ops/sec)\n", 
				atomic.LoadUint64(&walCount),
				float64(atomic.LoadUint64(&walCount))/elapsed.Seconds(),
			)
			fmt.Printf("   - WAL File Size:      %.2f MB\n", walSizeMB)
			fmt.Printf("   - Go Heap Alloc:      %.2f MB\n", heapAlloc)
			fmt.Printf("   - Go Sys (OS Virtual): %.2f MB\n", sysMem)
			fmt.Println("   -------------------------------------------------")

			if remaining <= 0 {
				fmt.Println("⏳ Stress test time completed. Stopping workers...")
				atomic.StoreUint32(&stop, 1)
				
				// Завершаем фоновые горутины подписчиков
				for i := 0; i < numSubs; i++ {
					close(subs[i].done)
				}
				
				wg.Wait()
				fmt.Println("🎉 STRESS TEST SUCCESSFULLY COMPLETED!")
				return
			}
		}
	}
}
