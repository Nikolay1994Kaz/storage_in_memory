package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"kvstore/kvstore/internal/ai"
	"kvstore/kvstore/internal/btree"
	"kvstore/kvstore/internal/monitoring"
	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

const (
	dataDir      = "data"
	syncInterval = 100 * time.Millisecond
)

// version — версия сборки. Дефолт "dev" для локальных прогонов; в релизных
// сборках проставляется через -ldflags "-X main.version=$(git describe ...)"
// (см. Makefile/Dockerfile).
var version = "dev"

var globalTxMu sync.Mutex

func unsafeString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

func main() {
	// CLI-флаги
	port := flag.Int("port", 6380, "порт для клиентов")
	maxMemoryMB := flag.Int("maxmemory", 0, "лимит памяти в МБ (0 = без лимита)")
	clusterEnabled := flag.Bool("cluster", false, "включить кластерный режим")
	clusterSlotStart := flag.Int("slot-start", 0, "начало диапазона слотов")
	clusterSlotEnd := flag.Int("slot-end", 16383, "конец диапазона слотов")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "URL Ollama API")
	requirePass := flag.String("requirepass", "", "пароль для AUTH через флаг (НЕБЕЗОПАСНО: виден в ps/history/proc; для прода см. -requirepass-file или env KVSTORE_REQUIREPASS)")
	requirePassFile := flag.String("requirepass-file", "", "путь к файлу с паролем для AUTH (приоритетнее env и -requirepass; секрет не светится в списке процессов)")
	tlsCert := flag.String("tls-cert", "", "путь к TLS сертификату (PEM)")
	tlsKey := flag.String("tls-key", "", "путь к TLS ключу (PEM)")
	tlsMinVersion := flag.String("tls-min-version", "1.2", "минимальная версия TLS: 1.2 или 1.3")
	tlsClientCA := flag.String("tls-client-ca", "", "путь к CA (PEM) для mTLS: если задан — требуем клиентский сертификат, подписанный этим CA")
	metricsPort := flag.Int("metrics-port", 9090, "порт для HTTP сервера метрик VictoriaMetrics (0 = отключен)")
	idleTimeout := flag.Duration("idle-timeout", 5*time.Minute, "закрывать соединение после простоя без активности (защита от Slowloris/брошенных conn; 0 = выключено)")
	writeTimeout := flag.Duration("write-timeout", 30*time.Second, "макс. время на отправку ответа клиенту (защита от застрявшего reader; 0 = выключено)")
	maxConnections := flag.Int("max-connections", 10000, "потолок одновременных соединений (защита от исчерпания fd/RAM; 0 = без лимита). Требует соответствующего ulimit -n")
	hnswM := flag.Int("hnsw-m", 32, "HNSW M parameter (number of node connections)")
	hnswEfConstruction := flag.Int("hnsw-ef-construction", 400, "HNSW efConstruction parameter")
	hnswEfSearch := flag.Int("hnsw-ef-search", 100, "HNSW efSearch parameter (0 = auto). 100 = рабочая точка recall@10≈0.966 на MNIST-784, ~1.56× QPS vs 200 (recall 0.983). См. step_profit_test.go:TestStep2_EfSearch")
	hnswUseLSH := flag.Bool("hnsw-use-lsh", false, "Enable LSH pre-filtering for high-dimensional vectors (dim >= 256)")
	hnswUseSQ := flag.Bool("hnsw-use-sq", false, "Enable Scalar Quantization (int8) for frozen segments (dim<=256). 4x memory compression, ~96% recall, higher QPS via L3 cache locality")
	compactionWorkers := flag.Int("compaction-workers", 0, "Number of parallel segment build workers (0 = auto NumCPU/2 clamped 2-8). Build Pool: insert does not block during heavy L2 compaction")
	showVersion := flag.Bool("version", false, "вывести версию и выйти")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}
	log.Printf("KVStore version %s", version)

	// Разрешение пароля AUTH из (в порядке приоритета): файл → env → флаг.
	// Файл/env предпочтительнее флага — секрет не попадает в ps/history/proc.
	authPassword, err := resolveAuthPassword(*requirePass, *requirePassFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "requirepass: %v\n", err)
		os.Exit(1)
	}
	if authPassword != "" && *requirePassFile == "" && os.Getenv("KVSTORE_REQUIREPASS") == "" {
		log.Println("WARNING: пароль задан через флаг -requirepass — он виден в списке процессов; " +
			"для прода используйте -requirepass-file или env KVSTORE_REQUIREPASS")
	}
	// Предвычисляем SHA-256 пароля один раз: на AUTH сравниваем дайджесты
	// constant-time (crypto/subtle) — без утечки длины/содержимого по таймингу.
	authEnabled := authPassword != ""
	authHash := sha256.Sum256([]byte(authPassword))

	// TCMallocStore: per-worker MCache (lock-free alloc) + lock-free HashTable (GET)
	s := tcmalloc.NewTCMallocStore(runtime.NumCPU())
	defer s.Close() // останавливает внутреннюю горутину deferred free

	// build-info метрика: kvstore_build_info{version="..."} = 1
	monitoring.SetBuildInfo(version)

	// Инициализация метрик памяти TCMalloc
	monitoring.InitMemoryMetrics(
		func() float64 { return float64(s.UsedMemory()) },
		func() float64 {
			numChunks, _, _, _ := s.HeapStats()
			return float64(numChunks)
		},
		func() float64 {
			_, _, _, numSpans := s.HeapStats()
			return float64(numSpans)
		},
		func() float64 { return float64(s.MaxMemory()) },
	)

	// Лимит памяти
	if *maxMemoryMB > 0 {
		s.SetMaxMemory(int64(*maxMemoryMB) * 1024 * 1024)
		log.Printf("Max memory: %d MB", *maxMemoryMB)
	}

	os.MkdirAll(dataDir, 0755)

	// === 1. TTL Manager ===
	// Инициализация перенесена после WAL (строка ниже), потому что
	// CompositeEvictor при TTL-expire записывает OpVSimDel в WAL.
	// Для фазы WAL replay используем временный KV-only evictor.
	ttl := store.NewTTLManager(tcmalloc.NewEvictor(s))
	// ttl.Stop() вызывается явно в упорядоченном shutdown (не через defer):
	// TTL-эвиктор пишет в WAL-канал, поэтому его надо заглушить ДО bw.Close().

	// === 2. Инициализация хранилища векторов и загрузка бинарного снапшота ===
	// Используем LeveledVectorStore (LSM+CSR) как реализацию VectorIndex.
	var vecStore vector.VectorIndex = vector.NewLeveledVectorStore(vector.LeveledConfig{
		M:              *hnswM,
		EfConstruction: *hnswEfConstruction,
		EfSearch:       *hnswEfSearch,
		Distance:       vector.EuclideanDistance,
		Allocator:      s,
		UseSQ:          *hnswUseSQ,
		NumBuilders:    *compactionWorkers,
	})
	// LSH не применяется в LeveledVectorStore, вызов no-op через type assertion:
	if lsh, ok := vecStore.(interface{ SetUseLSH(bool) }); ok {
		lsh.SetUseLSH(*hnswUseLSH)
	}

	// Регистрация gauge-метрик векторного хранилища (segments/tombstones/bytes).
	// Provider вызывается только при scrape /metrics — 0 overhead на hot path.
	if lvs, ok := vecStore.(*vector.LeveledVectorStore); ok {
		monitoring.SetVectorStateProvider(leveledStatsAdapter{lvs: lvs})
	}

	// === 2.5. Инициализация sorted sets (ZSet) ===
	zsetReg := zset.New(s)

	graphPath := filepath.Join(dataDir, "graph_leveled.bin")
	graphLoaded := false

	if _, err := os.Stat(graphPath); err == nil {
		log.Printf("Loading leveled vector store from binary snapshot %s...", graphPath)
		f, err := os.Open(graphPath)
		if err == nil {
			if err := vecStore.LoadBinary(f); err != nil {
				log.Printf("WARNING: failed to load vector snapshot: %v. Will rebuild from WAL.", err)
			} else {
				log.Printf("Leveled vector store loaded successfully from snapshot!")
				graphLoaded = true
			}
			f.Close()
		}
	}

	// === 3. Восстановление состояния из WAL ===
	restored := 0
	vecRestored := 0

	// maxLSN — наибольший встреченный номер записи (или watermark из snapshot).
	// После recovery ставим nextLSN = maxLSN+1 до приёма трафика, чтобы номера
	// не переиспользовались (иначе резюмируемая репликация сломается).
	var maxLSN uint64
	bumpLSN := func(v uint64) {
		if v > maxLSN {
			maxLSN = v
		}
	}

	// vecWatermark — LSN, до которого векторный снапшот (graph_leveled.bin) уже
	// содержит все операции. Реплей wal_*.log пропускает векторные записи с
	// LSN ≤ watermark — иначе операции из окна «ротация WAL → запись снапшота»
	// накатились бы поверх снапшота, дублируя векторы при каждом рестарте.
	var vecWatermark uint64
	if graphLoaded {
		if lvs, ok := vecStore.(*vector.LeveledVectorStore); ok {
			vecWatermark = lvs.SnapshotLSN()
		}
	}
	// skipVec решает, пропустить ли векторную операцию как уже отражённую в снапшоте.
	// Из snapshot.wal — пропускаем, если граф загружен из бинарного снапшота.
	// Из wal_*.log — пропускаем, если LSN ≤ watermark (запись уже в graph_leveled.bin).
	skipVec := func(entry wal.Entry, isFromSnapshot bool) bool {
		if isFromSnapshot {
			return graphLoaded
		}
		return entry.LSN <= vecWatermark
	}

	// Шаг A: Сначала читаем и накатываем snapshot.wal (если есть)
	snapshotPath := filepath.Join(dataDir, "snapshot.wal")
	snapWatermark, snapshotEntries, err := wal.ReadFile(snapshotPath)
	if err != nil {
		log.Fatalf("Failed to read snapshot.wal: %v", err)
	}
	bumpLSN(snapWatermark) // watermark: snapshot покрывает состояние до этого LSN

	applyEntry := func(entry wal.Entry, isFromSnapshot bool) {
		switch entry.Op {
		case wal.OpSet:
			s.Set(0, entry.Key, entry.Value)
			restored++
		case wal.OpDel:
			s.Del(0, entry.Key)
			vecStore.Delete(entry.Key) // Также удаляем вектор при реплее DEL
			ttl.OnDelete(entry.Key)
			restored++
		case wal.OpExpire:
			if len(entry.Value) == 8 {
				expiresAt := time.Unix(0, int64(binary.BigEndian.Uint64(entry.Value)))
				remaining := time.Until(expiresAt)
				if remaining > 0 {
					ttl.Set(entry.Key, remaining)
				} else {
					s.Del(0, entry.Key)
					vecStore.Delete(entry.Key) // Также удаляем вектор, если ключ просрочен в оффлайне
					ttl.OnDelete(entry.Key)
				}
			}
			restored++
		case wal.OpPersist:
			ttl.Remove(entry.Key)
			restored++
		case wal.OpVSimAdd:
			// Пропускаем, если операция уже в снапшоте (snapshot.wal при graphLoaded
			// или wal_*.log с LSN ≤ watermark) — иначе дубль вектора после рестарта.
			if skipVec(entry, isFromSnapshot) {
				return
			}
			vec := vector.DeserializeVector(entry.Value)
			if err := vecStore.Add(entry.Key, vec); err != nil {
				log.Printf("WARNING: failed to restore vector %s: %v", entry.Key, err)
			}
			vecRestored++
			restored++
		case wal.OpVSimAddAttrs:
			// Вектор + атрибуты (P0-4): attrs/tenant восстанавливаются через
			// AddWithAttrs, а не теряются как при голом Add.
			if skipVec(entry, isFromSnapshot) {
				return
			}
			vec, attrs, err := vector.DeserializeVectorWithAttrs(entry.Value)
			if err != nil {
				log.Printf("WARNING: failed to decode vector+attrs %s: %v", entry.Key, err)
				return
			}
			if lvs, ok := vecStore.(*vector.LeveledVectorStore); ok {
				if err := lvs.AddWithAttrs(entry.Key, vec, attrs); err != nil {
					log.Printf("WARNING: failed to restore vector+attrs %s: %v", entry.Key, err)
				}
			} else if err := vecStore.Add(entry.Key, vec); err != nil {
				// Индекс без attr-слоя — восстанавливаем хотя бы вектор.
				log.Printf("WARNING: failed to restore vector %s: %v", entry.Key, err)
			}
			vecRestored++
			restored++
		case wal.OpVSimDel:
			if skipVec(entry, isFromSnapshot) {
				return
			}
			vecStore.Delete(entry.Key)
			restored++
		case wal.OpZAdd:
			if len(entry.Value) >= 8 {
				score, member := zset.DecodeZAddValue(entry.Value)
				zsetReg.ZAdd(0, entry.Key, score, member)
				restored++
			}
		case wal.OpZRem:
			member := string(entry.Value)
			zsetReg.ZRem(0, entry.Key, member)
			restored++
		}
	}

	for _, entry := range snapshotEntries {
		applyEntry(entry, true)
	}

	// Шаг B: Потом читаем и накатываем все wal_*.log файлы в правильном порядке
	matches, _ := filepath.Glob(filepath.Join(dataDir, "wal_*.log"))
	sort.Strings(matches) // Сортируем по имени (по времени создания)

	for _, path := range matches {
		_, logEntries, err := wal.ReadFile(path)
		if err != nil {
			log.Fatalf("Failed to read WAL log %s: %v", path, err)
		}
		for _, entry := range logEntries {
			bumpLSN(entry.LSN)
			applyEntry(entry, false)
		}
	}

	if restored > 0 {
		log.Printf("Restored %d operations from WAL (%d vectors)", restored, vecRestored)
	}

	// === 3. WAL ===
	walPath := filepath.Join(dataDir, fmt.Sprintf("wal_%s.log", time.Now().Format("20060102_150405")))
	rawWAL, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("Failed to open WAL: %v", err)
	}

	// Выставляем счётчик LSN ДО того, как flusher начнёт присваивать номера
	// новым записям. rawWAL.Open() инициализировал его в 1; продолжаем с maxLSN+1.
	rawWAL.SetNextLSN(maxLSN + 1)

	bw := wal.NewBatchWAL(rawWAL)
	// bw.Close() вызывается явно в конце упорядоченного shutdown — ПОСЛЕ того,
	// как заглушены все писатели в WAL-канал (воркеры, TTL-эвиктор, AI-воркер,
	// syncer). Иначе запоздалый bw.Write словит send на закрытый канал.

	// === TTL: подключаем CompositeEvictor ===
	// Теперь при истечении TTL ключа TTLManager автоматически:
	//   1. Удаляет значение из KV Store (как раньше)
	//   2. Удаляет вектор из HNSW графа (если ключ есть в VectorStore)
	//   3. Записывает OpVSimDel в WAL (чтобы удаление пережило рестарт)
	ttl.SetEvictor(&compositeEvictor{kv: s, vec: vecStore, wal: bw})

	// === 4. Syncer ===
	// iterateAll — переснимает ВСЁ KV-состояние, живущее только в реплее WAL,
	// для snapshot.wal (векторы хранятся отдельно). Компакция удаляет старые
	// WAL, поэтому всё, что не попадёт сюда, теряется после рестарта.
	iterateAll := func(fn func(op byte, key string, value []byte)) {
		snapshotIterate(s, ttl, zsetReg, fn)
	}

	// saveVectors — сохраняет LeveledVectorStore в graph_leveled.bin.
	// Перед записью принудительно сбрасываем дельту в сегменты (FlushDeltaSync),
	// гарантируя что снапшот содержит все данные.
	saveVectors := func() error {
		// Приводим к *LeveledVectorStore для вызова FlushDeltaSync.
		if lvs, ok := vecStore.(*vector.LeveledVectorStore); ok {
			// Watermark ДО FlushDeltaSync: любая векторная операция с LSN ≤ него
			// уже присвоила LSN, значит вектор был в дельте/сегментах и попадёт в
			// снапшот через FlushDeltaSync. Recovery пропустит такие WAL-записи —
			// иначе окно «ротация → снапшот» дублировало бы векторы при рестарте.
			lvs.SetSnapshotWatermark(rawWAL.LastLSN())
			lvs.FlushDeltaSync()
		}
		graphPath := filepath.Join(dataDir, "graph_leveled.bin")
		tmpPath := graphPath + ".tmp"
		f, err := os.Create(tmpPath)
		if err != nil {
			return err
		}
		writer := bufio.NewWriterSize(f, 256*1024)
		if err := vecStore.SaveBinary(writer); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return err
		}
		if err := writer.Flush(); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return err
		}
		f.Close()
		if err := os.Rename(tmpPath, graphPath); err != nil {
			return err
		}
		// fsync каталога: делаем rename graph_leveled.bin durable (иначе power-loss
		// откатит замену снапшота при уже удалённых старых WAL).
		return wal.FsyncDir(dataDir)
	}

	syncer := wal.NewSyncer(rawWAL, syncInterval, dataDir, iterateAll, saveVectors)
	// syncer.Stop() вызывается явно в упорядоченном shutdown перед bw.Close().

	// === 5. Pub/Sub Hub (Classic + Semantic) ===
	semanticIndex := vector.NewVectorStoreCosine(s)
	semanticIndex.SetHNSWParams(*hnswM, *hnswEfConstruction, *hnswEfSearch)
	semanticIndex.SetUseLSH(*hnswUseLSH)
	hub := pubsub.NewHub(semanticIndex)

	// === 6. Cluster (опционально, только в experimental-сборке) ===
	// Вся обвязка вынесена за build-tag `experimental` (cluster_experimental.go).
	// В прод-сборке newClusterRouter — заглушка, возвращающая ошибку, а сам
	// distributed-код не линкуется. cl остаётся nil → single-node hot-path.
	var cl clusterNode
	if *clusterEnabled {
		addr := fmt.Sprintf("127.0.0.1:%d", *port)
		var err error
		cl, err = newClusterRouter(addr, *port+1, *clusterSlotStart, *clusterSlotEnd, s, vecStore, ttl)
		if err != nil {
			log.Fatalf("Failed to start cluster: %v", err)
		}
		defer cl.StopGossip()
	}

	// === 7. WASM Compute Engine (за build-tag experimental; в прод-сборке no-op) ===
	// Реальный движок и вся wazero-зависимость линкуются только с -tags experimental
	// (см. wasm_seam.go / wasm_stub.go / wasm_experimental.go). Небезопасная ACE-
	// поверхность (WASM.EXEC) в прод-бинарь не попадает.
	wasm := newComputeEngine(computeDeps{
		store:    s,
		ttl:      ttl,
		bw:       bw,
		hub:      hub,
		vecStore: vecStore,
		globalMu: &globalTxMu,
	})
	defer wasm.Close()

	// === 8. AI Engine (Ollama) ===
	var aiClient *ai.Client
	var aiWorker *ai.Worker

	aiClient = ai.NewClient(*ollamaURL, "nomic-embed-text", "gemma4:e2b")
	if err := aiClient.Ping(context.Background()); err != nil {
		log.Printf("WARNING: Ollama not available (%v), AI commands disabled", err)
		aiClient = nil
	} else {
		log.Println("Ollama connected: nomic-embed-text + gemma4:e2b")

		// Подключаем AI к WASM Engine — WASM-модули получают доступ к Ollama.
		// В прод-сборке (без experimental) SetAI — no-op.
		wasm.SetAI(aiClient.Embed, aiClient.Chat)

		// Background Worker: асинхронный embedding с PubSub-нотификациями
		aiWorker = ai.NewWorker(aiClient, 256)
		aiWorker.VecStoreAdd = func(key string, vec []float32) error {
			// WAL: без этого AI-проиндексированные векторы терялись при рестарте.
			// Записываем сериализованный вектор — при восстановлении OpVSimAdd
			// вставит его обратно в HNSW без повторного вызова Ollama.
			walValue := vector.SerializeVector(vec)
			bw.Write(wal.Entry{Op: wal.OpVSimAdd, Key: key, Value: walValue})
			return vecStore.Add(key, vec)
		}
		aiWorker.KVStoreSet = func(key string, value []byte) {
			s.Set(0, key, value)
		}
		aiWorker.Publish = func(channel, message string) {
			hub.Publish(channel, message)
		}
		aiWorker.Start(2) // 2 горутины (Ollama сама батчит)
		// aiWorker.Stop() вызывается явно в упорядоченном shutdown до bw.Close()
		// (AI-воркер пишет OpVSimAdd в WAL-канал).
	}

	// ═══════════════════════════════════════════════════
	// HANDLER — zero-alloc: args = [][]byte из ring buffer
	// ═══════════════════════════════════════════════════

	handler := func(cs *server.ConnState, args [][]byte) {
		if len(args) == 0 {
			cs.Buf.WriteError("ERR empty command")
			return
		}

		cmd := strings.ToUpper(string(args[0]))
		cmdArgs := args[1:]

		// ─── AUTH ───────────────────────────────────────────
		// Если --requirepass задан, клиент должен пройти AUTH
		// перед выполнением любых команд (кроме AUTH и PING).
		if authEnabled {
			if cmd == "AUTH" {
				if len(cmdArgs) != 1 {
					cs.Buf.WriteError("ERR wrong number of arguments for 'AUTH' command")
					return
				}
				// Сравниваем SHA-256-дайджесты constant-time: одинаковая длина
				// входа (32 байта) + subtle.ConstantTimeCompare убирают тайминг-
				// сайд-канал (утечку длины/префикса пароля при переборе).
				inHash := sha256.Sum256(cmdArgs[0])
				if subtle.ConstantTimeCompare(inHash[:], authHash[:]) == 1 {
					cs.Authenticated = true
					cs.Buf.WriteSimpleString("OK")
				} else {
					cs.Buf.WriteError("ERR invalid password")
				}
				return
			}
			if !cs.Authenticated && cmd != "PING" {
				cs.Buf.WriteError("NOAUTH Authentication required")
				return
			}
		}

		// Subscriber-mode (как в Redis): пока соединение имеет подписки, обслуживаем
		// его ТОЛЬКО через pub/sub-шов, где все ответы идут через единственного
		// писателя conn — writePump. Иначе основной обработчик пишет в cs.Buf
		// параллельно с writePump → два писателя в один сокет (перемешанные кадры),
		// а клиент-подписчик не ждёт обычных ответов. Первый SUBSCRIBE идёт обычным
		// путём (ещё не подписчик) и переводит соединение в этот режим.
		if hub.IsSubscriber(cs.Conn) {
			hub.HandleSubscriberCommand(cs.Conn, cmd, cmdArgs)
			return
		}

		// Транзакции
		switch cmd {
		case "MULTI":
			start := time.Now()
			cs.InTx = true
			cs.Buf.WriteSimpleString("OK")
			monitoring.RecordCommand(cmd, time.Since(start))
			return
		case "DISCARD":
			start := time.Now()
			if !cs.InTx {
				cs.Buf.WriteError("ERR DISCARD without MULTI")
				monitoring.RecordCommand(cmd, time.Since(start))
				return
			}
			cs.InTx = false
			cs.TxQueue = nil
			cs.Buf.WriteSimpleString("OK")
			monitoring.RecordCommand(cmd, time.Since(start))
			return
		case "EXEC":
			startEXEC := time.Now()
			if !cs.InTx {
				cs.Buf.WriteError("ERR EXEC without MULTI")
				monitoring.RecordCommand(cmd, time.Since(startEXEC))
				return
			}
			execQueuedTx(cs.TxQueue, cs.Buf.WriteArrayHeader, func(qCmd string, qCmdArgs [][]byte) {
				startCmd := time.Now()
				executeCommand(s, bw, ttl, hub, cl, wasm, vecStore, zsetReg, aiClient, aiWorker, iterateAll, saveVectors, cs, qCmd, qCmdArgs)
				monitoring.RecordCommand(qCmd, time.Since(startCmd))
			})
			cs.InTx = false
			cs.TxQueue = nil
			monitoring.RecordCommand(cmd, time.Since(startEXEC))
			return
		}

		if cs.InTx {
			// Копируем args — ring buffer будет перезаписан!
			argsCopy := make([][]byte, len(args))
			for i, a := range args {
				argsCopy[i] = append([]byte(nil), a...)
			}
			cs.TxQueue = append(cs.TxQueue, argsCopy)
			cs.Buf.WriteSimpleString("QUEUED")
			return
		}

		start := time.Now()
		executeCommand(s, bw, ttl, hub, cl, wasm, vecStore, zsetReg, aiClient, aiWorker, iterateAll, saveVectors, cs, cmd, cmdArgs)
		monitoring.RecordCommand(cmd, time.Since(start))
	}

	// Запуск HTTP-сервера метрик VictoriaMetrics
	if *metricsPort > 0 {
		monitoring.StartHttpServer(*metricsPort)
	}

	// === 8. Сервер ===
	listenAddr := fmt.Sprintf(":%d", *port)
	srv := server.NewServer(listenAddr, handler)
	srv.IdleTimeout = *idleTimeout
	srv.WriteTimeout = *writeTimeout
	srv.MaxConnections = *maxConnections
	// Отключение клиента чистит его подписки Pub/Sub (classic + semantic-вектор в
	// HNSW). Без этого хука вектор течёт в индекс навсегда, а writePump висит.
	srv.OnDisconnect = hub.RemoveConn

	// TLS: если указаны сертификат и ключ — включаем шифрование.
	if *tlsCert != "" && *tlsKey != "" {
		cfg, err := buildServerTLSConfig(*tlsCert, *tlsKey, *tlsMinVersion, *tlsClientCA)
		if err != nil {
			fmt.Fprintf(os.Stderr, "TLS: %v\n", err)
			os.Exit(1)
		}
		srv.TLSConfig = cfg
		if *tlsClientCA != "" {
			log.Println("TLS: mTLS включён — требуется клиентский сертификат, подписанный указанным CA")
		}
	}

	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start: %v\n", err)
		os.Exit(1)
	}

	// Снапшот загружен и listener поднят — помечаем процесс готовым (/ready → 200).
	monitoring.SetReady(true)

	log.Println("KVStore is running. Press Ctrl+C to stop.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	monitoring.SetReady(false) // /ready → 503: оркестратор уводит трафик до остановки

	// Graceful shutdown в строгом порядке. Цель — не потерять ни одной
	// подтверждённой записи и не словить панику send-на-закрытый-канал:
	//   1. srv.Stop()   — перестаём принимать команды и ДОЖИДАЕМСЯ завершения
	//                      in-flight обработчиков (их bw.Write уже в канале).
	//   2. ttl.Stop()   — глушим TTL-эвиктор (пишет OpVSimDel в WAL-канал).
	//   3. aiWorker     — глушим AI-воркер (пишет OpVSimAdd в WAL-канал).
	//   4. syncer.Stop()— останавливаем периодический fsync/compaction.
	//   5. bw.Close()   — дренаж WAL-канала + flush + fsync последнего батча.
	// Только после (1)-(4) закрываем канал в (5): все писатели уже заглушены.
	srv.Stop()
	ttl.Stop()
	if aiWorker != nil {
		aiWorker.Stop()
	}
	syncer.Stop()
	if err := bw.Close(); err != nil {
		log.Printf("WAL close error: %v", err)
	}
	log.Println("Shutdown complete: WAL flushed and fsynced.")
}

