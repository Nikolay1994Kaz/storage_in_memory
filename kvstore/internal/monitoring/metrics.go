package monitoring

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

var (
	// Active connections (Gauge-like counter)
	ActiveConnections = metrics.NewCounter("kvstore_network_active_connections")

	// Idle connections reaped (Slowloris/брошенные соединения)
	IdleReaped = metrics.NewCounter("kvstore_network_idle_reaped_total")

	// Bytes read and written
	BytesRead    = metrics.NewCounter("kvstore_network_bytes_read_total")
	BytesWritten = metrics.NewCounter("kvstore_network_bytes_written_total")

	// Greedy drain stats
	GreedyReads = metrics.NewCounter("kvstore_network_greedy_reads_total")
	GreedyHits  = metrics.NewCounter("kvstore_network_greedy_hits_total")

	// OOM Events
	OomEvents = metrics.NewCounter("kvstore_oom_events_total")

	// Command metrics
	commandsMu        sync.RWMutex
	commandCounters   = make(map[string]*metrics.Counter)
	commandHistograms = make(map[string]*metrics.Histogram)

	// WASM execution metrics
	wasmMu         sync.RWMutex
	wasmCounters   = make(map[string]*metrics.Counter)
	wasmHistograms = make(map[string]*metrics.Histogram)
	WasmRecycles   = metrics.NewCounter("kvstore_wasm_memory_recycles_total")

	// AI Engine metrics
	AiQueueLen      = metrics.NewCounter("kvstore_ai_worker_queue_len")
	AiEmbedDuration = metrics.NewHistogram("kvstore_ai_embedding_duration_seconds")
	AiChatDuration  = metrics.NewHistogram("kvstore_ai_chat_duration_seconds")

	// WAL metrics
	WalWriteDuration = metrics.NewHistogram("kvstore_wal_write_duration_seconds")

	// Callbacks for memory/TCMalloc metrics
	memUsedFunc   func() float64
	memChunksFunc func() float64
	memSpansFunc  func() float64
	memLimitFunc  func() float64
)

// InitMemoryMetrics registers the TCMalloc memory stat callbacks.
func InitMemoryMetrics(used, chunks, spans, limit func() float64) {
	memUsedFunc = used
	memChunksFunc = chunks
	memSpansFunc = spans
	memLimitFunc = limit

	metrics.NewGauge("kvstore_memory_used_bytes", func() float64 {
		if memUsedFunc != nil {
			return memUsedFunc()
		}
		return 0
	})
	metrics.NewGauge("kvstore_tcmalloc_chunks_total", func() float64 {
		if memChunksFunc != nil {
			return memChunksFunc()
		}
		return 0
	})
	metrics.NewGauge("kvstore_tcmalloc_spans_total", func() float64 {
		if memSpansFunc != nil {
			return memSpansFunc()
		}
		return 0
	})
	metrics.NewGauge("kvstore_memory_limit_bytes", func() float64 {
		if memLimitFunc != nil {
			return memLimitFunc()
		}
		return 0
	})
}

// RecordCommand records executed command metrics with caching to avoid string format overhead.
func RecordCommand(cmd string, duration time.Duration) {
	commandsMu.RLock()
	c, okC := commandCounters[cmd]
	h, okH := commandHistograms[cmd]
	commandsMu.RUnlock()

	if !okC || !okH {
		commandsMu.Lock()
		c, okC = commandCounters[cmd]
		if !okC {
			c = metrics.GetOrCreateCounter(fmt.Sprintf(`kvstore_commands_total{command="%s"}`, cmd))
			commandCounters[cmd] = c
		}
		h, okH = commandHistograms[cmd]
		if !okH {
			h = metrics.GetOrCreateHistogram(fmt.Sprintf(`kvstore_command_duration_seconds{command="%s"}`, cmd))
			commandHistograms[cmd] = h
		}
		commandsMu.Unlock()
	}

	c.Inc()
	h.Update(duration.Seconds())
}

// RecordWasm records WASM execution metrics with caching.
func RecordWasm(module, function string, duration time.Duration) {
	key := module + "." + function
	wasmMu.RLock()
	c, okC := wasmCounters[key]
	h, okH := wasmHistograms[key]
	wasmMu.RUnlock()

	if !okC || !okH {
		wasmMu.Lock()
		c, okC = wasmCounters[key]
		if !okC {
			c = metrics.GetOrCreateCounter(fmt.Sprintf(`kvstore_wasm_executions_total{module="%s",function="%s"}`, module, function))
			wasmCounters[key] = c
		}
		h, okH = wasmHistograms[key]
		if !okH {
			h = metrics.GetOrCreateHistogram(fmt.Sprintf(`kvstore_wasm_execution_duration_seconds{module="%s",function="%s"}`, module, function))
			wasmHistograms[key] = h
		}
		wasmMu.Unlock()
	}

	c.Inc()
	h.Update(duration.Seconds())
}

// StartHttpServer runs the HTTP metrics server at /metrics on the given port.
func StartHttpServer(port int) {
	if port <= 0 {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		metrics.WritePrometheus(w, true)
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		log.Printf("Starting HTTP metrics server on :%d/metrics", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Metrics HTTP server error: %v", err)
		}
	}()
}
