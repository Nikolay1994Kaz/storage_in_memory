package store

import (
	"math/rand"
	"testing"
)

func BenchmarkShardedStore_Set(b *testing.B) {
	s := NewShardedStore()
	keys := generateKeys(10000)
	value := []byte("some_value_here")

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		localRand := rand.New(rand.NewSource(rand.Int63()))
		for pb.Next() {
			key := keys[localRand.Intn(len(keys))]
			s.Set(key, value)
		}
	})
}

func BenchmarkShardedStore_Get(b *testing.B) {
	s := NewShardedStore()
	keys := generateKeys(10000)
	value := []byte("some_value_here")

	for _, k := range keys {
		s.Set(k, value)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		localRand := rand.New(rand.NewSource(rand.Int63()))
		for pb.Next() {
			key := keys[localRand.Intn(len(keys))]
			s.Get(key)
		}
	})
}

func BenchmarkShardedStore_Mixed(b *testing.B) {
	s := NewShardedStore()
	keys := generateKeys(10000)
	value := []byte("some_value_here")

	for _, k := range keys {
		s.Set(k, value)
	}

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
}

// BenchmarkComparison — прямое сравнение Naive vs Sharded.
func BenchmarkComparison_Mixed(b *testing.B) {
	keys := generateKeys(10000)
	value := []byte("some_value_here")

	b.Run("Naive", func(b *testing.B) {
		s := NewNaiveStore()
		for _, k := range keys {
			s.Set(k, value)
		}
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

	b.Run("Sharded", func(b *testing.B) {
		s := NewShardedStore()
		for _, k := range keys {
			s.Set(k, value)
		}
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
