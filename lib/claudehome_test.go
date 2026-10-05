package rota

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// livingClaude is a claude account whose login Claude Code can keep: a
// refresh token and an expiry an hour away.
func livingClaude() *Account {
	return &Account{ID: 5, Provider: "claude", UUID: "u-5", Email: "five@x",
		Token: Token{Access: "A1", Refresh: "R1", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			Scopes: []string{"user:inference", "user:profile"}},
		Extra: map[string]string{claudeSubscription: "max", claudeRateLimitTier: "tier-20x", claudeRefreshUntil: "1999999999000"}}
}

// homeWith is a home whose credential store holds the given login entry.
func homeWith(login string) fstest.MapFS {
	return fstest.MapFS{claudeCredentials: &fstest.MapFile{Data: []byte(`{"claudeAiOauth":` + login + `}`)}}
}

// onDisk is a platform on which Stage itself keeps a login in a home: any
// but Windows, which keeps the environment route, and macOS, where only
// StagePlan and AdoptFrom do, because the login lives in the keychain there.
// The test sees that platform whichever it runs on.
func onDisk(t *testing.T) {
	t.Helper()
	oldStored, oldDisk := storedLoginPlatform, diskLoginPlatform
	storedLoginPlatform, diskLoginPlatform = true, true
	t.Cleanup(func() { storedLoginPlatform, diskLoginPlatform = oldStored, oldDisk })
}

func envNames(env []string) []string {
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		out = append(out, k)
	}
	return out
}

// With a home, a claude account runs on a login of its own: Claude Code is
// pointed at the home, and nothing that authenticates reaches it through the
// environment — not rota's token, and not one the person's shell exported.
func TestAClaudeAccountWithAHomeRunsOnALoginOfItsOwn(t *testing.T) {
	onDisk(t)
	home := t.TempDir()
	a := livingClaude()
	a.Staged = stagedNone
	cmd, err := Stage(a, home)
	if err != nil {
		t.Fatal(err)
	}
	inherited := []string{"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=shell", "ANTHROPIC_API_KEY=k",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=/elsewhere", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3",
		"CLAUDE_CODE_OAUTH_SCOPES=user:inference", "CLAUDE_CONFIG_DIR=/person"}
	env := Environ(inherited, cmd)
	for _, e := range env {
		if strings.Contains(e, "A1") || strings.Contains(e, "R1") || strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("no token may reach the child on the stored route: %v", env)
		}
	}
	for _, gone := range []string{"ANTHROPIC_API_KEY", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_SCOPES"} {
		if slices.Contains(envNames(env), gone) {
			t.Fatalf("%s must be dropped: %v", gone, env)
		}
	}
	if got := configDirsIn(env); len(got) != 1 || got[0] != home {
		t.Fatalf("exactly one configuration directory, the home: %v", got)
	}
	raw, err := os.ReadFile(filepath.Join(home, claudeCredentials))
	if err != nil {
		t.Fatal(err)
	}
	l, ok := readClaudeLogin(os.DirFS(home))
	if !ok || l.AccessToken != "A1" || l.RefreshToken != "R1" || int64(l.ExpiresAt) != a.Token.ExpiresAt ||
		l.SubscriptionType != "max" || l.RateLimitTier != "tier-20x" || len(l.Scopes) != 2 || l.ClientID != claudeClientID {
		t.Fatalf("the store holds the account's login: %s", raw)
	}
	if fi, _ := os.Stat(filepath.Join(home, claudeCredentials)); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("the store is the owner's alone: %v", fi.Mode())
	}
	if a.Staged != fingerprint("R1") {
		t.Fatal("staging records which login the home holds")
	}
}

func configDirsIn(env []string) []string {
	var out []string
	for _, e := range env {
		if k, v, _ := strings.Cut(e, "="); k == "CLAUDE_CONFIG_DIR" {
			out = append(out, v)
		}
	}
	return out
}

