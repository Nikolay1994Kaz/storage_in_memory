package server

import (
	"bytes"
	"net"
	"strconv"
	"syscall"

	"kvstore/kvstore/internal/monitoring"
)

const (
	readBufSize = 65536 // 64KB — вмещает ~1000 команд SET
	maxArgs     = 2048  // поддерживает высокоразмерные векторные команды (до 2046 floats)
)

// ConnBuf — per-connection zero-alloc буфер чтения/записи.
//
// Заменяет: bufio.Reader + bufio.Writer + protocol.Value + Marshal().
//
// Read path (zero-alloc):
//
//	conn.Read() → rbuf        (ONE syscall, данные сразу в наш буфер)
//	ParseCommand() → [][]byte (слайсы ВНУТРЬ rbuf, ноль аллокаций)
//	compact() → сдвиг остатков к началу буфера
//
// Write path (zero-alloc):
//
//	WriteOK/WriteBulk/... → append в wbuf (прямая RESP-кодировка, без Value)
//	Flush() → conn.Write(wbuf)  (ONE syscall на ВСЕ ответы)
type ConnBuf struct {
	conn    net.Conn
	rawConn syscall.RawConn // для non-blocking TryRead

	// ─── Read side ───
	rbuf []byte
	rpos int
	rend int

	// protoErr — выставляется парсером при нарушении протокола (напр. невалидная
	// bulk-длина). Битый кадр нельзя «дождать» — он навсегда останется в буфере,
	// поэтому handleConn при этом флаге отвечает ошибкой и закрывает соединение.
	protoErr bool

	// ─── Write side ───
	wbuf []byte

	// ─── Parser reuse ───
	args [maxArgs][]byte
}

// NewConnBuf создаёт буфер для соединения.
func NewConnBuf(conn net.Conn) *ConnBuf {
	cb := &ConnBuf{
		conn: conn,
		rbuf: make([]byte, readBufSize),
		wbuf: make([]byte, 0, 4096),
	}

	// Извлекаем RawConn для non-blocking read (TryRead).
	// RawConn.Read() корректно работает с Go runtime netpoller.
	if sc, ok := conn.(syscall.Conn); ok {
		cb.rawConn, _ = sc.SyscallConn()
	}

	return cb
}

// ══════════════════════════════════════════════════
// READ SIDE
// ══════════════════════════════════════════════════

// ReadFromConn читает данные из TCP сокета в буфер.
// Перед чтением компактифицирует буфер.
func (cb *ConnBuf) ReadFromConn() (int, error) {
	cb.compact()
	if cb.rend == len(cb.rbuf) {
		newBuf := make([]byte, len(cb.rbuf)*2)
		copy(newBuf, cb.rbuf)
		cb.rbuf = newBuf
	}
	n, err := cb.conn.Read(cb.rbuf[cb.rend:])
	if n > 0 {
		monitoring.BytesRead.Add(n)
	}
	cb.rend += n
	return n, err
}

// TryRead делает non-blocking read через RawConn.
//
// ★ GREEDY DRAIN ★
//
// RawConn.Read() вызывает нашу функцию с raw fd.
// Если функция возвращает false — Go runtime НЕ блокирует горутину,
// а повторяет когда данные появятся. Но мы всегда возвращаем true,
// чтобы не блокироваться.
//
// Если EAGAIN — данных нет, возвращаем 0.
func (cb *ConnBuf) TryRead() int {
	monitoring.GreedyReads.Inc()
	if cb.rawConn == nil {
		return 0
	}
	cb.compact()
	if cb.rend == len(cb.rbuf) {
		newBuf := make([]byte, len(cb.rbuf)*2)
		copy(newBuf, cb.rbuf)
		cb.rbuf = newBuf
	}

	var nRead int
	cb.rawConn.Read(func(fd uintptr) bool {
		n, err := syscall.Read(int(fd), cb.rbuf[cb.rend:])
		if n > 0 {
			nRead = n
			cb.rend += n
			monitoring.BytesRead.Add(n)
			monitoring.GreedyHits.Inc()
		}
		// Всегда true: не ждём данных, просто проверяем
		_ = err
		return true
	})
	return nRead
}

