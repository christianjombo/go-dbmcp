// Package config loads server configuration from the environment on boot.
//
// Philosophy: this is a hosted service, so a typo in a unit file must never
// stop the process from starting. Every value has a safe fallback, and an
// unparseable or out-of-range value degrades to that fallback with a recorded
// warning rather than aborting boot. Load therefore returns warnings instead
// of errors; the caller logs them once the logger exists.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults for the serialization matrix, mandated by spec.
const (
	DefaultJSONMaxRows = 100
	DefaultCSVMaxRows  = 2000
)

// Config is the fully resolved, validated server configuration.
type Config struct {
	// HTTP
	ListenAddr        string
	ShutdownGrace     time.Duration
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration

	// Serialization matrix (see internal/serialize).
	JSONMaxRows int
	CSVMaxRows  int

	// Defaults applied to optional x-db-* headers.
	DefaultDBType string
	// DefaultDatabase empty means "derive from the engine" — see
	// dbconn.Engine.DefaultDatabase.
	DefaultDatabase string
	DefaultSSLMode  string

	// Per-session database pool behaviour.
	QueryTimeout        time.Duration
	ConnectTimeout      time.Duration
	PoolMaxConns        int32
	PoolMaxConnIdleTime time.Duration
	PoolMaxConnLifetime time.Duration

	// MaxResultBytes caps the raw bytes accumulated from a single result set
	// before the read stops. Row ceilings alone do not bound memory: 2000
	// rows each holding a 1 MB bytea is 2 GB on a droplet with 1 GB of RAM.
	MaxResultBytes int

	// Guardrails.
	//
	// AllowWrites keeps INSERT, UPDATE, DELETE, REPLACE and MERGE in the
	// permitted command set. Setting it false is a convenience, not an
	// enforcement mechanism: the dependable way to make access read-only is
	// to grant the role in x-db-username nothing but SELECT.
	AllowWrites          bool
	BlockPrivateHosts    bool
	ExtraBlockedPatterns []string

	// DisableLocalhostProtection turns off the SDK's DNS-rebinding guard.
	//
	// That guard rejects a request that arrives from a loopback address but
	// carries a non-loopback Host header. Behind nginx or any reverse proxy
	// on the same droplet, every request looks exactly like that, so the
	// guard returns 403 for all traffic. Set this when, and only when, the
	// listener is not directly reachable from the internet.
	DisableLocalhostProtection bool

	LogLevel string
	Version  string
}

