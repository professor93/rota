package store

// The launch lead, in the lineage world (lineage_test.go): a provider that
// spends each refresh token once and revokes the whole family when a spent one
// comes back, and a fake Claude Code that takes the same refresh lock around
// each refresh of its own.
//
// A launch into a quiet home whose login is in sync, with the access token
// inside the lead of its expiry, refreshes the login first, under Claude
// Code's refresh lock, so the Claude Code it starts there begins on a fresh
// token instead of meeting Claude Code's own five-minute window in step with
// whatever else starts there. It does so once, never while anything runs in
// the home, never forks the lineage, and a lead the provider refuses leaves
// the account as a refusal of the refresh a due launch makes leaves it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// leadWorld is the lineage world with A0 left to go before it expires: the
// account and its home in sync on R0, nothing alive there.
func leadWorld(t *testing.T, keychain bool, left time.Duration) *lineageWorld {
	t.Helper()
	w := newLineageWorld(t, keychain)
	w.clock.pass(8*time.Hour - left)
	return w
}

// leadOutcome is the last line on path in the journal with an outcome that
// begins with prefix, or "".
func leadOutcome(t *testing.T, s *Store, path, prefix string) string {
	t.Helper()
	var got string
	for _, l := range journalOf(t, s) {
		if o, _ := l["outcome"].(string); l["path"] == path && strings.HasPrefix(o, prefix) {
			got = o
		}
	}
	return got
}

// onDisk is the store's account as accounts.json has it.
func onDisk(t *testing.T, dir string) rota.Account {
	t.Helper()
	var disk Store
	if err := json.Unmarshal([]byte(readFile(filepath.Join(dir, "accounts.json"))), &disk); err != nil || len(disk.Accounts) != 1 {
		t.Fatalf("on disk: %v %+v", err, disk.Accounts)
	}
	return *disk.Accounts[0]
}

// Inside the lead, a launch is the one look that refreshes the login before it
// starts Claude Code, and it does so once: R0, the home's newest, is presented
// under Claude Code's refresh lock while nothing runs there, R1 is in the home
// before the launch goes on, and no look after it — a launch, a listing, the
// watcher's rescue, maintenance — presents anything. The other three looks,
// inside the lead before the launch, present nothing: a lead is a launch's
// alone. The journal names the refresh a lead, with the token's real expiry.
func TestALaunchIntoAQuietHomeRefreshesALoginNearItsEndOnce(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := leadWorld(t, m.keychain, 20*time.Minute)
			expires := w.a.Token.ExpiresAt
			looks := lineageLooks()
			for _, look := range looks[:3] {
				look.do(t, w)
			}
			if mine := w.p.byRota(); len(mine) != 0 {
				t.Fatalf("only a launch leads: %+v", mine)
			}
			env, err := launchEnv(t, w.s, w.a)
			if err != nil {
				t.Fatal(err)
			}
			w.p.dump(t)
			mine := w.p.byRota()
			if len(mine) != 1 || mine[0].rt != "R0" || mine[0].wasSpent || !mine[0].lockHeld || mine[0].homeRT != "R0" ||
				mine[0].daemon {
				t.Fatalf("R0 once, the home's newest, under the lock, nothing alive: %+v", mine)
			}
			if w.a.Dead || w.a.Token.Refresh != "R1" || !w.a.StagedCurrent() || refreshOf(w.cc.store()) != "R1" {
				t.Fatalf("rota and the home hold R1: dead=%v %q rota %s home %s", w.a.Dead, w.a.WhyDead(), w.a.Token.Refresh,
					refreshOf(w.cc.store()))
			}
			if hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Fatalf("the launch goes on the stored route: %q", env)
			}
			lockGone(t, w)
			for _, outcome := range []string{"presenting", "rotated"} {
				e := journalSays(t, w.s, "launch.lead", outcome)
				if e["exp_before"] != journalTime(expires) || e["lock"] != true {
					t.Fatalf("%s: exp_before %v (truly %v), lock %v", outcome, e["exp_before"], journalTime(expires), e["lock"])
				}
			}
			if journalLine(journalOf(t, w.s), "launch.renew", "presenting") != nil {
				t.Fatal("a lead is not journalled as a renew")
			}
			for i := 0; i < 2; i++ {
				for _, look := range looks {
					look.do(t, w)
				}
			}
			if n := len(w.p.byRota()); n != 1 {
				w.p.dump(t)
				t.Fatalf("the lead happens once: %d presentations", n)
			}
		})
	}
}

