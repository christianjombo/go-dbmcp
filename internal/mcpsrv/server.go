// Package mcpsrv builds the per-stream MCP server: the tool surface exposed
// to the AI client, bound to exactly one tenant's database session.
package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/christianjombo/go-dbmcp/internal/config"
	"github.com/christianjombo/go-dbmcp/internal/dbconn"
	"github.com/christianjombo/go-dbmcp/internal/guard"
	"github.com/christianjombo/go-dbmcp/internal/serialize"
)

// Deps are the process-wide collaborators shared by every session.
type Deps struct {
	Cfg     *config.Config
	Log     *slog.Logger
	Scanner *guard.Scanner
}

type handlers struct {
	Deps
	sess *dbconn.Session
}

// engine is the target engine, falling back to the server default when the
// session has no credentials yet.
func (h *handlers) engine() dbconn.Engine {
	if c := h.sess.Credentials(); c != nil && c.Engine != "" {
		return c.Engine
	}
	if e, err := dbconn.ParseEngine(h.Cfg.DefaultDBType); err == nil {
		return e
	}
	return dbconn.EngineMySQL
}

// fingerprint is a log-safe session identifier, safe on an unconfigured
// session.
func (h *handlers) fingerprint() string {
	if c := h.sess.Credentials(); c != nil {
		return c.Fingerprint()
	}
	return "unconfigured"
}

// requireCredentials returns a tool error explaining how to configure the
// connector, or nil when the session is ready to use.
func (h *handlers) requireCredentials() *mcp.CallToolResult {
	if h.sess.Configured() {
		return nil
	}
	var b strings.Builder
	b.WriteString("⚙️ THIS CONNECTOR HAS NO DATABASE CREDENTIALS YET\n\n")
	if err := h.sess.ConfigError(); err != nil {
		fmt.Fprintf(&b, "What is missing: %v\n\n", err)
	}
	b.WriteString("Supply the target in any one of these ways and reconnect.\n\n" +
		"1. RECOMMENDED — target in the connector URL, password in one header:\n" +
		"     URL:    .../v1/mcp/connect?type=mysql&host=db.example.com&port=3306&db=appdb&user=app_reader\n" +
		"     Header: x-api-key: <password>\n\n" +
		"2. Entirely in one header:\n" +
		"     x-api-key: <base64 of mysql://user:password@host:3306/appdb>\n\n" +
		"3. Granular headers, for clients that allow custom names:\n" +
		"     x-db-type, x-db-username, x-db-password, x-db-host, x-db-port, x-db-database\n\n" +
		"Do not put the password in the URL: connector URLs are displayed in plaintext " +
		"and recorded in server logs.")
	return errorResult(b.String())
}

// New builds an MCP server bound to one tenant session. One of these exists
// per live SSE stream.
func New(d Deps, sess *dbconn.Session) *mcp.Server {
	h := &handlers{Deps: d, sess: sess}
	engine := h.engine()

	srv := mcp.NewServer(
		&mcp.Implementation{
			Name:    "mcp-proxy",
			Title:   "SQL MCP Proxy",
			Version: d.Cfg.Version,
		},
		&mcp.ServerOptions{Instructions: instructions(d.Cfg, engine)},
	)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "execute_query",
		Description: executeQueryDescription(d.Cfg, engine),
	}, h.executeQuery)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_tables",
		Description: "List tables and views visible to the connected role, with their schema and type. " +
			"Use this before writing a query so table names are correct.",
	}, h.listTables)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "describe_table",
		Description: "Show the columns of one table or view: name, data type, nullability and default. " +
			"Use this before writing a query so column names and types are correct.",
	}, h.describeTable)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "ping_database",
		Description: "Verify that the credentials supplied in the x-db-* request headers can reach the " +
			"database, and report the server version. Use this first when a query fails with a " +
			"connection error.",
	}, h.pingDatabase)

	return srv
}

