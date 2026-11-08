package store

// The refresh journal, and a refusal rota met that the home's login covered.
//
// A refusal of rota's that the home's login then covers is kept on the
// account, said, and named in a later death of that login; a later rotation
// forgets it. Every refresh rota makes, and every change a look at a home finds
// there, leaves one line in refresh.log beside the store, which holds no token.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

const replacedWarning = "rota presented a token the provider had already replaced; the home's login may have been revoked with it"

// journalOf is every line of the store's journal, each one JSON object.
func journalOf(t *testing.T, s *Store) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(storeDir(s), journalName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a journal line is one JSON object: %q", line)
		}
		for _, k := range []string{"ts", "pid", "version", "account", "provider", "path", "before", "after",
			"staged_before", "staged_after", "lock", "outcome"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("a journal line has %s: %q", k, line)
			}
		}
		if m["version"] != rota.Version || m["pid"] != float64(os.Getpid()) {
			t.Fatalf("a journal line names this rota: %q", line)
		}
		if _, err := time.Parse(time.RFC3339Nano, m["ts"].(string)); err != nil {
			t.Fatalf("ts is RFC 3339: %q", line)
		}
		out = append(out, m)
	}
	return out
}

// journalLine is the first line on path whose outcome is outcome, or nil.
func journalLine(lines []map[string]any, path, outcome string) map[string]any {
	for _, m := range lines {
		if m["path"] == path && m["outcome"] == outcome {
			return m
		}
	}
	return nil
}

func journalSays(t *testing.T, s *Store, path, outcome string) map[string]any {
	t.Helper()
	lines := journalOf(t, s)
	m := journalLine(lines, path, outcome)
	if m == nil {
		var got []string
		for _, l := range lines {
			got = append(got, fmt.Sprint(l["path"], " ", l["outcome"]))
		}
		t.Fatalf("no %s line with outcome %q in the journal: %q", path, outcome, got)
	}
	return m
}

// journalClean fails when any of these tokens is anywhere in the journal.
func journalClean(t *testing.T, s *Store, tokens ...string) {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(storeDir(s), journalName))
	if len(raw) == 0 {
		t.Fatal("the journal is empty")
	}
	for _, tok := range tokens {
		if bytes.Contains(raw, []byte(tok)) {
			t.Fatalf("the journal holds a token, %q:\n%s", tok, raw)
		}
	}
}

// replacedMeanwhile sets the world up so that the first token rota presents
// has just been presented by somebody outside rota, and the home holds what
// that refresh issued: a second holder of the lineage, or a login written in
// without the refresh lock. rota's own request lands on a token the provider
// has replaced, the provider revokes the family, and the home's login is still
// one the profile vouches for.
func replacedMeanwhile(w *lineageWorld) {
	done := false
	w.p.onRota = func(rt string) {
		if done {
			return
		}
		done = true
		_, body := w.p.present("a second holder", rt)
		m := body.(map[string]any)
		w.cc.write(storeLogin(m["access_token"].(string), m["refresh_token"].(string),
			rota.Now().Add(8*time.Hour).UnixMilli(), lineageUntil))
	}
}

func (w *lineageWorld) unrevoke() {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	w.p.revoked = ""
}

