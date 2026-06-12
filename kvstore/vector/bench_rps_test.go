package vector

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─────────────────────────────────────────────
// Сетевой бенчмарк: реальный RPS через RESP протокол
// ─────────────────────────────────────────────
//
// Этот тест подключается к ЗАПУЩЕННОМУ серверу (localhost:6380)
// и измеряет реальный throughput через TCP.
//
// Запуск:
//   1. Запусти сервер: ./kvstore_bin
//   2. go test ./kvstore/vector/ -run TestRPSBenchmark -v -count=1

func TestRPSBenchmark(t *testing.T) {
	addr := "localhost:6380"

	// Проверяем, запущен ли сервер
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Skip("Server not running on :6380, skipping RPS benchmark")
	}
	conn.Close()

	const dim = 32
	const numVectors = 5000
	const benchDuration = 5 * time.Second

	t.Logf("═══ RPS Benchmark ═══")
	t.Logf("Server: %s", addr)
	t.Logf("Vectors: %d, Dimension: %d", numVectors, dim)

	// ─── Шаг 1: Загружаем вектора ───
	t.Log("\n--- Loading vectors ---")
	loadStart := time.Now()
	loadVectors(t, addr, numVectors, dim)
	t.Logf("Loaded %d vectors in %v", numVectors, time.Since(loadStart))

	// ─── Шаг 2: Бенчмарк PING ───
	t.Log("\n--- Benchmark: PING ---")
	pingRPS := benchmarkCommand(t, addr, benchDuration, 50, func(conn net.Conn) error {
		_, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n"))
		if err != nil {
			return err
		}
		buf := make([]byte, 64)
		_, err = conn.Read(buf)
		return err
	})
	t.Logf("PING: %d RPS", pingRPS)

	// ─── Шаг 3: Бенчмарк SET ───
	t.Log("\n--- Benchmark: SET ---")
	setRPS := benchmarkCommand(t, addr, benchDuration, 50, func(conn net.Conn) error {
		key := fmt.Sprintf("bench:%d", rand.Int63())
		cmd := fmt.Sprintf("*3\r\n$3\r\nSET\r\n$%d\r\n%s\r\n$5\r\nhello\r\n", len(key), key)
		_, err := conn.Write([]byte(cmd))
		if err != nil {
			return err
		}
		buf := make([]byte, 64)
		_, err = conn.Read(buf)
		return err
	})
	t.Logf("SET: %d RPS", setRPS)

	// ─── Шаг 4: Бенчмарк GET ───
	t.Log("\n--- Benchmark: GET ---")
	getRPS := benchmarkCommand(t, addr, benchDuration, 50, func(conn net.Conn) error {
		cmd := "*2\r\n$3\r\nGET\r\n$7\r\nbench:1\r\n"
		_, err := conn.Write([]byte(cmd))
		if err != nil {
			return err
		}
		buf := make([]byte, 256)
		_, err = conn.Read(buf)
		return err
	})
	t.Logf("GET: %d RPS", getRPS)

	// ─── Шаг 5: Бенчмарк VSIM.SEARCH (K=5, dim=32) ───
	t.Log("\n--- Benchmark: VSIM.SEARCH (K=5) ---")

	// Создаём RESP-команду для поиска заранее
	searchCmd := buildSearchCommand(dim, 5)

	searchRPS := benchmarkCommand(t, addr, benchDuration, 50, func(conn net.Conn) error {
		_, err := conn.Write(searchCmd)
		if err != nil {
			return err
		}
		buf := make([]byte, 4096)
		_, err = conn.Read(buf)
		return err
	})
	t.Logf("VSIM.SEARCH (K=5, dim=%d, %d vectors): %d RPS", dim, numVectors, searchRPS)

	// ─── Шаг 6: Бенчмарк VSIM.SEARCH (K=10, dim=32) ───
	t.Log("\n--- Benchmark: VSIM.SEARCH (K=10) ---")
	searchCmd10 := buildSearchCommand(dim, 10)

	searchRPS10 := benchmarkCommand(t, addr, benchDuration, 50, func(conn net.Conn) error {
		_, err := conn.Write(searchCmd10)
		if err != nil {
			return err
		}
		buf := make([]byte, 4096)
		_, err = conn.Read(buf)
		return err
	})
	t.Logf("VSIM.SEARCH (K=10, dim=%d, %d vectors): %d RPS", dim, numVectors, searchRPS10)

	// ─── Итоги ───
	t.Log("\n═══════════════════════════════════════")
	t.Logf("  PING:            %6d RPS", pingRPS)
	t.Logf("  SET:             %6d RPS", setRPS)
	t.Logf("  GET:             %6d RPS", getRPS)
	t.Logf("  VSIM.SEARCH K=5: %6d RPS", searchRPS)
	t.Logf("  VSIM.SEARCH K=10:%6d RPS", searchRPS10)
	t.Log("═══════════════════════════════════════")
}



// benchmarkCommand запускает concurrency горутин, каждая шлёт команды в цикле duration.
// Возвращает итоговый RPS.
func benchmarkCommand(t *testing.T, addr string, duration time.Duration, concurrency int, fn func(net.Conn) error) int64 {
	var totalOps atomic.Int64
	var wg sync.WaitGroup

	deadline := time.Now().Add(duration)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Logf("connect error: %v", err)
				return
			}
			defer conn.Close()

			for time.Now().Before(deadline) {
				if err := fn(conn); err != nil {
					return
				}
				totalOps.Add(1)
			}
		}()
	}

	wg.Wait()
	ops := totalOps.Load()
	rps := ops / int64(duration.Seconds())
	return rps
}

// loadVectors загружает N векторов через RESP
func loadVectors(t *testing.T, addr string, count, dim int) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("connect error: %v", err)
	}
	defer conn.Close()

	rng := rand.New(rand.NewSource(42))
	buf := make([]byte, 4096)

	for i := 0; i < count; i++ {
		key := fmt.Sprintf("vec:%d", i)

		// Строим RESP массив: *N\r\n$8\r\nVSIM.ADD\r\n$K\r\nkey\r\n$...\r\nfloat\r\n...
		cmd := fmt.Sprintf("*%d\r\n$8\r\nVSIM.ADD\r\n$%d\r\n%s\r\n", dim+2, len(key), key)
		for j := 0; j < dim; j++ {
			val := fmt.Sprintf("%.6f", rng.Float32())
			cmd += fmt.Sprintf("$%d\r\n%s\r\n", len(val), val)
		}

		_, err := conn.Write([]byte(cmd))
		if err != nil {
			t.Fatalf("write error at vector %d: %v", i, err)
		}

		// Читаем ответ
		_, err = conn.Read(buf)
		if err != nil {
			t.Fatalf("read error at vector %d: %v", i, err)
		}
	}
}

// buildSearchCommand строит RESP-команду для VSIM.SEARCH
func buildSearchCommand(dim, K int) []byte {
	rng := rand.New(rand.NewSource(99))
	kStr := fmt.Sprintf("%d", K)
	cmd := fmt.Sprintf("*%d\r\n$11\r\nVSIM.SEARCH\r\n$%d\r\n%s\r\n", dim+2, len(kStr), kStr)
	for j := 0; j < dim; j++ {
		val := fmt.Sprintf("%.6f", rng.Float32())
		cmd += fmt.Sprintf("$%d\r\n%s\r\n", len(val), val)
	}
	return []byte(cmd)
}
