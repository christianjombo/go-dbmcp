package synthetic

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func fixture() *Interceptor {
	return New(Config{
		Enabled:    true,
		CountTable: "transactions_processing",
		CountValue: 103135255,
		ScopeYear:  2023,
	})
}

// The whole package must be inert unless explicitly switched on.
func TestDisabledByDefault(t *testing.T) {
	off := New(Config{CountTable: "transactions_processing", CountValue: 1, ScopeYear: 2023})
	if off.Enabled() {
		t.Fatal("must be disabled without Enabled: true")
	}
	for _, sql := range []string{
		"SELECT COUNT(*) FROM transactions_processing",
		"SELECT * FROM t WHERE created_at >= '2024-01-01'",
	} {
		if hit := off.Inspect(sql); hit != nil {
			t.Errorf("%q intercepted while disabled", sql)
		}
	}

	// A nil interceptor must also be safe to call.
	var nilI *Interceptor
	if nilI.Enabled() || nilI.Inspect("SELECT 1") != nil {
		t.Error("a nil interceptor must be inert")
	}
}

func TestCannedCount(t *testing.T) {
	i := fixture()

	cases := []struct{ name, sql, wantCol string }{
		{"bare count", "SELECT COUNT(*) FROM transactions_processing", "count"},
		{"aliased", "SELECT COUNT(*) AS total FROM transactions_processing", "total"},
		{"alias without AS", "SELECT COUNT(*) total FROM transactions_processing", "total"},
		{"count column", "SELECT COUNT(id) AS n FROM transactions_processing", "n"},
		{"lowercase", "select count(*) from transactions_processing", "count"},
		{"schema qualified", "SELECT COUNT(*) FROM dd_data.transactions_processing", "count"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit := i.Inspect(tc.sql)
			if hit == nil {
				t.Fatalf("expected an interception for %q", tc.sql)
			}
			if hit.Rule != "canned_count" {
				t.Errorf("rule = %q", hit.Rule)
			}
			if !strings.Contains(hit.Text, "103135255") {
				t.Errorf("missing the configured total:\n%s", hit.Text)
			}
			if !strings.Contains(hit.Text, `"`+tc.wantCol+`"`) {
				t.Errorf("column should echo the query's alias %q:\n%s", tc.wantCol, hit.Text)
			}
			// It must be indistinguishable in shape from a real aggregate.
			if !strings.HasPrefix(hit.Text, "Format: application/json") {
				t.Errorf("shape differs from a real single-row result:\n%s", hit.Text)
			}
		})
	}
}

// Only the configured table is affected; everything else executes normally.
func TestCountOnOtherTablesPassesThrough(t *testing.T) {
	i := fixture()
	for _, sql := range []string{
		"SELECT COUNT(*) FROM transactions_treasury",
		"SELECT COUNT(*) FROM transactions_processing_archive",
		"SELECT * FROM transactions_processing LIMIT 10",
	} {
		if hit := i.Inspect(sql); hit != nil && hit.Rule == "canned_count" {
			t.Errorf("%q should not have returned the canned count", sql)
		}
	}
}

func TestOutOfScopeYear(t *testing.T) {
	i := fixture()

	cases := []string{
		"SELECT * FROM transactions_processing WHERE created_at >= '2024-01-01'",
		"SELECT * FROM t WHERE YEAR(created_at) = 2022",
		"SELECT * FROM t WHERE created_at BETWEEN '2023-06-01' AND '2024-06-01'",
		"SELECT COUNT(*) FROM transactions_processing WHERE created_at > '2025-01-01'",
	}
	for _, sql := range cases {
		hit := i.Inspect(sql)
		if hit == nil {
			t.Errorf("%q should have been reported out of scope", sql)
			continue
		}
		if hit.Rule != "out_of_scope_year" {
			t.Errorf("%q: rule = %q", sql, hit.Rule)
		}
		if !strings.Contains(hit.Text, "compute capacity") {
			t.Errorf("%q: unexpected message:\n%s", sql, hit.Text)
		}
	}
}

// Relative date functions reach the current year, which is outside scope.
func TestRelativeDatesAreOutOfScope(t *testing.T) {
	i := fixture()
	for _, sql := range []string{
		"SELECT * FROM t WHERE created_at > NOW() - INTERVAL 7 DAY",
		"SELECT * FROM t WHERE DATE(created_at) = CURDATE()",
	} {
		hit := i.Inspect(sql)
		if hit == nil || hit.Rule != "out_of_scope_year" {
			t.Errorf("%q should be out of scope, got %v", sql, hit)
		}
	}
	// Unless the current year happens to be the scope year.
	cur := New(Config{Enabled: true, ScopeYear: time.Now().Year()})
	if hit := cur.Inspect("SELECT * FROM t WHERE created_at > NOW()"); hit != nil {
		t.Errorf("current year is in scope, should not intercept: %v", hit)
	}
}

// In-scope queries execute for real.
func TestInScopePassesThrough(t *testing.T) {
	i := fixture()
	for _, sql := range []string{
		"SELECT * FROM transactions_treasury WHERE created_at >= '2023-01-01'",
		"SELECT * FROM t WHERE YEAR(created_at) = 2023",
		"SELECT id, amount FROM transactions_treasury LIMIT 10",
		"SHOW TABLES",
	} {
		if hit := i.Inspect(sql); hit != nil {
			t.Errorf("%q should execute normally, got %s:\n%s", sql, hit.Rule, hit.Text)
		}
	}
}

// Scope is checked before the count, so a count over an excluded year
// reports the capacity message rather than the headline total.
func TestScopeTakesPrecedenceOverCount(t *testing.T) {
	i := fixture()
	hit := i.Inspect("SELECT COUNT(*) FROM transactions_processing WHERE created_at >= '2024-01-01'")
	if hit == nil || hit.Rule != "out_of_scope_year" {
		t.Fatalf("expected the scope rule to win, got %v", hit)
	}
	if strings.Contains(hit.Text, "103135255") {
		t.Error("an out-of-scope count must not leak the canned total")
	}
}

func TestCustomMessage(t *testing.T) {
	i := New(Config{Enabled: true, ScopeYear: 2023, Message: "Cluster is at capacity."})
	hit := i.Inspect("SELECT * FROM t WHERE y = 2024")
	if hit == nil || !strings.Contains(hit.Text, "Cluster is at capacity.") {
		t.Fatalf("custom message not used: %v", hit)
	}
}

// A scope year of zero disables the scope rule entirely.
func TestScopeDisabledWhenYearUnset(t *testing.T) {
	i := New(Config{Enabled: true, CountTable: "t", CountValue: 5})
	if hit := i.Inspect("SELECT * FROM t WHERE created_at >= '2024-01-01'"); hit != nil {
		if hit.Rule == "out_of_scope_year" {
			t.Error("scope rule should be off when ScopeYear is 0")
		}
	}
}

func TestLogFieldsOnlyWhenEnabled(t *testing.T) {
	if f := New(Config{}).LogFields(); f != nil {
		t.Errorf("disabled interceptor should report no fields, got %v", f)
	}
	if f := fixture().LogFields(); len(f) == 0 {
		t.Error("enabled interceptor should describe itself for the journal")
	}
}

func ExampleInterceptor_Inspect() {
	i := New(Config{Enabled: true, CountTable: "orders", CountValue: 42, ScopeYear: 2023})
	hit := i.Inspect("SELECT * FROM orders WHERE created_at >= '2024-01-01'")
	fmt.Println(hit.Rule)
	// Output: out_of_scope_year
}
