package vector

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/monitoring"
)

// =============================================================================
// LeveledVectorStore — LSM-style векторное хранилище с Delta + Compaction.
//
// Архитектура:
//
//	Write Path (все размерности):
//	  Add(key, vec) → DeltaSegment (O(1) append)
//	  Когда delta.Full() → trigger background compaction
//
//	Read Path:
//	  Search(q, K) → параллельно по delta + все сегменты уровней → merge top-K
//
//	Compaction (background, не блокирует reads):
//	  L0: buildHNSW(deltaMax vectors) → [CSR если dim≤256]
//	  L1: merge 4×L0 → buildHNSW(4*deltaMax) → [CSR если dim≤256]
//	  L2: merge 4×L1 → buildHNSW(16*deltaMax) → ...
//
// Параметры по умолчанию:
//
//	deltaMax = min(50_000, 100MB / (dim * 4))
//	fanout   = 4
//	CSR threshold = dim ≤ 256
//
// =============================================================================

const (
	// csrDimThreshold — граница: dim ≤ N → freeze в CSR (+14% QPS),
	// dim > N → оставляем plain HNSW (distance compute доминирует).
	csrDimThreshold = 256

	// defaultFanout — сколько сегментов одного уровня сливается в следующий.
	defaultFanout = 4

	// flushingBacklogCap — swap дельты откладывается, пока flushing держит ≥ N
	// дельт: каждая flushing-дельта — nShards fp32-графов с ef-полом на каждый
	// запрос (brownout 09.07: 5-6 flushing × 12 шардов ≈ 70 графов, 70% CPU).
	// Дельта копит дальше (Append растёт за maxSize) → флаши реже и крупнее.
	// Повтор swap гарантирован: applyResult каждого build ре-триггерит compact,
	// пока delta.Full(); FlushDeltaSync ре-триггерит на каждом пробуждении.
	flushingBacklogCap = 2

	// deltaHardGrowthFactor — предел роста дельты сверх maxSize: при N×maxSize
	// свапаем несмотря на flushingBacklogCap, чтобы один build (и пиковая память
	// его аллокатора) не разрастался безгранично при перегрузе вставками.
	deltaHardGrowthFactor = 4

	// maxLevels — максимальное число уровней LSM-иерархии.
	maxLevels = 8

	// Типы сегментов в бинарном снапшоте.
	segTypeFrozen    byte = 0
	segTypeHNSWFlat  byte = 1
	segTypeFrozenSQ8 byte = 2 // SQ8-compressed frozen (uint8 codes)
)

// =============================================================================
// LeveledVectorStore
// =============================================================================

// LeveledConfig — параметры хранилища.
type LeveledConfig struct {
	// DeltaMax — максимальное число векторов в дельте до flush.
	// 0 = автоматически: min(10_000, 40MB / (dim * 4)).
	DeltaMax int

	// Fanout — кол-во сегментов одного уровня до merge в следующий.
	Fanout int

	// MergeBatchMax — макс сегментов, забираемых ОДНИМ merge с уровня
	// (batch-merge). Freeze-per-shard публикует nShards мини-сегментов за флаш;
	// каскад ровно по Fanout штук проигрывает продюсеру гонку (e2e 10.07:
	// 111 мини в L0 к концу bulk-load) и пере-rebuild'ит каждый вектор на
	// каждом уровне пирамиды. Батч-заход забирает весь накопившийся префикс
	// уровня до этого капа: та же линейная стоимость на вектор, но одним
	// rebuild'ом — заходов кратно меньше, fan-out поиска падает скачком.
	// Кап ограничивает пик памяти/длительность одного merge (fp32-копии всех
	// входных векторов + построение графа). 0 = 8×Fanout; значения < Fanout
	// поднимаются до Fanout.
	MergeBatchMax int

	// M — параметр M HNSW (число соседей). 0 = default из NewGraph.
	M int

	// EfConstruction — ширина поиска при построении HNSW.
	EfConstruction int

	// EfSearch — ширина поиска при запросах.
	EfSearch int

	// Distance — функция расстояния.
	Distance DistanceFunc

	// Metric — first-class метрика для SQ8-обхода (ADC-режим). Источник истины:
	// при MetricAuto (дефолт) выводится из Distance через inferMetric один раз
	// при инициализации (legacy-мост). Новый код задаёт явно (напр. MetricCosine),
	// чтобы не полагаться на хрупкий вывод по указателю. См. distance.go.
	Metric Metric

	// UseSQ — включить Scalar Quantization (int8) для frozen-сегментов (любая dim).
	// Сжимает рабочий набор векторов 4× (float32→uint8), поднимает QPS за счёт
	// cache-locality. Recall держится ≥96% (vs 97% float32) благодаря ADC+rerank.
	// Применяется только к новым сегментам после buildSegment; существующие
	// сегменты сохраняют свой формат (segType различает в сериализации).
	UseSQ bool

	// NumBuilders — число параллельных build workers для компакции.
	// 0 = auto (NumCPU/2, clamped 2..8). Build Pool: пока один worker строит
	// тяжёлый L2-сегмент (~100s), остальные параллельно флашат дельту и строят
	// L0/L1 → вставка не блокируется при burst-write.
	NumBuilders int

	// DeltaShards — число шардов дельты для конкурентных вставок (шаг 5).
	// 0/1 = один граф (прежнее поведение). >1 = ключи роутятся hash%N в
	// независимые шарды со своими локами → Add масштабируется по числу
	// писателей. Flush чистых векторов идёт через freeze-per-shard (каждый
	// шард → свой мини-сегмент L0, без rebuild); attrs/tenant/fp32 dim>256
	// флашатся rebuild'ом из slab.
	DeltaShards int

	// TenantOf — если задана, включает тенант-локальную раскладку (Вещь 1):
	// при build/merge entries сортируются по коду тенанта (#5 ведущий ключ) →
	// каждый тенант ложится непрерывным блоком, на сегмент вешается TenantCatalog
	// с типом индекса по порогу #6. nil = выключено (прежнее поведение).
	// В тенант-режиме freeze-on-flush отключается (нужен rebuild для сортировки).
	TenantOf func(key string) uint64

	// PartitionAttr — имя категориального атрибута (колоночный слой), по которому
	// идёт тенант-локальная раскладка (заменяет TenantOf продуктовым вводом через
	// AddWithAttrs). Его dict-код служит ведущим ключом сортировки/каталога. "" =
	// слой выключен. Имеет приоритет над TenantOf, если задан.
	PartitionAttr string

	// BruteThreshold — порог #6 (абсолютный размер блока тенанта): блок ≤ порога →
	// IndexBrute, иначе IndexGraph. 0 = DefaultBruteThreshold. Действует только
	// при TenantOf != nil или PartitionAttr != "".
	BruteThreshold int

	// IdleConsolidateAfter — затишье мутаций (Add/Delete), после которого фон
	// консолидирует индекс: остаток дельты флашится, все сегменты мержатся в один.
	// Закрывает сценарий bulk-load→read: суб-fanout остаток (<Fanout сегментов на
	// уровне) обычным каскадом не мержится никогда, а search платит за каждый
	// сегмент отдельным полным обходом графа. Guard от рестройки-шторма: если
	// крупнейший сегмент уже держит ≥90% векторов, полный rebuild ради хвоста не
	// запускается (мелочь дочистит fanout-каскад). 0 = выключено.
	IdleConsolidateAfter time.Duration

	// StrictSegShadow — строгое затенение сегмент-vs-сегмент в публичных путях
	// поиска (Search*/SearchTextFilter): хит сегмента гасится, если БОЛЕЕ СВЕЖИЙ
	// сегмент содержит этот ключ, даже когда свежая копия не попала в свои хиты
	// (отфильтрована атрибутами или не матчится запросом). Закрывает стейл-
	// семантику переходных мульти-сегментных состояний (стейл-копия из старого
	// сегмента всплывала в фильтрованном поиске до компакции) ценой
	// HasKey-лукапов по более свежим сегментам на каждый хит: O(log n) во
	// frozen/SQ, O(N) в hnsw. false = прежняя семантика («затенение живёт
	// только среди хитов», компакция нормализует). Экспериментальный флаг
	// П3-прототипа: судьба решается A/B-замером цены (порог ≥0.9× QPS).
	StrictSegShadow bool
}

