package dbconn

import (
	"net/http"
	"strings"
	"testing"
)

func tokenDefaults() Defaults {
	return Defaults{Engine: EngineMySQL, SSLMode: "prefer"}
}

func TestCredentialsFromTokenPlainDSN(t *testing.T) {
	c, err := CredentialsFromToken("mysql://app:s3cret@db.example.com:3306/appdb?sslmode=require", tokenDefaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Engine != EngineMySQL {
		t.Errorf("engine = %q", c.Engine)
	}
	if c.Username != "app" || c.Password != "s3cret" {
		t.Errorf("user/pass = %q/%q", c.Username, c.Password)
	}
	if c.Host != "db.example.com" || c.Port != 3306 {
		t.Errorf("host:port = %s:%d", c.Host, c.Port)
	}
	if c.Database != "appdb" || c.SSLMode != "require" {
		t.Errorf("db/sslmode = %q/%q", c.Database, c.SSLMode)
	}
}

func TestCredentialsFromTokenBase64(t *testing.T) {
	dsn := "postgres://app:p%40ss@db.example.com:5432/appdb"
	for _, encoded := range []string{
		EncodeToken(dsn),                         // standard, padded
		strings.TrimRight(EncodeToken(dsn), "="), // unpadded
	} {
		c, err := CredentialsFromToken(encoded, tokenDefaults())
		if err != nil {
			t.Fatalf("%q: %v", encoded, err)
		}
		if c.Engine != EnginePostgres {
			t.Errorf("engine = %q", c.Engine)
		}
		// The password was percent-encoded in the URL and must come back whole.
		if c.Password != "p@ss" {
			t.Errorf("password = %q, want %q", c.Password, "p@ss")
		}
	}
}

// A DSN names its engine, so an omitted port can be defaulted unambiguously.
func TestCredentialsFromTokenDefaultsPortPerEngine(t *testing.T) {
	my, err := CredentialsFromToken("mysql://u:p@h/db", tokenDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if my.Port != 3306 {
		t.Errorf("mysql port = %d, want 3306", my.Port)
	}

	pg, err := CredentialsFromToken("postgres://u:p@h/db", tokenDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if pg.Port != 5432 {
		t.Errorf("postgres port = %d, want 5432", pg.Port)
	}
}

func TestCredentialsFromTokenDatabaseFallsBackToEngine(t *testing.T) {
	my, err := CredentialsFromToken("mysql://u:p@h:3306/", tokenDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if my.Database != "" {
		t.Errorf("mysql database = %q, want empty", my.Database)
	}

	pg, err := CredentialsFromToken("postgres://u:p@h:5432", tokenDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if pg.Database != "postgres" {
		t.Errorf("postgres database = %q, want \"postgres\"", pg.Database)
	}
}

func TestCredentialsFromTokenRejectsBadInput(t *testing.T) {
	cases := []struct{ name, token string }{
		{"not a url", "just-some-text"},
		{"unsupported scheme", "oracle://u:p@h:1521/db"},
		{"no username", "mysql://@h:3306/db"},
		{"bad port", "mysql://u:p@h:99999/db"},
		{"bad sslmode", "mysql://u:p@h:3306/db?sslmode=sorta"},
		{"link-local host", "mysql://u:p@169.254.169.254:3306/db"},
		{"database name with path separators", "mysql://u:p@h/db/../../etc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CredentialsFromToken(tc.token, tokenDefaults()); err == nil {
				t.Fatalf("%q should have been rejected", tc.token)
			}
		})
	}
}

// Claude's connector sends Authorization; others may send X-Api-Key.
func TestFromHeadersAcceptsConsolidatedToken(t *testing.T) {
	dsn := "mysql://app:s3cret@db.example.com:3306/appdb?sslmode=require"

	for _, hdr := range []struct{ name, value string }{
		{"Authorization", "Bearer " + EncodeToken(dsn)},
		{"Authorization", "bearer " + EncodeToken(dsn)}, // scheme is case-insensitive
		{"Authorization", EncodeToken(dsn)},             // bare value, no scheme
		{"X-Api-Key", EncodeToken(dsn)},
		{"X-Db-Dsn", dsn}, // unencoded
	} {
		t.Run(hdr.name+"/"+hdr.value[:min(12, len(hdr.value))], func(t *testing.T) {
			h := http.Header{}
			h.Set(hdr.name, hdr.value)

			c, err := FromHeaders(h, tokenDefaults())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Host != "db.example.com" || c.Database != "appdb" || c.Username != "app" {
				t.Errorf("got %s as %q db %q", c.Host, c.Username, c.Database)
			}
		})
	}
}

// Granular headers remain authoritative when both forms are present, so a
// client that sets Authorization for some other purpose is not hijacked.
func TestGranularHeadersWinOverToken(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+EncodeToken("mysql://token:token@token-host:3306/tokendb"))
	h.Set("x-db-type", "postgres")
	h.Set("x-db-username", "granular")
	h.Set("x-db-password", "pw")
	h.Set("x-db-host", "granular-host")
	h.Set("x-db-port", "5432")

	c, err := FromHeaders(h, tokenDefaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Host != "granular-host" || c.Username != "granular" {
		t.Errorf("token overrode the granular headers: host=%q user=%q", c.Host, c.Username)
	}
}

// A bare secret with no target is the recommended layout's other half, so it
// must not be reported as a malformed connection string. The error has to say
// that the password was understood but the target is missing.
func TestBarePasswordWithoutTargetIsExplained(t *testing.T) {
	h := http.Header{}
	h.Set("X-Api-Key", "just-a-password")

	_, err := FromHeaders(h, tokenDefaults())
	if err == nil {
		t.Fatal("expected an error: a password alone cannot identify a database")
	}
	msg := err.Error()
	if !strings.Contains(msg, "a password was received") {
		t.Errorf("error should acknowledge the password, got: %v", err)
	}
	if !strings.Contains(msg, "which database") {
		t.Errorf("error should say the target is missing, got: %v", err)
	}
}

// A bare password combined with a target in the URL is the recommended
// layout and must resolve cleanly.
func TestBarePasswordMergesWithURLTarget(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet,
		"https://example.test/v1/mcp/connect?type=mysql&host=db.example.com&port=25060&db=appdb&user=app_reader", nil)
	r.Header.Set("X-Api-Key", "s3cret")

	c, err := FromRequest(r, tokenDefaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Password != "s3cret" {
		t.Errorf("password = %q, want the x-api-key value", c.Password)
	}
	if c.Host != "db.example.com" || c.Port != 25060 || c.Database != "appdb" || c.Username != "app_reader" {
		t.Errorf("target not taken from the URL: %+v", c)
	}
	if c.Engine != EngineMySQL {
		t.Errorf("engine = %q", c.Engine)
	}
}

// Provider dashboards spell sslmode in MySQL's vocabulary.
func TestURLAcceptsProviderSSLModeSpelling(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet,
		"https://example.test/v1/mcp/connect?type=mysql&host=h&port=25060&db=d&user=u&sslmode=REQUIRED", nil)
	r.Header.Set("X-Api-Key", "pw")

	c, err := FromRequest(r, tokenDefaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.SSLMode != "require" {
		t.Errorf("sslmode = %q, want \"require\" (REQUIRED normalised)", c.SSLMode)
	}
}

func TestEncodeTokenRoundTrips(t *testing.T) {
	dsn := "mysql://u:p@h:3306/db"
	c, err := CredentialsFromToken(EncodeToken(dsn), tokenDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "u" || c.Host != "h" {
		t.Errorf("round trip lost data: %+v", c)
	}
	// A stray trailing newline is the classic hand-rolled-base64 mistake.
	if strings.ContainsAny(EncodeToken(dsn+"\n"), "\n\r") {
		t.Error("EncodeToken must not emit newlines")
	}
}
