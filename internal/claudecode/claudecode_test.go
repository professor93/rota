package claudecode

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// The item's name is derived the way Claude Code derives it: the prefix and
// the first eight hex digits of the sha256 of the exact directory string.
func TestTheKeychainItemIsNamedForTheExactDirectory(t *testing.T) {
	got, ok := Service("/Users/someone/.rota/homes/claude-3")
	if !ok || got != "Claude Code-credentials-3099409d" {
		t.Fatalf("%q %v", got, ok)
	}
	if other, _ := Service("/Users/someone/.rota/homes/claude-3/"); other == got {
		t.Fatal("the exact string, not a cleaned path: a trailing slash is another item")
	}
	if _, ok := Service("/Users/sömeone/.rota/homes/claude-3"); ok {
		t.Fatal("a path that is not plain ASCII has no name rota can derive without normalising it")
	}
}

// The account is $USER when it is a plain name, and something else when not.
func TestTheKeychainAccountIsThePlainUserName(t *testing.T) {
	t.Setenv("USER", "ann.b-1")
	if Account() != "ann.b-1" {
		t.Fatal(Account())
	}
	t.Setenv("USER", "ann b")
	if a := Account(); a == "ann b" || a == "" {
		t.Fatalf("a name with a space is not used: %q", a)
	}
}

// Reading: exit 0 is the item minus security's one trailing newline, exit
// 44 is no item, anything else is an error. Deleting: 0 and 44 both mean
// gone. Nothing else is ever asked of security.
func TestTheKeychainIsReadAndDeletedAndNothingElse(t *testing.T) {
	var asked [][]string
	reply := func(out string, code int) {
		Security = func(_ context.Context, args ...string) ([]byte, int, error) {
			asked = append(asked, args)
			return []byte(out), code, nil
		}
	}
	t.Cleanup(StandIn)
	t.Setenv("USER", "ann")

	reply("{\"claudeAiOauth\":{}}\n", 0)
	raw, found, err := Read(context.Background(), "svc")
	if err != nil || !found || string(raw) != `{"claudeAiOauth":{}}` {
		t.Fatalf("%q %v %v", raw, found, err)
	}
	if want := []string{"find-generic-password", "-a", "ann", "-w", "-s", "svc"}; !slices.Equal(asked[0], want) {
		t.Fatalf("asked %v", asked[0])
	}
	reply("", Absent)
	if _, found, err := Read(context.Background(), "svc"); found || err != nil {
		t.Fatalf("44 is no item: %v %v", found, err)
	}
	reply("", 1)
	if _, _, err := Read(context.Background(), "svc"); err == nil {
		t.Fatal("any other status is an error")
	}
	for _, code := range []int{0, Absent} {
		reply("", code)
		if err := Delete(context.Background(), "svc"); err != nil {
			t.Fatalf("%d: %v", code, err)
		}
	}
	reply("", 36)
	if err := Delete(context.Background(), "svc"); err == nil {
		t.Fatal("a refusal is an error")
	}
	for _, args := range asked {
		if !strings.HasSuffix(args[0], "-generic-password") || strings.HasPrefix(args[0], "add") {
			t.Fatalf("rota never writes an item: %v", args)
		}
	}
}

// Inside a test binary the real programs refuse to run at all.
func TestTheRealRunnersRefuseInsideATest(t *testing.T) {
	if _, _, err := runSecurity(context.Background(), "find-generic-password"); err == nil {
		t.Fatal("security must not run from a test")
	}
	if err := runDaemonStop(context.Background(), t.TempDir(), nil); !errors.Is(err, errInTest) {
		t.Fatalf("claude must not run from a test: %v", err)
	}
}