func instructions(cfg *config.Config, engine dbconn.Engine) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Access to a %s database.\n\n", engineLabel(engine))

	b.WriteString("Results come back in one of three formats, and the first line of every response " +
		"declares which:\n")
	fmt.Fprintf(&b, "  - up to %d rows: `Format: application/json`\n", cfg.JSONMaxRows)
	fmt.Fprintf(&b, "  - up to %d rows: `Format: text/csv` (denser; the header row names the columns)\n", cfg.CSVMaxRows)
	b.WriteString("  - beyond that:   `Format: text/csv`, truncated, with an explicit TRUNCATION WARNING.\n\n")

	fmt.Fprintf(&b, "In CSV, SQL NULL is written as %s and an empty field is a genuine empty string.\n",
		serialize.NullToken)
	b.WriteString("If a response carries a truncation warning, the rows you received are a prefix of the " +
		"result set, not the whole of it — never compute totals or extremes from them.\n\n")

	fmt.Fprintf(&b, "Permitted statements: %s.\n", commandList())
	if !cfg.AllowWrites {
		b.WriteString("This server is configured read-only, so INSERT, UPDATE, DELETE, REPLACE and " +
			"MERGE are refused.\n")
	}
	b.WriteString("Schema and administrative commands (DROP, CREATE, ALTER, TRUNCATE, GRANT and the " +
		"like) are refused by a server-side guardrail.\n")

	if engine == dbconn.EngineMySQL {
		b.WriteString("\nThis is MySQL: use backticks for quoted identifiers, LIMIT n OFFSET m for " +
			"paging, and SHOW TABLES / SHOW COLUMNS for schema inspection.\n")
	} else {
		b.WriteString("\nThis is PostgreSQL: use double quotes for quoted identifiers, $1-style " +
			"placeholders, and ILIKE for case-insensitive matching.\n")
	}
	return b.String()
}

func executeQueryDescription(cfg *config.Config, engine dbconn.Engine) string {
	write := "Data-modifying statements (INSERT, UPDATE, DELETE, REPLACE, MERGE) are permitted"
	if !cfg.AllowWrites {
		write = "This server is read-only, so data-modifying statements are refused"
	}
	return fmt.Sprintf(
		"Run a single SQL statement against the connected %s database.\n\n"+
			"Pass exactly one statement; stacked statements separated by ';' are refused. "+
			"%s. Schema and administrative commands are refused.\n\n"+
			"Large results are compacted automatically: at most %d rows return as JSON, larger results "+
			"return as CSV, and anything beyond %d rows is truncated with a warning. Prefer an explicit "+
			"LIMIT, or an aggregate such as COUNT(*), when you only need a summary.",
		engineLabel(engine), write, cfg.JSONMaxRows, cfg.CSVMaxRows)
}

func engineLabel(e dbconn.Engine) string {
	if e == dbconn.EngineMySQL {
		return "MySQL/MariaDB"
	}
	return "PostgreSQL"
}

