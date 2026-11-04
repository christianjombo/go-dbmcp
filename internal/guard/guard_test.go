package guard

import (
	"strings"
	"testing"
)

func newScanner(t *testing.T) *Scanner {
	t.Helper()
	s, warnings := New(nil, true)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings from default rules: %v", warnings)
	}
	return s
}

func TestAllowedCommands(t *testing.T) {
	s := newScanner(t)

	cases := []struct {
		name string
		sql  string
	}{
		{"select", `SELECT 1`},
		{"select with clauses", `SELECT a, count(*) FROM t JOIN u ON t.id = u.id ` +
			`WHERE a > 1 GROUP BY a HAVING count(*) > 2 ORDER BY a LIMIT 10 OFFSET 5`},
		{"left join", `SELECT * FROM a LEFT JOIN b ON a.id = b.id`},
		{"cross join using", `SELECT * FROM a CROSS JOIN b`},
		{"union all", `SELECT 1 UNION ALL SELECT 2`},
		{"parenthesised union", `(SELECT 1) UNION (SELECT 2)`},
		{"intersect", `SELECT 1 INTERSECT SELECT 1`},
		{"except", `SELECT 1 EXCEPT SELECT 2`},
		{"insert", `INSERT INTO t (a, b) VALUES (1, 2)`},
		{"insert select", `INSERT INTO t (a) SELECT a FROM u`},
		{"delete", `DELETE FROM t WHERE id = 1`},
		{"replace", `REPLACE INTO t (a) VALUES (1)`},
		{"merge", `MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET t.a = s.a`},
		{"with cte", `WITH c AS (SELECT 1 AS n) SELECT * FROM c`},
		{"with writing cte", `WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d`},
		{"show", `SHOW TABLES`},
		{"explain", `EXPLAIN SELECT 1`},
		{"call", `CALL do_something(1)`},
		{"lowercase", `select * from users limit 1`},
		{"leading whitespace and newline", "\n\t  SELECT 1"},
		{"trailing semicolon", `SELECT 1;`},

		// Keywords appearing as data must not trip the scanner.
		{"denied word inside a literal", `SELECT 'drop table users' AS note`},
		{"denied word as identifier", `SELECT vacuum_log, set_count FROM audit`},
		{"denied word in a comment", "-- remember to DROP TABLE later\nSELECT 1"},
		{"denied word in a hash comment", "# DROP TABLE later\nSELECT 1"},
		{"backtick identifier", "SELECT `drop` FROM `set`"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if d := s.Inspect(tc.sql); !d.Allowed {
				t.Fatalf("expected allowed, blocked by %s: %s\nnormalised: %q",
					d.RuleID, d.Reason, Normalize(tc.sql))
			}
		})
	}
}

// The two cases a naive substring blocklist gets wrong. Both are ordinary,
// frequently used SQL that must not be refused.
func TestKeywordsAreOnlyCommandsInLeadingPosition(t *testing.T) {
	s := newScanner(t)

	t.Run("SET is a clause of UPDATE", func(t *testing.T) {
		d := s.Inspect(`UPDATE users SET name = 'bob', active = true WHERE id = 7`)
		if !d.Allowed {
			t.Fatalf("UPDATE ... SET must be allowed, blocked by %s: %s", d.RuleID, d.Reason)
		}
		if d.Command != "UPDATE" {
			t.Errorf("command = %q, want UPDATE", d.Command)
		}
	})

	t.Run("SET as a command is refused", func(t *testing.T) {
		d := s.Inspect(`SET autocommit = 0`)
		if d.Allowed {
			t.Fatal("SET as a statement must be refused")
		}
		if d.RuleID != "command_not_allowed" || d.Command != "SET" {
			t.Errorf("got rule=%q command=%q", d.RuleID, d.Command)
		}
	})

	t.Run("ANALYZE is a modifier of EXPLAIN", func(t *testing.T) {
		if d := s.Inspect(`EXPLAIN ANALYZE SELECT * FROM t`); !d.Allowed {
			t.Fatalf("EXPLAIN ANALYZE must be allowed, blocked by %s: %s", d.RuleID, d.Reason)
		}
	})

	t.Run("ANALYZE as a command is refused", func(t *testing.T) {
		d := s.Inspect(`ANALYZE users`)
		if d.Allowed || d.Command != "ANALYZE" {
			t.Fatalf("ANALYZE as a statement must be refused, got allowed=%v command=%q", d.Allowed, d.Command)
		}
	})

	t.Run("INSERT INTO is not SELECT INTO", func(t *testing.T) {
		if d := s.Inspect(`INSERT INTO archive SELECT * FROM t`); !d.Allowed {
			t.Fatalf("INSERT INTO ... SELECT must be allowed, blocked by %s: %s", d.RuleID, d.Reason)
		}
	})
}

