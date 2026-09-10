package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The listing gives its room to the numbers: the two widest headers are a
// mark and a short word, and each usage window sits on a line of its own
// under USAGE, so a row reads down rather than across.
func TestListPutsEachUsageWindowOnItsOwnLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	// A fresh reading, so the listing asks no provider anything.
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"},"quotaAt":%d,
		 "quota":{"windows":[{"name":"5h","percent":3,"primary":true},{"name":"7d","percent":29},{"name":"Fable","percent":48,"scoped":true}]}}]}`,
		time.Now().UnixMilli()))

	out, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 4 {
		t.Fatalf("a header, a row and two more windows:\n%s", out)
	}
	header, row, second, third := lines[0], lines[1], lines[2], lines[3]
	if fields := strings.Fields(header); len(fields) != 8 || fields[0] != "#" || fields[2] != "CLI" {
		t.Fatalf("header: %q", header)
	}
	if !strings.Contains(row, "a@x") || !strings.Contains(row, "5h 3%") || !strings.Contains(row, "  ok") || strings.Contains(row, "7d") {
		t.Fatalf("the row carries the account and its first window only: %q", row)
	}
	if strings.TrimSpace(second) != "7d 29%" || strings.TrimSpace(third) != "Fable 48%" {
		t.Fatalf("the other windows follow, one per line, nothing else on them: %q %q", second, third)
	}
	for i, line := range lines {
		if strings.TrimRight(line, " ") != line {
			t.Fatalf("line %d ends in padding: %q", i+1, line)
		}
	}
	col := strings.Index(header, "USAGE")
	if strings.Index(second, "7d") != col || strings.Index(third, "Fable") != col || strings.Index(row, "5h") != col {
		t.Fatalf("every window starts under USAGE (column %d):\n%s", col, out)
	}
}

// The short listing wears the same headers.
func TestShortListWearsTheSameHeaders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)
	out, _, code := call(t, "list", "--short")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	if fields := strings.Fields(strings.Split(out, "\n")[0]); len(fields) != 6 || fields[0] != "#" || fields[2] != "CLI" {
		t.Fatalf("header: %q", out)
	}
}