// While anything at all runs in the home — Claude Code's daemon, a window, or
// another rota run holding the account — no look leads, a launch included: the
// login is the home's, and Claude Code refreshes it in its own window, which
// rota then follows, presenting nothing.
func TestNoLookLeadsWhileAnythingRunsInTheHome(t *testing.T) {
	running := []struct {
		what string
		make func(t *testing.T, w *lineageWorld) (stop func())
	}{
		{"daemon", func(t *testing.T, w *lineageWorld) func() {
			w.cc.start()
			return w.cc.stop
		}},
		{"a window", func(t *testing.T, w *lineageWorld) func() {
			rec := filepath.Join(claudeSessionsDir, "1.json")
			alive(t, w.cc.home, rec, os.Getpid())
			return func() { _ = os.Remove(filepath.Join(w.cc.home, rec)) }
		}},
		{"another rota run", func(t *testing.T, w *lineageWorld) func() {
			release, ok := w.s.holdRun(w.a)
			if !ok {
				t.Fatal("the claim")
			}
			return release
		}},
	}
	for _, m := range lineageModes() {
		for _, c := range running {
			t.Run(m.name+"/"+c.what, func(t *testing.T) {
				w := leadWorld(t, m.keychain, 20*time.Minute)
				stop := c.make(t, w)
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				if mine := w.p.byRota(); len(mine) != 0 {
					t.Fatalf("nothing presented while something runs in the home: %+v", mine)
				}
				if w.a.Dead || w.a.Token.Refresh != "R0" || refreshOf(w.cc.store()) != "R0" {
					t.Fatalf("as it was: dead=%v %q rota %s", w.a.Dead, w.a.WhyDead(), w.a.Token.Refresh)
				}
				noPresenting(t, w.s)
				w.clock.pass(16 * time.Minute)
				if got := w.cc.refresh("claude-code, in its own window"); got != "rotated to R1" {
					t.Fatalf("Claude Code's own refresh: %q", got)
				}
				stop()
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				w.p.dump(t)
				if mine := w.p.byRota(); len(mine) != 0 || w.a.Token.Refresh != "R1" || w.a.Dead {
					t.Fatalf("rota follows Claude Code and presents nothing: %+v, holds %s", mine, w.a.Token.Refresh)
				}
				if bad := w.p.rotaSpent(); len(bad) != 0 || w.p.revoked != "" {
					t.Fatalf("no reuse: %+v %q", bad, w.p.revoked)
				}
			})
		}
	}
}

// Outside the lead nothing changes: every look presents nothing while the token
// has longer than the lead to go, and the first launch once it is inside does.
// With no lead at all, no launch leads.
func TestALaunchOutsideTheLeadRefreshesNothing(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := leadWorld(t, m.keychain, launchLead+time.Minute)
			for _, look := range lineageLooks() {
				look.do(t, w)
			}
			if mine := w.p.byRota(); len(mine) != 0 {
				t.Fatalf("nothing presented outside the lead: %+v", mine)
			}
			w.clock.pass(2 * time.Minute)
			if _, err := launchEnv(t, w.s, w.a); err != nil {
				t.Fatal(err)
			}
			if mine := w.p.byRota(); len(mine) != 1 || mine[0].rt != "R0" || w.a.Token.Refresh != "R1" {
				t.Fatalf("inside the lead the launch leads: %+v", mine)
			}
		})
		t.Run(m.name+"/no lead", func(t *testing.T) {
			was := launchLead
			launchLead = 0
			t.Cleanup(func() { launchLead = was })
			w := leadWorld(t, m.keychain, 10*time.Minute)
			for _, look := range lineageLooks() {
				look.do(t, w)
			}
			if mine := w.p.byRota(); len(mine) != 0 || w.a.Token.Refresh != "R0" {
				t.Fatalf("no lead, nothing presented: %+v", mine)
			}
		})
	}
}

