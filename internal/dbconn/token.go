package dbconn

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Consolidated credentials, for clients that cannot send arbitrary headers.
//
// Claude's custom connector restricts header names to an Anthropic-approved
// allowlist and permits at most four of them, so `x-db-host` and friends are
// rejected outright and there is no approved name meaning "database host".
// Two supported layouts work within that constraint:
//
//	One header (recommended) — the whole target, password included:
//	  authorization: Bearer <base64 of mysql://user:pw@host:3306/db?sslmode=require>
//
//	Two headers — secret separated from the target it applies to:
//	  x-tenant-id: mysql://user@host:3306/db?sslmode=require
//	  x-api-key:   <password>
//
// Both are exactly as safe as the granular headers: the same secret travels
// over the same TLS connection, and the server stays stateless because the
// credential is supplied per request rather than looked up.
//
// Every name below is on Anthropic's approved list. The granular x-db-*
// headers keep working for clients without the restriction (Cursor), and
// remain the preferred form once a custom name is approved.
var (
	// secretHeaders carry either a bare password or a complete DSN.
	secretHeaders = []string{
		"Authorization",
		"X-Api-Key", "Api-Key", "Apikey", "X-Apikey",
		"X-Api-Token", "Api-Token", "X-Auth-Token", "X-Access-Token", "X-Token",
		"X-Key", "Access-Key", "X-Api-Secret", "X-Secret-Key",
		"X-App-Key", "App-Key", "X-Application-Key",
		"X-Client-Key", "X-Client-Secret",
		"X-Functions-Key", "X-Gateway-Api-Key", "X-Gateway-Key",
		"X-Rapidapi-Key", "Ocp-Apim-Subscription-Key",
		// Not on the approved list, but natural for clients without the
		// restriction and harmless to accept.
		"X-Db-Dsn",
	}

	// targetHeaders carry a DSN identifying which database to reach. The
	// password may be omitted and supplied separately via a secret header.
	targetHeaders = []string{
		"X-Tenant-Id", "X-Tenant", "X-Tenant-Key",
		"X-Org-Id", "X-Org", "X-Organization", "X-Organization-Id",
		"X-Workspace-Id", "X-Workspace",
		"X-Account-Id", "X-Account-Key",
		"X-Project-Id", "X-Project-Key",
		"X-Environment",
		"X-Db-Dsn",
	}
)

// ErrNoToken reports that no consolidated-credential header was present.
var ErrNoToken = errors.New("no connection-string header present")

// dsnSchemes maps a DSN scheme onto an engine.
var dsnSchemes = map[string]Engine{
	"mysql": EngineMySQL, "mariadb": EngineMySQL,
	"postgres": EnginePostgres, "postgresql": EnginePostgres, "pg": EnginePostgres,
}

type headerGetter interface{ Get(string) string }

