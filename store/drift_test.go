package store

// Claude Code's own refresh writes min(prev, now + refresh_token_expires_in), so
// the refreshTokenExpiresAt it leaves in the home may lie up to a second short of
// the one rota wrote, and rota takes that drifted end on with the rotation. The
// audit's scenarios again, in the lineage world (lineage_test.go), with a Claude
// Code whose every rotation drifts the end a little further: no end is compared
// exactly, so a drifted rotation is the account's own, and rota's older copy
// beside it is still older, never another lineage.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// driftStep is how far below the last one each rotation of drifting's Claude
// Code sets the end: a reply in whole seconds, read a little later each time.
const driftStep = 300 * time.Millisecond

// drifting is the lineage world's Claude Code with an end that drifts: the same
// refresh as fakeClaudeCode's, under its refresh lock, blanking the store on a
// refusal, but each rotation it writes ends driftStep further below
// lineageUntil.
type drifting struct {
	w *lineageWorld
	n int
}

func (d *drifting) refresh(who string) string {
	c := d.w.cc
	lock := filepath.Join(c.home, claudeRefreshLock)
	if err := os.Mkdir(lock, 0o700); err != nil {
		return "refresh lock held by somebody else: waited, read the store again"
	}
	defer os.Remove(lock)
	rt := refreshOf(c.store())
	if rt == "" {
		return "no token found"
	}
	st, body := d.w.p.present(who, rt)
	if st != 200 {
		c.write(clearedLogin)
		writeFile(c.t, filepath.Join(c.home, "daemon-auth-status.json"),
			`{"status":"auth_required","since":`+strconv.FormatInt(rota.NowMS(), 10)+`}`)
		return "refused: login blanked, auth_required"
	}
	d.n++
	m := body.(map[string]any)
	c.write(storeLogin(m["access_token"].(string), m["refresh_token"].(string), rota.Now().Add(8*time.Hour).UnixMilli(),
		d.until()))
	return "rotated to " + m["refresh_token"].(string)
}

// until is the end the last rotation wrote.
func (d *drifting) until() string {
	end, _ := strconv.ParseInt(lineageUntil, 10, 64)
	return strconv.FormatInt(end-int64(d.n)*driftStep.Milliseconds(), 10)
}

// One end is one grant within a minute, the same rule in the store and in lib:
// for each pair, lib takes a later login in the home as a rotation exactly when
// the store's sameGrant says the two ends are one grant.
func TestTheStoreTellsAGrantByItsEndAsLibDoes(t *testing.T) {
	for _, c := range []struct{ home, own string }{
		{"", ""},
		{lineageUntil, lineageUntil},
		{"1999999998700", lineageUntil},
		{"1999999939000", lineageUntil},
		{"2000000059000", lineageUntil},
		{"1999999938999", lineageUntil},
		{"2000000059001", lineageUntil},
		{lineageUntil, ""},
		{"", lineageUntil},
	} {
		a := &rota.Account{ID: 1, Provider: "claude", Token: rota.Token{Access: "A1", Refresh: "R1",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, Extra: map[string]string{}}
		if c.own != "" {
			a.Extra[refreshUntilKey] = c.own
		}
		a.StagedWritten()
		later := strconv.FormatInt(time.Now().Add(3*time.Hour).UnixMilli(), 10)
		doc := `{"claudeAiOauth":{"accessToken":"A2","refreshToken":"R2","expiresAt":` + later
		if c.home != "" {
			doc += `,"refreshTokenExpiresAt":` + c.home
		}
		err := rota.AdoptFrom(a, storeFS{store: []byte(doc + `}}`)})
		lib := err == nil && a.Token.Refresh == "R2"
		if store := sameGrant(c.home, c.own); store != lib {
			t.Errorf("home %q, account %q: the store says one grant=%v, lib %v (%v)", c.home, c.own, store, lib, err)
		}
	}
}

// S0 with a drifting end: rota follows every rotation of a Claude Code whose end
// drifts, journalled as rotations, presents nothing while it runs, and once the
// home is quiet presents the newest token.
func TestRotaFollowsAClaudeCodeWhoseRefreshDriftsTheEnd(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			d := &drifting{w: w}
			w.cc.start()
			for i := 0; i < 3; i++ {
				w.clock.pass(7*time.Hour + 56*time.Minute)
				t.Logf("daemon: %s", d.refresh("claude-code daemon"))
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				if w.a.Token.Refresh != w.p.current || !w.a.StagedCurrent() {
					t.Fatalf("rota follows the drifted rotation: holds %s, home %s", w.a.Token.Refresh, w.p.current)
				}
			}
			nothingPresented(t, w)
			for _, l := range journalOf(t, w.s) {
				if l["path"] == "adopt" && l["outcome"] != "rotation" {
					t.Fatalf("a drifted rotation is journalled as one: %v", l)
				}
			}
			journalSays(t, w.s, "adopt", "rotation")
			w.cc.stop()
			w.clock.pass(9 * time.Hour)
			lineageLooks()[0].do(t, w)
			if bad := w.p.rotaSpent(); len(bad) != 0 || w.a.Dead || w.a.Token.Refresh != w.p.current {
				w.p.dump(t)
				t.Fatalf("the quiet renew presents the newest: spent=%+v dead=%v holds %s", bad, w.a.Dead, w.a.Token.Refresh)
			}
		})
	}
}

