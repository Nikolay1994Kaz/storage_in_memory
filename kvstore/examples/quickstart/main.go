// Quickstart: сквозной пример продуктового пути на РЕАЛЬНЫХ эмбеддингах.
//
// Что показывает (весь путь к серверу через RESP, без клиентских библиотек):
//  1. embed           — реальные эмбеддинги СЧИТАЕТ КЛИЕНТ (nomic-embed-text через
//     Ollama на этой машине); сервер получает готовые векторы —
//     это и есть BYO-embeddings, любой провайдер даёт тот же []float32;
//  2. VSIM.ADDATTR    — ингест вектора с колоночными атрибутами (tenant/категория/год);
//  3. VSIM.SEARCH     — глобальный ANN-поиск;
//  4. VSIM.FILTER     — тот же поиск, но суженный на тенант и числовой диапазон.
//
// Запуск: ollama pull nomic-embed-text && docker compose up -d && go run ./kvstore/examples/quickstart
// Подробности — в README.md рядом.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ─── Минимальный RESP-клиент ────────────────────────────────────────────────
// Намеренно без зависимостей: весь протокол сервера — это RESP-массивы bulk-строк,
// клиент на любом языке пишется за экран кода.

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

// ─── Эмбеддер на стороне клиента ────────────────────────────────────────────
// Сервер про Ollama не знает: он принимает готовые float32 через VSIM.ADD*.
// Здесь Ollama только потому, что пример должен быть воспроизводим без ключей
// к платным API; подставьте любой провайдер, формат вектора тот же.

type embedder struct {
	baseURL string
	model   string
	http    *http.Client
}

// embed возвращает вектор текста как строки для RESP (VEC f1 … fN).
func (e *embedder) embed(text string) ([]string, error) {
	body, _ := json.Marshal(map[string]any{"model": e.model, "input": text})
	resp, err := e.http.Post(e.baseURL+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) == 0 || len(out.Embeddings[0]) == 0 {
		return nil, errors.New("ollama returned no embedding")
	}
	vec := make([]string, len(out.Embeddings[0]))
	for i, v := range out.Embeddings[0] {
		vec[i] = strconv.FormatFloat(float64(v), 'f', 6, 32)
	}
	return vec, nil
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

// printHits печатает плоский массив key,dist,key,dist... из VSIM.*.
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
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama base URL (client-side embeddings)")
	model := flag.String("model", "nomic-embed-text", "Ollama embedding model")
	flag.Parse()
	emb := &embedder{baseURL: *ollamaURL, model: *model, http: &http.Client{Timeout: 60 * time.Second}}

	c, err := dial(*addr)
	if err != nil {
		fatalf("cannot connect to %s (is the server running? try: docker compose up -d): %v", *addr, err)
	}
	defer c.conn.Close()

	if _, err := c.do("PING"); err != nil {
		fatalf("PING failed: %v", err)
	}

	// Ollama нужна ПРИМЕРУ, не серверу: без неё нет реальных эмбеддингов.
	if _, err := emb.embed("warmup"); err != nil {
		fatalf("embedding failed: %v\n\nThis example computes embeddings on the client via Ollama.\nInstall it, then: ollama pull %s  (server URL: -ollama %s)", err, *model, *ollamaURL)
	}

	// ── 1. Ингест: реальный эмбеддинг + колоночные атрибуты ──
	fmt.Printf("Ingesting %d documents (client-side embed + VSIM.ADDATTR)...\n", len(corpus))
	for _, d := range corpus {
		vec, err := emb.embed(d.text)
		if err != nil {
			fatalf("embed %s: %v", d.key, err)
		}
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
	qvec, err := emb.embed(query)
	if err != nil {
		fatalf("embed query: %v", err)
	}

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
	q2, err := emb.embed(query2)
	if err != nil {
		fatalf("embed query: %v", err)
	}

	fmt.Println("  VSIM.FILTER 3 EQ tenant globex RANGE year 2024 2026:")
	res, err = c.do(append([]string{"VSIM.FILTER", "3",
		"EQ", "tenant", "globex", "RANGE", "year", "2024", "2026", "VEC"}, q2...)...)
	if err != nil {
		fatalf("VSIM.FILTER: %v", err)
	}
	printHits(res)

	fmt.Println("\nDone. Vectors and attributes are durable (WAL): restart the server and search again.")
}