// The lead presents nothing a refresh that falls due would not: not a login
// past its end, nor one that ends before its access token runs out, which no
// refresh may renew before it ends. Such an account is alive on its token, as
// before, until that runs out. A home that does not hold the account's current
// login gets it written in, as before, and no lead. One whose login ends after
// its token gets the lead as any other.
func TestALeadPresentsNothingANearRefreshWouldNot(t *testing.T) {
	cases := []struct {
		what  string
		end   time.Duration // from now; 0 keeps the world's own
		older bool          // the home holds an older login of the account's
		leads bool
	}{
		{"its login ended an hour ago", -time.Hour, false, false},
		{"its login ends before its token", 10 * time.Minute, false, false},
		{"its home holds an older login", 0, true, false},
		{"its login ends after its token", 2 * time.Hour, false, true},
	}
	for _, m := range lineageModes() {
		for _, c := range cases {
			t.Run(m.name+"/"+c.what, func(t *testing.T) {
				w := leadWorld(t, m.keychain, 20*time.Minute)
				until := lineageUntil
				if c.end != 0 {
					until = strconv.FormatInt(rota.Now().Add(c.end).UnixMilli(), 10)
					w.a.Extra[refreshUntilKey] = until
				}
				doc := storeLogin("A0", "R0", w.a.Token.ExpiresAt, until)
				if c.older {
					doc = storeLogin("A-older", "R-older", w.a.Token.ExpiresAt-time.Hour.Milliseconds(), until)
				}
				writeFile(t, filepath.Join(w.cc.home, claudeCredentials), doc)
				if err := w.s.Save(); err != nil {
					t.Fatal(err)
				}
				if _, err := launchEnv(t, w.s, w.a); err != nil {
					t.Fatal(err)
				}
				w.p.dump(t)
				want := 0
				if c.leads {
					want = 1
				}
				if n := len(w.p.byRota()); n != want {
					t.Fatalf("%d presentations, want %d", n, want)
				}
				if w.a.Dead {
					t.Fatalf("alive, as before: %s", w.a.WhyDead())
				}
				if c.older && refreshOf(w.cc.store()) != "R0" {
					t.Fatalf("the account's current login goes in, as before: %s", refreshOf(w.cc.store()))
				}
			})
		}
	}
}

// The audit's S3 for a lead: a Claude Code that starts in the home while the
// lead's refresh is with the provider finds the refresh lock held and waits,
// then reads R1, which is in the home when the lock goes, and refreshes that.
// Nothing is presented twice, and every look after follows Claude Code.
func TestClaudeCodeStartingDuringALeadWaitsForTheLock(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := leadWorld(t, m.keychain, 20*time.Minute)
			var meanwhile string
			w.p.onRota = func(string) {
				if meanwhile == "" {
					w.cc.start()
					meanwhile = w.cc.refresh("claude-code window started meanwhile")
				}
			}
			if _, err := launchEnv(t, w.s, w.a); err != nil {
				t.Fatal(err)
			}
			w.p.onRota = nil
			if !strings.HasPrefix(meanwhile, "refresh lock held") {
				t.Fatalf("Claude Code should have found rota's lock: %q", meanwhile)
			}
			lockGone(t, w)
			if got := w.cc.refresh("claude-code window, after the wait"); got != "rotated to R2" {
				t.Fatalf("the Claude Code that waited reads rota's R1 and refreshes it: %q", got)
			}
			for _, look := range lineageLooks() {
				look.do(t, w)
			}
			w.p.dump(t)
			if bad := w.p.rotaSpent(); len(bad) != 0 || w.p.revoked != "" {
				t.Fatalf("no reuse: %+v %q", bad, w.p.revoked)
			}
			if w.a.Dead || w.a.Token.Refresh != "R2" || len(w.p.byRota()) != 1 {
				t.Fatalf("rota follows Claude Code: dead=%v %q holds %s", w.a.Dead, w.a.WhyDead(), w.a.Token.Refresh)
			}
		})
	}
}

