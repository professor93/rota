package store

// Staged "-" no longer freezes rota off its own lineage. In the lineage world
// (lineage_test.go), once any look recorded the home as holding nothing of the
// account's, every later rotation of the account's own login used to be passed
// over, and the next quiet renew presented the frozen, spent token (the
// audit's S2). Now such a rotation is taken whatever the look before it found,
// and a file read only because the keychain said "no such item", where the
// home's login was last found in the item, demotes nothing at all.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// freezeEvery is how often Claude Code's daemon rotates the login: a little
// before the eight hours its access token lasts.
const freezeEvery = 7*time.Hour + 56*time.Minute

// freezeHideItem takes the home's keychain item away until the returned func
// puts it back: every read meanwhile answers "no such item".
func freezeHideItem(w *lineageWorld) (restore func()) {
	w.k.mu.Lock()
	defer w.k.mu.Unlock()
	stash, had := w.k.items[w.cc.svc]
	delete(w.k.items, w.cc.svc)
	return func() {
		w.k.mu.Lock()
		defer w.k.mu.Unlock()
		if had {
			w.k.items[w.cc.svc] = stash
		}
	}
}

// freezeItemGoneOnce makes the next read of the home's keychain item answer
// "no such item", as one made while Claude Code rewrites it might, and the
// read after it find the item again.
func freezeItemGoneOnce(w *lineageWorld) {
	var stash string
	reads := 0
	w.k.onFind = func(svc string) { // with the keychain's lock held
		if svc != w.cc.svc {
			return
		}
		if reads++; reads == 1 {
			stash = w.k.items[svc]
			delete(w.k.items, svc)
			return
		}
		w.k.items[svc] = stash
		w.k.onFind = nil
	}
}

// freezeStore puts doc in the place Claude Code reads its store from — the
// item, or the file when there is none — until the returned func puts back
// what was there. A nil doc takes the store away.
func freezeStore(t *testing.T, w *lineageWorld, doc *string) (restore func()) {
	t.Helper()
	path := filepath.Join(w.cc.home, ".credentials.json")
	if w.k != nil {
		w.k.mu.Lock()
		defer w.k.mu.Unlock()
		stash := w.k.items[w.cc.svc]
		if doc == nil {
			delete(w.k.items, w.cc.svc)
		} else {
			w.k.items[w.cc.svc] = *doc
		}
		return func() {
			w.k.mu.Lock()
			defer w.k.mu.Unlock()
			w.k.items[w.cc.svc] = stash
		}
	}
	stash := readFile(path)
	if doc == nil {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	} else {
		writeFile(t, path, *doc)
	}
	return func() { writeFile(t, path, stash) }
}

// freezeWorld is the lineage world in one of three modes: "keychain", the
// login in the home's keychain item once Claude Code has refreshed it; "file",
// the file as this platform reads it — on macOS a home whose login has never
// been in an item, so the file is its store; and "no keychain", the file on a
// platform that keeps no keychain, as Linux does, whatever this one is.
func freezeWorld(t *testing.T, mode string) *lineageWorld {
	t.Helper()
	if mode == "no keychain" {
		old := keychainKept
		keychainKept = false
		t.Cleanup(func() { keychainKept = old })
	}
	return newLineageWorld(t, mode == "keychain")
}

// freezeFollows is the rest of every test here: Claude Code rotates twice
// more while rota looks every way in between, and rota must hold each
// rotation; then the home goes quiet, rota's token runs out, and a listing
// presents the newest token, which the provider rotates, and the login rota
// writes into the home is one Claude Code can refresh.
func freezeFollows(t *testing.T, w *lineageWorld, from int) {
	t.Helper()
	for n := from + 1; n <= from+2; n++ {
		w.clock.pass(freezeEvery)
		t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
		for _, look := range lineageLooks() {
			look.do(t, w)
		}
		if want := "R" + itoa(n); w.a.Token.Refresh != want || !w.a.StagedCurrent() {
			t.Errorf("rota did not follow the rotation to %s: holds %s, staged-current=%v (home %s)",
				want, w.a.Token.Refresh, w.a.StagedCurrent(), refreshOf(w.cc.store()))
		}
	}
	w.cc.stop()
	w.clock.pass(9 * time.Hour)
	lineageLooks()[0].do(t, w) // rota list in a quiet home, its token run out
	w.p.dump(t)
	if bad := w.p.rotaSpent(); len(bad) != 0 {
		t.Errorf("rota presented a spent token: %+v", bad)
	}
	if w.a.Dead {
		t.Errorf("dead: %s", w.a.WhyDead())
	}
	if want := "R" + itoa(from+3); w.a.Token.Refresh != want {
		t.Errorf("the quiet renew presents the newest token: rota holds %s, want %s", w.a.Token.Refresh, want)
	}
	if st, _ := w.p.present("claude-code next start", refreshOf(w.cc.store())); st != 200 {
		t.Errorf("Claude Code's next refresh of what the home holds: http %d", st)
	}
}

