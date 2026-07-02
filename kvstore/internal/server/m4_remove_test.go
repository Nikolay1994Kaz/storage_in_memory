package server

import (
	"net"
	"sync/atomic"
	"testing"
)

// tcpPair поднимает реальную TCP-пару (client *net.TCPConn, accepted server conn).
// Нужны живые fd — socketFD работает только с *net.TCPConn.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	type res struct {
		c net.Conn
		e error
	}
	ch := make(chan res, 1)
	go func() {
		c, e := ln.Accept()
		ch <- res{c, e}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	r := <-ch
	if r.e != nil {
		t.Fatalf("Accept: %v", r.e)
	}
	return client, r.c
}

// TestM4_RemoveCleansAccountingOnEpollCtlError — главный страж M4.
//
// Раньше Remove при ошибке EpollCtl DEL делал ранний return БЕЗ очистки:
// connCount/ActiveConnections не декрементились, onRemove (очистка подписок
// Pub/Sub) не вызывался, conn не закрывался. Ошибка EpollCtl DEL реальна
// (EBADF — peer закрыл fd; ENOENT — fd уже снят), и тогда каждое такое
// соединение навсегда съедало слот → ложный отказ по MaxConnections + утечка
// вектора в HNSW.
//
// Тут соединение зарегистрировано в КАРТЕ и учёте, но НЕ в epoll (EpollCtl ADD
// не звали) → EpollCtl DEL вернёт ENOENT. Очистка обязана произойти всё равно.
// Тест падает на старом поведении (connCount остаётся 1, onRemove не зван).
func TestM4_RemoveCleansAccountingOnEpollCtlError(t *testing.T) {
	e, err := NewEpoll()
	if err != nil {
		t.Fatalf("NewEpoll: %v", err)
	}
	defer e.Close()

	var cnt atomic.Int64
	e.connCount = &cnt
	removed := 0
	e.onRemove = func(net.Conn) { removed++ }

	c1, c2 := tcpPair(t)
	defer c2.Close()
	cs := &ConnState{Conn: c1}

	fd := socketFD(c1)
	e.mu.Lock()
	e.connections[fd] = cs // в карте
	e.mu.Unlock()
	cnt.Add(1) // в учёте
	// НЕ вызываем EpollCtl ADD → EpollCtl DEL в Remove вернёт ENOENT.

	_ = e.Remove(cs)

	if got := cnt.Load(); got != 0 {
		t.Fatalf("M4: connCount=%d после Remove при ошибке EpollCtl DEL, want 0 "+
			"(учёт потерян → ложный отказ по MaxConnections)", got)
	}
	if removed != 1 {
		t.Fatalf("M4: onRemove вызван %d раз, want 1 (подписка Pub/Sub утекла)", removed)
	}
	e.mu.RLock()
	_, still := e.connections[fd]
	e.mu.RUnlock()
	if still {
		t.Fatal("M4: запись осталась в карте epoll после Remove")
	}
}

// TestM4_RemoveIdempotent — регресс-гард: повторный Remove того же conn не
// задваивает декремент (recover-путь handleConn + реапер/CloseConn могут звать
// Remove повторно). Гейт identity в карте делает второй вызов no-op'ом.
func TestM4_RemoveIdempotent(t *testing.T) {
	e, err := NewEpoll()
	if err != nil {
		t.Fatalf("NewEpoll: %v", err)
	}
	defer e.Close()

	var cnt atomic.Int64
	e.connCount = &cnt
	removed := 0
	e.onRemove = func(net.Conn) { removed++ }

	c1, c2 := tcpPair(t)
	defer c2.Close()
	cs := &ConnState{Conn: c1}

	if err := e.Add(cs); err != nil { // регистрирует в epoll + карте + учёте (cnt=1)
		t.Fatalf("Add: %v", err)
	}

	_ = e.Remove(cs) // первый: чистит, cnt→0, removed=1
	_ = e.Remove(cs) // второй: идемпотентный no-op

	if got := cnt.Load(); got != 0 {
		t.Fatalf("M4: connCount=%d после двойного Remove, want 0 (двойной декремент)", got)
	}
	if removed != 1 {
		t.Fatalf("M4: onRemove вызван %d раз при двойном Remove, want 1", removed)
	}
}
