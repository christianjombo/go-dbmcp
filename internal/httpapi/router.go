package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/christianjombo/go-dbmcp/internal/config"
	"github.com/christianjombo/go-dbmcp/internal/dbconn"
	"github.com/christianjombo/go-dbmcp/internal/guard"
	"github.com/christianjombo/go-dbmcp/internal/mcpsrv"
)

// ConnectPath is the MCP SSE endpoint. The same path serves both halves of
// the 2024-11-05 HTTP+SSE transport: GET opens the event stream and creates a
// session, and the client POSTs its messages back to the same path carrying
// the ?sessionid= the server issued in the opening `endpoint` event.
const ConnectPath = "/v1/mcp/connect"

// Deps are the collaborators the HTTP layer needs.
type Deps struct {
	Cfg     *config.Config
	Log     *slog.Logger
	Scanner *guard.Scanner
}

// Server wraps the router with the runtime counters exposed on /healthz.
type Server struct {
	Handler http.Handler

	startedAt     time.Time
	activeStreams atomic.Int64
	totalStreams  atomic.Int64
}

// New builds the HTTP surface.
func New(d Deps) *Server {
	s := &Server{startedAt: time.Now()}

	sessionCfg := dbconn.SessionConfig{
		ConnectTimeout:    d.Cfg.ConnectTimeout,
		MaxConns:          d.Cfg.PoolMaxConns,
		MaxConnIdleTime:   d.Cfg.PoolMaxConnIdleTime,
		MaxConnLifetime:   d.Cfg.PoolMaxConnLifetime,
		BlockPrivateHosts: d.Cfg.BlockPrivateHosts,
		PoolCloseGrace:    5 * time.Second,
	}

	defaults := dbconn.Defaults{
		Database:          d.Cfg.DefaultDatabase,
		SSLMode:           d.Cfg.DefaultSSLMode,
		BlockPrivateHosts: d.Cfg.BlockPrivateHosts,
	}

	// getServer runs once per opened SSE stream. Everything a tenant touches
	// — credentials, pool, MCP server — is created here and dies with the
	// request context, which is what makes the proxy stateless.
	sse := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
		creds, ok := CredentialsFrom(r.Context())
		if !ok {
			// Unreachable in practice: RequireCredentials gates every GET.
			// Returning nil makes the SDK answer 400 rather than panic.
			d.Log.Error("stream reached transport without credentials",
				"requestId", RequestIDFrom(r.Context()))
			return nil
		}

		// The session binds the pool to this stream's context. When the AI
		// client drops the connection, r.Context() fires and the session
		// cancels in-flight queries and closes the pool — no leaked backends
		// on the host.
		session := dbconn.NewSession(r.Context(), creds, sessionCfg, d.Log)

		s.activeStreams.Add(1)
		s.totalStreams.Add(1)
		context.AfterFunc(r.Context(), func() {
			s.activeStreams.Add(-1)
			d.Log.Info("stream closed",
				"requestId", RequestIDFrom(r.Context()),
				"fingerprint", creds.Fingerprint(),
				"activeStreams", s.activeStreams.Load())
		})

		d.Log.Info("stream opened",
			"requestId", RequestIDFrom(r.Context()),
			"fingerprint", creds.Fingerprint(),
			"activeStreams", s.activeStreams.Load())

		return mcpsrv.New(mcpsrv.Deps{Cfg: d.Cfg, Log: d.Log, Scanner: d.Scanner}, session)
	}, &mcp.SSEOptions{
		DisableLocalhostProtection: d.Cfg.DisableLocalhostProtection,
	})

	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(Recover(d.Log))
	r.Use(AccessLog(d.Log))

	r.Get("/healthz", s.health(d.Cfg, true))
	r.Get("/readyz", s.health(d.Cfg, false))
	r.Get("/", s.describe(d.Cfg))

	r.Group(func(r chi.Router) {
		r.Use(RequireCredentials(d.Log, defaults))
		r.Handle(ConnectPath, sse)
	})

	s.Handler = r
	return s
}

// ActiveStreams reports how many SSE streams are currently open. Shutdown
// uses it to report what it is waiting on.
func (s *Server) ActiveStreams() int64 { return s.activeStreams.Load() }

func (s *Server) health(cfg *config.Config, _ bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Deliberately does not touch any database. This process holds no
		// database of its own, and a tenant's unreachable server must never
		// make the proxy look unhealthy to its load balancer.
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "ok",
			"version":       cfg.Version,
			"uptimeSeconds": int64(time.Since(s.startedAt).Seconds()),
			"activeStreams": s.activeStreams.Load(),
			"totalStreams":  s.totalStreams.Load(),
		})
	}
}

func (s *Server) describe(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service":   "mcp-proxy",
			"version":   cfg.Version,
			"transport": "mcp/sse (2024-11-05)",
			"endpoint":  ConnectPath,
			"headers": map[string]any{
				"required": []string{"x-db-username", "x-db-password", "x-db-host", "x-db-port"},
				"optional": map[string]string{
					"x-db-database": cfg.DefaultDatabase,
					"x-db-sslmode":  cfg.DefaultSSLMode,
				},
			},
			"serialization": map[string]any{
				"jsonMaxRows": cfg.JSONMaxRows,
				"csvMaxRows":  cfg.CSVMaxRows,
			},
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
