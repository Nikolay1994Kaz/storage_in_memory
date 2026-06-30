package server

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// TestServer_MaxConnections — репро: нет потолка одновременных соединений.
//
// acceptLoop принимает соединения безгранично. Атакующий открывает их пачкой →
// исчерпание файловых дескрипторов (легитимные клиенты больше не подключатся) и
// RAM (ConnState + 64КБ rbuf на каждое). Реапер чистит idle, но не ограничивает
// ПИК одновременных.
//
// Корректное поведение: при достижении MaxConnections сервер сразу закрывает
// новое соединение.
func TestServer_MaxConnections(t *testing.T) {
	const max = 4

	srv := NewServer("127.0.0.1:0", func(cs *ConnState, args [][]byte) {
		cs.Buf.WriteSimpleString("OK")
	})
	srv.MaxConnections = max
	// IdleTimeout не задаём (0) — держим соединения, чтобы их не реапнуло.

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()
	addr := srv.listener.Addr().String()

	// Открываем ровно max соединений и держим.
	held := make([]net.Conn, 0, max)
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < max; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("Dial %d: %v", i, err)
		}
		held = append(held, c)
	}

	// Ждём, пока сервер зарегистрирует все max.
	deadline := time.Now().Add(2 * time.Second)
	for srv.activeConns.Load() != int64(max) {
		if time.Now().After(deadline) {
			t.Fatalf("сервер зарегистрировал %d соединений, ожидали %d", srv.activeConns.Load(), max)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// (max+1)-е соединение сервер должен сразу закрыть.
	extra, err := net.Dial("tcp", addr)
	if err != nil {
		// Отказ уже на уровне Dial — тоже корректно.
		return
	}
	defer extra.Close()

	_ = extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = extra.Read(buf)

	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("(max+1)-е соединение всё ещё открыто — лимит соединений не работает")
	}
	if err == nil {
		t.Fatal("ожидали закрытие лишнего соединения, Read вернул данные без ошибки")
	}
	// err == io.EOF — сервер закрыл лишнее соединение, корректно.

	// Освобождаем слот: закрываем одно held-соединение → счётчик должен упасть,
	// и новое соединение снова приниматься (лимит не «залипает»).
	held[0].Close()
	held = held[1:]

	deadline = time.Now().Add(2 * time.Second)
	for srv.activeConns.Load() >= int64(max) {
		if time.Now().After(deadline) {
			t.Fatalf("после закрытия соединения счётчик не упал ниже %d (=%d) — dec-путь сломан",
				max, srv.activeConns.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	fresh, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("новое соединение после освобождения слота отклонено: %v", err)
	}
	defer fresh.Close()
	if _, err := fresh.Write([]byte("PING\r\n")); err != nil {
		t.Fatalf("Write в новое соединение: %v", err)
	}
	_ = fresh.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp := make([]byte, 64)
	n, err := fresh.Read(resp)
	if err != nil {
		t.Fatalf("новое соединение не обслуживается после освобождения слота: %v", err)
	}
	if string(resp[:n]) != "+OK\r\n" {
		t.Fatalf("ответ %q, ожидали +OK", resp[:n])
	}
}