// compactionResult — результат build/merge goroutine, передаётся через resultChan.
// Значение (не указатель) — channel receive создаёт happens-before для всех полей.
type compactionResult struct {
	kind      taskKind
	sourceLvl int     // для merge: уровень источник
	targetLvl int     // куда добавлен результат
	epoch     uint64  // эпоха на момент GRAB
	result    segment // построенный сегмент (nil = failure)
	// results — мини-сегменты freeze-per-shard (taskFlushDelta, шардированная
	// дельта): каждый шард заморожен отдельным сегментом, applyResult публикует
	// ВСЕ атомарно под одним Lock вместе со снятием дельты с flushing. Взаимно
	// исключен с result (обычный flush/freeze кладёт один сегмент в result).
	// Пустой при result==nil = failure (all-or-nothing, как у result).
	results []segment
	oldSegs []segment // для merge: сегменты для удаления из sourceLvl
	// appliedTombstones — ключи, исключённые из merge-сегмента по tombstone.
	// applyResult чистит их tombstones ПОСЛЕ атомарной публикации и лишь если
	// ключа больше нет в других сегментах. nil для flush.
	appliedTombstones map[string]struct{}
	// flushDelta — сфлашенная дельта (taskFlushDelta): держалась searchable в
	// lvs.flushing до этого момента. applyResult удаляет её из flushing и Close'ит
	// ПОСЛЕ публикации сегмента (единственный owner Close на нормальном пути).
	flushDelta *DeltaSegment
}

