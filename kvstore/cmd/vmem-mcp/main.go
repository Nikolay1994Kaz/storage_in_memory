// vmem-mcp — MCP-сервер (stdio) поверх VMEM.* работающего kvstore-server.
//
// Тонкий сайдкар (docs/VMEM_MCP_DESIGN.md): хост агента (Claude Code /
// Claude Desktop / любой MCP-клиент) запускает этот бинарник сам и говорит
// с ним JSON-RPC по stdin/stdout; каждый tool-вызов = одна RESP-команда.
// Сервер адаптер НЕ запускает — источник памяти один, durability одна.
//
// Установка в Claude Code (см. docs/QUICKSTART_MCP.md):
//
//	claude mcp add memory -- vmem-mcp -addr 127.0.0.1:6380 -default-scope myproject
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"

	"kvstore/kvstore/internal/vmemmcp"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6380", "адрес kvstore-server (RESP)")
	auth := flag.String("auth", "", "пароль AUTH (если сервер поднят с -requirepass)")
	scope := flag.String("default-scope", "default", "scope по умолчанию — идентичность агента; tool-аргумент scope переопределяет")
	logLevel := flag.String("log-level", "info", "debug|info|warn|error (в stderr; stdout занят протоколом)")
	flag.Parse()

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "bad -log-level %q: %v\n", *logLevel, err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	hintPort := "6380"
	if _, p, err := net.SplitHostPort(*addr); err == nil {
		hintPort = p
	}
	cfg := vmemmcp.Config{
		DefaultScope: *scope,
		StartHint: fmt.Sprintf(
			"kvstore-server is unreachable at %s — start it (./kvstore-server --port %s, or: docker compose up -d) and retry",
			*addr, hintPort),
	}
	be := vmemmcp.NewRESPClient(*addr, *auth)

	log.Info("vmem-mcp up", "addr", *addr, "scope", *scope, "version", vmemmcp.Version)
	if err := vmemmcp.Run(os.Stdin, os.Stdout, be, cfg, log); err != nil {
		log.Error("session ended with error", "err", err)
		os.Exit(1)
	}
}
