package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// seedLong writes a store with one claude account, optionally holding a
// long-lived token that expires at the given moment.
func seedLong(t *testing.T, until time.Time, extra string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	long := ""
	if !until.IsZero() {
		long = fmt.Sprintf(`,"long":{"accessToken":"LONG-SECRET","expiresAt":%d}`, until.UnixMilli())
	}
	doc := fmt.Sprintf(`{"accounts":[{"id":1,"provider":"claude","email":"a@b.c","uuid":"u1","order":1,`+
		`"token":{"accessToken":"SHORT","refreshToken":"r"}%s%s}],"nextId":2,"ordered":true}`, long, extra)
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// claudeTokenEndpoint points the provider at a fake that refuses every
// refresh, so a test can tell a launch that needed one from a launch that
// did not.
func claudeTokenEndpoint(t *testing.T) *int {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL + "/token"
	t.Cleanup(func() { rota.ClaudeEndpoints.Token = old })
	return &hits
}

func TestLoginLongPrintsItsOwnInstructions(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	out, _, code := call(t, "login", "--long")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	for _, want := range []string{
		"approve as the claude account the token is for",
		"scope=user%3Ainference&",
		"lasts a year",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("login --long must say %q:\n%s", want, out)
		}
	}
	// Only claude has one to give, and the refusal names the provider.
	_, errOut, code := call(t, "login", "codex", "--long")
	if code == 0 || !strings.Contains(errOut, "codex issues no long-lived token") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestLoginLongStoresTheTokenAndSaysWhoseItIs(t *testing.T) {
	home := seedLong(t, time.Time{}, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "LONG-SECRET", "expires_in": 31536000,
			"account": map[string]string{"uuid": "u1", "email_address": "a@b.c"}})
	}))
	defer srv.Close()
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL
	defer func() { rota.ClaudeEndpoints.Token = old }()

	out, _, code := call(t, "login", "--long")
	if code != 0 {
		t.Fatal(out)
	}
	id := strings.Fields(strings.Split(out, "Then: rota login ")[1])[0]
	out, _, code = call(t, "login", id, "CODE")
	day := time.Now().AddDate(1, 0, 0).Format(time.DateOnly)
	want := "long-lived token stored for #1 claude/a@b.c, good until " + day +
		"; every launch uses it from now on.\n"
	if code != 0 || out != want {
		t.Fatalf("%d\n got %q\nwant %q", code, out, want)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "accounts.json"))
	if !strings.Contains(string(raw), `"long"`) || !strings.Contains(string(raw), "LONG-SECRET") {
		t.Fatalf("not stored: %s", raw)
	}

	// It is not shown afterwards, anywhere; only its date is.
	out, _, _ = call(t, "set", "1")
	if !strings.Contains(out, "long token  good until "+day) || strings.Contains(out, "LONG-SECRET") {
		t.Fatalf("set: %q", out)
	}
	out, _, _ = call(t, "list", "--short", "--json")
	if !strings.Contains(out, `"long_until"`) || strings.Contains(out, "LONG-SECRET") {
		t.Fatalf("json: %q", out)
	}
}

func TestLoginLongRefusesAnIdentityThatIsNotAnAccount(t *testing.T) {
	seedLong(t, time.Time{}, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "LONG-SECRET", "expires_in": 31536000,
			"account": map[string]string{"uuid": "u9", "email_address": "someone@else"}})
	}))
	defer srv.Close()
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL
	defer func() { rota.ClaudeEndpoints.Token = old }()

	out, _, _ := call(t, "login", "--long")
	id := strings.Fields(strings.Split(out, "Then: rota login ")[1])[0]
	out, errOut, code := call(t, "login", id, "CODE")
	if code == 0 || !strings.Contains(errOut, "approved as someone@else") ||
		!strings.Contains(errOut, "rota login claude") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	list, _, _ := call(t, "list", "--short")
	if strings.Contains(list, "someone@else") {
		t.Fatalf("no account may be created: %s", list)
	}
}