// LeveledVectorStore — основное хранилище с leveled compaction.
type LeveledVectorStore struct {
	cfg LeveledConfig

	mu    sync.RWMutex
	delta *DeltaSegment
	dim   int // устанавливается при первой вставке или LoadBinary

	// flushing — дельты, сфлашиваемые в фоне: swap уже произошёл (новые Add идут в
	// lvs.delta), но сегмент ещё НЕ опубликован в levels. Держим их searchable
	// (immutable memtable á-la LSM), иначе между swap и публикацией сегмента
	// сфлашиваемые векторы невидимы — flush-visibility gap (transient false-negative).
	// Заполняется под mu.Lock при swap в handleCompactSignal; элемент удаляется и
	// Close'ится в applyResult ПОСЛЕ публикации сегмента (единственный owner Close на
	// нормальном пути — эксклюзивно к search-RLock, нет UAF). После swap дельта
	// immutable (Add в неё не пишет) → конкурентный Search по ней безопасен под RLock.
	flushing []*DeltaSegment

	// tombstoneMu сериализует COW-мутации tombstones на горячем пути Add,
	// который теперь держит lvs.mu лишь как RLock (несколько писателей сразу).
	// Чтения tombstones остаются lock-free через atomic.Pointer.
	tombstoneMu sync.Mutex

	// levels[i] — слой i, содержит []segment, упорядоченных по времени создания.
	// levels[0] — самые свежие малые сегменты (каждый ≈ deltaMax векторов).
	levels [maxLevels][]segment

	// tombstones — COW-множество удалённых ключей (atomic.Pointer для lock-free чтения).
	// nil = нет удалений (zero-overhead).
	tombstones atomic.Pointer[map[string]struct{}]

	// compactionEpoch — поколение данных. Bump'ится под write-lock в Clear.
	// Compactor захватывает epoch перед долгой работой (build/merge вне lock) и
	// проверяет под lock перед записью в levels — иначе остаётся TOCTOU-окно,
	// в котором Clear() зануляет levels, а compactor публикует устаревший сегмент.
	compactionEpoch atomic.Uint64

	// compacting — атомарный флаг: идёт ли фоновая/синхронная компакция.
	compacting atomic.Bool

	// compactCh — сигнальный канал для запуска фоновой компакции.
	compactCh chan struct{}

	// done — канал для остановки фонового воркера.
	done chan struct{}

	// ─── Build Pool (producer-consumer компакция) ───
	//
	// coordinateCompaction() = producer: GRAB (delta flush + overflow merges)
	// под lvs.mu, кладёт tasks в buildQueue. Никаких тяжёлых BUILD внутри.
	//
	// buildWorker() × numBuilders = consumers: BUILD (buildSegment/mergeSegments
	// вне lock) + PUBLISH (append в levels под lock + epoch check).
	//
	// Это устраняет блокировку вставки: пока один worker строит L2 (~100s),
	// остальные флашат дельту и строят L0/L1 параллельно.

	// buildQueue — очередь задач для build workers. Bounded (32) — backpressure:
	// когда полна, coordinator блокируется → insert не опережает build (анти-OOM).
	buildQueue chan *compactionTask

	// buildWG — WaitGroup для graceful shutdown build workers.
	buildWG sync.WaitGroup

	// coordWG — WaitGroup для coordinator (compactWorker).
	coordWG sync.WaitGroup

	// numBuilders — фактическое число build workers (из cfg.NumBuilders, auto).
	numBuilders int

	// closeOnce — защита от двойного close(done) в Close().
	closeOnce sync.Once

	// flushCond — механизм ожидания FlushDeltaSync. FlushDeltaSync форсит swap
	// pre-call дельты и ждёт по членству в flushing; applyResult/empty-path будят
	// cond безусловно при снятии дельты с flushing (см. FlushDeltaSync).
	flushCond *sync.Cond

	// inFlightBuilds — счётчик delta-flush задач в работе.
	// Increment перед запуском горутины, decrement в applyResult после publish/drop.
	inFlightBuilds atomic.Int32

	// buildSem — семафор: не более numBuilders builds одновременно.
	// Горутины ждут на семафоре; coordinator никогда не блокируется.
	// Ограничивает CPU и пиковую память одновременных сборок графов.
	buildSem chan struct{}

	// ─── Event Loop Coordinator ───
	//
	// Coordinator — чистый select{} event loop. Никогда не строит сам.
	//   compactCh   → grab delta → запустить goroutine build → продолжить
	//   resultChan  → атомарно применить результат в levels[] → продолжить
	//
	// Старые сегменты НЕ убираются из levels[] пока merge не завершён (нет дыр для search).
	// mergeInFlight[L] = true → merge уровня L в работе (новый не запускаем).

	// resultChan — канал результатов от build/merge goroutines → coordinator.
	// Передаёт значение (не указатель) — happens-before через channel receive.
	resultChan chan compactionResult

	// mergeInFlight — флаги "идёт merge на уровне" (по одному merge на уровень одновременно).
	mergeInFlight [maxLevels]bool

	// lastMutation — UnixNano последней мутации данных (Add/Delete/Clear/LoadBinary).
	// Источник истины для idle-детекта консолидации (IdleConsolidateAfter).
	lastMutation atomic.Int64

	// consolidateInFlight — идёт idle-консолидация (merge ВСЕХ сегментов в один).
	// Взаимоисключение с обычными merge: maybeScheduleMerges пропускается, пока
	// флаг взведён — иначе одни и те же сегменты уехали бы в два параллельных
	// merge и оба результата опубликовались (дубли данных). Под lvs.mu.
	consolidateInFlight bool

	// deltaFlushInFlight — true пока delta-flush goroutine не вернула результат.
	// Только один delta-flush одновременно.
	deltaFlushInFlight bool

	// snapshotTime — UnixNano момента последнего успешного LoadBinary (для диагностики).
	snapshotTime int64

	// snapshotLSN — LSN-watermark снапшота: «graph_leveled.bin покрывает все
	// векторные операции с LSN ≤ snapshotLSN». Пишется в заголовок при SaveBinary
	// (значение выставляется SetSnapshotWatermark перед сохранением), читается при
	// LoadBinary. Recovery пропускает реплей векторных WAL-записей с LSN ≤ этого
	// watermark — иначе операции из окна «ротация WAL → запись снапшота» попали бы
	// и в снапшот, и в новый WAL, дублируя векторы при каждом рестарте после
	// компакции. Дедуп поиска/merge (#1) добивает async-хвост (см. SetSnapshotWatermark).
	snapshotLSN uint64
}

// NewLeveledVectorStore создаёт хранилище и запускает Build Pool.
func NewLeveledVectorStore(cfg LeveledConfig) *LeveledVectorStore {
	if cfg.Fanout <= 0 {
		cfg.Fanout = defaultFanout
	}
	if cfg.MergeBatchMax <= 0 {
		cfg.MergeBatchMax = 8 * cfg.Fanout
	} else if cfg.MergeBatchMax < cfg.Fanout {
		cfg.MergeBatchMax = cfg.Fanout
	}
	if cfg.EfConstruction <= 0 {
		cfg.EfConstruction = 200
	}
	if cfg.EfSearch <= 0 {
		cfg.EfSearch = 100
	}
	if cfg.Distance == nil {
		cfg.Distance = EuclideanDistance
	}
	// Резолвим метрику ОДИН раз: при MetricAuto выводим из Distance (legacy-мост),
	// иначе синхронизируем Distance с явно заданной метрикой. После этого весь
	// SQ8-путь оперирует cfg.Metric как данными — без сравнения указателей.
	if cfg.Metric == MetricAuto {
		cfg.Metric = inferMetric(cfg.Distance)
	} else {
		cfg.Distance = cfg.Metric.DistanceFunc()
	}

	// NumBuilders: auto = NumCPU/2, clamped [2, 8].
	numBuilders := cfg.NumBuilders
	if numBuilders <= 0 {
		numBuilders = runtime.NumCPU() / 2
		if numBuilders < 2 {
			numBuilders = 2
		}
		if numBuilders > 8 {
			numBuilders = 8
		}
	}

	lvs := &LeveledVectorStore{
		cfg:         cfg,
		compactCh:   make(chan struct{}, 1),
		done:        make(chan struct{}),
		buildQueue:  make(chan *compactionTask, 32),
		resultChan:  make(chan compactionResult, 32),
		numBuilders: numBuilders,
		buildSem:    make(chan struct{}, numBuilders),
	}
	lvs.flushCond = sync.NewCond(&lvs.mu)
	// Точка отсчёта затишья — старт стора: после рестарта/LoadBinary консолидация
	// не стартует раньше полного окна IdleConsolidateAfter.
	lvs.lastMutation.Store(time.Now().UnixNano())

	// Coordinator: 1 горутина, GRAB-only, кладёт tasks в buildQueue.
	lvs.coordWG.Add(1)
	go func() {
		defer lvs.coordWG.Done()
		lvs.compactWorker()
	}()

	// Build workers: N горутин, BUILD + PUBLISH параллельно.
	for i := 0; i < numBuilders; i++ {
		lvs.buildWG.Add(1)
		go func() {
			defer lvs.buildWG.Done()
			lvs.buildWorker()
		}()
	}

	return lvs
}