// Without a home there is nowhere to keep a login, and the token goes in
// the environment as it always did — the long one when it is worth using.
// Everything competing is still dropped; the token variable is set, so the
// child sees rota's and no other.
func TestWithoutAHomeTheTokenTravelsInTheEnvironment(t *testing.T) {
	a := livingClaude()
	cmd, err := Stage(a, "")
	if err != nil {
		t.Fatal(err)
	}
	env := Environ([]string{"CLAUDE_CODE_OAUTH_TOKEN=shell", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=4",
		"CLAUDE_CODE_SUBSCRIPTION_TYPE=pro", "CLAUDE_CODE_RATE_LIMIT_TIER=x"}, cmd)
	if env[0] != "CLAUDE_CODE_OAUTH_TOKEN=A1" || slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=shell") {
		t.Fatalf("rota's token and only rota's: %v", env)
	}
	for _, gone := range []string{"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_SUBSCRIPTION_TYPE", "CLAUDE_CODE_RATE_LIMIT_TIER"} {
		if slices.Contains(envNames(env), gone) {
			t.Fatalf("%s must be dropped: %v", gone, env)
		}
	}
	if len(configDirsIn(env)) != 0 {
		t.Fatalf("no home, no configuration directory of rota's: %v", env)
	}
	a.Long = &LongToken{Access: "LONG", ExpiresAt: time.Now().Add(300 * 24 * time.Hour).UnixMilli()}
	if cmd, _ := Stage(a, ""); cmd.Env[0] != "CLAUDE_CODE_OAUTH_TOKEN=LONG" {
		t.Fatalf("the long token wins on this route: %v", cmd.Env)
	}
}

// The stored route is for a login Claude Code can keep. A bare token, a
// token with no expiry, or a dead login with a long token beside it all go
// the old way — and a dead one with nothing beside it is refused.
func TestOnlyALoginClaudeCodeCanKeepIsStoredInTheHome(t *testing.T) {
	home := t.TempDir()
	future := time.Now().Add(300 * 24 * time.Hour).UnixMilli()
	for _, c := range []struct {
		what string
		a    *Account
	}{
		{"no refresh token", &Account{Provider: "claude", Token: Token{Access: "A", ExpiresAt: future}}},
		{"no expiry", &Account{Provider: "claude", Token: Token{Access: "A", Refresh: "R"}}},
		{"dead, with a long token", &Account{Provider: "claude", Dead: true,
			Token: Token{Access: "A", Refresh: "R", ExpiresAt: future}, Long: &LongToken{Access: "LONG", ExpiresAt: future}}},
	} {
		if StoresLogin(c.a, home) {
			t.Fatalf("%s: must not be stored", c.what)
		}
		cmd, err := Stage(c.a, home)
		if err != nil || !strings.HasPrefix(cmd.Env[0], "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Fatalf("%s: the environment route: %+v %v", c.what, cmd, err)
		}
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("the environment route writes nothing: %v", entries)
	}
}

// Windows keeps the route it has always had, home or no home.
func TestWindowsKeepsTheEnvironmentRoute(t *testing.T) {
	old := storedLoginPlatform
	storedLoginPlatform = false
	t.Cleanup(func() { storedLoginPlatform = old })
	home := t.TempDir()
	a := livingClaude()
	a.ConfigDir = home
	cmd, err := Stage(a, home)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Env[0] != "CLAUDE_CODE_OAUTH_TOKEN=A1" || !slices.Equal(configDirsIn(cmd.Env), []string{home}) {
		t.Fatalf("token in the environment, and the account's own directory once: %v", cmd.Env)
	}
	if _, err := os.Stat(filepath.Join(home, claudeCredentials)); !os.IsNotExist(err) {
		t.Fatalf("nothing is written on Windows: %v", err)
	}
}

