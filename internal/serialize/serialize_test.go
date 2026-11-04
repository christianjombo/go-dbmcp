package serialize

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func cols(names ...string) []Column {
	out := make([]Column, len(names))
	for i, n := range names {
		out[i] = Column{Name: n, TypeName: "text", Kind: KindText}
	}
	return out
}

func textRows(n int, cols int) []Row {
	rows := make([]Row, n)
	for i := range rows {
		r := make(Row, cols)
		for c := range r {
			r[c] = []byte("v")
		}
		rows[i] = r
	}
	return rows
}

func opts(jsonMax, csvMax int) Options {
	return Options{JSONMaxRows: jsonMax, CSVMaxRows: csvMax, Elapsed: 5 * time.Millisecond}
}

// The tier boundaries are the contract of this package, so they are pinned
// exactly at the edges rather than somewhere in the middle of each band.
func TestTierBoundaries(t *testing.T) {
	cases := []struct {
		rows      int
		wantTier  int
		wantMedia string
		wantShown int
		truncated bool
	}{
		{0, 1, MediaTypeJSON, 0, false},
		{1, 1, MediaTypeJSON, 1, false},
		{2, 1, MediaTypeJSON, 2, false}, // exactly at the JSON ceiling
		{3, 2, MediaTypeCSV, 3, false},  // one past it
		{4, 2, MediaTypeCSV, 4, false},  // exactly at the CSV ceiling
		{5, 3, MediaTypeCSV, 4, true},   // one past it: truncated to the ceiling
		{9, 3, MediaTypeCSV, 4, true},
	}

	for _, tc := range cases {
		res, err := Rows(cols("a", "b"), textRows(tc.rows, 2), opts(2, 4))
		if err != nil {
			t.Fatalf("%d rows: %v", tc.rows, err)
		}
		if res.Tier != tc.wantTier {
			t.Errorf("%d rows: tier = %d, want %d", tc.rows, res.Tier, tc.wantTier)
		}
		if res.MediaType != tc.wantMedia {
			t.Errorf("%d rows: media = %s, want %s", tc.rows, res.MediaType, tc.wantMedia)
		}
		if res.RowsShown != tc.wantShown {
			t.Errorf("%d rows: shown = %d, want %d", tc.rows, res.RowsShown, tc.wantShown)
		}
		if res.Truncated != tc.truncated {
			t.Errorf("%d rows: truncated = %v, want %v", tc.rows, res.Truncated, tc.truncated)
		}
	}
}

// The spec requires the metadata block to declare the format on the first
// line, because that is what tells the model how to read the payload.
func TestFirstLineDeclaresFormat(t *testing.T) {
	jsonRes, err := Rows(cols("a"), textRows(1, 1), opts(10, 100))
	if err != nil {
		t.Fatal(err)
	}
	if got := firstLine(jsonRes.Text); got != "Format: application/json" {
		t.Errorf("tier 1 first line = %q", got)
	}

	csvRes, err := Rows(cols("a"), textRows(50, 1), opts(10, 100))
	if err != nil {
		t.Fatal(err)
	}
	if got := firstLine(csvRes.Text); got != "Format: text/csv" {
		t.Errorf("tier 2 first line = %q", got)
	}

	truncRes, err := Rows(cols("a"), textRows(150, 1), opts(10, 100))
	if err != nil {
		t.Fatal(err)
	}
	if got := firstLine(truncRes.Text); got != "Format: text/csv" {
		t.Errorf("tier 3 first line = %q", got)
	}
}

func TestTruncationWarningIsPresentAndExplicit(t *testing.T) {
	res, err := Rows(cols("a"), textRows(5, 1), opts(1, 3))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "⚠️ TRUNCATION WARNING: Dataset exceeded max allowable limit") {
		t.Fatalf("missing the required truncation warning:\n%s", res.Text)
	}
	// The metadata must also admit the truncation, so a model that reads only
	// the header still learns the result is partial.
	if !strings.Contains(res.Text, "TRUNCATED") {
		t.Errorf("metadata block does not mark the result truncated:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "The true total is UNKNOWN.") {
		t.Errorf("warning should state the total is unknown:\n%s", res.Text)
	}
}

