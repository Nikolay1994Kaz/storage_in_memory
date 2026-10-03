package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"

	"kvstore/kvstore/internal/logging"
	"kvstore/kvstore/internal/monitoring"
	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/ship"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/kv"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

const syncInterval = 100 * time.Millisecond

// dataDir — каталог для WAL/снапшотов. Дефолт "data" (относительно рабочего
// каталога), переопределяется флагом -data-dir. var, а не const: присваивается
// один раз в main() сразу после flag.Parse(), до любого использования.
var dataDir = "data"

// version — версия сборки. Дефолт "dev" для локальных прогонов; в релизных
// сборках проставляется через -ldflags "-X main.version=$(git describe ...)"
// (см. Makefile/Dockerfile).
var version = "dev"

// restoreLSN — режим форензического восстановления на момент (0 = выключен).
// Выставляется один раз в main до приёма трафика и дальше только читается.
//
// Смысл режима: поднять состояние, каким оно было ПОСЛЕ записи с этим LSN, и
// дать по нему походить обычными командами чтения — «что агент знал в тот
// момент». Два свойства делают его пригодным для форензики, и оба обязательны:
//   - запись запрещена целиком (не «по возможности»): восстановленный узел,
//     принявший хоть одну запись, порождает вторую историю, и дальше уже
//     неизвестно, какая из них настоящая;
//   - каталог данных не изменяется вовсе — всё, что процесс пишет (новый WAL,
//     снапшоты), уводится во временный каталог. Восстановление, способное
//     испортить оригинал, в расследовании бесполезно.
var restoreLSN uint64