// writeValue — helper для записи protocol.Value в ConnBuf.
// Используется для cluster API (CheckKey, MigrateKey), которые
// всё ещё возвращают protocol.Value.
func writeValue(buf *server.ConnBuf, v protocol.Value) {
	switch v.Typ {
	case '+':
		buf.WriteSimpleString(v.Str)
	case '-':
		buf.WriteError(v.Str)
	case ':':
		buf.WriteInt(v.Num)
	case '$':
		if v.Num == -1 {
			buf.WriteNull()
		} else {
			buf.WriteBulkString(v.Str)
		}
	case '*':
		buf.WriteArrayHeader(len(v.Array))
		for _, item := range v.Array {
			writeValue(buf, item)
		}
	case 0:
		// пустой ответ (SUBSCRIBE — writePump сам отправляет)
	}
}

// buildServerTLSConfig собирает *tls.Config для серверного listener'а с явным
// MinVersion и опциональным mTLS. Если задан clientCAPath — сервер требует
// клиентский сертификат, подписанный этим CA (сетевая идентичность поверх пароля).
func buildServerTLSConfig(certPath, keyPath, minVersion, clientCAPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("не удалось загрузить сертификат/ключ: %w", err)
	}
	minVer, err := parseTLSVersion(minVersion)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}
	if clientCAPath != "" {
		caPEM, err := os.ReadFile(clientCAPath)
		if err != nil {
			return nil, fmt.Errorf("не удалось прочитать client-CA %q: %w", clientCAPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("client-CA %q: не найдено ни одного PEM-сертификата", clientCAPath)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert // mTLS
	}
	return cfg, nil
}

// parseTLSVersion переводит "1.2"/"1.3" в константу crypto/tls. Более старые
// версии не поддерживаем сознательно (TLS 1.0/1.1 сняты как небезопасные).
func parseTLSVersion(s string) (uint16, error) {
	switch s {
	case "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("недопустимая -tls-min-version %q (ожидается 1.2 или 1.3)", s)
	}
}

// resolveAuthPassword разрешает пароль AUTH в порядке приоритета:
// файл (-requirepass-file) → env (KVSTORE_REQUIREPASS) → флаг (-requirepass).
// Файл/env предпочтительнее флага, т.к. секрет не попадает в список процессов
// (ps/history//proc/<pid>/cmdline). Пустой результат = аутентификация выключена.
func resolveAuthPassword(flagVal, fileVal string) (string, error) {
	if fileVal != "" {
		b, err := os.ReadFile(fileVal)
		if err != nil {
			return "", fmt.Errorf("не удалось прочитать файл пароля %q: %w", fileVal, err)
		}
		// Trailing newline из `echo pass > file` — частый источник «неверного
		// пароля»; обрезаем окаймляющие пробелы/переводы строк.
		return strings.TrimSpace(string(b)), nil
	}
	if env := os.Getenv("KVSTORE_REQUIREPASS"); env != "" {
		return env, nil
	}
	return flagVal, nil
}

// isMemoryGrowingCmd сообщает, увеличивает ли команда использование памяти.
// Под OOM-гейт попадают только такие команды; удаляющие/читающие (DEL, VSIM.DEL,
// ZREM, EXPIRE, GET…) всегда разрешены — чтобы из состояния OOM можно было выйти
// освобождением памяти, а не заблокировать себе единственный путь наружу.
func isMemoryGrowingCmd(cmd string) bool {
	switch cmd {
	case "SET", "ZADD", "VSIM.ADD", "VSIM.ADDBIN",
		"WASM.LOAD", "WASM.LOADFILE", "AI.INGEST":
		return true
	}
	return false
}

// isWriteCmd сообщает, мутирует ли команда состояние и потому требует durable
// записи в WAL. Используется durability fail-stop гейтом: при сломанном WAL
// (ENOSPC/I/O error) ВСЕ такие команды отклоняются — включая удаляющие
// (DEL/VSIM.DEL/ZREM/EXPIRE/PERSIST), потому что удаление тоже надо записать в
// лог, иначе оно «воскреснет» после рестарта. Это отличается от OOM-гейта
// (isMemoryGrowingCmd), где удаления РАЗРЕШЕНЫ — там цель освободить память,
// а здесь диск не может принять вообще ничего. Чтение (GET/…) не затронуто.
func isWriteCmd(cmd string) bool {
	switch cmd {
	case "SET", "DEL", "EXPIRE", "PERSIST",
		"VSIM.ADD", "VSIM.ADDBIN", "VSIM.DEL",
		"ZADD", "ZREM", "AI.INGEST":
		return true
	}
	return false
}

// arg — helper: безопасное получение string из args.
func arg(args [][]byte, i int) string {
	if i >= len(args) {
		return ""
	}
	return string(args[i])
}

// leveledStatsAdapter адаптирует *vector.LeveledVectorStore к monitoring.VectorStateProvider.
// Избегает циклической зависимости monitoring ↔ vector: конверсия LeveledStats → VectorStats
// происходит здесь, в cmd-слое.
type leveledStatsAdapter struct {
	lvs *vector.LeveledVectorStore
}

func (a leveledStatsAdapter) Stats() monitoring.VectorStats {
	s := a.lvs.Stats()
	return monitoring.VectorStats{
		TotalVectors:    s.TotalVectors,
		DeltaLen:        s.DeltaLen,
		DeltaMax:        s.DeltaMax,
		Dim:             s.Dim,
		MaxLevel:        s.MaxLevel,
		SegmentsByLevel: s.SegmentsByLevel,
		Tombstones:      s.Tombstones,
		AllocatorBytes:  s.AllocatorBytes,
		DataBytes:       s.DataBytes,
	}
}

func executeCommand(s *tcmalloc.TCMallocStore, bw *wal.BatchWAL, ttl *store.TTLManager,
	hub *pubsub.Hub, cl clusterNode, wasm computeRuntime,
	vecStore vector.VectorIndex,
	zsetReg *zset.ZSetRegistry,
	aiClient *ai.Client, aiWorker *ai.Worker,
	iterateAll func(fn func(op byte, key string, value []byte)),
	saveVectors func() error,
	cs *server.ConnState, cmd string, args [][]byte) {

	buf := cs.Buf
	workerID := cs.WorkerID

	// OOM-гейт (единая точка для всех растущих в памяти команд). Раньше проверка
	// висела только на SET — LPUSH/HSET-класс и, главное, ZADD/VSIM.ADD/VSIM.ADDBIN
	// шли мимо неё и уводили процесс в OOM. Удаляющие/читающие команды (DEL,
	// VSIM.DEL, ZREM, GET…) НЕ блокируются — иначе из OOM не выйти освобождением.
	// Покрывает и прямой путь, и EXEC (обе ветки зовут executeCommand).
	if isMemoryGrowingCmd(cmd) && s.IsOOM() {
		monitoring.OomEvents.Inc()
		buf.WriteError("OOM command not allowed when used memory > 'maxmemory'")
		return
	}

	// Durability fail-stop: если WAL перестал durable-писать на диск (ENOSPC,
	// I/O error), мы больше не можем честно подтверждать мутации. Отклоняем ВСЕ
	// пишущие команды — включая удаляющие (в отличие от OOM-гейта выше), т.к. на
	// полном диске нельзя записать даже удаление. Чтение остаётся доступным,
	// чтобы клиенты могли снять данные. Аналог Redis stop-writes-on-bgsave-error:
	// лучше явная ошибка, чем тихая потеря уже подтверждённой записи.
	if isWriteCmd(cmd) {
		if err := bw.Failed(); err != nil {
			monitoring.WalFailStop.Inc()
			buf.WriteError("WAL persistence failed, writes are blocked (durability fail-stop): " + err.Error())
			return
		}
	}

	// WASM.* команды обслуживает compute-шов (за build-tag experimental). В прод-
	// сборке это no-op-заглушка, отвечающая «WASM disabled» — весь код движка и
	// wazero в бинарь не входят.
	if strings.HasPrefix(cmd, "WASM.") {
		wasm.HandleCommand(cmd, args, buf)
		return
	}

	switch cmd {
	case "PING":
		buf.WriteSimpleString("PONG")

	// === Cluster ===
	case "CLUSTER":
		if cl != nil {
			// Конвертируем [][]byte → []protocol.Value для legacy cluster API
			pArgs := make([]protocol.Value, len(args))
			for i, a := range args {
				pArgs[i] = protocol.Value{Typ: '$', Str: string(a)}
			}
			writeValue(buf, cl.HandleClusterCommand(pArgs))
		} else {
			buf.WriteError("ERR cluster mode is not enabled")
		}

	case "MIGRATE":
		if cl == nil {
			buf.WriteError("ERR cluster mode is not enabled")
			return
		}
		if len(args) < 3 {
			buf.WriteError("ERR wrong number of arguments for 'MIGRATE'")
			return
		}
		host := string(args[0])
		port, err := strconv.Atoi(string(args[1]))
		if err != nil {
			buf.WriteError("ERR invalid port")
			return
		}
		key := string(args[2])
		writeValue(buf, cl.MigrateKey(host, port, key))

	case "PSYNC":
		if cl == nil {
			buf.WriteError("ERR cluster mode is not enabled")
			return
		}
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'PSYNC'")
			return
		}
		replicaID := string(args[0])
		cl.HandlePsync(cs.Conn, replicaID)

	case "SET":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'SET'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		// OOM защищён единым гейтом в начале executeCommand.
		// args[1] — слайс ring buffer. Нужна копия для TCMalloc (буфер будет перезаписан).
		value := make([]byte, len(args[1]))
		copy(value, args[1])

		bw.Write(wal.Entry{Op: wal.OpSet, Key: key, Value: value})
		s.Set(workerID, key, value)

		if cl != nil {
			cl.ForwardWrite(fmt.Sprintf("SET %s %s", key, string(value)))
		}
		wasm.FireSet(key, workerID)

		if len(args) >= 4 && strings.ToUpper(string(args[2])) == "EX" {
			seconds, err := strconv.Atoi(string(args[3]))
			if err != nil || seconds <= 0 {
				buf.WriteError("ERR invalid expire time")
				return
			}
			dur := time.Duration(seconds) * time.Second
			expiresAt := time.Now().Add(dur)
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], uint64(expiresAt.UnixNano()))
			bw.Write(wal.Entry{Op: wal.OpExpire, Key: key, Value: b[:]})
			ttl.Set(key, dur)
		}

		buf.WriteSimpleString("OK")

	case "GET":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'GET'")
			return
		}
		key := string(args[0])
		if cl != nil && !cl.IsReplica() {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		if ttl.IsExpired(key) {
			buf.WriteNull()
			return
		}
		val, ok := s.Get(key)
		if !ok {
			if cl != nil {
				if ask := cl.CheckKeyAsk(key); ask != nil {
					writeValue(buf, *ask)
					return
				}
			}
			buf.WriteNull()
			return
		}
		buf.WriteBulk(val)

	case "DEL":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'DEL'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		bw.Write(wal.Entry{Op: wal.OpDel, Key: key})
		ok := s.Del(workerID, key)
		vecStore.Delete(key) // Также удаляем вектор при ручном DEL
		ttl.OnDelete(key)
		if cl != nil {
			cl.ForwardWrite(fmt.Sprintf("DEL %s", key))
		}
		wasm.FireDel(key, workerID)
		if ok {
			buf.WriteInt(1)
		} else {
			buf.WriteInt(0)
		}

	case "EXPIRE":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'EXPIRE'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		if _, ok := s.Get(key); !ok {
			buf.WriteInt(0)
			return
		}
		seconds, err := strconv.Atoi(string(args[1]))
		if err != nil || seconds <= 0 {
			buf.WriteError("ERR invalid expire time")
			return
		}
		dur := time.Duration(seconds) * time.Second
		expiresAt := time.Now().Add(dur)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(expiresAt.UnixNano()))
		bw.Write(wal.Entry{Op: wal.OpExpire, Key: key, Value: b[:]})
		ttl.Set(key, dur)
		buf.WriteInt(1)

	case "TTL":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'TTL'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		if _, ok := s.Get(key); !ok {
			buf.WriteInt(-2)
			return
		}
		remaining := ttl.TTL(key)
		if remaining == -1 {
			buf.WriteInt(-1)
			return
		}
		buf.WriteInt(int(remaining.Seconds()))

	case "PERSIST":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'PERSIST'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		if ttl.Remove(key) {
			bw.Write(wal.Entry{Op: wal.OpPersist, Key: key})
			buf.WriteInt(1)
		} else {
			buf.WriteInt(0)
		}

	// === Pub/Sub ===
	case "SUBSCRIBE":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'SUBSCRIBE'")
			return
		}
		channels := make([]string, len(args))
		for i, a := range args {
			channels[i] = string(a)
		}
		hub.Subscribe(cs.Conn, channels)
		// writePump отправляет подтверждения, не пишем в buf

	case "UNSUBSCRIBE":
		channels := make([]string, len(args))
		for i, a := range args {
			channels[i] = string(a)
		}
		hub.Unsubscribe(cs.Conn, channels)
		buf.WriteSimpleString("OK")

	case "PUBLISH":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'PUBLISH'")
			return
		}
		count := hub.Publish(string(args[0]), string(args[1]))
		buf.WriteInt(count)

	case "DBSIZE":
		buf.WriteInt(s.Len())

	case "COMPACT":
		wal.BackgroundCompact(bw.RawWAL(), dataDir, iterateAll, saveVectors)
		buf.WriteSimpleString("OK compaction started")

	// === Vector Search ===
	case "VSIM.ADDBIN":
		if len(args) != 2 {
			buf.WriteError("ERR usage: VSIM.ADDBIN <key> <binary_vec_bytes>")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		if len(args[1])%4 != 0 {
			buf.WriteError("ERR invalid binary vector: byte length must be a multiple of 4")
			return
		}
		// Copy bytes from ring buffer for asynchronous WAL logging
		walValue := make([]byte, len(args[1]))
		copy(walValue, args[1])

		// Zero-copy cast to []float32
		vec := vector.DeserializeVectorZeroCopy(walValue)

		bw.Write(wal.Entry{Op: wal.OpVSimAdd, Key: key, Value: walValue})
		monitoring.VectorAddTotal.Inc()
		addStart := time.Now()
		if err := vecStore.Add(key, vec); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		monitoring.VectorAddDuration.Update(time.Since(addStart).Seconds())
		if cl != nil {
			// Forward replication command in text format to replica nodes
			// to avoid breaking existing replica replication protocols.
			var sb strings.Builder
			sb.WriteString("VSIM.ADD ")
			sb.WriteString(key)
			for _, v := range vec {
				sb.WriteByte(' ')
				sb.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
			}
			cl.ForwardWrite(sb.String())
		}
		buf.WriteSimpleString("OK")

	case "VSIM.ADD":
		if len(args) < 2 {
			buf.WriteError("ERR usage: VSIM.ADD <key> <v1> <v2> ... <vN>")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		vec := make([]float32, len(args)-1)
		for i := 1; i < len(args); i++ {
			f, err := strconv.ParseFloat(unsafeString(args[i]), 32)
			if err != nil {
				buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i, unsafeString(args[i])))
				return
			}
			vec[i-1] = float32(f)
		}
		walValue := vector.SerializeVector(vec)
		bw.Write(wal.Entry{Op: wal.OpVSimAdd, Key: key, Value: walValue})
		monitoring.VectorAddTotal.Inc()
		addStart := time.Now()
		if err := vecStore.Add(key, vec); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		monitoring.VectorAddDuration.Update(time.Since(addStart).Seconds())
		if cl != nil {
			// Формат: VSIM.ADD key 0.1 0.2 0.3 ...
			var sb strings.Builder
			sb.WriteString("VSIM.ADD ")
			sb.WriteString(key)
			for _, v := range vec {
				sb.WriteByte(' ')
				sb.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
			}
			cl.ForwardWrite(sb.String())
		}
		buf.WriteSimpleString("OK")

	case "VSIM.DEL":
		if len(args) < 1 {
			buf.WriteError("ERR usage: VSIM.DEL <key>")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		monitoring.VectorDeleteTotal.Inc()
		if vecStore.Delete(key) {
			bw.Write(wal.Entry{Op: wal.OpVSimDel, Key: key})
			if cl != nil {
				cl.ForwardWrite("VSIM.DEL " + key)
			}
			buf.WriteInt(1)
		} else {
			buf.WriteInt(0)
		}

	// === Semantic Pub/Sub (Vector-routed) ===
	case "VSIM.SUBSCRIBE":
		// Формат: VSIM.SUBSCRIBE <threshold> <v1> <v2> ... <vN>
		if len(args) < 2 {
			buf.WriteError("ERR usage: VSIM.SUBSCRIBE <threshold> <v1> <v2> ... <vN>")
			return
		}
		threshold, err := strconv.ParseFloat(unsafeString(args[0]), 32)
		if err != nil || threshold < 0 {
			buf.WriteError("ERR invalid threshold (must be non-negative float)")
			return
		}
		vec := make([]float32, len(args)-1)
		for i := 1; i < len(args); i++ {
			f, err := strconv.ParseFloat(unsafeString(args[i]), 32)
			if err != nil {
				buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i, unsafeString(args[i])))
				return
			}
			vec[i-1] = float32(f)
		}
		if _, err := hub.SemanticSubscribe(cs.Conn, vec, float32(threshold)); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		// writePump отправляет подтверждение, не пишем в buf

	case "VSIM.UNSUBSCRIBE":
		if hub.SemanticUnsubscribe(cs.Conn) {
			buf.WriteSimpleString("OK")
		} else {
			buf.WriteSimpleString("OK") // idempotent: OK даже если не был подписан
		}

	case "VSIM.PUBLISH":
		// Формат: VSIM.PUBLISH <message> <v1> <v2> ... <vN>
		if len(args) < 2 {
			buf.WriteError("ERR usage: VSIM.PUBLISH <message> <v1> <v2> ... <vN>")
			return
		}
		message := string(args[0])
		vec := make([]float32, len(args)-1)
		for i := 1; i < len(args); i++ {
			f, err := strconv.ParseFloat(unsafeString(args[i]), 32)
			if err != nil {
				buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i, unsafeString(args[i])))
				return
			}
			vec[i-1] = float32(f)
		}
		count := hub.SemanticPublish(vec, message)
		buf.WriteInt(count)

	case "VSIM.SEARCH":
		if len(args) < 2 {
			buf.WriteError("ERR usage: VSIM.SEARCH <K> <v1> <v2> ... <vN>")
			return
		}
		K, err := strconv.Atoi(unsafeString(args[0]))
		if err != nil || K <= 0 {
			buf.WriteError("ERR invalid K (must be positive integer)")
			return
		}
		query := make([]float32, len(args)-1)
		for i := 1; i < len(args); i++ {
			f, err := strconv.ParseFloat(unsafeString(args[i]), 32)
			if err != nil {
				buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i, unsafeString(args[i])))
				return
			}
			query[i-1] = float32(f)
		}
		monitoring.VectorSearchTotal.Inc()
		searchStart := time.Now()
		results, err := vecStore.Search(query, K, nil)
		monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		buf.WriteArrayHeader(len(results) * 2)
		for _, r := range results {
			buf.WriteBulkString(r.Key)
			buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
		}

	case "VSIM.SEARCHBIN":
		if len(args) != 2 {
			buf.WriteError("ERR usage: VSIM.SEARCHBIN <K> <binary_vec_bytes>")
			return
		}
		K, err := strconv.Atoi(unsafeString(args[0]))
		if err != nil || K <= 0 {
			buf.WriteError("ERR invalid K (must be positive integer)")
			return
		}
		if len(args[1])%4 != 0 {
			buf.WriteError("ERR invalid binary vector: byte length must be a multiple of 4")
			return
		}

		// Zero-copy cast directly from the read buffer
		query := vector.DeserializeVectorZeroCopy(args[1])

		monitoring.VectorSearchTotal.Inc()
		searchStart := time.Now()
		results, err := vecStore.Search(query, K, nil)
		monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		buf.WriteArrayHeader(len(results) * 2)
		for _, r := range results {
			buf.WriteBulkString(r.Key)
			buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
		}

	case "VSIM.INFO":
		// FlushDeltaSync: гарантируем, что к моменту чтения состояния все вставленные
		// векторы перенесены из дельты в сегменты. Иначе при burst-write Info() видит
		// только частичные данные (компакция в полёте), а последующий Search работает
		// по неполному индексу. Бенчмарки (ann_bench) зовут VSIM.INFO как sync-точку.
		if lvs, ok := vecStore.(*vector.LeveledVectorStore); ok {
			lvs.FlushDeltaSync()
		}
		count, dim, maxLevel := vecStore.Info()
		info := fmt.Sprintf("vectors:%d dimension:%d max_level:%d", count, dim, maxLevel)
		buf.WriteBulkString(info)

	// VSIM.SEARCHFILTER — поиск ближайших векторов с фильтрацией по метаданным.
	//
	// Формат:  VSIM.SEARCHFILTER <K> <filter_field> <filter_value> <v1> <v2> ... <vN>
	//
	// Для каждого вектора с ключом X проверяется:
	//   GET "filter_field:X" == filter_value
	//
	// Пример: если вектор имеет ключ "product:123", а метаданные хранятся как
	//   SET "category:product:123" "electronics"
	// то команда:
	//   VSIM.SEARCHFILTER 10 category electronics 0.1 0.2 0.3 ...
	// найдёт 10 ближайших векторов только из категории "electronics".
	//
	// Альтернативный режим с PREFIX:
	//   VSIM.SEARCHFILTER <K> PREFIX <prefix> <v1> <v2> ... <vN>
	// Фильтрует по префиксу ключа вектора. Например:
	//   VSIM.SEARCHFILTER 10 PREFIX product: 0.1 0.2 0.3 ...
	// найдёт только векторы, чей ключ начинается с "product:".
	case "VSIM.SEARCHFILTER":
		if len(args) < 4 {
			buf.WriteError("ERR usage: VSIM.SEARCHFILTER <K> <filter_field> <filter_value> <v1> <v2> ... <vN>  or  VSIM.SEARCHFILTER <K> PREFIX <prefix> <v1> <v2> ... <vN>")
			return
		}
		K, err := strconv.Atoi(unsafeString(args[0]))
		if err != nil || K <= 0 {
			buf.WriteError("ERR invalid K (must be positive integer)")
			return
		}
		filterField := string(args[1])
		filterValue := string(args[2])
		vecArgs := args[3:]

		if len(vecArgs) == 0 {
			buf.WriteError("ERR no vector components provided")
			return
		}

		query := make([]float32, len(vecArgs))
		for i, a := range vecArgs {
			f, err := strconv.ParseFloat(unsafeString(a), 32)
			if err != nil {
				buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i+3, unsafeString(a)))
				return
			}
			query[i] = float32(f)
		}

		var filterFn func(string) bool

		if strings.ToUpper(filterField) == "PREFIX" {
			// Режим PREFIX: фильтрация по префиксу ключа вектора.
			prefix := filterValue
			filterFn = func(key string) bool {
				return strings.HasPrefix(key, prefix)
			}
		} else {
			// Режим KV: проверяем метаданные в KV Store.
			// Для вектора с ключом X ищем: GET "field:X" == value
			field := filterField
			value := filterValue
			filterFn = func(key string) bool {
				metaKey := field + ":" + key
				val, ok := s.Get(metaKey)
				return ok && string(val) == value
			}
		}

		monitoring.VectorSearchTotal.Inc()
		searchStart := time.Now()
		results, err := vecStore.SearchFiltered(query, K, filterFn, nil)
		monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		buf.WriteArrayHeader(len(results) * 2)
		for _, r := range results {
			buf.WriteBulkString(r.Key)
			buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
		}

	// === Sorted Sets (ZSET) ===
	case "ZADD":
		// ZADD <setName> <score> <member>
		if len(args) < 3 {
			buf.WriteError("ERR usage: ZADD <key> <score> <member>")
			return
		}
		setName := string(args[0])
		score, err := strconv.ParseFloat(unsafeString(args[1]), 64)
		if err != nil {
			buf.WriteError("ERR invalid score (must be a float)")
			return
		}
		member := string(args[2])
		isNew := zsetReg.ZAdd(workerID, setName, score, member)
		// WAL
		bw.Write(wal.Entry{Op: wal.OpZAdd, Key: setName, Value: zset.EncodeZAddValue(score, member)})
		if isNew {
			buf.WriteInt(1)
		} else {
			buf.WriteInt(0)
		}

	case "ZSCORE":
		// ZSCORE <setName> <member>
		if len(args) < 2 {
			buf.WriteError("ERR usage: ZSCORE <key> <member>")
			return
		}
		setName := string(args[0])
		member := string(args[1])
		score, ok := zsetReg.ZScore(setName, member)
		if !ok {
			buf.WriteNull()
		} else {
			buf.WriteBulkString(strconv.FormatFloat(score, 'f', -1, 64))
		}

	case "ZREM":
		// ZREM <setName> <member>
		if len(args) < 2 {
			buf.WriteError("ERR usage: ZREM <key> <member>")
			return
		}
		setName := string(args[0])
		member := string(args[1])
		ok := zsetReg.ZRem(workerID, setName, member)
		if ok {
			bw.Write(wal.Entry{Op: wal.OpZRem, Key: setName, Value: []byte(member)})
			buf.WriteInt(1)
		} else {
			buf.WriteInt(0)
		}

	case "ZRANGEBYSCORE":
		// ZRANGEBYSCORE <setName> <min> <max> [WITHSCORES]
		if len(args) < 3 {
			buf.WriteError("ERR usage: ZRANGEBYSCORE <key> <min> <max> [WITHSCORES]")
			return
		}
		setName := string(args[0])
		minScore, err := strconv.ParseFloat(unsafeString(args[1]), 64)
		if err != nil {
			buf.WriteError("ERR invalid min score")
			return
		}
		maxScore, err := strconv.ParseFloat(unsafeString(args[2]), 64)
		if err != nil {
			buf.WriteError("ERR invalid max score")
			return
		}
		withScores := len(args) >= 4 && strings.ToUpper(string(args[3])) == "WITHSCORES"

		results := zsetReg.ZRangeByScore(setName, minScore, maxScore)
		if withScores {
			buf.WriteArrayHeader(len(results) * 2)
			for _, r := range results {
				buf.WriteBulkString(r.Member)
				buf.WriteBulkString(strconv.FormatFloat(r.Score, 'f', -1, 64))
			}
		} else {
			buf.WriteArrayHeader(len(results))
			for _, r := range results {
				buf.WriteBulkString(r.Member)
			}
		}

	case "ZCARD":
		// ZCARD <setName>
		if len(args) < 1 {
			buf.WriteError("ERR usage: ZCARD <key>")
			return
		}
		setName := string(args[0])
		buf.WriteInt(zsetReg.ZCard(setName))

	// === VSIM.SEARCHRANGE — комбинированный поиск: вектор + score range ===
	//
	// VSIM.SEARCHRANGE <K> <setName> <minScore> <maxScore> <v1> <v2> ... <vN>
	//
	// Поток:
	//   1. B+Tree RangeSearch → множество допустимых member'ов (O(log n + k))
	//   2. HNSW SearchFiltered с filterFn = "member в множестве" → K лучших
	//
	// Без B+Tree пришлось бы делать N вызовов GET для каждого кандидата HNSW.
	case "VSIM.SEARCHRANGE":
		if len(args) < 5 {
			buf.WriteError("ERR usage: VSIM.SEARCHRANGE <K> <setName> <minScore> <maxScore> <v1> <v2> ... <vN>")
			return
		}
		K, err := strconv.Atoi(unsafeString(args[0]))
		if err != nil || K <= 0 {
			buf.WriteError("ERR invalid K (must be positive integer)")
			return
		}
		setName := string(args[1])
		minScore, err := strconv.ParseFloat(unsafeString(args[2]), 64)
		if err != nil {
			buf.WriteError("ERR invalid minScore")
			return
		}
		maxScore, err := strconv.ParseFloat(unsafeString(args[3]), 64)
		if err != nil {
			buf.WriteError("ERR invalid maxScore")
			return
		}
		vecArgs := args[4:]
		if len(vecArgs) == 0 {
			buf.WriteError("ERR no vector components provided")
			return
		}
		query := make([]float32, len(vecArgs))
		for i, a := range vecArgs {
			f, err := strconv.ParseFloat(unsafeString(a), 32)
			if err != nil {
				buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i+4, unsafeString(a)))
				return
			}
			query[i] = float32(f)
		}

		// Шаг 1: B+Tree RangeCollectHashes → множество memberHash'ей (1 alloc)
		// Оптимизация: вместо 113 allocs (resolveMember × N) — 1 alloc (map[uint64]).
		allowed := zsetReg.MembersInRangeHashed(setName, minScore, maxScore)
		if allowed == nil {
			// Пустой sorted set или нет элементов в диапазоне
			buf.WriteArrayHeader(0)
			return
		}

		// Шаг 2: HNSW SearchFiltered с hash-фильтром
		// HashMember(key) — inline FNV-1a (~15ns), zero-alloc.
		monitoring.VectorSearchTotal.Inc()
		searchStart := time.Now()
		results, err := vecStore.SearchFiltered(query, K, func(key string) bool {
			_, ok := allowed[btree.HashMember(key)]
			return ok
		}, nil)
		monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		buf.WriteArrayHeader(len(results) * 2)
		for _, r := range results {
			buf.WriteBulkString(r.Key)
			buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
		}

	// === AI Commands ===
	case "AI.EMBED":
		if aiClient == nil {
			buf.WriteError("ERR Ollama not available")
			return
		}
		if len(args) < 1 {
			buf.WriteError("ERR usage: AI.EMBED <text>")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		embedding, err := aiClient.Embed(ctx, string(args[0]))
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		buf.WriteArrayHeader(len(embedding))
		for _, v := range embedding {
			buf.WriteBulkString(fmt.Sprintf("%.6f", v))
		}

	case "AI.SEARCH":
		if aiClient == nil {
			buf.WriteError("ERR Ollama not available")
			return
		}
		if len(args) < 2 {
			buf.WriteError("ERR usage: AI.SEARCH <K> <text>")
			return
		}
		K, err := strconv.Atoi(string(args[0]))
		if err != nil || K <= 0 {
			buf.WriteError("ERR invalid K")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		embedding, err := aiClient.Embed(ctx, string(args[1]))
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR embed: %v", err))
			return
		}
		monitoring.VectorSearchTotal.Inc()
		searchStart := time.Now()
		results, err := vecStore.Search(embedding, K, nil)
		monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR search: %v", err))
			return
		}
		buf.WriteArrayHeader(len(results) * 2)
		for _, r := range results {
			buf.WriteBulkString(r.Key)
			buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
		}

	case "AI.ASK":
		if aiClient == nil {
			buf.WriteError("ERR Ollama not available")
			return
		}
		if len(args) < 1 {
			buf.WriteError("ERR usage: AI.ASK <question>")
			return
		}
		question := string(args[0])
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// RAG Step 1: вопрос → embedding
		embedding, err := aiClient.Embed(ctx, question)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR embed: %v", err))
			return
		}

		// RAG Step 2: поиск похожих документов
		monitoring.VectorSearchTotal.Inc()
		searchStart := time.Now()
		results, err := vecStore.Search(embedding, 3, nil)
		monitoring.VectorSearchDuration.Update(time.Since(searchStart).Seconds())
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR search: %v", err))
			return
		}

		// RAG Step 3: достаём оригинальные тексты из KV Store
		var contextParts []string
		for _, r := range results {
			if val, ok := s.Get(r.Key); ok {
				contextParts = append(contextParts, fmt.Sprintf("[%s]: %s", r.Key, string(val)))
			}
		}
		if len(contextParts) == 0 {
			buf.WriteError("ERR no documents found for context")
			return
		}

		// RAG Step 4: собираем промпт и отправляем в LLM
		prompt := fmt.Sprintf("Контекст:\n%s\n\nВопрос: %s\n\nОтветь кратко на основе контекста выше.",
			strings.Join(contextParts, "\n"), question)

		answer, err := aiClient.Chat(ctx, prompt)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR chat: %v", err))
			return
		}
		buf.WriteBulkString(answer)

	case "AI.INGEST":
		if aiWorker == nil {
			buf.WriteError("ERR Ollama not available")
			return
		}
		if len(args) < 2 {
			buf.WriteError("ERR usage: AI.INGEST <key> <text>")
			return
		}
		if err := aiWorker.Submit(string(args[0]), string(args[1])); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		buf.WriteSimpleString("QUEUED")

	default:
		buf.WriteError(fmt.Sprintf("ERR unknown command '%s'", cmd))
	}
}

