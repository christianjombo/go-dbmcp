// Package guard screens SQL before it reaches a tenant database.
//
// The model is an allowlist of statement-leading keywords, not a blocklist of
// substrings. That distinction is the whole design:
//
//   - A statement is identified by the command keyword it *starts* with, and
//     that keyword must be explicitly permitted. Anything unrecognised is
//     refused, so a command nobody thought to list cannot slip through.
//   - Keywords are therefore only meaningful in leading position. `SET` is a
//     forbidden command, but `UPDATE t SET x = 1` is permitted — a substring
//     scanner would reject that perfectly ordinary statement. Likewise
//     `ANALYZE` is forbidden as a command while `EXPLAIN ANALYZE SELECT ...`
//     is permitted, because there the word means "actually execute it".
//   - Stacked statements are refused outright, which is what makes the
//     leading-keyword check sufficient: there is only ever one command to
//     judge.
//
// Comments, string literals, quoted identifiers and dollar-quoted bodies are
// stripped before any of this, so `DROP/**/DATABASE x` is caught while
// `SELECT 'drop database x'` and a column named `vacuum_log` are not.
//
// IMPORTANT, and stated plainly because it governs how much weight this code
// can bear: this is a guardrail, not a security boundary. It stops a confused
// model from issuing a destructive statement by accident. It cannot stop a
// determined adversary, and it cannot see inside a stored procedure invoked
// by CALL. The real boundary is the privileges granted to the role in the
// x-db-username header.
package guard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Decision is the outcome of screening one statement.
type Decision struct {
	Allowed bool
	// Command is the leading keyword that was identified, when one was.
	Command string
	RuleID  string
	Reason  string
}

// AllowedCommands are the statement-leading keywords this server will run.
//
// The configured policy also names clause keywords — JOIN, WHERE, GROUP BY,
// ORDER BY, UNION, ON, USING and so on. Those cannot begin a statement; they
// are permitted implicitly wherever they appear inside one of the commands
// below, so listing them here would be meaningless.
var AllowedCommands = map[string]string{
	"SELECT":  "read rows",
	"INSERT":  "add rows",
	"UPDATE":  "modify rows",
	"DELETE":  "remove rows",
	"REPLACE": "insert-or-replace rows (MySQL)",
	"MERGE":   "conditional insert/update/delete",
	"WITH":    "common table expression",
	"SHOW":    "inspect server or schema metadata",
	"EXPLAIN": "inspect a query plan",
	"CALL":    "invoke a stored procedure",
}

// DeniedCommands are refused with a specific explanation. Anything not in
// AllowedCommands is refused regardless; this map exists so that the common
// cases get a useful message instead of "unrecognised command".
var DeniedCommands = map[string]string{
	"DROP":     "DROP destroys schema objects irreversibly.",
	"TRUNCATE": "TRUNCATE empties a table irreversibly and without row-level logging.",
	"ALTER":    "ALTER changes schema or server configuration.",
	"CREATE":   "CREATE adds schema objects; this service is for querying data, not defining it.",
	"RENAME":   "RENAME changes object names other clients depend on.",
	"GRANT":    "GRANT changes who can access this database.",
	"REVOKE":   "REVOKE changes who can access this database.",
	"VACUUM":   "VACUUM rewrites storage and can block normal traffic for a long time.",
	"ANALYZE":  "ANALYZE as a statement rewrites planner statistics. (EXPLAIN ANALYZE is permitted.)",
	"OPTIMIZE": "OPTIMIZE TABLE rebuilds storage and takes heavy locks.",
	"KILL":     "KILL terminates other sessions.",
	"SHUTDOWN": "SHUTDOWN stops the database server.",
	"SET":      "SET changes session or server state. (The SET clause of an UPDATE is permitted.)",
}

// Rule is a contextual pattern applied to the body of an otherwise permitted
// statement — the cases where the command is fine but a specific construct
// inside it is not.
type Rule struct {
	ID string
	// OnlyWhenLeading, when non-empty, restricts the rule to statements
	// beginning with that command.
	OnlyWhenLeading string
	Pattern         *regexp.Regexp
	Reason          string
}

// WriteCommands are the permitted commands that modify data. An operator can
// withdraw them with MCP_ALLOW_WRITES=false.
var WriteCommands = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true, "MERGE": true,
}

// Scanner screens statements against the command policy and contextual rules.
type Scanner struct {
	rules       []Rule
	allowWrites bool
}

// contextualRules catch constructs that a permitted command can still carry:
// filesystem access and session control reachable from inside a plain SELECT.
func contextualRules() []Rule {
	mk := func(id, leading, pattern, reason string) Rule {
		return Rule{ID: id, OnlyWhenLeading: leading, Pattern: regexp.MustCompile(pattern), Reason: reason}
	}
	return []Rule{
		// MySQL: writes a file on the database host.
		mk("into_outfile", "", `\bINTO\s+(OUTFILE|DUMPFILE)\b`,
			"SELECT ... INTO OUTFILE/DUMPFILE writes a file on the database server."),

		// PostgreSQL: SELECT ... INTO creates a table, which is DDL wearing a
		// SELECT costume. INSERT INTO is unaffected — that statement leads
		// with INSERT, not SELECT.
		mk("select_into", "SELECT", `\bINTO\b`,
			"SELECT ... INTO creates a new table. Use INSERT INTO ... SELECT if you meant to copy rows."),

		// Reading the database server's filesystem.
		mk("server_file_read", "", `\b(LOAD_FILE|PG_READ_FILE|PG_READ_BINARY_FILE|PG_LS_DIR|PG_STAT_FILE)\s*\(`,
			"These functions read files from the database server's filesystem."),
		mk("large_object_io", "", `\b(LO_IMPORT|LO_EXPORT)\s*\(`,
			"Large-object import/export reads and writes files on the database host."),

		// PostgreSQL equivalents of KILL and SHUTDOWN, reachable from SELECT.
		mk("backend_control", "", `\b(PG_TERMINATE_BACKEND|PG_CANCEL_BACKEND|PG_RELOAD_CONF)\s*\(`,
			"These functions terminate other sessions or reload server configuration."),

		// COPY ... PROGRAM executes a shell command. COPY is not an allowed
		// command, so this is defence in depth for any path that reaches the
		// body check with COPY still present.
		mk("copy_program", "", `\bCOPY\b[^;]*\bPROGRAM\b`,
			"COPY ... PROGRAM executes a shell command on the database host."),
	}
}