// firstHeader returns the first non-empty value among names.
func firstHeader(h headerGetter, names []string) string {
	for _, name := range names {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// stripAuthScheme removes a "Bearer "/"Token " prefix, which Authorization
// conventionally carries and the other headers do not.
func stripAuthScheme(v string) string {
	for _, scheme := range []string{"Bearer ", "Token "} {
		if len(v) >= len(scheme) && strings.EqualFold(v[:len(scheme)], scheme) {
			return strings.TrimSpace(v[len(scheme):])
		}
	}
	return v
}

// credentialsFromApprovedHeaders assembles credentials from the consolidated
// header layouts. It returns ErrNoToken when none are present.
func credentialsFromApprovedHeaders(h headerGetter, d Defaults) (*Credentials, error) {
	target := firstHeader(h, targetHeaders)
	secret := stripAuthScheme(firstHeader(h, secretHeaders))

	switch {
	case target != "":
		c, err := CredentialsFromToken(target, d)
		if err != nil {
			return nil, err
		}
		// The target DSN usually omits the password so it is not duplicated
		// in two places; take it from the secret header when absent.
		if c.Password == "" && secret != "" {
			c.Password = secret
		}
		return c, nil

	case looksLikeDSN(secret):
		// The secret header carries the whole connection string.
		return CredentialsFromToken(secret, d)

	default:
		// Either nothing was sent, or the secret header holds a bare password
		// with no target to attach it to — the recommended layout, where the
		// target comes from URL parameters. Report "no token" so the caller
		// falls through and the password is merged onto that target instead
		// of failing with a misleading "not a valid connection string".
		return nil, ErrNoToken
	}
}

// looksLikeDSN reports whether a value is a connection string rather than a
// bare password, after any base64 layer is removed.
func looksLikeDSN(v string) bool {
	return v != "" && strings.Contains(decodeToken(v), "://")
}

// CredentialsFromToken parses a connection string into validated credentials.
//
// The token may be base64 (standard or URL alphabet, with or without
// padding) or a bare DSN. Unlike the header form, the port may be omitted and
// defaults to the engine's conventional port — a DSN names its engine
// explicitly, so there is nothing to guess.
func CredentialsFromToken(token string, d Defaults) (*Credentials, error) {
	raw := decodeToken(stripAuthScheme(token))

	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return nil, errors.New("the connection string is not a valid URL; expected " +
			"mysql://user:password@host:3306/database or postgres://user:password@host:5432/database " +
			"(optionally base64-encoded)")
	}

	engine, ok := dsnSchemes[strings.ToLower(u.Scheme)]
	if !ok {
		return nil, fmt.Errorf("%q is not a supported scheme (expected one of: mysql, mariadb, postgres, postgresql)",
			u.Scheme)
	}

	c := &Credentials{Engine: engine}

	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("the connection string has no username")
	}
	c.Username = u.User.Username()
	if err := validIdentifier(c.Username, 63); err != nil {
		return nil, fmt.Errorf("username %w", err)
	}
	// An empty password is legitimate: trust/peer authentication, or the
	// password arriving separately in a secret header.
	c.Password, _ = u.User.Password()

	host, err := validHost(u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("host %w", err)
	}
	if err := checkLiteralHostPolicy(host, d.BlockPrivateHosts); err != nil {
		return nil, err
	}
	c.Host = host

	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("port %q must be a number between 1 and 65535", p)
		}
		c.Port = n
	} else {
		c.Port = engine.DefaultPort()
	}

	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		if err := validIdentifier(db, 63); err != nil {
			return nil, fmt.Errorf("database %w", err)
		}
		c.Database = db
	} else if d.Database != "" {
		c.Database = d.Database
	} else {
		c.Database = engine.DefaultDatabase()
	}

	q := u.Query()
	mode := NormalizeSSLMode(q.Get("sslmode"))
	if mode == "" {
		mode = NormalizeSSLMode(q.Get("ssl-mode")) // MySQL's spelling
	}
	switch {
	case mode == "":
		c.SSLMode = d.SSLMode
	case validSSLModes[mode]:
		c.SSLMode = mode
	default:
		return nil, fmt.Errorf("sslmode %q is not valid (expected one of: disable, allow, prefer, require, verify-ca, verify-full)", mode)
	}
	if c.SSLMode == "" {
		c.SSLMode = "prefer"
	}

	return c, nil
}

// decodeToken returns the decoded DSN if the token is base64, or the token
// unchanged otherwise. Both alphabets and optional padding are accepted
// because clients differ in what they produce.
func decodeToken(token string) string {
	// A bare DSN contains "://", which cannot appear in base64.
	if strings.Contains(token, "://") {
		return token
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(token); err == nil && strings.Contains(string(decoded), "://") {
			return strings.TrimSpace(string(decoded))
		}
	}
	return token
}

// EncodeToken renders a DSN as a base64 connection string, for pasting into a
// client that cannot send custom headers.
func EncodeToken(dsn string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(dsn)))
}