// rota's refresh token is refused because somebody presented it first, and the
// home's login, taken by profile, keeps the account alive. That refusal is not
// masked: the account keeps when, from where and with which token, the person
// is told on the existing warning path, and the journal says the login was
// accepted after a refusal — from each look that renews a home's login.
func TestARefusalTheHomesLoginCoveredIsKeptSaidAndJournalled(t *testing.T) {
	looks := []struct {
		look lineageLook
		path string
	}{{lineageLooks()[0], "list.renew"}, {lineageLooks()[2], "serve.renew"}, {lineageLooks()[3], "launch.renew"}}
	for _, m := range lineageModes() {
		for _, l := range looks {
			t.Run(m.name+"/"+l.look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				words := said(w.s)
				replacedMeanwhile(w)
				w.clock.pass(9 * time.Hour)
				l.look.do(t, w)
				w.p.dump(t)
				if w.a.Dead || w.a.Token.Refresh != "R1" {
					t.Fatalf("the home's login keeps the account alive: dead=%v %q %q", w.a.Dead, w.a.Token.Refresh, w.a.WhyDead())
				}
				want := fmt.Sprintf(`{"at":%d,"path":"%s","fp":"%s"}`, w.clock.at.UnixMilli(), l.path, fingerprintOf("R0"))
				if got := w.a.Extra[lastRefusalKey]; got != want {
					t.Fatalf("last_refusal:\n got %s\nwant %s", got, want)
				}
				if !strings.Contains(readFile(filepath.Join(storeDir(w.s), "accounts.json")), lastRefusalKey) {
					t.Fatal("last_refusal is saved")
				}
				if msg := saidOf(words, replacedWarning); !strings.HasPrefix(msg, w.a.String()+": ") {
					t.Fatalf("said, naming the account: %q", *words)
				}
				e := journalSays(t, w.s, l.path, "accepted-after-refusal")
				if e["before"] != fingerprintOf("R0") || e["after"] != fingerprintOf("R1") || e["account"] != float64(w.a.ID) ||
					e["provider"] != "claude" {
					t.Fatalf("the refresh's line: %v", e)
				}
				if home, _ := e["home"].(map[string]any); home["fp"] != fingerprintOf("R0") || home["source"] != "file" {
					t.Fatalf("the home rota read before it presented: %v", e["home"])
				}
				a := journalSays(t, w.s, "adopt", "new-login")
				if a["before"] != fingerprintOf("R0") || a["after"] != fingerprintOf("R1") || a["staged_after"] != fingerprintOf("R1") {
					t.Fatalf("the adoption's line: %v", a)
				}
				journalClean(t, w.s, "R0", "R1", "A0", "A1")
			})
		}
	}
}

// When the login the account was left on after such a refusal then dies of
// Claude Code clearing it, the reason says rota had presented a replaced token,
// and when.
func TestADeathAfterAKeptRefusalSaysRotaHadPresentedAReplacedToken(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			replacedMeanwhile(w)
			w.clock.pass(9 * time.Hour)
			lineageLooks()[0].do(t, w)
			refusedAt := w.clock.at
			if w.a.Dead || w.a.Extra[lastRefusalKey] == "" {
				t.Fatalf("setup: alive on the home's login, the refusal kept: %v %q", w.a.Dead, w.a.Extra)
			}
			// A launch in the quiet home finds it in sync, and takes rota's own
			// older file out from beside the keychain item.
			lineageLooks()[3].do(t, w)
			w.clock.pass(time.Minute)
			w.cc.start()
			t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
			lineageLooks()[1].do(t, w)
			w.p.dump(t)
			want := "Claude Code cleared the login in its home, and the provider refused rota's own refresh token too " +
				"(invalid_grant: Refresh token not found or invalid) (rota had presented a replaced token at " +
				refusedAt.UTC().Format("15:04") + " UTC)"
			if !w.a.Dead || w.a.DeadReason != want {
				t.Fatalf("dead=%v\n got %q\nwant %q", w.a.Dead, w.a.DeadReason, want)
			}
			journalSays(t, w.s, "adopt", "blanked")
			journalSays(t, w.s, "rescue.blank", "refused:invalid_grant")
			journalClean(t, w.s, "R0", "R1", "A0", "A1")
		})
	}
}

