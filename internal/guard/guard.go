// Package guard screens SQL before it reaches a tenant database.
//
// IMPORTANT, and stated plainly because it governs how much weight this code
// can bear: a keyword scanner is a guardrail, not a security boundary. It
// exists to stop a confused model from issuing a destructive statement by
// accident. It cannot stop a determined adversary, because SQL has too many
// ways to express the same intent. The real boundary is the privileges of the
// role in the x-db-username header — give it SELECT and nothing else.
//
// What this package does buy, over the naive strings.Contains check:
//
//   - `DROP/**/DATABASE x` cannot slip through, because comments are replaced
//     by whitespace before matching — the same thing PostgreSQL's own lexer
//     does, so the scanner sees the statement the server would execute.
//     (`DR/**/OP` needs no handling: splitting a keyword with a comment makes
//     it two tokens, which the server rejects as a syntax error.)
//   - `SELECT 'drop database x'` and a column named vacuum_log do not false
//     positive, because string literals and quoted identifiers are removed
//     before matching.
//   - `SELECT 1; DROP DATABASE x` is rejected outright, because stacked
//     statements are detected after literals are stripped.
package guard

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Decision is the outcome of screening one statement.
type Decision struct {
	Allowed bool
	RuleID  string
	Reason  string
}

// Rule is a single screening pattern applied to normalised SQL.
type Rule struct {
	ID      string
	Pattern *regexp.Regexp
	Reason  string
}

// Scanner screens statements against a rule set.
type Scanner struct {
	rules []Rule
}

// defaultRules. The first two are mandated by the service spec; the rest
// close off the adjacent ways to destroy, exfiltrate from, or execute code on
// a target database.
func defaultRules() []Rule {
	mk := func(id, pattern, reason string) Rule {
		return Rule{ID: id, Pattern: regexp.MustCompile(pattern), Reason: reason}
	}
	return []Rule{
		mk("drop_database", `\bDROP\s+DATABASE\b`,
			"DROP DATABASE destroys an entire database and cannot be undone."),
		mk("vacuum", `\bVACUUM\b`,
			"VACUUM is a maintenance command that rewrites storage and can block normal traffic for a long time."),

		mk("drop_schema", `\bDROP\s+SCHEMA\b`,
			"DROP SCHEMA destroys every object in a schema."),
		mk("drop_tablespace", `\bDROP\s+TABLESPACE\b`,
			"DROP TABLESPACE removes a storage location used by live relations."),
		mk("drop_principal", `\bDROP\s+(ROLE|USER|GROUP)\b`,
			"Dropping a role or user changes who can reach this database."),
		mk("drop_owned", `\bDROP\s+OWNED\b`,
			"DROP OWNED removes every object belonging to a role."),
		mk("truncate", `\bTRUNCATE\b`,
			"TRUNCATE empties a table irreversibly and without row-level logging."),
		mk("alter_system", `\bALTER\s+SYSTEM\b`,
			"ALTER SYSTEM rewrites server configuration for every connection, not just this one."),
		mk("maintenance", `\b(REINDEX|CLUSTER)\b`,
			"REINDEX and CLUSTER take heavy locks and rewrite storage."),

		mk("copy_program", `\bCOPY\b[^;]*\bPROGRAM\b`,
			"COPY ... PROGRAM executes a shell command on the database host."),
		mk("server_file_access", `\b(PG_READ_FILE|PG_READ_BINARY_FILE|PG_LS_DIR|PG_STAT_FILE)\s*\(`,
			"These functions read the database server's filesystem."),
		mk("large_object_io", `\b(LO_IMPORT|LO_EXPORT)\s*\(`,
			"Large-object import/export reads and writes files on the database host."),
		mk("backend_control", `\b(PG_TERMINATE_BACKEND|PG_CANCEL_BACKEND|PG_RELOAD_CONF)\s*\(`,
			"These functions disrupt other sessions or reload server configuration."),
		mk("untrusted_language", `\bLANGUAGE\s+("?C"?|INTERNAL)\b`,
			"Functions in C or internal language can execute arbitrary native code."),
		mk("anonymous_block", `^\s*DO\b`,
			"An anonymous DO block can run arbitrary procedural code, which this scanner cannot inspect."),
	}
}

// New builds a Scanner from the default rules plus any operator-supplied
// patterns. A pattern that fails to compile is skipped and reported as a
// warning rather than aborting boot: a typo in a unit file must not take the
// service down, but it must be visible.
func New(extraPatterns []string) (*Scanner, []string) {
	s := &Scanner{rules: defaultRules()}
	var warnings []string

	for i, p := range extraPatterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"MCP_EXTRA_BLOCKED_PATTERNS[%d] %q is not a valid regular expression and was ignored: %v", i, p, err))
			continue
		}
		s.rules = append(s.rules, Rule{
			ID:      fmt.Sprintf("custom_%d", i),
			Pattern: re,
			Reason:  fmt.Sprintf("Blocked by operator-configured pattern %q.", p),
		})
	}
	return s, warnings
}

