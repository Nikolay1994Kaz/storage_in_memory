package server

import (
	"bytes"
	"net"
	"testing"
)

// ─── Helpers ───────────────────────────────────────────────
//
// newTestParser создаёт ConnBuf для тестов парсера.
//
// ПОЧЕМУ не через NewConnBuf?
// NewConnBuf принимает net.Conn (TCP). В тестах парсера нам TCP не нужен.
// Мы хотим проверить ЛОГИКУ парсера, а не сетевой I/O.
//
// КАК ЭТО РАБОТАЕТ:
//
//	rbuf — буфер чтения (64KB в проде, в тестах — размер input)
//	rpos — текущая позиция чтения (всегда 0 для свежего ввода)
//	rend — конец данных (= len(input))
//
// Парсер ничего не знает о TCP — он работает только с rbuf[rpos:rend].
// Это и позволяет тестировать его изолированно.
func newTestParser(input []byte) *ConnBuf {
	cb := &ConnBuf{
		rbuf: make([]byte, readBufSize),
		wbuf: make([]byte, 0, 4096),
	}
	copy(cb.rbuf, input)
	cb.rpos = 0
	cb.rend = len(input)
	return cb
}

// ─── Parser Tests ──────────────────────────────────────────

// TestParse_SimpleSet — парсинг *3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n
//
// Это САМЫЙ частый формат — RESP Array с Bulk Strings.
// Redis-клиенты (redis-cli, Go redis, Python redis) шлют именно так.
//
// Протокол RESP:
//
//	*3        → "массив из 3 элементов"
//	$3\r\nSET → "bulk string длиной 3, данные: SET"
//	$3\r\nkey → "bulk string длиной 3, данные: key"
//	$5\r\nvalue → "bulk string длиной 5, данные: value"
//
// Парсер должен вернуть: [["SET"], ["key"], ["value"]]
// Слайсы указывают ВНУТРЬ rbuf — это zero-alloc фишка.
func TestParse_SimpleSet(t *testing.T) {
	input := []byte("*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n")
	cb := newTestParser(input)

	args := cb.ParseCommand()

	if args == nil {
		t.Fatal("ParseCommand returned nil — не смог разобрать валидную команду")
	}

	// Проверяем количество аргументов
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d", len(args))
	}

	// Проверяем каждый аргумент
	// bytes.Equal — сравнение []byte без создания string
	if !bytes.Equal(args[0], []byte("SET")) {
		t.Fatalf("args[0] = %q, want SET", args[0])
	}
	if !bytes.Equal(args[1], []byte("key")) {
		t.Fatalf("args[1] = %q, want key", args[1])
	}
	if !bytes.Equal(args[2], []byte("value")) {
		t.Fatalf("args[2] = %q, want value", args[2])
	}

	// После разбора: rpos должен указывать на конец данных
	// Это важно! Если rpos не сдвинулся — следующий ParseCommand сломается.
	if cb.rpos != len(input) {
		t.Fatalf("rpos = %d, want %d (parser didn't consume all input)", cb.rpos, len(input))
	}
}

// TestParse_InlinePing — inline-формат: PING\r\n
//
// Inline — это упрощённый формат без $-заголовков.
// Его шлёт redis-cli когда вводишь команду интерактивно,
// и его же шлёт telnet: `telnet localhost 6379` → `PING`
//
// Отличие от RESP Array:
//
//	RESP:   *1\r\n$4\r\nPING\r\n  (17 байт)
//	Inline: PING\r\n              (6 байт)
//
// Парсер определяет формат по ПЕРВОМУ байту:
//
//	'*' → parseArray()
//	всё остальное → parseInline()
func TestParse_InlinePing(t *testing.T) {
	input := []byte("PING\r\n")
	cb := newTestParser(input)

	args := cb.ParseCommand()

	if args == nil {
		t.Fatal("ParseCommand returned nil for PING")
	}
	if len(args) != 1 {
		t.Fatalf("expected 1 arg, got %d", len(args))
	}
	if !bytes.Equal(args[0], []byte("PING")) {
		t.Fatalf("args[0] = %q, want PING", args[0])
	}
}

