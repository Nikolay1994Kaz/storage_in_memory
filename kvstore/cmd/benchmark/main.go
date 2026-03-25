package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/protocol"
)

func main() {
	// Параметры теста через флаги
	numConns := flag.Int("c", 50, "количество параллельных соединений")
	numRequests := flag.Int("n", 100000, "общее количество запросов")
	addr := flag.String("addr", "localhost:6380", "адрес сервера")
	valueSize := flag.Int("size", 100, "размер значения в байтах")
	flag.Parse()

	fmt.Printf("=== KVStore Benchmark ===\n")
	fmt.Printf("Connections: %d\n", *numConns)
	fmt.Printf("Requests:    %d\n", *numRequests)
	fmt.Printf("Value size:  %d bytes\n", *valueSize)
	fmt.Printf("Server:      %s\n\n", *addr)

	value := make([]byte, *valueSize)
	for i := range value {
		value[i] = 'x'
	}

	// --- Бенчмарк SET ---
	fmt.Println("--- SET ---")
	runBenchmark(*addr, *numConns, *numRequests, func(conn net.Conn, id int) {
		key := fmt.Sprintf("key:%d", id)
		doSet(conn, key, value)
	})

	// --- Бенчмарк GET ---
	fmt.Println("--- GET ---")
	runBenchmark(*addr, *numConns, *numRequests, func(conn net.Conn, id int) {
		key := fmt.Sprintf("key:%d", rand.Intn(*numRequests))
		doGet(conn, key)
	})

	// --- Бенчмарк MIXED (80% GET / 20% SET) ---
	fmt.Println("--- MIXED (80% GET / 20% SET) ---")
	runBenchmark(*addr, *numConns, *numRequests, func(conn net.Conn, id int) {
		key := fmt.Sprintf("key:%d", rand.Intn(*numRequests))
		if rand.Intn(100) < 80 {
			doGet(conn, key)
		} else {
			doSet(conn, key, value)
		}
	})
}

func runBenchmark(addr string, numConns, numRequests int, work func(net.Conn, int)) {
	var (
		wg        sync.WaitGroup
		completed atomic.Int64
		errors    atomic.Int64
	)

	perConn := numRequests / numConns

	// Собираем латентности со всех горутин
	latencies := make([]time.Duration, numRequests)

	start := time.Now()

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func(connID int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				fmt.Printf("Connection failed: %v\n", err)
				errors.Add(int64(perConn))
				return
			}
			defer conn.Close()

			for j := 0; j < perConn; j++ {
				reqID := connID*perConn + j

				reqStart := time.Now()
				work(conn, reqID)
				latencies[reqID] = time.Since(reqStart)

				completed.Add(1)
			}
		}(i)
	}

	// Прогресс-бар
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(500 * time.Millisecond):
				c := completed.Load()
				pct := float64(c) / float64(numRequests) * 100
				fmt.Printf("\r  Progress: %d/%d (%.1f%%)", c, numRequests, pct)
			}
		}
	}()

	wg.Wait()
	close(done)

	elapsed := time.Since(start)
	total := completed.Load()
	errCount := errors.Load()
	rps := float64(total) / elapsed.Seconds()

	// Считаем перцентили
	sort.Slice(latencies[:total], func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	p50 := latencies[total*50/100]
	p95 := latencies[total*95/100]
	p99 := latencies[total*99/100]

	fmt.Printf("\r  Completed: %d requests in %v\n", total, elapsed.Round(time.Millisecond))
	fmt.Printf("  Errors:    %d\n", errCount)
	fmt.Printf("  RPS:       %.0f req/sec\n", rps)
	fmt.Printf("  Latency p50: %v\n", p50)
	fmt.Printf("  Latency p95: %v\n", p95)
	fmt.Printf("  Latency p99: %v\n\n", p99)
}

// doSet отправляет SET key value и читает ответ.
func doSet(conn net.Conn, key string, value []byte) error {
	writer := protocol.NewWriter(conn)
	reader := protocol.NewReader(conn)

	// Формируем RESP-команду: *3\r\n$3\r\nSET\r\n$...\r\nkey\r\n$...\r\nvalue\r\n
	cmd := protocol.Value{
		Typ: '*',
		Array: []protocol.Value{
			{Typ: '$', Str: "SET"},
			{Typ: '$', Str: key},
			{Typ: '$', Str: string(value)},
		},
	}

	if err := writer.Write(cmd); err != nil {
		return err
	}

	_, err := reader.Read() // читаем ответ +OK
	return err
}

// doGet отправляет GET key и читает ответ.
func doGet(conn net.Conn, key string) error {
	writer := protocol.NewWriter(conn)
	reader := protocol.NewReader(conn)

	cmd := protocol.Value{
		Typ: '*',
		Array: []protocol.Value{
			{Typ: '$', Str: "GET"},
			{Typ: '$', Str: key},
		},
	}

	if err := writer.Write(cmd); err != nil {
		return err
	}

	_, err := reader.Read()
	return err
}
