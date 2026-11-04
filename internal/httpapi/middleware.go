package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/christianjombo/go-dbmcp/internal/dbconn"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyCredentials
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
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.written,
				"durationMs", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
				"userAgent", r.UserAgent())
		})
	}
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

// RequireCredentials parses and validates the x-db-* headers and attaches the
// result to the request context.
//
// It enforces only on GET, the request that opens an SSE stream and creates
// the session. The subsequent POSTs carry an unguessable sessionid issued by
// the server and are bound to a session that was already authorised; some MCP
// clients do not replay custom headers onto the message endpoint, so demanding
// them there would break those clients for no security gain.
func RequireCredentials(log *slog.Logger, defaults dbconn.Defaults) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}

			creds, err := dbconn.FromHeaders(r.Header, defaults)
			if err != nil {
				var ve *dbconn.ValidationError
				body := errorBody{
					Error: "invalid_db_headers",
					Message: "This proxy is stateless and takes the target database from request headers. " +
						"Fix the headers listed below in your MCP client configuration and reconnect.",
					Required: []string{"x-db-username", "x-db-password", "x-db-host", "x-db-port"},
					Optional: map[string]string{
						"x-db-database": defaults.Database,
						"x-db-sslmode":  defaults.SSLMode,
					},
				}
				if errors.As(err, &ve) {
					body.Missing = ve.Missing
					body.Invalid = ve.Invalid
				} else {
					body.Detail = err.Error()
				}

				log.Warn("rejected stream: invalid database headers",
					"requestId", RequestIDFrom(r.Context()),
					"missing", body.Missing,
					"invalid", body.Invalid)

				writeJSONError(w, http.StatusBadRequest, body)
				return
			}

			log.Info("stream authorised",
				"requestId", RequestIDFrom(r.Context()),
				"db", creds)

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyCredentials, creds)))
		})
	}
}

type errorBody struct {
	Error    string            `json:"error"`
	Message  string            `json:"message"`
	Detail   string            `json:"detail,omitempty"`
	Missing  []string          `json:"missing_headers,omitempty"`
	Invalid  map[string]string `json:"invalid_headers,omitempty"`
	Required []string          `json:"required_headers,omitempty"`
	Optional map[string]string `json:"optional_headers_with_defaults,omitempty"`
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
