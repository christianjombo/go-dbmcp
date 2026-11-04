package dbconn

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestParseEngine(t *testing.T) {
	for _, in := range []string{"postgres", "PostgreSQL", "pg", " PSQL "} {
		got, err := ParseEngine(in)
		if err != nil || got != EnginePostgres {
			t.Errorf("ParseEngine(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"mysql", "MySQL", "mariadb", "MariaDB"} {
		got, err := ParseEngine(in)
		if err != nil || got != EngineMySQL {
			t.Errorf("ParseEngine(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "oracle", "sqlite", "mongo"} {
		if _, err := ParseEngine(in); err == nil {
			t.Errorf("ParseEngine(%q) should have failed", in)
		}
	}
}

func TestEngineSelectedByHeader(t *testing.T) {
	d := Defaults{Engine: EnginePostgres, Database: "postgres", SSLMode: "prefer"}

	hdr := validHeaders()
	hdr["x-db-type"] = "mysql"
	hdr["x-db-port"] = "3306"
	c, err := FromHeaders(headers(hdr), d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Engine != EngineMySQL {
		t.Errorf("engine = %q, want mysql", c.Engine)
	}
}

// An existing PostgreSQL-only client that sends no x-db-type must keep
// working against the server default.
func TestEngineFallsBackToServerDefault(t *testing.T) {
	c, err := FromHeaders(headers(validHeaders()),
		Defaults{Engine: EngineMySQL, Database: "db", SSLMode: "prefer"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Engine != EngineMySQL {
		t.Errorf("engine = %q, want the configured default mysql", c.Engine)
	}

	// And with no default configured at all, postgres.
	c2, err := FromHeaders(headers(validHeaders()), Defaults{Database: "db", SSLMode: "prefer"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c2.Engine != EnginePostgres {
		t.Errorf("engine = %q, want postgres", c2.Engine)
	}
}

func TestUnsupportedEngineIsRejected(t *testing.T) {
	hdr := validHeaders()
	hdr["x-db-type"] = "oracle"

	_, err := FromHeaders(headers(hdr), defaults())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if _, ok := ve.Invalid["x-db-type"]; !ok {
		t.Errorf("expected x-db-type to be flagged, got %v", ve.Invalid)
	}
}

func TestDefaultPorts(t *testing.T) {
	if got := EnginePostgres.DefaultPort(); got != 5432 {
		t.Errorf("postgres default port = %d", got)
	}
	if got := EngineMySQL.DefaultPort(); got != 3306 {
		t.Errorf("mysql default port = %d", got)
	}
}

// The MySQL DSN grammar is delimiter-sensitive, so a password containing its
// delimiters must not be able to restructure the DSN.
func TestMySQLDSNEscapesCredentials(t *testing.T) {
	c := &Credentials{
		Engine:   EngineMySQL,
		Username: "app_user",
		Password: `p@ss:w/rd?#&=x(tcp)`,
		Host:     "db.example.com",
		Port:     3306,
		Database: "appdb",
		SSLMode:  "require",
	}

	dsn, err := c.mysqlDSN(5 * time.Second)
	if err != nil {
		t.Fatalf("mysqlDSN: %v", err)
	}

	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("driver cannot parse its own DSN: %v", err)
	}
	if parsed.User != c.Username {
		t.Errorf("user round-trip = %q", parsed.User)
	}
	if parsed.Passwd != c.Password {
		t.Errorf("password round-trip = %q, want %q", parsed.Passwd, c.Password)
	}
	if parsed.Addr != "db.example.com:3306" {
		t.Errorf("addr = %q — credentials leaked into the address", parsed.Addr)
	}
	if parsed.DBName != "appdb" {
		t.Errorf("dbname = %q", parsed.DBName)
	}
}

// Enabling these would defeat the guard's stacked-statement check and expose
// the proxy's own filesystem, so they must stay off.
func TestMySQLDSNKeepsDangerousOptionsOff(t *testing.T) {
	c := &Credentials{
		Engine: EngineMySQL, Username: "u", Password: "p",
		Host: "h", Port: 3306, Database: "d", SSLMode: "prefer",
	}
	dsn, err := c.mysqlDSN(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.MultiStatements {
		t.Error("MultiStatements must stay off: it would let a second statement past the guard")
	}
	if parsed.AllowAllFiles {
		t.Error("AllowAllFiles must stay off: LOAD DATA LOCAL INFILE could read this proxy's files")
	}
	if parsed.ParseTime {
		t.Error("ParseTime must stay off so zone-less values keep the server's rendering")
	}
}

func TestMySQLTLSMapping(t *testing.T) {
	cases := map[string]string{
		"disable":     "false",
		"allow":       "preferred",
		"prefer":      "preferred",
		"require":     "skip-verify",
		"verify-ca":   "true",
		"verify-full": "true",
	}
	for sslmode, want := range cases {
		c := &Credentials{Engine: EngineMySQL, SSLMode: sslmode}
		if got := c.mysqlTLS(); got != want {
			t.Errorf("sslmode %q -> %q, want %q", sslmode, got, want)
		}
	}
}

// Two tenants on the same host/port/database but different engines are
// different targets and must not share a log fingerprint.
func TestFingerprintDistinguishesEngines(t *testing.T) {
	pg := &Credentials{Engine: EnginePostgres, Username: "u", Host: "h", Port: 1, Database: "d"}
	my := &Credentials{Engine: EngineMySQL, Username: "u", Host: "h", Port: 1, Database: "d"}
	if pg.Fingerprint() == my.Fingerprint() {
		t.Error("fingerprint must distinguish engines")
	}
}

func TestIntrospectionStatementsArePerEngine(t *testing.T) {
	pg := &Credentials{Engine: EnginePostgres}
	my := &Credentials{Engine: EngineMySQL}

	pgList := pg.ListTablesStatement("public")
	if !strings.Contains(pgList.SQL, "$1") {
		t.Errorf("postgres should use $1 placeholders:\n%s", pgList.SQL)
	}
	if len(pgList.Args) != 1 {
		t.Errorf("postgres list args = %d, want 1", len(pgList.Args))
	}

	myList := my.ListTablesStatement("appdb")
	if !strings.Contains(myList.SQL, "?") {
		t.Errorf("mysql should use ? placeholders:\n%s", myList.SQL)
	}
	if len(myList.Args) != 2 {
		t.Errorf("mysql list args = %d, want 2 (the placeholder appears twice)", len(myList.Args))
	}

	// Reserved words need the engine's own quoting or the query is a syntax error.
	myDesc := my.DescribeTableStatement("t", "")
	if !strings.Contains(myDesc.SQL, "`table`") || !strings.Contains(myDesc.SQL, "`default`") {
		t.Errorf("mysql describe must backtick reserved words:\n%s", myDesc.SQL)
	}
	pgDesc := pg.DescribeTableStatement("t", "")
	if !strings.Contains(pgDesc.SQL, `"table"`) || !strings.Contains(pgDesc.SQL, `"default"`) {
		t.Errorf("postgres describe must double-quote reserved words:\n%s", pgDesc.SQL)
	}
}

// A blank schema must become a SQL NULL so the "all schemas" branch of the
// introspection queries is taken, rather than matching a schema named "".
func TestIntrospectionOmittedSchemaBecomesNil(t *testing.T) {
	st := (&Credentials{Engine: EnginePostgres}).ListTablesStatement("")
	if st.Args[0] != nil {
		t.Errorf("omitted schema should be nil, got %#v", st.Args[0])
	}
}

func TestStatementWriteDetection(t *testing.T) {
	cases := map[string]bool{
		"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true, "MERGE": true,
		"SELECT": false, "WITH": false, "SHOW": false, "EXPLAIN": false, "CALL": false,
	}
	for cmd, want := range cases {
		if got := (Statement{Command: cmd}).isWrite(); got != want {
			t.Errorf("isWrite(%s) = %v, want %v", cmd, got, want)
		}
	}

	// With no Command supplied, it is derived from the statement text.
	if !(Statement{SQL: "insert into t values (1)"}).isWrite() {
		t.Error("should derive INSERT from the SQL when Command is empty")
	}
	if (Statement{SQL: "  (select 1)"}).isWrite() {
		t.Error("a parenthesised SELECT is not a write")
	}
}

func TestMySQLValueToTextDistinguishesNullFromEmpty(t *testing.T) {
	if mysqlValueToText(nil) != nil {
		t.Error("SQL NULL must map to nil")
	}
	empty := mysqlValueToText([]byte{})
	if empty == nil {
		t.Error("an empty string must stay non-nil, or it becomes indistinguishable from NULL")
	}
	if len(empty) != 0 {
		t.Errorf("empty string rendered as %q", empty)
	}
	// A nil []byte inside a non-nil interface is still a value, not NULL.
	var nilBytes []byte
	if got := mysqlValueToText(nilBytes); got == nil {
		t.Error("a typed nil []byte must not be reported as SQL NULL")
	}
}

func TestMySQLValueToTextFormatsScalars(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{int64(-42), "-42"},
		{uint64(18446744073709551615), "18446744073709551615"},
		{float64(1.5), "1.5"},
		{true, "1"},
		{false, "0"},
		{"text", "text"},
		{[]byte("bytes"), "bytes"},
	}
	for _, tc := range cases {
		if got := string(mysqlValueToText(tc.in)); got != tc.want {
			t.Errorf("mysqlValueToText(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// PostgreSQL always has a database called "postgres"; MySQL has no equivalent
// always-present database, so the right default there is to select none.
func TestEngineDefaultDatabase(t *testing.T) {
	if got := EnginePostgres.DefaultDatabase(); got != "postgres" {
		t.Errorf("postgres default database = %q, want \"postgres\"", got)
	}
	if got := EngineMySQL.DefaultDatabase(); got != "" {
		t.Errorf("mysql default database = %q, want empty (no database selected)", got)
	}
}

// With no operator default configured, the database must follow the engine
// rather than inheriting a literal that is wrong for one of them.
func TestDefaultDatabaseFollowsEngine(t *testing.T) {
	d := Defaults{Engine: EngineMySQL, SSLMode: "prefer"} // Database intentionally empty

	hdr := validHeaders()
	hdr["x-db-port"] = "3306"
	c, err := FromHeaders(headers(hdr), d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Database != "" {
		t.Errorf("mysql database = %q, want empty", c.Database)
	}

	hdr["x-db-type"] = "postgres"
	c2, err := FromHeaders(headers(hdr), d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c2.Database != "postgres" {
		t.Errorf("postgres database = %q, want \"postgres\"", c2.Database)
	}
}

// An explicit operator default still wins over the engine convention.
func TestOperatorDefaultDatabaseWins(t *testing.T) {
	d := Defaults{Engine: EngineMySQL, Database: "appdb", SSLMode: "prefer"}
	c, err := FromHeaders(headers(validHeaders()), d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Database != "appdb" {
		t.Errorf("database = %q, want \"appdb\"", c.Database)
	}
}

// A MySQL target with no database selected must still render readably.
func TestTargetHandlesNoDatabase(t *testing.T) {
	c := &Credentials{Engine: EngineMySQL, Host: "h", Port: 3306}
	if got := c.Target(); got != "mysql://h:3306/(none)" {
		t.Errorf("Target() = %q", got)
	}
}