// deltaMax вычисляет максимальный размер дельты для заданной размерности.
// min(50_000, 100MB / (dim * 4 bytes)).
//
// 50 000 выбрано как баланс:
// - 500k векторов → всего 10 L0 сегментов (быстрый merge, быстрый search).
// - L1 merge = 4×50k = 200k (остаётся frozen: uint32 neighbor IDs, потолка нет).
// - Delta flush занимает ~5-10s (build HNSW на 50k при M=32/efC=400).
func deltaMax(dim, cfgMax int) int {
	if cfgMax > 0 {
		return cfgMax
	}
	if dim <= 0 {
		// Defense-in-depth: ValidateVector отсекает пустой вектор до сюда, но
		// deltaMax не должен паниковать делением на ноль ни при каком входе
		// (напр. реплей отравленного до фикса WAL). Безопасное значение.
		return 1
	}
	const maxBytes = 100 * 1024 * 1024 // 100MB лимит на дельту
	bySize := maxBytes / (dim * 4)
	if bySize > 50_000 {
		return 50_000
	}
	return bySize
}

// Add добавляет вектор с заданным ключом.
// O(1) на горячем пути (append в flat slab).
func (lvs *LeveledVectorStore) Add(key string, vec []float32) error {
	return lvs.AddWithAttrs(key, vec, Attributes{})
}

// AddWithAttrs вставляет вектор с атрибутами (колоночный слой uint/multi-attr).
// Атрибуты едут с данными через дельту и переживают flush/merge. Партиционный
// атрибут (cfg.PartitionAttr) задаёт тенант-раскладку. Add — обёртка без атрибутов.
func (lvs *LeveledVectorStore) AddWithAttrs(key string, vec []float32, attrs Attributes) error {
	return lvs.addEntry(key, vec, attrs, nil)
}

// addEntry — общий путь вставки для Add/AddWithAttrs/AddDoc: полная замена
// состояния дока (вектор + attrs + термы текста; terms=nil ⇒ текста нет).
func (lvs *LeveledVectorStore) addEntry(key string, vec []float32, attrs Attributes, terms []TermTF) error {
	// Choke-point персистентности: и Add, и реплей WAL (OpVSimAdd/OpVSimAddAttrs)
	// идут сюда. Санитизируем недоверенный вход до любых сайд-эффектов — ошибка
	// вместо паники (deltaMax dim=0), тихой потери (пустой ключ = tombstone) или
	// порчи снапшота (ключ > uint16) / SQ8-калибровки (NaN/Inf).
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ValidateVector(vec); err != nil {
		return err
	}
	// Горячий путь держит lvs.mu лишь как RLock: несколько писателей идут
	// параллельно, безопасность данных — на пер-шардовых локах дельты (шаг 5).
	// Эксклюзивный Lock берут только swap дельты (compaction) и Delete/Clear.
	lvs.mu.RLock()
	if lvs.delta == nil {
		// Первая вставка: инициализируем дельту под эксклюзивным локом.
		lvs.mu.RUnlock()
		if err := lvs.initDelta(len(vec)); err != nil {
			return err
		}
		lvs.mu.RLock()
	}
	if len(vec) != lvs.dim {
		lvs.mu.RUnlock()
		return &dimMismatchError{expected: lvs.dim, got: len(vec)}
	}

	becameFull := lvs.delta.AppendDoc(key, vec, attrs, terms)
	lvs.touchMutation()

	// Если ключ ранее был удалён (есть в tombstones) — снимаем tombstone,
	// иначе Delete(key) после Add(key,...) вновь удалит живой вектор.
	lvs.removeTombstone(key)
	lvs.mu.RUnlock()

	// Если дельта ТОЛЬКО ЧТО переполнилась (ровно maxSize слотов) — сигнализируем
	// coordinator. becameFull истинен ровно у одного писателя → один сигнал, без
	// busy-loop. Coordinator swap'нет delta при первой возможности.
	if becameFull {
		lvs.triggerCompact()
	}

	return nil
}

// initDelta лениво создаёт дельту при первой вставке (под эксклюзивным локом).
// Double-check: при гонке первых вставок второй вызов застанет delta != nil.
func (lvs *LeveledVectorStore) initDelta(dim int) error {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	if lvs.delta != nil {
		if dim != lvs.dim {
			return &dimMismatchError{expected: lvs.dim, got: dim}
		}
		return nil
	}
	lvs.dim = dim
	max := deltaMax(dim, lvs.cfg.DeltaMax)
	lvs.delta = NewDeltaSegmentSharded(dim, max, lvs.cfg.Distance, lvs.cfg.M, lvs.cfg.EfConstruction, lvs.deltaShardCount())
	return nil
}

// deltaShardCount возвращает число шардов дельты (cfg.DeltaShards, >=1).
func (lvs *LeveledVectorStore) deltaShardCount() int {
	if lvs.cfg.DeltaShards > 1 {
		return lvs.cfg.DeltaShards
	}
	return 1
}

