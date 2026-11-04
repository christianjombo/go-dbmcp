package dbconn

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func defaults() Defaults {
	return Defaults{Database: "postgres", SSLMode: "prefer"}
}

func headers(kv map[string]string) http.Header {
	h := http.Header{}
	for k, v := range kv {
		h.Set(k, v)
	}
	return h
}

func validHeaders() map[string]string {
	return map[string]string{
		"x-db-username": "app_reader",
		"x-db-password": "s3cret",
		"x-db-host":     "db.example.com",
		"x-db-port":     "5432",
	}
}

func TestFromHeadersAppliesDefaults(t *testing.T) {
	c, err := FromHeaders(headers(validHeaders()), defaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Database != "postgres" {
		t.Errorf("database = %q, want the default %q", c.Database, "postgres")
	}
	if c.SSLMode != "prefer" {
		t.Errorf("sslmode = %q, want the default %q", c.SSLMode, "prefer")
	}
	if c.Port != 5432 {
		t.Errorf("port = %d", c.Port)
	}
}

func TestFromHeadersOverridesDefaults(t *testing.T) {
	hdr := validHeaders()
	hdr["x-db-database"] = "analytics"
	hdr["x-db-sslmode"] = "require"

	c, err := FromHeaders(headers(hdr), defaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Database != "analytics" || c.SSLMode != "require" {
		t.Errorf("got database=%q sslmode=%q", c.Database, c.SSLMode)
	}
}

// All problems are reported at once so a client fixes its configuration in
// one round trip rather than one header per attempt.
func TestFromHeadersReportsEveryMissingHeaderAtOnce(t *testing.T) {
	_, err := FromHeaders(http.Header{}, defaults())

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}

	want := []string{"x-db-host", "x-db-password", "x-db-port", "x-db-username"}
	if len(ve.Missing) != len(want) {
		t.Fatalf("missing = %v, want %v", ve.Missing, want)
	}
	for i, w := range want {
		if ve.Missing[i] != w {
			t.Errorf("missing[%d] = %q, want %q (list must be sorted)", i, ve.Missing[i], w)
		}
	}
}

func TestFromHeadersRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		value  string
		header string
	}{
		{"non-numeric port", "x-db-port", "abc", "x-db-port"},
		{"port out of range", "x-db-port", "70000", "x-db-port"},
		{"zero port", "x-db-port", "0", "x-db-port"},
		{"unknown sslmode", "x-db-sslmode", "sorta", "x-db-sslmode"},
		{"host with path traversal", "x-db-host", "db.example.com/evil", "x-db-host"},
		{"host with credentials", "x-db-host", "evil@db.example.com", "x-db-host"},
		{"host with space", "x-db-host", "db example", "x-db-host"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := validHeaders()
			hdr[tc.key] = tc.value

			_, err := FromHeaders(headers(hdr), defaults())
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected a validation error, got %v", err)
			}
			if _, ok := ve.Invalid[tc.header]; !ok {
				t.Fatalf("expected %s to be reported invalid, got %v", tc.header, ve.Invalid)
			}
		})
	}
}

// An empty password is legitimate under trust/peer authentication; only a
// missing header is an error.
func TestEmptyPasswordIsAccepted(t *testing.T) {
	hdr := validHeaders()
	hdr["x-db-password"] = ""

	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	if _, err := FromHeaders(h, defaults()); err != nil {
		t.Fatalf("empty password should be accepted: %v", err)
	}
}

func TestIPv6HostIsAccepted(t *testing.T) {
	hdr := validHeaders()
	hdr["x-db-host"] = "2001:db8::1"

	c, err := FromHeaders(headers(hdr), defaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// JoinHostPort must bracket the address, or the DSN is unparseable.
	if !strings.Contains(c.postgresDSN(), "[2001:db8::1]:5432") {
		t.Errorf("IPv6 host not bracketed in DSN: %s", c.String())
	}
}

// The cloud instance-metadata endpoint is never a legitimate database target,
// so it is refused regardless of the private-host setting.
func TestLinkLocalHostIsAlwaysBlocked(t *testing.T) {
	hdr := validHeaders()
	hdr["x-db-host"] = "169.254.169.254"

	_, err := FromHeaders(headers(hdr), Defaults{Database: "postgres", SSLMode: "prefer", BlockPrivateHosts: false})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected link-local to be refused, got %v", err)
	}
}

func TestPrivateHostsBlockedOnlyWhenConfigured(t *testing.T) {
	hdr := validHeaders()
	hdr["x-db-host"] = "10.0.0.5"

	if _, err := FromHeaders(headers(hdr), Defaults{Database: "db", SSLMode: "prefer"}); err != nil {
		t.Fatalf("private host should be allowed by default: %v", err)
	}

	d := Defaults{Database: "db", SSLMode: "prefer", BlockPrivateHosts: true}
	if _, err := FromHeaders(headers(hdr), d); err == nil {
		t.Fatal("private host should be refused when BlockPrivateHosts is set")
	}
}

// A password containing URI metacharacters must not be able to restructure
// the connection string.
func TestDSNEscapesCredentials(t *testing.T) {
	c := &Credentials{
		Username: "user name",
		Password: `p@ss:w/rd?#&=x`,
		Host:     "db.example.com",
		Port:     5432,
		Database: "my db",
		SSLMode:  "require",
	}

	u, err := url.Parse(c.postgresDSN())
	if err != nil {
		t.Fatalf("DSN does not parse: %v", err)
	}
	if u.Host != "db.example.com:5432" {
		t.Errorf("host = %q — credentials leaked into the authority", u.Host)
	}
	if got := u.User.Username(); got != "user name" {
		t.Errorf("username round-trip = %q", got)
	}
	pw, _ := u.User.Password()
	if pw != c.Password {
		t.Errorf("password round-trip = %q, want %q", pw, c.Password)
	}
	if u.Path != "/my db" {
		t.Errorf("database round-trip = %q", u.Path)
	}
	if u.Query().Get("sslmode") != "require" {
		t.Errorf("sslmode = %q", u.Query().Get("sslmode"))
	}
}

// Accidentally logging a Credentials value must not leak the password, so
// both the Stringer and the slog.LogValuer are checked.
func TestCredentialsNeverRenderThePassword(t *testing.T) {
	c := &Credentials{
		Username: "u", Password: "TOPSECRET", Host: "h", Port: 5432,
		Database: "d", SSLMode: "require",
	}

	if strings.Contains(c.String(), "TOPSECRET") {
		t.Errorf("String() leaked the password: %s", c.String())
	}
	if strings.Contains(c.LogValue().String(), "TOPSECRET") {
		t.Errorf("LogValue() leaked the password: %s", c.LogValue().String())
	}
}

// The fingerprint correlates log lines without being a password oracle.
func TestFingerprintIsStableAndExcludesPassword(t *testing.T) {
	a := &Credentials{Username: "u", Password: "one", Host: "h", Port: 1, Database: "d"}
	b := &Credentials{Username: "u", Password: "two", Host: "h", Port: 1, Database: "d"}
	c := &Credentials{Username: "u", Password: "one", Host: "h", Port: 2, Database: "d"}

	if a.Fingerprint() != b.Fingerprint() {
		t.Error("fingerprint must not depend on the password")
	}
	if a.Fingerprint() == c.Fingerprint() {
		t.Error("fingerprint must distinguish different targets")
	}
	if strings.Contains(a.Fingerprint(), "one") {
		t.Error("fingerprint leaked the password")
	}
}
