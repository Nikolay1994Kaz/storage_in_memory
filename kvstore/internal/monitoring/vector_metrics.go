package monitoring

import (
	"fmt"

	"github.com/VictoriaMetrics/metrics"
)

// =============================================================================
// Vector Search metrics.
//
// Назначение: data-driven тюнинг LeveledVectorStore (DeltaMax/Fanout/efSearch)
// и диагностика узких мест QPS. Без этих метрик оптимизации — гадание.
//
// Все метрики Prometheus-совместимы, экспортируются через /metrics.
// Hot-path метрики (search/add) — 0 аллокаций (обновление по указателю).
// State-метрики (segments/tombstones) — Gauge с callback, вычисляются на scrape.
// =============================================================================

// ── Latency гистограммы (hot path: Add / Search) ──────────────────────────────
//
// Используют Buckers, оптимизированные под микросекунды/миллисекунды.
// VictoriaMetrics автоматически экспонирует _bucket, _sum, _count.

var (
	// VectorAddDuration — гистограмма latency Add() на write path.
	// Ожидаемое распределение: ~50-100ns на delta-append.
	// Пики указывают на: delta swap при flush, lock contention с Search.
	VectorAddDuration = metrics.NewHistogram("kvstore_vector_add_duration_seconds")

	// VectorSearchDuration — гистограмма latency Search() на read path.
	// Ключевой SLO-индикатор. Разбивка по путям — см. VectorSearchPath ниже.
	VectorSearchDuration = metrics.NewHistogram("kvstore_vector_search_duration_seconds")

	// VectorCompactionDuration — гистограмма длительности одного цикла compaction
	// (flush delta + cascade merge). Длинный хвост = write-stall или merge-узкое место.
	VectorCompactionDuration = metrics.NewHistogram("kvstore_vector_compaction_duration_seconds")

	// VectorMergeDuration — длительность mergeSegments (построение нового HNSW).
	// Доминирующая часть compaction. Рост = пора думать о concurrent build.
	VectorMergeDuration = metrics.NewHistogram("kvstore_vector_merge_duration_seconds")

	// VectorSegmentBuildDuration — длительность buildSegment (Insert N векторов + Freeze).
	VectorSegmentBuildDuration = metrics.NewHistogram("kvstore_vector_segment_build_duration_seconds")
)

// ── Counters (hot path, инкремент через указатель — 0 аллокаций) ──────────────
var (
	// VectorAddTotal — счётчик всех Add-операций.
	VectorAddTotal = metrics.NewCounter("kvstore_vector_add_total")

	// VectorSearchTotal — счётчик всех Search-операций (включая filtered).
	VectorSearchTotal = metrics.NewCounter("kvstore_vector_search_total")

	// VectorDeleteTotal — счётчик Delete (delta hard-delete + segment tombstone).
	VectorDeleteTotal = metrics.NewCounter("kvstore_vector_delete_total")

	// VectorTombstoneTotal — счётчик tombstone-операций (Delete, попавший в сегмент).
	// Рост = deletes на скомпакченных данных. Нужно следить, чтобы не копились.
	VectorTombstoneTotal = metrics.NewCounter("kvstore_vector_tombstone_total")

	// VectorCompactionTotal — счётчик циклов compaction.
	VectorCompactionTotal = metrics.NewCounter("kvstore_vector_compaction_total")

	// VectorCompactionRollbackTotal — счётчик rollback'ов при merged==nil (data loss prevented).
	// Должен быть 0 в норме. Рост = баг или гонка в mergeSegments.
	VectorCompactionRollbackTotal = metrics.NewCounter("kvstore_vector_compaction_rollback_total")

	// VectorCompactionEpochMismatchTotal — счётчик отброшенных сегментов из-за Clear() во время compaction.
	// Должен быть 0 в норме. Рост = частые Clear во время активной компакции.
	VectorCompactionEpochMismatchTotal = metrics.NewCounter("kvstore_vector_compaction_epoch_mismatch_total")

	// VectorFlushDeltaTotal — счётчик flush'ей дельты в сегмент.
	VectorFlushDeltaTotal = metrics.NewCounter("kvstore_vector_flush_delta_total")

	// VectorMergeDeferredTotal — merge отложен из-за flush-builds в полёте
	// (приоритет ликвидации flushing-состояния, дорогого для поиска).
	VectorMergeDeferredTotal = metrics.NewCounter("kvstore_vector_merge_deferred_total")

	// VectorFlushSwapDeferredTotal — swap дельты отложен из-за полного
	// flushing-бэклога (дельта копит дальше, флаши реже и крупнее).
	VectorFlushSwapDeferredTotal = metrics.NewCounter("kvstore_vector_flush_swap_deferred_total")
)

