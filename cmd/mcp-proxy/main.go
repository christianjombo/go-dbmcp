// Command mcp-proxy is a stateless, multi-tenant Model Context Protocol
// server that proxies AI clients to third-party PostgreSQL databases.
//
// It holds no user database and no credential vault. Each client supplies its
// own target in x-db-* request headers, a connection pool is created for the
// life of that client's SSE stream, and it is torn down the moment the stream
// closes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/christianjombo/go-dbmcp/internal/config"
	"github.com/christianjombo/go-dbmcp/internal/guard"
	"github.com/christianjombo/go-dbmcp/internal/httpapi"
	"github.com/christianjombo/go-dbmcp/internal/logging"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Every failure path returns an error; nothing in this binary panics by
	// design, and handler panics are contained by the Recover middleware.
	if err := run(); err != nil {
		// The logger may not exist yet, so write plainly — to stderr.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("mcp-proxy %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	}

	cfg, warnings := config.Load(version)
	log := logging.Setup(cfg.LogLevel)

	for _, w := range warnings {
		log.Warn("configuration fallback applied", "detail", w)
	}

	scanner, guardWarnings := guard.New(cfg.ExtraBlockedPatterns, cfg.AllowWrites)
	for _, w := range guardWarnings {
		log.Warn("guardrail configuration problem", "detail", w)
	}

	api := httpapi.New(httpapi.Deps{Cfg: cfg, Log: log, Scanner: scanner})

	// Signals arrive here first; the base context below is cancelled only
	// after the graceful phase, so in-flight work gets a chance to finish.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	// Every request context descends from baseCtx. Cancelling it is what
	// terminates long-lived SSE streams, which http.Server.Shutdown cannot
	// do on its own: Shutdown waits for active connections, and an SSE
	// stream is active until the client goes away.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		// WriteTimeout is deliberately left at zero. It is an absolute
		// deadline measured from the start of the response, so any non-zero
		// value would sever every SSE stream at that age. Per-operation
		// bounds come from the query timeout and the request context instead.
		WriteTimeout: 0,
		BaseContext:  func(net.Listener) context.Context { return baseCtx },
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}

	log.Info("mcp-proxy starting", append([]any{"endpoint", httpapi.ConnectPath}, cfg.Redacted()...)...)
	if !cfg.DisableLocalhostProtection {
		log.Info("DNS-rebinding protection is enabled; if this server sits behind a reverse proxy " +
			"on the same host, set MCP_DISABLE_LOCALHOST_PROTECTION=true or every request will be rejected with 403")
	}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-signalCtx.Done():
	}

	// Stop intercepting signals so a second Ctrl-C / SIGTERM aborts hard.
	stopSignals()

	log.Info("shutdown requested", "activeStreams", api.ActiveStreams(), "grace", cfg.ShutdownGrace.String())
	return shutdown(srv, api, log, cfg.ShutdownGrace, cancelBase)
}

// shutdown runs a two-phase stop: first stop accepting and let work finish,
// then cancel stream contexts so SSE clients are released and their database
// pools closed, then hard-close anything still hanging on.
func shutdown(
	srv *http.Server,
	api *httpapi.Server,
	log *slog.Logger,
	grace time.Duration,
	cancelBase context.CancelFunc,
) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(shutdownCtx) }()

	// Phase 1: a short window for ordinary requests to drain on their own.
	drain := grace / 3
	if drain > 2*time.Second {
		drain = 2 * time.Second
	}
	select {
	case err := <-done:
		log.Info("shutdown complete")
		return err
	case <-time.After(drain):
	}

	// Phase 2: release the long-lived streams. Each session's context fires,
	// in-flight queries are cancelled and every tenant pool is closed.
	if n := api.ActiveStreams(); n > 0 {
		log.Info("cancelling active SSE streams and releasing database pools", "activeStreams", n)
	}
	cancelBase()

	select {
	case err := <-done:
		log.Info("shutdown complete", "activeStreams", api.ActiveStreams())
		return err
	case <-shutdownCtx.Done():
		log.Warn("graceful shutdown deadline exceeded; closing connections", "activeStreams", api.ActiveStreams())
		if err := srv.Close(); err != nil {
			return fmt.Errorf("force close: %w", err)
		}
		return nil
	}
}