// ═══════════════════════════════════════════════════
// compositeEvictor — TTL-expire удаляет и KV, и вектор
// ═══════════════════════════════════════════════════
//
// Когда TTLManager обнаруживает просроченный ключ, он вызывает Del(key).
// compositeEvictor выполняет три действия:
//
//  1. Удаляет значение из KV Store (TCMalloc)
//  2. Если ключ имеет ассоциированный вектор в HNSW графе — удаляет его,
//     освобождая память в VectorArena и TCMalloc (neighbors блок)
//  3. Записывает OpVSimDel в WAL, чтобы удаление пережило рестарт
//
// Без этого векторы с TTL «утекали» — KV-ключ удалялся,
// а вектор оставался навечно в графе HNSW, занимая RAM.
type compositeEvictor struct {
	kv  *tcmalloc.TCMallocStore
	vec vector.VectorIndex
	wal *wal.BatchWAL
}

func (e *compositeEvictor) Del(key string) bool {
	kvDeleted := e.kv.Del(0, key)

	vecDeleted := e.vec.Delete(key)
	if vecDeleted {
		e.wal.Write(wal.Entry{Op: wal.OpVSimDel, Key: key})
	}

	return kvDeleted || vecDeleted
}

// SendVectorToNode отправляет VSIM.ADD key v1 v2 ... vN на удалённую ноду через TCP.
func SendVectorToNode(addr, key string, vec []float32) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Формируем массив RESP аргументов для команды VSIM.ADD
	var sb strings.Builder
	// Число аргументов: 2 (VSIM.ADD + key) + len(vec)
	fmt.Fprintf(&sb, "*%d\r\n$8\r\nVSIM.ADD\r\n$%d\r\n%s\r\n", 2+len(vec), len(key), key)
	for _, v := range vec {
		vStr := strconv.FormatFloat(float64(v), 'f', -1, 32)
		fmt.Fprintf(&sb, "$%d\r\n%s\r\n", len(vStr), vStr)
	}

	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return fmt.Errorf("write to %s: %w", addr, err)
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read from %s: %w", addr, err)
	}

	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "+OK") {
		return fmt.Errorf("remote VSIM.ADD failed: %s", strings.TrimSpace(resp))
	}

	return nil
}