// ── State gauges (callback, вычисляются на scrape — 0 overhead на hot path) ───
//
// Регистрируются через SetVectorStateProvider() в main.go после инициализации
// LeveledVectorStore. Gauge вызывает callback только при scrape /metrics.

// VectorStateProvider отдаёт текущее состояние LeveledVectorStore для gauge-метрик.
// Реализуется *LeveledVectorStore (метод Stats()). Инъекция через колбэк избегает
// циклической зависимости monitoring ↔ vector.
type VectorStateProvider interface {
	// Stats возвращает snapshot состояния для метрик. Должен быть thread-safe.
	Stats() VectorStats
}

// VectorStats — снимок состояния для экспорта в метрики.
type VectorStats struct {
	TotalVectors    int   // живых векторов (delta + segments)
	DeltaLen        int   // векторов в активной дельте
	DeltaMax        int   // ёмкость дельты до flush
	Dim             int   // размерность векторов
	MaxLevel        int   // максимальный заполненный уровень LSM
	SegmentsByLevel []int // [maxLevels] — число сегментов на каждом уровне
	Tombstones      int   // активных tombstones (не применённых merge'ем)
	AllocatorBytes  int64 // память, занятая под графы/соседей (tcmalloc)
	DataBytes       int64 // память под векторы (float32 slab'ы frozen/hnsw)
}

var vectorStateFunc func() VectorStats

// SetVectorStateProvider регистрирует provider для gauge-метрик векторного хранилища.
// Вызывать ОДИН раз при старте из main.go после создания LeveledVectorStore.
func SetVectorStateProvider(p VectorStateProvider) {
	vectorStateFunc = func() VectorStats { return p.Stats() }

	metrics.NewGauge("kvstore_vector_total_vectors", func() float64 {
		if vectorStateFunc != nil {
			return float64(vectorStateFunc().TotalVectors)
		}
		return 0
	})
	metrics.NewGauge("kvstore_vector_delta_len", func() float64 {
		if vectorStateFunc != nil {
			return float64(vectorStateFunc().DeltaLen)
		}
		return 0
	})
	metrics.NewGauge("kvstore_vector_delta_max", func() float64 {
		if vectorStateFunc != nil {
			return float64(vectorStateFunc().DeltaMax)
		}
		return 0
	})
	metrics.NewGauge("kvstore_vector_dim", func() float64 {
		if vectorStateFunc != nil {
			return float64(vectorStateFunc().Dim)
		}
		return 0
	})
	metrics.NewGauge("kvstore_vector_max_level", func() float64 {
		if vectorStateFunc != nil {
			return float64(vectorStateFunc().MaxLevel)
		}
		return 0
	})
	metrics.NewGauge("kvstore_vector_tombstones", func() float64 {
		if vectorStateFunc != nil {
			return float64(vectorStateFunc().Tombstones)
		}
		return 0
	})
	metrics.NewGauge("kvstore_vector_allocator_bytes", func() float64 {
		if vectorStateFunc != nil {
			return float64(vectorStateFunc().AllocatorBytes)
		}
		return 0
	})
	metrics.NewGauge("kvstore_vector_data_bytes", func() float64 {
		if vectorStateFunc != nil {
			return float64(vectorStateFunc().DataBytes)
		}
		return 0
	})

	// Per-level segment counts: kvstore_vector_segments{level="0"}, {level="1"}, ...
	// Каждый уровень — отдельный gauge с уникальным именем (метка level фиксирована
	// при создании). VictoriaMetrics требует уникальное имя без dynamic-labels здесь.
	for lvl := 0; lvl < 8; lvl++ {
		lvl := lvl // capture
		metrics.NewGauge(fmt.Sprintf(`kvstore_vector_segments{level="%d"}`, lvl), func() float64 {
			if vectorStateFunc != nil {
				s := vectorStateFunc()
				if lvl < len(s.SegmentsByLevel) {
					return float64(s.SegmentsByLevel[lvl])
				}
			}
			return 0
		})
	}
}
