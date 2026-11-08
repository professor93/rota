package rota

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// A Claude login has a hard end, the moment its refresh token stops working
// for anybody. These tests pin how rota learns it — at sign-in and from every
// refresh — and that once it has passed, rota presents the refresh token to no
// one and the account is dead, saying why.

// lifeNow pins this package's clock to a whole millisecond, which is what an
// end kept in milliseconds can be compared with exactly, and returns it.
func lifeNow(t *testing.T) time.Time {
	t.Helper()
	at := time.UnixMilli(time.Now().UnixMilli())
	old := Now
	Now = func() time.Time { return at }
	t.Cleanup(func() { Now = old })
	return at
}

// endingClaude is a claude account whose access token has run out and whose
// login ends at end (unix ms; 0 for an end nobody has recorded).
func endingClaude(end int64) *Account {
	a := livingClaude()
	a.ID = 7
	a.Token.ExpiresAt = nowMS() - 60_000
	delete(a.Extra, claudeRefreshUntil)
	if end != 0 {
		a.Extra[claudeRefreshUntil] = strconv.FormatInt(end, 10)
	}
	return a
}

// refresh_token_expires_in is read as Claude Code reads it: seconds, as a
// number or as a string holding one. Any other shape is no answer, and costs
// only itself — the reply around it still decodes.
func TestALoginsEndIsReadFromTheTokenReply(t *testing.T) {
	now := lifeNow(t).UnixMilli()
	for _, c := range []struct {
		field string
		end   int64 // 0: no answer
	}{
		{`"refresh_token_expires_in":2592000`, now + 2_592_000_000},
		{`"refresh_token_expires_in":"2592000"`, now + 2_592_000_000},
		{`"refresh_token_expires_in":" 86400 "`, now + 86_400_000},
		{`"refresh_token_expires_in":1.5`, now + 1500},
		{`"refresh_token_expires_in":2.592e6`, now + 2_592_000_000},
		{`"other":1`, 0},
		{`"refresh_token_expires_in":null`, 0},
		{`"refresh_token_expires_in":"soon"`, 0},
		{`"refresh_token_expires_in":""`, 0},
		{`"refresh_token_expires_in":true`, 0},
		{`"refresh_token_expires_in":{"seconds":5}`, 0},
		{`"refresh_token_expires_in":[5]`, 0},
		{`"refresh_token_expires_in":0`, 0},
		{`"refresh_token_expires_in":-60`, 0},
		{`"refresh_token_expires_in":"-60"`, 0},
		{`"refresh_token_expires_in":"0x10"`, 0},
		{`"refresh_token_expires_in":"Infinity"`, 0},
		{`"refresh_token_expires_in":"NaN"`, 0},
		{`"refresh_token_expires_in":1e300`, 0},
		{`"refresh_token_expires_in":"12 days"`, 0},
	} {
		var r claudeTokenResp
		if err := decodeLenient([]byte(`{"access_token":"A","refresh_token":"R","expires_in":28800,`+c.field+`}`), &r); err != nil {
			t.Fatalf("%s: the reply must still decode: %v", c.field, err)
		}
		if r.AccessToken != "A" || r.RefreshToken != "R" || r.ExpiresIn != 28800 {
			t.Fatalf("%s: the rest of the reply is read as ever: %+v", c.field, r.oauthTokenResp)
		}
		end, ok := r.refreshEnd()
		if ok != (c.end != 0) || end != c.end {
			t.Errorf("%s: got %d %v, want %d", c.field, end, ok, c.end)
		}
	}
}

