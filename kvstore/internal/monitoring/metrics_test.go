package monitoring

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

func TestMetricsRecordAndExpose(t *testing.T) {
	// 1. Test active connections increment/decrement
	ActiveConnections.Inc()
	ActiveConnections.Inc()
	ActiveConnections.Dec()

	// 2. Test command recording
	RecordCommand("GET", 12*time.Millisecond)
	RecordCommand("GET", 15*time.Millisecond)
	RecordCommand("SET", 2*time.Millisecond)

	// 3. Test memory metrics callbacks
	InitMemoryMetrics(
		func() float64 { return 1024 * 1024 },
		func() float64 { return 5 },
		func() float64 { return 42 },
		func() float64 { return 10 * 1024 * 1024 },
	)

	// 4. Test WASM metrics
	RecordWasm("fraud_detector", "process", 3*time.Millisecond)

	// 5. Output Prometheus format
	var buf bytes.Buffer
	metrics.WritePrometheus(&buf, false)
	output := buf.String()

	t.Log(output)

	// 6. Assertions
	if !strings.Contains(output, "kvstore_network_active_connections 1") {
		t.Errorf("Expected active connections to be 1, output:\n%s", output)
	}
	if !strings.Contains(output, `kvstore_commands_total{command="GET"} 2`) {
		t.Errorf("Expected GET command count to be 2, output:\n%s", output)
	}
	if !strings.Contains(output, `kvstore_commands_total{command="SET"} 1`) {
		t.Errorf("Expected SET command count to be 1, output:\n%s", output)
	}
	if !strings.Contains(output, "kvstore_memory_used_bytes 1048576") {
		t.Errorf("Expected memory used bytes to be 1048576, output:\n%s", output)
	}
	if !strings.Contains(output, "kvstore_tcmalloc_chunks_total 5") {
		t.Errorf("Expected chunks total to be 5, output:\n%s", output)
	}
	if !strings.Contains(output, "kvstore_tcmalloc_spans_total 42") {
		t.Errorf("Expected spans total to be 42, output:\n%s", output)
	}
	if !strings.Contains(output, "kvstore_memory_limit_bytes 10485760") {
		t.Errorf("Expected limit to be 10485760, output:\n%s", output)
	}
	if !strings.Contains(output, `kvstore_wasm_executions_total{module="fraud_detector",function="process"} 1`) {
		t.Errorf("Expected WASM executions total to be 1, output:\n%s", output)
	}
}
