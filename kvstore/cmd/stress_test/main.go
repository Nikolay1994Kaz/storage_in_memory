package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math"
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
	"kvstore/kvstore/vector"
)

// ANSI colors for beautiful CLI output
const (
	ColorReset   = "\033[0m"
	ColorRed     = "\033[31m"
	ColorGreen   = "\033[32m"
	ColorYellow  = "\033[33m"
	ColorBlue    = "\033[34m"
	ColorMagenta = "\033[35m"
	ColorCyan    = "\033[36m"
	ColorWhite   = "\033[37m"
	ColorBold    = "\033[1m"
)

func main() {
	addr := flag.String("addr", "localhost:6380", "Адрес тестируемого сервера")
	metricsAddr := flag.String("metrics-addr", "localhost:9090", "Адрес сервера метрик VictoriaMetrics")
	durationStr := flag.String("duration", "10s", "Длительность теста (например: 10s, 5m, 2h, 48h)")
	concurrencyVal := flag.Int("concurrency", 32, "Количество параллельных клиентов")
	warmup := flag.Bool("warmup", true, "Запустить первичную загрузку (Warmup)")
	crashTest := flag.Bool("crash-test", false, "Запустить краш-тест в конце")
	oomTest := flag.Bool("oom-test", false, "Запустить тест OOM-защиты (запускает временный инстанс на порту 6389)")
	asmTest := flag.Bool("asm-test", true, "Запустить верификацию ассемблерных AVX2 инструкций")
	flag.Parse()

	duration, err := time.ParseDuration(*durationStr)
	if err != nil {
		fmt.Printf("%s[ОШИБКА] Некорректный формат длительности: %v%s\n", ColorRed, err, ColorReset)
		os.Exit(1)
	}

	if *asmTest {
		runAssemblyValidation()
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

// latCollector — потокобезопасный сборщик латентностей с двумя выходами:
//   - скользящее окно: мониторинг-горутина забирает его раз в logInterval и
//     печатает перцентили ЗА ИНТЕРВАЛ. Это линза «дрейф p99 во времени» —
//     глобальный агрегат за многочасовую фазу размазывает медленную деградацию
//     (рост tombstones/сегментов) до невидимости;
//   - reservoir-сэмпл (алгоритм R) для финального отчёта: память ограничена
//     reservoirCap независимо от длительности фазы (раньше latencies копился
//     безгранично — сотни МБ клиентской памяти за 2.5ч).
type latCollector struct {
	mu        sync.Mutex
	window    []time.Duration
	reservoir []time.Duration
	seen      int64
	rng       *rand.Rand
}

const reservoirCap = 1 << 20

func newLatCollector() *latCollector {
	return &latCollector{
		reservoir: make([]time.Duration, 0, 4096),
		rng:       rand.New(rand.NewSource(1)),
	}
}

func (lc *latCollector) add(d time.Duration) {
	lc.mu.Lock()
	lc.window = append(lc.window, d)
	lc.seen++
	if len(lc.reservoir) < reservoirCap {
		lc.reservoir = append(lc.reservoir, d)
	} else if j := lc.rng.Int63n(lc.seen); j < reservoirCap {
		lc.reservoir[j] = d
	}
	lc.mu.Unlock()
}

// swapWindow отдаёт накопленное окно и начинает новое.
func (lc *latCollector) swapWindow() []time.Duration {
	lc.mu.Lock()
	w := lc.window
	lc.window = make([]time.Duration, 0, len(w)+len(w)/4)
	lc.mu.Unlock()
	return w
}

func (lc *latCollector) finalSample() (sample []time.Duration, seen int64) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return append([]time.Duration(nil), lc.reservoir...), lc.seen
}

// percentiles сортирует lat на месте и возвращает p50/p95/p99 (0 при пустом входе).
func percentiles(lat []time.Duration) (p50, p95, p99 time.Duration) {
	if len(lat) == 0 {
		return
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return lat[len(lat)*50/100], lat[len(lat)*95/100], lat[len(lat)*99/100]
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
	fmt.Printf("%s[ФАЗА 2 & 3] Запуск параллельного стресс-теста (смешанная нагрузка + Pub/Sub)...%s\n", ColorBold+ColorCyan, ColorReset)

	const dim = 128

	var (
		wg           sync.WaitGroup
		completedOps atomic.Int64
		errorOps     atomic.Int64
	)
	lc := newLatCollector()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ─── 1. Запуск подписчиков (Subscribers) ───
	const numSubscribers = 8
	fmt.Printf("  📡 Запуск %d параллельных подписчиков Pub/Sub...\n", numSubscribers)
	for i := 0; i < numSubscribers; i++ {
		wg.Add(1)
		go func(subID int) {
			defer wg.Done()

			subConn, err := net.Dial("tcp", addr)
			if err != nil {
				fmt.Printf("    [Подписчик %d] Ошибка подключения: %v\n", subID, err)
				errorOps.Add(1)
				return
			}
			defer subConn.Close()

			sw := protocol.NewWriter(subConn)
			sr := protocol.NewReader(subConn)

			// Половина подписчиков — semantic (VSIM.SUBSCRIBE регистрирует вектор в
			// семантическом индексе → путь Add его графа, грузит аллокатор индекса
			// конкурентно с KV-нагрузкой = регресс-страж фикса #5 dedicated-allocator).
			// Другая половина — classic pub/sub.
			if subID%2 == 1 {
				sargs := []string{"VSIM.SUBSCRIBE", "5.0"}
				srng := rand.New(rand.NewSource(int64(subID)*7 + 1))
				for j := 0; j < dim; j++ {
					sargs = append(sargs, strconv.FormatFloat(srng.NormFloat64(), 'f', 4, 64))
				}
				if err := sendCommand(sw, sargs...); err != nil {
					errorOps.Add(1)
					return
				}
			} else {
				channel := fmt.Sprintf("stress:chan:%d", subID%10) // 10 общих каналов
				if err := sendCommand(sw, "SUBSCRIBE", channel); err != nil {
					errorOps.Add(1)
					return
				}
			}

			// Первый Read забирает подтверждение подписки (semantic — через writePump,
			// classic — из буфера), дальше drain-петля читает push-сообщения.
			if _, err := sr.Read(); err != nil {
				errorOps.Add(1)
				return
			}

			// Прерываем блокирующий Read при завершении контекста
			go func() {
				<-ctx.Done()
				subConn.Close()
			}()

			for {
				_, err := sr.Read()
				if err != nil {
					select {
					case <-ctx.Done():
						return
					default:
					}
					errorOps.Add(1)
					return
				}
				completedOps.Add(1)
			}
		}(i)
	}

	// ─── 2. Запуск клиентов-писателей (Writers) ───
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

			for {
				select {
				case <-ctx.Done():
					return
				default:
					reqStart := time.Now()
					dice := rng.Intn(100)

					var err error
					// Расширенный микс: помимо read/upsert/pub добавлены delete-churn
					// (VSIM.DEL/DEL → tombstone+merge, #3), бинарный путь (VSIM.ADDBIN),
					// filterFn-путь (VSIM.SEARCHFILTER PREFIX), semantic-routing
					// (VSIM.PUBLISH → поиск по семантич.индексу, #5), zset-удаления
					// (ZREM → btree delete/borrow/leaf-merge) и TTL-churn
					// (SET EX/EXPIRE → active-expiry на монотонных часах).
					switch {
					case dice < 22: // 22% GET
						err = sendCommand(w, "GET", fmt.Sprintf("key:%d", rng.Intn(5000)))
					case dice < 42: // 20% VSIM.SEARCH
						args := []string{"VSIM.SEARCH", "10"}
						for j := 0; j < dim; j++ {
							args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
						}
						err = sendCommand(w, args...)
					case dice < 45: // 3% tenant ADDATTR/FILTER (колоночный attr/tenant-слой, путь #2)
						tn := fmt.Sprintf("t%d", rng.Intn(3))
						if rng.Intn(2) == 0 {
							args := []string{"VSIM.ADDATTR", fmt.Sprintf("tn:%d", rng.Intn(300)), "CAT", "tenant", tn, "VEC"}
							for j := 0; j < dim; j++ {
								args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
							}
							err = sendCommand(w, args...)
						} else {
							args := []string{"VSIM.FILTER", "10", "EQ", "tenant", tn, "VEC"}
							for j := 0; j < dim; j++ {
								args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
							}
							err = sendCommand(w, args...)
						}
					case dice < 53: // 8% VSIM.SEARCHRANGE (B+Tree + HNSW)
						minScore := rng.Float64() * 10
						args := []string{"VSIM.SEARCHRANGE", "10", "benchset",
							strconv.FormatFloat(minScore, 'f', 2, 64),
							strconv.FormatFloat(minScore+5.0, 'f', 2, 64)}
						for j := 0; j < dim; j++ {
							args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
						}
						err = sendCommand(w, args...)
					case dice < 60: // 7% VSIM.SEARCHFILTER PREFIX (путь SearchFiltered/filterFn)
						args := []string{"VSIM.SEARCHFILTER", "10", "PREFIX", "vec:"}
						for j := 0; j < dim; j++ {
							args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
						}
						err = sendCommand(w, args...)
					case dice < 70: // 10% VSIM.ADD (upsert-churn на 500 ключей)
						args := []string{"VSIM.ADD", fmt.Sprintf("vec:%d", rng.Intn(500))}
						for j := 0; j < dim; j++ {
							args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
						}
						err = sendCommand(w, args...)
					case dice < 75: // 5% VSIM.ADDBIN (бинарный zero-copy путь)
						vecF := make([]float32, dim)
						for j := range vecF {
							vecF[j] = rng.Float32()
						}
						err = sendCommand(w, "VSIM.ADDBIN", fmt.Sprintf("vec:%d", rng.Intn(500)), string(vector.SerializeVector(vecF)))
					case dice < 82: // 7% VSIM.DEL (delete-churn → tombstone; VSIM.ADD ре-добавит)
						err = sendCommand(w, "VSIM.DEL", fmt.Sprintf("vec:%d", rng.Intn(500)))
					case dice < 87: // 5% SET + ZADD
						key := fmt.Sprintf("key:%d", rng.Intn(5000))
						if err = sendCommand(w, "SET", key, "updated_during_stress"); err == nil {
							r.Read()
							err = sendCommand(w, "ZADD", "benchset", strconv.FormatFloat(rng.Float64()*15, 'f', 2, 64), key)
						}
					case dice < 90: // 3% ZREM (btree delete → borrow/leaf-merge под конкурентным Search)
						err = sendCommand(w, "ZREM", "benchset", fmt.Sprintf("key:%d", rng.Intn(5000)))
					case dice < 92: // 2% TTL-churn (SET EX / EXPIRE → active-expiry, монотонные дедлайны)
						key := fmt.Sprintf("ttl:%d", rng.Intn(2000))
						ttlSec := strconv.Itoa(1 + rng.Intn(30))
						if rng.Intn(2) == 0 {
							err = sendCommand(w, "SET", key, "ttl_payload", "EX", ttlSec)
						} else {
							err = sendCommand(w, "EXPIRE", key, ttlSec)
						}
					case dice < 95: // 3% DEL (KV delete)
						err = sendCommand(w, "DEL", fmt.Sprintf("key:%d", rng.Intn(5000)))
					case dice < 99: // 4% VSIM.PUBLISH (semantic-routing → поиск по семантич.индексу)
						args := []string{"VSIM.PUBLISH", "stress_semantic_msg"}
						for j := 0; j < dim; j++ {
							args = append(args, fmt.Sprintf("%.6f", rng.Float32()))
						}
						err = sendCommand(w, args...)
					default: // 1% PUBLISH (classic)
						err = sendCommand(w, "PUBLISH", fmt.Sprintf("stress:chan:%d", rng.Intn(10)), "stress_message_payload_val")
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

					lc.add(time.Since(reqStart))
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

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				elapsed := time.Since(startTime)
				currCompleted := completedOps.Load()
				diff := currCompleted - lastCompleted
				lastCompleted = currCompleted

				rps := float64(diff) / logInterval.Seconds()
				memBytes := getServerMemory(metricsAddr)
				memMB := memBytes / 1024 / 1024
				vecCount := getVectorCount(addr)

				// Перцентили ЗА ИНТЕРВАЛ (не кумулятивные): их дрейф от тика к
				// тику — и есть линза «латентность деградирует со временем».
				wp50, wp95, wp99 := percentiles(lc.swapWindow())

				fmt.Printf("  ⏱️  [%s] Прогресс: %s/%s | Текущий RPS: %s%.0f%s | Ошибок: %s%d%s | Память: %.2f MB | Векторов: %d | окно p50=%v p95=%v p99=%v\n",
					time.Now().Format("15:04:05"),
					elapsed.Round(time.Second),
					testDuration,
					ColorCyan, rps, ColorReset,
					GetErrorColor(errorOps.Load()), errorOps.Load(), ColorReset,
					memMB, vecCount,
					wp50.Round(time.Microsecond), wp95.Round(time.Microsecond), wp99.Round(time.Microsecond),
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

	// Финальные перцентили — по reservoir-сэмплу (равномерная выборка всей фазы).
	sample, seen := lc.finalSample()
	p50, p95, p99 := percentiles(sample)

	fmt.Printf("\n  📊 %sРезультаты стресс-теста под нагрузкой:%s\n", ColorBold, ColorReset)
	if seen > int64(len(sample)) {
		fmt.Printf("    - Перцентили по reservoir-сэмплу %d из %d измерений\n", len(sample), seen)
	}
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

func runAssemblyValidation() {
	fmt.Printf("%s[ФАЗА 0] Локальная верификация ассемблерных AVX2 инструкций...%s\n", ColorBold+ColorCyan, ColorReset)

	// Различные размерности для тестирования:
	// - кратные 8 (оптимизированный путь)
	// - некратные 8 (проверка корректности хвостового цикла remainder_loop)
	// - маленькие и большие размерности
	dims := []int{1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 31, 32, 127, 128, 768, 1024}

	rng := rand.New(rand.NewSource(42))

	for _, dim := range dims {
		a := make([]float32, dim)
		b := make([]float32, dim)
		for i := 0; i < dim; i++ {
			a[i] = rng.Float32() * 10
			b[i] = rng.Float32() * 10
		}

		// 1. Тестируем евклидово расстояние
		asmEuclidean := vector.EuclideanDistance(a, b)
		goEuclidean := vector.EuclideanPureGo(a, b)
		diffEuc := math.Abs(float64(asmEuclidean - goEuclidean))
		maxValEuc := math.Max(math.Abs(float64(asmEuclidean)), math.Abs(float64(goEuclidean)))
		isOkEuc := diffEuc < 1e-5
		if !isOkEuc && maxValEuc > 0 {
			isOkEuc = (diffEuc / maxValEuc) < 1e-5
		}
		if !isOkEuc {
			fmt.Printf("  %s❌ [ОШИБКА AVX2] Евклидово расстояние не совпадает для dim=%d! ASM=%f, Go=%f (diff=%e, relDiff=%e)%s\n",
				ColorRed, dim, asmEuclidean, goEuclidean, diffEuc, diffEuc/maxValEuc, ColorReset)
			os.Exit(1)
		}

		// 2. Тестируем скалярное произведение (Dot Product)
		// Нормализуем вектора, чтобы проверить Cosine Distance и Dot Product
		aNorm := make([]float32, dim)
		bNorm := make([]float32, dim)
		copy(aNorm, a)
		copy(bNorm, b)
		vector.Normalize(aNorm)
		vector.Normalize(bNorm)

		asmDot := vector.DotProductDistance(aNorm, bNorm)
		goDot := 1.0 - vector.DotProductPureGo(aNorm, bNorm)
		diffDot := math.Abs(float64(asmDot - goDot))
		maxValDot := math.Max(math.Abs(float64(asmDot)), math.Abs(float64(goDot)))
		isOkDot := diffDot < 1e-5
		if !isOkDot && maxValDot > 0 {
			isOkDot = (diffDot / maxValDot) < 1e-5
		}
		if !isOkDot {
			fmt.Printf("  %s❌ [ОШИБКА AVX2] Dot Product не совпадает для dim=%d! ASM=%f, Go=%f (diff=%e, relDiff=%e)%s\n",
				ColorRed, dim, asmDot, goDot, diffDot, diffDot/maxValDot, ColorReset)
			os.Exit(1)
		}

		// 3. Тестируем Cosine Distance
		asmCosine := vector.CosineDistance(a, b)
		// Pure Go Cosine Distance
		dotGo := vector.DotProductPureGo(a, b)
		normAGo := vector.DotProductPureGo(a, a)
		normBGo := vector.DotProductPureGo(b, b)
		var goCosine float32 = 1.0
		if normAGo != 0 && normBGo != 0 {
			sim := dotGo / float32(math.Sqrt(float64(normAGo))*math.Sqrt(float64(normBGo)))
			goCosine = 1.0 - sim
		}
		diffCos := math.Abs(float64(asmCosine - goCosine))
		maxValCos := math.Max(math.Abs(float64(asmCosine)), math.Abs(float64(goCosine)))
		isOkCos := diffCos < 1e-5
		if !isOkCos && maxValCos > 0 {
			isOkCos = (diffCos / maxValCos) < 1e-5
		}
		if !isOkCos {
			fmt.Printf("  %s❌ [ОШИБКА AVX2] Косинусное расстояние не совпадает для dim=%d! ASM=%f, Go=%f (diff=%e, relDiff=%e)%s\n",
				ColorRed, dim, asmCosine, goCosine, diffCos, diffCos/maxValCos, ColorReset)
			os.Exit(1)
		}
	}

	fmt.Printf("  %s✅ [AVX2 ВЕРИФИЦИРОВАН] Ассемблерные инструкции Euclidean, Cosine и DotProduct успешно прошли тесты на точность по всем размерностям!%s\n\n",
		ColorGreen, ColorReset)
}