// A plan writes nothing, and plans the store only when the home does not
// already hold the account's current login.
func TestAPlanCarriesTheLoginOnlyWhenTheHomeLacksIt(t *testing.T) {
	if !storedLoginPlatform {
		t.Skip("this platform keeps the environment route")
	}
	home := t.TempDir()
	a := livingClaude()
	a.Staged = stagedNone
	cmd, files, err := StagePlan(context.Background(), a, home)
	if err != nil || len(files) != 1 || files[0].Path != claudeCredentials || files[0].Mode != 0o600 {
		t.Fatalf("one private store: %+v %v", files, err)
	}
	if !slices.Contains(cmd.Drop, "CLAUDE_CODE_OAUTH_TOKEN") || !slices.Equal(configDirsIn(cmd.Env), []string{home}) {
		t.Fatalf("the stored route's command: %+v", cmd)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("a plan writes nothing: %v", entries)
	}
	a.Staged = fingerprint(a.Token.Refresh)
	if _, files, _ := StagePlan(context.Background(), a, home); len(files) != 0 {
		t.Fatalf("a home holding the current login needs nothing: %+v", files)
	}
}

// A login written into a home never says less than a login: no empty
// refresh token — Claude Code's mark of a dead one — and no missing expiry.
func TestALoginIsNeverWrittenWithoutItsRefreshToken(t *testing.T) {
	for _, a := range []*Account{
		{Provider: "claude", Token: Token{Access: "A", ExpiresAt: 1}},
		{Provider: "claude", Token: Token{Access: "A", Refresh: "R"}},
	} {
		if _, err := claudeCredentialDoc(a); err == nil {
			t.Fatalf("%+v: must refuse", a.Token)
		}
	}
}

