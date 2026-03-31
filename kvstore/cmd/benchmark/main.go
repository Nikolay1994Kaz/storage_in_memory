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
	runBenchmark(*addr, *numConns, *numRequests, func(r *protocol.Reader, w *protocol.Writer, id int) {
		key := fmt.Sprintf("key:%d", id)
		sendCommand(w, "SET", key, string(value))
		r.Read() // +OK
	})

	// --- Бенчмарк GET ---
	fmt.Println("--- GET ---")
	runBenchmark(*addr, *numConns, *numRequests, func(r *protocol.Reader, w *protocol.Writer, id int) {
		key := fmt.Sprintf("key:%d", rand.Intn(*numRequests))
		sendCommand(w, "GET", key)
		r.Read() // $value или $-1
	})

	// --- Бенчмарк MIXED (80% GET / 20% SET) ---
	fmt.Println("--- MIXED (80% GET / 20% SET) ---")
	runBenchmark(*addr, *numConns, *numRequests, func(r *protocol.Reader, w *protocol.Writer, id int) {
		key := fmt.Sprintf("key:%d", rand.Intn(*numRequests))
		if rand.Intn(100) < 80 {
			sendCommand(w, "GET", key)
		} else {
			sendCommand(w, "SET", key, string(value))
		}
		r.Read()
	})

	// --- Бенчмарк SET EX (TTL) ---
	// SET key value EX 60 — каждый ключ живёт 60 секунд.
	// Измеряем overhead добавления TTL по сравнению с обычным SET.
	fmt.Println("--- SET EX (TTL) ---")
	runBenchmark(*addr, *numConns, *numRequests, func(r *protocol.Reader, w *protocol.Writer, id int) {
		key := fmt.Sprintf("ttl_key:%d", id)
		sendCommand(w, "SET", key, string(value), "EX", "60")
		r.Read() // +OK
	})

	// --- Бенчмарк TTL command ---
	// Предварительно создаём 1000 ключей с TTL, потом гоняем TTL-запросы.
	// Это тестирует скорость чтения из expires map.
	fmt.Println("--- TTL command ---")
	prepareKeysWithTTL(*addr, 1000)
	runBenchmark(*addr, *numConns, *numRequests, func(r *protocol.Reader, w *protocol.Writer, id int) {
		key := fmt.Sprintf("ttl_check:%d", rand.Intn(1000))
		sendCommand(w, "TTL", key)
		r.Read() // :seconds
	})

	// --- Бенчмарк EXPIRE ---
	// Устанавливаем TTL на существующие ключи (созданные в SET-бенчмарке).
	fmt.Println("--- EXPIRE ---")
	runBenchmark(*addr, *numConns, *numRequests, func(r *protocol.Reader, w *protocol.Writer, id int) {
		key := fmt.Sprintf("key:%d", rand.Intn(*numRequests))
		sendCommand(w, "EXPIRE", key, "120")
		r.Read() // :1 или :0
	})

	// --- Бенчмарк Pub/Sub ---
	// Отдельная функция — у Pub/Sub совсем другая модель:
	// подписчики ПОЛУЧАЮТ данные, а не запрашивают.
	fmt.Println("--- PUB/SUB (fan-out) ---")
	runPubSubBenchmark(*addr, 10, *numRequests/10)
}

// ==========================================================================
// sendCommand — универсальный отправщик RESP-команд.
// Принимает любое количество аргументов и формирует массив:
//
//	*N\r\n$len\r\narg1\r\n$len\r\narg2\r\n...
//
// Зачем: убираем дублирование. Раньше doSet и doGet строили
// protocol.Value вручную. Теперь одна функция для всех команд.
// ==========================================================================
func sendCommand(w *protocol.Writer, args ...string) error {
	arr := make([]protocol.Value, len(args))
	for i, a := range args {
		arr[i] = protocol.Value{Typ: '$', Str: a}
	}
	return w.Write(protocol.Value{Typ: '*', Array: arr})
}

// ==========================================================================
// prepareKeysWithTTL — подготовка данных для TTL-бенчмарка.
// Создаём n ключей с TTL=300 секунд, чтобы они точно не протухли
// за время бенчмарка. Одно соединение, последовательная запись.
// ==========================================================================
func prepareKeysWithTTL(addr string, n int) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Printf("Prepare TTL keys failed: %v\n", err)
		return
	}
	defer conn.Close()

	w := protocol.NewWriter(conn)
	r := protocol.NewReader(conn)

	for i := 0; i < n; i++ {
		sendCommand(w, "SET", fmt.Sprintf("ttl_check:%d", i), "val", "EX", "300")
		r.Read()
	}
	fmt.Printf("  Prepared %d keys with TTL\n", n)
}