// Adoption gives the same death the same words: an account a rota marked dead
// with no reason, whose home Claude Code then blanked.
func TestADeathAdoptionFindsAfterAKeptRefusalSaysSoToo(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), clearedLogin)
	a.StagedWritten()
	a.Dead = true
	at := time.Date(2026, 11, 7, 6, 22, 0, 0, time.UTC)
	a.Extra[lastRefusalKey] = fmt.Sprintf(`{"at":%d,"path":"list.renew","fp":"%s"}`, at.UnixMilli(), fingerprintOf(a.Token.Refresh))
	if err := s.claudeHome(a, false).adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "Claude Code cleared the login in its home after a refresh of its own was refused (rota had presented a replaced token at 06:22 UTC)"
	if a.DeadReason != want {
		t.Fatalf("\n got %q\nwant %q", a.DeadReason, want)
	}
	if err := s.claudeHome(a, false).adopt(context.Background()); err != nil || a.DeadReason != want {
		t.Fatalf("said once: %q", a.DeadReason)
	}
}

// A later successful rotation shows the login lived through the refusal, which
// is then forgotten: Claude Code's, taken from the home; rota's own; and a new
// login, which replaces the login it was about.
func TestALaterRotationForgetsAKeptRefusal(t *testing.T) {
	refused := func(t *testing.T) *lineageWorld {
		w := newLineageWorld(t, false)
		replacedMeanwhile(w)
		w.clock.pass(9 * time.Hour)
		lineageLooks()[0].do(t, w)
		if w.a.Dead || w.a.Extra[lastRefusalKey] == "" {
			t.Fatalf("setup: alive on the home's login, the refusal kept: %v %q", w.a.Dead, w.a.Extra)
		}
		w.unrevoke() // this provider let the family live
		return w
	}
	t.Run("Claude Code's", func(t *testing.T) {
		w := refused(t)
		w.cc.start()
		t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
		lineageLooks()[1].do(t, w)
		if w.a.Token.Refresh != "R2" || w.a.Extra[lastRefusalKey] != "" {
			t.Fatalf("taken, and the refusal forgotten: %q %q", w.a.Token.Refresh, w.a.Extra[lastRefusalKey])
		}
		e := journalSays(t, w.s, "adopt", "rotation")
		if e["before"] != fingerprintOf("R1") || e["after"] != fingerprintOf("R2") {
			t.Fatalf("%v", e)
		}
	})
	t.Run("rota's own", func(t *testing.T) {
		w := refused(t)
		w.clock.pass(9 * time.Hour)
		lineageLooks()[0].do(t, w)
		if w.a.Token.Refresh != "R2" || w.a.Extra[lastRefusalKey] != "" {
			t.Fatalf("rotated, and the refusal forgotten: %q %q", w.a.Token.Refresh, w.a.Extra[lastRefusalKey])
		}
		journalSays(t, w.s, "list.renew", "rotated")
		if bad := w.p.rotaSpent(); len(bad) != 1 {
			t.Fatalf("rota presented one spent token, the one the second holder spent: %+v", bad)
		}
	})
	t.Run("a new login", func(t *testing.T) {
		rota.Register(&fakeProvider{name: "t-journal-login", identity: &rota.Identity{UUID: "u9", Email: "u9@x"}})
		s := openTemp(t)
		a := s.add("t-journal-login")
		a.UUID, a.Extra = "u9", map[string]string{lastRefusalKey: `{"at":1,"path":"list.renew","fp":"0123456789abcdef"}`}
		l, err := s.BeginLogin(context.Background(), "t-journal-login")
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := s.FinishLogin(context.Background(), l.ID, "CODE")
		if err != nil || got != a || a.Extra[lastRefusalKey] != "" {
			t.Fatalf("%v %v %q", err, got == a, a.Extra)
		}
	})
}

// The secrets a journal test works with: long enough that finding one in the
// journal cannot be chance.
const (
	jAccess    = "sk-ant-oat01-journal-access-SECRET"
	jRefresh   = "sk-ant-ort01-journal-refresh-SECRET"
	jNewAccess = "sk-ant-oat01-journal-issued-access-SECRET"
	jNewRefr   = "sk-ant-ort01-journal-issued-refresh-SECRET"
)

var jTokens = []string{jAccess, jRefresh, jNewAccess, jNewRefr, "SECRET"}