func TestARunOnALongTokenNeedsNoRefresh(t *testing.T) {
	seedLong(t, time.Now().Add(200*24*time.Hour), "")
	hits := claudeTokenEndpoint(t)
	bin := t.TempDir()
	// The CLI prints what it was given, tail first: a test may say which
	// token arrived without ever writing one down whole.
	fakecli.Install(t, bin, "claude", fakecli.Lines(`[{"type":"result","result":"ANSWERED","session_id":"s-1"}]`))
	t.Setenv("PATH", bin)

	out, errOut, code := call(t, "run", "1", "hello")
	if code != 0 || out != "ANSWERED\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if *hits != 0 {
		t.Fatalf("the provider was asked to refresh %d times for a run that did not need it", *hits)
	}
}

func TestADeadAccountRunsWhenNamedIfItHasALongToken(t *testing.T) {
	seedLong(t, time.Now().Add(200*24*time.Hour), `,"dead":true,"deadReason":"invalid_grant: reused"`)
	claudeTokenEndpoint(t)
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(`[{"type":"result","result":"ANSWERED","session_id":"s-1"}]`))
	t.Setenv("PATH", bin)

	out, errOut, code := call(t, "run", "1", "hello")
	if code != 0 || out != "ANSWERED\n" {
		t.Fatalf("named by id: %d %q %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "its login is dead (invalid_grant: reused)") ||
		!strings.Contains(errOut, "usage is unknown") {
		t.Fatalf("it must say so once: %q", errOut)
	}
	// Without an id, the rotation still passes it by — a dead account is not
	// one to hand work to on its own.
	out, errOut, code = call(t, "run", "hello")
	if code == 0 || strings.Contains(out, "ANSWERED") {
		t.Fatalf("the rotation must still skip it: %d %q %q", code, out, errOut)
	}
}

func TestListSaysWhenALongTokenIsNearlyOverOrGone(t *testing.T) {
	for _, c := range []struct {
		what  string
		until time.Time
		says  string
	}{
		{"nearly over", time.Now().Add(12 * 24 * time.Hour), "expires on %s, in 11 days"},
		{"gone", time.Now().Add(-48 * time.Hour), "expired on %s"},
		{"plenty left", time.Now().Add(300 * 24 * time.Hour), ""},
	} {
		seedLong(t, c.until, "")
		out, _, code := call(t, "list", "--short")
		want := ""
		if c.says != "" {
			want = fmt.Sprintf(c.says, c.until.Format(time.DateOnly))
		}
		if code != 0 {
			t.Fatalf("%s: %d %q", c.what, code, out)
		}
		if want == "" {
			if strings.Contains(out, "long-lived token") {
				t.Fatalf("%s: nothing to say, said %q", c.what, out)
			}
			continue
		}
		if !strings.Contains(out, want) {
			t.Fatalf("%s: want %q in\n%s", c.what, want, out)
		}
		if strings.Contains(out, "LONG-SECRET") {
			t.Fatalf("%s: the token itself was printed", c.what)
		}
	}
}

func TestSetLongForgetThrowsTheTokenAway(t *testing.T) {
	home := seedLong(t, time.Now().Add(200*24*time.Hour), "")
	if _, errOut, code := call(t, "set", "1", "--long", "drop"); code == 0 ||
		!strings.Contains(errOut, "--long takes forget and nothing else") {
		t.Fatalf("%d %q", code, errOut)
	}
	out, errOut, code := call(t, "set", "1", "--long", "forget")
	if code != 0 || strings.Contains(out, "long token") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "accounts.json"))
	if strings.Contains(string(raw), "LONG-SECRET") {
		t.Fatalf("still there: %s", raw)
	}
	// And the ordinary credential is untouched: forgetting one key is not
	// logging the account out.
	if !strings.Contains(string(raw), `"accessToken": "SHORT"`) &&
		!strings.Contains(string(raw), `"accessToken":"SHORT"`) {
		t.Fatalf("the ordinary token must stay: %s", raw)
	}
}
