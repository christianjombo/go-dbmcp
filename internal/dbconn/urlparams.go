package dbconn

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Connection parameters supplied in the MCP endpoint's query string.
//
// Why this exists: some clients expose only a URL field and no usable way to
// set headers. Claude's custom connector restricts header names to an
// approved allowlist of at most four, and there is no approved name meaning
// "database host". Putting the target in the URL removes that problem
// entirely.
//
// What must NOT go here: the password. Claude displays the connector URL in
// plaintext while masking header values ("stored securely and never shown
// again"); nginx writes the full request line, query string included, to its
// access log on every request; and URLs travel in browser history and in any
// exported connector configuration. The recommended split is therefore:
//
//	URL:    ?type=mysql&host=db.example.com&port=3306&db=appdb&user=app_reader
//	Header: authorization: Bearer <password>
//
// A password in the query string is still accepted, because some clients can
// send nothing else, but it is redacted from logs (see httpapi.redactQuery)
// and should be treated as compromised if the URL is ever shared.
//
// Aliases are generous on purpose: this is pasted by hand, and `db` vs
// `database` vs `dbname` is not a distinction worth failing a connection over.
var urlParamAliases = map[string][]string{
	"type":     {"type", "db_type", "dbtype", "engine", "driver"},
	"host":     {"host", "hostname", "server", "db_host"},
	"port":     {"port", "db_port"},
	"user":     {"user", "username", "uid", "db_user"},
	"password": {"password", "pass", "pwd", "db_password"},
	"database": {"database", "db", "dbname", "db_name", "schema"},
	"sslmode":  {"sslmode", "ssl_mode", "ssl-mode", "tls"},
	"dsn":      {"dsn", "url", "uri", "connection_string"},
}

// paramValue returns the first non-empty value among a parameter's aliases.
func paramValue(q url.Values, canonical string) string {
	for _, alias := range urlParamAliases[canonical] {
		if v := strings.TrimSpace(q.Get(alias)); v != "" {
			return v
		}
	}
	return ""
}

// HasURLParams reports whether the query string carries any connection
// parameter, so the caller can tell "not configured" from "configured badly".
func HasURLParams(q url.Values) bool {
	for canonical := range urlParamAliases {
		if paramValue(q, canonical) != "" {
			return true
		}
	}
	return false
}

// FromRequest resolves credentials from a request, combining every supported
// source. Later sources override earlier ones:
//
//  1. URL query parameters — the target, and anything a client can only put
//     in a URL.
//  2. Consolidated headers (authorization / x-api-key / x-tenant-id) — the
//     approved-name path for restricted clients.
//  3. Granular x-db-* headers — the most explicit form, so it wins.
//
// Merging rather than choosing one is what makes the recommended layout work:
// a non-secret target in the URL plus a password in one approved header.
func FromRequest(r *http.Request, d Defaults) (*Credentials, error) {
	q := r.URL.Query()

	// Start from a DSN in the query string if one was given whole.
	var base *Credentials
	if dsn := paramValue(q, "dsn"); dsn != "" {
		c, err := CredentialsFromToken(dsn, d)
		if err != nil {
			return nil, &ValidationError{Invalid: map[string]string{"url parameter 'dsn'": err.Error()}}
		}
		base = c
	}

	// Overlay discrete query parameters.
	if HasURLParams(q) {
		c, err := credentialsFromQuery(q, d, base)
		if err != nil {
			return nil, err
		}
		base = c
	}

	// Overlay headers. They are the more secure channel, so they win.
	headerCreds, headerErr := FromHeaders(r.Header, d)
	switch {
	case headerErr == nil:
		return mergeCredentials(base, headerCreds, d), nil
	case base != nil:
		// Headers were absent or incomplete, but the URL already described a
		// usable target. A password-only header still merges in below.
		if secret := stripAuthScheme(firstHeader(r.Header, secretHeaders)); secret != "" && base.Password == "" {
			base.Password = secret
		}
		if err := validateComplete(base); err != nil {
			return nil, err
		}
		return base, nil
	default:
		return nil, headerErr
	}
}