// Truncation must be reported when the byte budget trips, even though the row
// count is well under the ceiling.
func TestByteCappedTruncates(t *testing.T) {
	res, err := Rows(cols("a"), textRows(3, 1), Options{
		JSONMaxRows: 100, CSVMaxRows: 2000, ByteCapped: true, ByteLimit: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != 3 || !res.Truncated {
		t.Fatalf("tier = %d truncated = %v, want tier 3 truncated", res.Tier, res.Truncated)
	}
	if !strings.Contains(res.Text, "exceeded max allowable limit of 1024 bytes") {
		t.Fatalf("expected a byte-based warning:\n%s", res.Text)
	}
	if res.RowsShown != 3 {
		t.Errorf("rows shown = %d, want all 3 retained", res.RowsShown)
	}
}

// The CSV we emit must be readable by a standard RFC 4180 parser, including
// for values containing the delimiter, quotes and newlines.
func TestCSVRoundTripsThroughStandardParser(t *testing.T) {
	c := cols("id", "note")
	rows := []Row{
		{[]byte("1"), []byte(`plain`)},
		{[]byte("2"), []byte(`has,comma`)},
		{[]byte("3"), []byte(`has "quotes"`)},
		{[]byte("4"), []byte("has\nnewline")},
		{[]byte("5"), []byte(` leading and trailing `)},
		{[]byte("6"), nil},        // SQL NULL
		{[]byte("7"), []byte(``)}, // empty string, which must stay distinct
	}

	res, err := Rows(c, rows, opts(0, 100))
	if err != nil {
		t.Fatal(err)
	}

	body := payload(res.Text)
	parsed, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("emitted CSV does not parse: %v\n%s", err, body)
	}

	if len(parsed) != len(rows)+1 {
		t.Fatalf("parsed %d records, want %d (header + %d rows)", len(parsed), len(rows)+1, len(rows))
	}
	if parsed[0][0] != "id" || parsed[0][1] != "note" {
		t.Fatalf("header = %v", parsed[0])
	}

	want := []string{"plain", "has,comma", `has "quotes"`, "has\nnewline", " leading and trailing ", NullToken, ""}
	for i, w := range want {
		if got := parsed[i+1][1]; got != w {
			t.Errorf("row %d value = %q, want %q", i+1, got, w)
		}
	}
}

// This is the distinction an empty-field NULL encoding would destroy.
func TestNullIsDistinguishableFromEmptyString(t *testing.T) {
	res, err := Rows(cols("v"), []Row{{nil}, {[]byte("")}}, opts(0, 100))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := csv.NewReader(strings.NewReader(payload(res.Text))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if parsed[1][0] == parsed[2][0] {
		t.Fatalf("NULL and empty string both rendered as %q", parsed[1][0])
	}
	if parsed[1][0] != NullToken {
		t.Errorf("NULL rendered as %q, want %q", parsed[1][0], NullToken)
	}
	if parsed[2][0] != "" {
		t.Errorf("empty string rendered as %q, want empty", parsed[2][0])
	}
	if !strings.Contains(res.Text, "SQL NULL is written as") {
		t.Error("metadata must declare the NULL encoding")
	}
}

func TestJSONPreservesColumnOrderAndTypes(t *testing.T) {
	c := []Column{
		{Name: "id", Kind: KindNumber, TypeName: "int4"},
		{Name: "amount", Kind: KindNumber, TypeName: "numeric"},
		{Name: "active", Kind: KindBool, TypeName: "bool"},
		{Name: "meta", Kind: KindJSON, TypeName: "jsonb"},
		{Name: "name", Kind: KindText, TypeName: "text"},
		{Name: "missing", Kind: KindText, TypeName: "text"},
	}
	rows := []Row{{
		[]byte("7"),
		[]byte("12345678901234567890.123456789"),
		[]byte("t"),
		[]byte(`{"k":1}`),
		[]byte("alice"),
		nil,
	}}

	res, err := Rows(c, rows, opts(10, 100))
	if err != nil {
		t.Fatal(err)
	}
	body := payload(res.Text)

	if !json.Valid([]byte(body)) {
		t.Fatalf("emitted JSON is not valid:\n%s", body)
	}

	// Column order must survive; encoding/json would have sorted map keys.
	wantOrder := []string{`"id"`, `"amount"`, `"active"`, `"meta"`, `"name"`, `"missing"`}
	prev := -1
	for _, key := range wantOrder {
		at := strings.Index(body, key)
		if at < 0 {
			t.Fatalf("key %s missing from output:\n%s", key, body)
		}
		if at < prev {
			t.Fatalf("key %s appears out of order:\n%s", key, body)
		}
		prev = at
	}

	// A high-precision numeric must keep every digit: routing it through a
	// float64 would silently round it.
	if !strings.Contains(body, `"amount":12345678901234567890.123456789`) {
		t.Errorf("numeric was not emitted verbatim:\n%s", body)
	}
	if !strings.Contains(body, `"active":true`) {
		t.Errorf("postgres 't' should become JSON true:\n%s", body)
	}
	if !strings.Contains(body, `"meta":{"k":1}`) {
		t.Errorf("jsonb should be embedded, not string-escaped:\n%s", body)
	}
	if !strings.Contains(body, `"missing":null`) {
		t.Errorf("NULL should become JSON null:\n%s", body)
	}
}

// NaN and Infinity are legal PostgreSQL float values but illegal JSON
// numbers, so they must degrade to strings rather than emit invalid JSON.
func TestJSONNonFiniteNumbersBecomeStrings(t *testing.T) {
	c := []Column{{Name: "v", Kind: KindNumber, TypeName: "float8"}}
	res, err := Rows(c, []Row{{[]byte("NaN")}, {[]byte("Infinity")}}, opts(10, 100))
	if err != nil {
		t.Fatal(err)
	}
	body := payload(res.Text)
	if !json.Valid([]byte(body)) {
		t.Fatalf("output is not valid JSON:\n%s", body)
	}
	if !strings.Contains(body, `"v":"NaN"`) {
		t.Errorf("NaN should be quoted:\n%s", body)
	}
}

// Duplicate column names would collide as JSON object keys and silently drop
// a column, so they are disambiguated.
func TestDuplicateColumnNamesAreDisambiguated(t *testing.T) {
	c := cols("id", "id", "id")
	res, err := Rows(c, []Row{{[]byte("1"), []byte("2"), []byte("3")}}, opts(10, 100))
	if err != nil {
		t.Fatal(err)
	}
	body := payload(res.Text)

	var decoded []map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	if len(decoded[0]) != 3 {
		t.Fatalf("expected 3 distinct keys, got %d: %v", len(decoded[0]), decoded[0])
	}
}

func TestMetadataListsColumnsAndTypes(t *testing.T) {
	c := []Column{{Name: "id", Kind: KindNumber, TypeName: "int4"}, {Name: "email", Kind: KindText, TypeName: "text"}}
	res, err := Rows(c, textRows(1, 2), opts(10, 100))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "id:int4, email:text") {
		t.Errorf("metadata should name columns with types:\n%s", res.Text)
	}
}

func TestCommandResult(t *testing.T) {
	res := Command("UPDATE 3", 3, 2*time.Millisecond)
	if !strings.Contains(res.Text, "Rows affected: 3") {
		t.Errorf("missing affected rows:\n%s", res.Text)
	}
	if firstLine(res.Text) != "Format: text/plain" {
		t.Errorf("first line = %q", firstLine(res.Text))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// payload returns everything after the metadata block's blank-line separator.
func payload(s string) string {
	if i := strings.Index(s, "\n\n"); i >= 0 {
		return strings.TrimSpace(s[i+2:])
	}
	return s
}