// TestParse_InlineWithArgs — inline: SET key value\r\n
//
// Пробелы разделяют аргументы. Парсер должен разбить на 3 части.
// Множественные пробелы "SET  key" должны игнорироваться
// (start = i + 1 пропускает пустые токены).
func TestParse_InlineWithArgs(t *testing.T) {
	input := []byte("SET mykey myvalue\r\n")
	cb := newTestParser(input)

	args := cb.ParseCommand()

	if args == nil {
		t.Fatal("ParseCommand returned nil for inline SET")
	}
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d", len(args))
	}
	if !bytes.Equal(args[0], []byte("SET")) {
		t.Fatalf("args[0] = %q, want SET", args[0])
	}
	if !bytes.Equal(args[1], []byte("mykey")) {
		t.Fatalf("args[1] = %q, want mykey", args[1])
	}
	if !bytes.Equal(args[2], []byte("myvalue")) {
		t.Fatalf("args[2] = %q, want myvalue", args[2])
	}
}

// TestParse_Incomplete — неполная команда должна вернуть nil.
//
// ЭТО КРИТИЧЕСКИ ВАЖНЫЙ ТЕСТ.
//
// TCP — потоковый протокол. Данные приходят фрагментами!
// Клиент отправил "*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n"
// Но ядро ОС может отдать только "*3\r\n$3\r\nSET\r\n" — остальное ещё в пути.
//
// Парсер ОБЯЗАН:
//  1. Вернуть nil (данных мало, команда неполная)
//  2. НЕ сдвигать rpos (savedPos restore) — чтобы при следующем Read
//     дочитать остаток и собрать полную команду
//
// Если парсер сдвинет rpos на неполных данных — данные потеряны навсегда.
func TestParse_Incomplete(t *testing.T) {
	// Обрезаем команду посередине — нет финального \r\n для "value"
	input := []byte("*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nval")
	cb := newTestParser(input)

	args := cb.ParseCommand()

	if args != nil {
		t.Fatal("ParseCommand should return nil for incomplete data")
	}

	// rpos должен остаться на 0 — данные НЕ потеряны
	if cb.rpos != 0 {
		t.Fatalf("rpos = %d, want 0 (parser must restore position on incomplete data)", cb.rpos)
	}
}

// TestParse_Pipeline — два запроса в одном TCP-пакете.
//
// Пайплайн — это КЛЮЧ к 5M RPS.
// Клиент отправляет 200 команд в одном write(), сервер парсит по одной.
//
// Механика:
//
//	ParseCommand() разбирает первую → сдвигает rpos
//	ParseCommand() разбирает вторую → сдвигает rpos
//	ParseCommand() → rpos == rend → nil (конец данных)
func TestParse_Pipeline(t *testing.T) {
	// Две команды подряд: SET + GET
	input := []byte("*3\r\n$3\r\nSET\r\n$1\r\na\r\n$1\r\nb\r\n*2\r\n$3\r\nGET\r\n$1\r\na\r\n")
	cb := newTestParser(input)

	// ── Первая команда: SET a b ──
	args1 := cb.ParseCommand()
	if args1 == nil {
		t.Fatal("first ParseCommand returned nil")
	}
	if len(args1) != 3 {
		t.Fatalf("cmd1: expected 3 args, got %d", len(args1))
	}
	if !bytes.Equal(args1[0], []byte("SET")) {
		t.Fatalf("cmd1: args[0] = %q, want SET", args1[0])
	}

	// ── Вторая команда: GET a ──
	args2 := cb.ParseCommand()
	if args2 == nil {
		t.Fatal("second ParseCommand returned nil")
	}
	if len(args2) != 2 {
		t.Fatalf("cmd2: expected 2 args, got %d", len(args2))
	}
	if !bytes.Equal(args2[0], []byte("GET")) {
		t.Fatalf("cmd2: args[0] = %q, want GET", args2[0])
	}
	if !bytes.Equal(args2[1], []byte("a")) {
		t.Fatalf("cmd2: args[1] = %q, want a", args2[1])
	}

	// ── Третий вызов: данных больше нет ──
	args3 := cb.ParseCommand()
	if args3 != nil {
		t.Fatal("third ParseCommand should return nil (no more data)")
	}
}