// S2 of the audit, as an assertion: the home holds R1 in its keychain item
// and rota's own older write R0 in the file beside it, and one look gets "no
// such item" on every read. It reads the file, which is older than the
// account's login and not the write rota recorded, and once recorded the home
// as holding nothing of the account's. Now that read changes nothing rota
// believes, rota follows the daemon to R2 and R3, and the quiet renew presents
// R3, the newest.
func TestAKeychainMisreadDoesNotFreezeRotaOffItsOwnLineage(t *testing.T) {
	w := newLineageWorld(t, true)
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R1, file still R0
	lineageLooks()[1].do(t, w)
	if w.a.Token.Refresh != "R1" || !w.a.StagedCurrent() {
		t.Fatalf("setup: rota should follow to R1: %s staged-current=%v", w.a.Token.Refresh, w.a.StagedCurrent())
	}
	restore := freezeHideItem(w)
	lineageLooks()[1].do(t, w)
	restore()
	if w.a.Token.Refresh != "R1" || !w.a.StagedCurrent() || w.a.Dead {
		t.Fatalf("a file read in place of an absent item changes nothing rota believes: holds %s, staged %q, dead=%v",
			w.a.Token.Refresh, w.a.Staged, w.a.Dead)
	}
	freezeFollows(t, w, 1)
}

// S2 from every look: whichever look it is that finds no keychain item while
// something runs in the home, the file read in its place changes nothing rota
// believes, nothing is presented, and rota goes on following its own lineage.
func TestAKeychainMisreadOnAnyLookDoesNotFreezeRota(t *testing.T) {
	for _, look := range lineageLooks() {
		t.Run(look.what, func(t *testing.T) {
			w := newLineageWorld(t, true)
			w.cc.start()
			w.clock.pass(freezeEvery)
			t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R1, file still R0
			lineageLooks()[1].do(t, w)
			restore := freezeHideItem(w)
			look.do(t, w)
			restore()
			if w.a.Token.Refresh != "R1" || !w.a.StagedCurrent() || w.a.Dead || len(w.p.rotaPresented()) != 0 {
				w.p.dump(t)
				t.Fatalf("a file read in place of an absent item changes nothing: holds %s, staged %q, dead=%v",
					w.a.Token.Refresh, w.a.Staged, w.a.Dead)
			}
			freezeFollows(t, w, 1)
		})
	}
}

// One "no such item" is asked again before anything is decided: the daemon
// rotated to R2 while rota was not looking, the first read of the next look
// is caught while the item is being rewritten, and the second finds R2, which
// rota takes in that same look rather than reading its own older file.
func TestANoSuchItemAnswerIsReadAgainBeforeAnythingIsDecided(t *testing.T) {
	w := newLineageWorld(t, true)
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
	lineageLooks()[1].do(t, w)
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R2; rota holds R1, the file R0
	freezeItemGoneOnce(w)
	lineageLooks()[1].do(t, w)
	if w.a.Token.Refresh != "R2" || !w.a.StagedCurrent() {
		t.Fatalf("the second read finds the item, and its rotation is taken: holds %s, staged-current=%v",
			w.a.Token.Refresh, w.a.StagedCurrent())
	}
	freezeFollows(t, w, 2)
}

