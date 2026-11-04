// Package e2e drives the proxy through the real MCP SSE protocol against a
// real PostgreSQL server: client -> SSE transport -> tool call -> pgx ->
// serialization matrix -> text content.
//
// It is skipped unless DBMCP_E2E_DSN points at a disposable database:
//
//	docker run -d --name dbmcp-pg -e POSTGRES_PASSWORD=secret \
//	    -e POSTGRES_DB=appdb -p 55432:5432 postgres:16-alpine
//
//	DBMCP_E2E_DSN='postgres://postgres:secret@127.0.0.1:55432/appdb?sslmode=disable' \
//	    go test ./test/... -v
//
// The target is written to, so never point this at anything real.
package e2e

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/christianjombo/go-dbmcp/internal/config"
	"github.com/christianjombo/go-dbmcp/internal/guard"
	"github.com/christianjombo/go-dbmcp/internal/httpapi"
)

const (
	jsonMaxRows = 100
	csvMaxRows  = 2000
)

// headerTransport injects the tenant's x-db-* headers onto every request the
// MCP client makes, which is exactly how a real client (Cursor, Claude
// Desktop) is configured against this proxy.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func dsnToHeaders(t *testing.T, dsn string) map[string]string {
	t.Helper()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DBMCP_E2E_DSN is not a valid URL: %v", err)
	}
	pw, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}
	return map[string]string{
		"x-db-username": u.User.Username(),
		"x-db-password": pw,
		"x-db-host":     u.Hostname(),
		"x-db-port":     port,
		"x-db-database": strings.TrimPrefix(u.Path, "/"),
		"x-db-sslmode":  sslmode,
	}
}

// connect starts the proxy in-process and returns an initialized MCP session.
func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()

	dsn := os.Getenv("DBMCP_E2E_DSN")
	if dsn == "" {
		t.Skip("set DBMCP_E2E_DSN to run end-to-end tests against a live PostgreSQL")
	}

	scanner, _ := guard.New(nil)
	deps := httpapi.Deps{
		Cfg: &config.Config{
			Version:             "e2e",
			JSONMaxRows:         jsonMaxRows,
			CSVMaxRows:          csvMaxRows,
			DefaultDatabase:     "postgres",
			DefaultSSLMode:      "prefer",
			QueryTimeout:        20 * time.Second,
			ConnectTimeout:      10 * time.Second,
			PoolMaxConns:        2,
			PoolMaxConnIdleTime: time.Minute,
			PoolMaxConnLifetime: 5 * time.Minute,
			MaxResultBytes:      32 << 20,
		},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner: scanner,
	}

	srv := httptest.NewServer(httpapi.New(deps).Handler)
	t.Cleanup(srv.Close)

	transport := &mcp.SSEClientTransport{
		Endpoint: srv.URL + httpapi.ConnectPath,
		HTTPClient: &http.Client{
			Transport: headerTransport{headers: dsnToHeaders(t, dsn)},
			Timeout:   60 * time.Second,
		},
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "1"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("MCP connect failed: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session
}

// callQuery runs execute_query and returns the text content of the result.
func callQuery(t *testing.T, s *mcp.ClientSession, sql string) (string, bool) {
	t.Helper()
	return callTool(t, s, "execute_query", map[string]any{"sql": sql})
}

func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) transport error: %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("CallTool(%s) returned no content", name)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("CallTool(%s) returned %T, want *mcp.TextContent", name, res.Content[0])
	}
	return text.Text, res.IsError
}

func TestToolsAreAdvertised(t *testing.T) {
	s := connect(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"execute_query", "list_tables", "describe_table", "ping_database"} {
		if !got[want] {
			t.Errorf("tool %q not advertised; got %v", want, got)
		}
	}
}

func TestPingDatabase(t *testing.T) {
	s := connect(t)

	text, isErr := callTool(t, s, "ping_database", nil)
	if isErr {
		t.Fatalf("ping failed: %s", text)
	}
	if !strings.Contains(text, "Connection OK") {
		t.Errorf("unexpected ping output:\n%s", text)
	}
}

// Tier 1: a small result comes back as structured JSON.
func TestTier1SmallResultIsJSON(t *testing.T) {
	s := connect(t)

	text, isErr := callQuery(t, s, "SELECT 1 AS n, 'hello' AS greeting, true AS flag, NULL AS nothing")
	if isErr {
		t.Fatalf("query failed: %s", text)
	}
	if !strings.HasPrefix(text, "Format: application/json") {
		t.Fatalf("expected a JSON tier response, got:\n%s", firstLines(text, 6))
	}
	for _, want := range []string{`"n":1`, `"greeting":"hello"`, `"flag":true`, `"nothing":null`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in:\n%s", want, text)
		}
	}
}

// Tier 2: past the JSON ceiling the payload switches to CSV.
func TestTier2MediumResultIsCSV(t *testing.T) {
	s := connect(t)

	text, isErr := callQuery(t, s,
		"SELECT g AS id, 'row-' || g AS label FROM generate_series(1, 500) g")
	if isErr {
		t.Fatalf("query failed: %s", text)
	}
	if !strings.HasPrefix(text, "Format: text/csv") {
		t.Fatalf("expected a CSV tier response, got:\n%s", firstLines(text, 6))
	}
	if !strings.Contains(text, "Rows: 500 (complete result set)") {
		t.Errorf("row count not reported correctly:\n%s", firstLines(text, 6))
	}
	if !strings.Contains(text, "id,label") {
		t.Errorf("missing CSV header row:\n%s", firstLines(text, 10))
	}
	if strings.Contains(text, "TRUNCATION WARNING") {
		t.Error("a complete 500-row result must not be marked truncated")
	}
}