// Load reads configuration from the process environment. The returned
// warnings describe every value that fell back to its default.
func Load(version string) (*Config, []string) {
	var warn []string
	w := func(format string, args ...any) { warn = append(warn, fmt.Sprintf(format, args...)) }

	c := &Config{
		ListenAddr:        envStr("MCP_LISTEN_ADDR", ":8080"),
		ShutdownGrace:     envDurMS("MCP_SHUTDOWN_GRACE_MS", 15_000, 0, 300_000, w),
		ReadHeaderTimeout: envDurMS("MCP_READ_HEADER_TIMEOUT_MS", 10_000, 1_000, 120_000, w),
		IdleTimeout:       envDurMS("MCP_IDLE_TIMEOUT_MS", 120_000, 5_000, 3_600_000, w),

		JSONMaxRows: envInt("MCP_JSON_MAX_ROWS", DefaultJSONMaxRows, 1, 1_000_000, w),
		CSVMaxRows:  envInt("MCP_CSV_MAX_ROWS", DefaultCSVMaxRows, 1, 10_000_000, w),

		DefaultDBType: strings.ToLower(envStr("MCP_DEFAULT_DB_TYPE", "mysql")),
		// Empty means "whatever the chosen engine conventionally expects":
		// "postgres" for PostgreSQL, no database selected for MySQL. A
		// single literal default cannot be correct for both.
		DefaultDatabase: envStr("MCP_DEFAULT_DATABASE", ""),
		DefaultSSLMode:  envStr("MCP_DEFAULT_SSLMODE", "prefer"),

		QueryTimeout:        envDurMS("MCP_QUERY_TIMEOUT_MS", 30_000, 100, 600_000, w),
		ConnectTimeout:      envDurMS("MCP_CONNECT_TIMEOUT_MS", 10_000, 500, 120_000, w),
		PoolMaxConns:        int32(envInt("MCP_POOL_MAX_CONNS", 4, 1, 100, w)),
		PoolMaxConnIdleTime: envDurMS("MCP_POOL_MAX_CONN_IDLE_MS", 60_000, 1_000, 3_600_000, w),
		PoolMaxConnLifetime: envDurMS("MCP_POOL_MAX_CONN_LIFETIME_MS", 1_800_000, 10_000, 86_400_000, w),

		MaxResultBytes: envInt("MCP_MAX_RESULT_BYTES", 32<<20, 64<<10, 1<<30, w),

		AllowWrites:                envBool("MCP_ALLOW_WRITES", true, w),
		BlockPrivateHosts:          envBool("MCP_BLOCK_PRIVATE_HOSTS", false, w),
		DisableLocalhostProtection: envBool("MCP_DISABLE_LOCALHOST_PROTECTION", false, w),

		LogLevel: strings.ToLower(envStr("MCP_LOG_LEVEL", "info")),
		Version:  version,
	}

	if extra := envStr("MCP_EXTRA_BLOCKED_PATTERNS", ""); extra != "" {
		for _, p := range strings.Split(extra, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.ExtraBlockedPatterns = append(c.ExtraBlockedPatterns, p)
			}
		}
	}

	// The matrix only makes sense as JSON ceiling <= CSV ceiling. Rather than
	// refusing to boot on a transposed pair, clamp and say so loudly.
	if c.JSONMaxRows > c.CSVMaxRows {
		w("MCP_JSON_MAX_ROWS (%d) exceeds MCP_CSV_MAX_ROWS (%d); raising the CSV ceiling to match",
			c.JSONMaxRows, c.CSVMaxRows)
		c.CSVMaxRows = c.JSONMaxRows
	}

	switch c.DefaultDBType {
	case "postgres", "postgresql", "pg", "psql", "mysql", "mariadb":
	default:
		w("MCP_DEFAULT_DB_TYPE %q is not a supported database type; falling back to \"mysql\"", c.DefaultDBType)
		c.DefaultDBType = "mysql"
	}

	if !validSSLMode(c.DefaultSSLMode) {
		w("MCP_DEFAULT_SSLMODE %q is not a recognised libpq sslmode; falling back to %q",
			c.DefaultSSLMode, "prefer")
		c.DefaultSSLMode = "prefer"
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		w("MCP_LOG_LEVEL %q is not recognised; falling back to \"info\"", c.LogLevel)
		c.LogLevel = "info"
	}

	return c, warn
}

// SSLModes is the set of libpq sslmode values accepted from clients.
var SSLModes = map[string]struct{}{
	"disable": {}, "allow": {}, "prefer": {}, "require": {}, "verify-ca": {}, "verify-full": {},
}

func validSSLMode(m string) bool {
	_, ok := SSLModes[m]
	return ok
}

// Redacted returns a log-safe view of the configuration. No tenant
// credentials ever reach this struct — they live only in request scope.
func (c *Config) Redacted() []any {
	return []any{
		"listenAddr", c.ListenAddr,
		"version", c.Version,
		"jsonMaxRows", c.JSONMaxRows,
		"csvMaxRows", c.CSVMaxRows,
		"defaultDatabase", c.DefaultDatabase,
		"defaultSSLMode", c.DefaultSSLMode,
		"queryTimeout", c.QueryTimeout.String(),
		"poolMaxConns", c.PoolMaxConns,
		"defaultDbType", c.DefaultDBType,
		"allowWrites", c.AllowWrites,
		"blockPrivateHosts", c.BlockPrivateHosts,
	}
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return def
}

func envInt(key string, def, min, max int, w func(string, ...any)) int {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		w("%s=%q is not an integer; using default %d", key, raw, def)
		return def
	}
	if n < min || n > max {
		w("%s=%d is outside [%d, %d]; using default %d", key, n, min, max, def)
		return def
	}
	return n
}

func envDurMS(key string, def, min, max int, w func(string, ...any)) time.Duration {
	return time.Duration(envInt(key, def, min, max, w)) * time.Millisecond
}

func envBool(key string, def bool, w func(string, ...any)) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		w("%s=%q is not a boolean; using default %t", key, raw, def)
		return def
	}
	return b
}