// snapshotIterate переснимает всё KV-состояние, живущее только в реплее WAL,
// в snapshot.wal при компакции. Вынесено из iterateAll ради тестируемости
// (стражи S1/S2): компакция удаляет старые WAL, поэтому всё, что не попадёт
// сюда, теряется после рестарта.
//
//   - обычные KV-ключи → OpSet;
//   - внутренние __zidx-ключи ПРОПУСКАЕМ — zset перестраивается из реестра
//     через OpZAdd (S2). Иначе split-brain: __zidx выживает как OpSet, а
//     дерево нет; к тому же при реплее __zidx (OpSet) ДО OpZAdd ранний return
//     ZAdd (oldScore==score) не вставил бы member в дерево;
//   - TTL → OpExpire с абсолютным временем смерти (S1): иначе после компакции
//     ключи с TTL становятся бессмертными (correctness + утечка памяти);
//   - zset-деревья → OpZAdd (score+member) (S2): реплей восстанавливает И
//     дерево, И __zidx-обратный индекс.
func snapshotIterate(
	s *tcmalloc.TCMallocStore,
	ttl *store.TTLManager,
	zsetReg *zset.ZSetRegistry,
	fn func(op byte, key string, value []byte),
) {
	s.ForEach(func(key string, value []byte) {
		if strings.HasPrefix(key, "__zidx:") {
			return
		}
		fn(wal.OpSet, key, value)
	})

	ttl.ForEach(func(key string, expiresAtUnixNano int64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(expiresAtUnixNano))
		fn(wal.OpExpire, key, b[:])
	})

	zsetReg.ForEachSet(func(setName string) {
		zsetReg.ForEach(setName, func(score float64, member string) {
			fn(wal.OpZAdd, setName, zset.EncodeZAddValue(score, member))
		})
	})
}

// execQueuedTx выполняет очередь команд MULTI/EXEC под globalTxMu.
//
// H1: defer Unlock ОБЯЗАТЕЛЕН. Если run (executeCommand) паникует на битой
// команде в очереди, её ловит recover в handleConn — но Unlock без defer был
// бы пропущен → globalTxMu залочен НАВСЕГДА → EXEC всех соединений виснут,
// воркеры застревают по одному. defer освобождает мьютекс на разворачивании
// паники, после чего она уходит в recover (соединение закрывается).
func execQueuedTx(queue [][][]byte, writeHeader func(int), run func(qCmd string, qCmdArgs [][]byte)) {
	globalTxMu.Lock()
	defer globalTxMu.Unlock()

	writeHeader(len(queue))
	for _, queuedArgs := range queue {
		qCmd := strings.ToUpper(string(queuedArgs[0]))
		run(qCmd, queuedArgs[1:])
	}
}
