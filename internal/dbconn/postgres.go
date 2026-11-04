package dbconn

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/christianjombo/go-dbmcp/internal/serialize"
)

// pgTypeNames resolves PostgreSQL OIDs to readable type names. Written once
// at init and only read afterwards, so it is safe to share across sessions.
var pgTypeNames = pgtype.NewMap()

// PostgreSQL type OIDs that can be rendered as native JSON values.
const (
	oidBool    uint32 = 16
	oidInt8    uint32 = 20
	oidInt2    uint32 = 21
	oidInt4    uint32 = 23
	oidJSON    uint32 = 114
	oidFloat4  uint32 = 700
	oidFloat8  uint32 = 701
	oidNumeric uint32 = 1700
	oidJSONB   uint32 = 3802
)

func pgKind(oid uint32) serialize.Kind {
	switch oid {
	case oidBool:
		return serialize.KindBool
	case oidInt2, oidInt4, oidInt8, oidFloat4, oidFloat8, oidNumeric:
		return serialize.KindNumber
	case oidJSON, oidJSONB:
		return serialize.KindJSON
	default:
		return serialize.KindText
	}
}

type pgPool struct {
	pool *pgxpool.Pool
}

func openPostgres(ctx context.Context, c *Credentials, cfg SessionConfig) (Pool, error) {
	pc, err := pgxpool.ParseConfig(c.postgresDSN())
	if err != nil {
		// Cannot surface err verbatim: pgx echoes the connection string,
		// which contains the tenant's password.
		return nil, fmt.Errorf("invalid connection parameters for %s", c.Target())
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = 0
	pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	pc.MaxConnLifetime = cfg.MaxConnLifetime
	pc.HealthCheckPeriod = 30 * time.Second
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, err
	}
	return &pgPool{pool: pool}, nil
}

func (p *pgPool) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }
func (p *pgPool) Close()                         { p.pool.Close() }

// Query always uses the extended query path. Unlike MySQL, PostgreSQL
// reports affected rows in the command tag of an empty result set, and
// supports RETURNING, so writes need no special handling here.
func (p *pgPool) Query(ctx context.Context, st Statement) (Rows, error) {
	// pgx.QueryResultFormats forces every column into PostgreSQL's text
	// representation, which is what gives this proxy type fidelity without a
	// client-side type table.
	callArgs := make([]any, 0, len(st.Args)+1)
	callArgs = append(callArgs, pgx.QueryResultFormats{pgx.TextFormatCode})
	callArgs = append(callArgs, st.Args...)

	rows, err := p.pool.Query(ctx, st.SQL, callArgs...)
	if err != nil {
		return nil, err
	}

	fds := rows.FieldDescriptions()
	cols := make([]serialize.Column, len(fds))
	for i, fd := range fds {
		name := ""
		if t, ok := pgTypeNames.TypeForOID(fd.DataTypeOID); ok {
			name = t.Name
		}
		cols[i] = serialize.Column{Name: fd.Name, TypeName: name, Kind: pgKind(fd.DataTypeOID)}
	}

	return &pgRows{rows: rows, cols: cols}, nil
}

type pgRows struct {
	rows pgx.Rows
	cols []serialize.Column
}

func (r *pgRows) Columns() []serialize.Column { return r.cols }
func (r *pgRows) Next() bool                  { return r.rows.Next() }
func (r *pgRows) Values() [][]byte            { return r.rows.RawValues() }
func (r *pgRows) Err() error                  { return r.rows.Err() }
func (r *pgRows) Close()                      { r.rows.Close() }

func (r *pgRows) Command() (string, int64) {
	tag := r.rows.CommandTag()
	return tag.String(), tag.RowsAffected()
}
