// Package dbconn turns per-request HTTP headers into a validated PostgreSQL
// connection and manages that connection's lifecycle.
//
// This server holds no credential vault and no tenant table. Every request
// carries its own credentials, which live only in request scope and are never
// written to logs, metrics or disk. Credentials implements fmt.Stringer and
// slog.LogValuer so that even an accidental %v or logger call emits a
// redacted form rather than a password.
package dbconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Canonical header names. Go's http.Header canonicalises on access, so these
// match case-insensitively on the wire.
const (
	HeaderUsername = "X-Db-Username"
	HeaderPassword = "X-Db-Password"
	HeaderHost     = "X-Db-Host"
	HeaderPort     = "X-Db-Port"
	HeaderDatabase = "X-Db-Database"
	HeaderSSLMode  = "X-Db-Sslmode"
)

// WireHeaderNames is the lowercase, client-facing spelling, used in docs and
// error payloads.
var WireHeaderNames = map[string]string{
	HeaderUsername: "x-db-username",
	HeaderPassword: "x-db-password",
	HeaderHost:     "x-db-host",
	HeaderPort:     "x-db-port",
	HeaderDatabase: "x-db-database",
	HeaderSSLMode:  "x-db-sslmode",
}

// Credentials is a validated, ready-to-dial target database.
type Credentials struct {
	Username string
	Password string
	Host     string
	Port     int
	Database string
	SSLMode  string
}

// ValidationError reports everything wrong with a request at once, so a client
// fixes its configuration in a single round trip instead of discovering one
// missing header per attempt.
type ValidationError struct {
	Missing []string
	Invalid map[string]string
}

func (e *ValidationError) Error() string {
	var parts []string
	if len(e.Missing) > 0 {
		parts = append(parts, "missing required headers: "+strings.Join(e.Missing, ", "))
	}
	for _, k := range sortedKeys(e.Invalid) {
		parts = append(parts, fmt.Sprintf("%s: %s", k, e.Invalid[k]))
	}
	return strings.Join(parts, "; ")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Small maps; insertion sort keeps output deterministic for tests.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Defaults supplies the fallback values for the optional headers.
type Defaults struct {
	Database string
	SSLMode  string
	// BlockPrivateHosts rejects literal addresses in loopback, private and
	// unspecified ranges. Link-local is always rejected regardless, because
	// 169.254.169.254 is the cloud instance-metadata endpoint on every major
	// provider and no legitimate PostgreSQL server lives there.
	BlockPrivateHosts bool
}

// FromHeaders parses and validates the x-db-* header set. It performs no I/O:
// hostname resolution is deliberately deferred to dial time (see Session) so
// that validation stays fast, pure and unit-testable.
func FromHeaders(h http.Header, d Defaults) (*Credentials, error) {
	ve := &ValidationError{Invalid: map[string]string{}}

	get := func(name string) (string, bool) {
		vs, ok := h[name]
		if !ok || len(vs) == 0 {
			return "", false
		}
		return strings.TrimSpace(vs[0]), true
	}

	username, hasUser := get(HeaderUsername)
	// The password header must be present, but an empty value is permitted:
	// `trust` and `peer` authentication are legitimate configurations and
	// rejecting them would be a correctness bug, not a security win.
	password, hasPass := get(HeaderPassword)
	host, hasHost := get(HeaderHost)
	portRaw, hasPort := get(HeaderPort)

	for name, present := range map[string]bool{
		HeaderUsername: hasUser, HeaderPassword: hasPass,
		HeaderHost: hasHost, HeaderPort: hasPort,
	} {
		if !present {
			ve.Missing = append(ve.Missing, WireHeaderNames[name])
		}
	}
	// Deterministic ordering for tests and readable error messages.
	ve.Missing = sortStrings(ve.Missing)

	c := &Credentials{Password: password}

	if hasUser {
		if err := validIdentifier(username, 63); err != nil {
			ve.Invalid[WireHeaderNames[HeaderUsername]] = err.Error()
		} else {
			c.Username = username
		}
	}

	if hasHost {
		normalised, err := validHost(host)
		if err != nil {
			ve.Invalid[WireHeaderNames[HeaderHost]] = err.Error()
		} else if err := checkLiteralHostPolicy(normalised, d.BlockPrivateHosts); err != nil {
			ve.Invalid[WireHeaderNames[HeaderHost]] = err.Error()
		} else {
			c.Host = normalised
		}
	}

	if hasPort {
		p, err := strconv.Atoi(portRaw)
		switch {
		case err != nil:
			ve.Invalid[WireHeaderNames[HeaderPort]] = fmt.Sprintf("%q is not a number", portRaw)
		case p < 1 || p > 65535:
			ve.Invalid[WireHeaderNames[HeaderPort]] = fmt.Sprintf("%d is outside 1-65535", p)
		default:
			c.Port = p
		}
	}

	database, hasDB := get(HeaderDatabase)
	if !hasDB || database == "" {
		c.Database = d.Database
	} else if err := validIdentifier(database, 63); err != nil {
		ve.Invalid[WireHeaderNames[HeaderDatabase]] = err.Error()
	} else {
		c.Database = database
	}

	sslmode, hasSSL := get(HeaderSSLMode)
	if !hasSSL || sslmode == "" {
		c.SSLMode = d.SSLMode
	} else if !validSSLModes[strings.ToLower(sslmode)] {
		ve.Invalid[WireHeaderNames[HeaderSSLMode]] = fmt.Sprintf(
			"%q is not a valid libpq sslmode (disable, allow, prefer, require, verify-ca, verify-full)", sslmode)
	} else {
		c.SSLMode = strings.ToLower(sslmode)
	}

	if len(ve.Missing) > 0 || len(ve.Invalid) > 0 {
		return nil, ve
	}
	return c, nil
}

var validSSLModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true,
	"require": true, "verify-ca": true, "verify-full": true,
}