func TestDeniedCommands(t *testing.T) {
	s := newScanner(t)

	cases := []struct{ name, sql, wantCommand string }{
		{"drop table", `DROP TABLE users`, "DROP"},
		{"drop database", `DROP DATABASE production`, "DROP"},
		{"truncate", `TRUNCATE TABLE users`, "TRUNCATE"},
		{"alter", `ALTER TABLE users ADD COLUMN x int`, "ALTER"},
		{"alter system", `ALTER SYSTEM SET work_mem = '1GB'`, "ALTER"},
		{"create", `CREATE TABLE t (id int)`, "CREATE"},
		{"rename", `RENAME TABLE a TO b`, "RENAME"},
		{"grant", `GRANT ALL ON t TO someone`, "GRANT"},
		{"revoke", `REVOKE ALL ON t FROM someone`, "REVOKE"},
		{"vacuum", `VACUUM ANALYZE users`, "VACUUM"},
		{"optimize", `OPTIMIZE TABLE users`, "OPTIMIZE"},
		{"kill", `KILL 12345`, "KILL"},
		{"shutdown", `SHUTDOWN`, "SHUTDOWN"},
		{"lowercase drop", `drop table users`, "DROP"},
		{"comment split keyword", `DROP/**/TABLE users`, "DROP"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := s.Inspect(tc.sql)
			if d.Allowed {
				t.Fatalf("expected refusal\nnormalised: %q", Normalize(tc.sql))
			}
			if d.RuleID != "command_not_allowed" {
				t.Errorf("rule = %q, want command_not_allowed", d.RuleID)
			}
			if d.Command != tc.wantCommand {
				t.Errorf("command = %q, want %q", d.Command, tc.wantCommand)
			}
		})
	}
}

// Anything not explicitly permitted is refused, so a command nobody listed
// cannot slip through by omission.
func TestUnlistedCommandsAreRefused(t *testing.T) {
	s := newScanner(t)

	for _, sql := range []string{
		`COPY t TO '/tmp/x.csv'`,
		`DO $$ BEGIN PERFORM 1; END $$`,
		`LOAD DATA INFILE '/etc/passwd' INTO TABLE t`,
		`LOCK TABLES t WRITE`,
		`REINDEX TABLE t`,
		`CLUSTER t`,
		`PREPARE s AS SELECT 1`,
		`BEGIN`,
		`COMMIT`,
	} {
		d := s.Inspect(sql)
		if d.Allowed {
			t.Errorf("%q should be refused as unlisted, got allowed", sql)
			continue
		}
		if d.RuleID != "command_not_recognised" && d.RuleID != "command_not_allowed" {
			t.Errorf("%q: rule = %q", sql, d.RuleID)
		}
	}
}

// A permitted command can still carry a forbidden construct.
func TestContextualRules(t *testing.T) {
	s := newScanner(t)

	cases := []struct{ name, sql, wantRule string }{
		{"mysql into outfile", `SELECT * FROM t INTO OUTFILE '/tmp/x'`, "into_outfile"},
		{"mysql into dumpfile", `SELECT x FROM t INTO DUMPFILE '/tmp/x'`, "into_outfile"},
		{"postgres select into", `SELECT * INTO copy_of_t FROM t`, "select_into"},
		{"mysql load_file", `SELECT load_file('/etc/passwd')`, "server_file_read"},
		{"postgres pg_read_file", `SELECT pg_read_file('/etc/passwd')`, "server_file_read"},
		{"postgres pg_ls_dir", `SELECT pg_ls_dir('/')`, "server_file_read"},
		{"large object export", `SELECT lo_export(1, '/tmp/x')`, "large_object_io"},
		{"terminate backend", `SELECT pg_terminate_backend(42)`, "backend_control"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := s.Inspect(tc.sql)
			if d.Allowed {
				t.Fatalf("expected refusal\nnormalised: %q", Normalize(tc.sql))
			}
			if d.RuleID != tc.wantRule {
				t.Errorf("rule = %q, want %q", d.RuleID, tc.wantRule)
			}
		})
	}
}

