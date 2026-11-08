package store

// A rescue presents only a copy rota can show is the home's newest. The
// audit's S1 and S1b, replayed in the lineage world (lineage_test.go) — a
// provider that spends each refresh token once and revokes the whole family
// when a spent one comes back, and a fake Claude Code in the home — now as
// assertions of what rota does, on the keychain and on the file, from every
// look.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// seenInSync records that a look found the account's current login in its home
// just now, as every look that reads it there does: what lets a rescue show its
// copy is the newest the home has held.
func seenInSync(a *rota.Account) {
	setExtra(a, homeSyncedKey, strconv.FormatInt(rota.NowMS(), 10))
	setExtra(a, homeSyncedFPKey, a.Staged)
}

// olderCopyWords is the reason, word for word, an account is dead whose rescue
// rota would not make.
func olderCopyWords(a *rota.Account) string {
	return fmt.Sprintf("Claude Code's refresh was refused and rota's copy is older than the home's last look, "+
		"so rota did not present it; rota login %d makes a new login", a.ID)
}

// rotaPresented is every refresh token rota presented, spent or not.
func (p *lineageProvider) rotaPresented() []lineagePresented {
	var out []lineagePresented
	for _, e := range p.entries() {
		if e.who == "rota" {
			out = append(out, e)
		}
	}
	return out
}

// nothingPresented fails when rota presented any refresh token at all.
func nothingPresented(t *testing.T, w *lineageWorld) {
	t.Helper()
	if got := w.p.rotaPresented(); len(got) != 0 {
		w.p.dump(t)
		t.Fatalf("rota presented a refresh token: %+v", got)
	}
}

// deadForAnOlderCopy fails unless the account is dead for the one reason, and
// that is on disk.
func deadForAnOlderCopy(t *testing.T, w *lineageWorld) {
	t.Helper()
	if !w.a.Dead || w.a.WhyDead() != olderCopyWords(w.a) {
		t.Fatalf("dead, and says why: %v %q", w.a.Dead, w.a.WhyDead())
	}
	if !strings.Contains(allFiles(t, storeDir(w.s)), "so rota did not present it") {
		t.Fatal("and that is saved")
	}
}

// stillBehindTheItem fails unless the home is as the keychain run left it: the
// blanked item in front, and rota's older file still behind it, never uncovered.
func stillBehindTheItem(t *testing.T, w *lineageWorld, file string) {
	t.Helper()
	if v, ok := w.k.item(w.cc.svc); !ok || refreshOf(v) != "" {
		t.Fatalf("the blanked item stays in front: %v %q", ok, refreshOf(v))
	}
	if r, _ := readLogin(t, w.cc.home); r != file {
		t.Fatalf("and the file behind it is not touched: %q", r)
	}
}

// S1, account 13 replayed: rota in sync on R0, the daemon rotates twice while
// nobody looks, the family is revoked from outside rota, and the daemon's next
// refresh is refused and blanks the store. rota's R0 was spent sixteen hours
// earlier, and rota's last look in sync was a day ago, so no look presents it:
// the account is dead and says why, and on the keychain rota's older file
// beside the blanked item is neither read as the login nor put back in front of
// Claude Code.
func TestARescuePresentsNoCopyTheDaemonRotatedPastUnseen(t *testing.T) {
	for _, m := range lineageModes() {
		for _, look := range lineageLooks() {
			t.Run(m.name+"/"+look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				w.cc.start()
				lineageLooks()[0].do(t, w) // in sync on R0
				for i := 0; i < 2; i++ {
					w.clock.pass(7*time.Hour + 56*time.Minute)
					t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
				}
				w.clock.pass(7*time.Hour + 56*time.Minute)
				w.p.present("another holder of the lineage", "R0")
				t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
				w.clock.pass(10 * time.Minute)
				look.do(t, w)
				nothingPresented(t, w)
				deadForAnOlderCopy(t, w)
				if m.keychain {
					stillBehindTheItem(t, w, "R0")
				}
				if got := w.cc.refresh("claude-code daemon re-check"); got != "no token found" {
					t.Fatalf("the home holds nothing for Claude Code to present: %s", got)
				}
				for _, again := range lineageLooks() {
					again.do(t, w)
				}
				nothingPresented(t, w)
			})
		}
	}
}

