package store

// Staged "-" no longer freezes rota off its own lineage. In the lineage world
// (lineage_test.go), once any look recorded the home as holding nothing of the
// account's, every later rotation of the account's own login used to be passed
// over, and the next quiet renew presented the frozen, spent token (the
// audit's S2). Now such a rotation is taken whatever the look before it found,
// and a file read only because the keychain said "no such item", where the
// home's login was last found in the item, demotes nothing at all.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
// item the login was last found in — the rows that take the item away —
// demotes nothing.
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
		{"no item, and a file that cannot be read, twice, in a quiet home", []string{"keychain"}, true, false,
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