func TestStackedStatements(t *testing.T) {
	s := newScanner(t)

	if d := s.Inspect(`SELECT 1; DROP TABLE users`); d.Allowed || d.RuleID != "stacked_statements" {
		t.Errorf("got allowed=%v rule=%q", d.Allowed, d.RuleID)
	}
	if d := s.Inspect(`SELECT 1; SELECT 2`); d.Allowed || d.RuleID != "stacked_statements" {
		t.Errorf("even two benign statements must be refused: allowed=%v rule=%q", d.Allowed, d.RuleID)
	}
	// A semicolon inside a literal is data, not a separator.
	if d := s.Inspect(`SELECT 'a;b' AS v`); !d.Allowed {
		t.Errorf("semicolon in a literal must not count: %s", d.Reason)
	}
}

func TestEmptyStatements(t *testing.T) {
	s := newScanner(t)
	for _, sql := range []string{``, `   `, `-- nothing`, `/* nothing */`, "\n\t"} {
		if d := s.Inspect(sql); d.Allowed || d.RuleID != "empty_statement" {
			t.Errorf("%q: allowed=%v rule=%q", sql, d.Allowed, d.RuleID)
		}
	}
}

func TestLeadingCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT 1", "SELECT"},
		{"(SELECT 1) UNION (SELECT 2)", "SELECT"},
		{"((SELECT 1))", "SELECT"},
		{"WITH C AS () SELECT", "WITH"},
		{"UPDATE T SET X = 1", "UPDATE"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := LeadingCommand(tc.in); got != tc.want {
			t.Errorf("LeadingCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"uppercases and collapses space", "select   1", "SELECT 1"},
		{"line comment removed", "SELECT 1 -- trailing\n+ 2", "SELECT 1 + 2"},
		{"hash comment removed", "SELECT 1 # mysql style\n+ 2", "SELECT 1 + 2"},
		{"block comment removed", "SELECT /* mid */ 1", "SELECT 1"},
		{"nested block comment removed", "SELECT /* a /* b */ c */ 1", "SELECT 1"},
		{"string literal blanked", "SELECT 'secret value'", "SELECT ''"},
		{"doubled quote inside literal", "SELECT 'it''s here'", "SELECT ''"},
		{"backslash escape inside literal", `SELECT 'a\'b'`, "SELECT ''"},
		{"quoted identifier blanked", `SELECT "my col" FROM t`, `SELECT "" FROM T`},
		{"backtick identifier blanked", "SELECT `my col` FROM t", "SELECT `` FROM T"},
		{"dollar quoted body blanked", "SELECT $$ anything ; here $$", "SELECT $$"},
		{"bind placeholder preserved", "SELECT $1", "SELECT $1"},
		{"unterminated literal does not hang", "SELECT 'abc", "SELECT ''"},
		{"unterminated block comment does not hang", "SELECT /* abc", "SELECT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtraPatterns(t *testing.T) {
	s, warnings := New([]string{`\bPG_AUTHID\b`}, true)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if d := s.Inspect(`SELECT * FROM pg_authid`); d.Allowed {
		t.Fatal("expected the operator pattern to block the statement")
	}
}

func TestExtraPatternInvalidRegexDoesNotBreakScanner(t *testing.T) {
	s, warnings := New([]string{`([unclosed`}, true)
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", warnings)
	}
	if d := s.Inspect(`SELECT 1`); !d.Allowed {
		t.Fatalf("scanner must still work after a bad pattern: %s", d.Reason)
	}
	if d := s.Inspect(`DROP TABLE t`); d.Allowed {
		t.Fatal("built-in policy must survive a bad operator pattern")
	}
}

func TestBlockMessageIsActionable(t *testing.T) {
	s := newScanner(t)
	msg := BlockMessage(s.Inspect(`DROP TABLE users`))

	for _, want := range []string{"BLOCKED", "DROP", "NOT sent to the database", "Permitted commands", "SELECT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("block message missing %q:\n%s", want, msg)
		}
	}
}

// With writes withdrawn, data-modifying commands are refused while queries
// continue to work.
func TestWritesCanBeDisabled(t *testing.T) {
	s, _ := New(nil, false)

	for _, sql := range []string{
		`INSERT INTO t (a) VALUES (1)`,
		`UPDATE t SET a = 1`,
		`DELETE FROM t`,
		`REPLACE INTO t (a) VALUES (1)`,
		`MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET t.a = s.a`,
	} {
		d := s.Inspect(sql)
		if d.Allowed {
			t.Errorf("%q should be refused when writes are disabled", sql)
		} else if d.RuleID != "writes_disabled" {
			t.Errorf("%q: rule = %q, want writes_disabled", sql, d.RuleID)
		}
	}

	for _, sql := range []string{`SELECT 1`, `WITH c AS (SELECT 1) SELECT * FROM c`, `SHOW TABLES`} {
		if d := s.Inspect(sql); !d.Allowed {
			t.Errorf("%q must still be allowed: %s", sql, d.Reason)
		}
	}
}
