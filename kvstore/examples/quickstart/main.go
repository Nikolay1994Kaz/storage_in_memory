// Quickstart: сквозной пример продуктового пути на РЕАЛЬНЫХ эмбеддингах.
//
// Что показывает (весь путь через RESP, без клиентских библиотек):
//  1. AI.EMBED        — реальные эмбеддинги (nomic-embed-text через Ollama);
//  2. VSIM.ADDATTR    — ингест вектора с колоночными атрибутами (tenant/категория/год);
//  3. VSIM.SEARCH     — глобальный ANN-поиск;
//  4. VSIM.FILTER     — тот же поиск, но суженный на тенант и числовой диапазон;
//  5. AI.SEARCH       — то же одной командой (embed на стороне сервера).
//
// Запуск: docker compose up -d && go run ./kvstore/examples/quickstart
// Подробности — в README.md рядом.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// ─── Минимальный RESP-клиент ────────────────────────────────────────────────
// Намеренно без зависимостей: весь протокол сервера — это RESP-массивы bulk-строк,
// клиент на любом языке пишется за экран кода (см. sdk/python для Python-версии).

type client struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
}

func dial(addr string) (*client, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	return &client{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}, nil
}

// do отправляет команду как RESP-массив bulk-строк и читает один ответ.
func (c *client) do(args ...string) (any, error) {
	fmt.Fprintf(c.w, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(c.w, "$%d\r\n%s\r\n", len(a), a)
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	return c.readReply()
}

func (c *client) readReply() (any, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, errors.New("empty reply line")
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return nil, errors.New(body)
	case ':':
		var n int64
		fmt.Sscan(body, &n)
		return n, nil
	case '$':
		var n int
		fmt.Sscan(body, &n)
		if n < 0 {
			return nil, nil // nil bulk
		}
		buf := make([]byte, n+2) // +2 = завершающий \r\n
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		var n int
		fmt.Sscan(body, &n)
		if n < 0 {
			return nil, nil
		}
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			v, err := c.readReply()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("unexpected reply type %q", line[0])
	}
}

// toStrings превращает ответ-массив в []string (все ответы сервера — bulk-строки).
func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

// ─── Демо-корпус ────────────────────────────────────────────────────────────
// Два тенанта с разной предметной областью, чтобы фильтрация была видна глазами:
// поисковая выдача обязана поменяться при сужении на тенант.

type doc struct {
	key    string
	tenant string
	topic  string
	year   int
	text   string
}

var corpus = []doc{
	{"doc:acme:1", "acme", "propulsion", 2021, "Reusable rockets land vertically on autonomous drone ships"},
	{"doc:acme:2", "acme", "comms", 2023, "Satellite constellations provide global broadband internet coverage"},
	{"doc:acme:3", "acme", "propulsion", 2019, "Ion thrusters propel deep space probes using minimal fuel"},
	{"doc:acme:4", "acme", "life-support", 2024, "Astronauts grow lettuce aboard the station in microgravity"},
	{"doc:acme:5", "acme", "reentry", 2026, "Ablative heat shields protect capsules during atmospheric re-entry"},
	{"doc:globex:1", "globex", "mains", 2022, "Slow-cooked beef stew with root vegetables and red wine"},
	{"doc:globex:2", "globex", "baking", 2020, "Sourdough bread needs a mature starter and long fermentation"},
	{"doc:globex:3", "globex", "sauces", 2025, "Fresh basil pesto with pine nuts, garlic and olive oil"},
	{"doc:globex:4", "globex", "mains", 2026, "Char-grilled salmon fillet with lemon butter and asparagus"},
	{"doc:globex:5", "globex", "baking", 2018, "Crispy pizza crust requires a very hot stone and thin dough"},
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "quickstart: "+format+"\n", a...)
	os.Exit(1)
}

// printHits печатает плоский массив key,dist,key,dist... из VSIM.*/AI.SEARCH.
func printHits(v any) {
	hits := toStrings(v)
	if len(hits) == 0 {
		fmt.Println("    (no results)")
		return
	}
	for i := 0; i+1 < len(hits); i += 2 {
		var text string
		for _, d := range corpus {
			if d.key == hits[i] {
				text = fmt.Sprintf("%q  (tenant=%s year=%d)", d.text, d.tenant, d.year)
				break
			}
		}
		fmt.Printf("    %-14s dist=%-9s %s\n", hits[i], hits[i+1], text)
	}
}