// ==========================================================================
// runBenchmark — общий движок бенчмарка.
//
// КЛЮЧЕВОЕ ИЗМЕНЕНИЕ: work-функция принимает Reader и Writer,
// которые создаются ОДИН РАЗ при открытии соединения.
//
// Было:  doSet(conn, key, value) → внутри создаёт NewReader + NewWriter
// Стало: work(reader, writer, id) → переиспользует готовые reader/writer
//
// Это убирает аллокацию bufio.Reader (4KB) на каждый запрос.
// При 100K запросов = 400MB GC-мусора → 0.
// ==========================================================================
func runBenchmark(addr string, numConns, numRequests int, work func(r *protocol.Reader, w *protocol.Writer, id int)) {
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

			// Reader и Writer создаются ОДИН РАЗ на соединение.
			// bufio.Reader (4KB буфер) переиспользуется между запросами.
			reader := protocol.NewReader(conn)
			writer := protocol.NewWriter(conn)

			for j := 0; j < perConn; j++ {
				reqID := connID*perConn + j

				reqStart := time.Now()
				work(reader, writer, reqID)
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

// ==========================================================================
// runPubSubBenchmark — бенчмарк Pub/Sub (fan-out).
//
// Pub/Sub ПРИНЦИПИАЛЬНО отличается от SET/GET:
//
//	SET/GET: клиент отправляет → сервер отвечает (request-response)
//	PubSub:  publisher отправляет → сервер РАССЫЛАЕТ всем подписчикам (fan-out)
//
// Поэтому мы не можем использовать runBenchmark — нужна своя логика:
//  1. Создаём N subscriber connections → SUBSCRIBE "bench:pubsub"
//  2. Ждём, пока все подписались
//  3. Publisher отправляет M сообщений → PUBLISH "bench:pubsub" "msg"
//  4. Каждый подписчик читает сообщения и считает
//  5. "__DONE__" — маркер завершения для подписчиков
//
// Метрики:
//   - Publish RPS: сколько PUBLISH/sec обрабатывает Hub
//   - Fan-out RPS: сколько доставок/sec (publish × subscribers)
//   - Delivery rate: % доставленных сообщений от ожидаемых
//
// ==========================================================================
func runPubSubBenchmark(addr string, numSubscribers, numMessages int) {
	channel := "bench:pubsub"

	// Счётчик полученных сообщений. atomic — потому что
	// N горутин-подписчиков инкрементят параллельно.
	var received atomic.Int64
	var wg sync.WaitGroup

	// === ШАГ 1: Запускаем подписчиков ===
	// Каждый подписчик — отдельное TCP-соединение + горутина.
	for i := 0; i < numSubscribers; i++ {
		wg.Add(1)
		go func(subID int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				fmt.Printf("  Subscriber %d: connect failed: %v\n", subID, err)
				return
			}
			defer conn.Close()

			writer := protocol.NewWriter(conn)
			reader := protocol.NewReader(conn)

			// Отправляем SUBSCRIBE — после этого сервер начинает
			// пушить сообщения в это соединение через writePump.
			sendCommand(writer, "SUBSCRIBE", channel)

			// Читаем подтверждение: *3\r\n$9\r\nsubscribe\r\n$...\r\n:1\r\n
			reader.Read()

			// Читаем сообщения в бесконечном цикле.
			// Формат: *3\r\n$7\r\nmessage\r\n$channel\r\n$payload\r\n
			for {
				val, err := reader.Read()
				if err != nil {
					return // соединение закрыто
				}

				// Проверяем: это message (не subscribe confirmation)?
				if val.Typ == '*' && len(val.Array) >= 3 && val.Array[0].Str == "message" {
					// Маркер завершения — publisher сигнализирует "хватит"
					if val.Array[2].Str == "__DONE__" {
						return
					}
					received.Add(1)
				}
			}
		}(i)
	}

	// === ШАГ 2: Ждём подключения подписчиков ===
	// 200ms достаточно для TCP handshake + SUBSCRIBE + confirmation.
	// В продакшене использовали бы barrier/condition, здесь допустимо.
	time.Sleep(200 * time.Millisecond)

	// === ШАГ 3: Publisher отправляет сообщения ===
	pubConn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Printf("  Publisher: connect failed: %v\n", err)
		return
	}

	pubWriter := protocol.NewWriter(pubConn)
	pubReader := protocol.NewReader(pubConn)

	start := time.Now()

	for i := 0; i < numMessages; i++ {
		msg := fmt.Sprintf("msg_%d", i)
		sendCommand(pubWriter, "PUBLISH", channel, msg)
		pubReader.Read() // :N — количество подписчиков, получивших сообщение
	}

	// === ШАГ 4: Маркер завершения ===
	// Без этого подписчики повиснут в Read() навечно.
	// "__DONE__" — конвенция: подписчик видит его и выходит из цикла.
	sendCommand(pubWriter, "PUBLISH", channel, "__DONE__")
	pubReader.Read()
	pubConn.Close()

	// Ждём пока все подписчики завершатся
	wg.Wait()
	elapsed := time.Since(start)

	// === ШАГ 5: Статистика ===
	totalExpected := int64(numMessages) * int64(numSubscribers)
	totalReceived := received.Load()
	rps := float64(numMessages) / elapsed.Seconds()
	fanoutRPS := float64(totalReceived) / elapsed.Seconds()

	fmt.Printf("\r  Subscribers:         %d\n", numSubscribers)
	fmt.Printf("  Messages sent:       %d\n", numMessages)
	fmt.Printf("  Expected deliveries: %d\n", totalExpected)
	fmt.Printf("  Actual deliveries:   %d (%.1f%%)\n", totalReceived,
		float64(totalReceived)/float64(totalExpected)*100)
	fmt.Printf("  Duration:            %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  Publish RPS:         %.0f msg/sec\n", rps)
	fmt.Printf("  Fan-out RPS:         %.0f deliveries/sec\n\n", fanoutRPS)
}
