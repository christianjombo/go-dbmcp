package dbconn

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/christianjombo/go-dbmcp/internal/serialize"
)

// Engine identifies the target database system.
type Engine string

const (
	EnginePostgres Engine = "postgres"
	EngineMySQL    Engine = "mysql"
)

// engineAliases maps what clients may reasonably send in x-db-type onto the
// canonical engine names.
var engineAliases = map[string]Engine{
	"postgres":   EnginePostgres,
	"postgresql": EnginePostgres,
	"pg":         EnginePostgres,
	"psql":       EnginePostgres,
	"mysql":      EngineMySQL,
	"mariadb":    EngineMySQL,
}

// ParseEngine resolves an x-db-type value. The comparison is
// case-insensitive and tolerant of the common spellings.
func ParseEngine(s string) (Engine, error) {
	if e, ok := engineAliases[strings.ToLower(strings.TrimSpace(s))]; ok {
		return e, nil
	}
	return "", fmt.Errorf("%q is not a supported database type (expected one of: %s)", s, EngineNames())
}

// EngineNames lists the accepted x-db-type values, for error messages.
func EngineNames() string {
	names := make([]string, 0, len(engineAliases))
	for k := range engineAliases {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// DefaultPort is the conventional port for an engine. It is used only in
// error messages: x-db-port remains mandatory, so the server never silently
// guesses where a tenant's database lives.
func (e Engine) DefaultPort() int {
	if e == EngineMySQL {
		return 3306
	}
	return 5432
}

// String implements fmt.Stringer.
func (e Engine) String() string { return string(e) }

// Rows is one result set being streamed from a tenant database.
//
// Values are PostgreSQL/MySQL text representations, passed through verbatim.
// That is deliberate for a generic proxy: uuid, numeric, decimal, json,
// arrays, enums and extension types all render exactly as their server
// renders them, with no client-side type table to drift out of date and no
// high-precision number routed through a float64 on the way out.
type Rows interface {
	// Columns is valid as soon as the Rows exists, including for an empty
	// result set.
	Columns() []serialize.Column
	Next() bool
	// Values returns the current row. A nil element is SQL NULL, which is
	// what keeps it distinguishable from an empty string. The backing arrays
	// may be reused by the next Next, so the caller must copy to retain them.
	Values() [][]byte
	Err() error
	Close()
	// Command reports the tag and affected-row count for a statement that
	// returned no result set.
	Command() (tag string, rowsAffected int64)
}

// Statement is one SQL statement to execute.
//
// Command carries the leading keyword the guard already identified. It is
// passed down rather than re-derived because MySQL needs it: a write with no
// RETURNING clause produces an OK packet instead of a result set, and its
// affected-row count is only reachable through Exec. Re-parsing the SQL here
// would duplicate the guard's normalisation and risk the two disagreeing.
type Statement struct {
	SQL     string
	Args    []any
	Command string
}

var writeCommands = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true, "MERGE": true,
}

func (s Statement) isWrite() bool { return writeCommands[s.commandTag()] }

// commandTag returns the leading keyword, falling back to the first word of
// the statement when the caller did not supply one.
func (s Statement) commandTag() string {
	if s.Command != "" {
		return strings.ToUpper(s.Command)
	}
	trimmed := strings.TrimLeft(s.SQL, "( \t\r\n")
	if i := strings.IndexFunc(trimmed, func(r rune) bool {
		return !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	}); i > 0 {
		return strings.ToUpper(trimmed[:i])
	}
	return strings.ToUpper(trimmed)
}

// Pool is an engine-specific connection pool scoped to one MCP stream.
type Pool interface {
	Query(ctx context.Context, st Statement) (Rows, error)
	Ping(ctx context.Context) error
	Close()
}