// TestParse_EmptyBuffer — пустой буфер → nil, без паники.
//
// Сценарий: сервер вызвал ParseCommand до того, как пришли данные.
// Не должно быть panic, index out of range и т.д.
func TestParse_EmptyBuffer(t *testing.T) {
	cb := newTestParser([]byte{})

	args := cb.ParseCommand()
	if args != nil {
		t.Fatal("empty buffer should return nil")
	}
}

// TestParse_NullBulkString — $-1\r\n (null bulk string).
//
// RESP протокол: $-1 означает NULL (ключ не существует).
// В парсере это обрабатывается как пустой []byte{}.
//
// Пример: *2\r\n$3\r\nGET\r\n$-1\r\n
// Это странный случай (клиент шлёт null как аргумент), но парсер не должен падать.
func TestParse_NullBulkString(t *testing.T) {
	input := []byte("*2\r\n$3\r\nGET\r\n$-1\r\n")
	cb := newTestParser(input)

	args := cb.ParseCommand()
	if args == nil {
		t.Fatal("ParseCommand returned nil")
	}
	if len(args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(args))
	}
	// $-1 должен дать пустой []byte (не nil)
	if args[1] == nil {
		t.Fatal("null bulk string should be empty []byte, not nil")
	}
	if len(args[1]) != 0 {
		t.Fatalf("null bulk string length = %d, want 0", len(args[1]))
	}
}

// ─── parseIntBytes Tests ───────────────────────────────────
//
// parseIntBytes — фундамент парсера. Каждый $N и *N проходит через неё.
// Если она сломана — ВСЁ сломано.

// TestParseIntBytes — table-driven тест для parseIntBytes.
//
// Table-driven — стандарт Go-тестирования:
// Все кейсы в одном массиве, цикл прогоняет каждый.
// Добавить новый кейс = добавить строку в таблицу.
func TestParseIntBytes(t *testing.T) {
	tests := []struct {
		name   string
		input  []byte
		wantN  int
		wantOK bool
	}{
		// Нормальные случаи
		{"zero", []byte("0"), 0, true},
		{"positive", []byte("123"), 123, true},
		{"negative", []byte("-1"), -1, true},
		{"large", []byte("65536"), 65536, true},

		// Граничные случаи
		{"empty", []byte{}, 0, false},
		{"non-digit", []byte("abc"), 0, false},
		{"mixed", []byte("12x"), 0, false},
		{"just-minus", []byte("-"), 0, false}, // "-" без цифр
		{"space", []byte(" 5"), 0, false},     // пробел перед числом
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, ok := parseIntBytes(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("parseIntBytes(%q): ok=%v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && n != tt.wantN {
				t.Fatalf("parseIntBytes(%q): n=%d, want %d", tt.input, n, tt.wantN)
			}
		})
	}
}

// ─── Compact Tests ─────────────────────────────────────────

// TestCompact_ShiftsData — compact сдвигает остаток данных к началу буфера.
//
// ЗАЧЕМ compact?
//
// Представь буфер после парсинга 3 команд:
//
//	[parsed|parsed|parsed|REMAINING DATA..........|free space]
//	 ^                    ^                        ^
//	 0                   rpos                     rend
//
// Без compact: rend дойдёт до конца буфера (65536) и Read вернёт 0 байт.
// С compact:   REMAINING DATA сдвигается к позиции 0, rpos=0, rend=len(remaining).
//
// compact() вызывается перед КАЖДЫМ Read — это гарантирует,
// что в буфере всегда есть место для новых данных.
func TestCompact_ShiftsData(t *testing.T) {
	cb := newTestParser([]byte("AAAAABBBBB"))

	// Имитируем: парсер разобрал первые 5 байт
	cb.rpos = 5

	// compact должен сдвинуть "BBBBB" к началу
	cb.compact()

	if cb.rpos != 0 {
		t.Fatalf("rpos = %d, want 0", cb.rpos)
	}
	if cb.rend != 5 {
		t.Fatalf("rend = %d, want 5", cb.rend)
	}
	if string(cb.rbuf[:5]) != "BBBBB" {
		t.Fatalf("data = %q, want BBBBB", cb.rbuf[:5])
	}
}