func journalClaude(s *Store) *rota.Account {
	a := livingClaude(s, "u1")
	a.Token.Access, a.Token.Refresh = jAccess, jRefresh
	return a
}

// Every refresh rota makes leaves a line, from every way into one — a home's
// own refresh, for each look; a token rota alone holds, for a claude account
// and any other provider — and so does every change a look at a home finds
// there and every login rota writes into a home. No line holds a token.
func TestEveryRefreshAndEveryChangeAtAHomeLeavesALineWithNoToken(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	ctx := context.Background()
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = refreshesTo(jNewAccess, jNewRefr)
		f.profile = profileOf(jNewAccess, "u1")
	})
	hermetic := func(t *testing.T, s *Store, a *rota.Account) {
		t.Helper()
		_, release, err := s.prepare(ctx, a, false)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	type want struct{ path, outcome string }
	for _, c := range []struct {
		what  string
		do    func(t *testing.T, s *Store)
		wants []want
	}{
		{"a listing renews a quiet home", func(t *testing.T, s *Store) {
			a := journalClaude(s)
			expire(a)
			if errs := s.Refresh(ctx, true, a); len(errs) != 0 {
				t.Fatal(errs)
			}
			seed := journalSays(t, s, "seed", "written")
			if seed["home"].(map[string]any)["source"] != "none" || seed["staged_before"] != "-" ||
				seed["staged_after"] != fingerprintOf(jRefresh) || seed["lock"] != false {
				t.Fatalf("the home held nothing before the login went in: %v", seed)
			}
			e := journalSays(t, s, "list.renew", "rotated")
			if e["before"] != fingerprintOf(jRefresh) || e["after"] != fingerprintOf(jNewRefr) ||
				e["home"].(map[string]any)["fp"] != fingerprintOf(jRefresh) || e["exp_after"] == e["exp_before"] {
				t.Fatalf("the refresh, of the login the home held: %v", e)
			}
			if lines := journalOf(t, s); len(lines) != 3 || lines[2]["path"] != "seed" ||
				lines[2]["home"].(map[string]any)["fp"] != fingerprintOf(jRefresh) {
				t.Fatalf("written before, refreshed, written after: %v", lines)
			}
		}, []want{{"seed", "written"}, {"list.renew", "rotated"}}},
		{"maintenance renews a quiet home", func(t *testing.T, s *Store) {
			a := journalClaude(s)
			expire(a)
			if errs := s.Maintain(ctx); len(errs) != 0 {
				t.Fatal(errs)
			}
		}, []want{{"seed", "written"}, {"serve.renew", "rotated"}}},
		{"a launch renews a quiet home", func(t *testing.T, s *Store) {
			a := journalClaude(s)
			expire(a)
			if _, err := launchEnv(t, s, a); err != nil {
				t.Fatal(err)
			}
		}, []want{{"seed", "written"}, {"launch.renew", "rotated"}}},
		{"a blanked home is rescued", func(t *testing.T, s *Store) {
			a := journalClaude(s)
			cleared(t, s, a)
			if errs := s.Rescue(ctx); len(errs) != 0 {
				t.Fatal(errs)
			}
		}, []want{{"adopt", "blanked"}, {"rescue.blank", "rotated"}, {"seed", "written"}}},
		{"a stranded home is rescued", func(t *testing.T, s *Store) {
			a := journalClaude(s)
			stranded(t, s, a)
			if errs := s.Rescue(ctx); len(errs) != 0 {
				t.Fatal(errs)
			}
			e := journalSays(t, s, "rescue.stranded", "rotated")
			if e["lock"] != true || !slices.Contains(e["alive"].([]any), any(fmt.Sprintf("daemon %d", os.Getpid()))) {
				t.Fatalf("a rescue holds the refresh lock, beside the daemon: %v", e)
			}
		}, []want{{"rescue.stranded", "rotated"}}},
		{"a hermetic run beside windows that hold no login", func(t *testing.T, s *Store) {
			a := journalClaude(s)
			expire(a)
			alive(t, s.Home(a), "sessions/1.json", os.Getpid())
			hermetic(t, s, a)
		}, []want{{"token.env", "rotated"}}},
		{"an account in a directory you chose", func(t *testing.T, s *Store) {
			a := journalClaude(s)
			a.ConfigDir = t.TempDir()
			expire(a)
			if errs := s.Refresh(ctx, true, a); len(errs) != 0 {
				t.Fatal(errs)
			}
			expire(a)
			if errs := s.Maintain(ctx); len(errs) != 0 {
				t.Fatal(errs)
			}
			expire(a)
			if _, err := launchEnv(t, s, a); err != nil {
				t.Fatal(err)
			}
			expire(a)
			hermetic(t, s, a)
			if n := len(journalOf(t, s)); n != 4 {
				t.Fatalf("four refreshes, four lines: %d", n)
			}
		}, []want{{"token.env", "rotated"}}},
		{"another provider", func(t *testing.T, s *Store) {
			rota.Register(fakeRefresher{&fakeProvider{name: "t-journal",
				refreshTok: &rota.Token{Access: jNewAccess, Refresh: jNewRefr, ExpiresAt: rota.NowMS() + 3_600_000}}})
			a := s.add("t-journal")
			a.Token = rota.Token{Access: jAccess, Refresh: jRefresh, ExpiresAt: rota.NowMS() - 1}
			if errs := s.Maintain(ctx); len(errs) != 0 {
				t.Fatal(errs)
			}
			a.Token.ExpiresAt = rota.NowMS() - 1
			_, release, err := s.prepare(ctx, a, true)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if e := journalSays(t, s, "launch.renew", "rotated"); e["provider"] != "t-journal" || e["home"] != nil {
				t.Fatalf("%v", e)
			}
		}, []want{{"serve.renew", "rotated"}, {"launch.renew", "rotated"}}},
		{"Claude Code rotated the login, and then an older one is found", func(t *testing.T, s *Store) {
			a := journalClaude(s)
			if _, err := launchEnv(t, s, a); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin(jNewAccess, jNewRefr, later(3*time.Hour), "1999999999000"))
			if _, err := launchEnv(t, s, a); err != nil {
				t.Fatal(err)
			}
			alive(t, s.Home(a), "sessions/1.json", os.Getpid())
			writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin(jAccess+"-old", jRefresh+"-old", later(-time.Hour), "1999999999000"))
			if errs := s.Rescue(ctx); len(errs) != 0 {
				t.Fatal(errs)
			}
			e := journalSays(t, s, "adopt", "rotation")
			if e["before"] != fingerprintOf(jRefresh) || e["after"] != fingerprintOf(jNewRefr) {
				t.Fatalf("%v", e)
			}
			if e := journalSays(t, s, "adopt", "demoted"); e["staged_after"] != "-" || e["home"].(map[string]any)["fp"] != fingerprintOf(jRefresh+"-old") {
				t.Fatalf("%v", e)
			}
		}, []want{{"seed", "written"}, {"adopt", "rotation"}, {"adopt", "demoted"}}},
		{"a refusal", func(t *testing.T, s *Store) {
			f.set(func(f *anthropic) {
				f.refresh = func() (int, any) {
					return 400, map[string]string{"error": "invalid_grant", "error_description": "gone"}
				}
			})
			defer f.set(func(f *anthropic) { f.refresh = refreshesTo(jNewAccess, jNewRefr) })
			a := journalClaude(s)
			expire(a)
			s.Refresh(ctx, true, a)
			if !a.Dead {
				t.Fatal("refused, and dead")
			}
		}, []want{{"list.renew", "refused:invalid_grant"}}},
		{"a refresh that does not get through", func(t *testing.T, s *Store) {
			f.set(func(f *anthropic) { f.refresh = func() (int, any) { return 503, nil } })
			defer f.set(func(f *anthropic) { f.refresh = refreshesTo(jNewAccess, jNewRefr) })
			a := journalClaude(s)
			expire(a)
			s.Refresh(ctx, true, a)
			if a.Dead {
				t.Fatal("not dead")
			}
		}, []want{{"list.renew", "transient:503"}}},
		{"a token refused earlier in this process", func(t *testing.T, s *Store) {
			a := journalClaude(s)
			expire(a)
			key := spentKey(s.Home(a), a.Token.Refresh)
			spentTokens.Store(key, "invalid_grant: gone")
			t.Cleanup(func() { spentTokens.Delete(key) })
			before := f.refreshes.Load()
			if _, err := s.claudeHome(a, false).refresh(ctx); err == nil || f.refreshes.Load() != before {
				t.Fatalf("not presented again: %v", err)
			}
		}, []want{{"launch.renew", "skipped:refused earlier in this process"}}},
	} {
		t.Run(c.what, func(t *testing.T) {
			s := openTemp(t)
			c.do(t, s)
			for _, w := range c.wants {
				journalSays(t, s, w.path, w.outcome)
			}
			journalClean(t, s, jTokens...)
		})
	}
}

