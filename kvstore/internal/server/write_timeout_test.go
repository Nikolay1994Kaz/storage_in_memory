package server

import (
	"net"
	"testing"
	"time"
)

// TestFlush_WriteTimeout — репро: Flush блокируется на застрявшем reader.
//
// Flush → conn.Write(wbuf). Если клиент не читает (полное TCP-окно), Write
// паркует горутину воркера. Воркер обслуживает МНОГО соединений на одном epoll —
// один залипший reader стопорит их все (DoS). Нужен SetWriteDeadline.
//
// net.Pipe синхронен: Write блокируется, пока другой конец не вычитает. Мы НЕ
// читаем c2 → Flush(c1) обязан вернуть ошибку по таймауту, а не висеть вечно.
func TestFlush_WriteTimeout(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	cb := NewConnBuf(c1)
	cb.WriteTimeout = 200 * time.Millisecond
	cb.wbuf = append(cb.wbuf, []byte("+OK\r\n")...) // есть что писать

	done := make(chan error, 1)
	go func() { done <- cb.Flush() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ожидали ошибку write-таймаута, Flush вернул nil")
		}
		// timeout error — корректно: воркер не залип.
	case <-time.After(2 * time.Second):
		t.Fatal("Flush заблокировался дольше 2с при WriteTimeout=200мс — " +
			"write-дедлайн не выставлен → застрявший reader стопорит воркер")
	}
}