// removeTombstone снимает tombstone с ключа на горячем пути Add под RLock.
// COW-мутация сериализуется tombstoneMu (несколько писателей под RLock).
// Fast path: если tombstones пусто — ничего не делаем (zero-overhead).
func (lvs *LeveledVectorStore) removeTombstone(key string) {
	if t := lvs.tombstones.Load(); t == nil || len(*t) == 0 {
		return
	}
	lvs.tombstoneMu.Lock()
	deleteTombstone(&lvs.tombstones, key)
	lvs.tombstoneMu.Unlock()
}

// Delete помечает ключ как удалённый.
func (lvs *LeveledVectorStore) Delete(key string) bool {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	lvs.touchMutation()

	// Если в дельте — hard delete (вектор физически исчезает из дельты).
	if lvs.delta != nil && lvs.delta.Delete(key) {
		// Но тот же ключ мог жить и в сегменте, и в сфлашиваемой дельте (upsert
		// через flush: Add→flush→Add оставляет старую копию, новую — в дельте).
		// Тогда ставим tombstone на неё, иначе она воскреснет при чтении/merge.
		// Иначе — снимаем возможный прежний tombstone.
		if lvs.keyInFlushingLocked(key) || lvs.keyPhysicallyInSegmentsLocked(key) {
			addTombstone(&lvs.tombstones, key)
			return true
		}
		deleteTombstone(&lvs.tombstones, key)
		return true
	}

	// Не в активной дельте. Уже tombstoned → уже удалён.
	if t := lvs.tombstones.Load(); t != nil {
		if _, ok := (*t)[key]; ok {
			return false
		}
	}
	// Ключ мог быть в сфлашиваемой дельте (flush-окно) ИЛИ в сегменте. В обоих
	// случаях ставим tombstone: сфлашиваемую дельту мутировать НЕЛЬЗЯ (её читает
	// build-горутина), а tombstone замаскирует ключ и в получившемся сегменте.
	if !lvs.keyInFlushingLocked(key) && !lvs.keyPhysicallyInSegmentsLocked(key) {
		return false
	}
	monitoring.VectorTombstoneTotal.Inc()
	addTombstone(&lvs.tombstones, key)
	return true
}

// keyInFlushingLocked — есть ли живой ключ в какой-либо сфлашиваемой дельте
// (flush-окно между swap дельты и публикацией сегмента). Только чтение
// (Contains); мутировать flushing-дельту нельзя — её читает build-горутина.
// Под lvs.mu.
func (lvs *LeveledVectorStore) keyInFlushingLocked(key string) bool {
	for _, fd := range lvs.flushing {
		if fd.Contains(key) {
			return true
		}
	}
	return false
}

// keyPhysicallyInSegmentsLocked — есть ли ФИЗИЧЕСКАЯ копия ключа в каком-либо
// сегменте, ИГНОРИРУЯ tombstones. Нужна для решений о самом tombstone (ставить/
// снимать) — там учёт tombstone был бы циклом. Под lvs.mu.
func (lvs *LeveledVectorStore) keyPhysicallyInSegmentsLocked(key string) bool {
	for _, level := range lvs.levels {
		for _, seg := range level {
			if seg.HasKey(key) {
				return true
			}
		}
	}
	return false
}

// keyPhysicallyInOtherSegmentsLocked — как keyPhysicallyInSegmentsLocked, но
// пропускает exclude (только что построенный merge-сегмент: он по определению
// исключил применённые tombstone-ключи, скан по нему бесполезен и дорог). Под mu.
func (lvs *LeveledVectorStore) keyPhysicallyInOtherSegmentsLocked(key string, exclude segment) bool {
	for _, level := range lvs.levels {
		for _, seg := range level {
			if seg == exclude {
				continue
			}
			if seg.HasKey(key) {
				return true
			}
		}
	}
	return false
}

// clearAppliedTombstonesLocked чистит tombstones ключей, физически исчезнувших из
// индекса после merge. Кандидаты — только ключи, исключённые (применённые) в ЭТОМ
// merge; ключ чистится ⟺ его больше нет ни в одном другом живом сегменте (newSeg
// его исключил, oldSegs уже удалены свапом; копия могла остаться в не-мёрдженном
// сегменте другого уровня — тогда tombstone обязан жить). Под lvs.mu (write).
//
// Исправляет два бага прежнего in-merge-clearing: (#1) чистились ВСЕ ключи
// глобального снапшота ts, включая живущие вне oldSegs → воскрешение; (#3) чистка
// шла в merge-горутине ДО публикации — при дропе результата (epoch-mismatch/
// rollback) свапа нет, этот метод не зовётся, tombstone сохраняется.
func (lvs *LeveledVectorStore) clearAppliedTombstonesLocked(applied map[string]struct{}, newSeg segment) {
	if len(applied) == 0 {
		return
	}
	if lvs.tombstones.Load() == nil {
		return
	}
	var toClear []string
	for key := range applied {
		if !lvs.keyPhysicallyInOtherSegmentsLocked(key, newSeg) {
			toClear = append(toClear, key)
		}
	}
	deleteTombstonesBatch(&lvs.tombstones, toClear)
}

// addTombstone: COW-добавление ключа. Вызывать под lvs.mu (write).
// Дедуп: если ключ уже есть — no-op (без аллокации).
func addTombstone(p *atomic.Pointer[map[string]struct{}], key string) {
	old := p.Load()
	if old != nil {
		if _, exists := (*old)[key]; exists {
			return
		}
		nw := make(map[string]struct{}, len(*old)+1)
		for k, v := range *old {
			nw[k] = v
		}
		nw[key] = struct{}{}
		p.Store(&nw)
		return
	}
	nw := map[string]struct{}{key: {}}
	p.Store(&nw)
}

