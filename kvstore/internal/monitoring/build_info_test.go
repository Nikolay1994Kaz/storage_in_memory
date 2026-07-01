package monitoring

import (
	"bytes"
	"strings"
	"testing"

	"github.com/VictoriaMetrics/metrics"
)

// TestSetBuildInfo — build-info экспортируется как gauge со значением 1 и версией
// в лейбле (паттерн для джойна метрик по версии сборки в Grafana/Alerting).
func TestSetBuildInfo(t *testing.T) {
	SetBuildInfo("v9.9.9-test")

	var buf bytes.Buffer
	metrics.WritePrometheus(&buf, false)
	out := buf.String()

	want := `kvstore_build_info{version="v9.9.9-test"} 1`
	if !strings.Contains(out, want) {
		t.Fatalf("не нашли %q в выводе /metrics:\n%s", want, out)
	}
}