// compact сдвигает неразобранные данные к началу буфера.
func (cb *ConnBuf) compact() {
	if cb.rpos == 0 {
		return
	}
	if cb.rpos == cb.rend {
		cb.rpos = 0
		cb.rend = 0
		return
	}
	n := copy(cb.rbuf[:], cb.rbuf[cb.rpos:cb.rend])
	cb.rpos = 0
	cb.rend = n
}

// ══════════════════════════════════════════════════
// ZERO-ALLOC RESP PARSER
// ══════════════════════════════════════════════════

// ParseCommand разбирает одну RESP-команду из буфера.
// Возвращает [][]byte — слайсы ВНУТРЬ rbuf (zero-alloc).
// Возвращает nil если данных недостаточно.
func (cb *ConnBuf) ParseCommand() [][]byte {
	if cb.rpos >= cb.rend {
		return nil
	}

	savedPos := cb.rpos

	var result [][]byte
	switch cb.rbuf[cb.rpos] {
	case '*':
		result = cb.parseArray()
	default:
		result = cb.parseInline()
	}

	if result == nil {
		cb.rpos = savedPos
	}
	return result
}

// ProtoErr сообщает, что парсер встретил нарушение протокола и соединение нужно
// закрыть. Битый кадр нельзя «дождать» (он останется в буфере навсегда), поэтому
// handleConn проверяет этот флаг после разбора и рвёт соединение с RESP-ошибкой.
func (cb *ConnBuf) ProtoErr() bool { return cb.protoErr }

// parseArray разбирает RESP массив: *3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n
func (cb *ConnBuf) parseArray() [][]byte {
	line, ok := cb.peekLine()
	if !ok {
		return nil
	}
	cb.rpos += len(line) + 2

	// Zero-alloc parseInt: НЕ создаём string, парсим прямо из []byte
	count, ok := parseIntBytes(line[1:])
	if !ok || count <= 0 || count > maxArgs {
		return nil
	}

	for i := 0; i < count; i++ {
		if cb.rpos >= cb.rend {
			return nil
		}

		switch cb.rbuf[cb.rpos] {
		case '$':
			arg := cb.parseBulkString()
			if arg == nil {
				return nil
			}
			cb.args[i] = arg
		default:
			line, ok := cb.peekLine()
			if !ok {
				return nil
			}
			cb.rpos += len(line) + 2
			cb.args[i] = line[1:]
		}
	}

	return cb.args[:count]
}

// parseBulkString разбирает: $5\r\nhello\r\n → []byte("hello")
// Возвращает слайс rbuf (zero-copy).
func (cb *ConnBuf) parseBulkString() []byte {
	line, ok := cb.peekLine()
	if !ok {
		return nil
	}
	cb.rpos += len(line) + 2

	// Zero-alloc parseInt вместо strconv.Atoi(string(...))
	size, ok := parseIntBytes(line[1:])
	if !ok {
		return nil
	}

	if size < 0 {
		// $-1\r\n = легитимный null bulk string. Любая другая отрицательная длина —
		// нарушение протокола: без этого гарда cb.rbuf[rpos:rpos+size] при size<-1
		// даёт low>high → slice-bounds panic (remote unauth DoS, т.к. парсинг идёт
		// до AUTH). Сигналим handleConn закрыть соединение.
		if size == -1 {
			return []byte{}
		}
		cb.protoErr = true
		return nil
	}

	end := cb.rpos + size + 2
	if end > cb.rend {
		return nil
	}

	data := cb.rbuf[cb.rpos : cb.rpos+size]
	cb.rpos = end
	return data
}

// parseInline разбирает: PING\r\n или SET key value\r\n
func (cb *ConnBuf) parseInline() [][]byte {
	line, ok := cb.peekLine()
	if !ok {
		return nil
	}
	cb.rpos += len(line) + 2

	argc := 0
	start := 0
	for i := 0; i <= len(line); i++ {
		if i == len(line) || line[i] == ' ' {
			if i > start && argc < maxArgs {
				cb.args[argc] = line[start:i]
				argc++
			}
			start = i + 1
		}
	}

	if argc == 0 {
		return nil
	}
	return cb.args[:argc]
}

