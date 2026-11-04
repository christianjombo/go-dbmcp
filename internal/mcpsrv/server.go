// Package mcpsrv builds the per-stream MCP server: the tool surface exposed
// to the AI client, bound to exactly one tenant's database session.
package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/christianjombo/go-dbmcp/internal/config"
	"github.com/christianjombo/go-dbmcp/internal/dbconn"
	"github.com/christianjombo/go-dbmcp/internal/guard"
	"github.com/christianjombo/go-dbmcp/internal/serialize"
)

// typeNames resolves PostgreSQL OIDs to readable type names for the result
// metadata block. It is written once at init and only read afterwards, so it
// is safe to share across every concurrent session.
var typeNames = pgtype.NewMap()

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

// New builds an MCP server bound to one tenant session. One of these exists
// per live SSE stream.
func New(d Deps, sess *dbconn.Session) *mcp.Server {
	h := &handlers{Deps: d, sess: sess}

	srv := mcp.NewServer(
		&mcp.Implementation{
			Name:    "mcp-proxy",
			Title:   "PostgreSQL MCP Proxy",
			Version: d.Cfg.Version,
		},
		&mcp.ServerOptions{
			Instructions: fmt.Sprintf(
				"Read-oriented access to a PostgreSQL database.\n\n"+
					"Results are returned in one of three formats, and the first line of every "+
					"response declares which:\n"+
					"  - up to %d rows: `Format: application/json`\n"+
					"  - up to %d rows: `Format: text/csv` (denser; the header row names the columns)\n"+
					"  - beyond that:   `Format: text/csv`, truncated, with an explicit TRUNCATION WARNING.\n\n"+
					"In CSV, SQL NULL is written as %s and an empty field is a genuine empty string.\n"+
					"If a response carries a truncation warning, the rows you received are a prefix of the "+
					"result set, not the whole of it — never compute totals or extremes from them.\n"+
					"Destructive and maintenance statements are refused by a server-side guardrail.",
				d.Cfg.JSONMaxRows, d.Cfg.CSVMaxRows, serialize.NullToken),
		},
	)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "execute_query",
		Description: fmt.Sprintf(
			"Run a single read-only SQL statement against the connected PostgreSQL database and "+
				"return the rows.\n\n"+
				"Pass exactly one statement; stacked statements separated by ';' are refused. "+
				"Destructive and maintenance commands (DROP DATABASE, TRUNCATE, VACUUM, ALTER SYSTEM "+
				"and similar) are refused before reaching the database.\n\n"+
				"Large results are automatically compacted: at most %d rows come back as JSON, larger "+
				"results come back as CSV, and anything beyond %d rows is truncated with a warning. "+
				"Prefer an explicit LIMIT, or an aggregate such as COUNT(*), when you only need a summary.",
			d.Cfg.JSONMaxRows, d.Cfg.CSVMaxRows),
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
		Description: "Verify that the credentials supplied in the x-db-* request headers can actually " +
			"reach the database, and report the server version. Use this first when a query fails " +
			"with a connection error.",
	}, h.pingDatabase)

	return srv
}

// ---------------------------------------------------------------- tool input

// ExecuteQueryInput is the argument schema for execute_query.
type ExecuteQueryInput struct {
	SQL string `json:"sql" jsonschema:"The SQL statement to execute. Exactly one statement, no trailing second statement."`
}

// ListTablesInput is the argument schema for list_tables.
type ListTablesInput struct {
	Schema string `json:"schema,omitempty" jsonschema:"Optional schema to restrict the listing to. Omit to list every non-system schema."`
}

// DescribeTableInput is the argument schema for describe_table.
type DescribeTableInput struct {
	Table  string `json:"table" jsonschema:"Name of the table or view to describe, without the schema prefix."`
	Schema string `json:"schema,omitempty" jsonschema:"Optional schema containing the table. Omit to search every non-system schema."`
}

// -------------------------------------------------------------- tool handlers

func (h *handlers) executeQuery(ctx context.Context, _ *mcp.CallToolRequest, in ExecuteQueryInput) (*mcp.CallToolResult, any, error) {
	if decision := h.Scanner.Inspect(in.SQL); !decision.Allowed {
		h.Log.Warn("statement refused by guardrail",
			"rule", decision.RuleID,
			"fingerprint", h.sess.Credentials().Fingerprint())
		return errorResult(guard.BlockMessage(decision)), nil, nil
	}
	return h.run(ctx, in.SQL, nil)
}

func (h *handlers) listTables(ctx context.Context, _ *mcp.CallToolRequest, in ListTablesInput) (*mcp.CallToolResult, any, error) {
	const q = `
		SELECT table_schema AS schema,
		       table_name   AS name,
		       lower(table_type) AS kind
		  FROM information_schema.tables
		 WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		   AND ($1::text IS NULL OR table_schema = $1::text)
		 ORDER BY 1, 2`
	return h.run(ctx, q, []any{nullIfEmpty(in.Schema)})
}