// deleteTombstone: COW-удаление ключа. Вызывать под lvs.mu (write).
// Когда множество пустеет — Store(nil), возвращая zero-overhead-состояние.
func deleteTombstone(p *atomic.Pointer[map[string]struct{}], key string) {
	cur := p.Load()
	if cur == nil {
		return
	}
	if _, ok := (*cur)[key]; !ok {
		return
	}
	if len(*cur) == 1 {
		p.Store(nil)
		return
	}
	nw := make(map[string]struct{}, len(*cur)-1)
	for k, v := range *cur {
		if k != key {
			nw[k] = v
		}
	}
	p.Store(&nw)
}

// deleteTombstonesBatch — COW-удаление набора ключей одним rebuild (без O(k²)
// пересборки на каждый ключ). Удаляет лишь присутствующие. Вызывать под lvs.mu.
func deleteTombstonesBatch(p *atomic.Pointer[map[string]struct{}], keys []string) {
	if len(keys) == 0 {
		return
	}
	cur := p.Load()
	if cur == nil {
		return
	}
	rm := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if _, ok := (*cur)[k]; ok {
			rm[k] = struct{}{}
		}
	}
	if len(rm) == 0 {
		return
	}
	if len(rm) == len(*cur) {
		p.Store(nil)
		return
	}
	nw := make(map[string]struct{}, len(*cur)-len(rm))
	for k, v := range *cur {
		if _, drop := rm[k]; !drop {
			nw[k] = v
		}
	}
	p.Store(&nw)
}

// Clear сбрасывает всё хранилище в пустое состояние.
func (lvs *LeveledVectorStore) Clear() {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()

	// Bump эпохи ПОД локом: инвариант «epoch сменился ⇒ levels уже занулён».
	// Compactor, державший старую эпоху в долгом merge вне lock, увидит новый
	// epoch при последующем Lock и выбросит устаревший сегмент.
	lvs.compactionEpoch.Add(1)

	lvs.delta = nil
	lvs.dim = 0
	for i := range lvs.levels {
		lvs.levels[i] = nil
	}
	// Снимаем сфлашиваемые дельты с searchable-списка: их эпоха устарела. Сам Close
	// НЕ здесь — in-flight build/freeze-горутины ещё читают эти дельты; их закроет
	// applyResult по epoch-mismatch, когда горутина завершится (нет UAF, нет утечки).
	lvs.flushing = nil
	lvs.tombstones.Store(nil)
	lvs.touchMutation()
}

// touchMutation отмечает момент последней мутации данных (idle-детект консолидации).
func (lvs *LeveledVectorStore) touchMutation() {
	lvs.lastMutation.Store(time.Now().UnixNano())
}

// FlushDelta принудительно флашит дельту (async — для graceful shutdown).
func (lvs *LeveledVectorStore) FlushDelta() {
	lvs.mu.Lock()
	if lvs.delta == nil || lvs.delta.Len() == 0 {
		lvs.mu.Unlock()
		return
	}
	lvs.triggerCompact()
	lvs.mu.Unlock()
}

// FlushDeltaSync — см. реализацию ниже (Build Pool версия с cond-ожиданием).

// =============================================================================
// Дополнительные методы VectorIndex
// =============================================================================

// ForEach итерирует по всем живым векторам (delta + все сегменты).
// Tombstoned ключи пропускаются. fn вызывается без блокировки lvs.mu.
func (lvs *LeveledVectorStore) ForEach(fn func(key string, vec []float32)) {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	// Snapshot tombstones под локом
	var ts map[string]struct{}
	if t := lvs.tombstones.Load(); t != nil {
		ts = *t
	}

	// Дедуп upsert-копий: ключ, переписанный после flush, живёт в 2+ источниках
	// (delta + сегмент, либо два сегмента). Обходим источники СВЕЖЕЕ-ПЕРВЫМ
	// (зеркально провенанс-рангу search) и отдаём только первую встреченную
	// копию — иначе потребитель (репликация full-sync) применил бы новую версию,
	// а затем перезатёр её устаревшей. seen под RLock: view-строки frozen-арен
	// живы, пока живы сегменты. Память O(живых ключей) — ForEach и так O(N).
	seen := make(map[string]struct{}, 256)
	shadowed := func(key string) bool {
		if _, dup := seen[key]; dup {
			return true
		}
		seen[key] = struct{}{}
		return false
	}

	// Delta (tombstones дельты обработаны внутри; пер-шардовые RLock)
	if lvs.delta != nil {
		lvs.delta.ForEachLive(func(key string, vec []float32) {
			if !shadowed(key) {
				fn(key, vec)
			}
		})
	}

	// Сфлашиваемые дельты (flush-окно): в обратном порядке (позже добавленная
	// свежее); пропускаем tombstoned (Delete в окне ставит tombstone, не мутируя
	// дельту → живая копия там ещё есть).
	for fi := len(lvs.flushing) - 1; fi >= 0; fi-- {
		lvs.flushing[fi].ForEachLive(func(key string, vec []float32) {
			if ts != nil {
				if _, deleted := ts[key]; deleted {
					return
				}
			}
			if !shadowed(key) {
				fn(key, vec)
			}
		})
	}

	// Segments свежее-первым (уровень 0 → max, внутри уровня с конца):
	// пропускаем tombstoned и затенённые ключи.
	for _, level := range lvs.levels {
		for pos := len(level) - 1; pos >= 0; pos-- {
			switch s := level[pos].(type) {
			case *frozenSegment:
				fg := s.fg
				for i := 0; i < fg.n; i++ {
					if fg.keys.view(i) == "" {
						continue
					}
					if ts != nil {
						if _, deleted := ts[fg.keys.view(i)]; deleted {
							continue
						}
					}
					if shadowed(fg.keys.view(i)) {
						continue
					}
					fn(fg.keys.clone(i), fg.data[i*fg.dim:(i+1)*fg.dim])
				}
			case *frozenSQSegment:
				fg := s.fg
				dim := fg.dim
				vecBuf := make([]float32, dim)
				for i := 0; i < fg.n; i++ {
					if fg.keys.view(i) == "" {
						continue
					}
					if ts != nil {
						if _, deleted := ts[fg.keys.view(i)]; deleted {
							continue
						}
					}
					if shadowed(fg.keys.view(i)) {
						continue
					}
					// Деквантование int8 → float32
					base := i * dim
					for d := 0; d < dim; d++ {
						vecBuf[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]
					}
					fn(fg.keys.clone(i), vecBuf)
				}
			case *hnswSegment:
				s.mu.RLock()
				for id, key := range s.keys {
					if key == "" {
						continue
					}
					if ts != nil {
						if _, deleted := ts[key]; deleted {
							continue
						}
					}
					if shadowed(key) {
						continue
					}
					node := &s.g.nodes[id]
					if node.Alive {
						fn(key, s.g.arena.Get(node.VectorOffset))
					}
				}
				s.mu.RUnlock()
			}
		}
	}
}

