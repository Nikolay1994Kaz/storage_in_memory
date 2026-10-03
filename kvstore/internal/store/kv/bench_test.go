package kv

import (
	"runtime"
	"strconv"
	"testing"
	"time"
)

// Условия те же, что в A/B 22.09.2026: 10k ключей вида k:N, значение 20 байт.
const benchKeys = 10_000

func benchStore(b *testing.B) (*Store, []string) {
	s := New()
	keys := make([]string, benchKeys)
	val := make([]byte, 20)
	for i := range keys {
		keys[i] = "k:" + strconv.Itoa(i)
		s.Set(keys[i], val)
	}
	return s, keys
}

func BenchmarkGet(b *testing.B) {
	s, keys := benchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Get(keys[i%benchKeys])
	}
}

func BenchmarkGetParallel(b *testing.B) {
	s, keys := benchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.Get(keys[i%benchKeys])
			i++
		}
	})
}

// BenchmarkSet — перезапись существующих ключей (вставка ключа в map тут не
// меряется). Аллокаций 0: стор забирает срез значения во владение, а копию
// из буфера соединения делает обработчик команды.
func BenchmarkSet(b *testing.B) {
	s, keys := benchStore(b)
	val := make([]byte, 20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Set(keys[i%benchKeys], val)
	}
}

// BenchmarkGCWith1MKeys — цена полной сборки мусора при миллионе живых ключей
// (значения по 64 байта): та самая статья, ради которой стояла своя арена.
// Отчёт — миллисекунды на один runtime.GC().
func BenchmarkGCWith1MKeys(b *testing.B) {
	s := New()
	for i := 0; i < 1_000_000; i++ {
		s.Set("key:"+strconv.Itoa(i), make([]byte, 64))
	}
	runtime.GC()
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		runtime.GC()
	}
	b.ReportMetric(float64(time.Since(start).Microseconds())/1000/float64(b.N), "ms/gc")
	runtime.KeepAlive(s)
}
