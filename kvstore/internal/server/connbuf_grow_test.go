package server

import (
	"bytes"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestConnBuf_IdleFootprint — молчащее соединение держит килобайты, а не
// сотню: прежде каждый ConnBuf сразу выделял 64 КБ буфера чтения и встроенный
// массив на maxArgs аргументов (48 КБ) — около 116 КБ кучи на соединение.
func TestConnBuf_IdleFootprint(t *testing.T) {
	const n = 2000
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	bufs := make([]*ConnBuf, n)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range bufs {
		bufs[i] = NewConnBuf(c1)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	perConn := (after.HeapAlloc - before.HeapAlloc) / n
	if perConn > 16<<10 {
		t.Fatalf("ConnBuf молчащего соединения занимает %d Б кучи, ожидалось не больше 16 КБ", perConn)
	}
	runtime.KeepAlive(bufs)
}

// readCommand читает из соединения, пока ConnBuf не разберёт команду целиком.
func readCommand(t *testing.T, cb *ConnBuf) [][]byte {
	t.Helper()
	for {
		if args := cb.ParseCommand(); args != nil {
			return args
		}
		if cb.ProtoErr() {
			t.Fatal("ProtoErr на корректной команде")
		}
		if _, err := cb.ReadFromConn(); err != nil {
			t.Fatalf("ReadFromConn: %v", err)
		}
	}
}

// TestConnBuf_GrowsForLargeCommand — команда больше стартового буфера и
// стартовой ёмкости аргументов разбирается целиком: оба буфера растут.
func TestConnBuf_GrowsForLargeCommand(t *testing.T) {
	const nArgs = 2000 // близко к maxArgs: высокоразмерный вектор
	var cmd bytes.Buffer
	cmd.WriteString("*" + strconv.Itoa(nArgs) + "\r\n")
	for i := 0; i < nArgs; i++ {
		v := "v" + strconv.Itoa(i) + strings.Repeat("x", 12)
		cmd.WriteString("$" + strconv.Itoa(len(v)) + "\r\n" + v + "\r\n")
	}
	if cmd.Len() <= initialReadBuf*4 {
		t.Fatalf("команда %d Б не превышает стартовый буфер с запасом — тест ни о чём", cmd.Len())
	}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() { _, _ = c2.Write(cmd.Bytes()) }()

	cb := NewConnBuf(c1)
	args := readCommand(t, cb)
	if len(args) != nArgs {
		t.Fatalf("разобрано %d аргументов, ожидалось %d", len(args), nArgs)
	}
	for _, i := range []int{0, 1, nArgs / 2, nArgs - 1} {
		want := "v" + strconv.Itoa(i) + strings.Repeat("x", 12)
		if string(args[i]) != want {
			t.Fatalf("аргумент %d = %q, ожидалось %q", i, args[i], want)
		}
	}
}

// TestConnBuf_ShrinksAfterHugeCommand — буфер, раздутый одной огромной
// командой, отдаётся, когда опустеет; прежде он жил до закрытия соединения.
func TestConnBuf_ShrinksAfterHugeCommand(t *testing.T) {
	big := strings.Repeat("z", 3<<20) // 3 МБ — больше shrinkReadBuf
	frame := "*2\r\n$3\r\nSET\r\n$" + strconv.Itoa(len(big)) + "\r\n" + big + "\r\n"

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() { _, _ = c2.Write([]byte(frame)) }()

	cb := NewConnBuf(c1)
	args := readCommand(t, cb)
	if len(args) != 2 || len(args[1]) != len(big) {
		t.Fatalf("огромная команда разобрана неверно: %d аргументов", len(args))
	}
	if len(cb.rbuf) <= shrinkReadBuf {
		t.Fatalf("буфер не вырос под команду (%d Б) — тест ни о чём", len(cb.rbuf))
	}
	cb.compact() // буфер пуст: команда разобрана целиком
	if len(cb.rbuf) != initialReadBuf {
		t.Fatalf("после огромной команды буфер %d Б, ожидалось %d", len(cb.rbuf), initialReadBuf)
	}
}