// The home is asked again once the lead holds the lock: a Claude Code that
// started there after the launch's first look and before the lock was taken has
// the login, and nothing is presented; the launch goes on and joins it.
func TestAClaudeCodeThatStartedBeforeTheLeadTookTheLockIsLeftTheLogin(t *testing.T) {
	w := leadWorld(t, true, 20*time.Minute)
	started := false
	w.k.onFind = func(string) {
		if _, err := os.Stat(lockPath(w)); err == nil && !started {
			started = true
			w.cc.start()
		}
	}
	if _, err := launchEnv(t, w.s, w.a); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("the lead reads the home under the lock")
	}
	if mine := w.p.byRota(); len(mine) != 0 {
		t.Fatalf("nothing presented once Claude Code runs there: %+v", mine)
	}
	if w.a.Dead || w.a.Token.Refresh != "R0" {
		t.Fatalf("as it was: %v %s", w.a.Dead, w.a.Token.Refresh)
	}
	lockGone(t, w)
	noPresenting(t, w.s)
}

// A lead that does not get through — the provider failing, an unknown code, a
// reply that never comes, or Claude Code holding its refresh lock — stops no
// launch: it goes on on the login as it was, the real expiry kept on the
// account and on disk, and says nothing beyond the journal. The next launch
// leads again, and presents R0 once: the provider never saw it.
func TestALeadThatDoesNotGetThroughStopsNoLaunch(t *testing.T) {
	failures := []struct {
		what    string
		answer  func(http.ResponseWriter) // nil: the refresh lock is held instead
		outcome string
	}{
		{"server error", func(rw http.ResponseWriter) {
			rw.WriteHeader(http.StatusInternalServerError)
		}, "transient:"},
		{"unknown code", func(rw http.ResponseWriter) {
			rw.WriteHeader(http.StatusBadRequest)
			_, _ = rw.Write([]byte(`{"error":"temporarily_overloaded","error_description":"try later"}`))
		}, "transient:"},
		{"no answer", func(rw http.ResponseWriter) {
			if conn, _, err := rw.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
		}, "transient:"},
		{"lock held", nil, "skipped:lock-held"},
	}
	for _, m := range lineageModes() {
		for _, f := range failures {
			t.Run(m.name+"/"+f.what, func(t *testing.T) {
				w := leadWorld(t, m.keychain, 20*time.Minute)
				warned := said(w.s)
				expires := w.a.Token.ExpiresAt
				var failing *failingToken
				if f.answer != nil {
					failing = failRota(t, w, f.answer)
				} else if err := os.Mkdir(lockPath(w), 0o700); err != nil {
					t.Fatal(err)
				}
				if _, err := launchEnv(t, w.s, w.a); err != nil {
					t.Fatalf("a lead that does not get through stops no launch: %v", err)
				}
				if w.a.Dead || w.a.Token.Refresh != "R0" || w.a.Token.Access != "A0" || w.a.Token.ExpiresAt != expires ||
					!w.a.StagedCurrent() || refreshOf(w.cc.store()) != "R0" {
					t.Fatalf("the login as it was: dead=%v %q %s %s exp %d want %d", w.a.Dead, w.a.WhyDead(), w.a.Token.Refresh,
						w.a.Token.Access, w.a.Token.ExpiresAt, expires)
				}
				if disk := onDisk(t, storeDir(w.s)); disk.Token.ExpiresAt != expires || disk.Dead {
					t.Fatalf("on disk: expires %d, want %d; dead %v", disk.Token.ExpiresAt, expires, disk.Dead)
				}
				if got := leadOutcome(t, w.s, "launch.lead", f.outcome); got == "" {
					t.Fatalf("the journal says the lead's %s", f.outcome)
				}
				if len(*warned) != 0 {
					t.Fatalf("nothing said: %q", *warned)
				}
				if failing != nil {
					if got := failing.presented(); len(got) != 1 || got[0] != "R0" {
						t.Fatalf("R0 presented once: %q", got)
					}
					failing.stop()
					lockGone(t, w)
				} else {
					if _, err := os.Stat(lockPath(w)); err != nil {
						t.Fatalf("the lock is not rota's to touch: %v", err)
					}
					_ = os.Remove(lockPath(w))
				}
				if _, err := launchEnv(t, w.s, w.a); err != nil {
					t.Fatal(err)
				}
				w.p.dump(t)
				if mine := w.p.byRota(); len(mine) != 1 || mine[0].rt != "R0" || mine[0].wasSpent || w.a.Token.Refresh != "R1" {
					t.Fatalf("the next launch leads, once: %+v, holds %s", mine, w.a.Token.Refresh)
				}
			})
		}
	}
}

