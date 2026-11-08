package store

// Every refresh of a login kept in a home holds Claude Code's refresh lock, and
// reads the home again once it holds it, in the lineage world (lineage_test.go):
// a provider that spends each refresh token once and revokes the whole family
// when a spent one comes back, and a fake Claude Code that takes the same lock
// around each refresh of its own.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// renewingLooks are the looks that refresh an expired login in a quiet home:
// a listing, maintenance and a launch. The short listing only rescues.
func renewingLooks() []lineageLook {
	l := lineageLooks()
	return []lineageLook{l[0], l[2], l[3]}
}

// byRota is every presentation of rota's the provider answered.
func (p *lineageProvider) byRota() []lineagePresented {
	var out []lineagePresented
	for _, e := range p.entries() {
		if e.who == "rota" {
			out = append(out, e)
		}
	}
	return out
}

func lockPath(w *lineageWorld) string { return filepath.Join(w.s.Home(w.a), claudeRefreshLock) }

func lockGone(t *testing.T, w *lineageWorld) {
	t.Helper()
	if _, err := os.Stat(lockPath(w)); !os.IsNotExist(err) {
		t.Errorf("rota let go of the refresh lock: %v", err)
	}
}

// The audit's S3, closed: the home is quiet when rota looks, and a Claude Code
// process starts there while rota's refresh is with the provider. It finds the
// refresh lock held, so it waits; rota's R0 is the only presentation of R0, and
// R1 is in the home when the lock goes, so the Claude Code that waited reads it
// and its own refresh goes through. Nothing is reused, and the next look follows
// Claude Code's rotation.
func TestClaudeCodeStartingDuringRotasRefreshWaitsForTheLock(t *testing.T) {
	for _, m := range lineageModes() {
		for _, look := range renewingLooks() {
			t.Run(m.name+"/"+look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				w.clock.pass(9 * time.Hour) // rota's token has run out; nothing alive
				var meanwhile string
				w.p.onRota = func(string) {
					if meanwhile == "" {
						w.cc.start()
						meanwhile = w.cc.refresh("claude-code window started meanwhile")
					}
				}
				look.do(t, w)
				w.p.onRota = nil
				w.clock.pass(time.Minute)
				waited := w.cc.refresh("claude-code window, after the wait")
				w.p.dump(t)
				if !strings.HasPrefix(meanwhile, "refresh lock held") {
					t.Fatalf("Claude Code should have found rota's lock: %q", meanwhile)
				}
				mine := w.p.byRota()
				if len(mine) != 1 || mine[0].rt != "R0" || mine[0].wasSpent || !mine[0].lockHeld || mine[0].homeRT != "R0" {
					t.Fatalf("rota presents R0 once, the home's newest, holding the lock: %+v", mine)
				}
				if waited != "rotated to R2" {
					t.Fatalf("the Claude Code that waited reads rota's R1 and refreshes it: %q", waited)
				}
				lockGone(t, w)
				look.do(t, w)
				if bad := w.p.rotaSpent(); len(bad) != 0 || w.p.revoked != "" {
					t.Fatalf("no reuse: %+v %q", bad, w.p.revoked)
				}
				if w.a.Dead || w.a.Token.Refresh != "R2" || len(w.p.byRota()) != 1 {
					t.Fatalf("rota follows Claude Code's rotation and presents nothing more: dead=%v %q holds %s",
						w.a.Dead, w.a.WhyDead(), w.a.Token.Refresh)
				}
			})
		}
	}
}

