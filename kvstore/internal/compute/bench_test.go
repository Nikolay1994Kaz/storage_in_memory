package compute

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// ─── Загрузка реального WASM-модуля ──────────────────────

func loadFraudScorer(b *testing.B) (*Engine, []byte) {
	b.Helper()

	// Находим fraud_scorer.wasm относительно корня проекта
	_, thisFile, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	wasmPath := filepath.Join(projectRoot, "fraud_scorer.wasm")

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		b.Skipf("fraud_scorer.wasm not found: %v (skip real benchmark)", err)
	}

	e := NewEngine()

	// Подключаем mock-колбэки для host-функций
	store := map[string][]byte{
		"tx:1001": []byte(`{"amount":15000,"country":"NK"}`),
		"tx:1002": []byte(`{"amount":500,"country":"US"}`),
		"tx:1003": []byte(`{"amount":75000,"country":"IR"}`),
	}

	e.StoreGet = func(key string) ([]byte, bool) {
		v, ok := store[key]
		return v, ok
	}
	e.StoreSet = func(key string, value []byte) {}
	e.StoreDel = func(key string) {}
	e.StoreSetWithWAL = func(key string, value []byte) error { return nil }
	e.Publish = func(channel, message string) {}

	if err := e.LoadModule("fraud_scorer", wasmBytes); err != nil {
		b.Fatalf("LoadModule: %v", err)
	}

	return e, wasmBytes
}

// ─── Benchmark: реальный WASM fraud scorer ───────────────

