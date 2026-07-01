package server

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestServer_StopDrainsInFlight — страж graceful drain.
//
// Репро дефекта: Stop() возвращался сразу после закрытия epoll, НЕ дожидаясь
// завершения обработчиков «в полёте». В main это приводило к тому, что bw.Close()
// (закрытие WAL-канала) мог выполниться, пока воркер ещё в handleConn и вот-вот
// вызовет bw.Write → паника send-на-закрытый-канал + потеря последней команды.
//
// Корректное поведение: Stop() блокируется, пока in-flight обработчик не завершился.
func TestServer_StopDrainsInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool

	srv := NewServer("127.0.0.1:0", func(cs *ConnState, args [][]byte) {
		select {
		case <-started:
		default:
			close(started) // сигналим один раз: обработчик стартовал
		}
		<-release // держим обработчик «в полёте»
		finished.Store(true)
		cs.Buf.WriteSimpleString("OK")
	})
	// Малый idle-timeout → простаивающие воркеры выходят по тику, не завися от
	// того, будит ли закрытие epoll-fd заблокированный epoll_wait. Тест быстрый.
	srv.IdleTimeout = 50 * time.Millisecond

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := srv.listener.Addr().String()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("PING\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Ждём, пока обработчик стартует и повиснет на release.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("обработчик не стартовал")
	}

	// Освобождаем обработчик с задержкой; Stop() обязан его дождаться.
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()

	srv.Stop()

	if !finished.Load() {
		t.Fatal("Stop() вернулся до завершения in-flight обработчика — graceful drain не работает")
	}
}
