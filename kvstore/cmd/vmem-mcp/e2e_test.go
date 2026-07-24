// E2E-тест адаптера сквозь весь стек (критерий шага 2 спринта 3,
// docs/VMEM_MCP_DESIGN.md): реальный kvstore-server + реальный vmem-mcp
// сабпроцессом, MCP-сессия по stdin/stdout — remember → supersedes → recall
// (валидное-сейчас и as_of) → изоляция scope → forget → server-down.
//
// Как и exec-тесты cmd/kvstore, гоняется локально полным `go test`
// (собирает бинарники — под -short скипается).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpSession — живой сабпроцесс адаптера и JSON-RPC поверх его пайпов.
type mcpSession struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	nextID int
}

func startMCP(t *testing.T, bin string, args ...string) *mcpSession {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		cmd.Wait()
	})
	sc := bufio.NewScanner(outPipe)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &mcpSession{t: t, cmd: cmd, in: in, out: sc}
}

// rpc шлёт запрос и читает ровно один ответ (нотификации ответа не имеют).
func (s *mcpSession) rpc(method string, params any) map[string]any {
	s.t.Helper()
	s.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("write %s: %v", method, err)
	}
	if !s.out.Scan() {
		s.t.Fatalf("no response to %s: %v", method, s.out.Err())
	}
	var resp map[string]any
	if err := json.Unmarshal(s.out.Bytes(), &resp); err != nil {
		s.t.Fatalf("bad response %q: %v", s.out.Text(), err)
	}
	if e, ok := resp["error"]; ok {
		s.t.Fatalf("%s: protocol error %v", method, e)
	}
	return resp
}