// The new login is in the home before the lock goes. On the keychain the write
// takes the item Claude Code put its last rotation in out of the way right
// before the new file is renamed into place, and the lock is still held then.
func TestRotaWritesTheRefreshedLoginInBeforeItLetsGoOfTheLock(t *testing.T) {
	for _, look := range renewingLooks() {
		t.Run(look.what, func(t *testing.T) {
			w := newLineageWorld(t, true)
			w.cc.start()
			w.clock.pass(7*time.Hour + 56*time.Minute)
			if got := w.cc.refresh("claude-code daemon"); got != "rotated to R1" {
				t.Fatal(got)
			}
			lineageLooks()[1].do(t, w)
			w.cc.stop()
			if w.a.Token.Refresh != "R1" || !w.a.StagedCurrent() {
				t.Fatalf("setup: rota follows to R1: %s", w.a.Token.Refresh)
			}
			w.clock.pass(9 * time.Hour)
			var deletes, unlocked int
			w.k.onDelete = func(string) {
				deletes++
				if _, err := os.Stat(lockPath(w)); err != nil {
					unlocked++
				}
			}
			look.do(t, w)
			w.p.dump(t)
			if deletes == 0 || unlocked != 0 {
				t.Fatalf("the item goes under the lock: %d deletes, %d without it", deletes, unlocked)
			}
			if got := refreshOf(w.cc.store()); got != "R2" || w.a.Token.Refresh != "R2" {
				t.Fatalf("the home holds the refreshed login: %q, rota %s", got, w.a.Token.Refresh)
			}
			if mine := w.p.byRota(); len(mine) != 1 || !mine[0].lockHeld || mine[0].wasSpent {
				t.Fatalf("%+v", mine)
			}
			lockGone(t, w)
		})
	}
}

// The home is asked again once the lock is held: a Claude Code that started
// there after rota's first look at it, and before rota took the lock, has the
// login, and rota presents nothing. Its start is made to land in that gap by
// writing its daemon's record when rota reads the keychain under the lock.
func TestAClaudeCodeThatStartedBeforeRotaTookTheLockIsLeftTheLogin(t *testing.T) {
	for _, look := range renewingLooks() {
		t.Run(look.what, func(t *testing.T) {
			w := newLineageWorld(t, true)
			w.clock.pass(9 * time.Hour)
			started := false
			w.k.onFind = func(string) {
				if _, err := os.Stat(lockPath(w)); err == nil && !started {
					started = true
					w.cc.start()
				}
			}
			look.do(t, w)
			w.p.dump(t)
			if !started {
				t.Fatal("rota read the home under the lock")
			}
			if mine := w.p.byRota(); len(mine) != 0 {
				t.Fatalf("nothing presented while Claude Code runs there: %+v", mine)
			}
			if w.a.Dead {
				t.Fatalf("not dead: %s", w.a.WhyDead())
			}
			lockGone(t, w)
		})
	}
}

// A lock somebody holds is Claude Code refreshing right now. Every renewing look
// leaves the login to them: nothing presented, no error, the account alive and
// its token as it was, and the lock left where it is. Once it goes, the next look
// refreshes as ever.
func TestARefreshLockSomebodyHoldsIsLeftToThemWithNoError(t *testing.T) {
	for _, m := range lineageModes() {
		for _, look := range renewingLooks() {
			t.Run(m.name+"/"+look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				w.clock.pass(9 * time.Hour)
				if err := os.Mkdir(lockPath(w), 0o700); err != nil {
					t.Fatal(err)
				}
				var errs []error
				switch look.what {
				case "rota list":
					errs = w.s.Refresh(t.Context(), false, w.a)
				case "rota serve maintenance":
					errs = w.s.Maintain(t.Context())
				default:
					if _, err := launchEnv(t, w.s, w.a); err != nil {
						errs = append(errs, err)
					}
				}
				if len(errs) != 0 {
					t.Fatalf("no error: %v", errs)
				}
				if mine := w.p.byRota(); len(mine) != 0 {
					t.Fatalf("nothing presented beside the lock: %+v", mine)
				}
				if w.a.Dead || w.a.Token.Refresh != "R0" {
					t.Fatalf("alive and unchanged: dead=%v %q %s", w.a.Dead, w.a.WhyDead(), w.a.Token.Refresh)
				}
				if _, err := os.Stat(lockPath(w)); err != nil {
					t.Fatalf("the lock is not rota's to touch: %v", err)
				}
				if err := os.Remove(lockPath(w)); err != nil {
					t.Fatal(err)
				}
				look.do(t, w)
				w.p.dump(t)
				if mine := w.p.byRota(); len(mine) != 1 || mine[0].rt != "R0" || !mine[0].lockHeld || w.a.Token.Refresh != "R1" {
					t.Fatalf("the next look refreshes: %+v, holds %s", mine, w.a.Token.Refresh)
				}
				lockGone(t, w)
			})
		}
	}
}