// New builds a Scanner from the contextual rules plus any operator-supplied
// patterns. A pattern that fails to compile is skipped and reported as a
// warning rather than aborting boot: a typo in a unit file must not take the
// service down, but it must be visible.
// allowWrites controls whether INSERT, UPDATE, DELETE, REPLACE and MERGE are
// permitted. Note that this is a guardrail toggle, not an enforcement
// mechanism: the dependable way to make a connection read-only is to grant
// the role nothing but SELECT.
func New(extraPatterns []string, allowWrites bool) (*Scanner, []string) {
	s := &Scanner{rules: contextualRules(), allowWrites: allowWrites}
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

// Inspect screens a statement. It performs no I/O.
func (s *Scanner) Inspect(sql string) Decision {
	normalised := Normalize(sql)

	if normalised == "" {
		return Decision{
			RuleID: "empty_statement",
			Reason: "The statement was empty once comments were stripped, so there was nothing to run.",
		}
	}

	if n := countStatements(normalised); n > 1 {
		return Decision{
			RuleID: "stacked_statements",
			Reason: fmt.Sprintf("Found %d statements separated by ';'. Send one statement per call — "+
				"a benign leading statement can otherwise hide a destructive trailing one.", n),
		}
	}

	command := LeadingCommand(normalised)
	if command == "" {
		return Decision{
			RuleID: "unparseable",
			Reason: "Could not identify a leading SQL command in this statement.",
		}
	}

	if reason, denied := DeniedCommands[command]; denied {
		return Decision{
			Command: command,
			RuleID:  "command_not_allowed",
			Reason:  reason,
		}
	}

	if _, allowed := AllowedCommands[command]; !allowed {
		return Decision{
			Command: command,
			RuleID:  "command_not_recognised",
			Reason: fmt.Sprintf("%s is not among the commands this server permits. "+
				"Allowed commands are: %s.", command, allowedList()),
		}
	}

	if WriteCommands[command] && !s.allowWrites {
		return Decision{
			Command: command,
			RuleID:  "writes_disabled",
			Reason: fmt.Sprintf("%s modifies data, and this server is configured for read-only access "+
				"(MCP_ALLOW_WRITES=false). Only queries are accepted.", command),
		}
	}

	for _, r := range s.rules {
		if r.OnlyWhenLeading != "" && r.OnlyWhenLeading != command {
			continue
		}
		if r.Pattern.MatchString(normalised) {
			return Decision{Command: command, RuleID: r.ID, Reason: r.Reason}
		}
	}

	return Decision{Allowed: true, Command: command}
}

func allowedList() string {
	names := make([]string, 0, len(AllowedCommands))
	for k := range AllowedCommands {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// LeadingCommand returns the command keyword a normalised statement begins
// with, skipping any opening parentheses so that `(SELECT ...) UNION
// (SELECT ...)` is recognised as a SELECT.
func LeadingCommand(normalised string) string {
	s := strings.TrimLeft(normalised, "( \t\n")
	end := 0
	for end < len(s) && (isWordByte(s[end])) {
		end++
	}
	return s[:end]
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
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

		// Line comment: -- to end of line.
		case c == '-' && i+1 < n && rs[i+1] == '-':
			for i < n && rs[i] != '\n' {
				i++
			}
			b.WriteByte(' ')

		// MySQL also accepts # as a line comment.
		case c == '#':
			for i < n && rs[i] != '\n' {
				i++
			}
			b.WriteByte(' ')

		// String literal. E'...' allows backslash escapes; MySQL allows
		// backslash escapes in ordinary literals too, so they are always
		// honoured — over-consuming an escape cannot hide a keyword, it can
		// only end the literal later than a strict parser would.
		case c == '\'':
			i++
			for i < n {
				if rs[i] == '\\' && i+1 < n {
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

		// Quoted identifier (SQL standard / PostgreSQL).
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

		// Backtick-quoted identifier (MySQL).
		case c == '`':
			i++
			for i < n {
				if rs[i] == '`' {
					if i+1 < n && rs[i+1] == '`' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteString(" `` ")

		// Dollar-quoted body: $$...$$ or $tag$...$tag$ (PostgreSQL).
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

// BlockMessage renders the refusal returned to the model. It names the rule
// and lists what is permitted, so the model can adapt rather than retrying
// the same thing.
func BlockMessage(d Decision) string {
	var b strings.Builder
	b.WriteString("⛔ QUERY BLOCKED BY SAFETY GUARDRAIL\n\n")
	if d.Command != "" {
		fmt.Fprintf(&b, "Command: %s\n", d.Command)
	}
	fmt.Fprintf(&b, "Rule:    %s\nReason:  %s\n\n", d.RuleID, d.Reason)
	b.WriteString("The statement was NOT sent to the database. Nothing was executed and nothing changed.\n\n")
	fmt.Fprintf(&b, "Permitted commands: %s.\n", allowedList())
	b.WriteString("Schema changes and administrative commands must be performed by the database owner directly.")
	return b.String()
}