func (s *mcpSession) notify(method string) {
	s.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

// call — tools/call; возвращает text-содержимое и isError.
func (s *mcpSession) call(tool string, args map[string]any) (string, bool) {
	s.t.Helper()
	resp := s.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	res := resp["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

// callOK — tools/call, падает на isError, декодирует JSON-результат.
func (s *mcpSession) callOK(tool string, args map[string]any, into any) {
	s.t.Helper()
	text, isErr := s.call(tool, args)
	if isErr {
		s.t.Fatalf("%s(%v): isError: %s", tool, args, text)
	}
	if err := json.Unmarshal([]byte(text), into); err != nil {
		s.t.Fatalf("%s: bad payload %q: %v", tool, text, err)
	}
}

type recallResult struct {
	Facts []struct {
		ID    string  `json:"id"`
		Score float64 `json:"score"`
		Text  string  `json:"text"`
	} `json:"facts"`
}

func (r *recallResult) ids() []string {
	out := make([]string, len(r.Facts))
	for i, f := range r.Facts {
		out[i] = f.ID
	}
	return out
}

func (r *recallResult) has(id string) bool {
	for _, f := range r.Facts {
		if f.ID == id {
			return true
		}
	}
	return false
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func buildBins(t *testing.T) (serverBin, mcpBin string) {
	t.Helper()
	dir := t.TempDir()
	serverBin = filepath.Join(dir, "kvstore-server")
	mcpBin = filepath.Join(dir, "vmem-mcp")
	for bin, pkg := range map[string]string{
		serverBin: "kvstore/kvstore/cmd/kvstore",
		mcpBin:    "kvstore/kvstore/cmd/vmem-mcp",
	} {
		out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, out)
		}
	}
	return serverBin, mcpBin
}

func startServer(t *testing.T, bin string, port int) {
	t.Helper()
	cmd := exec.Command(bin, "-port", fmt.Sprint(port), "-metrics-port", "0")
	cmd.Dir = t.TempDir()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("server did not come up in 15s")
}

func TestMCPEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and spawns real binaries; skipped in -short")
	}
	serverBin, mcpBin := buildBins(t)
	port := freePort(t)
	startServer(t, serverBin, port)

	s := startMCP(t, mcpBin, "-addr", fmt.Sprintf("127.0.0.1:%d", port), "-default-scope", "e2e")

	// Handshake.
	init := s.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": "e2e-test", "version": "0"},
	})
	if pv := init["result"].(map[string]any)["protocolVersion"]; pv != "2025-06-18" {
		t.Fatalf("protocolVersion: %v", pv)
	}
	s.notify("notifications/initialized")
	if tools := s.rpc("tools/list", nil)["result"].(map[string]any)["tools"].([]any); len(tools) != 3 {
		t.Fatalf("want 3 tools, got %d", len(tools))
	}

	// v1 → пауза (гранулярность valid_from = секунды) → v2 supersedes v1.
	var rem struct {
		ID string `json:"id"`
	}
	s.callOK("memory_remember", map[string]any{
		"text": "deploy target is the staging cluster", "type": "decision"}, &rem)
	v1 := rem.ID
	if v1 == "" {
		t.Fatal("empty id from remember")
	}
	betweenTS := time.Now().Unix() + 1
	time.Sleep(2200 * time.Millisecond)
	s.callOK("memory_remember", map[string]any{
		"text": "deploy target is the production cluster", "type": "decision", "supersedes": v1}, &rem)
	v2 := rem.ID

	// Валидное-сейчас: наследник с дословным якорем, закрытый v1 скрыт.
	var got recallResult
	s.callOK("memory_recall", map[string]any{"query": "deploy target cluster"}, &got)
	if !got.has(v2) {
		t.Fatalf("recall must return successor %s, got %v", v2, got.ids())
	}
	if got.has(v1) {
		t.Fatalf("superseded fact %s leaked into valid-now recall: %v", v1, got.ids())
	}
	for _, f := range got.Facts {
		if f.ID == v2 && !strings.Contains(f.Text, "production") {
			t.Fatalf("anchor text lost: %+v", f)
		}
	}

	// Машина времени: as_of между версиями видит v1, а не v2.
	s.callOK("memory_recall", map[string]any{"query": "deploy target cluster", "as_of": betweenTS}, &got)
	if !got.has(v1) || got.has(v2) {
		t.Fatalf("as_of=%d: want v1 only, got %v", betweenTS, got.ids())
	}

	// Изоляция scope: чужой scope не видит и не может стереть.
	s.callOK("memory_remember", map[string]any{
		"text": "other scope secret fact", "scope": "tenant-b"}, &rem)
	s.callOK("memory_recall", map[string]any{"query": "secret fact"}, &got)
	if len(got.Facts) != 0 {
		t.Fatalf("scope leak: %v", got.ids())
	}
	if text, isErr := s.call("memory_forget", map[string]any{"id": rem.ID}); !isErr {
		t.Fatalf("cross-scope forget must be an error, got %s", text)
	}

	// Erasure: forget наследника — исчез отовсюду, включая as_of/ALL; повтор
	// идемпотентен (erased=false).
	var fg struct {
		Erased bool `json:"erased"`
	}
	s.callOK("memory_forget", map[string]any{"id": v2}, &fg)
	if !fg.Erased {
		t.Fatal("forget: want erased=true")
	}
	s.callOK("memory_recall", map[string]any{"query": "deploy target cluster", "all": true}, &got)
	if got.has(v2) {
		t.Fatalf("erased fact visible under ALL: %v", got.ids())
	}
	s.callOK("memory_forget", map[string]any{"id": v2}, &fg)
	if fg.Erased {
		t.Fatal("second forget: want erased=false (idempotent)")
	}
}

// Сервер лежит → isError с подсказкой запуска, протокол при этом жив.
func TestMCPServerDown(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real binaries; skipped in -short")
	}
	_, mcpBin := buildBins(t)
	s := startMCP(t, mcpBin, "-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)))
	s.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	s.notify("notifications/initialized")

	text, isErr := s.call("memory_recall", map[string]any{"query": "anything"})
	if !isErr || !strings.Contains(text, "unreachable") {
		t.Fatalf("want isError with start hint, got isErr=%v %q", isErr, text)
	}
	// Протокол жив после транспортной ошибки.
	if _, ok := s.rpc("ping", nil)["result"]; !ok {
		t.Fatal("ping after transport error failed")
	}
}
