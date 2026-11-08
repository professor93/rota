package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// A Claude login has a hard end. Once it has passed, no look of rota's
// presents the refresh token — not a listing, not the watcher's rescue, not
// the server's maintenance, not a launch — whatever state the home is in, and
// the account is dead with one reason, word for word.

// endedReason is the reason an account whose login ended at end is dead.
func endedReason(id int, end time.Time) string {
	return fmt.Sprintf("its login ended on %s: a Claude login lasts 30 days from sign-in; rota login %d makes a new one",
		end.UTC().Format("2006-01-02 15:04 UTC"), id)
}

// rotaPresented is everything rota presented to the provider, spent or not.
func (p *lineageProvider) rotaPresented() []lineagePresented {
	var out []lineagePresented
	for _, e := range p.entries() {
		if e.who == "rota" {
			out = append(out, e)
		}
	}
	return out
}

// endedWorld is the lineage world with a login that ended an hour after it
// was set up, eight hours before its access token ran out, both now in the
// past, and the home in one of the states that can follow:
//   - "quiet": the home holds the account's login and nothing runs there;
//   - "stranded": the daemon runs and has given up on the login;
//   - "blanked": the daemon runs and Claude Code has blanked the login.
func endedWorld(t *testing.T, keychain bool, state string) (*lineageWorld, time.Time) {
	t.Helper()
	w := newLineageWorld(t, keychain)
	end := rota.Now().Add(time.Hour)
	until := strconv.FormatInt(end.UnixMilli(), 10)
	w.a.Extra["refresh_token_expires_at"] = until
	writeFile(t, filepath.Join(w.cc.home, ".credentials.json"), storeLogin("A0", "R0", w.a.Token.ExpiresAt, until))
	if err := w.s.Save(); err != nil {
		t.Fatal(err)
	}
	w.clock.pass(9 * time.Hour)
	gaveUp := func() {
		writeFile(t, filepath.Join(w.cc.home, claudeDaemonAuth), fmt.Sprintf(`{"status":"auth_required","since":%d}`, rota.NowMS()))
	}
	switch state {
	case "quiet":
	case "stranded":
		w.cc.start()
		gaveUp()
	case "blanked":
		w.cc.start()
		w.cc.write(clearedLogin)
		gaveUp()
	default:
		t.Fatalf("no state %q", state)
	}
	return w, end
}

// Each look on its own, in each mode and each state, presents nothing once
// the login has ended. A look that would have refreshed — every one of them,
// but the watcher's rescue where the home holds the login and nothing runs,
// which never refreshes — leaves the account dead with the login's reason.
func TestNoLookPresentsALoginPastItsEnd(t *testing.T) {
	for _, m := range lineageModes() {
		for _, state := range []string{"quiet", "stranded", "blanked"} {
			for i, look := range lineageLooks() {
				t.Run(m.name+"/"+state+"/"+look.what, func(t *testing.T) {
					w, end := endedWorld(t, m.keychain, state)
					look.do(t, w)
					w.p.dump(t)
					if got := w.p.rotaPresented(); len(got) != 0 {
						t.Fatalf("rota presented a token past the login's end: %+v", got)
					}
					wantDead := !(state == "quiet" && i == 1)
					if w.a.Dead != wantDead {
						t.Fatalf("dead %v, want %v (%s)", w.a.Dead, wantDead, w.a.WhyDead())
					}
					if want := endedReason(w.a.ID, end); w.a.Dead && w.a.DeadReason != want {
						t.Fatalf("reason %q\nwant   %q", w.a.DeadReason, want)
					}
				})
			}
		}
	}
}

