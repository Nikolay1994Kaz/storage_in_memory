package server

import (
	"net"
	"testing"
	"time"
)

// TestServer_OnDisconnectFires — СТРАЖ C1: колбэк OnDisconnect вызывается при
// закрытии соединения через единую точку Epoll.Remove.
//
// Регрессия к баге, когда hub.RemoveConn был написан, но НЕ подключён: при
// отключении клиента его семантический вектор навсегда оставался в HNSW-индексе,
// а горутина writePump висела. Фикс — хук onRemove в Epoll.Remove; этот тест
// проверяет, что хук реально срабатывает на разрыв (здесь — через idle-реап).
func TestServer_OnDisconnectFires(t *testing.T) {
	fired := make(chan net.Conn, 1)

	srv := NewServer("127.0.0.1:0", func(cs *ConnState, args [][]byte) {
		cs.Buf.WriteSimpleString("OK")
	})
	srv.IdleTimeout = 150 * time.Millisecond // реапнёт молчащее соединение
	srv.OnDisconnect = func(c net.Conn) {
		select {
		case fired <- c:
		default:
		}
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	conn, err := net.Dial("tcp", srv.listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Молчим — idle-реапер закроет соединение через Epoll.Remove, что обязано
	// вызвать OnDisconnect. Ждём сигнал с запасом.
	select {
	case <-fired:
		// OK: хук сработал → подписки Pub/Sub будут вычищены.
	case <-time.After(3 * time.Second):
		t.Fatal("OnDisconnect не вызван при закрытии соединения — hub.RemoveConn " +
			"не подключён к Epoll.Remove → утечка семантического вектора в HNSW")
	}
}