// hostnameRE matches a conventional DNS name. Anything outside it — notably
// '/', '@', '?', '#', whitespace and control characters — is rejected before
// it can reach the connection string.
var hostnameRE = regexp.MustCompile(
	`^[A-Za-z0-9]([A-Za-z0-9\-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9\-]{0,61}[A-Za-z0-9])?)*\.?$`)

// validHost accepts an IPv4/IPv6 literal or a DNS hostname and returns the
// unbracketed canonical form.
func validHost(h string) (string, error) {
	if h == "" {
		return "", errors.New("must not be empty")
	}
	if len(h) > 255 {
		return "", errors.New("exceeds 255 characters")
	}
	unbracketed := strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if addr, err := netip.ParseAddr(unbracketed); err == nil {
		return addr.String(), nil
	}
	if !hostnameRE.MatchString(h) {
		return "", errors.New("is neither a valid IP address nor a valid hostname")
	}
	return h, nil
}

// validIdentifier rejects control characters and absurd lengths. Quoting is
// handled structurally by net/url when the DSN is assembled, so this is a
// sanity check rather than the escaping mechanism.
func validIdentifier(s string, max int) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	if len(s) > max {
		return fmt.Errorf("exceeds %d characters", max)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return errors.New("contains control characters")
		}
	}
	return nil
}

// ErrBlockedHost is returned when a target address is barred by host policy.
var ErrBlockedHost = errors.New("blocked by host policy")

func checkLiteralHostPolicy(host string, blockPrivate bool) error {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil // a DNS name; re-checked after resolution at dial time
	}
	return checkAddrPolicy(addr, blockPrivate)
}

func checkAddrPolicy(addr netip.Addr, blockPrivate bool) error {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	// Always blocked: cloud instance metadata lives at 169.254.169.254.
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return fmt.Errorf("%w: link-local addresses are never permitted", ErrBlockedHost)
	}
	if !blockPrivate {
		return nil
	}
	switch {
	case addr.IsLoopback():
		return fmt.Errorf("%w: loopback addresses are not permitted", ErrBlockedHost)
	case addr.IsPrivate():
		return fmt.Errorf("%w: private addresses are not permitted", ErrBlockedHost)
	case addr.IsUnspecified():
		return fmt.Errorf("%w: the unspecified address is not permitted", ErrBlockedHost)
	}
	return nil
}

// VerifyResolvedHost re-applies host policy after DNS resolution. It closes
// the gap where a hostname resolves into a blocked range. A determined client
// can still win the DNS race (TOCTOU), so this is defence in depth, not a
// security boundary; the real boundary is network egress policy on the host.
func VerifyResolvedHost(ctx context.Context, resolver *net.Resolver, host string, blockPrivate bool) error {
	if _, err := netip.ParseAddr(host); err == nil {
		return nil // literal, already checked during validation
	}
	ips, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if err := checkAddrPolicy(ip, blockPrivate); err != nil {
			return fmt.Errorf("%s resolves to %s which is %w", host, ip, err)
		}
	}
	return nil
}

// DSN assembles a libpq connection URI. net/url performs the escaping, so a
// password containing '@', '/', ':' or '?' cannot restructure the URI.
func (c *Credentials) DSN() string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.Username, c.Password),
		Host:   net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:   "/" + c.Database,
	}
	q := url.Values{}
	q.Set("sslmode", c.SSLMode)
	q.Set("application_name", "mcp-proxy")
	u.RawQuery = q.Encode()
	return u.String()
}

// Fingerprint is a stable, non-reversible handle for correlating log lines
// belonging to the same target. The password is deliberately excluded so that
// a fingerprint can never be used to confirm a guessed password.
func (c *Credentials) Fingerprint() string {
	sum := sha256.Sum256([]byte(c.Username + "\x00" + c.Host + "\x00" +
		strconv.Itoa(c.Port) + "\x00" + c.Database))
	return hex.EncodeToString(sum[:8])
}

// Target is the human-readable destination, without credentials.
func (c *Credentials) Target() string {
	return fmt.Sprintf("%s:%d/%s", c.Host, c.Port, c.Database)
}

// String implements fmt.Stringer with the password redacted.
func (c *Credentials) String() string {
	return fmt.Sprintf("postgres://%s:REDACTED@%s?sslmode=%s", c.Username, c.Target(), c.SSLMode)
}

// LogValue implements slog.LogValuer so structured logging cannot leak a
// password even if a Credentials value is passed to a logger wholesale.
func (c *Credentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("user", c.Username),
		slog.String("target", c.Target()),
		slog.String("sslmode", c.SSLMode),
		slog.String("fingerprint", c.Fingerprint()),
	)
}

func sortStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}
