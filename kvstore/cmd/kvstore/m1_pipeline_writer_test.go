package main

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"

	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
)

// recordingConn — net.Conn, фиксирующий все Write в порядке поступления.
// Нужен, чтобы наблюдать межкадровый порядок двух независимых писателей одного
// соединения (конечный Flush воркера через cs.Buf vs writePump подписчика).
// net.Pipe для этого не годится: он синхронный и блокирует запись до чтения.
type recordingConn struct {
	mu  sync.Mutex
	buf []byte
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.buf = append(c.buf, p...)
	c.mu.Unlock()
	return len(p), nil
}

func (c *recordingConn) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...)
}

func (c *recordingConn) Read([]byte) (int, error)         { return 0, nil }
func (c *recordingConn) Close() error                     { return nil }
func (c *recordingConn) LocalAddr() net.Addr              { return testAddr{} }
func (c *recordingConn) RemoteAddr() net.Addr             { return testAddr{} }
func (c *recordingConn) SetDeadline(time.Time) error      { return nil }
func (c *recordingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordingConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr struct{}

func (testAddr) Network() string { return "tcp" }
func (testAddr) String() string  { return "test" }

// TestM1_SubscribeFlushesPipelinedOutputFirst — страж M1.
//
// Сценарий: клиент пайплайнит PING + SUBSCRIBE в одном батче. Ответ на PING
// (+PONG) уже лежит в cs.Buf, когда обрабатывается SUBSCRIBE. SUBSCRIBE стартует
// writePump — ВТОРОЙ независимый писатель того же conn. Без флаша cs.Buf перед
// стартом writePump два писателя гонятся за сокет: subscribe-подтверждение
// (writePump) может уйти клиенту РАНЬШЕ +PONG (конечный Flush воркера) →
// клиент, ждущий +PONG следующим ответом, десинхронизирует RESP-поток навсегда.
//
// subscribeClassic обязана синхронно флашить cs.Buf ДО hub.Subscribe. Тогда
// +PONG гарантированно на проводе прежде первого кадра writePump.
//
// На СТАРОМ поведении (без флаша в переходе) +PONG остаётся в cs.Buf — тест не
// гоняет серверный батч-цикл, который флашил бы его в конце, — и на провод
// уходит только подтверждение. Проверка «PONG присутствует и раньше subscribe»
// падает: PONG отсутствует.
func TestM1_SubscribeFlushesPipelinedOutputFirst(t *testing.T) {
	rc := &recordingConn{}
	cs := &server.ConnState{Conn: rc, Buf: server.NewConnBuf(rc)}

	// Имитируем ответ на пайплайненный перед SUBSCRIBE PING: он буферизован в
	// cs.Buf и ещё НЕ на проводе (как в реальном батче до конечного Flush).
	cs.Buf.WriteSimpleString("PONG")
	if len(rc.snapshot()) != 0 {
		t.Fatal("предусловие: +PONG должен быть только в буфере, не на проводе")
	}

	hub := pubsub.NewHub(nil)
	subscribeClassic(cs, hub, []string{"news"})

	// writePump доставляет подтверждение асинхронно — ждём его появления.
	var wire []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		wire = rc.snapshot()
		if bytes.Contains(wire, []byte("subscribe")) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	subIdx := bytes.Index(wire, []byte("subscribe"))
	if subIdx < 0 {
		t.Fatal("subscribe-подтверждение не дошло на провод за 2с")
	}
	pongIdx := bytes.Index(wire, []byte("PONG"))
	if pongIdx < 0 {
		t.Fatal("M1: +PONG не ушёл на провод перед стартом writePump — при пайплайне " +
			"PING+SUBSCRIBE клиент получил бы subscribe-подтверждение вместо PONG (RESP-десинк)")
	}
	if pongIdx > subIdx {
		t.Fatalf("M1: +PONG (offset %d) ушёл ПОСЛЕ subscribe-подтверждения (offset %d) — "+
			"межкадровый порядок нарушен, два писателя гонятся за сокет", pongIdx, subIdx)
	}
}
