package vmemmcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"kvstore/kvstore/internal/protocol"
)

// stubBackend — скриптованный RESP-бэкенд: записывает команды, отдаёт
// заготовленные ответы по префиксу команды.
type stubBackend struct {
	got   [][]string
	reply map[string]protocol.Value
	err   error
}

func (b *stubBackend) Do(args []protocol.Value) (protocol.Value, error) {
	flat := make([]string, len(args))
	for i, a := range args {
		flat[i] = a.Str
	}
	b.got = append(b.got, flat)
	if b.err != nil {
		return protocol.Value{}, b.err
	}
	return b.reply[flat[0]], nil
}

// session прогоняет строки через Run и возвращает распарсенные ответы.
func session(t *testing.T, be Backend, lines ...string) []map[string]any {
	t.Helper()
	return sessionCfg(t, be, Config{DefaultScope: "agent", StartHint: "start the server"}, lines...)
}

// sessionCfg — session с явной конфигурацией адаптера (провенанс, scope).
func sessionCfg(t *testing.T, be Backend, cfg Config, lines ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := Run(in, &out, be, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var resps []map[string]any
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad response line %q: %v", sc.Text(), err)
		}
		resps = append(resps, m)
	}
	return resps
}

func rpc(id int, method, params string) string {
	if params == "" {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, id, method)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
}

func notif(method string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":%q}`, method)
}

// toolText достаёт text-содержимое tools/call-результата и флаг isError.
func toolText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

// Шаг 1, критерий: handshake + tools/list + незнакомый метод + нотификации.
func TestHandshakeAndList(t *testing.T) {
	resps := session(t, &stubBackend{},
		rpc(1, "initialize", `{"protocolVersion":"2025-03-26","clientInfo":{"name":"test"}}`),
		notif("notifications/initialized"),
		rpc(2, "tools/list", ""),
		rpc(3, "no/such/method", ""),
		notif("no/such/notification"), // нотификация не получает даже ошибки
		rpc(4, "ping", ""),
	)
	if len(resps) != 4 {
		t.Fatalf("want 4 responses (notifications unanswered), got %d: %v", len(resps), resps)
	}

	init := resps[0]["result"].(map[string]any)
	if pv := init["protocolVersion"]; pv != "2025-03-26" {
		t.Errorf("known client version must be echoed, got %v", pv)
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("tools capability missing: %v", init)
	}

	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("want 3 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		m := tl.(map[string]any)
		names[m["name"].(string)] = true
		if m["description"].(string) == "" || m["inputSchema"] == nil {
			t.Errorf("tool %v lacks description/schema", m["name"])
		}
	}
	for _, want := range []string{"memory_remember", "memory_recall", "memory_forget"} {
		if !names[want] {
			t.Errorf("tool %s missing", want)
		}
	}

	if code := resps[2]["error"].(map[string]any)["code"].(float64); code != -32601 {
		t.Errorf("unknown method: want -32601, got %v", code)
	}
	if _, ok := resps[3]["result"]; !ok {
		t.Errorf("ping must return a result: %v", resps[3])
	}
}

// Незнакомая ревизия протокола поднимается до latest, известная — эхо.
func TestProtocolVersionNegotiation(t *testing.T) {
	resps := session(t, &stubBackend{},
		rpc(1, "initialize", `{"protocolVersion":"2099-01-01"}`))
	if pv := resps[0]["result"].(map[string]any)["protocolVersion"]; pv != latestProtocolVersion {
		t.Errorf("unknown version: want %s, got %v", latestProtocolVersion, pv)
	}
}

// Битый JSON → -32700, цикл не падает и продолжает обслуживать.
func TestParseError(t *testing.T) {
	resps := session(t, &stubBackend{},
		`{this is not json`,
		rpc(1, "ping", ""),
	)
	if len(resps) != 2 {
		t.Fatalf("want 2 responses, got %d", len(resps))
	}
	if code := resps[0]["error"].(map[string]any)["code"].(float64); code != -32700 {
		t.Errorf("want -32700, got %v", code)
	}
}

// remember: маппинг аргументов в RESP 1:1 + дефолтный scope из конфига.
func TestRememberMapping(t *testing.T) {
	be := &stubBackend{reply: map[string]protocol.Value{
		"VMEM.REMEMBER": {Typ: '$', Str: "01ABC"},
	}}
	resps := session(t, be,
		rpc(1, "tools/call", `{"name":"memory_remember","arguments":{"text":"prefers Go","type":"preference","importance":0.9,"ttl_seconds":3600,"supersedes":"01OLD"}}`),
	)
	text, isErr := toolText(t, resps[0])
	if isErr {
		t.Fatalf("unexpected isError: %s", text)
	}
	if text != `{"id":"01ABC"}` {
		t.Errorf("bad result: %s", text)
	}
	want := []string{"VMEM.REMEMBER", "agent", "TEXT", "prefers Go",
		"TYPE", "preference", "IMPORTANCE", "0.9", "TTL", "3600", "SUPERSEDES", "01OLD"}
	if fmt.Sprint(be.got[0]) != fmt.Sprint(want) {
		t.Errorf("RESP args:\n got %v\nwant %v", be.got[0], want)
	}
}