// TestCompact_AllConsumed — если rpos == rend, просто сбрасываем в 0.
func TestCompact_AllConsumed(t *testing.T) {
	cb := newTestParser([]byte("data"))
	cb.rpos = 4 // всё прочитано
	cb.rend = 4

	cb.compact()

	if cb.rpos != 0 || cb.rend != 0 {
		t.Fatalf("after compact: rpos=%d, rend=%d, want 0,0", cb.rpos, cb.rend)
	}
}

// TestCompact_NothingToShift — rpos == 0, compact ничего не делает.
func TestCompact_NothingToShift(t *testing.T) {
	cb := newTestParser([]byte("hello"))

	cb.compact() // rpos=0, ничего не нужно сдвигать

	if cb.rpos != 0 || cb.rend != 5 {
		t.Fatalf("compact shouldn't change anything: rpos=%d, rend=%d", cb.rpos, cb.rend)
	}
}

// ─── Write Side Tests ──────────────────────────────────────
//
// Write-методы просто append'ят RESP-формат в wbuf.
// Здесь мы проверяем, что формат корректен байт-в-байт.

// TestWrite_SimpleString — "+OK\r\n"
func TestWrite_SimpleString(t *testing.T) {
	cb := &ConnBuf{wbuf: make([]byte, 0, 256)}

	cb.WriteSimpleString("OK")

	want := "+OK\r\n"
	if string(cb.wbuf) != want {
		t.Fatalf("wbuf = %q, want %q", cb.wbuf, want)
	}
}

// TestWrite_Error — "-ERR message\r\n"
func TestWrite_Error(t *testing.T) {
	cb := &ConnBuf{wbuf: make([]byte, 0, 256)}

	cb.WriteError("ERR unknown command")

	want := "-ERR unknown command\r\n"
	if string(cb.wbuf) != want {
		t.Fatalf("wbuf = %q, want %q", cb.wbuf, want)
	}
}

// TestWrite_Int — ":42\r\n"
func TestWrite_Int(t *testing.T) {
	cb := &ConnBuf{wbuf: make([]byte, 0, 256)}

	cb.WriteInt(42)

	want := ":42\r\n"
	if string(cb.wbuf) != want {
		t.Fatalf("wbuf = %q, want %q", cb.wbuf, want)
	}
}

// TestWrite_BulkString — "$5\r\nhello\r\n"
func TestWrite_BulkString(t *testing.T) {
	cb := &ConnBuf{wbuf: make([]byte, 0, 256)}

	cb.WriteBulkString("hello")

	want := "$5\r\nhello\r\n"
	if string(cb.wbuf) != want {
		t.Fatalf("wbuf = %q, want %q", cb.wbuf, want)
	}
}

// TestWrite_Null — "$-1\r\n"
func TestWrite_Null(t *testing.T) {
	cb := &ConnBuf{wbuf: make([]byte, 0, 256)}

	cb.WriteNull()

	want := "$-1\r\n"
	if string(cb.wbuf) != want {
		t.Fatalf("wbuf = %q, want %q", cb.wbuf, want)
	}
}

// TestWrite_Multiple — несколько ответов накапливаются в wbuf.
//
// Это проверяет пайплайн-ответ: сервер разобрал 3 команды,
// записал 3 ответа в wbuf, затем Flush() отправит всё одним syscall.
func TestWrite_Multiple(t *testing.T) {
	cb := &ConnBuf{wbuf: make([]byte, 0, 256)}

	cb.WriteSimpleString("OK")
	cb.WriteBulkString("hello")
	cb.WriteNull()

	want := "+OK\r\n$5\r\nhello\r\n$-1\r\n"
	if string(cb.wbuf) != want {
		t.Fatalf("wbuf = %q, want %q", cb.wbuf, want)
	}
}

// ─── Flush Test ────────────────────────────────────────────

