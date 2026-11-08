package store

import (
	"bytes"
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

// A refresh of rota's own whose reply says when the login ends brings the end
// earlier, and the login rota writes into the home carries it; nothing reads
// that home's login as somebody else's afterwards. The end is what the reply
// says, counted from the moment of the refresh.
func TestARefreshedEndGoesIntoTheHome(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			const life = 20 * 24 * 3600 // seconds
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

			w.clock.pass(9 * time.Hour)
			refreshedAt := rota.Now()
			lineageLooks()[0].do(t, w)
			w.p.dump(t)
			want := strconv.FormatInt(refreshedAt.Add(life*time.Second).UnixMilli(), 10)
			if w.a.Dead || w.a.Token.Refresh != "R1" || w.a.Extra["refresh_token_expires_at"] != want {
				t.Fatalf("rota's refresh: dead=%v %s, end %q want %s", w.a.Dead, w.a.Token.Refresh, w.a.Extra["refresh_token_expires_at"], want)
			}
			l, ok := storedLogin([]byte(w.cc.store()))
			if !ok || l.Refresh != "R1" {
				t.Fatalf("the home holds the refreshed login: %+v", l)
			}
			var doc struct {
				Login struct {
					Until json.Number `json:"refreshTokenExpiresAt"`
				} `json:"claudeAiOauth"`
			}
			if err := json.Unmarshal([]byte(w.cc.store()), &doc); err != nil || doc.Login.Until.String() != want {
				t.Fatalf("the home's refreshTokenExpiresAt is %q, want %s (%v)", doc.Login.Until, want, err)
			}

			w.clock.pass(time.Minute)
			for _, look := range lineageLooks() {
				look.do(t, w)
			}
			if got := w.p.rotaPresented(); len(got) != 1 {
				t.Fatalf("rota presented once, the refresh above, and nothing since: %+v", got)
			}
			if w.a.Dead || !w.a.StagedCurrent() || w.a.Extra["refresh_token_expires_at"] != want {
				t.Fatalf("the home's login is the account's own: dead=%v staged=%q end %q", w.a.Dead, w.a.Staged,
					w.a.Extra["refresh_token_expires_at"])
			}
		})
	}
}
