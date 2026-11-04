package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"time"

	"github.com/christianjombo/go-dbmcp/internal/dbconn"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyCredentials
	ctxKeyCredentialsErr
)

// RequestIDFrom returns the correlation id assigned to this request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// CredentialsFrom returns the validated tenant credentials, if the request
// passed through RequireCredentials.
func CredentialsFrom(ctx context.Context) (*dbconn.Credentials, bool) {
	c, ok := ctx.Value(ctxKeyCredentials).(*dbconn.Credentials)
	return c, ok
}

// RequestID assigns a correlation id, preferring one supplied upstream so
// traces survive a reverse proxy hop.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 128 {
			var b [8]byte
			if _, err := rand.Read(b[:]); err != nil {
				// crypto/rand failing is not a reason to drop a request;
				// fall back to a timestamp-derived id.
				id = "ts-" + time.Now().UTC().Format("20060102T150405.000000000")
			} else {
				id = hex.EncodeToString(b[:])
			}
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// Recover converts a panic in any handler into a 500 and keeps the process
// alive. A hosted multi-tenant proxy must never let one tenant's pathological
// input take down every other tenant's stream.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// A client disconnecting mid-write is normal, not a fault.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.Error("panic recovered in handler",
					"requestId", RequestIDFrom(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()))
				writeJSONError(w, http.StatusInternalServerError, errorBody{
					Error:   "internal_error",
					Message: "The server encountered an unexpected condition and could not complete the request.",
				})
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog records one structured line per completed request. For an SSE
// stream that line is emitted when the stream ends, which makes stream
// duration directly visible.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			log.Info("http request",
				"requestId", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", redactQuery(r.URL),
				"status", rec.status,
				"bytes", rec.written,
				"durationMs", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
				"userAgent", r.UserAgent())
		})
	}
}

// redactQuery renders a URL for logging with any credential-bearing query
// parameter masked.
//
// Connection details may be supplied in the query string for clients that
// cannot set headers, and some of those clients will put the password there
// too. Logging the raw request line would then write tenant passwords to disk
// on every request, so the value is replaced before it reaches the log.
func redactQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return u.Path
	}
	q := u.Query()
	redacted := false
	for _, name := range []string{"password", "pass", "pwd", "db_password", "dsn", "url", "uri", "connection_string"} {
		if q.Get(name) != "" {
			q.Set(name, "REDACTED")
			redacted = true
		}
	}
	if !redacted {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path + "?" + q.Encode()
}

// recorder captures status and size without breaking streaming.
//
// Implementing Flush is mandatory here: without it the SSE handler's flushes
// are swallowed, the first event never reaches the client, and the connection
// appears to hang. Unwrap keeps http.NewResponseController working for the
// same reason.
type recorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ResolveCredentials parses the x-db-* headers (or the consolidated
// connection-string header) and attaches the result to the request context.
//
// It never rejects the request. An MCP client must be able to complete the
// handshake and list tools before it has been configured — Claude's connector
// setup probes the endpoint with no credentials at all and treats any 4xx as
// "server unreachable", which made a credential check here look like a broken
// server. Missing or invalid credentials are therefore carried forward and
// reported on the first tool call, where the message can actually be read.
func ResolveCredentials(log *slog.Logger, defaults dbconn.Defaults) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			creds, err := dbconn.FromRequest(r, defaults)
			ctx := r.Context()

			if err != nil {
				log.Debug("stream has no usable database credentials yet",
					"requestId", RequestIDFrom(ctx), "detail", err.Error())
				ctx = context.WithValue(ctx, ctxKeyCredentialsErr, err)
			} else {
				log.Info("stream credentials resolved",
					"requestId", RequestIDFrom(ctx), "db", creds)
				ctx = context.WithValue(ctx, ctxKeyCredentials, creds)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// CredentialsErrFrom returns why credential resolution failed, if it did.
func CredentialsErrFrom(ctx context.Context) error {
	err, _ := ctx.Value(ctxKeyCredentialsErr).(error)
	return err
}

// describeDefaultDatabase renders what a client will actually get if it omits
// x-db-database, which depends on the engine when no operator default is set.
func describeDefaultDatabase(d dbconn.Defaults) string {
	if d.Database != "" {
		return d.Database
	}
	if derived := d.Engine.DefaultDatabase(); derived != "" {
		return derived
	}
	return "(none selected — qualify table names or send x-db-database)"
}

type errorBody struct {
	Error       string            `json:"error"`
	Message     string            `json:"message"`
	Alternative string            `json:"single_header_alternative,omitempty"`
	Detail      string            `json:"detail,omitempty"`
	Missing     []string          `json:"missing_headers,omitempty"`
	Invalid     map[string]string `json:"invalid_headers,omitempty"`
	Required    []string          `json:"required_headers,omitempty"`
	Optional    map[string]string `json:"optional_headers_with_defaults,omitempty"`
}

func writeJSONError(w http.ResponseWriter, status int, body errorBody) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// The connection may already be gone; there is nothing useful to do with
	// a write error on an error path.
	_ = enc.Encode(body)
}