// S1 and S1b-behind with a drifting end. rota took Claude Code's first, drifted
// rotation from the home, so its grant's end is no longer the one on rota's own
// seed file; Claude Code then rotates on unseen, and the home is blanked — by a
// refusal after the family was revoked from outside (S1), or with nothing
// refused while something still holds the newest token (S1b-behind). rota's copy
// is older than the home's last look: no look presents it, the account is dead
// and says why, rota's seed file is neither read as the login nor put back in
// front of Claude Code (it is gone where rota knew it as its own), the holder of
// the newest token goes on refreshing, and once the home is quiet the same look
// presents nothing either.
func TestADriftingEndLetsNoSpentCopyThrough(t *testing.T) {
	for _, scenario := range []string{"S1", "S1b-behind"} {
		for _, m := range lineageModes() {
			for _, seeded := range []bool{false, true} {
				for _, look := range lineageLooks() {
					name := scenario + "/" + m.name + "/" + map[bool]string{false: "seed unknown", true: "seed known"}[seeded] + "/" + look.what
					t.Run(name, func(t *testing.T) {
						w := newLineageWorld(t, m.keychain)
						if seeded {
							seededByRota(w.a)
						}
						d := &drifting{w: w}
						w.cc.start()
						lineageLooks()[0].do(t, w) // in sync on R0, rota's seed
						w.clock.pass(7*time.Hour + 56*time.Minute)
						t.Logf("daemon: %s", d.refresh("claude-code daemon"))
						lineageLooks()[1].do(t, w) // the watcher takes R1, and its drifted end
						if w.a.Token.Refresh != "R1" || w.a.Extra[refreshUntilKey] != d.until() {
							t.Fatalf("setup: rota holds R1 and its drifted end: %s %q", w.a.Token.Refresh, w.a.Extra[refreshUntilKey])
						}
						w.clock.pass(7*time.Hour + 56*time.Minute)
						t.Logf("daemon: %s (unseen)", d.refresh("claude-code daemon"))
						held := refreshOf(w.cc.store())
						if scenario == "S1" {
							w.clock.pass(7*time.Hour + 56*time.Minute)
							w.p.present("another holder of the lineage", "R1")
							t.Logf("daemon: %s", d.refresh("claude-code daemon"))
						} else {
							w.cc.write(clearedLogin) // a blank no refusal caused
						}
						w.clock.pass(10 * time.Minute)
						look.do(t, w) // something still runs in the home
						nothingPresented(t, w)
						deadForAnOlderCopy(t, w)
						if m.keychain {
							file := "R0"
							if seeded {
								file = "" // rota's own seed went once Claude Code had moved the login into the item
							}
							stillBehindTheItem(t, w, file)
						}
						if scenario == "S1b-behind" {
							if st, _ := w.p.present("holder of "+held+" (not re-read)", held); st != 200 {
								w.p.dump(t)
								t.Fatalf("the holder of %s keeps working: http %d", held, st)
							}
						}
						w.cc.stop()
						w.clock.pass(time.Minute)
						look.do(t, w) // and once the home is quiet
						nothingPresented(t, w)
						if !w.a.Dead {
							t.Fatal("still dead")
						}
					})
				}
			}
		}
	}
}