// S1b with rota behind: a blank no refusal caused, while something still holds
// the newest token R2 without having re-read the store. rota's R0 is two
// rotations old, so nothing is presented and nothing is uncovered, and the
// holder of R2 goes on refreshing: the lineage lives.
func TestARescueBehindTheHomeLeavesItsLineageToWhoeverHoldsTheNewest(t *testing.T) {
	for _, m := range lineageModes() {
		for _, look := range lineageLooks() {
			t.Run(m.name+"/"+look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				w.cc.start()
				lineageLooks()[0].do(t, w)
				for i := 0; i < 2; i++ {
					w.clock.pass(7*time.Hour + 56*time.Minute)
					t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
				}
				held := refreshOf(w.cc.store())
				w.cc.write(clearedLogin) // a blank no provider refusal caused
				w.clock.pass(10 * time.Minute)
				look.do(t, w)
				nothingPresented(t, w)
				deadForAnOlderCopy(t, w)
				if m.keychain {
					stillBehindTheItem(t, w, "R0")
				}
				if got := w.cc.refresh("claude-code daemon re-check"); got != "no token found" {
					t.Fatalf("nothing stale is put in front of Claude Code: %s", got)
				}
				if st, _ := w.p.present("holder of "+held+" (not re-read)", held); st != 200 {
					w.p.dump(t)
					t.Fatalf("the holder of %s keeps working: http %d", held, st)
				}
			})
		}
	}
}

// S1b with rota current: a watcher's look found the home holding R2 a few
// minutes before the blank, so rota's R2 is the newest the home held and the
// rescue presents it, once, under Claude Code's refresh lock: the measured
// recovery is kept, and Claude Code goes on from what rota wrote.
func TestARescueOfACopySeenAMomentAgoStillWorks(t *testing.T) {
	for _, m := range lineageModes() {
		for _, look := range lineageLooks() {
			t.Run(m.name+"/"+look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				w.cc.start()
				lineageLooks()[0].do(t, w)
				for i := 0; i < 2; i++ {
					w.clock.pass(7*time.Hour + 56*time.Minute)
					t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
				}
				lineageLooks()[1].do(t, w) // the watcher's look
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
				if got := w.cc.refresh("claude-code daemon re-check"); got != "rotated to R4" {
					t.Fatalf("Claude Code goes on from there: %s", got)
				}
			})
		}
	}
}

// The window is two watcher intervals from the last look in sync to the look
// that first finds the home blanked, to the millisecond.
func TestARescueIsMadeUpToTwoWatcherIntervalsAfterTheLastLookInSync(t *testing.T) {
	for _, c := range []struct {
		after    time.Duration
		presents bool
	}{
		{2 * WatchEvery, true},
		{2*WatchEvery + time.Millisecond, false},
	} {
		t.Run(c.after.String(), func(t *testing.T) {
			w := newLineageWorld(t, false)
			w.cc.start()
			lineageLooks()[0].do(t, w)
			w.cc.write(clearedLogin)
			w.clock.pass(c.after)
			lineageLooks()[1].do(t, w)
			if !c.presents {
				nothingPresented(t, w)
				deadForAnOlderCopy(t, w)
				return
			}
			if got := w.p.rotaPresented(); len(got) != 1 || got[0].rt != "R0" || got[0].wasSpent || w.a.Dead {
				t.Fatalf("presented R0 once and alive: %+v %v %q", got, w.a.Dead, w.a.WhyDead())
			}
		})
	}
}

