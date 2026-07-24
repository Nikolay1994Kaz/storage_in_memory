// Package vmemmcp — тонкий MCP-адаптер (stdio JSON-RPC 2.0) поверх замороженной
// RESP-поверхности VMEM.* (спринт 3, docs/VMEM_MCP_DESIGN.md).
//
// Принципы:
//   - адаптер добавляет имена и типы, но НЕ семантику: один tool-вызов =
//     ровно одна RESP-команда против работающего kvstore-server;
//   - сторонних зависимостей нет — протокол = newline-delimited JSON-RPC 2.0,
//     encoding/json достаточно;
//   - семантический отказ сервера (-ERR …) = isError-результат tool'а, а не
//     JSON-RPC-ошибка: для хоста это ответ, а не сбой транспорта;
//   - stdout принадлежит протоколу, логи — только в stderr.
package vmemmcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"kvstore/kvstore/internal/protocol"
)

// Version — версия адаптера в serverInfo (не версия движка).
const Version = "0.1.0"

// поддерживаемые ревизии MCP: клиентскую эхоим, незнакомую поднимаем до latest.
var knownProtocolVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

const latestProtocolVersion = "2025-06-18"

// Backend — одна RESP-операция. В проде *RESPClient, в тестах — стаб.
// Семантический отказ сервера возвращается как ошибка типа RespError.
type Backend interface {
	Do(args []protocol.Value) (protocol.Value, error)
}

// RespError — '-ERR …' от сервера: отказ по контракту, не сбой транспорта.
type RespError string

func (e RespError) Error() string { return string(e) }

// Config — параметры адаптера.
type Config struct {
	// DefaultScope подставляется во все tool-вызовы без явного scope —
	// идентичность агента, настраивается один раз при установке.
	DefaultScope string
	// StartHint — как поднять сервер; уходит в isError при недоступности.
	StartHint string
}

type rpcRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Run — главный цикл: читает JSON-RPC построчно из r, пишет ответы в w.
// Возвращается на EOF (хост закрыл stdin) или на ошибке записи.
func Run(r io.Reader, w io.Writer, be Backend, cfg Config, log *slog.Logger) error {
	sc := bufio.NewScanner(r)
	// Факты бывают длинными (текст едет в аргументах) — поднимаем лимит строки.
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	out := bufio.NewWriter(w)
	srv := &server{be: be, cfg: cfg, out: out, log: log}

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			if err := srv.reply(rpcResponse{Jsonrpc: "2.0", ID: json.RawMessage("null"),
				Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}}); err != nil {
				return err
			}
			continue
		}
		if err := srv.handle(&req); err != nil {
			return err
		}
	}
	return sc.Err()
}

type server struct {
	be  Backend
	cfg Config
	out *bufio.Writer
	log *slog.Logger
}

func (s *server) reply(resp rpcResponse) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := s.out.Write(b); err != nil {
		return err
	}
	return s.out.Flush()
}

func (s *server) handle(req *rpcRequest) error {
	// Нотификация (без id) никогда не получает ответа — включая незнакомые.
	notification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		pv := p.ProtocolVersion
		if !knownProtocolVersions[pv] {
			pv = latestProtocolVersion
		}
		return s.reply(rpcResponse{Jsonrpc: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "vmem-mcp", "version": Version},
		}})
	case "ping":
		if notification {
			return nil
		}
		return s.reply(rpcResponse{Jsonrpc: "2.0", ID: req.ID, Result: map[string]any{}})
	case "tools/list":
		if notification {
			return nil
		}
		return s.reply(rpcResponse{Jsonrpc: "2.0", ID: req.ID, Result: map[string]any{"tools": toolList()}})
	case "tools/call":
		if notification {
			return nil
		}
		return s.reply(rpcResponse{Jsonrpc: "2.0", ID: req.ID, Result: s.call(req.Params)})
	default:
		if notification {
			s.log.Debug("notification ignored", "method", req.Method)
			return nil
		}
		return s.reply(rpcResponse{Jsonrpc: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
	}
}

// --- tools ---

