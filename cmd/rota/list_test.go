package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// cells is a bordered line without its frame: the words in it, in order.
func cells(line string) []string {
	return strings.Fields(strings.ReplaceAll(line, "│", " "))
}

// The listing is a framed table with a space of padding on each side of a
// cell and nothing more: the two widest headers are a mark and a short
// word, and each usage window sits on a line of its own under USAGE. Rows
// that span lines are ruled apart, so a row reads down rather than across.
func TestListIsAFramedTableWithOneUsageWindowPerLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	// Fresh readings, so the listing asks no provider anything.
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "quota":{"windows":[{"name":"5h","percent":3,"primary":true},{"name":"7d","percent":29},{"name":"Fable","percent":48,"scoped":true}]}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"},"quotaAt":%[1]d}]}`,
		time.Now().UnixMilli()))

	out, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 9 {
		t.Fatalf("a frame, a header, a ruled row of three lines, a row, a frame:\n%s", out)
	}
	top, header, rule, row, second, third, between, next, bottom := lines[0], lines[1], lines[2], lines[3], lines[4], lines[5], lines[6], lines[7], lines[8]
	if !strings.HasPrefix(top, "┌") || !strings.HasSuffix(top, "┐") || !strings.HasPrefix(bottom, "└") || !strings.HasSuffix(bottom, "┘") {
		t.Fatalf("framed top and bottom:\n%s", out)
	}
	if got := strings.Join(cells(header), " "); got != "# ID CLI ACCOUNT USAGE UNTIL CHECKED STATUS" {
		t.Fatalf("header: %q", header)
	}
	if !strings.HasPrefix(rule, "├") || !strings.Contains(rule, "┼") || !strings.HasSuffix(rule, "┤") {
		t.Fatalf("a rule under the header: %q", rule)
	}
	if !strings.Contains(row, "│ a@x") || !strings.Contains(row, "│ 5h 3%") || !strings.Contains(row, "│ ok") || strings.Contains(row, "7d") {
		t.Fatalf("the row carries the account and its first window only: %q", row)
	}
	if got := strings.Join(cells(second), " ") + "|" + strings.Join(cells(third), " "); got != "7d 29%|Fable 48%" {
		t.Fatalf("the other windows follow, one per line, nothing else on them: %q %q", second, third)
	}
	col := strings.Index(header, "USAGE")
	if strings.Index(row, "5h") != col || strings.Index(second, "7d") != col || strings.Index(third, "Fable") != col {
		t.Fatalf("every window starts under USAGE (byte %d):\n%s", col, out)
	}
	if !strings.HasPrefix(between, "├") || !strings.Contains(next, "│ b@x") {
		t.Fatalf("rows that span lines are ruled apart: %q %q", between, next)
	}
	// Minimal padding: one space each side of the widest cell, so the
	// header's ACCOUNT column is exactly as wide as the longest address.
	if !strings.Contains(header, "│ ACCOUNT │") || !strings.Contains(row, "│ a@x     │") {
		t.Fatalf("one space of padding around the widest cell in a column:\n%s", out)
	}
	width := utf8.RuneCountInString(top)
	for i, line := range lines[:9] {
		if utf8.RuneCountInString(line) != width {
			t.Fatalf("line %d is not the table's width %d: %q", i+1, width, line)
		}
	}
}

// The short listing wears the same frame and headers. Its rows are one line
// each, so nothing rules them apart.
func TestShortListWearsTheSameFrame(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"}}]}`)
	out, _, code := call(t, "list", "--short")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "┌") || !strings.HasPrefix(lines[2], "├") || !strings.HasPrefix(lines[5], "└") {
		t.Fatalf("frame, header, rule, two rows, frame — six lines:\n%s", out)
	}
	if got := strings.Join(cells(lines[1]), " "); got != "# ID CLI ACCOUNT USAGE CHECKED" {
		t.Fatalf("header: %q", lines[1])
	}
	if strings.HasPrefix(lines[4], "├") || !strings.Contains(lines[3], "a@x") || !strings.Contains(lines[4], "b@x") {
		t.Fatalf("one-line rows are not ruled apart:\n%s", out)
	}
}