// failingToken stands between rota and the provider's token endpoint and, until
// it is stopped, answers rota itself, the way a reply whose outcome rota cannot
// know comes back: the provider never saw it, so nothing was spent.
type failingToken struct {
	mu     sync.Mutex
	answer func(http.ResponseWriter) // nil once stopped
	seen   []string                  // the refresh tokens rota presented that were answered here
}

func failRota(t *testing.T, w *lineageWorld, answer func(http.ResponseWriter)) *failingToken {
	t.Helper()
	f := &failingToken{answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" && strings.HasPrefix(r.Header.Get("User-Agent"), "rota") {
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			answer := f.answer
			if answer != nil {
				var req struct {
					Refresh string `json:"refresh_token"`
				}
				_ = json.Unmarshal(body, &req)
				f.seen = append(f.seen, req.Refresh)
			}
			f.mu.Unlock()
			if answer != nil {
				answer(rw)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		w.p.serve(rw, r)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL + "/token"
	t.Cleanup(func() { rota.ClaudeEndpoints.Token = old })
	return f
}

func (f *failingToken) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = nil
}

func (f *failingToken) presented() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// A refresh whose outcome rota cannot know — a failure on the provider's side,
// a code nobody knows — leaves the account alive on its token, and the token is
// not presented again before the home has been read under the lock. Claude Code
// rotates the login meanwhile; rota takes R1 and presents nothing. "before the
// look" has the rotation in the home when the look starts. "under the lock"
// lands it after the look's first read and before rota's read under the lock,
// by putting it in the keychain's answer to that read: only that read sees it.
func TestAnUnknownOutcomeIsNotPresentedAgainOnceTheHomeRotated(t *testing.T) {
	outcomes := []struct {
		what   string
		answer func(http.ResponseWriter)
	}{
		{"server error", func(rw http.ResponseWriter) {
			rw.WriteHeader(http.StatusInternalServerError)
			_, _ = rw.Write([]byte(`upstream connect error`))
		}},
		{"unknown code", func(rw http.ResponseWriter) {
			rw.WriteHeader(http.StatusBadRequest)
			_, _ = rw.Write([]byte(`{"error":"temporarily_overloaded","error_description":"try later"}`))
		}},
	}
	type when struct {
		what     string
		keychain bool
	}
	whens := []when{{"before the look", false}, {"before the look", true}, {"under the lock", true}}
	for _, o := range outcomes {
		for _, wh := range whens {
			for _, look := range renewingLooks() {
				mode := "file"
				if wh.keychain {
					mode = "keychain"
				}
				t.Run(o.what+"/"+wh.what+"/"+mode+"/"+look.what, func(t *testing.T) {
					w := newLineageWorld(t, wh.keychain)
					w.clock.pass(9 * time.Hour)
					f := failRota(t, w, o.answer)
					look.do(t, w)
					f.stop()
					// Maintenance asks twice in one sweep: its own renew, then the
					// listing it ends with, each after reading the home under the lock.
					got := f.presented()
					if len(got) == 0 || strings.Trim(strings.Join(got, ""), "R0") != "" {
						t.Fatalf("rota presented R0 and got no answer it could read: %v", got)
					}
					if w.a.Dead || w.a.Token.Refresh != "R0" {
						t.Fatalf("alive on R0: dead=%v %q %s", w.a.Dead, w.a.WhyDead(), w.a.Token.Refresh)
					}
					lockGone(t, w)
					w.clock.pass(time.Minute)
					switch wh.what {
					case "before the look":
						if got := w.cc.refresh("claude-code window"); got != "rotated to R1" {
							t.Fatal(got)
						}
					default:
						// Claude Code's refresh got through; its write lands later.
						w.p.present("claude-code window", "R0")
						rotated := storeLogin("A1", "R1", rota.Now().Add(8*time.Hour).UnixMilli(), lineageUntil)
						w.k.onFind = func(svc string) {
							if _, err := os.Stat(lockPath(w)); err == nil && rotated != "" && svc == w.cc.svc {
								w.k.items[svc] = rotated
								rotated = ""
							}
						}
					}
					look.do(t, w)
					w.p.dump(t)
					if mine := w.p.byRota(); len(mine) != 0 {
						t.Fatalf("rota presents nothing once the home has rotated: %+v", mine)
					}
					if w.a.Dead || w.a.Token.Refresh != "R1" || w.p.revoked != "" {
						t.Fatalf("rota takes R1: dead=%v %q holds %s revoked=%q", w.a.Dead, w.a.WhyDead(), w.a.Token.Refresh, w.p.revoked)
					}
					if look.what == "rota list" && w.a.Quota == nil {
						t.Fatal("and reads usage with it")
					}
				})
			}
		}
	}
}

// A run on a token while Claude Code from before runs in the home on tokens of
// its own (launchBeside, tokenNow) refreshes the account's token under the lock
// too, and a lock somebody holds turns the run away for a moment, not for good.
func TestARunOnATokenRefreshesUnderTheLock(t *testing.T) {
	for _, m := range lineageModes() {
		for _, locked := range []bool{false, true} {
			name := m.name + "/lock free"
			if locked {
				name = m.name + "/lock held"
			}
			t.Run(name, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				home := w.s.Home(w.a)
				if err := os.Remove(filepath.Join(home, claudeCredentials)); err != nil {
					t.Fatal(err)
				}
				w.a.StagedSuperseded()
				w.a.Extra[routeKey] = routeEnv
				alive(t, home, filepath.Join(claudeSessionsDir, "1.json"), os.Getpid())
				if err := w.s.Save(); err != nil {
					t.Fatal(err)
				}
				w.clock.pass(9 * time.Hour)
				if locked {
					if err := os.Mkdir(lockPath(w), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				_, err := launchEnv(t, w.s, w.a)
				w.p.dump(t)
				mine := w.p.byRota()
				if locked {
					if !errors.Is(err, rota.ErrBusy) || len(mine) != 0 || w.a.Dead || w.a.Token.Refresh != "R0" {
						t.Fatalf("turned away for a moment: %v, %+v, dead=%v %s", err, mine, w.a.Dead, w.a.Token.Refresh)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(mine) != 1 || mine[0].rt != "R0" || !mine[0].lockHeld || w.a.Token.Refresh != "R1" {
					t.Fatalf("refreshed under the lock: %+v, holds %s", mine, w.a.Token.Refresh)
				}
				lockGone(t, w)
			})
		}
	}
}

// A panic while the home is read under the lock — here the keychain's answer to
// that read — lets go of the lock and of the account's claim, on every look: a
// lock left behind would be kept fresh by its beat, and Claude Code shut out of
// its own refresh for as long as the process lives.
func TestAPanicWhileReadingTheHomeUnderTheLockLetsGoOfIt(t *testing.T) {
	for _, look := range lineageLooks() {
		t.Run(look.what, func(t *testing.T) {
			w := newLineageWorld(t, true)
			if look.what == "rota list --short / watcher" {
				// A write mend owes the home, under the lock: rota refreshed R0 to
				// R1, and R1 never reached the home, which holds the R0 rota wrote.
				w.a.Token = rota.Token{Access: "A1", Refresh: "R1", ExpiresAt: rota.Now().Add(8 * time.Hour).UnixMilli(),
					Scopes: []string{"user:inference"}}
				w.a.Extra[seedOwedKey] = "1"
			} else {
				w.clock.pass(9 * time.Hour)
			}
			panicked := false
			w.k.onFind = func(string) {
				if _, err := os.Stat(lockPath(w)); err == nil && !panicked {
					panicked = true
					panic("while reading the home under the lock")
				}
			}
			func() {
				defer func() { _ = recover() }()
				look.do(t, w)
			}()
			if !panicked {
				t.Fatal("the home was read under the lock")
			}
			lockGone(t, w)
			if w.s.claimed(w.a) || stillHeld(w.s, w.a) {
				t.Fatal("and the claim on the account is let go")
			}
		})
	}
}

// tokenWorld is the lineage world as a run on a token finds it beside Claude Code
// from before (launchBeside): a window alive in the home, which was last launched
// on the environment route and holds no login, and rota's token run out.
func tokenWorld(t *testing.T, keychain bool) *lineageWorld {
	t.Helper()
	w := newLineageWorld(t, keychain)
	home := w.s.Home(w.a)
	if err := os.Remove(filepath.Join(home, claudeCredentials)); err != nil {
		t.Fatal(err)
	}
	w.a.StagedSuperseded()
	w.a.Extra[routeKey] = routeEnv
	alive(t, home, filepath.Join(claudeSessionsDir, "1.json"), os.Getpid())
	if err := w.s.Save(); err != nil {
		t.Fatal(err)
	}
	w.clock.pass(9 * time.Hour)
	return w
}

// underTheLock puts doc in the home's keychain item in the keychain's answer to
// the first read made while the refresh lock is held: a write that lands after
// the look's first read and before rota's read under the lock. It reports
// whether it has landed.
func underTheLock(w *lineageWorld, doc string) func() bool {
	landed := false
	w.k.onFind = func(svc string) {
		if _, err := os.Stat(lockPath(w)); err == nil && !landed && svc == w.cc.svc {
			landed = true
			w.k.items[svc] = doc
		}
	}
	return func() bool { return landed }
}

// A run on a token whose home, read again under the lock, now holds the
// account's current login while a window is alive there presents nothing: that
// window reads the store, and would present the token rota had just spent.
func TestATokenRunIsRefusedWhenTheHomeReadUnderTheLockHoldsTheCurrentLogin(t *testing.T) {
	w := tokenWorld(t, true)
	landed := underTheLock(w, storeLogin("A0", "R0", w.a.Token.ExpiresAt, lineageUntil))
	_, err := launchEnv(t, w.s, w.a)
	w.p.dump(t)
	if !landed() {
		t.Fatal("the home was read under the lock")
	}
	if mine := w.p.byRota(); len(mine) != 0 || !errors.Is(err, rota.ErrBusy) || w.a.Dead {
		t.Fatalf("refused, nothing presented, not dead: err=%v presented=%+v dead=%v", err, mine, w.a.Dead)
	}
	lockGone(t, w)
}

// The home read again under the lock may hold the account's refresh token with
// a fresh access token beside it — a refresh of Claude Code's that did not
// rotate the refresh token. The run takes it and goes on it, presenting nothing.
func TestATokenRunTakesAFreshAccessTokenFoundUnderTheLock(t *testing.T) {
	w := tokenWorld(t, true)
	landed := underTheLock(w, storeLogin("A0b", "R0", rota.Now().Add(8*time.Hour).UnixMilli(), lineageUntil))
	_, err := launchEnv(t, w.s, w.a)
	w.p.dump(t)
	if !landed() {
		t.Fatal("the home was read under the lock")
	}
	if err != nil {
		t.Fatal(err)
	}
	if mine := w.p.byRota(); len(mine) != 0 || w.a.Token.Access != "A0b" || w.a.Token.Refresh != "R0" {
		t.Fatalf("taken, nothing presented: %+v, holds %s/%s", mine, w.a.Token.Access, w.a.Token.Refresh)
	}
	lockGone(t, w)
}

// tokenNow never presents the account's refresh token while something alive in
// the home may hold it: here the home holds the account's current login and a
// window runs there, although the home was last launched on a token.
func TestTokenNowPresentsNothingWhileAWindowCanReadTheCurrentLogin(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			home := w.s.Home(w.a)
			w.a.Extra[routeKey] = routeEnv
			alive(t, home, filepath.Join(claudeSessionsDir, "1.json"), os.Getpid())
			w.clock.pass(9 * time.Hour)
			h := w.s.claudeHome(w.a, false)
			if err := h.adopt(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !h.usable || !w.a.StagedCurrent() {
				t.Fatal("setup: the home holds the account's current login")
			}
			err := h.tokenNow(t.Context())
			w.p.dump(t)
			if mine := w.p.byRota(); len(mine) != 0 || !errors.Is(err, rota.ErrBusy) || w.a.Dead {
				t.Fatalf("refused, nothing presented, not dead: err=%v presented=%+v", err, mine)
			}
			lockGone(t, w)
		})
	}
}

// A store Claude Code blanked where the account's current login was is a
// rescue's to present, never a run's: tokenNow presents nothing, even in a
// quiet home.
func TestTokenNowPresentsNothingForABlankedLogin(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			writeFile(t, filepath.Join(w.s.Home(w.a), claudeCredentials), clearedLogin)
			w.clock.pass(9 * time.Hour)
			h := w.s.claudeHome(w.a, false)
			if err := h.adopt(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !h.blanked {
				t.Fatal("setup: the account's current login is blanked")
			}
			err := h.tokenNow(t.Context())
			w.p.dump(t)
			if mine := w.p.byRota(); len(mine) != 0 || !errors.Is(err, rota.ErrBusy) || w.a.Dead {
				t.Fatalf("refused, nothing presented, not dead: err=%v presented=%+v", err, mine)
			}
			lockGone(t, w)
		})
	}
}