// Adoption reads the home's store into the account, and what it finds
// decides what the account believes about the home.
func TestAdoptionReadsWhatTheHomeHolds(t *testing.T) {
	later := time.Now().Add(2 * time.Hour).UnixMilli()
	earlier := time.Now().Add(-time.Hour).UnixMilli()
	login := func(refresh string, exp int64, until string) string {
		return `{"accessToken":"A-` + refresh + `","refreshToken":"` + refresh + `","expiresAt":` +
			strconv.FormatInt(exp, 10) + `,"scopes":["user:inference"],"subscriptionType":"pro","rateLimitTier":"t2",` +
			`"refreshTokenExpiresAt":` + until + `,"clientId":"c-1"}`
	}
	const same = "1999999999000"
	blank := `{"accessToken":"","refreshToken":"","expiresAt":0}`

	t.Run("nothing there", func(t *testing.T) {
		for _, fsys := range []fstest.MapFS{{}, {claudeCredentials: {Data: []byte(`{"mcpOAuth":{}}`)}},
			{claudeCredentials: {Data: []byte(`{"claudeAiOauth":null}`)}}} {
			a := livingClaude()
			a.Staged = fingerprint("R1")
			if err := AdoptFrom(a, fsys); err != nil || a.Staged != stagedNone || a.Token.Refresh != "R1" {
				t.Fatalf("an empty home is recorded so the next staging writes: %+v %v", a, err)
			}
		}
	})
	t.Run("there and unreadable", func(t *testing.T) {
		for _, body := range []string{"", "{nope", `{"claudeAiOauth":{"refreshToken":"R9"`, "[1]", `{"claudeAiOauth":"x"}`} {
			a := livingClaude()
			a.Staged = fingerprint("R1")
			before := *a
			err := AdoptFrom(a, fstest.MapFS{claudeCredentials: {Data: []byte(body)}})
			if !errors.Is(err, ErrUnreadableLogin) || a.Staged != before.Staged || a.Token.Refresh != before.Token.Refresh || a.Token.Access != before.Token.Access || a.Dead {
				t.Fatalf("%q is not an empty store; nothing changes: %+v %v", body, a, err)
			}
		}
	})
	t.Run("blanked current login is dead", func(t *testing.T) {
		a := livingClaude()
		a.Staged = fingerprint("R1")
		if err := AdoptFrom(a, homeWith(blank)); err != nil || !a.Dead || !strings.Contains(a.DeadReason, "Claude Code") {
			t.Fatalf("Claude Code refused this very login: %+v %v", a, err)
		}
	})
	t.Run("blanked older login is just empty", func(t *testing.T) {
		a := livingClaude()
		a.Staged = stagedNone
		if err := AdoptFrom(a, homeWith(blank)); err != nil || a.Dead || a.Staged != stagedNone {
			t.Fatalf("an older login the home gave up on says nothing about this one: %+v %v", a, err)
		}
	})
	t.Run("in sync", func(t *testing.T) {
		a := livingClaude()
		a.Staged = ""
		if err := AdoptFrom(a, homeWith(login("R1", later, same))); err != nil || a.Staged != fingerprint("R1") ||
			a.Token.Access != "A-R1" || a.Token.ExpiresAt != later {
			t.Fatalf("the same refresh token is in sync, and a newer access token is taken: %+v %v", a, err)
		}
	})
	t.Run("rotated by Claude Code", func(t *testing.T) {
		a := livingClaude()
		a.Staged = fingerprint("R1")
		if err := AdoptFrom(a, homeWith(login("R2", later, same))); err != nil {
			t.Fatal(err)
		}
		if a.Token.Refresh != "R2" || a.Token.Access != "A-R2" || a.Token.ExpiresAt != later || a.Staged != fingerprint("R2") ||
			a.Extra[claudeSubscription] != "pro" || a.Extra[claudeRateLimitTier] != "t2" ||
			a.Extra[claudeRefreshUntil] != same || a.Extra[claudeClient] != "c-1" {
			t.Fatalf("the rotation and everything around it are taken: %+v", a)
		}
	})
	t.Run("rotated but older", func(t *testing.T) {
		a := livingClaude()
		a.Staged = fingerprint("R1")
		if err := AdoptFrom(a, homeWith(login("R0", earlier, same))); err != nil || a.Token.Refresh != "R1" {
			t.Fatalf("a login that expires before the account's is not newer: %+v %v", a, err)
		}
		if a.Staged != stagedNone {
			t.Fatalf("and the home is behind, so the next staging writes the current one: %q", a.Staged)
		}
		if _, files, _ := (claudeProvider{}).Plan(context.Background(), a, "/h"); storedLoginPlatform && len(files) != 1 {
			t.Fatalf("a write is planned: %+v", files)
		}
	})
	t.Run("what rota wrote before a refresh of its own", func(t *testing.T) {
		for _, exp := range []int64{later, earlier} {
			a := livingClaude()
			a.Staged = fingerprint("R0")
			if err := AdoptFrom(a, homeWith(login("R0", exp, same))); err != nil || a.Token.Refresh != "R1" {
				t.Fatalf("behind, not a rotation: %+v %v", a, err)
			}
			if a.Staged != fingerprint("R0") || !ClaudeHomeSpent(a, homeWith(login("R0", exp, same))) {
				t.Fatalf("and still recorded as rota's own write, which proves it spent: %q", a.Staged)
			}
			if _, files, _ := (claudeProvider{}).Plan(context.Background(), a, "/h"); storedLoginPlatform && len(files) != 1 {
				t.Fatalf("a write is planned: %+v", files)
			}
		}
	})
	t.Run("a rotation when neither side knows when the login ends", func(t *testing.T) {
		a := livingClaude()
		delete(a.Extra, claudeRefreshUntil)
		a.Staged = fingerprint("R1")
		body := `{"accessToken":"A-R2","refreshToken":"R2","expiresAt":` + strconv.FormatInt(later, 10) + `}`
		if err := AdoptFrom(a, homeWith(body)); err != nil || a.Token.Refresh != "R2" || a.Staged != fingerprint("R2") {
			t.Fatalf("absent on both sides is the same login rotated: %+v %v", a, err)
		}
	})
	t.Run("an older login after a fresh rota login", func(t *testing.T) {
		a := livingClaude()
		a.Staged = stagedNone
		for _, until := range []string{same, "1888888888000"} {
			if err := AdoptFrom(a, homeWith(login("R0", later, until))); err != nil || a.Token.Refresh != "R1" || a.Staged != stagedNone {
				t.Fatalf("what the home held before the login is older, whatever it says: %+v %v", a, err)
			}
		}
	})
	t.Run("a new login is handed back, not taken", func(t *testing.T) {
		for _, until := range []string{"1888888888000", "null"} {
			a := livingClaude()
			a.Staged = fingerprint("R1")
			err := AdoptFrom(a, homeWith(login("R9", later, until)))
			var nl *NewLogin
			if !errors.As(err, &nl) || !errors.Is(err, ErrNewLogin) || nl.Access != "A-R9" {
				t.Fatalf("another refreshTokenExpiresAt is somebody signing in (%s): %v", until, err)
			}
			if a.Token.Refresh != "R1" || a.Staged != fingerprint("R1") {
				t.Fatalf("and nothing of it is taken until somebody checks: %+v", a)
			}
		}
	})
	t.Run("a new login that is this account's", func(t *testing.T) {
		a := livingClaude()
		a.Staged = fingerprint("R1")
		var nl *NewLogin
		errors.As(AdoptFrom(a, homeWith(login("R9", later, "1888888888000"))), &nl)
		nl.Accept(a)
		if a.Token.Refresh != "R9" || a.Staged != fingerprint("R9") || a.Extra[claudeRefreshUntil] != "1888888888000" {
			t.Fatalf("accepted: %+v", a)
		}
	})
	t.Run("a new login that is somebody else's", func(t *testing.T) {
		a := livingClaude()
		a.Staged = fingerprint("R1")
		var nl *NewLogin
		errors.As(AdoptFrom(a, homeWith(login("R9", later, "1888888888000"))), &nl)
		before := a.Token
		err := nl.Refuse(a, &Identity{UUID: "u-other", Email: "other@x"})
		if !errors.Is(err, ErrForeignLogin) || !strings.Contains(err.Error(), "other@x") || !strings.Contains(err.Error(), "five@x") {
			t.Fatalf("refused, naming both: %v", err)
		}
		if a.Token.Refresh != before.Refresh || a.Token.Access != before.Access || a.Staged != stagedNone {
			t.Fatalf("the account's own tokens stay, and the home is marked for its own login: %+v", a)
		}
	})
	t.Run("a dead account's home holding a living login", func(t *testing.T) {
		a := livingClaude()
		a.Dead, a.DeadReason = true, "invalid_grant"
		a.Staged = fingerprint("R1")
		var nl *NewLogin
		if !errors.As(AdoptFrom(a, homeWith(login("R3", later, same))), &nl) {
			t.Fatal("any living login in a dead account's home is a new login")
		}
		nl.Accept(a)
		if a.Dead || a.DeadReason != "" || a.Token.Refresh != "R3" {
			t.Fatalf("somebody signed it in again: %+v", a)
		}
	})
}