func commandList() string {
	names := make([]string, 0, len(guard.AllowedCommands))
	for k := range guard.AllowedCommands {
		names = append(names, k)
	}
	// Presentation order: read, write, then inspection.
	order := []string{"SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "REPLACE", "MERGE", "SHOW", "EXPLAIN", "CALL"}
	out := make([]string, 0, len(order))
	for _, o := range order {
		if _, ok := guard.AllowedCommands[o]; ok {
			out = append(out, o)
		}
	}
	return strings.Join(out, ", ")
}

// ---------------------------------------------------------------- tool input

// ExecuteQueryInput is the argument schema for execute_query.
type ExecuteQueryInput struct {
	SQL string `json:"sql" jsonschema:"The SQL statement to execute. Exactly one statement, with no trailing second statement."`
}

// ListTablesInput is the argument schema for list_tables.
type ListTablesInput struct {
	Schema string `json:"schema,omitempty" jsonschema:"Optional schema or database to restrict the listing to. Omit to list every non-system schema."`
}

// DescribeTableInput is the argument schema for describe_table.
type DescribeTableInput struct {
	Table  string `json:"table" jsonschema:"Name of the table or view to describe, without the schema prefix."`
	Schema string `json:"schema,omitempty" jsonschema:"Optional schema or database containing the table. Omit to search every non-system schema."`
}

// -------------------------------------------------------------- tool handlers

func (h *handlers) executeQuery(ctx context.Context, _ *mcp.CallToolRequest, in ExecuteQueryInput) (*mcp.CallToolResult, any, error) {
	if res := h.requireCredentials(); res != nil {
		return res, nil, nil
	}
	decision := h.Scanner.Inspect(in.SQL)
	if !decision.Allowed {
		h.Log.Warn("statement refused by guardrail",
			"rule", decision.RuleID,
			"command", decision.Command,
			"engine", string(h.engine()),
			"fingerprint", h.fingerprint())
		return errorResult(guard.BlockMessage(decision)), nil, nil
	}

	return h.run(ctx, dbconn.Statement{SQL: in.SQL, Command: decision.Command})
}

func (h *handlers) listTables(ctx context.Context, _ *mcp.CallToolRequest, in ListTablesInput) (*mcp.CallToolResult, any, error) {
	if res := h.requireCredentials(); res != nil {
		return res, nil, nil
	}
	return h.run(ctx, h.sess.Credentials().ListTablesStatement(in.Schema))
}

func (h *handlers) describeTable(ctx context.Context, _ *mcp.CallToolRequest, in DescribeTableInput) (*mcp.CallToolResult, any, error) {
	if res := h.requireCredentials(); res != nil {
		return res, nil, nil
	}
	if strings.TrimSpace(in.Table) == "" {
		return errorResult("The `table` argument is required and must not be empty."), nil, nil
	}
	return h.run(ctx, h.sess.Credentials().DescribeTableStatement(in.Table, in.Schema))
}

func (h *handlers) pingDatabase(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	if res := h.requireCredentials(); res != nil {
		return res, nil, nil
	}
	creds := h.sess.Credentials()

	pool, err := h.sess.Acquire(ctx)
	if err != nil {
		return errorResult(connectionMessage(err)), nil, nil
	}

	qctx, cancel := h.sess.QueryContext(ctx, h.Cfg.QueryTimeout)
	defer cancel()

	rows, err := pool.Query(qctx, creds.ServerInfoStatement())
	if err != nil {
		return errorResult(queryErrorMessage(err)), nil, nil
	}
	defer rows.Close()

	var version, database, who string
	if rows.Next() {
		v := rows.Values()
		get := func(i int) string {
			if i < len(v) && v[i] != nil {
				return string(v[i])
			}
			return "(none)"
		}
		version, database, who = get(0), get(1), get(2)
	}
	if err := rows.Err(); err != nil {
		return errorResult(queryErrorMessage(err)), nil, nil
	}

	return textResult(fmt.Sprintf(
		"✅ Connection OK\n\nEngine:   %s\nTarget:   %s\nUser:     %s\nDatabase: %s\nVersion:  %s\n",
		engineLabel(creds.Engine), creds.Target(), who, database, version)), nil, nil
}

// --------------------------------------------------------------- query engine

// run executes one statement and renders the result through the tier matrix.
// It never returns a non-nil error for a tenant-caused failure: those come
// back as tool errors so the model can read the message and correct itself,
// rather than as protocol errors that abort the call.
func (h *handlers) run(ctx context.Context, st dbconn.Statement) (*mcp.CallToolResult, any, error) {
	pool, err := h.sess.Acquire(ctx)
	if err != nil {
		return errorResult(connectionMessage(err)), nil, nil
	}

	qctx, cancel := h.sess.QueryContext(ctx, h.Cfg.QueryTimeout)
	defer cancel()

	start := time.Now()

	rows, err := pool.Query(qctx, st)
	if err != nil {
		return errorResult(queryErrorMessage(err)), nil, nil
	}
	defer rows.Close()

	cols := rows.Columns()

	// A statement with no result set (an INSERT/UPDATE/DELETE without
	// RETURNING) reports its tag and affected-row count instead of rows.
	if len(cols) == 0 {
		// Drain so the driver can report the command tag.
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			return errorResult(queryErrorMessage(err)), nil, nil
		}
		tag, affected := rows.Command()
		if tag == "" {
			tag = st.Command
		}
		return textResult(serialize.Command(tag, affected, time.Since(start)).Text), nil, nil
	}

	// Read one row beyond the CSV ceiling: that extra row is what proves more
	// data exists, without counting or buffering the remainder.
	rowLimit := h.Cfg.CSVMaxRows + 1
	collected := make([]serialize.Row, 0, min(rowLimit, 512))
	byteBudget := h.Cfg.MaxResultBytes
	used := 0
	byteCapped := false

	for rows.Next() {
		row, n := copyRow(rows.Values())
		used += n
		collected = append(collected, row)

		if used >= byteBudget {
			byteCapped = true
			break
		}
		if len(collected) >= rowLimit {
			break
		}
	}

	if err := rows.Err(); err != nil {
		return errorResult(queryErrorMessage(err)), nil, nil
	}

	result, err := serialize.Rows(cols, collected, serialize.Options{
		JSONMaxRows: h.Cfg.JSONMaxRows,
		CSVMaxRows:  h.Cfg.CSVMaxRows,
		Elapsed:     time.Since(start),
		ByteCapped:  byteCapped,
		ByteLimit:   byteBudget,
	})
	if err != nil {
		// Serialization failing is our bug, not the tenant's.
		h.Log.Error("failed to serialize result", "err", err)
		return nil, nil, fmt.Errorf("could not serialize result: %w", err)
	}

	h.Log.Debug("query complete",
		"engine", string(h.engine()),
		"command", st.Command,
		"tier", result.Tier,
		"rows", result.RowsShown,
		"truncated", result.Truncated,
		"elapsedMs", time.Since(start).Milliseconds(),
		"fingerprint", h.fingerprint())

	return textResult(result.Text), nil, nil
}

