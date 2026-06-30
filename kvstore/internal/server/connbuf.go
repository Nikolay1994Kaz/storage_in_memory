package server

import (
	"bytes"
	"net"
	"strconv"
	"syscall"
	"time"

	"kvstore/kvstore/internal/monitoring"
)

const (
	readBufSize = 65536 // 64KB — вмещает ~1000 команд SET
	maxArgs     = 2048  // поддерживает высокоразмерные векторные команды (до 2046 floats)

	// maxBulkSize — верхняя граница длины одного bulk-string (как Redis
	// proto-max-bulk-len = 512 МБ). Без неё клиент объявляет "$<огромное>",
	// парсер ждёт данные, а read-слой бесконечно удваивает rbuf → remote
	// unauth OOM (mem-DoS). Превышение = protoErr (соединение закрывается).
	maxBulkSize = 512 * 1024 * 1024
)

// maxRbufSize — потолок роста буфера чтения. Даже с кэпом на bulk-длину остаётся
// вектор: клиент льёт байты БЕЗ \r\n (inline без перевода строки) → peekLine
// всегда false → парсер не делает прогресс → ReadFromConn/TryRead удваивают rbuf
// без предела → mem-DoS. По достижении потолка без полного кадра ставим protoErr.
//
// Запас readBufSize над maxBulkSize нужен, чтобы легитимный кадр максимального
// bulk ("$<512МБ>\r\n<512МБ>\r\n" + рамка/пайплайн) поместился целиком.
//
// var (не const) — тесты понижают потолок, чтобы не аллоцировать 512 МБ.
var maxRbufSize = maxBulkSize + readBufSize

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

	// WriteTimeout — макс. время на conn.Write в Flush. Без него медленный/
	// застрявший reader (полное TCP-окно) блокирует горутину воркера со ВСЕМИ
	// его соединениями. 0 = без дедлайна (legacy-поведение).
	WriteTimeout time.Duration

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

// ensureSpace гарантирует место в rbuf для следующего read.
//
// Если буфер заполнен — удваивает его, но НЕ выше maxRbufSize. По достижении
// потолка без полного кадра (клиент льёт данные, парсер не делает прогресс)
// выставляет protoErr и возвращает false: расти больше нельзя, соединение надо
// закрыть. Это закрывает mem-DoS через неограниченный рост буфера (напр. inline
// без \r\n), который кэп maxBulkSize сам по себе не ловит.
func (cb *ConnBuf) ensureSpace() bool {
	if cb.rend < len(cb.rbuf) {
		return true // место ещё есть
	}
	if len(cb.rbuf) >= maxRbufSize {
		cb.protoErr = true
		return false
	}
	newSize := len(cb.rbuf) * 2
	if newSize > maxRbufSize {
		newSize = maxRbufSize
	}
	newBuf := make([]byte, newSize)
	copy(newBuf, cb.rbuf)
	cb.rbuf = newBuf
	return true
}

// ReadFromConn читает данные из TCP сокета в буфер.
// Перед чтением компактифицирует буфер.
func (cb *ConnBuf) ReadFromConn() (int, error) {
	cb.compact()
	if !cb.ensureSpace() {
		return 0, nil // потолок rbuf достигнут, protoErr выставлен → handleConn закроет
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
	if !cb.ensureSpace() {
		return 0 // потолок rbuf достигнут, protoErr выставлен → handleConn закроет
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
	if count > maxArgs {
		// Объявлено больше аргументов, чем влезает в стековый args[maxArgs].
		// Кадр невалиден и его нельзя «дождать» — иначе он застрянет в буфере
		// (wedge + рост памяти). Сигналим handleConn закрыть соединение.
		cb.protoErr = true
		return nil
	}
	if !ok || count <= 0 {
		// Битый/неполный заголовок числа, *0, *-1 (null array) — легитимный nil.
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

	if size > maxBulkSize {
		// Длина выше кэпа: данных столько ещё нет, парсер вернул бы nil
		// («ждём»), а read-слой удваивал бы rbuf к OOM. Рвём соединение.
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
	const maxInt = int(^uint(0) >> 1)
	n := 0
	for ; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		// Защита от переполнения: иначе огромная длина «обернулась» бы в
		// положительное число и проскочила бы кэп maxBulkSize/maxArgs.
		if n > (maxInt-int(c-'0'))/10 {
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
//
// WriteTimeout ограничивает время на conn.Write: застрявший reader (полное
// TCP-окно) иначе блокирует горутину воркера со всеми его соединениями. По
// истечении дедлайна Write вернёт ошибку → handleConn закроет соединение.
func (cb *ConnBuf) Flush() error {
	if len(cb.wbuf) == 0 {
		return nil
	}
	if cb.WriteTimeout > 0 {
		_ = cb.conn.SetWriteDeadline(time.Now().Add(cb.WriteTimeout))
	}
	n, err := cb.conn.Write(cb.wbuf)
	if n > 0 {
		monitoring.BytesWritten.Add(n)
	}
	cb.wbuf = cb.wbuf[:0]
	return err
}