// All four looks one after another, as a day of use goes: still nothing is
// presented, the account is dead with the login's reason, and that is what
// the store on disk says too.
func TestEveryLookInTurnLeavesALoginPastItsEndDead(t *testing.T) {
	for _, m := range lineageModes() {
		for _, state := range []string{"quiet", "stranded", "blanked"} {
			t.Run(m.name+"/"+state, func(t *testing.T) {
				w, end := endedWorld(t, m.keychain, state)
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				w.p.dump(t)
				if got := w.p.rotaPresented(); len(got) != 0 {
					t.Fatalf("rota presented a token past the login's end: %+v", got)
				}
				want := endedReason(w.a.ID, end)
				if !w.a.Dead || w.a.DeadReason != want {
					t.Fatalf("dead %v: %q\nwant %q", w.a.Dead, w.a.DeadReason, want)
				}
				var disk Store
				if err := json.Unmarshal([]byte(readFile(filepath.Join(storeDir(w.s), "accounts.json"))), &disk); err != nil ||
					len(disk.Accounts) != 1 || !disk.Accounts[0].Dead || disk.Accounts[0].DeadReason != want {
					t.Fatalf("on disk: %v %+v", err, disk.Accounts)
				}
			})
		}
	}
}

// endReplies makes the lineage provider's token replies to rota say the login
// has life seconds left (refresh_token_expires_in), as Claude's do.
func endReplies(t *testing.T, w *lineageWorld, life int) {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := http.NewRequest(r.Method, w.p.url+r.URL.Path, bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			rw.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		var reply map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&reply)
		if resp.StatusCode == 200 && r.URL.Path == "/token" {
			reply["refresh_token_expires_in"] = life
		}
		rw.WriteHeader(resp.StatusCode)
		_ = json.NewEncoder(rw).Encode(reply)
	}))
	t.Cleanup(proxy.Close)
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = proxy.URL + "/token"
	t.Cleanup(func() { rota.ClaudeEndpoints.Token = old })
}

