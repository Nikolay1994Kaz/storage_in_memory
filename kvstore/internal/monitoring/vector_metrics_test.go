package monitoring

import (
	"strings"
	"testing"

	"github.com/VictoriaMetrics/metrics"
)

// fakeProvider — тестовая реализация VectorStateProvider.
type fakeProvider struct {
	s VectorStats
}

func (f fakeProvider) Stats() VectorStats { return f.s }

func TestVectorMetrics_CounterAndHistogramUpdate(t *testing.T) {
	// Counters инкрементятся — проверяем что не паникуют и значение растёт.
	VectorAddTotal.Inc()
	VectorAddTotal.Inc()
	VectorSearchTotal.Inc()
	VectorDeleteTotal.Inc()
	VectorTombstoneTotal.Inc()
	VectorCompactionTotal.Inc()
	VectorCompactionRollbackTotal.Inc()
	VectorCompactionEpochMismatchTotal.Inc()
	VectorFlushDeltaTotal.Inc()

	// Histograms — Update не должен паниковать.
	VectorAddDuration.Update(0.0001)
	VectorSearchDuration.Update(0.013)
	VectorCompactionDuration.Update(0.5)
	VectorMergeDuration.Update(0.4)
	VectorSegmentBuildDuration.Update(0.3)

	// Проверяем, что counter реально получил значение 2.
	got := metrics.GetOrCreateCounter("kvstore_vector_add_total").Get()
	if got != 2 {
		t.Errorf("VectorAddTotal: expected ≥2 (test-local increments), got %d", got)
	}
}

func TestVectorMetrics_StateGauges(t *testing.T) {
	// Регистрируем provider с известными значениями.
	SetVectorStateProvider(fakeProvider{s: VectorStats{
		TotalVectors:    42000,
		DeltaLen:        1234,
		DeltaMax:        10000,
		Dim:             128,
		MaxLevel:        2,
		SegmentsByLevel: []int{1, 2, 3, 0, 0, 0, 0, 0},
		Tombstones:      42,
		DataBytes:       20480000,
	}})

	// Записываем Prometheus-вывод и проверяем наличие ожидаемых метрик.
	var sb strings.Builder
	metrics.WritePrometheus(&sb, false)
	out := sb.String()

	expectedSubstrings := []string{
		"kvstore_vector_total_vectors",
		"kvstore_vector_delta_len",
		"kvstore_vector_delta_max",
		"kvstore_vector_dim",
		"kvstore_vector_max_level",
		"kvstore_vector_tombstones",
		"kvstore_vector_data_bytes",
		`kvstore_vector_segments{level="0"}`,
		`kvstore_vector_segments{level="1"}`,
		`kvstore_vector_segments{level="2"}`,
	}
	for _, s := range expectedSubstrings {
		if !strings.Contains(out, s) {
			t.Errorf("expected metric %q in Prometheus output, missing", s)
		}
	}
}