// A hermetic run in a quiet home whose lock somebody holds is told to try again
// in a moment, as a run on a token is: Claude Code is refreshing the login.
func TestAHermeticRunIsToldToTryAgainWhileTheLockIsHeld(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			w.clock.pass(9 * time.Hour)
			if err := os.Mkdir(lockPath(w), 0o700); err != nil {
				t.Fatal(err)
			}
			_, release, err := w.s.prepare(t.Context(), w.a, false)
			if release != nil {
				release()
			}
			if !errors.Is(err, rota.ErrBusy) || !errors.Is(err, errLockHeld) || !strings.Contains(err.Error(), "try again in a moment") {
				t.Fatalf("told to try again in a moment: %v", err)
			}
			if mine := w.p.byRota(); len(mine) != 0 || w.a.Dead || w.a.Token.Refresh != "R0" {
				t.Fatalf("nothing presented, alive: %+v dead=%v", mine, w.a.Dead)
			}
		})
	}
}

// A home where the refresh lock cannot be made at all — one rota may not write
// in — is not taken for one whose lock somebody holds: every renewing look says
// so, presents nothing, and leaves the account alive.
func TestAHomeWhereTheLockCannotBeMadeIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes in any directory")
	}
	for _, m := range lineageModes() {
		for _, look := range renewingLooks() {
			t.Run(m.name+"/"+look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				w.clock.pass(9 * time.Hour)
				home := w.s.Home(w.a)
				// The run claim's file is there before the home is locked down.
				if f, err := os.OpenFile(filepath.Join(home, runLock), os.O_CREATE|os.O_RDWR, 0o600); err == nil {
					f.Close()
				}
				if err := os.Chmod(home, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
				var errs []error
				switch look.what {
				case "rota list":
					errs = w.s.Refresh(t.Context(), false, w.a)
				case "rota serve maintenance":
					errs = w.s.Maintain(t.Context())
				default:
					if _, err := launchEnv(t, w.s, w.a); err != nil {
						errs = append(errs, err)
					}
				}
				said := errors.Join(errs...)
				if said == nil || !strings.Contains(said.Error(), "refresh lock could not be made") {
					t.Fatalf("the look says the lock could not be made: %v", errs)
				}
				if mine := w.p.byRota(); len(mine) != 0 || w.a.Dead || w.a.Token.Refresh != "R0" {
					t.Fatalf("nothing presented, alive: %+v dead=%v", mine, w.a.Dead)
				}
			})
		}
	}
}