func (h *handlers) describeTable(ctx context.Context, _ *mcp.CallToolRequest, in DescribeTableInput) (*mcp.CallToolResult, any, error) {
	if in.Table == "" {
		return errorResult("The `table` argument is required and must not be empty."), nil, nil
	}
	const q = `
		SELECT table_schema    AS schema,
		       table_name      AS "table",
		       column_name     AS column,
		       data_type       AS type,
		       is_nullable     AS nullable,
		       column_default  AS "default"
		  FROM information_schema.columns
		 WHERE table_name = $1::text
		   AND ($2::text IS NULL OR table_schema = $2::text)
		 ORDER BY table_schema, ordinal_position`
	return h.run(ctx, q, []any{in.Table, nullIfEmpty(in.Schema)})
}

func (h *handlers) pingDatabase(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	pool, err := h.sess.Acquire(ctx)
	if err != nil {
		return errorResult(connectionMessage(err)), nil, nil
	}

	qctx, cancel := h.sess.QueryContext(ctx, h.Cfg.QueryTimeout)
	defer cancel()

	var version, database, user string
	err = pool.QueryRow(qctx, "SELECT version(), current_database(), current_user").
		Scan(&version, &database, &user)
	if err != nil {
		return errorResult(queryErrorMessage(err)), nil, nil
	}

	return textResult(fmt.Sprintf(
		"✅ Connection OK\n\nTarget:   %s\nUser:     %s\nDatabase: %s\nVersion:  %s\n",
		h.sess.Credentials().Target(), user, database, version)), nil, nil
}

// --------------------------------------------------------------- query engine

// querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// run executes one statement and renders the result through the tier matrix.
// It never returns a non-nil error for a tenant-caused failure: those come
// back as tool errors so the model can read the message and correct itself,
// rather than as protocol errors that abort the call.
func (h *handlers) run(ctx context.Context, sqlText string, args []any) (*mcp.CallToolResult, any, error) {
	pool, err := h.sess.Acquire(ctx)
	if err != nil {
		return errorResult(connectionMessage(err)), nil, nil
	}

	qctx, cancel := h.sess.QueryContext(ctx, h.Cfg.QueryTimeout)
	defer cancel()

	var q querier = pool

	if h.Cfg.ReadOnlyTx {
		tx, err := pool.BeginTx(qctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			return errorResult(queryErrorMessage(err)), nil, nil
		}
		defer func() {
			// The query context may already be cancelled or past its
			// deadline; roll back on a detached one so the transaction is
			// always released rather than left for the pool to reap.
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer rcancel()
			_ = tx.Rollback(rctx)
		}()
		q = tx
	}

	start := time.Now()

	// pgx.QueryResultFormats forces every column into PostgreSQL's text
	// representation. For a generic proxy that is exactly what we want: uuid,
	// numeric, arrays, ranges, enums and extension types all render the way
	// the server renders them, with no client-side type table to maintain and
	// no numeric silently routed through a float64.
	callArgs := make([]any, 0, len(args)+1)
	callArgs = append(callArgs, pgx.QueryResultFormats{pgx.TextFormatCode})
	callArgs = append(callArgs, args...)

	rows, err := q.Query(qctx, sqlText, callArgs...)
	if err != nil {
		return errorResult(queryErrorMessage(err)), nil, nil
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	cols := make([]serialize.Column, len(fds))
	for i, fd := range fds {
		cols[i] = serialize.Column{
			Name:     fd.Name,
			OID:      fd.DataTypeOID,
			TypeName: typeNameFor(fd.DataTypeOID),
		}
	}

	// Read one row beyond the CSV ceiling: that extra row is what proves more
	// data exists, without counting or buffering the remainder.
	rowLimit := h.Cfg.CSVMaxRows + 1
	collected := make([]serialize.Row, 0, min(rowLimit, 512))
	byteBudget := h.Cfg.MaxResultBytes
	used := 0
	byteCapped := false

	for rows.Next() {
		raw := rows.RawValues()
		// RawValues aliases the connection's read buffer and is invalidated
		// by the next Next(), so every non-NULL value must be copied.
		row := make(serialize.Row, len(raw))
		for i, v := range raw {
			if v == nil {
				continue // nil stays nil: this is what distinguishes NULL from ''
			}
			row[i] = append([]byte(nil), v...)
			used += len(v)
		}
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

	// A statement with no result set (INSERT/UPDATE/DDL) reports its tag.
	if len(cols) == 0 {
		tag := rows.CommandTag()
		rows.Close()
		return textResult(serialize.Command(tag.String(), tag.RowsAffected(), time.Since(start)).Text), nil, nil
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
		"tier", result.Tier,
		"rows", result.RowsShown,
		"truncated", result.Truncated,
		"elapsedMs", time.Since(start).Milliseconds(),
		"fingerprint", h.sess.Credentials().Fingerprint())

	return textResult(result.Text), nil, nil
}

// ------------------------------------------------------------------- helpers

func typeNameFor(oid uint32) string {
	if t, ok := typeNames.TypeForOID(oid); ok {
		return t.Name
	}
	return ""
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
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
			"Check the x-db-host, x-db-port, x-db-username, x-db-password and x-db-sslmode "+
			"headers in the MCP client configuration. Run ping_database to retest.", err)
}

// queryErrorMessage surfaces PostgreSQL's own diagnostics. SQLSTATE, hint and
// position are precisely what lets a model repair its own query instead of
// guessing, so they are passed through rather than flattened.
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

	return fmt.Sprintf("❌ QUERY FAILED\n\n%v", err)
}