// What counts is the look that first finds the home blanked. Claude Code holds
// no login to rotate once its store is blanked, so a rescue that did not get
// through then — Claude Code's refresh lock was held, or the provider did not
// answer — is made by a later look, however much later, on the same terms.
// Without that first look the same later one finds rota's copy too old.
func TestARescueIsJudgedFromTheLookThatFirstFoundTheHomeBlanked(t *testing.T) {
	t.Run("the refresh lock was held", func(t *testing.T) {
		w := newLineageWorld(t, false)
		w.cc.start()
		lineageLooks()[0].do(t, w)
		w.cc.write(clearedLogin)
		lock := filepath.Join(w.cc.home, claudeRefreshLock)
		if err := os.Mkdir(lock, 0o700); err != nil {
			t.Fatal(err)
		}
		w.clock.pass(4 * time.Minute)
		lineageLooks()[1].do(t, w)
		nothingPresented(t, w)
		if w.a.Dead || w.a.Extra[homeLostKey] != strconv.FormatInt(rota.NowMS(), 10) {
			t.Fatalf("alive, and found blanked now: %v %q", w.a.Dead, w.a.Extra[homeLostKey])
		}
		if err := os.Remove(lock); err != nil {
			t.Fatal(err)
		}
		w.clock.pass(26 * time.Minute)
		lineageLooks()[0].do(t, w)
		if got := w.p.rotaPresented(); len(got) != 1 || got[0].rt != "R0" || got[0].wasSpent || w.a.Dead {
			t.Fatalf("presented R0 once and alive: %+v %v %q", got, w.a.Dead, w.a.WhyDead())
		}
		if _, ok := w.a.Extra[homeLostKey]; ok {
			t.Fatal("the home holds the login again, so nothing is lost there")
		}
	})
	t.Run("the provider did not answer", func(t *testing.T) {
		storedRouteHere(t)
		claudeWorld(t)
		clock := useLineageClock(t)
		f := fakeAnthropic(t)
		f.set(func(f *anthropic) {
			f.refresh = func() (int, any) { return 503, map[string]string{"error": "overloaded"} }
		})
		s := openTemp(t)
		a := livingClaude(s, "u1")
		home := cleared(t, s, a)
		alive(t, home, "sessions/1.json", os.Getpid())
		clock.pass(4 * time.Minute)
		if errs := s.Rescue(context.Background(), a); len(errs) != 1 || a.Dead || f.refreshes.Load() != 1 {
			t.Fatalf("tried once, and alive: %v %v %d", errs, a.Dead, f.refreshes.Load())
		}
		clock.pass(26 * time.Minute)
		f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
		if errs := s.Rescue(context.Background(), a); len(errs) != 0 {
			t.Fatal(errs)
		}
		if a.Dead || a.Token.Refresh != "R-new" || f.refreshes.Load() != 2 {
			t.Fatalf("the later look gets through: %v %q %d", a.Dead, a.Token.Refresh, f.refreshes.Load())
		}
	})
	t.Run("no look found it then", func(t *testing.T) {
		w := newLineageWorld(t, false)
		w.cc.start()
		lineageLooks()[0].do(t, w)
		w.cc.write(clearedLogin)
		w.clock.pass(30 * time.Minute)
		lineageLooks()[0].do(t, w)
		nothingPresented(t, w)
		deadForAnOlderCopy(t, w)
	})
}

