// Package serialize implements the three-tier response matrix that keeps
// query results from flooding a model's context window.
//
//	Tier 1  (0 .. JSONMaxRows]          structured JSON, key order preserved
//	Tier 2  (JSONMaxRows .. CSVMaxRows] flat CSV, roughly 2-3x denser
//	Tier 3  (> CSVMaxRows)              CSV truncated at the ceiling + warning
//
// Why CSV above a threshold: a JSON array of objects repeats every key on
// every row. For a typical eight-column result the keys, quotes, colons and
// braces are over half the payload. CSV pays for the column names exactly
// once, so the same information costs roughly a third of the tokens.
//
// Every tier is prefixed with a metadata block whose first line declares the
// media type, so the model never has to infer the shape of what it received.
// The metadata also states how NULL is encoded and whether the result was
// truncated — the latter being the failure that actually matters, since a
// model handed a silent sample will confidently report its row count as the
// true total.
package serialize

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Media types declared in the metadata block.
const (
	MediaTypeJSON = "application/json"
	MediaTypeCSV  = "text/csv"
	MediaTypeText = "text/plain"
)

// NullToken is how SQL NULL is written in CSV output.
//
// An empty field would be denser, but it is indistinguishable from a genuine
// empty string, and that ambiguity silently corrupts any downstream
// aggregation. \N is the convention used by PostgreSQL COPY and MySQL
// mysqldump, and it is declared in the metadata block so the reader never has
// to guess.
const NullToken = `\N`

// PostgreSQL type OIDs whose text rendering can be emitted as a native JSON
// value rather than a quoted string.
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

// Column describes one result column.
type Column struct {
	Name     string
	TypeName string
	OID      uint32
}

// Row holds one result row in PostgreSQL text format. A nil element is SQL
// NULL, which is what distinguishes it from an empty string.
//
// Results are requested from the server in text format and passed through
// verbatim. That is a deliberate choice for a generic proxy: it means uuid,
// numeric, arrays, ranges, enums, geometry and every extension type render
// exactly as the server renders them, with no client-side type table to drift
// out of date, and no numeric going through a float64 on the way.
type Row [][]byte

// Options configures one serialization.
type Options struct {
	JSONMaxRows int
	CSVMaxRows  int
	Elapsed     time.Duration

	// ByteCapped reports that the caller stopped reading because the result
	// exceeded its byte budget rather than its row ceiling. Row counts alone
	// do not bound memory — a hundred rows each holding a megabyte of bytea
	// is still a hundred megabytes — so this is a second, independent
	// truncation trigger and must be reported just as loudly.
	ByteCapped bool
	ByteLimit  int
}

// Result is a rendered payload ready to be returned as MCP text content.
type Result struct {
	Text      string
	MediaType string
	Tier      int
	RowsShown int
	Truncated bool
}

// Rows renders a result set according to the tier matrix.
//
// The caller is expected to have fetched at most CSVMaxRows+1 rows: that one
// extra row is what proves more data exists without reading (or counting) the
// remainder, which keeps memory bounded no matter how large the table is.
func Rows(cols []Column, rows []Row, opt Options) (Result, error) {
	cols = dedupeNames(cols)

	jsonMax, csvMax := opt.JSONMaxRows, opt.CSVMaxRows
	if jsonMax < 0 {
		jsonMax = 0
	}
	if csvMax < jsonMax {
		csvMax = jsonMax
	}

	switch {
	case len(rows) > csvMax:
		return tier3(cols, rows[:csvMax], opt, rowLimitWarning(csvMax))
	case opt.ByteCapped:
		// Fewer rows than the ceiling, but they were large enough to hit the
		// byte budget. Still CSV, still truncated, different explanation.
		return tier3(cols, rows, opt, byteLimitWarning(len(rows), opt.ByteLimit))
	case len(rows) > jsonMax:
		return tier2(cols, rows, opt)
	default:
		return tier1(cols, rows, opt)
	}
}

// tier1 renders structured JSON.
func tier1(cols []Column, rows []Row, opt Options) (Result, error) {
	body, err := encodeJSON(cols, rows)
	if err != nil {
		return Result{}, err
	}
	meta := metadata(MediaTypeJSON, cols, len(rows), false, 0, opt.Elapsed)
	return Result{
		Text:      meta + "\n" + body,
		MediaType: MediaTypeJSON,
		Tier:      1,
		RowsShown: len(rows),
	}, nil
}

