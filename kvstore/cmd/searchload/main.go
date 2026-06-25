// searchload — минимальный нативный нагрузчик VSIM.SEARCH для замера реального
// search-QPS сервера (без Python-узкого горла). Duration-based.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/protocol"
)

func main() {
	addr := flag.String("addr", "localhost:6391", "адрес сервера")
	conns := flag.Int("c", 50, "параллельных соединений")
	dim := flag.Int("dim", 768, "размерность запроса")
	k := flag.Int("k", 10, "K")
	dur := flag.Duration("d", 5*time.Second, "длительность")
	useBin := flag.Bool("bin", false, "VSIM.SEARCHBIN (бинарный протокол)")
	flag.Parse()

	// Предгенерим пул готовых RESP-команд (текст или бинарь).
	pool := make([][]protocol.Value, 256)
	for i := range pool {
		vec := make([]float32, *dim)
		for d := range vec {
			vec[d] = rand.Float32()
		}
		if *useBin {
			b := make([]byte, *dim*4)
			for j, v := range vec {
				binary.LittleEndian.PutUint32(b[j*4:], math.Float32bits(v))
			}
			pool[i] = []protocol.Value{
				{Typ: '$', Str: "VSIM.SEARCHBIN"},
				{Typ: '$', Str: strconv.Itoa(*k)},
				{Typ: '$', Str: string(b)},
			}
		} else {
			arr := make([]protocol.Value, *dim+2)
			arr[0] = protocol.Value{Typ: '$', Str: "VSIM.SEARCH"}
			arr[1] = protocol.Value{Typ: '$', Str: strconv.Itoa(*k)}
			for j, v := range vec {
				arr[j+2] = protocol.Value{Typ: '$', Str: strconv.FormatFloat(float64(v), 'f', -1, 32)}
			}
			pool[i] = arr
		}
	}

	var ops int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(*dur)
	for c := 0; c < *conns; c++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", *addr)
			if err != nil {
				return
			}
			defer conn.Close()
			w := protocol.NewWriter(conn)
			r := protocol.NewReader(conn)
			local := 0
			i := seed
			for time.Now().Before(deadline) {
				arr := pool[i&255]
				i++
				if w.Write(protocol.Value{Typ: '*', Array: arr}) != nil {
					return
				}
				if w.Flush() != nil {
					return
				}
				if _, err := r.Read(); err != nil {
					return
				}
				local++
			}
			atomic.AddInt64(&ops, int64(local))
		}(c * 7)
	}
	wg.Wait()
	elapsed := time.Since(deadline.Add(-*dur)).Seconds()
	qps := float64(ops) / elapsed
	fmt.Printf("SEARCH: conns=%d dim=%d K=%d dur=%.1fs ops=%d => %.0f QPS (lat~%.2fms/conn)\n",
		*conns, *dim, *k, elapsed, ops, qps, 1000.0*elapsed/(float64(ops)/float64(*conns)))
}