// Every other look that records the home as holding nothing of the account's
// leaves the next rotation of the account's own login to be taken. Each
// starts in sync on R1 with the daemon alive, puts the trigger in the home for
// one look, has Claude Code put back what it held and rotate, and then asks
// what freezeFollows asks. A trigger read from a file in place of the keychain
// item the login was last found in, while something runs in the home — the
// rows that take the item away from a live home — demotes nothing; in a quiet
// home a missing item is gone, and the file is the store.
func TestEveryDemotionLeavesTheNextRotationToBeTaken(t *testing.T) {
	old := unreadablePause
	unreadablePause = time.Millisecond
	t.Cleanup(func() { unreadablePause = old })
	doc := func(s string) *string { return &s }
	all, files := []string{"keychain", "file", "no keychain"}, []string{"file", "no keychain"}
	for _, tr := range []struct {
		name    string
		modes   []string
		quiet   bool // the look that finds it runs with nothing alive
		demotes bool // read from the home's store, not from a file in place of its item
		put     func(t *testing.T, w *lineageWorld) (restore func())
	}{
		{"no store", files, false, true, func(t *testing.T, w *lineageWorld) func() {
			return freezeStore(t, w, nil)
		}},
		{"a store with no login", all, false, true, func(t *testing.T, w *lineageWorld) func() {
			return freezeStore(t, w, doc(`{"mcpOAuth":{"srv":{"accessToken":"mcp-secret"}}}`))
		}},
		{"a blanked login the account's record does not name", files, false, true,
			func(t *testing.T, w *lineageWorld) func() {
				w.a.Staged = "" // an account whose record names no write: one from a store before it was kept
				return freezeStore(t, w, doc(clearedLogin))
			}},
		{"a login without an expiry", all, false, true, func(t *testing.T, w *lineageWorld) func() {
			return freezeStore(t, w, doc(storeLogin("A1", "R1", 0, lineageUntil)))
		}},
		{"an older login of the lineage, not the write rota recorded", all, false, true,
			func(t *testing.T, w *lineageWorld) func() {
				return freezeStore(t, w, doc(storeLogin("A0", "R0", w.clock.at.Add(-time.Hour).UnixMilli(), lineageUntil)))
			}},
		{"a store that cannot be read, twice, in a quiet home", all, true, true,
			func(t *testing.T, w *lineageWorld) func() {
				return freezeStore(t, w, doc(`{"claudeAiOauth":{"refreshToken":"R`))
			}},
		{"no item, and rota's older write in the file", []string{"keychain"}, false, false,
			func(t *testing.T, w *lineageWorld) func() { return freezeHideItem(w) }},
		{"no item and no file", []string{"keychain"}, false, false, func(t *testing.T, w *lineageWorld) func() {
			restore, file := freezeHideItem(w), filepath.Join(w.cc.home, ".credentials.json")
			stash := readFile(file)
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			return func() { restore(); writeFile(t, file, stash) }
		}},
		{"no item, and a file that cannot be read, twice, in a quiet home", []string{"keychain"}, true, true,
			func(t *testing.T, w *lineageWorld) func() {
				restore, file := freezeHideItem(w), filepath.Join(w.cc.home, ".credentials.json")
				stash := readFile(file)
				writeFile(t, file, `{"claudeAiOauth":`)
				return func() { restore(); writeFile(t, file, stash) }
			}},
	} {
		for _, mode := range tr.modes {
			t.Run(mode+"/"+tr.name, func(t *testing.T) {
				w := freezeWorld(t, mode)
				w.cc.start()
				w.clock.pass(freezeEvery)
				t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
				lineageLooks()[1].do(t, w)
				if w.a.Token.Refresh != "R1" || !w.a.StagedCurrent() {
					t.Fatalf("setup: rota should follow to R1: %s staged-current=%v", w.a.Token.Refresh, w.a.StagedCurrent())
				}
				if tr.quiet {
					w.cc.stop()
				}
				restore := tr.put(t, w)
				before := w.a.Staged
				lineageLooks()[1].do(t, w)
				restore()
				switch {
				case w.a.Token.Refresh != "R1" || w.a.Dead:
					t.Fatalf("the look takes nothing and kills nothing: holds %s, dead=%v %s",
						w.a.Token.Refresh, w.a.Dead, w.a.WhyDead())
				case tr.demotes && w.a.Staged != stagedNoneForTest:
					t.Fatalf("the look records the home as holding nothing of the account's: staged %q", w.a.Staged)
				case !tr.demotes && w.a.Staged != before:
					t.Fatalf("a file read in place of an absent item demotes nothing: staged %q, was %q", w.a.Staged, before)
				}
				if len(w.p.rotaSpent()) != 0 || len(w.p.entries()) != 1 {
					w.p.dump(t)
					t.Fatalf("nothing was presented by that look")
				}
				if tr.quiet {
					w.cc.start()
				}
				freezeFollows(t, w, 1)
			})
		}
	}
}

// Where the home's login lives is followed both ways. Claude Code's refresh
// moves it into the keychain item, and a read that then finds no item demotes
// nothing; rota's own quiet renew writes it back into the file and takes the
// item away, and from then on the file is the home's store again: a look that
// finds it behind records so, and the next quiet launch writes the account's
// login over it.
func TestTheFileIsTheStoreAgainOnceRotaWroteTheLoginBackIntoIt(t *testing.T) {
	w := freezeWorld(t, "keychain")
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R1
	lineageLooks()[1].do(t, w)
	if w.a.Extra[inItemKey] == "" {
		t.Fatalf("the login was found in the item: %v", w.a.Extra)
	}
	restore := freezeHideItem(w)
	lineageLooks()[1].do(t, w)
	restore()
	if !w.a.StagedCurrent() {
		t.Fatalf("a file read in place of the item demotes nothing: staged %q", w.a.Staged)
	}
	w.cc.stop()
	w.clock.pass(9 * time.Hour)
	lineageLooks()[0].do(t, w) // quiet, run out: rota presents R1 and writes R2 into the file
	if _, ok := w.k.item(w.cc.svc); ok || w.a.Token.Refresh != "R2" {
		t.Fatalf("rota's renew writes the file and takes the item away: holds %s", w.a.Token.Refresh)
	}
	lineageLooks()[1].do(t, w)
	if w.a.Extra[inItemKey] != "" || !w.a.StagedCurrent() {
		t.Fatalf("the login lives in the file now: %v staged-current=%v", w.a.Extra, w.a.StagedCurrent())
	}
	file := filepath.Join(w.cc.home, ".credentials.json")
	older := storeLogin("A1", "R1", w.clock.at.Add(-time.Hour).UnixMilli(), lineageUntil)
	writeFile(t, file, older)
	lineageLooks()[1].do(t, w)
	if w.a.Staged != stagedNoneForTest || w.a.Token.Refresh != "R2" {
		t.Fatalf("an older login in the file the home keeps its login in is behind: staged %q, holds %s",
			w.a.Staged, w.a.Token.Refresh)
	}
	if _, err := launchEnv(t, w.s, w.a); err != nil {
		t.Fatal(err)
	}
	if refreshOf(readFile(file)) != "R2" || !w.a.StagedCurrent() {
		t.Fatalf("the quiet launch writes the account's login over it: %s", refreshOf(readFile(file)))
	}
	if bad := w.p.rotaSpent(); len(bad) != 0 || w.a.Dead {
		w.p.dump(t)
		t.Fatalf("nothing spent was presented: %+v dead=%v", bad, w.a.Dead)
	}
}

