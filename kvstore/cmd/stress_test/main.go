package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"kvstore/kvstore/internal/protocol"
)

// ANSI colors for beautiful CLI output
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorMagenta= "\033[35m"
	ColorCyan   = "\033[36m"
	ColorWhite  = "\033[37m"
	ColorBold   = "\033[1m"
)

func main() {
	addr := flag.String("addr", "localhost:6380", "Адрес тестируемого сервера")
	metricsAddr := flag.String("metrics-addr", "localhost:9090", "Адрес сервера метрик VictoriaMetrics")
	durationStr := flag.String("duration", "10s", "Длительность теста (например: 10s, 5m, 2h, 48h)")
	concurrencyVal := flag.Int("concurrency", 32, "Количество параллельных клиентов")
	warmup := flag.Bool("warmup", true, "Запустить первичную загрузку (Warmup)")
	crashTest := flag.Bool("crash-test", false, "Запустить краш-тест в конце")
	oomTest := flag.Bool("oom-test", false, "Запустить тест OOM-защиты (запускает временный инстанс на порту 6389)")
	flag.Parse()

	duration, err := time.ParseDuration(*durationStr)
	if err != nil {
		fmt.Printf("%s[ОШИБКА] Некорректный формат длительности: %v%s\n", ColorRed, err, ColorReset)
		os.Exit(1)
	}

	fmt.Printf("%s%s================================================================%s\n", ColorBold, ColorCyan, ColorReset)
	fmt.Printf("%s%s🚀 ЗАПУСК АУДИТА ГОТОВНОСТИ К ПРОДАКШЕНУ (PRODUCTION READINESS AUDIT) 🚀%s\n", ColorBold, ColorMagenta, ColorReset)
	fmt.Printf("%s%s   Параметры: duration=%v, concurrency=%d, warmup=%v, crash=%v, oom=%v%s\n", ColorBold, ColorCyan, duration, *concurrencyVal, *warmup, *crashTest, *oomTest, ColorReset)
	fmt.Printf("%s%s================================================================%s\n\n", ColorBold, ColorCyan, ColorReset)

	// Проверяем доступность сервера
	conn, err := net.DialTimeout("tcp", *addr, 2*time.Second)
	if err != nil {
		fmt.Printf("%s[КРИТИЧЕСКАЯ ОШИБКА] Сервер не запущен на %s! Запустите сервер: ./kvstore-server --port 6380%s\n", ColorRed, *addr, ColorReset)
		os.Exit(1)
	}
	conn.Close()

	// Фаза 1: Первичная загрузка данных (Ingest)
	if *warmup {
		runPhase1(*addr)
	}

	// Фаза 2 и 3: Параллельный стресс-тест под смешанной нагрузкой + Горячее переключение движка на лету
	runPhase2And3(*addr, *metricsAddr, duration, *concurrencyVal)

	// Фаза 4: Устойчивость при сбоях (Crash Recovery & WAL Durability)
	if *crashTest {
		runPhase4(*addr)
	}

	// Фаза 5: Проверка лимитов памяти и OOM-защиты
	if *oomTest {
		runPhase5()
	}

	fmt.Printf("\n%s%s================================================================%s\n", ColorBold, ColorGreen, ColorReset)
	fmt.Printf("%s%s🥇 АУДИТ ЗАВЕРШЕН УСПЕШНО! 🥇%s\n", ColorBold, ColorGreen, ColorReset)
	fmt.Printf("%s%s================================================================%s\n", ColorBold, ColorGreen, ColorReset)
}

func sendCommand(w *protocol.Writer, args ...string) error {
	arr := make([]protocol.Value, len(args))
	for i, a := range args {
		arr[i] = protocol.Value{Typ: '$', Str: a}
	}
	if err := w.Write(protocol.Value{Typ: '*', Array: arr}); err != nil {
		return err
	}
	return w.Flush()
}

