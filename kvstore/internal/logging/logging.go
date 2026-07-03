// Package logging centralizes structured logging setup for the server.
//
// It wraps the standard library log/slog (Go 1.21+) — no external
// dependencies — so that log level and output format are configured once,
// in main(), and every package logs through the slog default logger via the
// package-level slog.Info/Warn/Error helpers.
//
// Convention across the codebase:
//   - Info  — normal lifecycle events (startup, shutdown, snapshot, load).
//   - Warn  — recoverable degradation (crash recovery, fallback, slow peer).
//   - Error — real failures the operator should see (I/O errors, panics).
//   - Fatalf — unrecoverable startup failure; logs at Error and exits(1).
//
// The old inline "WARNING:"/"ERROR:" prefixes are lifted into real slog
// levels so operators can filter (level threshold) and ship structured
// fields to an aggregator (JSON format).
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Setup configures the process-wide slog default logger and returns the
// resolved level.
//
// level:  "debug" | "info" | "warn" | "error" (case-insensitive; default info).
// format: "text" (human-readable, default) | "json" (for log aggregators).
func Setup(level, format string) slog.Level {
	lvl := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	return lvl
}

// Fatalf logs at Error level and terminates the process with status 1.
// Use ONLY for unrecoverable initialization failures (the former
// log.Fatalf call sites) — never on a request path.
func Fatalf(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