// credentialsFromQuery builds credentials from discrete query parameters,
// layered over an optional base from a `dsn` parameter.
func credentialsFromQuery(q url.Values, d Defaults, base *Credentials) (*Credentials, error) {
	c := &Credentials{}
	if base != nil {
		*c = *base
	}
	ve := &ValidationError{Invalid: map[string]string{}}

	if v := paramValue(q, "type"); v != "" {
		engine, err := ParseEngine(v)
		if err != nil {
			ve.Invalid["url parameter 'type'"] = err.Error()
		} else {
			c.Engine = engine
		}
	}
	if c.Engine == "" {
		c.Engine = d.Engine
	}
	if c.Engine == "" {
		c.Engine = EnginePostgres
	}

	if v := paramValue(q, "host"); v != "" {
		host, err := validHost(v)
		if err != nil {
			ve.Invalid["url parameter 'host'"] = err.Error()
		} else if err := checkLiteralHostPolicy(host, d.BlockPrivateHosts); err != nil {
			ve.Invalid["url parameter 'host'"] = err.Error()
		} else {
			c.Host = host
		}
	}

	if v := paramValue(q, "port"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			ve.Invalid["url parameter 'port'"] = "must be a number between 1 and 65535"
		} else {
			c.Port = n
		}
	}
	if c.Port == 0 {
		c.Port = c.Engine.DefaultPort()
	}

	if v := paramValue(q, "user"); v != "" {
		if err := validIdentifier(v, 63); err != nil {
			ve.Invalid["url parameter 'user'"] = err.Error()
		} else {
			c.Username = v
		}
	}

	if v := paramValue(q, "password"); v != "" {
		c.Password = v
	}

	if v := paramValue(q, "database"); v != "" {
		if err := validIdentifier(v, 63); err != nil {
			ve.Invalid["url parameter 'database'"] = err.Error()
		} else {
			c.Database = v
		}
	}
	if c.Database == "" {
		if d.Database != "" {
			c.Database = d.Database
		} else {
			c.Database = c.Engine.DefaultDatabase()
		}
	}

	if v := NormalizeSSLMode(paramValue(q, "sslmode")); v != "" {
		if !validSSLModes[v] {
			ve.Invalid["url parameter 'sslmode'"] = "must be one of: disable, allow, prefer, require, verify-ca, verify-full"
		} else {
			c.SSLMode = v
		}
	}
	if c.SSLMode == "" {
		c.SSLMode = d.SSLMode
	}
	if c.SSLMode == "" {
		c.SSLMode = "prefer"
	}

	if len(ve.Invalid) > 0 {
		return nil, ve
	}
	return c, nil
}

// mergeCredentials overlays non-empty fields of override onto base.
func mergeCredentials(base, override *Credentials, d Defaults) *Credentials {
	if base == nil {
		return override
	}
	if override == nil {
		return base
	}
	out := *base
	if override.Engine != "" {
		out.Engine = override.Engine
	}
	if override.Username != "" {
		out.Username = override.Username
	}
	if override.Password != "" {
		out.Password = override.Password
	}
	if override.Host != "" {
		out.Host = override.Host
	}
	if override.Port != 0 {
		out.Port = override.Port
	}
	// An empty database is meaningful for MySQL, so only override when the
	// header carried something other than the engine's own default.
	if override.Database != "" && override.Database != out.Engine.DefaultDatabase() {
		out.Database = override.Database
	}
	if override.SSLMode != "" {
		out.SSLMode = override.SSLMode
	}
	return &out
}

// validateComplete checks that a merged credential set can actually dial.
func validateComplete(c *Credentials) error {
	ve := &ValidationError{Invalid: map[string]string{}}
	if c.Username == "" {
		ve.Missing = append(ve.Missing, "user (url parameter 'user', or x-db-username)")
	}
	if c.Host == "" {
		ve.Missing = append(ve.Missing, "host (url parameter 'host', or x-db-host)")
	}
	if c.Port == 0 {
		ve.Missing = append(ve.Missing, "port (url parameter 'port', or x-db-port)")
	}
	if len(ve.Missing) > 0 {
		return ve
	}
	return nil
}