// recall: триплеты → facts, явный scope побеждает дефолтный, as_of → ASOF.
func TestRecallMapping(t *testing.T) {
	be := &stubBackend{reply: map[string]protocol.Value{
		"VMEM.RECALL": {Typ: '*', Array: []protocol.Value{
			{Typ: '$', Str: "01A"}, {Typ: '$', Str: "0.42"}, {Typ: '$', Str: "fact one"},
			{Typ: '$', Str: "01B"}, {Typ: '$', Str: "0.17"}, {Typ: '$', Str: ""},
		}},
	}}
	resps := session(t, be,
		rpc(1, "tools/call", `{"name":"memory_recall","arguments":{"query":"prefers","scope":"other","k":2,"as_of":1700000000}}`),
	)
	text, isErr := toolText(t, resps[0])
	if isErr {
		t.Fatalf("unexpected isError: %s", text)
	}
	var got struct {
		Facts []struct {
			ID    string  `json:"id"`
			Score float64 `json:"score"`
			Text  string  `json:"text"`
		} `json:"facts"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 2 || got.Facts[0].ID != "01A" || got.Facts[0].Score != 0.42 ||
		got.Facts[0].Text != "fact one" || got.Facts[1].Text != "" {
		t.Errorf("bad facts: %+v", got.Facts)
	}
	want := []string{"VMEM.RECALL", "other", "2", "prefers", "ASOF", "1700000000"}
	if fmt.Sprint(be.got[0]) != fmt.Sprint(want) {
		t.Errorf("RESP args:\n got %v\nwant %v", be.got[0], want)
	}
}

// forget: :1/:0 → erased true/false.
func TestForgetMapping(t *testing.T) {
	be := &stubBackend{reply: map[string]protocol.Value{
		"VMEM.FORGET": {Typ: ':', Num: 1},
	}}
	resps := session(t, be,
		rpc(1, "tools/call", `{"name":"memory_forget","arguments":{"id":"01A"}}`),
	)
	text, _ := toolText(t, resps[0])
	if text != `{"erased":true}` {
		t.Errorf("bad result: %s", text)
	}
	want := []string{"VMEM.FORGET", "agent", "01A"}
	if fmt.Sprint(be.got[0]) != fmt.Sprint(want) {
		t.Errorf("RESP args: got %v want %v", be.got[0], want)
	}
}

// Валидация аргументов и перевод ошибок: обязательные поля, взаимоисключение,
// отказ сервера дословно, транспортный сбой с подсказкой запуска.
func TestToolErrors(t *testing.T) {
	cases := []struct {
		name string
		be   *stubBackend
		call string
		want string
	}{
		{"missing text", &stubBackend{}, `{"name":"memory_remember","arguments":{}}`, "text is required"},
		{"missing query", &stubBackend{}, `{"name":"memory_recall","arguments":{}}`, "query is required"},
		{"missing id", &stubBackend{}, `{"name":"memory_forget","arguments":{}}`, "id is required"},
		{"asof+all", &stubBackend{}, `{"name":"memory_recall","arguments":{"query":"x","all":true,"as_of":5}}`, "mutually exclusive"},
		{"unknown tool", &stubBackend{}, `{"name":"memory_zap","arguments":{}}`, "unknown tool"},
		{"server refusal", &stubBackend{err: RespError("ERR vmem: supersedes target not found")},
			`{"name":"memory_forget","arguments":{"id":"01A"}}`, "supersedes target not found"},
		{"transport down", &stubBackend{err: errors.New("connection refused")},
			`{"name":"memory_recall","arguments":{"query":"x"}}`, "start the server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resps := session(t, tc.be, rpc(1, "tools/call", tc.call))
			text, isErr := toolText(t, resps[0])
			if !isErr {
				t.Fatalf("want isError, got %s", text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("error %q must contain %q", text, tc.want)
			}
		})
	}
}

// Провенанс адаптера: SOURCE берётся из конфигурации и НЕ управляется агентом.
// Оба свойства проверяются вместе, потому что смысл только в паре: источник
// проставлен, и подсунутый в аргументы вызова "source" его не подменил.
func TestRememberSourceFromConfigNotArguments(t *testing.T) {
	be := &stubBackend{reply: map[string]protocol.Value{
		"VMEM.REMEMBER": {Typ: '$', Str: "01ABC"},
	}}
	cfg := Config{DefaultScope: "agent", Source: "claude-code", StartHint: "start the server"}
	resps := sessionCfg(t, be, cfg,
		rpc(1, "tools/call", `{"name":"memory_remember","arguments":{"text":"prefers Go","source":"trusted-admin"}}`),
	)
	if text, isErr := toolText(t, resps[0]); isErr {
		t.Fatalf("unexpected isError: %s", text)
	}
	want := []string{"VMEM.REMEMBER", "agent", "TEXT", "prefers Go", "SOURCE", "claude-code"}
	if fmt.Sprint(be.got[0]) != fmt.Sprint(want) {
		t.Errorf("провенанс управляем агентом либо не проставлен:\n got %v\nwant %v", be.got[0], want)
	}
}

// Пустой Source в конфиге → SOURCE не передаётся, штамп "unknown" ставит сервер
// (адаптер не выдумывает происхождение за него).
func TestRememberNoSourceWhenUnconfigured(t *testing.T) {
	be := &stubBackend{reply: map[string]protocol.Value{
		"VMEM.REMEMBER": {Typ: '$', Str: "01ABC"},
	}}
	session(t, be, rpc(1, "tools/call", `{"name":"memory_remember","arguments":{"text":"prefers Go"}}`))
	for _, a := range be.got[0] {
		if a == "SOURCE" {
			t.Fatalf("SOURCE передан без настройки: %v", be.got[0])
		}
	}
}