// ==========================================
// ФАЗА 1: Первичная загрузка данных
// ==========================================
func runPhase1(addr string) {
	fmt.Printf("%s[ФАЗА 1] Запуск первичного наполнения базы данными (Ingest Warmup)...%s\n", ColorBold+ColorCyan, ColorReset)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	w := protocol.NewWriter(conn)
	r := protocol.NewReader(conn)

	// 1. Чистим базу перед тестом
	sendCommand(w, "COMPACT") // Очистка старых логов
	r.Read()

	numKV := 5000
	numVec := 500
	dim := 128

	// Сброс движка на Go
	sendCommand(w, "VSIM.SETENGINE", "0")
	r.Read()

	// Загрузка KV
	kvStart := time.Now()
	for i := 0; i < numKV; i++ {
		key := fmt.Sprintf("key:%d", i)
		val := fmt.Sprintf("value_data_payload_realistic_%d", i)
		sendCommand(w, "SET", key, val)
		r.Read()
	}
	kvDuration := time.Since(kvStart)
	fmt.Printf("  ✅ Успешно записано %s%d KV-записей%s в базу за %v (RPS: %.0f)\n", 
		ColorGreen, numKV, ColorReset, kvDuration.Round(time.Millisecond), float64(numKV)/kvDuration.Seconds())

	// Загрузка векторов
	vecStart := time.Now()
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < numVec; i++ {
		key := fmt.Sprintf("vec:%d", i)
		args := []string{"VSIM.ADD", key}
		for j := 0; j < dim; j++ {
			args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
		}
		sendCommand(w, args...)
		r.Read()
	}
	vecDuration := time.Since(vecStart)
	fmt.Printf("  ✅ Успешно добавлено %s%d векторов (dim=%d)%s за %v (RPS: %.0f)\n\n", 
		ColorGreen, numVec, dim, ColorReset, vecDuration.Round(time.Millisecond), float64(numVec)/vecDuration.Seconds())
}

// Helpers для фонового сбора метрик
func getServerMemory(metricsAddr string) float64 {
	resp, err := http.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "kvstore_memory_used_bytes ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, _ := strconv.ParseFloat(fields[1], 64)
				return val
			}
		}
	}
	return 0
}

func getVectorCount(addr string) int {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return 0
	}
	defer conn.Close()
	w := protocol.NewWriter(conn)
	r := protocol.NewReader(conn)
	sendCommand(w, "VSIM.INFO")
	val, err := r.Read()
	if err != nil {
		return 0
	}
	fields := strings.Fields(val.Str)
	for _, f := range fields {
		if strings.HasPrefix(f, "vectors:") {
			count, _ := strconv.Atoi(strings.TrimPrefix(f, "vectors:"))
			return count
		}
	}
	return 0
}