// Every look that finds the home's store holding exactly the account's refresh
// token says when, and which token by its fingerprint, and keeps nothing of the
// token itself. A record of the same token less than a minute old is left as it
// is, so such a look saves nothing. A file read beside a blanked keychain item
// is not the store Claude Code reads, and says nothing.
func TestALookInSyncRecordsWhenAndNothingOfTheToken(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			w.cc.start()
			inSyncNow := func(what, token string) {
				t.Helper()
				if got := w.a.Extra[homeSyncedKey]; got != strconv.FormatInt(rota.NowMS(), 10) || w.a.Extra[homeSyncedFPKey] != fingerprint(token) {
					t.Fatalf("%s: in sync on %s, now: %q %q", what, token, got, w.a.Extra[homeSyncedFPKey])
				}
			}
			lineageLooks()[1].do(t, w)
			inSyncNow("the first look", "R0")
			stamp := w.a.Extra[homeSyncedKey]
			w.clock.pass(time.Minute)
			before := snapshot(w.a)
			lineageLooks()[1].do(t, w)
			if w.a.Extra[homeSyncedKey] != stamp || before.differs(w.a) {
				t.Fatalf("a record a minute old stays, and nothing is to save: %q %q", w.a.Extra[homeSyncedKey], stamp)
			}
			w.clock.pass(time.Millisecond)
			lineageLooks()[1].do(t, w)
			inSyncNow("a look past the minute", "R0")
			w.clock.pass(7*time.Hour + 56*time.Minute)
			w.cc.refresh("claude-code daemon")
			lineageLooks()[1].do(t, w)
			inSyncNow("the look that takes the rotation", "R1")
			w.clock.pass(10 * time.Second)
			w.cc.refresh("claude-code daemon")
			lineageLooks()[1].do(t, w)
			inSyncNow("another token, however soon", "R2")
			for k, v := range w.a.Extra {
				if strings.Contains(v, "R0") || strings.Contains(v, "R1") || strings.Contains(v, "R2") || strings.Contains(v, "A2") {
					t.Fatalf("a token kept in %s: %q", k, v)
				}
			}
		})
	}
	t.Run("a file beside a blanked item", func(t *testing.T) {
		storedRouteHere(t)
		claudeWorld(t)
		clock := useLineageClock(t)
		k := fakeKeychain(t)
		s := openTemp(t)
		a := livingClaude(s, "u1")
		home := s.Home(a)
		a.StagedWritten()
		seenInSync(a)
		synced := a.Extra[homeSyncedKey]
		k.items[service(t, home)] = clearedLogin
		writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
		clock.pass(2 * time.Minute)
		h := looked(t, s, a)
		if !h.covered() || a.Extra[homeSyncedKey] != synced {
			t.Fatalf("the file is taken, and proves nothing: %v %q %q", h.covered(), a.Extra[homeSyncedKey], synced)
		}
	})
}