// The credential store is more than the login. A write replaces the login
// and keeps every other entry where it was, exactly as it was.
func TestWritingTheLoginKeepsEverythingElseInTheStore(t *testing.T) {
	existing := []byte(`{"mcpOAuth":{"srv|1":{"accessToken":"m","x":[1, 2]}},"claudeAiOauth":{"refreshToken":"old"},` +
		`"trustedDeviceToken":"tdt","pluginSecrets":{"p":"s"}}`)
	staged := []byte(`{"claudeAiOauth":{"refreshToken":"new","expiresAt":5}}`)
	merged, err := MergeClaudeCredentials(existing, staged)
	if err != nil {
		t.Fatal(err)
	}
	got := objectMembers(merged)
	var names []string
	for _, m := range got {
		names = append(names, m.name)
	}
	if !slices.Equal(names, []string{"mcpOAuth", "claudeAiOauth", "trustedDeviceToken", "pluginSecrets"}) {
		t.Fatalf("every entry, in its place: %v", names)
	}
	if string(got[0].value) != `{"srv|1":{"accessToken":"m","x":[1, 2]}}` || string(got[2].value) != `"tdt"` {
		t.Fatalf("siblings unchanged: %s", merged)
	}
	if string(got[1].value) != `{"refreshToken":"new","expiresAt":5}` {
		t.Fatalf("the login replaced: %s", merged)
	}
	for _, empty := range [][]byte{nil, []byte(""), []byte("{broken"), []byte(`[1]`), []byte(`{} trailing`)} {
		out, err := MergeClaudeCredentials(empty, staged)
		if err != nil || string(out) != `{"claudeAiOauth":{"refreshToken":"new","expiresAt":5}}` {
			t.Fatalf("%q reads as an empty store: %s %v", empty, out, err)
		}
	}
	twice, _ := MergeClaudeCredentials([]byte(`{"claudeAiOauth":1,"a":2,"claudeAiOauth":3}`), staged)
	if strings.Count(string(twice), claudeOAuthKey) != 1 {
		t.Fatalf("one login: %s", twice)
	}
	if _, err := MergeClaudeCredentials(existing, []byte(`{}`)); err == nil {
		t.Fatal("a plan without a login is a mistake")
	}
}