func main() {
	// CLI-флаги
	port := flag.Int("port", 6380, "порт для клиентов")
	bindAddr := flag.String("bind", "127.0.0.1", "интерфейс для listen (клиенты и метрики). По умолчанию только localhost; для доступа извне укажите -bind 0.0.0.0 и настройте AUTH (+TLS)")
	maxMemoryMB := flag.Int("maxmemory", 0, "лимит памяти в МБ (0 = без лимита)")
	clusterEnabled := flag.Bool("cluster", false, "включить кластерный режим")
	clusterSlotStart := flag.Int("slot-start", 0, "начало диапазона слотов")
	clusterSlotEnd := flag.Int("slot-end", 16383, "конец диапазона слотов")
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
	hnswUseSQ := flag.Bool("hnsw-use-sq", false, "Enable Scalar Quantization (int8) for frozen segments (any dim; рекомендуется для dim>256). 4x memory compression, recall ~0.97+ на реальных эмбеддингах, higher QPS via memory bandwidth")
	compactionWorkers := flag.Int("compaction-workers", 0, "Number of parallel segment build workers (0 = auto NumCPU/2 clamped 2-8). Build Pool: insert does not block during heavy L2 compaction")
	deltaShards := flag.Int("delta-shards", 1, "число шардов дельты для конкурентных вставок (1 = один граф, freeze-on-flush; >1 = ~2.4× insert @4 / ~4.25× @12 ценой search-штрафа ∝ доле дельты). См. step_profit_test.go:TestStep5_ShardedAddScaling")
	strictSegShadow := flag.Bool("strict-seg-shadow", true, "строгое затенение сегмент-vs-сегмент в поиске: после upsert стейл-копия из старого сегмента не всплывает в фильтрованных запросах даже до компакции. Цена ≤10% QPS только в мульти-сегментных переходных состояниях, 0 на консолидированном индексе (замер 23.07, TestStrictShadowQPSProbe)")
	partitionAttr := flag.String("partition-attr", "", "Categorical attribute name for tenant-contiguous layout (enables VSIM.FILTER tenant routing via columnar SearchFilter). Empty = no partition; attrs still filterable, just no tenant block-routing")
	idleConsolidate := flag.Duration("idle-consolidate", 60*time.Second, "затишье записей, после которого индекс консолидируется в один сегмент (флаш остатка дельты + merge всех уровней). Закрывает bulk-load→read: суб-fanout сегменты иначе не мержатся, search в разы медленнее. Guard: пропуск, если крупнейший сегмент уже ≥90% данных. 0 = выключено")
	shipURL := flag.String("ship-url", "", "continuous WAL-shipping на удалённое хранилище: file:///abs/path или s3://bucket/prefix?endpoint=...&region=... (S3-креды из env KVSTORE_S3_ACCESS_KEY/SECRET_KEY или AWS_*). Пусто = выключено")
	shipInterval := flag.Duration("ship-interval", time.Second, "период доставки WAL-хвоста; RPO при аварии ≈ этот интервал + время загрузки")
	shipRetain := flag.Int("ship-retain", 3, "сколько последних restore-точек (манифестов) хранить на удалённом хранилище")
	shipRestore := flag.Bool("ship-restore", false, "перед стартом восстановить каталог данных из -ship-url (каталог не должен содержать прежнего состояния)")
	logLevel := flag.String("log-level", "info", "уровень логирования: debug | info | warn | error")
	logFormat := flag.String("log-format", "text", "формат логов: text (человекочитаемый) | json (для агрегаторов)")
	enablePprof := flag.Bool("pprof", false, "включить /debug/pprof/* на metrics-порту для диагностики утечек/профилирования (НЕ для прода)")
	dataDirFlag := flag.String("data-dir", "data", "каталог для WAL и снапшотов (относительный путь считается от рабочего каталога)")
	restoreToLSN := flag.Uint64("restore-to-lsn", 0, "форензическое восстановление на момент: поднять состояние, каким оно было после записи с этим LSN, и обслуживать ТОЛЬКО чтение (0 = обычный старт). Каталог данных не изменяется")
	walInspect := flag.Bool("wal-inspect", false, "напечатать журнал (LSN, операция, ключ) и выйти — как найти LSN для -restore-to-lsn")
	showVersion := flag.Bool("version", false, "вывести версию и выйти")
	flag.Parse()

	dataDir = *dataDirFlag
	restoreLSN = *restoreToLSN

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if *walInspect {
		if err := inspectWAL(dataDir, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "wal-inspect: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Настраиваем структурный логгер (slog) до первой лог-строки: уровень и
	// формат управляются флагами, всё остальное логируется через slog-дефолт.
	logging.Setup(*logLevel, *logFormat)

	slog.Info("KVStore starting", "version", version)

	// Разрешение пароля AUTH из (в порядке приоритета): файл → env → флаг.
	// Файл/env предпочтительнее флага — секрет не попадает в ps/history/proc.
	authPassword, err := resolveAuthPassword(*requirePass, *requirePassFile)
	if err != nil {
		logging.Fatalf("requirepass: %v", err)
	}
	if authPassword != "" && *requirePassFile == "" && os.Getenv("KVSTORE_REQUIREPASS") == "" {
		slog.Warn("пароль задан через флаг -requirepass — он виден в списке процессов; " +
			"для прода используйте -requirepass-file или env KVSTORE_REQUIREPASS")
	}
	// Предвычисляем SHA-256 пароля один раз: на AUTH сравниваем дайджесты
	// constant-time (crypto/subtle) — без утечки длины/содержимого по таймингу.
	authEnabled := authPassword != ""
	authHash := sha256.Sum256([]byte(authPassword))

	// KV-хранилище: 256 шардов map[string][]byte под RWMutex (internal/store/kv).
	s := kv.New()

	// build-info метрика: kvstore_build_info{version="..."} = 1
	monitoring.SetBuildInfo(version)

	// Метрики памяти: учтённый объём (гейт maxmemory), лимит, число ключей.
	monitoring.InitMemoryMetrics(
		func() float64 { return float64(s.UsedMemory()) },
		func() float64 { return float64(s.MaxMemory()) },
		func() float64 { return float64(s.Len()) },
	)

	// Лимит памяти
	if *maxMemoryMB > 0 {
		s.SetMaxMemory(int64(*maxMemoryMB) * 1024 * 1024)
		slog.Info("max memory configured", "MB", *maxMemoryMB)
	}

	os.MkdirAll(dataDir, 0755)
	if err := checkDataDirSupported(dataDir); err != nil {
		logging.Fatalf("%v", err)
	}

	// outDir — куда пишет ЭТОТ процесс. В обычном режиме = dataDir; в режиме
	// восстановления на момент — временный каталог, поэтому оригинал остаётся
	// нетронутым (см. restore.go): расследование не должно уметь испортить
	// вещдок, по которому ведётся.
	outDir, cleanupOutDir, err := restoreOutDir(dataDir)
	if err != nil {
		logging.Fatalf("failed to prepare restore workspace: %v", err)
	}
	defer cleanupOutDir()
	if restoreLSN > 0 {
		slog.Warn("point-in-time restore mode: read-only, data directory is not modified",
			"restore_to_lsn", restoreLSN, "data_dir", dataDir, "scratch_dir", outDir)
	}

	// === WAL-shipping: remote + restore (до загрузки снапшотов/реплея) ===
	// Remote открываем ДО recovery: битый -ship-url должен уронить процесс
	// сразу и громко, а не после минут реплея.
	var shipRemote ship.Remote
	if *shipURL != "" {
		var err error
		shipRemote, err = ship.OpenRemote(*shipURL)
		if err != nil {
			logging.Fatalf("ship: %v", err)
		}
	}
	if *shipRestore {
		if shipRemote == nil {
			logging.Fatalf("ship: -ship-restore требует -ship-url")
		}
		sum, err := ship.Restore(context.Background(), shipRemote, dataDir)
		if err != nil {
			logging.Fatalf("ship: restore failed: %v", err)
		}
		slog.Info("ship: восстановлено из манифеста",
			"gen", sum.Gen, "snapshot", sum.Snapshot, "graph", sum.Graph,
			"wal_files", sum.WALFiles, "mb", fmt.Sprintf("%.1f", float64(sum.Bytes)/(1024*1024)))
	}

	// === 1. TTL Manager ===
	// Инициализация перенесена после WAL (строка ниже), потому что
	// CompositeEvictor при TTL-expire записывает OpVSimDel в WAL.
	// Для фазы WAL replay используем временный KV-only evictor.
	ttl := store.NewTTLManager(s)
	// ttl.Stop() вызывается явно в упорядоченном shutdown (не через defer):
	// TTL-эвиктор пишет в WAL-канал, поэтому его надо заглушить ДО bw.Close().

	// === 2. Инициализация хранилища векторов и загрузка бинарного снапшота ===
	// LeveledVectorStore: LSM из HNSW-сегментов (дельта → frozen/SQ8/hnsw).
	vecStore := vector.NewLeveledVectorStore(vector.LeveledConfig{
		M:              *hnswM,
		EfConstruction: *hnswEfConstruction,
		EfSearch:       *hnswEfSearch,
		Distance:       vector.EuclideanDistance,
		UseSQ:          *hnswUseSQ,
		NumBuilders:    *compactionWorkers,
		PartitionAttr:  *partitionAttr,
		DeltaShards:    *deltaShards,

		StrictSegShadow:      *strictSegShadow,
		IdleConsolidateAfter: *idleConsolidate,
	})

	// Регистрация gauge-метрик векторного хранилища (segments/tombstones/bytes).
	// Provider вызывается только при scrape /metrics — 0 overhead на hot path.
	monitoring.SetVectorStateProvider(leveledStatsAdapter{lvs: vecStore})

	// HTTP-сервер метрик/здоровья поднимаем ДО реплея WAL. Восстановление после
	// многочасовой нагрузки может занять минуты; без раннего слушателя процесс не
	// отвечает ни на /health, ни на /metrics всё это время — оркестратор (или
	// soak-хелпер) посчитал бы его мёртвым и убил посреди recovery. /health отдаёт
	// 200 сразу (порт открыт), а /ready держит 503 до SetReady(true) ниже — трафик
	// не пойдёт, пока снапшот+WAL не накатаны и listener не поднят.
	if *metricsPort > 0 {
		monitoring.StartHttpServer(*bindAddr, *metricsPort, *enablePprof)
	}

	// === 2.5. Инициализация sorted sets (ZSet) ===
	zsetReg := zset.New(s.MemoryCounter())

	graphPath := filepath.Join(dataDir, "graph_leveled.bin")
	graphLoaded := false

	if _, err := os.Stat(graphPath); err == nil {
		slog.Info("loading leveled vector store from binary snapshot", "path", graphPath)
		f, err := os.Open(graphPath)
		if err == nil {
			if err := vecStore.LoadBinary(f); err != nil {
				slog.Warn("failed to load vector snapshot, will rebuild from WAL", "err", err)
			} else {
				slog.Info("leveled vector store loaded from snapshot")
				graphLoaded = true
			}
			f.Close()
		}
	}

	// === 3. Восстановление состояния из WAL ===

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
		vecWatermark = vecStore.SnapshotLSN()
	}
	// nextLSN обязан быть выше vecWatermark: снапшот покрывает LSN ≤ него, новые
	// операции не должны переиспользовать эти номера (иначе резюмируемая
	// репликация ломается, а skipVec ошибочно пропустит свежую запись).
	bumpLSN(vecWatermark)

	// Шаг A: Сначала читаем и накатываем snapshot.wal (если есть)
	snapshotPath := filepath.Join(dataDir, "snapshot.wal")
	snapWatermark, snapshotEntries, err := wal.ReadFile(snapshotPath)
	if err != nil {
		logging.Fatalf("failed to read snapshot.wal: %v", err)
	}
	bumpLSN(snapWatermark) // watermark: snapshot покрывает состояние до этого LSN

	// Цель восстановления обязана быть достижима честно — иначе отказ с
	// названным самым ранним LSN, а не молча «почти то состояние».
	if err := checkRestoreReachable(vecWatermark, snapWatermark); err != nil {
		logging.Fatalf("%v", err)
	}

	applier := &walApplier{
		s: s, ttl: ttl, vec: vecStore, zsetReg: zsetReg,
		graphLoaded: graphLoaded, vecWatermark: vecWatermark,
	}

	for _, entry := range snapshotEntries {
		applier.apply(entry, true)
	}

	// Шаг B: Потом читаем и накатываем все wal_*.log файлы в правильном порядке
	matches, _ := filepath.Glob(filepath.Join(dataDir, "wal_*.log"))
	sort.Strings(matches) // Сортируем по имени (по времени создания)

	for _, path := range matches {
		_, logEntries, err := wal.ReadFile(path)
		if err != nil {
			logging.Fatalf("failed to read WAL log %s: %v", path, err)
		}
		for _, entry := range logEntries {
			// Условие остановки восстановления на момент: всё, что случилось
			// ПОЗЖЕ цели, просто не применяется. Журнал append-only, поэтому
			// «отмотать» — это перестать читать, а не откатывать назад.
			if restoreLSN > 0 && entry.LSN > restoreLSN {
				continue
			}
			bumpLSN(entry.LSN)
			applier.apply(entry, false)
		}
	}

	if err := applier.unknownOpsErr(); err != nil {
		logging.Fatalf("%v", err)
	}
	if applier.restored > 0 {
		slog.Info("restored from WAL", "operations", applier.restored, "vectors", applier.vecRestored)
	}

	// === 3. WAL ===
	walPath := filepath.Join(outDir, fmt.Sprintf("wal_%s.log", time.Now().Format("20060102_150405")))
	rawWAL, err := wal.Open(walPath)
	if err != nil {
		logging.Fatalf("failed to open WAL: %v", err)
	}

	// Выставляем счётчик LSN ДО того, как flusher начнёт присваивать номера
	// новым записям. rawWAL.Open() инициализировал его в 1; продолжаем с maxLSN+1.
	rawWAL.SetNextLSN(maxLSN + 1)

	bw := wal.NewBatchWAL(rawWAL)
	// bw.Close() вызывается явно в конце упорядоченного shutdown — ПОСЛЕ того,
	// как заглушены все писатели в WAL-канал (обработчики команд, TTL-эвиктор,
	// syncer). Иначе запоздалый bw.Write словит send на закрытый канал.

	// === TTL: подключаем CompositeEvictor ===
	// Теперь при истечении TTL ключа TTLManager автоматически:
	//   1. Удаляет значение из KV Store (как раньше)
	//   2. Удаляет вектор из векторного индекса (если ключ там есть)
	//   3. Записывает OpVSimDel в WAL (чтобы удаление пережило рестарт)
	ttl.SetEvictor(&compositeEvictor{kvs: s, vec: vecStore, wal: bw})

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
		// Watermark ДО FlushDeltaSync: любая векторная операция с LSN ≤ него
		// уже присвоила LSN, значит вектор был в дельте/сегментах и попадёт в
		// снапшот через FlushDeltaSync. Recovery пропустит такие WAL-записи —
		// иначе окно «ротация → снапшот» дублировало бы векторы при рестарте.
		vecStore.SetSnapshotWatermark(rawWAL.LastLSN())
		vecStore.FlushDeltaSync()
		graphPath := filepath.Join(outDir, "graph_leveled.bin")
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
		return wal.FsyncDir(outDir)
	}

	syncer := wal.NewSyncer(rawWAL, syncInterval, outDir, iterateAll, saveVectors)
	// syncer.Stop() вызывается явно в упорядоченном shutdown перед bw.Close().

	// === 4.5. WAL-shipping (continuous, async) ===
	// Шиппер работает поверх каталога данных (append-only WAL + иммутабельные
	// снапшоты) и не трогает write-путь: ошибки доставки не блокируют запись —
	// это внешняя durability с RPO ≈ ship-interval, наблюдаемая через метрики
	// kvstore_ship_* (за отставанием обязан следить алёрт).
	var shipper *ship.Shipper
	// В режиме восстановления шиппер молчит: отправить форензическую копию
	// в тот же remote значило бы затереть настоящую резервную историю.
	if shipRemote != nil && restoreLSN == 0 {
		shipper = ship.New(shipRemote, dataDir, ship.Options{
			Interval:        *shipInterval,
			RetainManifests: *shipRetain,
		})
		monitoring.InitShipMetrics(shipper.LagBytes, shipper.LastSuccessUnixNano)
		shipper.Start()
		slog.Info("WAL-shipping включён", "url", *shipURL, "interval", *shipInterval, "retain", *shipRetain)
	}
	// shipper.Stop() вызывается явно ПОСЛЕ bw.Close() в shutdown: финальный тик
	// дошипливает уже-fsyncнутый хвост WAL → graceful shutdown даёт RPO=0 и удалённо.

	// === 5. Pub/Sub ===
	hub := pubsub.NewHub()

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
			logging.Fatalf("failed to start cluster: %v", err)
		}
		defer cl.StopGossip()
	}

	// === 7. Диспетчер команд (dispatch.go) ===
	cmds := &commands{
		kv: s, wal: bw, ttl: ttl, hub: hub, cluster: cl, vec: vecStore, zsets: zsetReg,
		iterateAll: iterateAll, saveVectors: saveVectors,
		authEnabled: authEnabled, authHash: authHash,
	}

	// HTTP-сервер метрик уже поднят до реплея WAL (см. выше по коду).

	// === 8. Сервер ===
	// Дефолт 127.0.0.1 — защита от «поднял попробовать и открыл в интернет»
	// (история раннего Redis до protected mode). Наружу — только явным -bind.
	if !isLoopbackBind(*bindAddr) && !authEnabled {
		slog.Warn("сервер слушает не-loopback интерфейс БЕЗ пароля — любой, кто дотянется до порта, получит полный доступ; " +
			"настройте -requirepass-file/KVSTORE_REQUIREPASS (и желательно TLS) или верните -bind 127.0.0.1")
	}
	listenAddr := net.JoinHostPort(*bindAddr, strconv.Itoa(*port))
	srv := server.NewServer(listenAddr, cmds.handle)
	srv.IdleTimeout = *idleTimeout
	srv.WriteTimeout = *writeTimeout
	srv.MaxConnections = *maxConnections
	// Отключение клиента чистит его подписки Pub/Sub. Без этого хука подписчик
	// остаётся в реестре навсегда, а его writePump висит.
	srv.OnDisconnect = hub.RemoveConn
	// Подписчик после SUBSCRIBE легитимно молчит (получает только push'и), а
	// idle-реапер считает активностью лишь приём данных — без эксемпта он рвал бы
	// подписчиков ровно через idle-timeout (Redis тоже не применяет timeout к
	// CLIENT_PUBSUB). Мёртвых peer'ов ловят TCP keepalive и ошибка writePump.
	srv.IdleExempt = hub.IsSubscriber
	// H3: медленный подписчик отключается через единую точку учёта сервера.
	hub.SetOnSlowClose(srv.CloseConn)

	// TLS: если указаны сертификат и ключ — включаем шифрование.
	if *tlsCert != "" && *tlsKey != "" {
		cfg, err := buildServerTLSConfig(*tlsCert, *tlsKey, *tlsMinVersion, *tlsClientCA)
		if err != nil {
			logging.Fatalf("TLS: %v", err)
		}
		srv.TLSConfig = cfg
		if *tlsClientCA != "" {
			slog.Info("TLS: mTLS включён — требуется клиентский сертификат, подписанный указанным CA")
		}
	}

	if err := srv.Start(); err != nil {
		logging.Fatalf("failed to start: %v", err)
	}

	// Снапшот загружен и listener поднят — помечаем процесс готовым (/ready → 200).
	monitoring.SetReady(true)

	slog.Info("KVStore is running, press Ctrl+C to stop")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutting down")
	monitoring.SetReady(false) // /ready → 503: оркестратор уводит трафик до остановки

	// Graceful shutdown в строгом порядке. Цель — не потерять ни одной
	// подтверждённой записи и не словить панику send-на-закрытый-канал:
	//   1. srv.Stop()   — перестаём принимать команды и ДОЖИДАЕМСЯ завершения
	//                      in-flight обработчиков (их bw.Write уже в канале).
	//   2. ttl.Stop()   — глушим TTL-эвиктор (пишет OpVSimDel в WAL-канал).
	//   3. syncer.Stop()— останавливаем периодический fsync/compaction.
	//   4. bw.Close()   — дренаж WAL-канала + flush + fsync последнего батча.
	// Только после (1)-(3) закрываем канал в (4): все писатели уже заглушены.
	srv.Stop()
	ttl.Stop()
	syncer.Stop()
	if err := bw.Close(); err != nil {
		slog.Error("WAL close error", "err", err)
	}
	// 6. Финальный тик шиппера ПОСЛЕ bw.Close(): последний батч WAL уже на
	// диске, поэтому штатная остановка оставляет удалённую копию с RPO=0.
	if shipper != nil {
		shipper.Stop()
		slog.Info("WAL-shipping: финальная доставка завершена")
	}
	slog.Info("shutdown complete: WAL flushed and fsynced")
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
