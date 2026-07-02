package ai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOllama поднимает HTTP-заглушку Ollama /api/embed. Возвращает фиксированный
// вектор. delay>0 имитирует «залипшую» Ollama: держит ответ, пока не истечёт
// задержка ИЛИ пока клиент не отменит запрос (context) — как при bounded shutdown.
//
// release закрывается в t.Cleanup ДО srv.Close(): иначе Close() блокируется, пока
// зависший хендлер не отспит delay (серверный r.Context() при клиентской отмене
// освобождается не сразу) — из-за этого «стук»-тест тянул бы 60с.
func fakeOllama(t *testing.T, embedding []float32, delay time.Duration) *httptest.Server {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return // клиент отменил (ctx воркера) — отвечать нечем
			case <-release:
				return // тест завершается — не держим Close()
			}
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{embedding}})
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv
}

// TestWorker_RecoverFromPanic — СТРАЖ Critical: паника в обработке задачи не
// убивает горутину воркера (и процесс). После паникующей задачи воркер обязан
// продолжить и обработать следующую.
//
// Регрессия к баге: loop не имел recover, паника в VecStoreAdd/Embed валила
// процесс. С 1 горутиной: если её убьёт — задача "ok" не обработается никогда.
func TestWorker_RecoverFromPanic(t *testing.T) {
	srv := fakeOllama(t, []float32{0.1, 0.2, 0.3}, 0)

	w := NewWorker(NewClient(srv.URL, "m", "m"), 16)
	indexed := make(chan string, 8)
	w.VecStoreAdd = func(key string, vec []float32) error {
		if key == "boom" {
			panic("simulated panic in VecStoreAdd")
		}
		return nil
	}
	w.KVStoreSet = func(key string, value []byte) {}
	w.Publish = func(ch, msg string) {
		if ch == "ai:indexed" {
			indexed <- msg
		}
	}

	w.Start(1) // ровно 1 горутина: паника без recover убьёт единственного воркера
	defer w.Stop()

	if err := w.Submit("boom", "text"); err != nil {
		t.Fatalf("Submit boom: %v", err)
	}
	if err := w.Submit("ok", "text"); err != nil {
		t.Fatalf("Submit ok: %v", err)
	}

	select {
	case key := <-indexed:
		if key != "ok" {
			t.Fatalf("ожидали обработку 'ok', получили %q", key)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("после паникующей задачи воркер не обработал следующую — " +
			"горутина умерла (нет recover в обработке задачи)")
	}
}

// TestWorker_DrainOnStop — СТРАЖ High: Stop дообрабатывает очередь, а не бросает.
//
// AI.INGEST возвращает "QUEUED" без WAL. Если Stop бросит буфер — эти документы
// исчезнут молча. Требуем: после Stop все поставленные задачи обработаны.
func TestWorker_DrainOnStop(t *testing.T) {
	// Небольшая задержка на embed — чтобы задачи реально скопились в очереди
	// к моменту Stop, а не были все обработаны на лету.
	srv := fakeOllama(t, []float32{0.1, 0.2, 0.3}, 15*time.Millisecond)

	w := NewWorker(NewClient(srv.URL, "m", "m"), 256)
	w.DrainTimeout = 5 * time.Second

	var processed atomic.Int32
	w.VecStoreAdd = func(key string, vec []float32) error { processed.Add(1); return nil }
	w.KVStoreSet = func(key string, value []byte) {}

	w.Start(1)

	const n = 20
	for i := 0; i < n; i++ {
		if err := w.Submit("k", "text"); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	// Останавливаем почти сразу — большая часть из n ещё в очереди.
	w.Stop()

	if got := processed.Load(); got != n {
		t.Fatalf("после Stop обработано %d из %d — очередь потеряна (drain не работает)", got, n)
	}
}

// TestWorker_BoundedShutdown — СТРАЖ High: Stop не залипает на застрявшей Ollama.
//
// Регрессия к баге: Embed на context.Background() → Stop ждал до ~30с (15с×2)
// на одной in-flight задаче. С привязкой к embedCtx и бюджетом DrainTimeout
// залипший запрос отменяется, Stop укладывается в бюджет.
func TestWorker_BoundedShutdown(t *testing.T) {
	srv := fakeOllama(t, []float32{0.1}, 60*time.Second) // «вечно» висящий embed

	w := NewWorker(NewClient(srv.URL, "m", "m"), 16)
	w.DrainTimeout = 300 * time.Millisecond
	w.VecStoreAdd = func(key string, vec []float32) error { return nil }
	w.KVStoreSet = func(key string, value []byte) {}

	w.Start(1)
	if err := w.Submit("stuck", "text"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // дать воркеру войти в Embed

	start := time.Now()
	w.Stop()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("Stop занял %v при DrainTimeout=300мс — embedCtx не прерывает "+
			"застрявший Embed, shutdown залипает", elapsed)
	}
}
