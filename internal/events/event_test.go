package events

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTypesAreEveryTypeButGapInOrder(t *testing.T) {
	want := []string{
		"account.threshold", "account.reset", "account.dead", "account.rotated",
		"session.started", "session.ended", "session.attention", "session.quiet",
		"viewer.joined", "viewer.left", "daemon.started", "daemon.stopped", "test",
	}
	if !slices.Equal(Types, want) {
		t.Fatalf("Types = %q, want %q", Types, want)
	}
	consts := []string{
		AccountThreshold, AccountReset, AccountDead, AccountRotated,
		SessionStarted, SessionEnded, SessionAttention, SessionQuiet,
		ViewerJoined, ViewerLeft, DaemonStarted, DaemonStopped, Test,
	}
	if !slices.Equal(consts, want) {
		t.Fatalf("the constants are %q, want %q", consts, want)
	}
	if Gap != "gap" || slices.Contains(Types, Gap) {
		t.Fatalf("Gap = %q, and Types must not hold it", Gap)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		patterns []string
		typ      string
		want     bool
	}{
		{nil, "session.ended", true},
		{[]string{}, "session.ended", true},
		{[]string{"*"}, "test", true},
		{[]string{"account.*"}, "account.reset", true},
		{[]string{"account.*"}, "session.ended", false},
		{[]string{"session.ended"}, "session.ended", true},
		{[]string{"session.ended"}, "session.started", false},
		{[]string{"account.*", "session.ended"}, "session.ended", true},
		{nil, Gap, false},
		{[]string{"*"}, Gap, false},
		{[]string{"gap"}, Gap, false},
		{[]string{"gap.*"}, Gap, false},
	}
	for _, c := range cases {
		if got := Match(c.patterns, c.typ); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.patterns, c.typ, got, c.want)
		}
	}
}

func TestCheckPatterns(t *testing.T) {
	err := CheckPatterns([]string{"account.*", "session.atention"})
	want := `on: "session.atention" names no event; the events are ` + strings.Join(Types, ", ")
	if err == nil || err.Error() != want {
		t.Fatalf("CheckPatterns(session.atention) = %v, want %q", err, want)
	}
	if !strings.HasPrefix(want, `on: "session.atention" names no event; the events are account.threshold, `) ||
		!strings.HasSuffix(want, ", test") {
		t.Fatalf("the list of events reads %q", want)
	}
	for _, bad := range []string{"account.", "", "*.ended", "nothing.*", "gap", "test.*", "account.**", "Account.reset"} {
		if CheckPatterns([]string{bad}) == nil {
			t.Errorf("CheckPatterns(%q) took it", bad)
		}
	}
	for _, good := range [][]string{{"*"}, {"account.*", "session.ended"}, nil, {"test"}, {"viewer.*", "daemon.*"}} {
		if err := CheckPatterns(good); err != nil {
			t.Errorf("CheckPatterns(%q) = %v", good, err)
		}
	}
}

func TestSentence(t *testing.T) {
	cases := []struct {
		e    Event
		want string
	}{
		{Event{Name: "refactor", Text: "rang its bell"}, "refactor rang its bell"},
		{Event{Text: "A test event from local"}, "A test event from local"},
		{Event{Name: "refactor"}, "refactor"},
	}
	for _, c := range cases {
		if got := Sentence(c.e); got != c.want {
			t.Errorf("Sentence(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
}

func TestCleanRemovesControlsAndBounds(t *testing.T) {
	if got := Clean("a\x1b[31mb\nc\x7f", 200); got != "a [31mb c " {
		t.Errorf("Clean of controls = %q", got)
	}
	if got := Clean("x\u009by\u0085z", 200); got != "x y z" {
		t.Errorf("Clean of C1 = %q", got)
	}
	if got := Clean("ok\xffthen", 200); !utf8.ValidString(got) || strings.ContainsRune(got, 0xff) {
		t.Errorf("Clean of invalid UTF-8 = %q", got)
	}
	if got := Clean("work@x.com", 200); got != "work@x.com" {
		t.Errorf("Clean changed a plain name: %q", got)
	}

	long := strings.Repeat("€", 100) // 300 bytes, three to a rune
	got := Clean(long, 200)
	if len(got) > 200 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("Clean(300 bytes, 200) = %d bytes %q", len(got), got)
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Fatalf("Clean did not keep a prefix: %q", got)
	}
	if got := Clean(strings.Repeat("a", 200), 200); got != strings.Repeat("a", 200) {
		t.Errorf("Clean cut a string that fit: %d bytes", len(got))
	}
	if got := Clean(strings.Repeat("a", 300), 0); len(got) != 300 {
		t.Errorf("Clean with no bound cut to %d bytes", len(got))
	}
}
