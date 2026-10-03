package server

import (
	"net"
	"testing"
	"time"
)

// TestServer_CloseConn_CleansAccounting — страж H3 (серверная половина).
//
// disconnectSlow теперь закрывает медленного подписчика через Server.CloseConn
// (единая точка). Проверяем, что CloseConn по net.Conn декрементит accounting
// (activeConns) и убирает соединение из учёта сервера. До фикса disconnectSlow звал
// conn.Close() напрямую → счётчики не чистились → каждый slow-subscriber
// навсегда съедал слот → перманентный DoS при MaxConnections.
func TestServer_CloseConn_CleansAccounting(t *testing.T) {
	srv := NewServer("127.0.0.1:0", func(cs *ConnState, args [][]byte) {})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	client, err := net.Dial("tcp", srv.listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	serverConn := waitServerConn(t, srv)
	if got := srv.activeConns.Load(); got != 1 {
		t.Fatalf("до CloseConn activeConns=%d, want 1", got)
	}

	srv.CloseConn(serverConn)

	// CloseConn синхронен и декрементит до возврата; поллим на случай гонки с
	// выходом горутины соединения (её closeConn — идемпотентный no-op).
	if !waitFor(func() bool { return srv.activeConns.Load() == 0 }, time.Second) {
		t.Fatalf("H3: activeConns=%d после CloseConn, want 0 — слот утёк (DoS при MaxConnections)", srv.activeConns.Load())
	}
	srv.mu.Lock()
	total := len(srv.conns)
	srv.mu.Unlock()
	if total != 0 {
		t.Fatalf("H3: сервер держит %d соединений после CloseConn, want 0", total)
	}
}

func waitServerConn(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var found net.Conn
		srv.mu.Lock()
		for c := range srv.conns {
			found = c
			break
		}
		srv.mu.Unlock()
		if found != nil {
			return found
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("сервер не зарегистрировал соединение за 2с")
	return nil
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