// A launch whose caller goes away under the lead's lock presents nothing and
// starts nothing, as one making a due refresh would.
func TestALeadWhoseCallerLeftPresentsNothing(t *testing.T) {
	w := leadWorld(t, true, 20*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.k.onFind = func(string) {
		if _, err := os.Stat(lockPath(w)); err == nil {
			cancel()
		}
	}
	_, _, err := w.s.prepare(ctx, w.a, true)
	if !Left(err) {
		t.Fatalf("the launch says whoever asked went away: %v", err)
	}
	if n := len(w.p.entries()); n != 0 {
		t.Fatalf("nothing sent: %d", n)
	}
	if e := journalSays(t, w.s, "launch.lead", "skipped:caller-left"); e["lock"] != true {
		t.Fatalf("stopped under the lock: %v", e)
	}
	noPresenting(t, w.s)
}

// refusedLaunch revokes the lineage, launches once, and then makes every look
// twice, and says what became of the account and of the launch. Only the
// launch presents the refused R0, once; nothing presents it again.
type refusedOutcome struct {
	dead           bool
	reason         string
	reauth, failed bool
	onLong         bool
}

func refusedLaunch(t *testing.T, w *lineageWorld, long bool) refusedOutcome {
	t.Helper()
	if long {
		w.a.Long = &rota.LongToken{Access: "L0", ExpiresAt: rota.Now().Add(300 * 24 * time.Hour).UnixMilli()}
		if err := w.s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	w.p.revoke("the login was signed out elsewhere")
	env, err := launchEnv(t, w.s, w.a)
	t.Logf("launch: %v", err)
	o := refusedOutcome{dead: w.a.Dead, reason: w.a.DeadReason, reauth: errors.Is(err, rota.ErrReauth), failed: err != nil,
		onLong: slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=L0")}
	for i := 0; i < 2; i++ {
		for _, look := range lineageLooks() {
			look.do(t, w)
		}
	}
	w.p.dump(t)
	if mine := w.p.byRota(); len(mine) != 1 || mine[0].rt != "R0" {
		t.Fatalf("R0 is presented once and refused, then never again: %+v", mine)
	}
	if !w.a.Dead || w.a.DeadReason != o.reason {
		t.Fatalf("dead, and the reason stays: %v %q, was %q", w.a.Dead, w.a.DeadReason, o.reason)
	}
	if disk := onDisk(t, storeDir(w.s)); !disk.Dead || disk.DeadReason != o.reason {
		t.Fatalf("on disk: %v %q", disk.Dead, disk.DeadReason)
	}
	return o
}

// A lead the provider refuses leaves the account exactly as a refusal of the
// refresh a launch makes when the token has run out: dead with the provider's
// reason, the launch refused as re-auth needed — or, with a long-lived token,
// run on that — and the refused token presented to no one again by any look.
func TestARefusedLeadLeavesTheAccountDeadAsADueRefreshWould(t *testing.T) {
	for _, m := range lineageModes() {
		for _, long := range []bool{false, true} {
			name := m.name
			if long {
				name += "/with a long-lived token"
			}
			var due, lead refusedOutcome
			t.Run(name+"/due", func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				w.clock.pass(9 * time.Hour)
				due = refusedLaunch(t, w, long)
			})
			t.Run(name+"/lead", func(t *testing.T) {
				w := leadWorld(t, m.keychain, 20*time.Minute)
				lead = refusedLaunch(t, w, long)
				if got := leadOutcome(t, w.s, "launch.lead", "refused:"); got != "refused:invalid_grant" {
					t.Fatalf("the journal says the lead was refused: %q", got)
				}
			})
			if !due.dead || due != lead {
				t.Fatalf("%s: a refused lead %+v\nleaves the account as a refused due refresh does %+v", name, lead, due)
			}
			if due.failed == long || due.onLong != long {
				t.Fatalf("%s: the launch is refused, or runs on the long-lived token: %+v", name, due)
			}
		}
	}
}

// ordered puts an orderBackend under the world's store, on the same files.
func ordered(w *lineageWorld) *orderBackend {
	ob := &orderBackend{FileBackend: w.s.backend.(*FileBackend)}
	w.s.backend = ob
	return ob
}

// A lead's refresh is kept as any refresh of rota's is: saved before it goes
// into the home; and when that save fails, it goes into the home all the same
// as the only copy, the launch is refused, and the next look reads it back from
// there as a rotation, presenting nothing.
func TestALeadsRefreshIsSavedFirstAndNeverLost(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name+"/saved, then written", func(t *testing.T) {
			w := leadWorld(t, m.keychain, 20*time.Minute)
			ob := ordered(w)
			ob.watch(w.s, w.a, "R1")
			if _, err := launchEnv(t, w.s, w.a); err != nil {
				t.Fatal(err)
			}
			ob.savedThenWritten(t)
		})
		t.Run(m.name+"/not saved", func(t *testing.T) {
			w := leadWorld(t, m.keychain, 20*time.Minute)
			dir := storeDir(w.s)
			ob := ordered(w)
			w.p.onRota = func(string) { ob.failSaves = 2 }
			if _, err := launchEnv(t, w.s, w.a); err == nil || !strings.Contains(err.Error(), "could not be saved") {
				t.Fatalf("the launch is refused: %v", err)
			}
			w.p.onRota = nil
			if got := refreshOf(w.cc.store()); got != "R1" {
				t.Fatalf("the home holds R1, the only copy: %q", got)
			}
			if disk := onDisk(t, dir); disk.Token.Refresh != "R0" {
				t.Fatalf("setup: the store on disk is behind: %s", disk.Token.Refresh)
			}
			w.s.Close()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			a := s.Find(w.a.ID)
			if _, err := launchEnv(t, s, a); err != nil {
				t.Fatal(err)
			}
			w.p.dump(t)
			if a.Dead || a.Token.Refresh != "R1" || len(w.p.byRota()) != 1 {
				t.Fatalf("the next look reads R1 back and presents nothing: dead=%v holds %s", a.Dead, a.Token.Refresh)
			}
			if bad := w.p.rotaSpent(); len(bad) != 0 || w.p.revoked != "" {
				t.Fatalf("no reuse: %+v %q", bad, w.p.revoked)
			}
		})
	}
}