// S1b with rota current and a drifting end: a watcher took Claude Code's drifted
// rotations as they came, so rota's copy is the newest the home held, and the
// rescue presents it once, under Claude Code's refresh lock, rota's own older
// seed file beside the blanked item notwithstanding.
func TestARescueOfADriftedCopySeenAMomentAgoStillWorks(t *testing.T) {
	for _, m := range lineageModes() {
		for _, look := range lineageLooks() {
			t.Run(m.name+"/"+look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				d := &drifting{w: w}
				w.cc.start()
				lineageLooks()[0].do(t, w)
				for i := 0; i < 2; i++ {
					w.clock.pass(7*time.Hour + 56*time.Minute)
					t.Logf("daemon: %s", d.refresh("claude-code daemon"))
					lineageLooks()[1].do(t, w)
				}
				if w.a.Token.Refresh != "R2" {
					t.Fatalf("setup: rota follows to R2: %s", w.a.Token.Refresh)
				}
				w.cc.write(clearedLogin)
				w.clock.pass(2*WatchEvery - time.Minute)
				look.do(t, w)
				got := w.p.rotaPresented()
				if len(got) != 1 || got[0].rt != "R2" || got[0].wasSpent || !got[0].lockHeld || got[0].outcome != "rotated to R3" {
					w.p.dump(t)
					t.Fatalf("R2 presented once, under the lock: %+v", got)
				}
				if w.a.Dead || w.a.Token.Refresh != "R3" || refreshOf(w.cc.store()) != "R3" {
					t.Fatalf("alive on R3, and the home holds it: %v %q %q", w.a.Dead, w.a.Token.Refresh, refreshOf(w.cc.store()))
				}
				if got := d.refresh("claude-code daemon re-check"); got != "rotated to R4" {
					t.Fatalf("Claude Code goes on from there: %s", got)
				}
			})
		}
	}
}

// S2 with a drifting end: one look finds no keychain item while it is being
// rewritten, and rota's own older seed file is read in its place; rota still
// follows every drifted rotation after it, and the quiet renew presents the
// newest token.
func TestAKeychainMisreadWithADriftingEndDoesNotFreezeRota(t *testing.T) {
	w := newLineageWorld(t, true)
	d := &drifting{w: w}
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", d.refresh("claude-code daemon"))
	lineageLooks()[1].do(t, w)
	restore := freezeHideItem(w)
	lineageLooks()[1].do(t, w)
	restore()
	if w.a.Token.Refresh != "R1" || !w.a.StagedCurrent() || w.a.Dead {
		t.Fatalf("the misread changes nothing: holds %s, staged %q, dead=%v", w.a.Token.Refresh, w.a.Staged, w.a.Dead)
	}
	for n := 2; n <= 3; n++ {
		w.clock.pass(freezeEvery)
		t.Logf("daemon: %s", d.refresh("claude-code daemon"))
		for _, look := range lineageLooks() {
			look.do(t, w)
		}
		if want := "R" + itoa(n); w.a.Token.Refresh != want || !w.a.StagedCurrent() {
			t.Fatalf("rota follows to %s: holds %s", want, w.a.Token.Refresh)
		}
	}
	w.cc.stop()
	w.clock.pass(9 * time.Hour)
	lineageLooks()[0].do(t, w)
	if bad := w.p.rotaSpent(); len(bad) != 0 || w.a.Dead || w.a.Token.Refresh != "R4" {
		w.p.dump(t)
		t.Fatalf("the quiet renew presents the newest: spent=%+v dead=%v holds %s", bad, w.a.Dead, w.a.Token.Refresh)
	}
}

// S3 with a drifting end: rota holds a rotation of Claude Code's with a drifted
// end, the home goes quiet and the token runs out, and a Claude Code that starts
// while rota's refresh is under way finds rota's lock and waits, then goes on
// from the login rota wrote.
func TestAClaudeCodeStartingDuringTheRefreshOfADriftedLoginWaits(t *testing.T) {
	for _, m := range lineageModes() {
		for _, look := range renewingLooks() {
			t.Run(m.name+"/"+look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				d := &drifting{w: w}
				w.cc.start()
				lineageLooks()[0].do(t, w)
				w.clock.pass(7*time.Hour + 56*time.Minute)
				t.Logf("daemon: %s", d.refresh("claude-code daemon"))
				lineageLooks()[1].do(t, w)
				w.cc.stop()
				w.clock.pass(9 * time.Hour)
				var cc string
				w.p.onRota = func(string) {
					if cc == "" {
						cc = d.refresh("claude-code window started meanwhile")
					}
				}
				look.do(t, w)
				w.p.onRota = nil
				if !strings.HasPrefix(cc, "refresh lock held") {
					t.Fatalf("Claude Code finds rota's lock: %q", cc)
				}
				got := w.p.rotaPresented()
				if len(got) != 1 || got[0].rt != "R1" || got[0].wasSpent || !got[0].lockHeld {
					w.p.dump(t)
					t.Fatalf("R1 presented once, under the lock: %+v", got)
				}
				if got := d.refresh("claude-code window"); got != "rotated to R3" {
					t.Fatalf("Claude Code goes on from the login rota wrote: %s", got)
				}
				if bad := w.p.rotaSpent(); len(bad) != 0 || w.a.Dead {
					t.Fatalf("spent=%+v dead=%v", bad, w.a.Dead)
				}
			})
		}
	}
}