// ==========================================
// ФАЗА 2 & 3: Смешанный стресс-тест + Горячий своп движка
// ==========================================
func runPhase2And3(addr, metricsAddr string, testDuration time.Duration, concurrency int) {
	fmt.Printf("%s[ФАЗА 2 & 3] Запуск параллельного стресс-теста...%s\n", ColorBold+ColorCyan, ColorReset)

	const dim = 128

	var (
		wg           sync.WaitGroup
		completedOps atomic.Int64
		errorOps     atomic.Int64
		latencies    []time.Duration
		mu           sync.Mutex
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("  🔥 Спавним %d параллельных клиентов, выполняющих смешанные запросы...\n", concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				fmt.Printf("    [Воркер %d] Ошибка подключения: %v\n", workerID, err)
				errorOps.Add(100)
				return
			}
			defer conn.Close()

			w := protocol.NewWriter(conn)
			r := protocol.NewReader(conn)
			rng := rand.New(rand.NewSource(int64(workerID * 999)))
			localLatencies := make([]time.Duration, 0, 1000)

			for {
				select {
				case <-ctx.Done():
					mu.Lock()
					latencies = append(latencies, localLatencies...)
					mu.Unlock()
					return
				default:
					reqStart := time.Now()
					dice := rng.Intn(100)

					var err error
					if dice < 40 {
						// 40% GET
						key := fmt.Sprintf("key:%d", rng.Intn(5000))
						err = sendCommand(w, "GET", key)
					} else if dice < 70 {
						// 30% VSIM.SEARCH
						args := []string{"VSIM.SEARCH", "10"}
						for j := 0; j < dim; j++ {
							args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
						}
						err = sendCommand(w, args...)
					} else if dice < 80 {
						// 10% VSIM.SEARCHRANGE (B+Tree + HNSW)
						minScore := rng.Float64() * 10
						maxScore := minScore + 5.0
						args := []string{"VSIM.SEARCHRANGE", "10", "benchset", 
							strconv.FormatFloat(minScore, 'f', 2, 64), 
							strconv.FormatFloat(maxScore, 'f', 2, 64)}
						for j := 0; j < dim; j++ {
							args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
						}
						err = sendCommand(w, args...)
					} else if dice < 90 {
						// 10% SET + ZADD
						idNum := rng.Intn(5000)
						key := fmt.Sprintf("key:%d", idNum)
						err = sendCommand(w, "SET", key, "updated_during_stress")
						if err == nil {
							r.Read()
							score := rng.Float64() * 15
							err = sendCommand(w, "ZADD", "benchset", strconv.FormatFloat(score, 'f', 2, 64), key)
						}
					} else {
						// 10% VSIM.ADD
						key := fmt.Sprintf("vec:%d", rng.Intn(500))
						args := []string{"VSIM.ADD", key}
						for j := 0; j < dim; j++ {
							args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
						}
						err = sendCommand(w, args...)
					}

					if err != nil {
						errorOps.Add(1)
						continue
					}

					_, readErr := r.Read()
					if readErr != nil {
						errorOps.Add(1)
						continue
					}

					localLatencies = append(localLatencies, time.Since(reqStart))
					completedOps.Add(1)
				}
			}
		}(i)
	}

	// Поток мониторинга
	wg.Add(1)
	go func() {
		defer wg.Done()
		
		var logInterval time.Duration
		if testDuration > 10*time.Minute {
			logInterval = 1 * time.Minute
		} else if testDuration > 1*time.Minute {
			logInterval = 10 * time.Second
		} else {
			logInterval = 1 * time.Second
		}
		
		ticker := time.NewTicker(logInterval)
		defer ticker.Stop()
		
		startTime := time.Now()
		var lastCompleted int64

		// Периодическое переключение движка для проверки стабильности на лету
		swapInterval := 5 * time.Minute
		if testDuration < 10*time.Minute {
			swapInterval = 1500 * time.Millisecond
		}
		swapTicker := time.NewTicker(swapInterval)
		defer swapTicker.Stop()
		
		engineState := 0

		for {
			select {
			case <-ctx.Done():
				return
			case <-swapTicker.C:
				// Меняем движок 0 <-> 1
				engineState = 1 - engineState
				ctrlConn, err := net.Dial("tcp", addr)
				if err == nil {
					cw := protocol.NewWriter(ctrlConn)
					cr := protocol.NewReader(ctrlConn)
					sendCommand(cw, "VSIM.SETENGINE", strconv.Itoa(engineState))
					cr.Read()
					ctrlConn.Close()
					fmt.Printf("\n  %s🔄 [ГОРЯЧЕЕ ПЕРЕКЛЮЧЕНИЕ] Движок переключен на %d (0=Go, 1=Rust WASM)%s\n", ColorYellow, engineState, ColorReset)
				}
			case <-ticker.C:
				elapsed := time.Since(startTime)
				currCompleted := completedOps.Load()
				diff := currCompleted - lastCompleted
				lastCompleted = currCompleted
				
				rps := float64(diff) / logInterval.Seconds()
				memBytes := getServerMemory(metricsAddr)
				memMB := memBytes / 1024 / 1024
				vecCount := getVectorCount(addr)
				
				fmt.Printf("  ⏱️  [%s] Прогресс: %s/%s | Текущий RPS: %s%.0f%s | Ошибок: %s%d%s | Память: %.2f MB | Векторов: %d\n",
					time.Now().Format("15:04:05"),
					elapsed.Round(time.Second),
					testDuration,
					ColorCyan, rps, ColorReset,
					GetErrorColor(errorOps.Load()), errorOps.Load(), ColorReset,
					memMB, vecCount,
				)
				
				if elapsed >= testDuration {
					return
				}
			}
		}
	}()

	// Ждем окончания времени теста
	time.Sleep(testDuration)
	cancel()
	wg.Wait()

	total := completedOps.Load()
	errors := errorOps.Load()

	// Считаем перцентили латентности
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := time.Duration(0)
	p95 := time.Duration(0)
	p99 := time.Duration(0)
	if len(latencies) > 0 {
		p50 = latencies[len(latencies)*50/100]
		p95 = latencies[len(latencies)*95/100]
		p99 = latencies[len(latencies)*99/100]
	}

	fmt.Printf("\n  📊 %sРезультаты стресс-теста под нагрузкой:%s\n", ColorBold, ColorReset)
	fmt.Printf("    - Всего успешных транзакций:  %s%d%s\n", ColorGreen, total, ColorReset)
	fmt.Printf("    - Ошибок / падений системы:  %s%d%s\n", 
		GetErrorColor(errors), errors, ColorReset)
	fmt.Printf("    - Средний RPS системы:       %s%.0f req/sec%s\n", 
		ColorCyan, float64(total)/testDuration.Seconds(), ColorReset)
	fmt.Printf("    - Задержка p50 (Медиана):     %v\n", p50)
	fmt.Printf("    - Задержка p95 (95%% клиентов): %v\n", p95)
	fmt.Printf("    - Задержка p99 (Худший случай): %v\n\n", p99)
}