func main() {
	addr := flag.String("addr", "localhost:6380", "kvstore-server address")
	flag.Parse()

	c, err := dial(*addr)
	if err != nil {
		fatalf("cannot connect to %s (is the server running? try: docker compose up -d): %v", *addr, err)
	}
	defer c.conn.Close()

	if _, err := c.do("PING"); err != nil {
		fatalf("PING failed: %v", err)
	}

	// Ollama обязателен для этого примера: без него нет реальных эмбеддингов.
	if _, err := c.do("AI.EMBED", "warmup"); err != nil {
		fatalf("AI.EMBED failed: %v\n\nThis example needs Ollama with the embedding model.\nEasiest path: docker compose up -d (pulls nomic-embed-text automatically),\nthen run this example against localhost:6380.", err)
	}

	// ── 1. Ингест: реальный эмбеддинг + колоночные атрибуты ──
	fmt.Printf("Ingesting %d documents (embed via Ollama + VSIM.ADDATTR)...\n", len(corpus))
	for _, d := range corpus {
		emb, err := c.do("AI.EMBED", d.text)
		if err != nil {
			fatalf("embed %s: %v", d.key, err)
		}
		vec := toStrings(emb)
		// VSIM.ADDATTR <key> CAT tenant <t> CAT topic <c> NUM year <y> VEC f1..fN —
		// атрибуты ложатся в колоночный слой и durable через WAL (OpVSimAddAttrs).
		args := append([]string{"VSIM.ADDATTR", d.key,
			"CAT", "tenant", d.tenant,
			"CAT", "topic", d.topic,
			"NUM", "year", fmt.Sprint(d.year),
			"VEC"}, vec...)
		if _, err := c.do(args...); err != nil {
			fatalf("VSIM.ADDATTR %s: %v", d.key, err)
		}
	}

	// VSIM.INFO — sync-точка: дельта сброшена в сегменты, состояние видно целиком.
	info, err := c.do("VSIM.INFO")
	if err != nil {
		fatalf("VSIM.INFO: %v", err)
	}
	fmt.Printf("Index state: %v\n\n", info)

	// ── 2. Глобальный семантический поиск ──
	query := "how does a spacecraft survive the heat of returning to Earth"
	fmt.Printf("Query: %q\n", query)
	emb, err := c.do("AI.EMBED", query)
	if err != nil {
		fatalf("embed query: %v", err)
	}
	qvec := toStrings(emb)

	fmt.Println("  VSIM.SEARCH 3 (all tenants):")
	res, err := c.do(append([]string{"VSIM.SEARCH", "3"}, qvec...)...)
	if err != nil {
		fatalf("VSIM.SEARCH: %v", err)
	}
	printHits(res)

	// ── 3. Тот же запрос, суженный на тенант ──
	fmt.Println("  VSIM.FILTER 3 EQ tenant globex (same query, tenant-scoped):")
	res, err = c.do(append([]string{"VSIM.FILTER", "3", "EQ", "tenant", "globex", "VEC"}, qvec...)...)
	if err != nil {
		fatalf("VSIM.FILTER: %v", err)
	}
	printHits(res)

	// ── 4. Тенант + числовой диапазон ──
	query2 := "quick dinner idea with fish"
	fmt.Printf("\nQuery: %q\n", query2)
	emb2, err := c.do("AI.EMBED", query2)
	if err != nil {
		fatalf("embed query: %v", err)
	}
	q2 := toStrings(emb2)

	fmt.Println("  VSIM.FILTER 3 EQ tenant globex RANGE year 2024 2026:")
	res, err = c.do(append([]string{"VSIM.FILTER", "3",
		"EQ", "tenant", "globex", "RANGE", "year", "2024", "2026", "VEC"}, q2...)...)
	if err != nil {
		fatalf("VSIM.FILTER: %v", err)
	}
	printHits(res)

	// ── 5. Однострочный вариант: embed на стороне сервера ──
	fmt.Println("\n  AI.SEARCH 3 (server-side embedding, one round-trip):")
	res, err = c.do("AI.SEARCH", "3", query2)
	if err != nil {
		fatalf("AI.SEARCH: %v", err)
	}
	printHits(res)

	fmt.Println("\nDone. Vectors and attributes are durable (WAL): restart the server and search again.")
}