// Fix C: a file beside a blanked keychain item is the home's login only when it
// holds exactly the account's refresh token and rota saw that token in the
// home a moment ago; then it is put in front of Claude Code and nothing is
// presented. Otherwise the home is as blanked as the item, the file is never
// uncovered, and the rescue decides: rota's own copy, seen a moment ago, is
// presented and written in; one seen too long ago, one the record of that look
// does not name, or one a later login of its own lineage there shows spent, is
// not, and the account is dead.
func TestAFileBesideABlankedItemIsTheLoginOnlyWhenConfirmedCurrent(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	own := func(a *rota.Account) string {
		return storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000")
	}
	older := func(a *rota.Account) string {
		return storeLogin("A-old", "R-old", a.Token.ExpiresAt-int64(8*time.Hour/time.Millisecond), "1999999999000")
	}
	for _, c := range []struct {
		what  string
		file  func(a *rota.Account) string
		seen  time.Duration // how long before the look rota last saw its login in the home; 0 for never
		other bool          // the record of that look names another token
		then  string        // uncovered, rescued or dead
	}{
		{"the account's own, seen a minute ago", own, time.Minute, false, "uncovered"},
		{"the account's own, seen an hour ago", own, time.Hour, false, "dead"},
		{"the account's own, never seen", own, 0, false, "dead"},
		{"the account's own, seen a minute ago as another token", own, time.Minute, true, "dead"},
		{"an older one of the lineage, seen a minute ago", older, time.Minute, false, "rescued"},
		{"an older one of the lineage, seen an hour ago", older, time.Hour, false, "dead"},
		{"an older one of the lineage, seen a minute ago as another token", older, time.Minute, true, "dead"},
		{"a later one of the lineage, seen a minute ago", func(a *rota.Account) string {
			return storeLogin("A-later", "R-later", a.Token.ExpiresAt+int64(8*time.Hour/time.Millisecond), "1999999999000")
		}, time.Minute, false, "dead"},
	} {
		t.Run(c.what, func(t *testing.T) {
			clock := useLineageClock(t)
			k := fakeKeychain(t)
			f := fakeAnthropic(t)
			f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
			s := openTemp(t)
			a := livingClaude(s, "u1")
			home := s.Home(a)
			svc := service(t, home)
			a.StagedWritten()
			if c.seen != 0 {
				clock.pass(-c.seen)
				seenInSync(a)
				clock.pass(c.seen)
			}
			if c.other {
				a.Extra[homeSyncedFPKey] = fingerprint("R-before")
			}
			k.items[svc] = clearedLogin
			file := c.file(a)
			writeFile(t, filepath.Join(home, ".credentials.json"), file)
			alive(t, home, "sessions/1.json", os.Getpid())
			h := looked(t, s, a)
			if uncovers := c.then == "uncovered"; h.covered() != uncovers || h.blanked == uncovers {
				t.Fatalf("covered=%v blanked=%v", h.covered(), h.blanked)
			}
			errs := s.Rescue(context.Background(), a)
			item, itemThere := k.item(svc)
			r, _ := readLogin(t, home)
			switch c.then {
			case "uncovered":
				if len(errs) != 0 || a.Dead || itemThere || r != "R-u1" || f.refreshes.Load() != 0 {
					t.Fatalf("uncovered, nothing presented: %v %v %v %q %d", errs, a.Dead, itemThere, r, f.refreshes.Load())
				}
			case "rescued":
				if len(errs) != 0 || a.Dead || itemThere || r != "R-new" || f.refreshes.Load() != 1 {
					t.Fatalf("rota's own presented and written in: %v %v %v %q %d", errs, a.Dead, itemThere, r, f.refreshes.Load())
				}
			default:
				if len(errs) != 1 || !errors.Is(errs[0], rota.ErrReauth) || f.refreshes.Load() != 0 {
					t.Fatalf("nothing presented, and the account needs a login: %v %d", errs, f.refreshes.Load())
				}
				if !a.Dead || a.WhyDead() != olderCopyWords(a) {
					t.Fatalf("dead, and says why: %v %q", a.Dead, a.WhyDead())
				}
				if !itemThere || item != clearedLogin || readFile(filepath.Join(home, ".credentials.json")) != file {
					t.Fatal("nothing uncovered or written: the item stays in front of the file")
				}
			}
		})
	}
}