// homeUntil is the refreshTokenExpiresAt of the login the home holds, as
// Claude Code reads the home.
func homeUntil(t *testing.T, w *lineageWorld) string {
	t.Helper()
	var doc struct {
		Login struct {
			Until json.RawMessage `json:"refreshTokenExpiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(w.cc.store()), &doc); err != nil {
		t.Fatal(err)
	}
	return string(doc.Login.Until)
}

// heldNow reports why the account's home is in the hold, as a look reading it
// now would find it, or "" when it is not.
func heldNow(t *testing.T, w *lineageWorld) string {
	t.Helper()
	h := w.s.claudeHome(w.a, false)
	if err := h.adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h.hold
}

// A refresh of rota's own whose reply says when the login ends brings the end
// in effect earlier, counted from the moment of the refresh. The grant's own
// end is not touched: it is what the login rota writes into the home carries,
// as Claude Code's refreshTokenExpiresAt, and what the next look tells this
// login by. Nothing reads the home's login as somebody else's afterwards.
func TestARefreshedEndStaysOutOfTheHome(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			const life = 20 * 24 * 3600 // seconds
			endReplies(t, w, life)
			w.clock.pass(9 * time.Hour)
			want := rota.Now().Add(life * time.Second).UnixMilli()
			lineageLooks()[0].do(t, w)
			w.p.dump(t)
			if w.a.Dead || w.a.Token.Refresh != "R1" || w.a.LoginUntil().UnixMilli() != want ||
				w.a.Extra["refresh_token_expires_at"] != lineageUntil {
				t.Fatalf("rota's refresh: dead=%v %s, in effect %v want %d, the grant's %q", w.a.Dead, w.a.Token.Refresh,
					w.a.LoginUntil(), want, w.a.Extra["refresh_token_expires_at"])
			}
			if refreshOf(w.cc.store()) != "R1" || homeUntil(t, w) != lineageUntil {
				t.Fatalf("the home holds the refreshed login with the grant's own end: %s %s", refreshOf(w.cc.store()), homeUntil(t, w))
			}

			w.clock.pass(time.Minute)
			for _, look := range lineageLooks() {
				look.do(t, w)
			}
			if got := w.p.rotaPresented(); len(got) != 1 {
				t.Fatalf("rota presented once, the refresh above, and nothing since: %+v", got)
			}
			if w.a.Dead || !w.a.StagedCurrent() || w.a.LoginUntil().UnixMilli() != want || heldNow(t, w) != "" {
				t.Fatalf("the home's login is the account's own: dead=%v staged=%q in effect %v", w.a.Dead, w.a.Staged, w.a.LoginUntil())
			}
		})
	}
}

// rota refreshes, saves, and the write of the new login into the home does not
// land — the process stopped between the two, the home could not be read again,
// the write failed — so the home still holds the login rota wrote and has just
// spent, and the store owes the home the new one. Whatever the refresh said
// about the login's end, in either mode, and whether the provider still takes
// the spent login's access token or not, the next look puts the new login in,
// nothing is held, and no spent token is ever presented, by rota or by a
// Claude Code starting there.
func TestAHomeLeftOnASpentLoginGetsTheNewOne(t *testing.T) {
	for _, m := range lineageModes() {
		for _, c := range []struct {
			name   string
			life   int  // the reply's refresh_token_expires_in; 0: none
			noEnd  bool // the account and its home know no end: logged in by an older rota
			revoke bool // the provider no longer takes the spent login's access token
		}{
			{"the reply says no end", 0, false, false},
			{"the reply says an earlier end", 20 * 24 * 3600, false, false},
			{"the reply says an end not known before", 20 * 24 * 3600, true, false},
			{"the reply says no end, A0 revoked", 0, false, true},
			{"the reply says an earlier end, A0 revoked", 20 * 24 * 3600, false, true},
			{"the reply says an end not known before, A0 revoked", 20 * 24 * 3600, true, true},
		} {
			t.Run(m.name+"/"+c.name, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				home := w.cc.home
				if c.noEnd {
					delete(w.a.Extra, "refresh_token_expires_at")
					writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A0", "R0", w.a.Token.ExpiresAt, "null"))
					if err := w.s.Save(); err != nil {
						t.Fatal(err)
					}
				}
				if c.life != 0 {
					endReplies(t, w, c.life)
				}
				w.clock.pass(9 * time.Hour)
				refreshedAt := rota.Now()
				before, staged := readFile(filepath.Join(home, ".credentials.json")), w.a.Staged
				lineageLooks()[0].do(t, w)
				if w.a.Token.Refresh != "R1" {
					t.Fatalf("rota refreshed: %s", w.a.Token.Refresh)
				}
				if c.life != 0 && w.a.LoginUntil().UnixMilli() != refreshedAt.Add(time.Duration(c.life)*time.Second).UnixMilli() {
					t.Fatalf("the end in effect is the reply's: %v", w.a.LoginUntil())
				}
				if _, ok := w.a.Extra["refresh_token_expires_at"]; ok == c.noEnd {
					t.Fatalf("the grant's own end is as it was: %v", w.a.Extra)
				}
				// The write did not land: the home as it was, Staged on the login
				// rota spent, and the new one owed, as writeRefreshed leaves it
				// when its write fails after the save.
				writeFile(t, filepath.Join(home, ".credentials.json"), before)
				if w.k != nil {
					w.k.mu.Lock()
					delete(w.k.items, w.cc.svc)
					w.k.mu.Unlock()
				}
				w.a.Staged = staged
				w.a.Extra[seedOwedKey] = "1"
				if c.revoke {
					w.p.mu.Lock()
					w.p.issued["A0"] = false
					w.p.mu.Unlock()
				}
				if err := w.s.Save(); err != nil {
					t.Fatal(err)
				}

				w.clock.pass(time.Minute)
				lineageLooks()[1].do(t, w)
				if w.a.Dead || w.a.Token.Refresh != "R1" || refreshOf(w.cc.store()) != "R1" {
					t.Fatalf("the next look puts R1 in: dead=%v (%s) account %s, home %s", w.a.Dead, w.a.WhyDead(),
						w.a.Token.Refresh, refreshOf(w.cc.store()))
				}
				if why := heldNow(t, w); why != "" {
					t.Fatalf("held: %s", why)
				}
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				t.Logf("Claude Code starts in the home: %s", w.cc.refresh("claude-code window"))
				w.clock.pass(9 * time.Hour)
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				w.p.dump(t)
				if bad := w.p.rotaSpent(); len(bad) != 0 {
					t.Fatalf("rota presented a spent token: %+v", bad)
				}
				if w.p.revoked != "" {
					t.Fatalf("family revoked: %s", w.p.revoked)
				}
				if w.a.Dead || w.a.Token.Refresh != w.p.current {
					t.Fatalf("the account follows the lineage: dead=%v (%s) holds %s, provider at %s", w.a.Dead, w.a.WhyDead(),
						w.a.Token.Refresh, w.p.current)
				}
			})
		}
	}
}

// A login made inside the home that the profile confirms as the account's own
// takes over when the account's own refresh comes to nothing. When that refresh
// came to nothing because the login had ended, nothing was presented, so
// nothing is kept or said as a refusal the home's login covered.
func TestAnEndedLoginTakenOverInTheHomeKeepsNoRefusal(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w, _ := endedWorld(t, m.keychain, "quiet")
			words := said(w.s)
			h := w.s.claudeHome(w.a, false)
			if err := h.adopt(context.Background()); err != nil || h.hold != "" {
				t.Fatalf("in sync before: %v %q", err, h.hold)
			}
			w.p.mu.Lock()
			w.p.issued["A-new"] = true
			w.p.mu.Unlock()
			w.cc.write(storeLogin("A-new", "R-new", rota.Now().Add(8*time.Hour).UnixMilli(),
				strconv.FormatInt(rota.Now().Add(30*24*time.Hour).UnixMilli(), 10)))
			changed, err := h.refresh(context.Background())
			if !changed || err != nil || w.a.Dead || w.a.Token.Refresh != "R-new" {
				t.Fatalf("taken over by the new login: %v %v dead=%v %q holds %q", changed, err, w.a.Dead, w.a.WhyDead(), w.a.Token.Refresh)
			}
			if got := w.p.rotaPresented(); len(got) != 0 {
				t.Fatalf("nothing presented: %+v", got)
			}
			if w.a.Extra[lastRefusalKey] != "" || w.a.Extra[pendingRefusalKey] != "" || len(*words) != 0 {
				t.Fatalf("no refusal kept or said: %q %q %q", w.a.Extra[lastRefusalKey], w.a.Extra[pendingRefusalKey], *words)
			}
		})
	}
}

// A login made inside the home that rota cannot confirm yet puts the account in
// the hold when its own refresh comes to nothing. When that refresh came to
// nothing because the login had ended, nothing was presented and nothing
// refused, and the account says why it is dead in the login's own words.
func TestAnEndedLoginInTheHoldKeepsItsReason(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w, end := endedWorld(t, m.keychain, "quiet")
			h := w.s.claudeHome(w.a, false)
			if err := h.adopt(context.Background()); err != nil || h.hold != "" {
				t.Fatalf("in sync before: %v %q", err, h.hold)
			}
			// Between the look's reading and its refresh, somebody signs in inside
			// the home, with a token the profile does not know yet.
			w.cc.write(storeLogin("A-new", "R-new", rota.Now().Add(8*time.Hour).UnixMilli(),
				strconv.FormatInt(rota.Now().Add(30*24*time.Hour).UnixMilli(), 10)))
			changed, err := h.refresh(context.Background())
			if !changed || err != nil || h.hold == "" {
				t.Fatalf("held on the new login: %v %v %q", changed, err, h.hold)
			}
			if got := w.p.rotaPresented(); len(got) != 0 {
				t.Fatalf("nothing presented: %+v", got)
			}
			if want := endedReason(w.a.ID, end); !w.a.Dead || w.a.DeadReason != want {
				t.Fatalf("reason %q\nwant   %q", w.a.DeadReason, want)
			}
		})
	}
}