// tier2 renders the full result as CSV.
func tier2(cols []Column, rows []Row, opt Options) (Result, error) {
	body, err := encodeCSV(cols, rows)
	if err != nil {
		return Result{}, err
	}
	meta := metadata(MediaTypeCSV, cols, len(rows), false, 0, opt.Elapsed)
	return Result{
		Text:      meta + "\n" + body,
		MediaType: MediaTypeCSV,
		Tier:      2,
		RowsShown: len(rows),
	}, nil
}

// tier3 renders a truncated CSV payload and appends a conspicuous warning.
// rows must already be trimmed to what should be shown.
func tier3(cols []Column, rows []Row, opt Options, warning string) (Result, error) {
	body, err := encodeCSV(cols, rows)
	if err != nil {
		return Result{}, err
	}
	meta := metadata(MediaTypeCSV, cols, len(rows), true, opt.CSVMaxRows, opt.Elapsed)
	return Result{
		Text:      meta + "\n" + body + "\n" + warning,
		MediaType: MediaTypeCSV,
		Tier:      3,
		RowsShown: len(rows),
		Truncated: true,
	}, nil
}

func rowLimitWarning(limit int) string {
	return fmt.Sprintf(`⚠️ TRUNCATION WARNING: Dataset exceeded max allowable limit of %d rows.

Only the first %d rows appear above. The remaining rows were discarded.
The true total is UNKNOWN.

Do NOT report counts, totals, averages, maxima or minima from this sample, and do
not describe it as the complete result — it is only a prefix of the result set.

To work with the full data: add a WHERE filter to narrow it, use ORDER BY with
LIMIT/OFFSET to page through it, or run SELECT COUNT(*) to get the true total.`,
		limit, limit)
}

func byteLimitWarning(rowsShown, byteLimit int) string {
	return fmt.Sprintf(`⚠️ TRUNCATION WARNING: Dataset exceeded max allowable limit of %d bytes.

Reading stopped after %d rows because the rows themselves are large, not because a row
ceiling was reached. The remaining rows were discarded and the true total is UNKNOWN.
Do NOT report counts, totals or extremes from this sample.

To work with the full data: select only the columns you need instead of *, avoid large
text or bytea columns, or page through the result with LIMIT/OFFSET.`,
		byteLimit, rowsShown)
}

// metadata builds the block prepended to every payload. The first line always
// declares the media type.
func metadata(mediaType string, cols []Column, rowsShown int, truncated bool, limit int, elapsed time.Duration) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Format: %s\n", mediaType)

	if truncated {
		fmt.Fprintf(&b, "Rows: %d (TRUNCATED at the %d row ceiling; true total unknown)\n", rowsShown, limit)
	} else {
		fmt.Fprintf(&b, "Rows: %d (complete result set)\n", rowsShown)
	}

	fmt.Fprintf(&b, "Columns: %d — %s\n", len(cols), describeColumns(cols))

	if mediaType == MediaTypeCSV {
		fmt.Fprintf(&b, "Nulls: SQL NULL is written as %s; an empty field is a genuine empty string\n", NullToken)
		b.WriteString("Encoding: RFC 4180 CSV, comma delimited, first line is the header row\n")
	}

	fmt.Fprintf(&b, "Elapsed: %s\n", formatDuration(elapsed))
	return b.String()
}

func describeColumns(cols []Column) string {
	if len(cols) == 0 {
		return "(none)"
	}
	parts := make([]string, len(cols))
	for i, c := range cols {
		t := c.TypeName
		if t == "" {
			t = "oid:" + strconv.FormatUint(uint64(c.OID), 10)
		}
		parts[i] = c.Name + ":" + t
	}
	return strings.Join(parts, ", ")
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0ms"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
	}
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}

// encodeCSV writes the header row followed by the data rows, using the
// standard library so that quoting and escaping follow RFC 4180 exactly.
func encodeCSV(cols []Column, rows []Row) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	rec := make([]string, len(cols))
	for i, c := range cols {
		rec[i] = c.Name
	}
	if err := w.Write(rec); err != nil {
		return "", fmt.Errorf("write csv header: %w", err)
	}

	for _, row := range rows {
		for i := range cols {
			switch {
			case i >= len(row) || row[i] == nil:
				rec[i] = NullToken
			default:
				rec[i] = string(row[i])
			}
		}

		// Edge case worth handling explicitly: a single-column row holding an
		// empty string renders as an empty line, and RFC 4180 readers —
		// including encoding/csv itself — skip blank lines, so the row would
		// silently vanish on round trip. Emitting an explicitly quoted empty
		// field preserves it. Rows with two or more columns are safe already,
		// because the delimiters keep the line non-blank.
		if len(cols) == 1 && rec[0] == "" {
			w.Flush()
			if err := w.Error(); err != nil {
				return "", fmt.Errorf("flush csv: %w", err)
			}
			buf.WriteString("\"\"\n")
			continue
		}

		if err := w.Write(rec); err != nil {
			return "", fmt.Errorf("write csv row: %w", err)
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return "", fmt.Errorf("flush csv: %w", err)
	}
	return buf.String(), nil
}