// rota login records when the login ends: now plus what the exchange says, or
// thirty days from now when it says nothing usable, as Claude Code reckons its
// own sign-in. It replaces the end the account had, which was the last login's,
// and forgets the last grant's end in effect.
// The long-lived login's refresh token is dropped, and when it would have
// ended goes with it: the account's own login keeps its end.
func TestRotaLoginRecordsWhenTheLoginEnds(t *testing.T) {
	now := lifeNow(t).UnixMilli()
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	setURL(t, &ClaudeEndpoints.Profile, f.URL+"/profile")
	f.reply["/profile"] = func(*http.Request, map[string]any) (int, any) { return 500, nil }
	p, _ := Lookup("claude")
	_, state, _ := p.Begin(context.Background())
	for _, c := range []struct {
		life any // nil: the reply carries none
		end  int64
	}{
		{2_000_000, now + 2_000_000_000},
		{"2000000", now + 2_000_000_000},
		{nil, now + 30*24*3600*1000},
		{"a month", now + 30*24*3600*1000},
		{-5, now + 30*24*3600*1000},
	} {
		f.reply["/token"] = func(*http.Request, map[string]any) (int, any) {
			body := map[string]any{"access_token": "A", "refresh_token": "R", "expires_in": 28800,
				"account": map[string]string{"uuid": "u-5"}}
			if c.life != nil {
				body["refresh_token_expires_in"] = c.life
			}
			return 200, body
		}
		tok, err := p.Complete(context.Background(), "CODE#ST", state)
		if err != nil {
			t.Fatal(err)
		}
		a := livingClaude()
		a.Extra[claudeLoginEnds] = strconv.FormatInt(now+60_000, 10) // the last grant's, nearly over
		a.Apply(tok)
		if got := a.Extra[claudeRefreshUntil]; got != strconv.FormatInt(c.end, 10) || a.LoginUntil().UnixMilli() != c.end {
			t.Errorf("life %#v: the account's end is %q, in effect %v, want %d", c.life, got, a.LoginUntil(), c.end)
		}
		if _, ok := a.Extra[claudeLoginEnds]; ok {
			t.Errorf("life %#v: the last grant's end in effect is forgotten: %v", c.life, a.Extra)
		}
	}

	f.reply["/token"] = func(*http.Request, map[string]any) (int, any) {
		return 200, map[string]any{"access_token": "LONG", "refresh_token": "NOPE", "refresh_token_expires_in": 60,
			"account": map[string]string{"uuid": "u-5"}}
	}
	tok, err := p.(LongLived).CompleteLong(context.Background(), "CODE#ST", state)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tok.Extra[claudeRefreshUntil]; ok {
		t.Fatalf("the long login's refresh end is not kept: %v", tok.Extra)
	}
	a := livingClaude()
	a.ApplyLong(tok)
	if a.Extra[claudeRefreshUntil] != "1999999999000" || a.LoginEnded() {
		t.Fatalf("the account's own login keeps its end: %v", a.Extra)
	}
}

// Every refresh rota makes keeps the earlier of the end in effect and the end
// the reply says, so the end in effect never moves later; a reply that says
// nothing leaves it as it was, and an account that had none learns it. A
// difference smaller than claudeEndNoise is the reply's whole seconds against
// this machine's milliseconds, and moves nothing. The grant's own end, which
// is its identity, is never touched: an account that had none still has none.
func TestARefreshNeverMovesTheEndLater(t *testing.T) {
	now := lifeNow(t).UnixMilli()
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	const day = 24 * 3600 // seconds
	had := now + 10*day*1000
	for _, c := range []struct {
		name string
		had  int64 // 0: none recorded
		life any   // nil: the reply carries none
		want int64
	}{
		{"an earlier end is taken", had, 3 * day, now + 3*day*1000},
		{"an earlier end as a string is taken", had, strconv.Itoa(3 * day), now + 3*day*1000},
		{"a later end is not", had, 20 * day, had},
		{"no end leaves it", had, nil, had},
		{"garbage leaves it", had, "later", had},
		{"a few seconds earlier is noise", had, 10*day - 5, had},
		{"an end not known before is learned", 0, 3 * day, now + 3*day*1000},
		{"nothing is invented", 0, nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f.reply["/token"] = func(_ *http.Request, b map[string]any) (int, any) {
				if b["refresh_token"] != "R1" {
					return 400, map[string]string{"error": "invalid_grant"}
				}
				body := map[string]any{"access_token": "A2", "refresh_token": "R2", "expires_in": 28800}
				if c.life != nil {
					body["refresh_token_expires_in"] = c.life
				}
				return 200, body
			}
			a := endingClaude(c.had)
			if changed, err := Refresh(context.Background(), a); err != nil || !changed || a.Token.Refresh != "R2" {
				t.Fatalf("refresh: %v %v %+v", changed, err, a.Token)
			}
			if got := a.LoginUntil(); (c.want == 0) != got.IsZero() || (c.want != 0 && got.UnixMilli() != c.want) {
				t.Fatalf("end %v, want %d", got, c.want)
			}
			own, had := a.Extra[claudeRefreshUntil]
			if had != (c.had != 0) || (c.had != 0 && own != strconv.FormatInt(c.had, 10)) {
				t.Fatalf("the grant's own end is never a refresh's to change: %v", a.Extra)
			}
		})
	}
}

