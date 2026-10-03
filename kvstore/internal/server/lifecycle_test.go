package server

import (
	"bufio"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestCloseConn_ExactlyOnce — страж единой точки закрытия (наследник M4).
//
// Соединение закрывают три пути сразу: клиент рвёт сокет (горутина соединения
// выходит), эвикция медленного подписчика (CloseConn) и Stop. Учёт и
// OnDisconnect обязаны отработать ровно один раз: задвоенный декремент увёл бы
// activeConns в минус (лимит соединений перестаёт работать), пропущенный —
// навсегда съел бы слот, а неотработавший OnDisconnect оставил бы подписку.
func TestCloseConn_ExactlyOnce(t *testing.T) {
	var disconnects atomic.Int64
	srv := NewServer("127.0.0.1:0", func(cs *ConnState, args [][]byte) {
		cs.Buf.WriteSimpleString("OK")
	})
	srv.OnDisconnect = func(net.Conn) { disconnects.Add(1) }
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const n = 20
	clients := make([]net.Conn, n)
	for i := range clients {
		c, err := net.Dial("tcp", srv.listener.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		clients[i] = c
	}
	if !waitFor(func() bool { return srv.activeConns.Load() == n }, 2*time.Second) {
		t.Fatalf("зарегистрировано %d из %d", srv.activeConns.Load(), n)
	}
	srv.mu.Lock()
	serverSide := make([]net.Conn, 0, n)
	for c := range srv.conns {
		serverSide = append(serverSide, c)
	}
	srv.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(c net.Conn) { defer wg.Done(); c.Close() }(clients[i])
		go func(c net.Conn) { defer wg.Done(); srv.CloseConn(c) }(serverSide[i])
	}
	wg.Wait()
	srv.Stop()

	if got := srv.activeConns.Load(); got != 0 {
		t.Fatalf("activeConns=%d после закрытия всех, ожидали 0", got)
	}
	if got := disconnects.Load(); got != n {
		t.Fatalf("OnDisconnect вызван %d раз на %d соединений", got, n)
	}
}

// flakyListener отдаёт временную ошибку Accept первые fails раз — как при
// исчерпании дескрипторов (EMFILE), — затем работает как обычный листенер.
type flakyListener struct {
	net.Listener
	fails atomic.Int64
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.fails.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

// TestAcceptLoop_SurvivesTemporaryError — исчерпание дескрипторов не убивает
// приём. Прежний acceptLoop выходил на ЛЮБОЙ ошибке Accept: один всплеск
// соединений до EMFILE — и процесс жив, но новых клиентов не принимает никогда.
func TestAcceptLoop_SurvivesTemporaryError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	fl := &flakyListener{Listener: ln}
	fl.fails.Store(3)

	srv := NewServer("", func(cs *ConnState, args [][]byte) {
		cs.Buf.WriteSimpleString("PONG")
	})
	srv.startOn(fl)
	defer srv.Stop()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("PING\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "+PONG\r\n" {
		t.Fatalf("после временных ошибок Accept сервер не обслуживает: %q, %v", line, err)
	}
}

// TestStop_WakesIdleReadersPromptly — Stop не ждёт drainTimeout, если
// соединения просто молчат: дедлайн чтения «сейчас» будит их горутины сразу.
// Без этого каждая остановка процесса с подключёнными клиентами стоила бы
// drainTimeout (5 с), а при IdleTimeout=0 — ровно его.
func TestStop_WakesIdleReadersPromptly(t *testing.T) {
	srv := NewServer("127.0.0.1:0", func(cs *ConnState, args [][]byte) {})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c, err := net.Dial("tcp", srv.listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if !waitFor(func() bool { return srv.activeConns.Load() == 1 }, 2*time.Second) {
		t.Fatal("соединение не зарегистрировано")
	}

	start := time.Now()
	srv.Stop()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Stop занял %v — молчащее соединение не разбужено", d)
	}
	// Дедлайн на клиентской стороне значит, что сервер соединение НЕ закрыл:
	// иначе Read вернул бы EOF/reset раньше.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("после Stop соединение не закрыто сервером: %v", err)
	}
}
