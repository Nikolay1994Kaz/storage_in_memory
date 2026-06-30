package monitoring

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealth_Liveness — /health и алиасы всегда 200 (процесс жив).
func TestHealth_Liveness(t *testing.T) {
	h := httpHandler()
	for _, path := range []string{"/health", "/healthz", "/livez"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: код %d, ожидали 200", path, rec.Code)
		}
	}
}

// TestHealth_Readiness — /ready отражает флаг готовности: 503 до SetReady(true),
// 200 после. Это позволяет оркестратору не слать трафик в непрогретый процесс.
func TestHealth_Readiness(t *testing.T) {
	h := httpHandler()

	SetReady(false)
	defer SetReady(false) // не протекаем состоянием в другие тесты
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready до готовности: код %d, ожидали 503", rec.Code)
	}

	SetReady(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/ready после SetReady(true): код %d, ожидали 200", rec.Code)
	}
}