// A lead whose new login cannot go into the home — it was locked down while the
// refresh was with the provider — starts no Claude Code there on the login the
// lead has just spent: the launch is refused, as after a renew, the new login
// is saved and owed to the home, and the next look writes it in with no
// refresh of its own.
func TestALeadWhoseLoginCannotGoIntoTheHomeStartsNothingOnTheSpentOne(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a directory's mode keeps nobody out here")
	}
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := leadWorld(t, m.keychain, 20*time.Minute)
			home := w.cc.home
			t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
			w.p.onRota = func(string) { _ = os.Chmod(home, 0o500) }
			_, err := launchEnv(t, w.s, w.a)
			w.p.onRota = nil
			if err == nil || !strings.Contains(err.Error(), "could not be written into its home") {
				t.Fatalf("the launch is refused: %v", err)
			}
			if err := os.Chmod(home, 0o700); err != nil {
				t.Fatal(err)
			}
			// rota could not take its lock back out of a home it could not write
			// in; Claude Code takes such a lock for dead a minute after its last
			// touch, and so does rota, which a test does not wait for.
			_ = os.Remove(lockPath(w))
			if got := refreshOf(w.cc.store()); got != "R0" {
				t.Fatalf("setup: the home still holds R0: %q", got)
			}
			if disk := onDisk(t, storeDir(w.s)); disk.Token.Refresh != "R1" || disk.Extra[seedOwedKey] == "" {
				t.Fatalf("R1 is saved and owed to the home: %s %v", disk.Token.Refresh, disk.Extra)
			}
			if _, err := launchEnv(t, w.s, w.a); err != nil {
				t.Fatal(err)
			}
			w.p.dump(t)
			if got := refreshOf(w.cc.store()); got != "R1" || len(w.p.byRota()) != 1 {
				t.Fatalf("the next look writes R1 in and presents nothing: home %q, %d presentations", got, len(w.p.byRota()))
			}
			if bad := w.p.rotaSpent(); len(bad) != 0 || w.p.revoked != "" {
				t.Fatalf("no reuse: %+v %q", bad, w.p.revoked)
			}
		})
	}
}

