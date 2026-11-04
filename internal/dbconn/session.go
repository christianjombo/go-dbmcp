package dbconn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// ErrSessionClosed is returned once the owning SSE stream has gone away.
var ErrSessionClosed = errors.New("session closed: the MCP stream was disconnected")

// SessionConfig is the pool policy applied to every tenant connection.
type SessionConfig struct {
	ConnectTimeout    time.Duration
	MaxConns          int32
	MaxConnIdleTime   time.Duration
	MaxConnLifetime   time.Duration
	BlockPrivateHosts bool
	// PoolCloseGrace bounds how long Close waits for pgx to drain before it
	// gives up and logs. It never blocks shutdown indefinitely.
	PoolCloseGrace time.Duration
	// IdleTimeout closes a session that has gone unused for this long.
	//
	// It exists because the two MCP transports have different lifetimes. The
	// legacy SSE transport holds one long-lived GET, so the request context
	// spans the whole session and cancelling it is the right teardown signal.
	// Streamable HTTP has no such stream: initialize and every tools/call are
	// separate POSTs, and binding the pool to any one of them would release
	// it the instant that request finished. For those sessions the pool is
	// detached from the request and reclaimed on inactivity instead.
	IdleTimeout time.Duration
}

// Session owns exactly one tenant's connection pool for exactly the lifetime
// of one MCP stream.
//
// Two deliberate choices:
//
//   - The pool is created lazily, on first query. A hosted proxy accumulates
//     idle streams from clients that connect and then sit silent; opening a
//     TCP connection and a PostgreSQL backend for each of those would burn
//     the target database's connection budget for nothing.
//   - The session carries its own context, derived from the HTTP request.
//     Closing cancels it first and only then closes the pool, so in-flight
//     queries unwind instead of making pgxpool.Close block on a busy
//     connection.
type Session struct {
	creds *Credentials
	// credsErr is set when the request carried no usable credentials. The
	// session still exists so the MCP handshake and tools/list succeed —
	// a client must be able to discover what a server offers before it has
	// been configured — and the error surfaces on the first tool call.
	credsErr error
	cfg      SessionConfig
	log      *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	pool      Pool
	closed    bool
	idleTimer *time.Timer
}

// NewSession binds a tenant's credentials to the lifetime of parent, which is
// expected to be the SSE request context. When the AI client drops the stream
// the context fires and Close tears the pool down.
func NewSession(parent context.Context, creds *Credentials, cfg SessionConfig, log *slog.Logger) *Session {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	if creds != nil {
		log = log.With("fingerprint", creds.Fingerprint(), "target", creds.Target())
	}
	s := &Session{
		creds:  creds,
		cfg:    cfg,
		log:    log,
		ctx:    ctx,
		cancel: cancel,
	}
	// Detach from the parent's cancellation above, then re-attach explicitly
	// via Close so that teardown always runs through one path.
	context.AfterFunc(parent, func() {
		s.log.Debug("stream context done; releasing database pool")
		s.Close()
	})
	s.startIdleTimer()
	return s
}

// startIdleTimer arms inactivity-based reclamation, when configured.
func (s *Session) startIdleTimer() {
	if s.cfg.IdleTimeout <= 0 {
		return
	}
	s.idleTimer = time.AfterFunc(s.cfg.IdleTimeout, func() {
		s.log.Info("session idle; releasing database pool", "idleTimeout", s.cfg.IdleTimeout.String())
		s.Close()
	})
}

// touch pushes the idle deadline out. Called on every use.
func (s *Session) touch() {
	if s.idleTimer != nil {
		s.idleTimer.Reset(s.cfg.IdleTimeout)
	}
}

// NewUnconfiguredSession represents a stream whose request carried no usable
// credentials. The MCP handshake still completes, so a client can connect and
// list tools; calling one reports credsErr.
func NewUnconfiguredSession(parent context.Context, credsErr error, cfg SessionConfig, log *slog.Logger) *Session {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s := &Session{credsErr: credsErr, cfg: cfg, log: log, ctx: ctx, cancel: cancel}
	context.AfterFunc(parent, func() { s.Close() })
	s.startIdleTimer()
	return s
}