// peekLine ищет \r\n начиная с rpos.
// bytes.IndexByte использует SIMD (AVX2) — ~10x быстрее цикла.
func (cb *ConnBuf) peekLine() ([]byte, bool) {
	data := cb.rbuf[cb.rpos:cb.rend]
	idx := bytes.IndexByte(data, '\n')
	if idx < 1 {
		return nil, false
	}
	if data[idx-1] != '\r' {
		return nil, false
	}
	return data[:idx-1], true
}

// parseIntBytes — zero-alloc parseInt из []byte.
//
// strconv.Atoi(string(b)) создаёт строку на хипе (allocation!).
// parseIntBytes парсит число прямо из []byte — ноль аллокаций.
//
// Поддерживает отрицательные числа (для $-1 = null bulk string).
func parseIntBytes(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	neg := false
	i := 0
	if b[0] == '-' {
		neg = true
		i = 1
		if len(b) == 1 {
			return 0, false
		}
	}
	n := 0
	for ; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}

// ══════════════════════════════════════════════════
// WRITE SIDE — Direct RESP Encoding
// ══════════════════════════════════════════════════

// WriteSimpleString пишет "+msg\r\n"
func (cb *ConnBuf) WriteSimpleString(msg string) {
	cb.wbuf = append(cb.wbuf, '+')
	cb.wbuf = append(cb.wbuf, msg...)
	cb.wbuf = append(cb.wbuf, '\r', '\n')
}

// WriteError пишет "-msg\r\n"
func (cb *ConnBuf) WriteError(msg string) {
	cb.wbuf = append(cb.wbuf, '-')
	cb.wbuf = append(cb.wbuf, msg...)
	cb.wbuf = append(cb.wbuf, '\r', '\n')
}

// WriteInt пишет ":N\r\n"
func (cb *ConnBuf) WriteInt(n int) {
	cb.wbuf = append(cb.wbuf, ':')
	cb.wbuf = strconv.AppendInt(cb.wbuf, int64(n), 10)
	cb.wbuf = append(cb.wbuf, '\r', '\n')
}

// WriteBulkString пишет "$len\r\ndata\r\n" из string.
func (cb *ConnBuf) WriteBulkString(s string) {
	cb.wbuf = append(cb.wbuf, '$')
	cb.wbuf = strconv.AppendInt(cb.wbuf, int64(len(s)), 10)
	cb.wbuf = append(cb.wbuf, '\r', '\n')
	cb.wbuf = append(cb.wbuf, s...)
	cb.wbuf = append(cb.wbuf, '\r', '\n')
}

// WriteBulk пишет "$len\r\ndata\r\n" из []byte.
func (cb *ConnBuf) WriteBulk(data []byte) {
	cb.wbuf = append(cb.wbuf, '$')
	cb.wbuf = strconv.AppendInt(cb.wbuf, int64(len(data)), 10)
	cb.wbuf = append(cb.wbuf, '\r', '\n')
	cb.wbuf = append(cb.wbuf, data...)
	cb.wbuf = append(cb.wbuf, '\r', '\n')
}

// WriteNull пишет "$-1\r\n"
func (cb *ConnBuf) WriteNull() {
	cb.wbuf = append(cb.wbuf, "$-1\r\n"...)
}

// WriteArrayHeader пишет "*N\r\n"
func (cb *ConnBuf) WriteArrayHeader(n int) {
	cb.wbuf = append(cb.wbuf, '*')
	cb.wbuf = strconv.AppendInt(cb.wbuf, int64(n), 10)
	cb.wbuf = append(cb.wbuf, '\r', '\n')
}

// Flush отправляет все накопленные ответы одним syscall.
func (cb *ConnBuf) Flush() error {
	if len(cb.wbuf) == 0 {
		return nil
	}
	n, err := cb.conn.Write(cb.wbuf)
	if n > 0 {
		monitoring.BytesWritten.Add(n)
	}
	cb.wbuf = cb.wbuf[:0]
	return err
}