// A blanked keychain item taken out of the way of the login in the file beside
// it is a write into the home like any other, and leaves its line.
func TestUncoveringALoginLeavesALine(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	fakeAnthropic(t)
	s := openTemp(t)
	a := journalClaude(s)
	home := s.Home(a)
	a.StagedWritten()
	k.items[service(t, home)] = clearedLogin
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
	alive(t, home, "sessions/1.json", os.Getpid())
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	e := journalSays(t, s, "uncover", "written")
	if e["lock"] != true || e["before"] != fingerprintOf(jRefresh) || e["home"].(map[string]any)["source"] != "file" {
		t.Fatalf("%v", e)
	}
	journalClean(t, s, jTokens...)
}

// The journal is private, and is moved aside at one MiB, replacing what was
// moved aside before.
func TestTheJournalIsPrivateAndMovedAsideAtOneMiB(t *testing.T) {
	s := openTemp(t)
	a := livingClaude(s, "u1")
	path := filepath.Join(storeDir(s), journalName)
	s.journalStart("token.env", a).finish("rotated")
	if fi, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("mode 0600: %v %v", err, fi)
	}

	under := append(bytes.Repeat([]byte("x"), int(journalCap)-2), '\n')
	if err := os.WriteFile(path, under, 0o600); err != nil {
		t.Fatal(err)
	}
	s.journalStart("token.env", a).finish("rotated")
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("under one MiB, nothing is moved aside")
	}

	full := append(bytes.Repeat([]byte("y"), int(journalCap)-1), '\n')
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("older\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.journalStart("token.env", a).finish("rotated")
	if moved, _ := os.ReadFile(path + ".1"); !bytes.Equal(moved, full) {
		t.Fatalf("the full journal is moved aside, over the older one: %d bytes", len(moved))
	}
	if lines := journalOf(t, s); len(lines) != 1 || lines[0]["outcome"] != "rotated" {
		t.Fatalf("and a new one begun: %v", lines)
	}
}

// A journal that cannot be written loses its line and fails nothing.
func TestAJournalThatCannotBeWrittenFailsNoRefresh(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo(jNewAccess, jNewRefr) })
	s := openTemp(t)
	if err := os.Mkdir(filepath.Join(storeDir(s), journalName), 0o700); err != nil {
		t.Fatal(err)
	}
	a := journalClaude(s)
	expire(a)
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 || a.Token.Refresh != jNewRefr {
		t.Fatalf("refreshed all the same: %v %q", errs, a.Token.Refresh)
	}
}

// The journal's fingerprint is the one Staged keeps.
func TestTheJournalsFingerprintIsStageds(t *testing.T) {
	a := &rota.Account{Token: rota.Token{Refresh: jRefresh}}
	a.StagedWritten()
	if got := fingerprintOf(jRefresh); got != a.Staged || len(got) != 16 || fingerprintOf("") != "" {
		t.Fatalf("%q %q", got, a.Staged)
	}
}