// Configured reports whether this session has usable database credentials.
func (s *Session) Configured() bool { return s.creds != nil && s.credsErr == nil }

// ConfigError explains why the session has no credentials, or nil.
func (s *Session) ConfigError() error { return s.credsErr }

// Acquire returns the pool, creating and verifying it on first use.
func (s *Session) Acquire(ctx context.Context) (Pool, error) {
	if !s.Configured() {
		if s.credsErr != nil {
			return nil, s.credsErr
		}
		return nil, errors.New("this stream has no database credentials")
	}
	s.touch()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrSessionClosed
	}
	if s.pool != nil {
		p := s.pool
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()

	// Build outside the lock: dialling can take as long as ConnectTimeout and
	// must not stall a concurrent Close.
	pool, err := s.build(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		go pool.Close()
		return nil, ErrSessionClosed
	}
	if s.pool != nil {
		// Lost a benign race with another tool call; keep the winner.
		go pool.Close()
		return s.pool, nil
	}
	s.pool = pool
	return pool, nil
}

func (s *Session) build(ctx context.Context) (Pool, error) {
	dialCtx, cancel := context.WithTimeout(ctx, s.cfg.ConnectTimeout)
	defer cancel()

	// Re-check host policy against resolved addresses now that we can do I/O.
	if err := VerifyResolvedHost(dialCtx, net.DefaultResolver, s.creds.Host, s.cfg.BlockPrivateHosts); err != nil {
		return nil, err
	}

	var (
		pool Pool
		err  error
	)
	switch s.creds.Engine {
	case EngineMySQL:
		pool, err = openMySQL(dialCtx, s.creds, s.cfg)
	case EnginePostgres:
		pool, err = openPostgres(dialCtx, s.creds, s.cfg)
	default:
		return nil, fmt.Errorf("unsupported database type %q", s.creds.Engine)
	}
	if err != nil {
		return nil, fmt.Errorf("could not initialise connection pool for %s: %w",
			s.creds.Target(), redact(err, s.creds))
	}

	// Both drivers connect lazily; ping so that a bad password surfaces here
	// with a clear message instead of midway through a tool call.
	if err := pool.Ping(dialCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("could not connect to %s as %q: %w",
			s.creds.Target(), s.creds.Username, redact(err, s.creds))
	}

	s.log.Info("database pool established",
		"engine", string(s.creds.Engine), "maxConns", s.cfg.MaxConns)
	return pool, nil
}

// QueryContext derives a context bounded by both the caller's deadline and the
// life of this session, so a dropped MCP stream cancels running queries.
func (s *Session) QueryContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// Close cancels in-flight work and releases the pool. It is idempotent and
// safe to call from any goroutine.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	pool := s.pool
	s.pool = nil
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.mu.Unlock()

	// Cancel first: pgxpool.Close waits for checked-out connections to be
	// returned, so an in-flight query would otherwise hold teardown open.
	s.cancel()

	if pool == nil {
		s.log.Debug("session closed before any pool was created")
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Close()
	}()

	grace := s.cfg.PoolCloseGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	select {
	case <-done:
		s.log.Info("database pool closed")
	case <-time.After(grace):
		// The goroutine stays parked until pgx finishes; log so a leak is
		// visible in metrics rather than silent.
		s.log.Warn("database pool did not close within grace period", "grace", grace)
	}
}

// Credentials exposes the (redacting) credentials for logging and messages.
func (s *Session) Credentials() *Credentials { return s.creds }

// redact strips anything resembling the tenant password out of driver errors
// before they travel back to the client or into a log line.
func redact(err error, c *Credentials) error {
	if err == nil || c.Password == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, c.Password) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, c.Password, "REDACTED"))
}