// Once the login has reached its end, nothing is sent: not through
// Refresh, whatever asked for it, and not through the provider's own Refresh
// either. The account is dead, with a reason that says when the login ended
// and what to do. An end a moment away still refreshes, and so does a login
// whose end nobody recorded, as it always has.
func TestNothingIsPresentedOnceTheLoginHasEnded(t *testing.T) {
	now := lifeNow(t)
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	f.reply["/token"] = func(*http.Request, map[string]any) (int, any) {
		return 200, map[string]any{"access_token": "A2", "refresh_token": "R2", "expires_in": 28800}
	}
	end := time.Date(2026, 11, 6, 23, 57, 27, 0, time.UTC)
	const want = "its login ended on 2026-11-06 23:57 UTC: a Claude login lasts 30 days from sign-in; rota login 7 makes a new one"

	for _, at := range []time.Time{end, end.Add(time.Millisecond), end.Add(40 * 24 * time.Hour)} {
		Now = func() time.Time { return at }
		a := endingClaude(end.UnixMilli())
		changed, err := Refresh(context.Background(), a)
		if !changed || !errors.Is(err, ErrReauth) || !a.Dead || a.DeadReason != want || a.WhyDead() != want {
			t.Fatalf("at %v: %v %v dead=%v %q", at, changed, err, a.Dead, a.DeadReason)
		}
		if a.Token.Refresh != "R1" {
			t.Fatalf("at %v: the tokens are as they were: %+v", at, a.Token)
		}
		r, _ := Lookup("claude")
		if _, err := r.(Refresher).Refresh(context.Background(), endingClaude(end.UnixMilli())); !errors.Is(err, ErrDeadToken) || err.Error() != want {
			t.Fatalf("at %v: the provider's own refresh refuses too: %v", at, err)
		}
	}
	if n := f.count("/token"); n != 0 {
		t.Fatalf("nothing is sent once the login has ended, sent %d", n)
	}

	Now = func() time.Time { return end.Add(-time.Millisecond) }
	a := endingClaude(end.UnixMilli())
	if changed, err := Refresh(context.Background(), a); err != nil || !changed || a.Dead || a.Token.Refresh != "R2" {
		t.Fatalf("a moment before the end it refreshes: %v %v %+v", changed, err, a)
	}
	Now = func() time.Time { return now }
	a = endingClaude(0)
	if changed, err := Refresh(context.Background(), a); err != nil || !changed || a.Dead || a.Token.Refresh != "R2" {
		t.Fatalf("an end nobody recorded refreshes as ever: %v %v %+v", changed, err, a)
	}
	if n := f.count("/token"); n != 2 {
		t.Fatalf("two refreshes sent, got %d", n)
	}

	// The end decides what a refresh may present, and an access token still
	// good asks for no refresh: the account runs on it, and dies with this
	// reason the first time a refresh is due.
	Now = func() time.Time { return end.Add(time.Hour) }
	a = endingClaude(end.UnixMilli())
	a.Token.ExpiresAt = end.Add(5 * time.Hour).UnixMilli()
	if changed, err := Refresh(context.Background(), a); changed || err != nil || a.Dead || !a.LoginEnded() {
		t.Fatalf("an access token still good: %v %v %+v", changed, err, a)
	}
}