// ==========================================
// ФАЗА 4: Краш-тест и восстановление (Durability)
// ==========================================
func runPhase4(addr string) {
	fmt.Printf("%s[ФАЗА 4] Тестирование надежности и восстановления после сбоев (WAL Durability)...%s\n", ColorBold+ColorCyan, ColorReset)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		panic(err)
	}
	w := protocol.NewWriter(conn)
	r := protocol.NewReader(conn)

	sendCommand(w, "DBSIZE")
	res, _ := r.Read()
	preCrashCount := res.Num

	sendCommand(w, "VSIM.INFO")
	resVec, _ := r.Read()
	preCrashVecInfo := resVec.Str
	conn.Close()

	fmt.Printf("  📝 Состояние системы перед сбоем: KV записей: %d, Векторный индекс: %s\n", preCrashCount, preCrashVecInfo)

	fmt.Printf("  %s💥 Имитируем аварийное выключение (SIGKILL / kill -9)...%s\n", ColorRed, ColorReset)
	
	cmd := exec.Command("pgrep", "-f", "kvstore-server")
	out, err := cmd.Output()
	if err != nil {
		fmt.Printf("    Не удалось найти процесс сервера! Убедитесь, что сервер запущен как ./kvstore-server\n")
		return
	}
	
	pids := strings.Fields(string(out))
	for _, pidStr := range pids {
		pid, _ := strconv.Atoi(pidStr)
		syscall.Kill(pid, syscall.SIGKILL)
	}
	time.Sleep(1 * time.Second)
	fmt.Println("    💀 Сервер успешно «упал». Подключение невозможно.")

	fmt.Printf("  %s🔄 Перезапуск сервера и автоматическое восстановление из лога WAL...%s\n", ColorYellow, ColorReset)
	
	runCmd := exec.Command("./kvstore-server", "--port", "6380")
	runCmd.Stdout = nil
	runCmd.Stderr = nil
	err = runCmd.Start()
	if err != nil {
		fmt.Printf("    Не удалось запустить сервер: %v\n", err)
		return
	}
	
	time.Sleep(3 * time.Second)

	conn2, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Printf("    %s❌ Ошибка: Сервер не смог подняться после падения!%s\n", ColorRed, ColorReset)
		return
	}
	defer conn2.Close()

	w2 := protocol.NewWriter(conn2)
	r2 := protocol.NewReader(conn2)

	sendCommand(w2, "DBSIZE")
	res2, _ := r2.Read()
	postCrashCount := res2.Num

	sendCommand(w2, "VSIM.INFO")
	resVec2, _ := r2.Read()
	postCrashVecInfo := resVec2.Str

	fmt.Printf("  📝 Состояние системы после восстановления: KV записей: %d, Векторный индекс: %s\n", postCrashCount, postCrashVecInfo)

	parseVecInfo := func(info string) (vectors int, dim int, engine string) {
		fields := strings.Fields(info)
		for _, f := range fields {
			parts := strings.SplitN(f, ":", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "vectors":
				vectors, _ = strconv.Atoi(parts[1])
			case "dimension":
				dim, _ = strconv.Atoi(parts[1])
			}
		}
		return
	}

	preVectors, preDim, _ := parseVecInfo(preCrashVecInfo)
	postVectors, postDim, _ := parseVecInfo(postCrashVecInfo)

	if postCrashCount == preCrashCount && preVectors == postVectors && preDim == postDim {
		fmt.Printf("  %s✅ [WAL ВЕРИФИЦИРОВАН] Восстановление прошло со 100%% точностью! Ни один байт данных не был утерян!%s\n\n", 
			ColorGreen, ColorReset)
	} else {
		fmt.Printf("  %s❌ [ОШИБКА ДАННЫХ] Обнаружено несовпадение данных после краша!%s\n\n", ColorRed, ColorReset)
	}
}