func BenchmarkRealWasm_FraudScorer(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e, _ := loadFraudScorer(b)
	defer e.Close()

	b.ReportAllocs()
	b.SetBytes(33)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.ExecFunctionWithKey("fraud_scorer", "process", "tx:1001"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRealWasm_FraudScorer_SmallTx(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e, _ := loadFraudScorer(b)
	defer e.Close()

	b.ReportAllocs()
	b.SetBytes(31)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.ExecFunctionWithKey("fraud_scorer", "process", "tx:1002"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRealWasm_FraudScorer_BlockedTx(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e, _ := loadFraudScorer(b)
	defer e.Close()

	b.ReportAllocs()
	b.SetBytes(33)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.ExecFunctionWithKey("fraud_scorer", "process", "tx:1003"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRealWasm_FraudScorer_Parallel(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e, _ := loadFraudScorer(b)
	defer e.Close()

	b.ReportAllocs()
	b.SetBytes(33)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := e.ExecFunctionWithKey("fraud_scorer", "process", "tx:1001"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ─── Benchmark: компиляция реального WASM ────────────────

func BenchmarkRealWasm_LoadModule(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e, wasmBytes := loadFraudScorer(b)
	defer e.Close()
	e.DropModule("fraud_scorer")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.LoadModule("fraud_scorer", wasmBytes)
		e.DropModule("fraud_scorer")
	}
}

// ─── Для сравнения: минимальный addWasm (baseline) ───────

func BenchmarkBaseline_ExecFunction_Add(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e := NewEngine()
	defer e.Close()
	e.LoadModule("calc", addWasm)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.ExecFunction("calc", "add", 2, 3); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBaseline_LoadModule_Add(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e := NewEngine()
	defer e.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.LoadModule("calc", addWasm)
		e.DropModule("calc")
	}
}

// ─── Benchmark: ExecTimeout overhead ─────────────────────

func BenchmarkExecTimeout_1ms(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e := NewEngine()
	defer e.Close()
	e.ExecTimeout = 1 * time.Millisecond
	e.LoadModule("calc", addWasm)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.ExecFunction("calc", "add", 1, 1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExecTimeout_100ms(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e := NewEngine()
	defer e.Close()
	e.ExecTimeout = 100 * time.Millisecond
	e.LoadModule("calc", addWasm)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.ExecFunction("calc", "add", 1, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// ─── Benchmark: TriggerManager glob matching ─────────────

func BenchmarkTriggerFire_1Trigger(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e := NewEngine()
	defer e.Close()
	tm := NewTriggerManager(e)
	tm.AddTrigger(OnSet, "tx:*", "mod", "fn")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tm.Fire(OnSet, "user:123", 0)
	}
}

func BenchmarkTriggerFire_10Triggers(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e := NewEngine()
	defer e.Close()
	tm := NewTriggerManager(e)
	for i := 0; i < 10; i++ {
		tm.AddTrigger(OnSet, "prefix"+string(rune('a'+i))+":*", "mod", "fn")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tm.Fire(OnSet, "nomatch:key", 0)
	}
}

// ─── Benchmark: Worker-Local Reactor (Tier 0/1) ──────────

/*
Ключевые уроки архитектуры Worker-Local (WASM Reactor):
1. -buildmode=c-shared — критически важный флаг TinyGo. Только он убирает _start и делает настоящий Reactor.
2. Offset ≠ 0 — адрес 0 в WASM linear memory = nil pointer в TinyGo.
3. Отдельный runtime для worker-local — WithCloseOnContextDone в основном runtime убивает persistent инстансы.
4. ExportedFunctions() map keys ≠ def.Name() — ключи = export names, Name() = internal wasm names.
*/

func loadReactorFraudScorer(b *testing.B) (*Engine, []byte) {
	b.Helper()

	_, thisFile, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	wasmPath := filepath.Join(projectRoot, "fraud_scorer_reactor.wasm")

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		b.Skipf("fraud_scorer_reactor.wasm not found: %v (skip reactor benchmark)", err)
	}

	e := NewEngine()

	// Mock-колбэки для host-функций
	store := map[string][]byte{
		"tx:1001": []byte(`{"amount":15000,"country":"NK"}`),
		"tx:1002": []byte(`{"amount":500,"country":"US"}`),
		"tx:1003": []byte(`{"amount":75000,"country":"IR"}`),
	}
	e.StoreGet = func(key string) ([]byte, bool) {
		v, ok := store[key]
		return v, ok
	}
	e.StoreSet = func(key string, value []byte) {}
	e.StoreDel = func(key string) {}
	e.StoreSetWithWAL = func(key string, value []byte) error { return nil }
	e.Publish = func(channel, message string) {}

	if err := e.LoadModule("fraud_scorer", wasmBytes); err != nil {
		b.Fatalf("LoadModule: %v", err)
	}

	return e, wasmBytes
}

// BenchmarkWorkerLocal_FraudScorer — главный бенчмарк.
// Измеряет горячий путь: worker-local persistent instance.
func BenchmarkWorkerLocal_FraudScorer(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e, _ := loadReactorFraudScorer(b)
	defer e.Close()

	key := []byte("tx:1001")
	b.ReportAllocs()
	b.SetBytes(int64(len(key)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.WorkerLocal.Exec(0, "fraud_scorer", "process", key); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWorkerLocal_FraudScorer_SmallTx — маленькая транзакция.
func BenchmarkWorkerLocal_FraudScorer_SmallTx(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e, _ := loadReactorFraudScorer(b)
	defer e.Close()

	key := []byte("tx:1002")
	b.ReportAllocs()
	b.SetBytes(int64(len(key)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.WorkerLocal.Exec(0, "fraud_scorer", "process", key); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWorkerLocal_FraudScorer_Blocked — транзакция auto-block (score=100).
func BenchmarkWorkerLocal_FraudScorer_Blocked(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e, _ := loadReactorFraudScorer(b)
	defer e.Close()

	key := []byte("tx:1003")
	b.ReportAllocs()
	b.SetBytes(int64(len(key)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.WorkerLocal.Exec(0, "fraud_scorer", "process", key); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWorkerLocal_FraudScorer_Parallel — тест, чтобы задействовать все воркеры (напр. 12) параллельно.
func BenchmarkWorkerLocal_FraudScorer_Parallel(b *testing.B) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	e, _ := loadReactorFraudScorer(b)
	defer e.Close()

	key := []byte("tx:1001")
	b.ReportAllocs()
	b.SetBytes(int64(len(key)))
	b.ResetTimer()

	var counter uint32
	numWorkers := uint32(e.WorkerLocal.numWorkers)
	if numWorkers == 0 {
		numWorkers = 1
	}

	b.RunParallel(func(pb *testing.PB) {
		// Каждая горутина получает свой уникальный workerID (round-robin)
		workerID := int(atomic.AddUint32(&counter, 1) % numWorkers)
		for pb.Next() {
			if _, err := e.WorkerLocal.Exec(workerID, "fraud_scorer", "process", key); err != nil {
				b.Fatal(err)
			}
		}
	})
}
