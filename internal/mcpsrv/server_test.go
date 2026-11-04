package mcpsrv

import "testing"

// Regression test for a bug caught by the live end-to-end suite: an empty
// string was arriving at the CSV encoder as SQL NULL.
//
// The cause was `append([]byte(nil), v...)`, which returns nil when v is
// empty. Every empty string therefore became indistinguishable from NULL —
// precisely the corruption the \N encoding exists to prevent — on both
// engines.
func TestCopyRowKeepsEmptyStringsDistinctFromNull(t *testing.T) {
	raw := [][]byte{
		nil,          // SQL NULL
		{},           // empty string
		[]byte("ok"), // ordinary value
	}

	row, used := copyRow(raw)

	if row[0] != nil {
		t.Errorf("NULL must stay nil, got %#v", row[0])
	}
	if row[1] == nil {
		t.Error("an empty string must stay non-nil, or it is reported as NULL")
	}
	if len(row[1]) != 0 {
		t.Errorf("empty string became %q", row[1])
	}
	if string(row[2]) != "ok" {
		t.Errorf("value = %q, want \"ok\"", row[2])
	}
	if used != 2 {
		t.Errorf("used = %d, want 2 (only the non-empty value has bytes)", used)
	}
}

// The copy must not alias the driver's buffer, which is reused by the next
// call to Next.
func TestCopyRowDetachesFromDriverBuffer(t *testing.T) {
	buf := []byte("original")
	row, _ := copyRow([][]byte{buf})

	copy(buf, "OVERWROTE")

	if string(row[0]) != "original" {
		t.Errorf("row aliases the driver buffer: became %q after reuse", row[0])
	}
}

func TestCopyRowHandlesEmptyInput(t *testing.T) {
	row, used := copyRow(nil)
	if len(row) != 0 || used != 0 {
		t.Errorf("copyRow(nil) = %v, %d", row, used)
	}
}
