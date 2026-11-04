package dbconn

// Introspection statements, per engine.
//
// Both engines expose information_schema, but they differ in two ways that
// matter: the placeholder syntax ($1 vs ?) and which identifiers need
// quoting — `table`, `column` and `default` are reserved words, and the
// quoting character is not the same. Writing the statements out per engine is
// clearer than building them from a shared template with substitutions.

// ListTablesStatement lists tables and views visible to the connected role.
func (c *Credentials) ListTablesStatement(schema string) Statement {
	var s any
	if schema != "" {
		s = schema
	}

	if c.Engine == EngineMySQL {
		return Statement{
			Command: "SELECT",
			SQL: "SELECT table_schema AS `schema`, table_name AS `name`, " +
				"LOWER(table_type) AS `kind`, table_rows AS `approx_rows` " +
				"FROM information_schema.tables " +
				"WHERE table_schema NOT IN ('mysql','information_schema','performance_schema','sys') " +
				"AND (? IS NULL OR table_schema = ?) " +
				"ORDER BY 1, 2",
			Args: []any{s, s},
		}
	}

	return Statement{
		Command: "SELECT",
		SQL: `SELECT table_schema AS schema,
		             table_name   AS name,
		             lower(table_type) AS kind
		        FROM information_schema.tables
		       WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		         AND ($1::text IS NULL OR table_schema = $1::text)
		       ORDER BY 1, 2`,
		Args: []any{s},
	}
}

// DescribeTableStatement returns the columns of one table or view.
func (c *Credentials) DescribeTableStatement(table, schema string) Statement {
	var s any
	if schema != "" {
		s = schema
	}

	if c.Engine == EngineMySQL {
		return Statement{
			Command: "SELECT",
			SQL: "SELECT table_schema AS `schema`, table_name AS `table`, " +
				"column_name AS `column`, column_type AS `type`, " +
				"is_nullable AS `nullable`, column_default AS `default`, " +
				"column_key AS `key`, column_comment AS `comment` " +
				"FROM information_schema.columns " +
				"WHERE table_name = ? AND (? IS NULL OR table_schema = ?) " +
				"ORDER BY table_schema, ordinal_position",
			Args: []any{table, s, s},
		}
	}

	return Statement{
		Command: "SELECT",
		SQL: `SELECT table_schema   AS schema,
		             table_name     AS "table",
		             column_name    AS column,
		             data_type      AS type,
		             is_nullable    AS nullable,
		             column_default AS "default"
		        FROM information_schema.columns
		       WHERE table_name = $1::text
		         AND ($2::text IS NULL OR table_schema = $2::text)
		       ORDER BY table_schema, ordinal_position`,
		Args: []any{table, s},
	}
}

// ServerInfoStatement reports version, current database and current user.
func (c *Credentials) ServerInfoStatement() Statement {
	if c.Engine == EngineMySQL {
		return Statement{
			Command: "SELECT",
			SQL:     "SELECT version() AS version, database() AS db, user() AS who",
		}
	}
	return Statement{
		Command: "SELECT",
		SQL:     "SELECT version() AS version, current_database() AS db, current_user AS who",
	}
}