// After a fresh `rota login` — Staged "-", with the end of the account's new
// login recorded — the login a quiet home holds is told by when it ends,
// because a login ends a fixed time after it was signed in. One ending more
// than a minute later was made with /login after rota's: it goes the way of
// any new login (the provider is asked whose it is) and is never written over
// before that; this account's is taken, and one nobody can vouch for yet is
// the hold, in which nothing is written. One ending earlier is the login from
// before `rota login`, and rota's newer login goes in over it. An account with
// no end recorded is as it was: the home's login is passed over either way.
func TestAfterAFreshRotaLoginTheHomesLoginIsToldByItsEnd(t *testing.T) {
	const (
		own    = "1999999999000" // livingClaude's: the end of the account's login
		after  = "2000086399000" // signed in a day after it, with /login
		before = "1999913599000" // signed in a day before it: the old lineage
	)
	for _, c := range []struct {
		name    string
		until   string
		noEnd   bool                    // the account has no end recorded
		profile func(string) (int, any) // nil: never to be asked
		check   func(*testing.T, *rota.Account, string, []string)
	}{
		{"a /login after, this account's", after, false, profileOf("A-login", "u1"),
			func(t *testing.T, a *rota.Account, home string, _ []string) {
				if a.Token.Refresh != "R-login" || !a.StagedCurrent() || refreshOf(home) != "R-login" {
					t.Fatalf("confirmed and taken, and left in the home: holds %s, home %s", a.Token.Refresh, refreshOf(home))
				}
			}},
		{"a /login after, nobody can vouch for it yet", after, false, func(string) (int, any) { return 401, nil },
			func(t *testing.T, a *rota.Account, home string, _ []string) {
				if a.Token.Refresh != "R-u1" || refreshOf(home) != "R-login" {
					t.Fatalf("the hold: nothing taken, nothing written: holds %s, home %s", a.Token.Refresh, refreshOf(home))
				}
			}},
		{"a /login after, somebody else's", after, false, profileOf("A-login", "u2"),
			func(t *testing.T, a *rota.Account, home string, words []string) {
				if a.Token.Refresh != "R-u1" || len(words) == 0 || !strings.Contains(words[0], "u2@x") {
					t.Fatalf("refused, saying whose it is: holds %s, said %q", a.Token.Refresh, words)
				}
				if refreshOf(home) != "R-u1" {
					t.Fatalf("and, as for any login refused, the account's own goes in once nothing runs there: %s", refreshOf(home))
				}
			}},
		{"the login from before rota login", before, false, nil,
			func(t *testing.T, a *rota.Account, home string, _ []string) {
				if a.Token.Refresh != "R-u1" || refreshOf(home) != "R-u1" || !a.StagedCurrent() {
					t.Fatalf("passed over, and rota's newer login written over it: holds %s, home %s", a.Token.Refresh, refreshOf(home))
				}
			}},
		{"no end recorded", after, true, nil,
			func(t *testing.T, a *rota.Account, home string, _ []string) {
				if a.Token.Refresh != "R-u1" || refreshOf(home) != "R-u1" {
					t.Fatalf("as before: passed over and written over: holds %s, home %s", a.Token.Refresh, refreshOf(home))
				}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			storedRouteHere(t)
			claudeWorld(t)
			f := fakeAnthropic(t)
			if c.profile != nil {
				f.set(func(f *anthropic) { f.profile = c.profile })
			}
			s := openTemp(t)
			words := said(s)
			a := livingClaude(s, "u1") // Staged "-", as `rota login` leaves it
			if a.Extra["refresh_token_expires_at"] != own {
				t.Fatalf("setup: %v", a.Extra)
			}
			if c.noEnd {
				delete(a.Extra, "refresh_token_expires_at")
			}
			file := filepath.Join(s.Home(a), ".credentials.json")
			access := "A-login"
			if c.until == before {
				access = "A-old"
			}
			writeFile(t, file, storeLogin(access, "R-"+strings.TrimPrefix(access, "A-"), later(2*time.Hour), c.until))
			if _, err := launchEnv(t, s, a); err != nil {
				t.Fatal(err)
			}
			s.Refresh(context.Background(), false, a)
			if (f.profiles.Load() != 0) != (c.profile != nil) || f.refreshes.Load() != 0 {
				t.Fatalf("the provider is asked whose it is exactly when it ends later: %d profile reads, %d refreshes",
					f.profiles.Load(), f.refreshes.Load())
			}
			c.check(t, a, readFile(file), *words)
		})
	}
}