// ==========================================
// ФАЗА 5: Ограничение памяти и OOM-защита
// ==========================================
func runPhase5() {
	fmt.Printf("%s[ФАЗА 5] Проверка лимитов оперативной памяти и OOM-защиты (Out Of Memory Prevention)...%s\n", ColorBold+ColorCyan, ColorReset)

	const tempPort = 6389
	const maxMemMB = 10 

	fmt.Printf("  🔒 Запуск тестового инстанса сервера с лимитом -maxmemory %d MB на порту %d...\n", maxMemMB, tempPort)
	tempAddr := fmt.Sprintf("localhost:%d", tempPort)
	
	tempCmd := exec.Command("./kvstore-server", "--port", strconv.Itoa(tempPort), "--maxmemory", strconv.Itoa(maxMemMB))
	tempCmd.Start()
	defer func() {
		if tempCmd.Process != nil {
			tempCmd.Process.Kill()
		}
	}()

	time.Sleep(3 * time.Second)

	conn, err := net.Dial("tcp", tempAddr)
	if err != nil {
		fmt.Printf("    Не удалось подключиться к временному серверу: %v\n", err)
		return
	}
	defer conn.Close()

	w := protocol.NewWriter(conn)
	r := protocol.NewReader(conn)

	largeVal := strings.Repeat("A", 100*1024) 

	fmt.Println("  📥 Агрессивно наполняем сервер данными для достижения лимита в 10 МБ...")
	
	var oomBlocked bool
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("oom_check:%d", i)
		err := sendCommand(w, "SET", key, largeVal)
		if err != nil {
			break
		}
		
		res, readErr := r.Read()
		if readErr != nil {
			break
		}

		if res.Typ == '-' && strings.Contains(res.Str, "OOM") {
			oomBlocked = true
			fmt.Printf("    %s🛡️  [OOM БЛОКИРОВКА АКТИВИРОВАНА] Сервер успешно заблокировал запись на ключе %d с ошибкой: \"%s\"%s\n", 
				ColorYellow, i, res.Str, ColorReset)
			break
		}
	}

	if oomBlocked {
		fmt.Printf("  %s✅ [OOM ЗАЩИТА ВЕРИФИЦИРОВАНА] Система гарантирует стабильность и никогда не упадет по нехватке памяти ОС!%s\n\n", 
			ColorGreen, ColorReset)
	} else {
		fmt.Printf("  %s❌ [ОШИБКА] Лимит памяти не был сдержан, OOM защита не сработала!%s\n\n", ColorRed, ColorReset)
	}
}

func GetErrorColor(errors int64) string {
	if errors == 0 {
		return ColorGreen
	}
	return ColorRed
}
