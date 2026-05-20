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

// ─────────────────────────────────────────────
// Сравнение производительности Go vs Rust WASM
// ─────────────────────────────────────────────
//
// Этот тест автоматически переключает движки и сравнивает их на dim=128 и dim=768.
//
// Запуск:
//   1. Запусти сервер: ./kvstore_bin
//   2. go test ./kvstore/vector/ -run TestCompareEnginesRPS -v -count=1

func TestCompareEnginesRPS(t *testing.T) {
	addr := "localhost:6380"

	// Проверяем, запущен ли сервер
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Skip("Server not running on :6380, skipping comparative RPS benchmark")
	}
	conn.Close()

	dimensions := []int{128, 768}
	numVectors := 1000
	benchDuration := 3 * time.Second
	concurrency := 24

	t.Logf("═══ Automated Go vs Rust WASM Performance Comparison ═══")
	t.Logf("Server: %s", addr)
	t.Logf("Vectors: %d, Concurrency: %d, Duration per run: %v", numVectors, concurrency, benchDuration)

	type Result struct {
		Dim     int
		GoRPS   int64
		RustRPS int64
	}
	var results []Result

	for _, dim := range dimensions {
		t.Logf("\n=================== Dimension %d ===================", dim)

		// 1. Сбрасываем движок на Go
		t.Log("Setting engine to Go (0)...")
		sendSimpleCommand(t, addr, "*2\r\n$14\r\nVSIM.SETENGINE\r\n$1\r\n0\r\n")

		// 2. Очищаем старые векторы
		t.Log("Cleaning old vectors...")
		for i := 0; i < numVectors; i++ {
			key := fmt.Sprintf("vec:%d", i)
			sendSimpleCommand(t, addr, fmt.Sprintf("*2\r\n$8\r\nVSIM.DEL\r\n$%d\r\n%s\r\n", len(key), key))
		}

		// 3. Загружаем новые векторы
		t.Logf("Loading %d vectors of dimension %d...", numVectors, dim)
		loadStart := time.Now()
		loadVectors(t, addr, numVectors, dim)
		t.Logf("Loaded vectors in %v", time.Since(loadStart))

		// 4. Тестируем Go Engine
		t.Log("Benchmarking Go Engine (Hybrid WASM SIMD distance calculations)...")
		searchCmd := buildSearchCommand(dim, 10)
		goRPS := benchmarkCommand(t, addr, benchDuration, concurrency, func(c net.Conn) error {
			_, err := c.Write(searchCmd)
			if err != nil {
				return err
			}
			buf := make([]byte, 4096)
			_, err = c.Read(buf)
			return err
		})
		t.Logf("Go Engine: %d RPS", goRPS)

		// 5. Переключаемся на Rust WASM Engine
		t.Log("Switching to Rust WASM Engine (1)...")
		switchStart := time.Now()
		sendSimpleCommand(t, addr, "*2\r\n$14\r\nVSIM.SETENGINE\r\n$1\r\n1\r\n")
		t.Logf("Switch and synchronization of %d vectors completed in %v", numVectors, time.Since(switchStart))

		// 6. Тестируем Rust WASM Engine
		t.Log("Benchmarking Rust WASM Engine (Full Rust HNSW inside WASM)...")
		rustRPS := benchmarkCommand(t, addr, benchDuration, concurrency, func(c net.Conn) error {
			_, err := c.Write(searchCmd)
			if err != nil {
				return err
			}
			buf := make([]byte, 4096)
			_, err = c.Read(buf)
			return err
		})
		t.Logf("Rust WASM Engine: %d RPS", rustRPS)

		results = append(results, Result{
			Dim:     dim,
			GoRPS:   goRPS,
			RustRPS: rustRPS,
		})
	}

	// Выводим красивую итоговую таблицу
	t.Log("\n# Comparative Benchmark Results (RPS)")
	t.Log("| Vector Dimension | Go Engine (Hybrid) | Rust WASM Engine (Full HNSW) | Performance Gain |")
	t.Log("|-------------------|--------------------|-----------------------------|------------------|")
	for _, r := range results {
		gain := float64(r.RustRPS) / float64(r.GoRPS)
		t.Logf("| %17d | %18d | %27d | %15.2fx |", r.Dim, r.GoRPS, r.RustRPS, gain)
	}

	// Сбрасываем движок обратно на Go в конце
	sendSimpleCommand(t, addr, "*2\r\n$14\r\nVSIM.SETENGINE\r\n$1\r\n0\r\n")
}

func sendSimpleCommand(t *testing.T, addr string, cmd string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Failed to connect to server: %v", err)
	}
	defer conn.Close()
	_, err = conn.Write([]byte(cmd))
	if err != nil {
		t.Fatalf("Failed to write command: %v", err)
	}
	buf := make([]byte, 256)
	_, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}
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