// A login of another lineage beside a blanked item is somebody's /login there,
// written to the file because the keychain refused it, and so is a login later
// than the account's when the lineage of either is not known — every account
// rota has not yet seen rotate, and an account logged in before rota kept when
// a login ends. Each is read as the new login it is: confirmed and taken, and
// put in front of Claude Code, or found to be somebody else's and left alone.
// rota presents nothing of its own beside it, although it saw its own login in
// the home a moment ago, and writes nothing over it.
func TestALoginMadeThereBesideABlankedItemIsANewLoginNotABlank(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	for _, c := range []struct {
		what        string
		ours, until string // the account's recorded end, the file's; "" for none
		whose       string
	}{
		{"another lineage", "1999999999000", "1888888888000", "u1"},
		{"the account keeps no end", "", "1888888888000", "u1"},
		{"the file carries no end", "1999999999000", "", "u1"},
		{"the account keeps no end, and the login is somebody else's", "", "1888888888000", "u2"},
	} {
		t.Run(c.what, func(t *testing.T) {
			k := fakeKeychain(t)
			f := fakeAnthropic(t)
			f.set(func(f *anthropic) {
				f.refresh = refreshesTo("A-new", "R-new")
				f.profile = profileOf("A-login", c.whose)
			})
			s := openTemp(t)
			a := livingClaude(s, "u1")
			if c.ours == "" {
				delete(a.Extra, "refresh_token_expires_at")
			}
			home := s.Home(a)
			svc := service(t, home)
			a.StagedWritten()
			seenInSync(a)
			k.items[svc] = clearedLogin
			until := c.until
			if until == "" {
				until = "null"
			}
			login := storeLogin("A-login", "R-login", later(8*time.Hour), until)
			writeFile(t, filepath.Join(home, ".credentials.json"), login)
			alive(t, home, "sessions/1.json", os.Getpid())
			s.Rescue(context.Background(), a)
			if f.refreshes.Load() != 0 || a.Dead {
				t.Fatalf("nothing of rota's presented: %d dead=%v %q", f.refreshes.Load(), a.Dead, a.WhyDead())
			}
			if r, _ := readLogin(t, home); r != "R-login" {
				t.Fatalf("nothing written over the login: %q", r)
			}
			_, itemThere := k.item(svc)
			if c.whose != "u1" {
				if a.Token.Refresh != "R-u1" || readFile(filepath.Join(home, ".credentials.json")) != login || !itemThere {
					t.Fatalf("somebody else's is not taken, and is left as it is: %q %v", a.Token.Refresh, itemThere)
				}
				return
			}
			if a.Token.Refresh != "R-login" || itemThere {
				t.Fatalf("taken, and the blanked item out of its way: %q %v", a.Token.Refresh, itemThere)
			}
		})
	}
}

// seededByRota records that the login in the home's file is the one rota wrote
// there, as seed does on a platform with a keychain.
func seededByRota(a *rota.Account) { setExtra(a, seedFPKey, a.Staged) }