// Get возвращает копию вектора по ключу.
// Сначала ищет в дельте (O(1)), затем проверяет tombstones, затем сканирует сегменты.
func (lvs *LeveledVectorStore) Get(key string) ([]float32, bool) {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	// Дельта: O(1) через keyIdx map шарда — tombstones дельты обработаны внутри.
	if lvs.delta != nil {
		if out, ok := lvs.delta.Get(key); ok {
			return out, true
		}
	}

	// Tombstone check: ключ мог быть удалён из сегмента после последнего merge.
	// Проверяем ДО flushing/сегментов: Delete в flush-окне ставит tombstone, не
	// мутируя сфлашиваемую дельту (её читает build), поэтому живая копия там ещё
	// есть — tombstone обязан её замаскировать.
	if t := lvs.tombstones.Load(); t != nil {
		if _, deleted := (*t)[key]; deleted {
			return nil, false
		}
	}

	// Сфлашиваемые дельты (flush-окно между swap и публикацией сегмента): новее
	// сегментов, только чтение. Иначе Get промахивается по ключу в этом окне.
	// В обратном порядке — позже добавленная flushing-дельта свежее (провенанс,
	// зеркально memtable-порядку search): upsert-копии затеняются правильно.
	for i := len(lvs.flushing) - 1; i >= 0; i-- {
		if out, ok := lvs.flushing[i].Get(key); ok {
			return out, true
		}
	}

	// Сканирование сегментов СВЕЖЕЕ-ПЕРВЫМ, зеркально провенанс-рангу search
	// (rank = lvl<<40 - pos): уровень 0 свежее уровня 1, внутри уровня больший
	// индекс свежее. Иначе после upsert-через-flush (две frozen-копии ключа)
	// Get возвращал бы устаревшую копию из более раннего сегмента.
	for _, level := range lvs.levels {
		for pos := len(level) - 1; pos >= 0; pos-- {
			seg := level[pos]
			switch s := seg.(type) {
			case *frozenSegment:
				fg := s.fg
				if i, ok := fg.keys.find(key); ok {
					out := make([]float32, fg.dim)
					copy(out, fg.data[i*fg.dim:(i+1)*fg.dim])
					return out, true
				}
			case *frozenSQSegment:
				fg := s.fg
				dim := fg.dim
				if i, ok := fg.keys.find(key); ok {
					out := make([]float32, dim)
					base := i * dim
					for d := 0; d < dim; d++ {
						out[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]
					}
					return out, true
				}
			case *hnswSegment:
				s.mu.RLock()
				for id, k := range s.keys {
					if k == key {
						node := &s.g.nodes[id]
						if node.Alive {
							raw := s.g.arena.Get(node.VectorOffset)
							out := make([]float32, len(raw))
							copy(out, raw)
							s.mu.RUnlock()
							return out, true
						}
					}
				}
				s.mu.RUnlock()
			}
		}
	}
	return nil, false
}

// Info возвращает общее кол-во векторов, размерность и номер максимального заполненного уровня.
func (lvs *LeveledVectorStore) Info() (count, dim, maxLevel int) {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	count = 0
	if lvs.delta != nil {
		count += lvs.delta.Len()
	}
	// Сфлашиваемые дельты (flush-окно): их векторы ещё не в сегментах — без них
	// count недосчитывал бы в этом окне (тот же фикс, что в Len).
	for _, fd := range lvs.flushing {
		count += fd.Len()
	}
	maxLevel = 0
	for i, level := range lvs.levels {
		for _, seg := range level {
			count += seg.Len()
			if i > maxLevel {
				maxLevel = i
			}
		}
	}
	// Tombstones маскируют удалённые ключи, физически ещё живущие в сегментах:
	// без вычета Delete не уменьшал бы count до компакции (юзер-видимо через
	// VSIM.INFO). Вычет точен для ключей с одной физической копией; ключ,
	// апсерченный через flush (2+ копии) и затем удалённый, до merge даёт
	// временный overcount — count никогда не ЗАнижается. Clamp — страховка
	// инварианта «tombstone ⇒ есть физическая копия».
	if t := lvs.tombstones.Load(); t != nil {
		count -= len(*t)
		if count < 0 {
			count = 0
		}
	}
	return count, lvs.dim, maxLevel
}

// SetHNSWParams обновляет параметры HNSW — применяются к следующим buildSegment.
func (lvs *LeveledVectorStore) SetHNSWParams(M, efConstruction, efSearch int) {
	lvs.mu.Lock()
	defer lvs.mu.Unlock()
	if M > 0 {
		lvs.cfg.M = M
	}
	if efConstruction > 0 {
		lvs.cfg.EfConstruction = efConstruction
	}
	if efSearch > 0 {
		lvs.cfg.EfSearch = efSearch
	}
}

// Len возвращает общее число векторов (delta + все сегменты).
func (lvs *LeveledVectorStore) Len() int {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	total := 0
	if lvs.delta != nil {
		total += lvs.delta.Len()
	}
	// Сфлашиваемые дельты (flush-окно): их векторы ещё не в сегментах — без учёта
	// Len недосчитывал бы в этом окне.
	for _, fd := range lvs.flushing {
		total += fd.Len()
	}
	for _, level := range lvs.levels {
		for _, seg := range level {
			total += seg.Len()
		}
	}
	return total
}

