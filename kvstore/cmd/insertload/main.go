// insertload — нативный нагрузчик VSIM.ADDBIN для замера реального insert-rate
// сервера через полный RESP-стек. Парный инструмент к cmd/searchload.
//
// Читает сырой float32 little-endian файл (N*dim), раздаёт векторы по -c
// соединениям страйдом и меряет стену от первой до последней вставки.
package main

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/protocol"

	"net"
)

func main() {
	addr := flag.String("addr", "localhost:6391", "адрес сервера")
	conns := flag.Int("c", 12, "параллельных соединений")
	dim := flag.Int("dim", 784, "размерность вектора")
	n := flag.Int("n", 60000, "сколько векторов вставить (0 = до конца файла)")
	start := flag.Int("start", 0, "с какого индекса файла начинать (для догрузки диапазона)")
	file := flag.String("file", "", "путь к raw float32 LE файлу (N*dim)")
	prefix := flag.String("prefix", "e2e", "префикс ключей")
	pipeline := flag.Int("pipeline", 1, "глубина RESP-пайплайна: сколько команд слать до чтения ответов (1 = строгий запрос-ответ)")
	flag.Parse()

	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	vecBytes := *dim * 4
	total := len(raw) / vecBytes
	if *n > 0 && *start+*n < total {
		total = *start + *n
	}

	var ops, errs int64
	var wg sync.WaitGroup
	t0 := time.Now()
	for c := 0; c < *conns; c++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", *addr)
			if err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			defer conn.Close()
			w := protocol.NewWriter(conn)
			r := protocol.NewReader(conn)
			// Пайплайн окном: шлём до -pipeline команд без ожидания, потом
			// вычитываем накопленные ответы. inflight — сколько ответов должны.
			inflight := 0
			drain := func() bool {
				for ; inflight > 0; inflight-- {
					if _, err := r.Read(); err != nil {
						atomic.AddInt64(&errs, 1)
						return false
					}
					atomic.AddInt64(&ops, 1)
				}
				return true
			}
			for i := *start + id; i < total; i += *conns {
				vec := raw[i*vecBytes : (i+1)*vecBytes]
				cmd := []protocol.Value{
					{Typ: '$', Str: "VSIM.ADDBIN"},
					{Typ: '$', Str: fmt.Sprintf("%s:%d", *prefix, i)},
					{Typ: '$', Str: string(vec)},
				}
				if w.Write(protocol.Value{Typ: '*', Array: cmd}) != nil {
					atomic.AddInt64(&errs, 1)
					return
				}
				inflight++
				if inflight >= *pipeline {
					if w.Flush() != nil {
						atomic.AddInt64(&errs, 1)
						return
					}
					if !drain() {
						return
					}
				}
			}
			if w.Flush() != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			drain()
		}(c)
	}
	wg.Wait()
	elapsed := time.Since(t0).Seconds()
	fmt.Printf("INSERT: conns=%d dim=%d ops=%d errs=%d dur=%.1fs => %.0f insert/s\n",
		*conns, *dim, atomic.LoadInt64(&ops), atomic.LoadInt64(&errs), elapsed,
		float64(atomic.LoadInt64(&ops))/elapsed)
}