// encodeJSON emits an array of objects preserving the SELECT's column order.
// encoding/json sorts map keys alphabetically, which would scramble that, so
// the object is assembled directly.
func encodeJSON(cols []Column, rows []Row) (string, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')

	keys := make([]string, len(cols))
	for i, c := range cols {
		k, err := json.Marshal(c.Name)
		if err != nil {
			return "", fmt.Errorf("encode column name %q: %w", c.Name, err)
		}
		keys[i] = string(k)
	}

	for ri, row := range rows {
		if ri > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString("\n  {")
		for ci, c := range cols {
			if ci > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(keys[ci])
			buf.WriteByte(':')
			var raw []byte
			if ci < len(row) {
				raw = row[ci]
			}
			writeJSONValue(&buf, raw, c.OID)
		}
		buf.WriteByte('}')
	}

	if len(rows) > 0 {
		buf.WriteByte('\n')
	}
	buf.WriteByte(']')
	return buf.String(), nil
}

// writeJSONValue maps a text-format value to the most faithful JSON form.
// Anything it cannot prove is a valid JSON scalar becomes a string, so the
// output is always valid JSON.
func writeJSONValue(buf *bytes.Buffer, raw []byte, oid uint32) {
	if raw == nil {
		buf.WriteString("null")
		return
	}
	s := string(raw)

	switch oid {
	case oidBool:
		// PostgreSQL text format renders booleans as t / f.
		switch s {
		case "t", "true":
			buf.WriteString("true")
			return
		case "f", "false":
			buf.WriteString("false")
			return
		}

	case oidInt2, oidInt4, oidInt8, oidFloat4, oidFloat8, oidNumeric:
		// Emitted verbatim so a bigint or a high-precision numeric keeps every
		// digit instead of being rounded through a float64. NaN and Infinity
		// are legal PostgreSQL values but illegal JSON numbers, so they fall
		// through to the string path below.
		if isJSONNumber(s) {
			buf.WriteString(s)
			return
		}

	case oidJSON, oidJSONB:
		if json.Valid(raw) {
			buf.Write(raw)
			return
		}
	}

	encoded, err := json.Marshal(s)
	if err != nil {
		buf.WriteString(`""`)
		return
	}
	buf.Write(encoded)
}

// isJSONNumber reports whether s is a valid JSON number literal.
func isJSONNumber(s string) bool {
	if s == "" {
		return false
	}
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	tok, err := d.Token()
	if err != nil {
		return false
	}
	if _, ok := tok.(json.Number); !ok {
		return false
	}
	// Reject trailing junk such as "1 2".
	if _, err := d.Token(); err == nil {
		return false
	}
	return true
}

// dedupeNames disambiguates repeated column names (`SELECT a.id, b.id ...`),
// which would otherwise collide as JSON object keys and silently drop a
// column from the payload.
func dedupeNames(cols []Column) []Column {
	seen := make(map[string]int, len(cols))
	out := make([]Column, len(cols))
	copy(out, cols)

	for i, c := range out {
		name := c.Name
		if name == "" {
			name = "column_" + strconv.Itoa(i+1)
		}
		if n, dup := seen[name]; dup {
			n++
			seen[name] = n
			out[i].Name = fmt.Sprintf("%s_%d", name, n)
		} else {
			seen[name] = 1
			out[i].Name = name
		}
	}
	return out
}

// Command renders the outcome of a statement that returned no result set.
func Command(tag string, rowsAffected int64, elapsed time.Duration) Result {
	text := fmt.Sprintf("Format: %s\nRows: 0 (statement returned no result set)\nElapsed: %s\n\n"+
		"Command: %s\nRows affected: %d\n",
		MediaTypeText, formatDuration(elapsed), tag, rowsAffected)
	return Result{Text: text, MediaType: MediaTypeText, Tier: 1}
}