// Inspect screens a statement. The returned Decision is advisory metadata for
// the caller to render; it performs no I/O.
func (s *Scanner) Inspect(sql string) Decision {
	normalised := Normalize(sql)

	if normalised == "" {
		return Decision{
			Allowed: false,
			RuleID:  "empty_statement",
			Reason:  "The statement was empty once comments were stripped, so there was nothing to run.",
		}
	}

	if n := countStatements(normalised); n > 1 {
		return Decision{
			Allowed: false,
			RuleID:  "stacked_statements",
			Reason: fmt.Sprintf("Found %d statements separated by ';'. Only one statement may be sent per call, "+
				"because a benign leading statement can hide a destructive trailing one.", n),
		}
	}

	for _, r := range s.rules {
		if r.Pattern.MatchString(normalised) {
			return Decision{Allowed: false, RuleID: r.ID, Reason: r.Reason}
		}
	}
	return Decision{Allowed: true}
}

// Normalize strips comments, string literals, quoted identifiers and
// dollar-quoted bodies, collapses whitespace, and upper-cases the result.
//
// Removing literals before matching is what makes the scanner both harder to
// evade and far less prone to false positives. The output is for pattern
// matching only — it is never executed.
func Normalize(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))

	rs := []rune(sql)
	n := len(rs)

	for i := 0; i < n; {
		c := rs[i]

		switch {
		// Block comment. PostgreSQL nests these, so track depth.
		case c == '/' && i+1 < n && rs[i+1] == '*':
			depth := 1
			i += 2
			for i < n && depth > 0 {
				if rs[i] == '/' && i+1 < n && rs[i+1] == '*' {
					depth++
					i += 2
				} else if rs[i] == '*' && i+1 < n && rs[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
			b.WriteByte(' ')

		// Line comment.
		case c == '-' && i+1 < n && rs[i+1] == '-':
			for i < n && rs[i] != '\n' {
				i++
			}
			b.WriteByte(' ')

		// String literal. E'' allows backslash escapes; '' doubling otherwise.
		case c == '\'':
			escapeString := precedingEscapeMarker(rs, i)
			i++
			for i < n {
				if escapeString && rs[i] == '\\' {
					i += 2
					continue
				}
				if rs[i] == '\'' {
					if i+1 < n && rs[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteString(" '' ")

		// Quoted identifier.
		case c == '"':
			i++
			for i < n {
				if rs[i] == '"' {
					if i+1 < n && rs[i+1] == '"' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteString(` "" `)

		// Dollar-quoted body: $$...$$ or $tag$...$tag$.
		case c == '$':
			if tag, bodyStart, ok := dollarTag(rs, i); ok {
				closer := "$" + tag + "$"
				rest := string(rs[bodyStart:])
				if idx := strings.Index(rest, closer); idx >= 0 {
					i = bodyStart + len([]rune(rest[:idx])) + len([]rune(closer))
				} else {
					i = n // unterminated
				}
				b.WriteString(" $$ ")
			} else {
				b.WriteRune(c)
				i++
			}

		default:
			b.WriteRune(c)
			i++
		}
	}

	return collapseSpace(strings.ToUpper(b.String()))
}

// precedingEscapeMarker reports whether the quote at index i opens an
// escape string of the form E'...', i.e. is immediately preceded by a
// standalone E or e.
func precedingEscapeMarker(rs []rune, i int) bool {
	if i == 0 {
		return false
	}
	p := rs[i-1]
	if p != 'E' && p != 'e' {
		return false
	}
	if i-1 == 0 {
		return true
	}
	// The E must be its own token, not the tail of an identifier.
	prev := rs[i-2]
	return !unicode.IsLetter(prev) && !unicode.IsDigit(prev) && prev != '_'
}

// dollarTag recognises a dollar-quote opener at rs[i] == '$' and returns the
// tag and the index just past the opener. `$1` and `$2` placeholders are
// correctly rejected, because a tag may not begin with a digit.
func dollarTag(rs []rune, i int) (tag string, bodyStart int, ok bool) {
	j := i + 1
	for j < len(rs) {
		c := rs[j]
		if c == '$' {
			return string(rs[i+1 : j]), j + 1, true
		}
		isFirst := j == i+1
		if !(unicode.IsLetter(c) || c == '_' || (!isFirst && unicode.IsDigit(c))) {
			return "", 0, false
		}
		j++
	}
	return "", 0, false
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// countStatements counts non-empty ';'-separated statements in normalised SQL.
// A single trailing semicolon is conventional and does not count.
func countStatements(normalised string) int {
	count := 0
	for _, part := range strings.Split(normalised, ";") {
		if strings.TrimSpace(part) != "" {
			count++
		}
	}
	return count
}

// BlockMessage renders the "structural warning text" returned to the model
// when a statement is refused. It names the rule so the model can adapt
// rather than blindly retrying the same thing.
func BlockMessage(d Decision) string {
	return fmt.Sprintf(`⛔ QUERY BLOCKED BY SAFETY GUARDRAIL

Rule:   %s
Reason: %s

The statement was NOT sent to the database. Nothing was executed and nothing changed.
Rewrite the query as a read-only statement, or ask the user to perform this operation
directly against their database if it is genuinely intended.`, d.RuleID, d.Reason)
}
