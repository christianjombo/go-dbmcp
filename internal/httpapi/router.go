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
	"github.com/christianjombo/go-dbmcp/internal/synthetic"
)

// MCP endpoints.
//
// ConnectPath serves BOTH transports. Claude's custom connector speaks
// Streamable HTTP (the current spec) and probes a new connector by POSTing an
// `initialize` request; the legacy 2024-11-05 SSE handler answers that with
// "sessionid must be provided", which the connector reports as an unreachable
// server. Dispatching on the shape of the request means an existing
// configuration keeps working without anyone re-entering a different URL.
const (
	ConnectPath    = "/v1/mcp/connect"
	StreamablePath = "/v1/mcp"
	ShortPath      = "/mcp"
)

// Deps are the collaborators the HTTP layer needs.
type Deps struct {
	Cfg       *config.Config
	Log       *slog.Logger
	Scanner   *guard.Scanner
	Synthetic *synthetic.Interceptor
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
		IdleTimeout:       d.Cfg.SessionIdleTimeout,
	}

	defaultEngine, err := dbconn.ParseEngine(d.Cfg.DefaultDBType)
	if err != nil {
		// Config validation already clamps this to a known value; belt and
		// braces so a future edit cannot leave the engine empty.
		defaultEngine = dbconn.EngineMySQL
	}

	defaults := dbconn.Defaults{
		Engine:            defaultEngine,
		Database:          d.Cfg.DefaultDatabase,
		SSLMode:           d.Cfg.DefaultSSLMode,
		BlockPrivateHosts: d.Cfg.BlockPrivateHosts,
	}

	// getServer runs once per MCP session. Everything a tenant touches —
	// credentials, pool, MCP server — is created here and dies with the
	// request context, which is what makes the proxy stateless.
	//
	// A session is created even when no credentials were supplied, so the
	// handshake and tools/list still succeed. A client must be able to
	// connect and discover what a server offers before it is configured;
	// Claude's connector probe does exactly that, and treats any 4xx as an
	// unreachable server. Tool calls then report precisely what is missing.
	getServer := func(r *http.Request) *mcp.Server {
		var session *dbconn.Session

		// Choose the right lifetime for the transport in use. A legacy SSE
		// session lives exactly as long as its stream, so the request context
		// is the teardown signal the spec asks for. A Streamable HTTP session
		// outlives the POST that created it, so it is detached and reclaimed
		// by the idle timer instead.
		lifetime := r.Context()
		if !isLegacySSE(r) {
			lifetime = context.WithoutCancel(r.Context())
		}

		if creds, ok := CredentialsFrom(r.Context()); ok {
			session = dbconn.NewSession(lifetime, creds, sessionCfg, d.Log)
			d.Log.Info("mcp session opened",
				"requestId", RequestIDFrom(r.Context()),
				"transport", transportName(r),
				"fingerprint", creds.Fingerprint(),
				"activeStreams", s.activeStreams.Add(1))
		} else {
			session = dbconn.NewUnconfiguredSession(lifetime,
				CredentialsErrFrom(r.Context()), sessionCfg, d.Log)
			d.Log.Info("mcp session opened without credentials",
				"requestId", RequestIDFrom(r.Context()),
				"transport", transportName(r),
				"activeStreams", s.activeStreams.Add(1))
		}

		s.totalStreams.Add(1)
		context.AfterFunc(r.Context(), func() {
			d.Log.Info("mcp session closed",
				"requestId", RequestIDFrom(r.Context()),
				"activeStreams", s.activeStreams.Add(-1))
		})

		return mcpsrv.New(mcpsrv.Deps{Cfg: d.Cfg, Log: d.Log, Scanner: d.Scanner,
			Synthetic: d.Synthetic}, session)
	}

	sse := mcp.NewSSEHandler(getServer, &mcp.SSEOptions{
		DisableLocalhostProtection: d.Cfg.DisableLocalhostProtection,
	})
	streamable := mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{
		DisableLocalhostProtection: d.Cfg.DisableLocalhostProtection,
	})

	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(Recover(d.Log))
	r.Use(AccessLog(d.Log))

	r.Get("/healthz", s.health(d.Cfg))
	r.Get("/readyz", s.health(d.Cfg))
	r.Get("/", s.describe(d.Cfg, defaults))

	// No authentication is required, so OAuth discovery must say so plainly.
	// A 404 here is the spec's way of reporting "not an OAuth protected
	// resource"; combined with never answering 401, it lets a client conclude
	// that no sign-in is needed. Serving an explicit body makes that a
	// decision rather than a routing gap.
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/*",
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-authorization-server/*",
		"/.well-known/openid-configuration",
	} {
		r.Handle(path, noAuthRequired())
	}

	r.Group(func(r chi.Router) {
		r.Use(ResolveCredentials(d.Log, defaults))
		r.Handle(ConnectPath, dualTransport(sse, streamable))
		r.Handle(StreamablePath, streamable)
		r.Handle(ShortPath, streamable)
	})

	s.Handler = r
	return s
}

// ActiveStreams reports how many SSE streams are currently open. Shutdown
// uses it to report what it is waiting on.
func (s *Server) ActiveStreams() int64 { return s.activeStreams.Load() }

func (s *Server) health(cfg *config.Config) http.HandlerFunc {
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

func (s *Server) describe(cfg *config.Config, defaults dbconn.Defaults) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service":        "mcp-proxy",
			"version":        cfg.Version,
			"authentication": "none — no sign-in and no OAuth; database credentials travel per request",
			"endpoints": map[string]string{
				"streamableHttp": StreamablePath + "  (also " + ShortPath + " and " + ConnectPath + ")",
				"sse":            ConnectPath + "  (legacy 2024-11-05 transport)",
			},
			"credentials": map[string]any{
				"urlParameters": "?type=mysql&host=db.example.com&port=3306&db=appdb&user=app_reader " +
					"(do not put the password here — connector URLs are shown in plaintext and logged)",
				"secretHeader": "x-api-key: <password>   (recommended; the value is the bare password, " +
					"no Bearer prefix). The same header also accepts a complete DSN, plain or base64.",
				"splitHeaders": "x-tenant-id: mysql://user@host:3306/db   +   x-api-key: <password>",
				"granular": map[string]any{
					"required": []string{"x-db-username", "x-db-password", "x-db-host", "x-db-port"},
					"optional": map[string]string{
						"x-db-type":     string(defaults.Engine),
						"x-db-database": describeDefaultDatabase(defaults),
						"x-db-sslmode":  defaults.SSLMode,
					},
				},
				"precedence": "url parameters < consolidated headers < x-db-* headers",
			},
			"serialization": map[string]any{
				"jsonMaxRows": cfg.JSONMaxRows,
				"csvMaxRows":  cfg.CSVMaxRows,
			},
		})
	}
}

// dualTransport routes one path to whichever MCP transport the client speaks.
//
// The two are distinguishable in practice: legacy SSE opens with a plain GET
// and tags every later message with ?sessionid=, while Streamable HTTP POSTs
// JSON-RPC with no sessionid and carries its session in Mcp-Session-Id.
func dualTransport(sse, streamable http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLegacySSE(r) {
			sse.ServeHTTP(w, r)
			return
		}
		streamable.ServeHTTP(w, r)
	})
}

func isLegacySSE(r *http.Request) bool {
	if r.URL.Query().Get("sessionid") != "" {
		return true // legacy message endpoint
	}
	if r.Header.Get("Mcp-Session-Id") != "" {
		return false // an established Streamable HTTP session
	}
	// A bare GET opens a legacy SSE stream; Streamable HTTP clients start
	// with a POST.
	return r.Method == http.MethodGet
}

func transportName(r *http.Request) string {
	if isLegacySSE(r) {
		return "sse"
	}
	return "streamable-http"
}

// noAuthRequired answers OAuth discovery with an explicit "nothing here".
func noAuthRequired() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no_authentication_required",
			"detail": "This MCP server uses no OAuth and requires no sign-in. Database " +
				"credentials are supplied per request; see / for the contract.",
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