// A listing reads usage with a login it refreshed only once that login is in
// its home and the lock is let go, so a Claude Code waiting on the lock waits
// for the refresh and the write, and not for a reading.
func TestAListingReadsUsageOnlyAfterLettingGoOfTheLock(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			w.clock.pass(9 * time.Hour)
			var (
				mu     sync.Mutex
				reads  int
				locked bool
				homeRT string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/usage" {
					_, err := os.Stat(lockPath(w))
					held := refreshOf(w.cc.store())
					mu.Lock()
					reads++
					locked = locked || err == nil
					homeRT = held
					mu.Unlock()
				}
				w.p.serve(rw, r)
			}))
			t.Cleanup(srv.Close)
			old := rota.ClaudeEndpoints.Usage
			rota.ClaudeEndpoints.Usage = srv.URL + "/usage"
			t.Cleanup(func() { rota.ClaudeEndpoints.Usage = old })
			if errs := w.s.Refresh(t.Context(), false, w.a); len(errs) != 0 {
				t.Fatal(errs)
			}
			w.p.dump(t)
			mu.Lock()
			defer mu.Unlock()
			if reads != 1 || locked || homeRT != "R1" {
				t.Fatalf("one reading, after the lock went and with R1 in the home: %d reads, locked=%v, home %q",
					reads, locked, homeRT)
			}
			if mine := w.p.byRota(); len(mine) != 1 || !mine[0].lockHeld || w.a.Quota == nil {
				t.Fatalf("refreshed under the lock, read after: %+v quota=%v", mine, w.a.Quota)
			}
			lockGone(t, w)
		})
	}
}