// Launch keeps what the store in the home holds beside the login.
func TestStagingIntoAHomeKeepsItsOtherSecrets(t *testing.T) {
	onDisk(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, claudeCredentials), []byte(`{"mcpOAuth":{"k":"v"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := livingClaude()
	a.Staged = stagedNone
	if _, err := Stage(a, home); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, claudeCredentials))
	if !strings.Contains(string(raw), `"mcpOAuth":{"k":"v"}`) || !strings.Contains(string(raw), `"refreshToken":"R1"`) {
		t.Fatalf("the MCP tokens survive: %s", raw)
	}
}

// A home is read before it is used, and a login somebody made inside it is
// not written over by the SDK's own staging: it comes back to the caller,
// who has to find out whose it is first.
func TestStagingHandsBackALoginSomebodyMadeInTheHome(t *testing.T) {
	onDisk(t)
	home := t.TempDir()
	foreign := `{"claudeAiOauth":{"accessToken":"X","refreshToken":"RX","refreshTokenExpiresAt":1777777777000,"expiresAt":` +
		strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10) + `}}`
	os.WriteFile(filepath.Join(home, claudeCredentials), []byte(foreign), 0o600)
	a := livingClaude()
	a.Staged = fingerprint("R1")
	if _, err := Stage(a, home); !errors.Is(err, ErrNewLogin) {
		t.Fatalf("handed back: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(home, claudeCredentials)); string(raw) != foreign {
		t.Fatalf("and left alone: %s", raw)
	}
}

// Whether a home holds a login Claude Code can sign a new process in with.
func TestAHomeHoldsALoginOnlyWithARefreshTokenAndAnExpiry(t *testing.T) {
	a := &Account{Provider: "claude"}
	for login, want := range map[string]bool{
		`{"refreshToken":"R","expiresAt":5}`:                 true,
		`{"accessToken":"","refreshToken":"","expiresAt":0}`: false,
		`{"accessToken":"A","refreshToken":"R"}`:             false,
		`{"accessToken":"A","expiresAt":5}`:                  false,
	} {
		if got := HoldsLogin(a, homeWith(login)); got != want {
			t.Fatalf("%s: got %v", login, got)
		}
	}
	if HoldsLogin(a, fstest.MapFS{}) || HoldsLogin(&Account{Provider: "codex"}, homeWith(`{"refreshToken":"R","expiresAt":5}`)) {
		t.Fatal("no store holds no login, and only a shared home is asked")
	}
	if !SharedHome("claude") || SharedHome("codex") || SharedHome("grok") || SharedHome("nope") {
		t.Fatal("claude's home is shared by many processes; the others are not")
	}
}

// The profile is read once at login, and a profile that cannot be read
// never fails one. A long login never asks it: its token may not read it.
func TestALoginReadsThePlanAndSurvivesWithoutIt(t *testing.T) {
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	setURL(t, &ClaudeEndpoints.Profile, f.URL+"/profile")
	f.reply["/token"] = func(*http.Request, map[string]any) (int, any) {
		return 200, map[string]any{"access_token": "A", "refresh_token": "R", "expires_in": 3600,
			"account": map[string]string{"uuid": "u1", "email_address": "e@x"}}
	}
	f.reply["/profile"] = func(*http.Request, map[string]any) (int, any) { return 500, nil }
	p, _ := Lookup("claude")
	_, state, _ := p.Begin(context.Background())
	tok, err := p.Complete(context.Background(), "CODE#ST", state)
	if err != nil || tok.Access != "A" || tok.Extra[claudeSubscription] != "" {
		t.Fatalf("the login stands: %+v %v", tok, err)
	}
	if f.calls["/profile"] != 1 {
		t.Fatalf("asked once: %d", f.calls["/profile"])
	}
	f.reply["/profile"] = func(*http.Request, map[string]any) (int, any) {
		return 200, map[string]any{"organization": map[string]string{"organization_type": "claude_free"}}
	}
	if tok, _ := p.Complete(context.Background(), "CODE#ST", state); tok.Extra[claudeSubscription] != "" {
		t.Fatalf("a plan rota has no word for is left unsaid: %v", tok.Extra)
	}
	_, longState, _ := p.(LongLived).BeginLong(context.Background())
	before := f.calls["/profile"]
	if _, err := p.(LongLived).CompleteLong(context.Background(), "CODE#ST", longState); err != nil {
		t.Fatal(err)
	}
	if f.calls["/profile"] != before {
		t.Fatal("a long login must not ask the profile")
	}
}

// Remote Control is Claude Code's, and an account of another provider is
// refused it rather than told something it cannot obey.
func TestRemoteControlIsClaudeCodesSetting(t *testing.T) {
	if err := (&Account{Provider: "claude", RemoteControl: true}).CheckProject(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"codex", "grok", "kimi"} {
		if err := (&Account{Provider: p, RemoteControl: true}).CheckProject(); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	raw, _ := Encode(&Account{Provider: "claude"})
	if strings.Contains(string(raw), "remoteControl") {
		t.Fatalf("omitted when off: %s", raw)
	}
}

// A fresh login is a new lineage: when the last one's refresh token ended
// is forgotten, so it is neither written into Claude Code's store as this
// one's nor makes a rotation of this login look like somebody else's.
func TestAFreshLoginForgetsWhenTheLastOneEnded(t *testing.T) {
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	setURL(t, &ClaudeEndpoints.Profile, f.URL+"/profile")
	f.reply["/token"] = func(*http.Request, map[string]any) (int, any) {
		return 200, map[string]any{"access_token": "A", "refresh_token": "R", "expires_in": 3600,
			"account": map[string]string{"uuid": "u-5"}}
	}
	f.reply["/profile"] = func(*http.Request, map[string]any) (int, any) { return 500, nil }
	p, _ := Lookup("claude")
	_, state, _ := p.Begin(context.Background())
	tok, err := p.Complete(context.Background(), "CODE#ST", state)
	if err != nil {
		t.Fatal(err)
	}
	a := livingClaude()
	a.Apply(tok)
	if _, ok := a.Extra[claudeRefreshUntil]; ok || a.Extra[claudeSubscription] != "max" {
		t.Fatalf("the old expiry is forgotten, the plan kept: %v", a.Extra)
	}
}

// On macOS Stage itself never keeps a login in a home, and Adopt reads none
// back: the login lives in a keychain item there, which neither can see. An
// application reaches the stored route there through StagePlan and
// AdoptFrom, handing over the item's content itself.
func TestOnMacOSOnlyThePlanKeepsALoginInAHome(t *testing.T) {
	oldStored, oldDisk := storedLoginPlatform, diskLoginPlatform
	storedLoginPlatform, diskLoginPlatform = true, false
	t.Cleanup(func() { storedLoginPlatform, diskLoginPlatform = oldStored, oldDisk })
	home := t.TempDir()
	a := livingClaude()
	a.Staged = stagedNone
	cmd, err := Stage(a, home)
	if err != nil || cmd.Env[0] != "CLAUDE_CODE_OAUTH_TOKEN=A1" {
		t.Fatalf("the environment route: %+v %v", cmd, err)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("nothing written: %v", entries)
	}
	os.WriteFile(filepath.Join(home, claudeCredentials), []byte(`{"claudeAiOauth":{"refreshToken":"R9","expiresAt":1}}`), 0o600)
	if err := Adopt(a, home); err != nil || a.Token.Refresh != "R1" || a.Staged != stagedNone {
		t.Fatalf("Adopt reads nothing: %+v %v", a, err)
	}
	if _, files, err := StagePlan(context.Background(), a, home); err != nil || len(files) != 1 || !StoresLogin(a, home) {
		t.Fatalf("the plan still keeps the login in the home: %+v %v", files, err)
	}
}

// A hermetic run takes an empty directory instead of the home, so a command
// staged on the stored route — no token, the login in the home — is given
// the account's token on the way, or Claude Code would start signed in as
// nobody. The long-lived token when it is worth using.
func TestAHermeticRunOfAStoredRouteCommandCarriesAToken(t *testing.T) {
	a := livingClaude()
	stored := identify(a, claudeStoredCommand("/home/a"))
	got := hermeticCommand(withToken(a, stored), "/tmp/h")
	if !slices.Contains(got.Env, "CLAUDE_CODE_OAUTH_TOKEN=A1") || !slices.Equal(configDirsIn(got.Env), []string{"/tmp/h"}) {
		t.Fatalf("a token and the throwaway directory: %v", got.Env)
	}
	a.Long = &LongToken{Access: "LONG", ExpiresAt: time.Now().Add(300 * 24 * time.Hour).UnixMilli()}
	if got := withToken(a, stored); !slices.Contains(got.Env, "CLAUDE_CODE_OAUTH_TOKEN=LONG") {
		t.Fatalf("the long one: %v", got.Env)
	}
	given := &Command{Bin: "/x/claude", Env: []string{"CLAUDE_CODE_OAUTH_TOKEN=mine"}}
	if got := withToken(a, given); got != given {
		t.Fatal("a command that carries a token is left as it is")
	}
	if got := withToken(&Account{Provider: "codex"}, stored); got != stored {
		t.Fatal("only claude")
	}
}

// Joining a home is the stored route's command whatever the account's state
// is, with nothing written — and nothing on a platform that keeps the
// environment route.
func TestJoiningAHomeWritesNothingAndHandsOverNothing(t *testing.T) {
	home := t.TempDir()
	a := livingClaude()
	a.Dead = true
	cmd := JoinHome(a, home)
	if storedLoginPlatform && (cmd == nil || !slices.Equal(configDirsIn(cmd.Env), []string{home}) ||
		slices.ContainsFunc(cmd.Env, func(e string) bool { return strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") })) {
		t.Fatalf("pointed at the home, no token: %+v", cmd)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("nothing written: %v", entries)
	}
	old := storedLoginPlatform
	storedLoginPlatform = false
	defer func() { storedLoginPlatform = old }()
	if JoinHome(a, home) != nil || JoinHome(&Account{Provider: "codex"}, home) != nil {
		t.Fatal("nothing to join on Windows, or for a provider whose home is not shared")
	}
}

// A home holds a spent login only when it holds the very login rota recorded
// writing there and the account has refreshed away since: not nothing, not
// the account's current login, not anything else — and with nothing
// recorded, an older login proves nothing.
func TestOnlyTheLoginRotaWroteAndRefreshedAwayIsSpent(t *testing.T) {
	a := livingClaude()
	login := func(refresh string) string {
		return `{"accessToken":"A-` + refresh + `","refreshToken":"` + refresh + `","expiresAt":5}`
	}
	a.Staged = fingerprint("R0")
	for i, c := range []struct {
		fsys fstest.MapFS
		want bool
	}{
		{fstest.MapFS{}, false},
		{homeWith(`{"accessToken":"","refreshToken":"","expiresAt":0}`), false},
		{fstest.MapFS{claudeCredentials: {Data: []byte("{torn")}}, false},
		{homeWith(login("R1")), false},
		{homeWith(login("R0")), true},
		{homeWith(login("R9")), false},
	} {
		if got := ClaudeHomeSpent(a, c.fsys); got != c.want {
			t.Fatalf("case %d: got %v", i, got)
		}
	}
	a.Staged = stagedNone
	if ClaudeHomeSpent(a, homeWith(login("R0"))) {
		t.Fatal("with nothing recorded, an older login proves nothing")
	}
}