// The end rota learns at sign-in is the grant's own, and it goes into the home
// with the login, as Claude Code's refreshTokenExpiresAt. A refresh's earlier
// end is the end in effect, and stays out of the home: the home goes on being
// given the grant's own end, which is how its login is told from a new one.
func TestTheEndRotaLearnsReachesTheHome(t *testing.T) {
	onDisk(t)
	now := lifeNow(t).UnixMilli()
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	setURL(t, &ClaudeEndpoints.Profile, f.URL+"/profile")
	f.reply["/profile"] = func(*http.Request, map[string]any) (int, any) { return 500, nil }
	life := 1_000_000
	f.reply["/token"] = func(*http.Request, map[string]any) (int, any) {
		return 200, map[string]any{"access_token": "A", "refresh_token": "R", "expires_in": 28800,
			"refresh_token_expires_in": life, "account": map[string]string{"uuid": "u-5"}}
	}
	p, _ := Lookup("claude")
	_, state, _ := p.Begin(context.Background())
	tok, err := p.Complete(context.Background(), "CODE#ST", state)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAccount(5, "claude", tok)
	planned := func() int64 {
		t.Helper()
		_, files, err := StagePlan(context.Background(), a, t.TempDir())
		if err != nil || len(files) != 1 {
			t.Fatalf("plan: %v %v", files, err)
		}
		var doc struct {
			Login struct {
				Until int64 `json:"refreshTokenExpiresAt"`
			} `json:"claudeAiOauth"`
		}
		if err := decodeLenient(files[0].Content, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Login.Until
	}
	if got := planned(); got != now+int64(life)*1000 {
		t.Fatalf("after rota login the home is given %d, want %d", got, now+int64(life)*1000)
	}

	life = 500_000
	a.Token.ExpiresAt = now - 1
	if _, err := Refresh(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if got := planned(); got != now+1_000_000_000 || a.LoginUntil().UnixMilli() != now+int64(life)*1000 {
		t.Fatalf("after a refresh the home is given %d, want %d; in effect %v", got, now+1_000_000_000, a.LoginUntil())
	}
}

// A refresh that keeps the refresh token writes nothing into the home, which
// still holds the login in sync. The end in effect it learned survives the next
// adoption of that home: a home whose end is later, or which names none, takes
// nothing away from it, and one whose end is earlier still brings it earlier.
func TestAnEndARefreshLearnedSurvivesTheHome(t *testing.T) {
	onDisk(t)
	now := lifeNow(t).UnixMilli()
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	f.reply["/token"] = func(*http.Request, map[string]any) (int, any) {
		return 200, map[string]any{"access_token": "A2", "expires_in": 28800, "refresh_token_expires_in": 3 * 24 * 3600}
	}
	learned := now + 3*24*3600*1000
	for _, c := range []struct {
		name, until string // the home's refreshTokenExpiresAt
		own         string // the grant's own end afterwards ("" for none)
		want        int64  // the end in effect afterwards
	}{
		{"the home says the grant's own, later, end", "1999999999000", "1999999999000", learned},
		{"the home names no end", "null", "", learned},
		{"the home says an end earlier still", strconv.FormatInt(learned-3600_000, 10), strconv.FormatInt(learned-3600_000, 10), learned - 3600_000},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := livingClaude()
			a.Token.ExpiresAt = now - 1
			a.StagedWritten() // the home holds R1, as rota wrote it
			if _, err := Refresh(context.Background(), a); err != nil || a.Token.Refresh != "R1" {
				t.Fatalf("refresh: %v %+v", err, a.Token)
			}
			if a.LoginUntil().UnixMilli() != learned || a.Extra[claudeRefreshUntil] != "1999999999000" {
				t.Fatalf("the refresh's end is in effect, the grant's own untouched: %v", a.Extra)
			}
			if _, files, err := StagePlan(context.Background(), a, t.TempDir()); err != nil || len(files) != 0 {
				t.Fatalf("the home holds the login already, so nothing is written: %v %v", files, err)
			}
			home := homeWith(`{"accessToken":"A1","refreshToken":"R1","expiresAt":` + strconv.FormatInt(now-1, 10) +
				`,"refreshTokenExpiresAt":` + c.until + `}`)
			if err := AdoptFrom(a, home); err != nil {
				t.Fatal(err)
			}
			if a.LoginUntil().UnixMilli() != c.want || a.Extra[claudeRefreshUntil] != c.own || !a.StagedCurrent() {
				t.Fatalf("after adoption: in effect %v want %d, the grant's own %q want %q", a.LoginUntil(), c.want,
					a.Extra[claudeRefreshUntil], c.own)
			}
		})
	}
}

