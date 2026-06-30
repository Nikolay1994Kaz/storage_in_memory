package server

import (
	"net"
	"testing"
	"time"
)

// floodConn — fake net.Conn, который на каждый Read отдаёт байты БЕЗ \r\n.
// Имитирует атакующего, который льёт данные, не завершая кадр (inline без
// перевода строки), чтобы парсер никогда не делал прогресс.
type floodConn struct{ chunk int }

func (c *floodConn) Read(p []byte) (int, error) {
	n := len(p)
	if n > c.chunk {
		n = c.chunk
	}
	for i := 0; i < n; i++ {
		p[i] = 'x' // не \r и не \n — peekLine никогда не найдёт конец строки
	}
	return n, nil
}
func (c *floodConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *floodConn) Close() error                     { return nil }
func (c *floodConn) LocalAddr() net.Addr              { return nil }
func (c *floodConn) RemoteAddr() net.Addr             { return nil }
func (c *floodConn) SetDeadline(time.Time) error      { return nil }
func (c *floodConn) SetReadDeadline(time.Time) error  { return nil }
func (c *floodConn) SetWriteDeadline(time.Time) error { return nil }

// TestRead_RbufCeiling_ProtoErr — репро mem-DoS через рост rbuf (inline без \r\n).
//
// Кэп maxBulkSize закрывает амплификацию через объявленную bulk-длину, но не этот
// вектор: клиент шлёт поток байтов без \r\n. peekLine всегда возвращает false →
// ParseCommand не съедает ничего → ReadFromConn/TryRead при заполнении буфера
// УДВАИВАЮТ rbuf (connbuf.go) без верхней границы → память растёт пока клиент
// льёт → remote OOM.
//
// Корректное поведение: по достижении потолка maxRbufSize без полного кадра
// выставить protoErr, чтобы handleConn закрыл соединение.
func TestRead_RbufCeiling_ProtoErr(t *testing.T) {
	orig := maxRbufSize
	maxRbufSize = 4096 // маленький потолок, чтобы не аллоцировать 512 МБ
	defer func() { maxRbufSize = orig }()

	cb := &ConnBuf{
		conn: &floodConn{chunk: 1024},
		rbuf: make([]byte, 256), // маленький старт → быстро упрёмся в потолок
		wbuf: make([]byte, 0, 64),
	}

	// Имитируем handleConn: читаем, пытаемся парсить, повторяем.
	for i := 0; i < 2000; i++ {
		n, err := cb.ReadFromConn()
		for cb.ParseCommand() != nil { // съедаем всё, что распарсилось (ничего)
		}
		if cb.ProtoErr() {
			break
		}
		if n == 0 || err != nil {
			break
		}
	}

	if !cb.ProtoErr() {
		t.Fatalf("ProtoErr не выставлен: rbuf вырос до %d при потоке без \\r\\n — "+
			"потолок rbuf не сработал → remote OOM (mem-DoS)", len(cb.rbuf))
	}
	if len(cb.rbuf) > maxRbufSize {
		t.Fatalf("rbuf=%d превысил потолок maxRbufSize=%d", len(cb.rbuf), maxRbufSize)
	}
}