// A seed on a platform with a keychain keeps the fingerprint of what it wrote,
// which is what lets a later look tell rota's own file from any other.
func TestASeedRemembersWhatItWrote(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	fakeKeychain(t)
	fakeAnthropic(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if r, _ := readLogin(t, s.Home(a)); r != "R-u1" || a.Extra[seedFPKey] != fingerprint("R-u1") {
		t.Fatalf("written, and remembered: %q %q", r, a.Extra[seedFPKey])
	}
}

// Fix C, last part: once rota has taken Claude Code's rotation into the keychain
// item, the file rota seeded beside it goes, with Claude Code alive, from
// whichever look takes it: Claude Code reads the item while there is one, and a
// blanked item would otherwise leave that file standing behind it.
func TestRotasSeedFileGoesOnceClaudeCodeMovedTheLoginIntoTheItem(t *testing.T) {
	for _, look := range lineageLooks() {
		t.Run(look.what, func(t *testing.T) {
			w := newLineageWorld(t, true)
			seededByRota(w.a)
			w.cc.start()
			w.clock.pass(7*time.Hour + 56*time.Minute)
			t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R1, rota's file R0 beside it
			look.do(t, w)
			nothingPresented(t, w)
			if w.a.Token.Refresh != "R1" {
				t.Fatalf("rota takes the rotation: %q", w.a.Token.Refresh)
			}
			if _, err := os.Stat(filepath.Join(w.cc.home, ".credentials.json")); !os.IsNotExist(err) {
				t.Fatalf("rota's own file goes: %v", err)
			}
			if v, ok := w.k.item(w.cc.svc); !ok || refreshOf(v) != "R1" || w.a.Extra[seedFPKey] != "" {
				t.Fatalf("the item stays, and nothing is left to remember: %v %q %q", ok, refreshOf(v), w.a.Extra[seedFPKey])
			}
		})
	}
}

// Only rota's own file goes, only for a newer login of the same lineage in the
// item, and only once that login is the account's.
func TestOnlyRotasOwnSeedFileGoesAndOnlyForARotation(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	const until = "1999999999000"
	expires := time.Now().Add(time.Hour).UnixMilli()
	newer := expires + int64(8*time.Hour/time.Millisecond)
	for _, c := range []struct {
		what   string
		file   string
		item   string
		record bool // rota remembers writing the file
		taken  string
		goes   bool
	}{
		{"rota's file, Claude Code's rotation", storeLogin("A-u1", "R-u1", expires, until), storeLogin("A-n", "R-n", newer, until), true, "R-n", true},
		{"a seed rota kept no record of", storeLogin("A-u1", "R-u1", expires, until), storeLogin("A-n", "R-n", newer, until), false, "R-n", false},
		{"a file rota did not write", storeLogin("A-x", "R-x", expires-1, until), storeLogin("A-n", "R-n", newer, until), true, "R-n", false},
		{"a login of another lineage in the item", storeLogin("A-u1", "R-u1", expires, until), storeLogin("A-n", "R-n", newer, "1888888888000"), true, "R-n", false},
		{"an item no newer than the file", storeLogin("A-u1", "R-u1", expires, until), storeLogin("A-n", "R-n", expires, until), true, "R-n", false},
		{"an item rota cannot place yet", storeLogin("A-u1", "R-u1", expires, until), storeLogin("A-z", "R-z", newer, "1888888888000"), true, "R-u1", false},
	} {
		t.Run(c.what, func(t *testing.T) {
			k := fakeKeychain(t)
			f := fakeAnthropic(t)
			f.set(func(f *anthropic) { f.profile = profileOf("A-n", "u1") })
			s := openTemp(t)
			a := livingClaude(s, "u1")
			a.Token.ExpiresAt = expires
			home := s.Home(a)
			a.StagedWritten()
			if c.record {
				seededByRota(a)
			}
			k.items[service(t, home)] = c.item
			writeFile(t, filepath.Join(home, ".credentials.json"), c.file)
			alive(t, home, "daemon.lock", os.Getpid())
			looked(t, s, a)
			if a.Token.Refresh != c.taken {
				t.Fatalf("the account holds %q", a.Token.Refresh)
			}
			_, err := os.Stat(filepath.Join(home, ".credentials.json"))
			if gone := os.IsNotExist(err); gone != c.goes {
				t.Fatalf("the file gone: %v", gone)
			}
		})
	}
}

// The file goes under Claude Code's refresh lock: while somebody holds it the
// look leaves the file, and any later look that finds the same removes it, as
// the look that took the rotation would have.
func TestRotasSeedFileWaitsForTheRefreshLockAndGoesLater(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	fakeAnthropic(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := s.Home(a)
	a.StagedWritten()
	seededByRota(a)
	k.items[service(t, home)] = storeLogin("A-n", "R-n", later(9*time.Hour), "1999999999000")
	file := filepath.Join(home, ".credentials.json")
	writeFile(t, file, storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
	alive(t, home, "daemon.lock", os.Getpid())
	lock := filepath.Join(home, claudeRefreshLock)
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	looked(t, s, a)
	if _, err := os.Stat(file); err != nil || a.Token.Refresh != "R-n" {
		t.Fatalf("the rotation is taken and the file stays while the lock is held: %v %q", err, a.Token.Refresh)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	looked(t, s, a)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("a later look removes it: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("and lets go of the lock")
	}
}

// A death for an older copy is on disk before the look that found it returns,
// as every death is, so no other process looks again and decides otherwise.
func TestADeathForAnOlderCopyIsSavedAtOnce(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	fakeAnthropic(t)
	s, ob := orderedStore(t)
	a := livingClaude(s, "u1")
	home := cleared(t, s, a)
	a.Extra[homeSyncedKey] = strconv.FormatInt(rota.Now().Add(-time.Hour).UnixMilli(), 10)
	alive(t, home, "sessions/1.json", os.Getpid())
	h := looked(t, s, a)
	if _, err := h.mend(context.Background()); !errors.Is(err, rota.ErrReauth) || !a.Dead {
		t.Fatalf("dead: %v %v", err, a.Dead)
	}
	if !ob.dead || !strings.Contains(allFiles(t, ob.FileBackend.Dir), "so rota did not present it") {
		t.Fatal("saved before the look returned")
	}
}

// uncover never puts a file it passes over in front of Claude Code, whoever
// asks it to.
func TestUncoverLeavesAFileItPassesOver(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := s.Home(a)
	svc := service(t, home)
	a.StagedWritten()
	k.items[svc] = clearedLogin
	file := storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000")
	writeFile(t, filepath.Join(home, ".credentials.json"), file)
	h := looked(t, s, a)
	if err := h.uncover(); err != nil {
		t.Fatal(err)
	}
	if v, ok := k.item(svc); !ok || v != clearedLogin || readFile(filepath.Join(home, ".credentials.json")) != file {
		t.Fatal("the blanked item stays in front of the file it passed over")
	}
}

// A blanked item in front of the account's own file, seen a moment ago, is
// found lost by the look that first sees it, so an uncover that has to wait —
// here for Claude Code's refresh lock — is made by a later look, however much
// later, on the same terms.
func TestAnUncoverThatWaitsIsMadeLaterOnTheSameTerms(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	clock := useLineageClock(t)
	k := fakeKeychain(t)
	f := fakeAnthropic(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := s.Home(a)
	svc := service(t, home)
	a.StagedWritten()
	seenInSync(a)
	k.items[svc] = clearedLogin
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
	alive(t, home, "sessions/1.json", os.Getpid())
	lock := filepath.Join(home, claudeRefreshLock)
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	clock.pass(4 * time.Minute)
	if errs := s.Rescue(context.Background(), a); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := k.item(svc); !ok || a.Extra[homeLostKey] != strconv.FormatInt(rota.NowMS(), 10) {
		t.Fatalf("left for later, and found lost now: %v %q", ok, a.Extra[homeLostKey])
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	clock.pass(26 * time.Minute)
	if errs := s.Rescue(context.Background(), a); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := k.item(svc); ok || a.Dead || f.refreshes.Load() != 0 {
		t.Fatalf("uncovered, nothing presented: %v %v %d", ok, a.Dead, f.refreshes.Load())
	}
	if r, _ := readLogin(t, home); r != "R-u1" {
		t.Fatalf("the file is the login: %q", r)
	}
}

// What the first look found survives the process that found it: another one,
// opening the store from disk half an hour later, makes the rescue on the same
// terms.
func TestTheLookThatFirstFoundTheHomeBlankedOutlivesItsProcess(t *testing.T) {
	w := newLineageWorld(t, false)
	w.cc.start()
	lineageLooks()[0].do(t, w)
	w.cc.write(clearedLogin)
	lock := filepath.Join(w.cc.home, claudeRefreshLock)
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	w.clock.pass(4 * time.Minute)
	lineageLooks()[1].do(t, w)
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	dir := storeDir(w.s)
	w.s.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.Close() })
	w.s, w.a = again, again.Accounts[0]
	if w.a.Extra[homeLostKey] == "" || w.a.Extra[homeSyncedKey] == "" {
		t.Fatalf("on disk: lost=%q synced=%q", w.a.Extra[homeLostKey], w.a.Extra[homeSyncedKey])
	}
	w.clock.pass(26 * time.Minute)
	lineageLooks()[0].do(t, w)
	if got := w.p.rotaPresented(); len(got) != 1 || got[0].rt != "R0" || got[0].wasSpent || w.a.Dead {
		t.Fatalf("presented R0 once and alive after a reopen: %+v %v %q", got, w.a.Dead, w.a.WhyDead())
	}
}

// The store's fingerprint is the one lib records in Staged, or nothing a look
// compares would ever match.
func TestTheFingerprintIsTheOneStagedRecords(t *testing.T) {
	a := &rota.Account{Token: rota.Token{Refresh: "R-anything"}}
	a.StagedWritten()
	if fingerprint(a.Token.Refresh) != a.Staged || fingerprint("") != "" {
		t.Fatalf("%q %q", fingerprint(a.Token.Refresh), a.Staged)
	}
}