// TestFlush — Flush отправляет wbuf через соединение и очищает буфер.
//
// net.Pipe() — встроенный фейковый TCP:
//
//	client, server := net.Pipe()
//	client.Write() → server.Read()  (и наоборот)
//
// Нам не нужен настоящий TCP для проверки Flush.
func TestFlush(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	cb := NewConnBuf(server)
	cb.WriteSimpleString("PONG")

	// Создаем канал для синхронизации
	done := make(chan struct{})

	go func() {
		if err := cb.Flush(); err != nil {
			t.Errorf("Flush error: %v", err)
		}
		// Сигнализируем, что Flush ПОЛНОСТЬЮ завершен (включая очистку буфера)
		close(done)
	}()

	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}

	want := "+PONG\r\n"
	if string(buf[:n]) != want {
		t.Fatalf("received = %q, want %q", buf[:n], want)
	}

	// ЖДЕМ завершения фоновой горутины
	<-done

	// Теперь проверять безопасно — Data Race не будет
	if len(cb.wbuf) != 0 {
		t.Fatalf("wbuf length = %d after Flush, want 0", len(cb.wbuf))
	}
}

// TestFlush_Empty — Flush на пустом wbuf не должен вызывать Write.
func TestFlush_Empty(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	cb := NewConnBuf(server)

	// Flush с пустым wbuf — должен вернуть nil, не зависнуть
	err := cb.Flush()
	if err != nil {
		t.Fatalf("Flush on empty wbuf: %v", err)
	}
}

// ─── Edge Cases ────────────────────────────────────────────

// TestParse_MaxArgsOverflow — команда с > 16 аргументами.
//
// maxArgs = 16. Если клиент пришлёт *17, парсер вернёт nil.
// Это защита: массив args[maxArgs] на СТЕКЕ, переполнение = паника.
func TestParse_MaxArgsOverflow(t *testing.T) {
	input := []byte("*2049\r\n")
	// Добавляем 2049 аргументов
	for i := 0; i < 2049; i++ {
		input = append(input, []byte("$1\r\nx\r\n")...)
	}
	cb := newTestParser(input)

	args := cb.ParseCommand()
	if args != nil {
		t.Fatal("ParseCommand should return nil for > maxArgs arguments")
	}
}

// TestParse_ZeroCount — *0\r\n (пустой массив).
//
// *0 = "массив из 0 элементов". Парсер вернёт nil (count <= 0).
func TestParse_ZeroCount(t *testing.T) {
	input := []byte("*0\r\n")
	cb := newTestParser(input)

	args := cb.ParseCommand()
	if args != nil {
		t.Fatal("*0 should return nil (no args)")
	}
}

// TestParse_NegativeCount — *-1\r\n (null array).
func TestParse_NegativeCount(t *testing.T) {
	input := []byte("*-1\r\n")
	cb := newTestParser(input)

	args := cb.ParseCommand()
	if args != nil {
		t.Fatal("*-1 should return nil (null array)")
	}
}

// ─── Benchmarks ────────────────────────────────────────────

// BenchmarkParseCommand_RESP — скорость парсинга RESP-команды.
//
// Показывает сколько наносекунд уходит на разбор одной команды SET.
// На твоём CPU ожидаем ~50-100 ns/op.
//
// ТРЮК: после каждого ParseCommand мы сбрасываем rpos=0, rend=len(input).
// Иначе второй вызов вернёт nil (данные "съедены") и бенчмарк
// будет мерить скорость return nil — бесполезно.
//
// Запуск: go test ./internal/server/ -bench BenchmarkParse -benchtime 3s
func BenchmarkParseCommand_RESP(b *testing.B) {
	input := []byte("*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n")
	cb := newTestParser(input)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cb.ParseCommand()
		// Сброс позиции — иначе парсер упрётся в конец буфера
		cb.rpos = 0
		cb.rend = len(input)
	}
}

// BenchmarkParseCommand_Inline — скорость парсинга inline-команды.
//
// Inline проще (нет $-заголовков), поэтому должен быть быстрее RESP.
func BenchmarkParseCommand_Inline(b *testing.B) {
	input := []byte("SET key value\r\n")
	cb := newTestParser(input)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cb.ParseCommand()
		cb.rpos = 0
		cb.rend = len(input)
	}
}

// BenchmarkParseIntBytes — скорость zero-alloc parseInt.
//
// Сравни мысленно с strconv.Atoi(string(b)):
//
//	strconv.Atoi(string(b)) = аллокация string + парсинг
//	parseIntBytes(b) = только парсинг, ноль аллокаций
func BenchmarkParseIntBytes(b *testing.B) {
	input := []byte("12345")
	for i := 0; i < b.N; i++ {
		parseIntBytes(input)
	}
}