// Tier 3: past the CSV ceiling the result is cut and loudly flagged.
func TestTier3LargeResultIsTruncatedAndWarned(t *testing.T) {
	s := connect(t)

	text, isErr := callQuery(t, s, "SELECT g AS id FROM generate_series(1, 5000) g")
	if isErr {
		t.Fatalf("query failed: %s", text)
	}
	if !strings.HasPrefix(text, "Format: text/csv") {
		t.Fatalf("expected CSV, got:\n%s", firstLines(text, 6))
	}
	if !strings.Contains(text, "⚠️ TRUNCATION WARNING") {
		t.Fatalf("missing truncation warning:\n%s", firstLines(text, 8))
	}
	if !strings.Contains(text, strconv.Itoa(csvMaxRows)) {
		t.Errorf("warning does not name the %d row ceiling", csvMaxRows)
	}
	// Exactly the ceiling, not one more and not one less.
	if n := strings.Count(text, "\n"); n < csvMaxRows {
		t.Errorf("payload has %d lines, expected at least the %d retained rows", n, csvMaxRows)
	}
	if strings.Contains(text, "\n2001\n") {
		t.Error("row 2001 leaked past the truncation ceiling")
	}
}

// The guardrail refuses the statement before it reaches the database.
func TestDestructiveStatementsAreBlocked(t *testing.T) {
	s := connect(t)

	for _, sql := range []string{
		"DROP DATABASE appdb",
		"VACUUM FULL",
		"TRUNCATE TABLE pg_class",
		"SELECT 1; DROP DATABASE appdb",
	} {
		text, isErr := callQuery(t, s, sql)
		if !isErr {
			t.Errorf("%q should have been refused, got:\n%s", sql, firstLines(text, 4))
			continue
		}
		if !strings.Contains(text, "BLOCKED BY SAFETY GUARDRAIL") {
			t.Errorf("%q: unexpected refusal text:\n%s", sql, text)
		}
	}
}

// A keyword appearing inside a literal is data, and must still run.
func TestKeywordInLiteralIsNotBlocked(t *testing.T) {
	s := connect(t)

	text, isErr := callQuery(t, s, "SELECT 'drop database production' AS note")
	if isErr {
		t.Fatalf("statement should have been allowed, got:\n%s", text)
	}
	if !strings.Contains(text, "drop database production") {
		t.Errorf("literal did not survive:\n%s", text)
	}
}

// NULL and the empty string must stay distinguishable through CSV.
func TestNullAndEmptyStringAreDistinctInCSV(t *testing.T) {
	s := connect(t)

	// 150 rows forces the CSV tier, where the distinction actually matters.
	text, isErr := callQuery(t, s, `
		SELECT g AS id,
		       CASE WHEN g = 1 THEN NULL WHEN g = 2 THEN '' ELSE 'x' END AS v
		  FROM generate_series(1, 150) g`)
	if isErr {
		t.Fatalf("query failed: %s", text)
	}
	if !strings.HasPrefix(text, "Format: text/csv") {
		t.Fatalf("expected CSV tier:\n%s", firstLines(text, 6))
	}
	if !strings.Contains(text, `1,\N`) {
		t.Errorf("NULL should render as \\N:\n%s", firstLines(text, 12))
	}
	if !strings.Contains(text, "2,\n") {
		t.Errorf("empty string should render as an empty field:\n%s", firstLines(text, 12))
	}
}

// A SQL error must come back as a readable tool error carrying SQLSTATE, not
// as a transport failure, so the model can correct itself.
func TestSQLErrorIsReportedWithDiagnostics(t *testing.T) {
	s := connect(t)

	text, isErr := callQuery(t, s, "SELECT * FROM definitely_not_a_table")
	if !isErr {
		t.Fatalf("expected a tool error, got:\n%s", text)
	}
	if !strings.Contains(text, "SQLSTATE") || !strings.Contains(text, "42P01") {
		t.Errorf("error should carry SQLSTATE 42P01:\n%s", text)
	}
}

func TestListTablesAndDescribeTable(t *testing.T) {
	s := connect(t)

	if text, isErr := callTool(t, s, "list_tables", map[string]any{"schema": "information_schema"}); isErr {
		t.Fatalf("list_tables failed: %s", text)
	} else if !strings.Contains(text, "information_schema") {
		t.Errorf("expected information_schema tables:\n%s", firstLines(text, 8))
	}

	text, isErr := callTool(t, s, "describe_table", map[string]any{"table": "tables", "schema": "information_schema"})
	if isErr {
		t.Fatalf("describe_table failed: %s", text)
	}
	if !strings.Contains(text, "table_name") {
		t.Errorf("expected a table_name column:\n%s", firstLines(text, 10))
	}
}

// High-precision numerics must survive: routing them through a float64 would
// silently round them.
func TestNumericPrecisionIsPreserved(t *testing.T) {
	s := connect(t)

	const want = "12345678901234567890.123456789"
	text, isErr := callQuery(t, s, "SELECT "+want+"::numeric AS amount")
	if isErr {
		t.Fatalf("query failed: %s", text)
	}
	if !strings.Contains(text, want) {
		t.Errorf("numeric lost precision; wanted %s in:\n%s", want, text)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
		return strings.Join(lines, "\n") + "\n..."
	}
	return s
}