// SnapshotTime возвращает UnixNano времени последнего успешного LoadBinary (для диагностики).
func (lvs *LeveledVectorStore) SnapshotTime() int64 { return lvs.snapshotTime }

// SnapshotLSN возвращает LSN-watermark, прочитанный последним LoadBinary (0 если
// снапшот без watermark / не загружался). Recovery пропускает векторные WAL-записи
// с LSN ≤ этого значения — они уже в снапшоте.
func (lvs *LeveledVectorStore) SnapshotLSN() uint64 { return lvs.snapshotLSN }

// SetSnapshotWatermark выставляет LSN-watermark, записываемый следующим SaveBinary.
//
// Вызывать ПЕРЕД SaveBinary со значением LastLSN() на момент НАЧАЛА сохранения
// (до FlushDeltaSync). Контракт безопасности: любая векторная операция с
// LSN ≤ watermark получила свой LSN до этого момента, значит вектор уже был в
// дельте/сегментах и попадёт в снапшот через FlushDeltaSync → пропуск такой
// записи при реплее не теряет данных. Векторы, чей WAL-LSN присвоился позже
// (async batch-flusher), но которые всё же попали в снапшот, реплеятся повторно
// и схлопываются дедупом поиска/merge (#1) — корректность сохраняется, лишь
// возможен временный дубль в памяти до ближайшего merge.
func (lvs *LeveledVectorStore) SetSnapshotWatermark(lsn uint64) {
	lvs.mu.Lock()
	lvs.snapshotLSN = lsn
	lvs.mu.Unlock()
}

// Close останавливает coordinator и build workers, дожидаясь завершения.
// Idempotent (sync.Once) — безопасен для многократного вызова.
func (lvs *LeveledVectorStore) Close() {
	lvs.closeOnce.Do(func() {
		close(lvs.done)
		close(lvs.buildQueue) // build workers дорабатывают in-flight tasks и выходят
		lvs.coordWG.Wait()    // coordinator вышел
		lvs.buildWG.Wait()    // все build workers вышли

		// Пробудить зависший в FlushDeltaSync cond.Wait, если он ждёт.
		lvs.mu.Lock()
		lvs.flushCond.Broadcast()
		lvs.mu.Unlock()
	})
}

// =============================================================================
// Утилиты
// =============================================================================

// LeveledStats — снимок состояния LeveledVectorStore для метрик/диагностики.
// Возвращается методом Stats(). Не зависит от пакета monitoring (нет import cycle).
type LeveledStats struct {
	TotalVectors    int   // живых векторов (delta + все сегменты)
	DeltaLen        int   // векторов в активной дельте
	DeltaMax        int   // ёмкость дельты до flush
	Dim             int   // размерность векторов (0 если пусто)
	MaxLevel        int   // максимальный заполненный уровень LSM (0-based)
	SegmentsByLevel []int // [maxLevels] — число сегментов на каждом уровне
	Tombstones      int   // активных tombstones (не применённых merge'ем)
	AllocatorBytes  int64 // связи HNSW изменяемых графов (дельта, hnsw-сегменты); у frozen они в DataBytes
	DataBytes       int64 // память под векторы (frozen.data + hnsw arena)
}

// Stats возвращает snapshot состояния для метрик.
// Потокобезопасен: берёт RLock на короткое время. Не делает аллокаций вне
// возвращаемого среза SegmentsByLevel (фиксированной длины maxLevels).
func (lvs *LeveledVectorStore) Stats() LeveledStats {
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()

	segsByLevel := make([]int, maxLevels)
	totalVectors := 0
	maxLevel := 0
	var dataBytes, neighborBytes int64
	for i, level := range lvs.levels {
		segsByLevel[i] = len(level)
		if len(level) > 0 && i > maxLevel {
			maxLevel = i
		}
		for _, seg := range level {
			totalVectors += seg.Len()
			dataBytes += segMemoryBytes(seg)
			if hs, ok := seg.(*hnswSegment); ok {
				hs.mu.RLock()
				neighborBytes += hs.g.NeighborBytes()
				hs.mu.RUnlock()
			}
		}
	}

	deltaLen := 0
	deltaMax := 0
	if lvs.delta != nil {
		deltaLen = lvs.delta.Len()
		deltaMax = lvs.delta.MaxSize()
		totalVectors += deltaLen
		neighborBytes += lvs.delta.NeighborBytes()
	}

	tombstones := 0
	if t := lvs.tombstones.Load(); t != nil {
		tombstones = len(*t)
	}

	return LeveledStats{
		TotalVectors:    totalVectors,
		DeltaLen:        deltaLen,
		DeltaMax:        deltaMax,
		Dim:             lvs.dim,
		MaxLevel:        maxLevel,
		SegmentsByLevel: segsByLevel,
		Tombstones:      tombstones,
		AllocatorBytes:  neighborBytes,
		DataBytes:       dataBytes,
	}
}

// segMemoryBytes возвращает приближённый размер данных векторов в сегменте.
// Не учитывает overhead структуры (keys []string, метаданные) — только payload.
func segMemoryBytes(seg segment) int64 {
	switch s := seg.(type) {
	case *frozenSegment:
		// data slab (n*dim*4) + CSR layers
		return int64(s.fg.MemoryBytes())
	case *frozenSQSegment:
		// codes (n*dim*1) + sqMin/sqScale + CSR layers
		return int64(s.fg.MemoryBytes())
	case *hnswSegment:
		s.mu.RLock()
		defer s.mu.RUnlock()
		// arena.data (float32); связи графа считаются в AllocatorBytes
		return int64(len(s.g.arena.data)) * 4
	}
	return 0
}

type dimMismatchError struct {
	expected, got int
}

func (e *dimMismatchError) Error() string {
	return "vector dimension mismatch: expected " +
		itoa(e.expected) + ", got " + itoa(e.got)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := 20
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