// Описания tool'ов — промпт для агента: несут политику использования памяти
// (recall перед ответом, remember решений а не транскриптов, supersede вместо
// дубля), а не только сигнатуру.
func toolList() []map[string]any {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	num := func(desc string) map[string]any {
		return map[string]any{"type": "number", "description": desc}
	}
	return []map[string]any{
		{
			"name": "memory_remember",
			"description": "Store a durable memory fact that survives across sessions. " +
				"Use for stable facts, decisions and preferences worth keeping — not for transcripts or transient context. " +
				"If the fact REPLACES an older one (a preference changed, a config moved), pass the old fact's id in `supersedes` " +
				"instead of adding a duplicate: the old version stays queryable historically via memory_recall's as_of. " +
				"Returns the stored fact's id.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":        str("The fact itself, one self-contained statement."),
					"scope":       str("Memory namespace (defaults to the adapter's configured scope). Facts never leak between scopes."),
					"type":        str("Optional category tag, e.g. 'preference', 'decision', 'profile'. Filterable in recall."),
					"importance":  num("0..1, default 0.5. Biases recall ranking, never truth."),
					"ttl_seconds": num("Optional hard expiry: the fact is erased after this many seconds (right to be forgotten, not history)."),
					"supersedes":  str("Id of the fact this one replaces. Closes the old fact's validity interval instead of deleting it."),
				},
				"required": []string{"text"},
			},
		},
		{
			"name": "memory_recall",
			"description": "Search memory. Call this BEFORE answering questions about the user, prior decisions or project state. " +
				"Keyword (BM25) matching — use distinctive words from the expected fact, not full sentences. " +
				"Returns the top-k facts valid NOW, ranked by relevance × recency × importance. " +
				"Time travel: pass as_of (unix seconds) to ask what was true at a past moment (supersession is transparent, erased facts are not resurrected), " +
				"or all=true to ignore validity intervals entirely.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":             str("Search keywords."),
					"scope":             str("Memory namespace (defaults to the adapter's configured scope)."),
					"k":                 num("Max facts to return, default 5."),
					"as_of":             num("Unix seconds: recall what was true at that moment. Mutually exclusive with all."),
					"all":               map[string]any{"type": "boolean", "description": "Ignore validity intervals (full history). Mutually exclusive with as_of."},
					"type":              str("Only facts with this type tag."),
					"half_life_seconds": num("Recency decay half-life (default 30 days). Decay policy belongs to you, the client."),
				},
				"required": []string{"query"},
			},
		},
		{
			"name": "memory_forget",
			"description": "Permanently erase one fact by id — gone from history and as_of too (right to be forgotten). " +
				"NOT for updates: when a fact merely changed, use memory_remember with supersedes so history survives.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    str("Id of the fact to erase (as returned by memory_remember / memory_recall)."),
					"scope": str("Memory namespace the fact lives in (defaults to the adapter's configured scope)."),
				},
				"required": []string{"id"},
			},
		},
	}
}

// callResult — результат tools/call в терминах MCP.
type callResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

func textResult(v any) callResult {
	b, _ := json.Marshal(v)
	return callResult{Content: []map[string]any{{"type": "text", "text": string(b)}}}
}

func errResult(msg string) callResult {
	return callResult{Content: []map[string]any{{"type": "text", "text": msg}}, IsError: true}
}

func (s *server) call(params json.RawMessage) callResult {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errResult("bad tools/call params: " + err.Error())
	}
	switch p.Name {
	case "memory_remember":
		return s.remember(p.Arguments)
	case "memory_recall":
		return s.recall(p.Arguments)
	case "memory_forget":
		return s.forget(p.Arguments)
	default:
		return errResult("unknown tool: " + p.Name)
	}
}

func bulk(s string) protocol.Value { return protocol.Value{Typ: '$', Str: s} }

func (s *server) scope(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return s.cfg.DefaultScope
}

