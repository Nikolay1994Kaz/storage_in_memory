package server

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// TestServer_ReapsIdleConnection — репро: молчащее соединение не реапится.
//
// Сервер ждёт в EpollWait(-1), а не в conn.Read. По молчащему соединению epoll
// никогда не сработает → handleConn не вызовется → соединение висит с fd + rbuf
// ВЕЧНО. Это Slowloris: открыть пачку conn, ничего не слать → исчерпание fd/RAM.
//
// Go-дедлайн (SetReadDeadline) тут не спасает: он завязан на рантайм-поллер, а
// EpollWait от него не проснётся. Нужен реапер по неактивности.
//
// Корректное поведение: сервер сам закрывает соединение через IdleTimeout.
func TestServer_ReapsIdleConnection(t *testing.T) {
	srv := NewServer("127.0.0.1:0", func(cs *ConnState, args [][]byte) {
		cs.Buf.WriteSimpleString("OK")
	})
	srv.IdleTimeout = 200 * time.Millisecond // маленький для теста

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	conn, err := net.Dial("tcp", srv.listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Ничего не шлём — имитируем idle/Slowloris. Ждём, что СЕРВЕР закроет conn.
	// Наш дедлайн (3с) — лишь страховка, чтобы тест не висел, если реапера нет.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)

	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("соединение всё ещё открыто через 3с (IdleTimeout=200мс) — " +
			"idle-реапер не сработал → Slowloris: молчащие conn висят вечно")
	}
	if err == nil {
		t.Fatal("ожидали закрытие соединения сервером, Read вернул данные без ошибки")
	}
	// err == io.EOF (сервер закрыл) — корректно.
}

// TestServer_KeepsActiveConnection — страж: активное соединение НЕ реапится.
//
// Регрессия к TestServer_ReapsIdleConnection: реапер не должен убивать клиента,
// который шлёт команды чаще IdleTimeout. Каждый успешный read сбрасывает счётчик.
func TestServer_KeepsActiveConnection(t *testing.T) {
	srv := NewServer("127.0.0.1:0", func(cs *ConnState, args [][]byte) {
		cs.Buf.WriteSimpleString("OK")
	})
	srv.IdleTimeout = 300 * time.Millisecond

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	conn, err := net.Dial("tcp", srv.listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	resp := make([]byte, 64)
	// Шлём команду каждые 100мс в течение ~600мс (2× IdleTimeout) — соединение
	// должно жить, отвечая на каждый запрос.
	for i := 0; i < 6; i++ {
		if _, err := conn.Write([]byte("PING\r\n")); err != nil {
			t.Fatalf("итерация %d: Write: %v — активное соединение было закрыто", i, err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, err := conn.Read(resp)
		if err != nil {
			t.Fatalf("итерация %d: Read: %v — активное соединение реапнули по ошибке", i, err)
		}
		if string(resp[:n]) != "+OK\r\n" {
			t.Fatalf("итерация %d: ответ %q, ожидали +OK", i, resp[:n])
		}
		time.Sleep(100 * time.Millisecond)
	}
}