// A login in the home that ends in the same minute as the account's but that
// Claude Code records as another account's (tokenAccount) is not taken as a
// rotation without a word: the provider is asked whose it is, as for any new
// login, and somebody else's is refused, naming them.
func TestALoginRecordedAsAnotherAccountsGoesTheNewLoginWay(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.profile = profileOf("A-x", "u2") })
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil || !a.StagedCurrent() {
		t.Fatalf("setup: the account's login in its home: %v staged-current=%v", err, a.StagedCurrent())
	}
	home := s.Home(a)
	alive(t, home, "daemon.lock", os.Getpid())
	writeFile(t, filepath.Join(home, ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"A-x","refreshToken":"R-x","expiresAt":`+itoa(int(later(2*time.Hour)))+
			`,"refreshTokenExpiresAt":1999999998100,"scopes":["user:inference"],`+
			`"tokenAccount":{"uuid":"u2","emailAddress":"u2@x"}}}`)
	s.Refresh(context.Background(), false, a)
	if a.Token.Refresh != "R-u1" || f.profiles.Load() == 0 || f.refreshes.Load() != 0 {
		t.Fatalf("not taken as a rotation; the provider was asked: holds %s, %d profile reads, %d refreshes",
			a.Token.Refresh, f.profiles.Load(), f.refreshes.Load())
	}
	if len(*words) == 0 || !strings.Contains((*words)[0], "u2@x") {
		t.Fatalf("refused, naming whose it is: %q", *words)
	}
}

// The keychain item really goes — deleted by hand or by a tool, a keychain
// reset — while the home is quiet, with rota's older R0 file still beside it.
// Nothing can be rewriting an item while nothing runs, so the look takes the
// file for the home's store: R0 is behind, and the quiet launch writes the
// account's R1 over it, so Claude Code starts on a token the provider still
// honours. (The review's TestReviewRealItemLossInAQuietHome.)
func TestAnItemGoneFromAQuietHomeIsGoneAndTheFileIsTheStore(t *testing.T) {
	w := freezeWorld(t, "keychain")
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R1, file R0
	lineageLooks()[1].do(t, w)
	if w.a.Token.Refresh != "R1" || !w.a.StagedCurrent() || w.a.Extra[inItemKey] == "" {
		t.Fatalf("setup: holds %s, staged-current=%v, %v", w.a.Token.Refresh, w.a.StagedCurrent(), w.a.Extra)
	}
	w.cc.stop()
	_ = freezeHideItem(w) // never put back: the item is gone for real
	w.clock.pass(time.Hour)
	for _, look := range lineageLooks() {
		look.do(t, w)
	}
	home := refreshOf(w.cc.store())
	if st, _ := w.p.present("claude-code next start", home); home != "R1" || st != 200 {
		t.Fatalf("the quiet launch writes the account's login over the spent file: home %s, Claude Code's start http %d", home, st)
	}
	if bad := w.p.rotaSpent(); len(bad) != 0 || w.a.Dead {
		w.p.dump(t)
		t.Fatalf("nothing spent presented: %+v dead=%v", bad, w.a.Dead)
	}
}

// As above with no file left either: the quiet launch writes the account's
// login into the empty home, so Claude Code starts signed in. (The review's
// TestReviewRealItemLossNoFileInAQuietHome.)
func TestAnItemGoneFromAQuietHomeWithNoFileIsWrittenAgain(t *testing.T) {
	w := freezeWorld(t, "keychain")
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
	lineageLooks()[1].do(t, w)
	if err := os.Remove(filepath.Join(w.cc.home, ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	w.cc.stop()
	_ = freezeHideItem(w)
	w.clock.pass(time.Hour)
	lineageLooks()[3].do(t, w) // a quiet launch
	if home := refreshOf(w.cc.store()); home != "R1" || !w.a.StagedCurrent() {
		t.Fatalf("the quiet launch writes the account's login in: home %q, staged-current=%v", home, w.a.StagedCurrent())
	}
}

// A file read in place of the item while something runs records nothing about
// the home: a home rota had recorded as not holding the account's login is not
// promoted to holding it because the file beside the missing item happens to
// hold the account's token, and the record of where the login lives stays.
func TestAFileReadInPlaceOfTheItemRecordsNothing(t *testing.T) {
	w := freezeWorld(t, "keychain")
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R1
	lineageLooks()[1].do(t, w)
	noLogin := `{"mcpOAuth":{"srv":{"accessToken":"mcp-secret"}}}`
	putBack := freezeStore(t, w, &noLogin)
	lineageLooks()[1].do(t, w)
	if w.a.Staged != stagedNoneForTest {
		t.Fatalf("setup: an item with no login demotes: %q", w.a.Staged)
	}
	file := filepath.Join(w.cc.home, ".credentials.json")
	writeFile(t, file, storeLogin(w.a.Token.Access, "R1", w.a.Token.ExpiresAt, lineageUntil))
	unhide := freezeHideItem(w)
	lineageLooks()[1].do(t, w)
	if w.a.Staged != stagedNoneForTest || w.a.Extra[inItemKey] == "" {
		t.Fatalf("nothing recorded from the file: staged %q, %v", w.a.Staged, w.a.Extra)
	}
	unhide()
	putBack()
	lineageLooks()[1].do(t, w)
	if !w.a.StagedCurrent() {
		t.Fatalf("the item itself holding the account's login is in sync: staged %q", w.a.Staged)
	}
}

// Nor is a login found in that file settled: the provider is not asked whose
// it is, and nothing of it is taken or refused.
func TestALoginInAFileReadInPlaceOfTheItemIsNotSettled(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.profile = profileOf("A-x", "u1") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil || !a.StagedCurrent() {
		t.Fatalf("setup: %v", err)
	}
	home := s.Home(a)
	svc := service(t, home)
	k.mu.Lock()
	k.items[svc] = storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000")
	k.mu.Unlock()
	alive(t, home, "daemon.lock", os.Getpid())
	s.Refresh(context.Background(), false, a)
	if a.Extra[inItemKey] == "" {
		t.Fatalf("setup: the login was found in the item: %v", a.Extra)
	}
	k.mu.Lock()
	delete(k.items, svc)
	k.mu.Unlock()
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A-x", "R-x", later(2*time.Hour), "2000086399000"))
	s.Refresh(context.Background(), false, a)
	if f.profiles.Load() != 0 || a.Token.Refresh != "R-u1" || !a.StagedCurrent() {
		t.Fatalf("not settled from that file: %d profile reads, holds %s, staged-current=%v",
			f.profiles.Load(), a.Token.Refresh, a.StagedCurrent())
	}
}

// A login the provider said is somebody else's is not asked about again: five
// listings of a live home holding another account's /login (ending a day after
// the account's own) ask once and say so once. (The review's
// TestReviewForeignLoginCheckedEveryLook.)
func TestARefusedLoginIsNotAskedAboutAgain(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.profile = profileOf("A-login", "u2") })
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1") // Staged "-"
	home := s.Home(a)
	alive(t, home, "daemon.lock", os.Getpid())
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A-login", "R-login", later(2*time.Hour), "2000086399000"))
	for i := 0; i < 5; i++ {
		s.Refresh(context.Background(), false, a)
	}
	if f.profiles.Load() != 1 || len(*words) != 1 || a.Token.Refresh != "R-u1" {
		t.Fatalf("asked once, said once: %d profile reads, %q, holds %s", f.profiles.Load(), *words, a.Token.Refresh)
	}
}

// Refused in a live home, another account's /login stays refused overnight:
// once the home is quiet and that login's access token has lapsed, the launch
// does not put the account's run on it (the hold) but writes the account's own
// login over it, as rota said it would, with no second question. (The review's
// TestReviewRefusedForeignLoginLapsesOvernight.)
func TestARefusedLoginLapsingOvernightIsStillWrittenOver(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.profile = profileOf("A-login", "u2") })
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil || !a.StagedCurrent() {
		t.Fatalf("setup: %v", err)
	}
	home := s.Home(a)
	file := filepath.Join(home, ".credentials.json")
	alive(t, home, "daemon.lock", os.Getpid())
	writeFile(t, file, storeLogin("A-login", "R-login", later(2*time.Hour), "2000086399000"))
	s.Refresh(context.Background(), false, a)
	if len(*words) != 1 || !strings.Contains((*words)[0], "u2@x") {
		t.Fatalf("refused, naming u2: %q", *words)
	}
	_ = os.Remove(filepath.Join(home, "daemon.lock"))
	f.set(func(f *anthropic) { f.profile = func(string) (int, any) { return 401, nil } })
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if got := refreshOf(readFile(file)); got != "R-u1" || f.profiles.Load() != 1 {
		t.Fatalf("the account's own login goes in, with one question in all: home %s, %d profile reads", got, f.profiles.Load())
	}
	if a.Extra["refused_grant"] != "" {
		t.Fatalf("and once the account's own login is in, the refusal is forgotten: %v", a.Extra)
	}
}

// Where the home's login lives belongs to the home, and so does a login in it
// the provider refused: moving the account to another configuration directory
// forgets both, whatever route it was on.
func TestMovingTheAccountForgetsWhereItsLoginLived(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	a.ConfigDir = t.TempDir() // a directory the person chose: rota keeps no login there
	a.Extra[inItemKey], a.Extra[refusedGrantKey] = "1", "2000086399000 u2"
	if err := s.MoveHome(context.Background(), a, ""); err != nil {
		t.Fatal(err)
	}
	if a.ConfigDir != "" || a.Extra[inItemKey] != "" || a.Extra[refusedGrantKey] != "" {
		t.Fatalf("forgotten with the move, and the refusal with it: %q %v", a.ConfigDir, a.Extra)
	}
}

// "Wrong account, /login again": another account's /login in the live home is
// refused, and within the minute the person signs in again as this account.
// That login ends within a minute of the refused one, but Claude Code records
// it as this account's, so it is not taken for the refused grant: the provider
// is asked, it is taken, and nothing writes over it once the home is quiet.
// (The review's TestReviewOwnLoginInTheMinuteAfterARefusal.)
func TestThisAccountsLoginInTheMinuteAfterARefusalIsTaken(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.profile = func(auth string) (int, any) {
			switch auth {
			case "Bearer A-u2login":
				return 200, map[string]any{"account": map[string]string{"uuid": "u2", "email": "u2@x"}}
			case "Bearer A-u1login":
				return 200, map[string]any{"account": map[string]string{"uuid": "u1", "email": "u1@x"}}
			}
			return 401, nil
		}
	})
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil || !a.StagedCurrent() {
		t.Fatalf("setup: %v", err)
	}
	home := s.Home(a)
	file := filepath.Join(home, ".credentials.json")
	alive(t, home, "daemon.lock", os.Getpid())
	put := func(access, refresh, until, uuid string) {
		writeFile(t, file, `{"claudeAiOauth":{"accessToken":"`+access+`","refreshToken":"`+refresh+`","expiresAt":`+
			itoa(int(later(8*time.Hour)))+`,"refreshTokenExpiresAt":`+until+`,"scopes":["user:inference"],`+
			`"tokenAccount":{"uuid":"`+uuid+`","emailAddress":"`+uuid+`@x"}}}`)
	}
	put("A-u2login", "R-u2login", "2000086399000", "u2")
	s.Refresh(context.Background(), false, a)
	if a.Extra[refusedGrantKey] != "2000086399000 u2" || f.profiles.Load() != 1 {
		t.Fatalf("refused, and remembered with whose it is: %v, %d profile reads", a.Extra, f.profiles.Load())
	}
	put("A-u1login", "R-u1login", "2000086429000", "u1") // thirty seconds later, as this account
	s.Refresh(context.Background(), false, a)
	if a.Token.Refresh != "R-u1login" || !a.StagedCurrent() || f.profiles.Load() != 2 {
		t.Fatalf("asked about and taken: holds %s, staged-current=%v, %d profile reads",
			a.Token.Refresh, a.StagedCurrent(), f.profiles.Load())
	}
	_ = os.Remove(filepath.Join(home, "daemon.lock"))
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if got := refreshOf(readFile(file)); got != "R-u1login" {
		t.Fatalf("and never written over: home %s", got)
	}
}

// Claude Code falls back to the file for good — the keychain refuses its
// writes — and clears its item itself, so rota never takes an item away there.
// Once a quiet look finds no item, the account forgets the item: from then on a
// login in the file is read as the home's store, and another account's /login
// in it while Claude Code runs is asked about and refused at once.
// (The review's TestReviewPlaintextHomeKeepsTheKey.)
func TestAHomeWhoseItemClaudeCodeClearedIsReadFromItsFile(t *testing.T) {
	w := freezeWorld(t, "keychain")
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R1
	lineageLooks()[1].do(t, w)
	file := filepath.Join(w.cc.home, ".credentials.json")
	writeFile(t, file, w.cc.store()) // the login goes to the file
	_ = freezeHideItem(w)            // Claude Code clears its item...
	w.cc.k = nil                     // ...and keeps the login in the file from now on
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // file R2
	lineageLooks()[1].do(t, w)
	if w.a.Token.Refresh != "R2" || !w.a.StagedCurrent() {
		t.Fatalf("a newer login of the account's own in the file is taken: holds %s", w.a.Token.Refresh)
	}
	w.cc.stop()
	w.clock.pass(9 * time.Hour)
	lineageLooks()[0].do(t, w) // quiet: rota renews and writes its refreshed login into the file
	if w.a.Extra[inItemKey] != "" || refreshOf(readFile(file)) != w.a.Token.Refresh {
		t.Fatalf("a quiet look with no item forgets it: %v, file %s", w.a.Extra, refreshOf(readFile(file)))
	}
	w.cc.start()
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.profile = profileOf("A-x", "u2") })
	writeFile(t, file, storeLogin("A-x", "R-x", w.clock.at.Add(8*time.Hour).UnixMilli(), "2000086399000"))
	words := said(w.s)
	w.s.Refresh(context.Background(), false, w.a)
	if f.profiles.Load() != 1 || len(*words) != 1 || !strings.Contains((*words)[0], "u2@x") || w.a.Staged != stagedNoneForTest {
		t.Fatalf("another account's /login is asked about and refused: %d profile reads, said %q, staged %q",
			f.profiles.Load(), *words, w.a.Staged)
	}
}

// A newer login of the account's own taken from a file read in place of the
// item is recorded as what the home holds: the record follows the token rather
// than going back to the one it replaced.
func TestANewerLoginTakenFromTheFileInPlaceOfTheItemIsRecordedAsHeld(t *testing.T) {
	w := freezeWorld(t, "keychain")
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R1
	lineageLooks()[1].do(t, w)
	writeFile(t, filepath.Join(w.cc.home, ".credentials.json"), w.cc.store())
	_ = freezeHideItem(w)
	w.cc.k = nil // Claude Code keeps the login in the file from here on
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // file R2, while alive
	lineageLooks()[1].do(t, w)
	if w.a.Token.Refresh != "R2" || !w.a.StagedCurrent() {
		t.Fatalf("taken, and recorded as held: holds %s, staged %q", w.a.Token.Refresh, w.a.Staged)
	}
	freezeFollows(t, w, 2)
}

// When rota itself takes an item away, the home's login lives in the file: a
// quiet launch's tidy, removing an item older than the file beside it, says so
// at once, as does removing the account's login from its home.
func TestRotaTakingTheItemAwayForgetsIt(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil || !a.StagedCurrent() {
		t.Fatalf("setup: %v", err)
	}
	home := s.Home(a)
	svc := service(t, home)
	k.mu.Lock()
	k.items[svc] = storeLogin("A-old", "R-old", later(-time.Hour), "1999999999000")
	k.mu.Unlock()
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.item(svc); ok || a.Extra[inItemKey] != "" {
		t.Fatalf("tidy took the older item away, and forgot it: item there %v, %v", ok, a.Extra)
	}

	k.mu.Lock()
	k.items[svc] = storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000")
	k.mu.Unlock()
	a.Extra[inItemKey] = "1"
	if err := s.claudeHome(a, false).dropOwnLogin(); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.item(svc); ok || a.Extra[inItemKey] != "" {
		t.Fatalf("removing the account's login forgets the item: item there %v, %v", ok, a.Extra)
	}
}

// A look that takes a blanked item out of the way of the login in the file
// (uncover) — here the watcher's, which reads no usage and saves only what
// changed — forgets the item, and that is saved, not left for some later save.
func TestUncoveringALoginForgetsTheItemOnDisk(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil || !a.StagedCurrent() {
		t.Fatalf("setup: %v", err)
	}
	home := s.Home(a)
	svc := service(t, home)
	alive(t, home, "daemon.lock", os.Getpid())
	s.Rescue(context.Background(), a) // settles what the first look after the write takes from the store
	k.mu.Lock()
	k.items[svc] = clearedLogin
	k.mu.Unlock()
	a.Extra[inItemKey] = "1"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if errs := s.Rescue(context.Background(), a); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := k.item(svc); ok || a.Extra[inItemKey] != "" {
		t.Fatalf("setup: the blanked item is taken out of the way and forgotten: item there %v, %v", ok, a.Extra)
	}
	s.Close()
	s2, err := Open(storeDir(s))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if b := s2.Find(a.ID); b.Extra[inItemKey] != "" {
		t.Fatalf("and that is on disk: %v", b.Extra)
	}
}

// A dead account whose home holds another account's /login asks once, says so
// once, and passes that login over after, instead of asking at every look.
func TestADeadAccountsRefusedLoginIsNotAskedAboutAgain(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.profile = profileOf("A-login", "u2") })
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	a.Dead, a.DeadReason = true, "invalid_grant"
	home := s.Home(a)
	alive(t, home, "daemon.lock", os.Getpid())
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A-login", "R-login", later(2*time.Hour), "2000086399000"))
	for i := 0; i < 3; i++ {
		s.Refresh(context.Background(), false, a)
	}
	if f.profiles.Load() != 1 || len(*words) != 1 || !a.Dead || a.Token.Refresh != "R-u1" {
		t.Fatalf("asked once, said once, still dead: %d profile reads, %q, dead=%v", f.profiles.Load(), *words, a.Dead)
	}
}

// Whether anything runs is asked before the keychain is read as well as after:
// a Claude Code that is running when the read finds no item — rewriting it, say
// — and exits before the read is done still counts, so the read records
// nothing and the item is not forgotten.
func TestAClaudeCodeThatExitsDuringTheReadStillCounts(t *testing.T) {
	w := freezeWorld(t, "keychain")
	w.cc.start()
	w.clock.pass(freezeEvery)
	t.Logf("daemon: %s", w.cc.refresh("claude-code daemon")) // item R1, file R0
	lineageLooks()[1].do(t, w)
	if !w.a.StagedCurrent() || w.a.Extra[inItemKey] == "" {
		t.Fatalf("setup: staged-current=%v %v", w.a.StagedCurrent(), w.a.Extra)
	}
	var stash string
	w.k.onFind = func(svc string) { // with the keychain's lock held
		if v, ok := w.k.items[svc]; ok {
			stash = v
			delete(w.k.items, svc)
		}
		w.cc.stop() // Claude Code exits while the item is missing
	}
	lineageLooks()[1].do(t, w)
	w.k.mu.Lock()
	w.k.onFind = nil
	w.k.items[w.cc.svc] = stash
	w.k.mu.Unlock()
	if !w.a.StagedCurrent() || w.a.Extra[inItemKey] == "" {
		t.Fatalf("the read records nothing, and the item is not forgotten: staged %q, %v", w.a.Staged, w.a.Extra)
	}
}