// do — RESP-обращение с переводом ошибок в isError: отказ сервера едет
// дословно, транспортный сбой — с подсказкой запуска.
func (s *server) do(args []protocol.Value) (protocol.Value, error) {
	v, err := s.be.Do(args)
	if err != nil {
		var re RespError
		if errors.As(err, &re) {
			return v, err
		}
		return v, fmt.Errorf("%s (%s)", s.cfg.StartHint, err)
	}
	return v, nil
}

func (s *server) remember(raw json.RawMessage) callResult {
	var a struct {
		Text       string   `json:"text"`
		Scope      string   `json:"scope"`
		Type       string   `json:"type"`
		Importance *float64 `json:"importance"`
		TTLSeconds *int64   `json:"ttl_seconds"`
		Supersedes string   `json:"supersedes"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("bad arguments: " + err.Error())
	}
	if a.Text == "" {
		return errResult("text is required")
	}
	args := []protocol.Value{bulk("VMEM.REMEMBER"), bulk(s.scope(a.Scope)), bulk("TEXT"), bulk(a.Text)}
	if a.Type != "" {
		args = append(args, bulk("TYPE"), bulk(a.Type))
	}
	if a.Importance != nil {
		args = append(args, bulk("IMPORTANCE"), bulk(strconv.FormatFloat(*a.Importance, 'f', -1, 64)))
	}
	if a.TTLSeconds != nil {
		args = append(args, bulk("TTL"), bulk(strconv.FormatInt(*a.TTLSeconds, 10)))
	}
	if a.Supersedes != "" {
		args = append(args, bulk("SUPERSEDES"), bulk(a.Supersedes))
	}
	v, err := s.do(args)
	if err != nil {
		return errResult(err.Error())
	}
	return textResult(map[string]any{"id": v.Str})
}

func (s *server) recall(raw json.RawMessage) callResult {
	var a struct {
		Query           string   `json:"query"`
		Scope           string   `json:"scope"`
		K               *int     `json:"k"`
		AsOf            *int64   `json:"as_of"`
		All             bool     `json:"all"`
		Type            string   `json:"type"`
		HalfLifeSeconds *float64 `json:"half_life_seconds"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("bad arguments: " + err.Error())
	}
	if a.Query == "" {
		return errResult("query is required")
	}
	if a.All && a.AsOf != nil {
		return errResult("as_of and all are mutually exclusive")
	}
	k := 5
	if a.K != nil {
		k = *a.K
	}
	args := []protocol.Value{bulk("VMEM.RECALL"), bulk(s.scope(a.Scope)), bulk(strconv.Itoa(k)), bulk(a.Query)}
	switch {
	case a.All:
		args = append(args, bulk("ALL"))
	case a.AsOf != nil:
		args = append(args, bulk("ASOF"), bulk(strconv.FormatInt(*a.AsOf, 10)))
	}
	if a.Type != "" {
		args = append(args, bulk("TYPE"), bulk(a.Type))
	}
	if a.HalfLifeSeconds != nil {
		args = append(args, bulk("HALFLIFE"), bulk(strconv.FormatFloat(*a.HalfLifeSeconds, 'f', -1, 64)))
	}
	v, err := s.do(args)
	if err != nil {
		return errResult(err.Error())
	}
	type fact struct {
		ID    string  `json:"id"`
		Score float64 `json:"score"`
		Text  string  `json:"text"`
	}
	facts := make([]fact, 0, len(v.Array)/3)
	for i := 0; i+2 < len(v.Array); i += 3 {
		score, _ := strconv.ParseFloat(v.Array[i+1].Str, 64)
		facts = append(facts, fact{ID: v.Array[i].Str, Score: score, Text: v.Array[i+2].Str})
	}
	return textResult(map[string]any{"facts": facts})
}

func (s *server) forget(raw json.RawMessage) callResult {
	var a struct {
		ID    string `json:"id"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("bad arguments: " + err.Error())
	}
	if a.ID == "" {
		return errResult("id is required")
	}
	v, err := s.do([]protocol.Value{bulk("VMEM.FORGET"), bulk(s.scope(a.Scope)), bulk(a.ID)})
	if err != nil {
		return errResult(err.Error())
	}
	return textResult(map[string]any{"erased": v.Num == 1})
}