// A login somebody made inside the home and the application found to be this
// account's is a new grant: it ends when it says, and nothing the last grant
// said about its end, the end that killed the account included, counts.
func TestALoginAcceptedFromTheHomeStartsANewEnd(t *testing.T) {
	now := lifeNow(t)
	a := livingClaude()
	a.Extra[claudeLoginEnds] = strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10)
	a.Dead, a.DeadReason = true, "its login ended"
	a.StagedWritten()
	next := now.Add(30 * 24 * time.Hour).UnixMilli()
	home := homeWith(`{"accessToken":"A9","refreshToken":"R9","expiresAt":` + strconv.FormatInt(now.Add(8*time.Hour).UnixMilli(), 10) +
		`,"refreshTokenExpiresAt":` + strconv.FormatInt(next, 10) + `}`)
	var nl *NewLogin
	if err := AdoptFrom(a, home); !errors.As(err, &nl) {
		t.Fatalf("a new login: %v", err)
	}
	nl.Accept(a)
	if a.Dead || a.LoginEnded() || a.LoginUntil().UnixMilli() != next || a.Extra[claudeRefreshUntil] != strconv.FormatInt(next, 10) {
		t.Fatalf("the new grant's end: dead=%v in effect %v, %v", a.Dead, a.LoginUntil(), a.Extra)
	}
	if _, ok := a.Extra[claudeLoginEnds]; ok {
		t.Fatalf("nothing of the last grant's end in effect is left: %v", a.Extra)
	}
}

// The login this package wrote into the home, and the account has refreshed
// past since, is behind whatever end it carries, and whatever became of the
// account: it is never offered as a new login, because taking it back would
// take a spent token.
func TestTheLoginRotaWroteIsNeverANewLogin(t *testing.T) {
	now := lifeNow(t)
	for _, dead := range []bool{false, true} {
		a := livingClaude()
		a.Token.Refresh = "R0"
		a.StagedWritten() // rota wrote R0 into the home
		a.Token.Refresh, a.Token.Access = "R1", "A1-new"
		a.Token.ExpiresAt = now.Add(8 * time.Hour).UnixMilli()
		a.Dead = dead
		home := homeWith(`{"accessToken":"A0","refreshToken":"R0","expiresAt":` + strconv.FormatInt(now.Add(7*time.Hour).UnixMilli(), 10) +
			`,"refreshTokenExpiresAt":1888888888000}`)
		staged := a.Staged
		if err := AdoptFrom(a, home); err != nil {
			t.Fatalf("dead=%v: behind, not a new login: %v", dead, err)
		}
		if a.Token.Refresh != "R1" || a.Staged != staged || a.Dead != dead || !ClaudeHomeSpent(a, home) {
			t.Fatalf("dead=%v: nothing taken, still recorded as rota's write: %+v", dead, a)
		}
	}
}
