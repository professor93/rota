package fakecli

import (
	"bytes"
	"strings"
	"testing"
)

// TestTheTtyFakePrintsAnyBytesAndShowsControlBytesItReads drives the tty
// fake the way a capture does: a print line writes exactly its bytes, with no
// newline, and a line with control bytes in it comes back with each one
// spelled out, so a test can see what arrived without the terminal acting on
// it.
func TestTheTtyFakePrintsAnyBytesAndShowsControlBytesItReads(t *testing.T) {
	in := strings.Join([]string{
		"print 1b5b3f3130343968",
		"print e4b896",
		"print zz",
		"a\x1bb\x7fc\x01",
		"plain words",
		"bye",
	}, "\n") + "\n"
	var out bytes.Buffer
	if code := atty(Spec{Tty: true}, strings.NewReader(in), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	_, got, ok := strings.Cut(out.String(), "\r\n")
	if !ok {
		t.Fatalf("no ready line: %q", out.String())
	}
	want := "\x1b[?1049h" + "世" +
		"bad print: zz\r\n" +
		`got:a\x1bb\x7fc\x01` + "\r\n" +
		"got:plain words\r\n" +
		"bye\r\n"
	if got != want {
		t.Fatalf("the fake wrote\n%q\nwant\n%q", got, want)
	}
}