// A lead is journalled under the name of the launch's caller, with ".lead" in
// place of a renew's ".renew".
func TestALeadIsJournalledUnderItsCallersName(t *testing.T) {
	storedRouteHere(t)
	for _, c := range []struct{ name, path string }{
		{"", "launch.lead"},
		{"holder", "holder.lead"},
		{"detach", "detach.lead"},
	} {
		t.Run(c.path, func(t *testing.T) {
			onPath(t, "claude")
			w := leadWorld(t, false, 20*time.Minute)
			ctx := context.Background()
			if c.name != "" {
				ctx = WithCaller(ctx, c.name)
			}
			_, _, release, err := w.s.Prepare(ctx, w.a)
			if err != nil {
				t.Fatal(err)
			}
			release()
			journalSays(t, w.s, c.path, "presenting")
			journalSays(t, w.s, c.path, "rotated")
		})
	}
}

// Day after day of launches into a quiet home, each inside the lead: the launch
// refreshes the login first, the Claude Code it started runs on R(n) and
// refreshes it in its own window eight hours on, and stops; every look of
// rota's between. Over three days rota presents once a day, always the newest
// token, Claude Code's refreshes all go through, and the lineage is never
// forked: nothing is presented twice and the family is never revoked.
func TestLaunchesWithALeadNeverForkTheLineage(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := leadWorld(t, m.keychain, 20*time.Minute)
			for day := 0; day < 3; day++ {
				if _, err := launchEnv(t, w.s, w.a); err != nil {
					t.Fatalf("day %d: %v", day, err)
				}
				w.cc.start()
				w.clock.pass(30 * time.Minute) // past where the old token ran out: nothing is due
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				w.clock.pass(7*time.Hour + 26*time.Minute)
				if got := w.cc.refresh("claude-code, in its own window"); !strings.HasPrefix(got, "rotated to") {
					t.Fatalf("day %d: Claude Code's refresh: %q", day, got)
				}
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				w.cc.stop()
				w.clock.pass(7*time.Hour + 40*time.Minute) // twenty minutes left on Claude Code's token
			}
			w.p.dump(t)
			if bad := w.p.rotaSpent(); len(bad) != 0 || w.p.revoked != "" {
				t.Fatalf("no reuse: %+v %q", bad, w.p.revoked)
			}
			if mine := w.p.byRota(); len(mine) != 3 {
				t.Fatalf("one lead a day: %+v", mine)
			}
			for _, e := range w.p.byRota() {
				if e.homeRT != e.rt || !e.lockHeld || e.daemon {
					t.Fatalf("each lead presents the home's newest, under the lock, nothing alive: %+v", e)
				}
			}
			if w.a.Dead || w.a.Token.Refresh != w.p.current {
				t.Fatalf("rota holds the newest: %s, provider %s", w.a.Token.Refresh, w.p.current)
			}
		})
	}
}
