package dbconn

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/christianjombo/go-dbmcp/internal/serialize"
)

// mysqlDSN builds the driver DSN through mysql.Config rather than by string
// concatenation. The driver's DSN grammar is delimiter-sensitive, so a
// password containing '@', '/', ':' or '?' would otherwise restructure the
// DSN; FormatDSN escapes it correctly.
func (c *Credentials) mysqlDSN(connectTimeout time.Duration) (string, error) {
	cfg := mysql.NewConfig()
	cfg.User = c.Username
	cfg.Passwd = c.Password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	cfg.DBName = c.Database
	cfg.TLSConfig = c.mysqlTLS()
	cfg.Timeout = connectTimeout
	cfg.AllowNativePasswords = true

	// Off deliberately: zone-less DATE/DATETIME values stay as the server
	// rendered them instead of being reinterpreted in the process timezone.
	cfg.ParseTime = false

	// MultiStatements stays false. Enabling it would let a single call carry
	// a second, hidden statement past the guard's stacked-statement check.
	cfg.MultiStatements = false

	// AllowAllFiles stays false so LOAD DATA LOCAL INFILE cannot be used to
	// read files from this proxy's own filesystem.
	cfg.AllowAllFiles = false

	if _, err := mysql.ParseDSN(cfg.FormatDSN()); err != nil {
		return "", fmt.Errorf("invalid connection parameters for %s", c.Target())
	}
	return cfg.FormatDSN(), nil
}

func mysqlKind(dbType string) serialize.Kind {
	switch strings.ToUpper(dbType) {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT",
		"UNSIGNED TINYINT", "UNSIGNED SMALLINT", "UNSIGNED MEDIUMINT",
		"UNSIGNED INT", "UNSIGNED BIGINT", "FLOAT", "DOUBLE", "DECIMAL", "YEAR":
		return serialize.KindNumber
	case "JSON":
		return serialize.KindJSON
	default:
		// DATE/DATETIME/TIMESTAMP arrive as text because ParseTime is off,
		// which keeps the server's own rendering rather than reinterpreting
		// a zone-less value in the process timezone.
		return serialize.KindText
	}
}

type myPool struct {
	db *sql.DB
}

func openMySQL(ctx context.Context, c *Credentials, cfg SessionConfig) (Pool, error) {
	dsn, err := c.mysqlDSN(cfg.ConnectTimeout)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		// sql.Open with a bad DSN can echo it, and it holds the password.
		return nil, fmt.Errorf("invalid connection parameters for %s", c.Target())
	}
	db.SetMaxOpenConns(int(cfg.MaxConns))
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(cfg.MaxConnIdleTime)
	db.SetConnMaxLifetime(cfg.MaxConnLifetime)

	return &myPool{db: db}, nil
}

func (p *myPool) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }
func (p *myPool) Close()                         { _ = p.db.Close() }

func (p *myPool) Query(ctx context.Context, st Statement) (Rows, error) {
	// MySQL has no universal RETURNING, so a write statement produces an OK
	// packet rather than a result set and its affected-row count is only
	// reachable through Exec. MariaDB's INSERT/DELETE ... RETURNING does
	// produce rows, so fall back to Query whenever RETURNING is present.
	if st.isWrite() && !containsFold(st.SQL, "RETURNING") {
		res, err := p.db.ExecContext(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		return &commandRows{tag: st.commandTag(), affected: affected}, nil
	}

	rows, err := p.db.QueryContext(ctx, st.SQL, st.Args...)
	if err != nil {
		return nil, err
	}

	types, err := rows.ColumnTypes()
	if err != nil {
		_ = rows.Close()
		return nil, err
	}

	cols := make([]serialize.Column, len(types))
	for i, ct := range types {
		cols[i] = serialize.Column{
			Name:     ct.Name(),
			TypeName: strings.ToLower(ct.DatabaseTypeName()),
			Kind:     mysqlKind(ct.DatabaseTypeName()),
		}
	}

	// Scanning into *any rather than *sql.RawBytes is deliberate. RawBytes
	// cannot distinguish SQL NULL from an empty string — both can surface as
	// a zero-length slice — and that ambiguity would silently corrupt any
	// downstream aggregation. With *any, NULL is unambiguously nil, and
	// database/sql clones the driver's buffer so the values stay valid.
	scan := make([]any, len(cols))
	holders := make([]any, len(cols))
	for i := range holders {
		scan[i] = &holders[i]
	}

	return &myRows{rows: rows, cols: cols, scan: scan, holders: holders,
		current: make([][]byte, len(cols))}, nil
}

type myRows struct {
	rows    *sql.Rows
	cols    []serialize.Column
	scan    []any
	holders []any
	current [][]byte
	err     error
}

func (r *myRows) Columns() []serialize.Column { return r.cols }
func (r *myRows) Values() [][]byte            { return r.current }
func (r *myRows) Close()                      { _ = r.rows.Close() }
func (r *myRows) Command() (string, int64)    { return "", 0 }

func (r *myRows) Next() bool {
	if r.err != nil || !r.rows.Next() {
		return false
	}
	if err := r.rows.Scan(r.scan...); err != nil {
		r.err = err
		return false
	}
	for i := range r.holders {
		r.current[i] = mysqlValueToText(r.holders[i])
	}
	return true
}

func (r *myRows) Err() error {
	if r.err != nil {
		return r.err
	}
	return r.rows.Err()
}

// mysqlValueToText renders a driver value as its text representation. nil is
// returned only for SQL NULL; an empty string yields a non-nil empty slice so
// the two stay distinguishable all the way to the CSV encoder.
func mysqlValueToText(v any) []byte {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		if t == nil {
			return []byte{}
		}
		return t
	case string:
		return []byte(t)
	case int64:
		return []byte(strconv.FormatInt(t, 10))
	case uint64:
		return []byte(strconv.FormatUint(t, 10))
	case float32:
		return []byte(strconv.FormatFloat(float64(t), 'g', -1, 32))
	case float64:
		return []byte(strconv.FormatFloat(t, 'g', -1, 64))
	case bool:
		if t {
			return []byte("1")
		}
		return []byte("0")
	case time.Time:
		return []byte(t.Format(time.RFC3339Nano))
	default:
		return []byte(fmt.Sprint(t))
	}
}

// commandRows represents a statement that returned no result set.
type commandRows struct {
	tag      string
	affected int64
}

func (c *commandRows) Columns() []serialize.Column { return nil }
func (c *commandRows) Next() bool                  { return false }
func (c *commandRows) Values() [][]byte            { return nil }
func (c *commandRows) Err() error                  { return nil }
func (c *commandRows) Close()                      {}
func (c *commandRows) Command() (string, int64)    { return c.tag, c.affected }

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToUpper(haystack), needle)
}
