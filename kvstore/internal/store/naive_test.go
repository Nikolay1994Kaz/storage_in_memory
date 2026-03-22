package store

import (
	"fmt"
	"math/rand"
	"runtime"
	"testing"
)

// generateKeys создаёт набор случайных ключей заранее,
// чтобы не тратить время бенчмарка на генерацию строк.
func generateKeys(n int) []string {
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = fmt.Sprintf("key_%d", i)
	}
	return keys
}

// BenchmarkNaiveStore_Set измеряет скорость записи
// при разном количестве конкурентных горутин.
func BenchmarkNaiveStore_Set(b *testing.B) {
	goroutineCounts := []int{1, 100, 1000, 10000}

	for _, numG := range goroutineCounts {
		b.Run(fmt.Sprintf("goroutines_%d", numG), func(b *testing.B) {
			s := NewNaiveStore()
			keys := generateKeys(10000)
			value := []byte("some_value_here")
			cores := runtime.GOMAXPROCS(0)
			p := numG / cores
			if p < 1 {
				p = 1
			}
			b.SetParallelism(p)

			b.ResetTimer() // Сбрасываем таймер — не считаем время подготовки

			b.RunParallel(func(pb *testing.PB) {
				// Каждая горутина получает свой локальный rand
				// чтобы избежать contention на глобальном rand
				localRand := rand.New(rand.NewSource(rand.Int63()))

				for pb.Next() {
					key := keys[localRand.Intn(len(keys))]
					s.Set(key, value)
				}
			})
		})
	}
}

// BenchmarkNaiveStore_Get измеряет скорость чтения.
func BenchmarkNaiveStore_Get(b *testing.B) {
	goroutineCounts := []int{1, 100, 1000, 10000}

	for _, numG := range goroutineCounts {
		b.Run(fmt.Sprintf("goroutines_%d", numG), func(b *testing.B) {
			s := NewNaiveStore()
			keys := generateKeys(10000)
			value := []byte("some_value_here")

			// Предзаполняем хранилище
			for _, k := range keys {
				s.Set(k, value)
			}
			cores := runtime.GOMAXPROCS(0)
			p := numG / cores
			if p < 1 {
				p = 1
			}
			b.SetParallelism(p)

			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				localRand := rand.New(rand.NewSource(rand.Int63()))

				for pb.Next() {
					key := keys[localRand.Intn(len(keys))]
					s.Get(key)
				}
			})
		})
	}
}

// BenchmarkNaiveStore_Mixed — смешанная нагрузка: 80% GET, 20% SET.
// Это самый реалистичный сценарий.
func BenchmarkNaiveStore_Mixed(b *testing.B) {
	goroutineCounts := []int{1, 100, 1000, 10000}

	for _, numG := range goroutineCounts {
		b.Run(fmt.Sprintf("goroutines_%d", numG), func(b *testing.B) {
			s := NewNaiveStore()
			keys := generateKeys(10000)
			value := []byte("some_value_here")

			for _, k := range keys {
				s.Set(k, value)
			}

			// --- НОВЫЙ КОД ЗДЕСЬ ---
			// Узнаем, сколько ядер доступно (у вас 12)
			cores := runtime.GOMAXPROCS(0)
			// Считаем множитель, чтобы получить желаемое numG
			p := numG / cores
			if p < 1 {
				p = 1 // Минимум 1 множитель (в итоге будет 12 горутин)
			}
			b.SetParallelism(p)
			// -----------------------

			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				localRand := rand.New(rand.NewSource(rand.Int63()))

				for pb.Next() {
					key := keys[localRand.Intn(len(keys))]
					if localRand.Intn(100) < 80 {
						s.Get(key)
					} else {
						s.Set(key, value)
					}
				}
			})
		})
	}
}