// ------------------------------------------------------------------- helpers

// copyRow detaches one row from the driver's read buffer, which the next
// call to Next may overwrite. It returns the copy and the bytes consumed.
//
// The empty-slice handling is load-bearing and easy to get wrong:
// `append([]byte(nil), v...)` returns nil when v is empty, which would turn
// every empty string into a SQL NULL by the time it reached the CSV encoder.
// make+copy always yields a non-nil slice, so NULL (nil) and ” (non-nil,
// length zero) stay distinct all the way out.
func copyRow(raw [][]byte) (serialize.Row, int) {
	row := make(serialize.Row, len(raw))
	used := 0
	for i, v := range raw {
		if v == nil {
			continue // nil stays nil: this is what distinguishes NULL from ''
		}
		cp := make([]byte, len(v))
		copy(cp, v)
		row[i] = cp
		used += len(v)
	}
	return row, used
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// connectionMessage explains a pool failure in terms the model can act on.
func connectionMessage(err error) string {
	if errors.Is(err, dbconn.ErrSessionClosed) {
		return "⚠️ The MCP stream has been disconnected and its database pool released. Reconnect and retry."
	}
	if errors.Is(err, dbconn.ErrBlockedHost) {
		return fmt.Sprintf("⛔ CONNECTION REFUSED BY HOST POLICY\n\n%v", err)
	}
	return fmt.Sprintf(
		"❌ COULD NOT CONNECT TO THE DATABASE\n\n%v\n\n"+
			"Check the x-db-type, x-db-host, x-db-port, x-db-username, x-db-password and "+
			"x-db-sslmode headers in the MCP client configuration. Run ping_database to retest.", err)
}

// queryErrorMessage surfaces the server's own diagnostics. The error code,
// hint and position are precisely what lets a model repair its own query
// instead of guessing, so they are passed through rather than flattened.
func queryErrorMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "⏱️ QUERY TIMED OUT\n\nThe statement exceeded the server's query timeout and was cancelled. " +
			"Narrow it with a WHERE clause, add a LIMIT, or aggregate instead of selecting raw rows."
	}
	if errors.Is(err, context.Canceled) {
		return "⚠️ QUERY CANCELLED\n\nThe MCP stream closed before the statement finished."
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		msg := fmt.Sprintf("❌ SQL ERROR\n\nSQLSTATE: %s\nMessage:  %s", pgErr.Code, pgErr.Message)
		if pgErr.Detail != "" {
			msg += "\nDetail:   " + pgErr.Detail
		}
		if pgErr.Hint != "" {
			msg += "\nHint:     " + pgErr.Hint
		}
		if pgErr.Position > 0 {
			msg += fmt.Sprintf("\nPosition: character %d of the statement", pgErr.Position)
		}
		return msg
	}

	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return fmt.Sprintf("❌ SQL ERROR\n\nError:   %d\nSQLSTATE: %s\nMessage: %s",
			myErr.Number, string(myErr.SQLState[:]), myErr.Message)
	}

	return fmt.Sprintf("❌ QUERY FAILED\n\n%v", err)
}
