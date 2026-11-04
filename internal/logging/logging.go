// Package logging configures structured logging to stderr.
//
// Under the stdio transport, stdout *is* the JSON-RPC wire. A single stray
// write to it corrupts the stream and the client drops the session with an
// opaque parse error — historically the single most common way to break an
// MCP server. Setup redirects every logging path in the process to stderr and
// Guard replaces os.Stdout for the duration of initialisation so a chatty
// dependency fails loudly instead of silently.
package logging

import (
	"log"
	"log/slog"
	"os"
	"strings"
)

// Setup installs a JSON slog handler on stderr as the process default and
// points the standard library's log package at it too.
func Setup(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}

	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	l := slog.New(h)
	slog.SetDefault(l)

	// Anything using the stdlib logger (including some drivers) goes to stderr.
	log.SetOutput(os.Stderr)
	log.SetFlags(0)

	return l
}
