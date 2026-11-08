package rota

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"testing/fstest"
	"time"
)

// freezeLogin is a login entry as Claude Code writes it, ending at until.
func freezeLogin(refresh string, exp int64, until string) string {
	return `{"accessToken":"A-` + refresh + `","refreshToken":"` + refresh + `","expiresAt":` +
		strconv.FormatInt(exp, 10) + `,"scopes":["user:inference"],"refreshTokenExpiresAt":` + until + `}`
}

// Every look that records a home as holding nothing of the account's ("-")
// leaves the next rotation of the account's own login to be taken: a later
// login with the account's refreshTokenExpiresAt is Claude Code having
// refreshed it since, and the account's token is spent.
func TestARotationIsTakenAfterEveryLookThatFoundTheHomeWithoutTheLogin(t *testing.T) {
	login := func(refresh string, exp int64) string { return freezeLogin(refresh, exp, "1999999999000") }
	earlier := time.Now().Add(-time.Hour).UnixMilli()
	later := time.Now().Add(3 * time.Hour).UnixMilli()
	for _, tr := range []struct {
		name   string
		staged string // what the account recorded before the look
		home   fstest.MapFS
	}{
		{"no store", fingerprint("R1"), fstest.MapFS{}},
		{"a store with no login", fingerprint("R1"), fstest.MapFS{claudeCredentials: {Data: []byte(`{"mcpOAuth":{}}`)}}},
		{"a blanked login that was not the current one", fingerprint("R0"),
			homeWith(`{"accessToken":"","refreshToken":"","expiresAt":0}`)},
		{"a login without an expiry", fingerprint("R1"), homeWith(`{"accessToken":"A-R1","refreshToken":"R1","expiresAt":0}`)},
		{"an older login of the lineage that is not this package's write", fingerprint("R1"), homeWith(login("R0", earlier))},
	} {
		t.Run(tr.name, func(t *testing.T) {
			a := livingClaude()
			a.Staged = tr.staged
			if err := AdoptFrom(a, tr.home); err != nil || a.Staged != stagedNone || a.Dead || a.Token.Refresh != "R1" {
				t.Fatalf("the look records the home as holding nothing of the account's: %q dead=%v %v", a.Staged, a.Dead, err)
			}
			if err := AdoptFrom(a, homeWith(login("R2", later))); err != nil || a.Token.Refresh != "R2" ||
				a.Token.Access != "A-R2" || a.Token.ExpiresAt != later || !a.StagedCurrent() || a.Dead {
				t.Fatalf("Claude Code's later rotation is taken: %+v %v", a, err)
			}
		})
	}
}

// One grant is told by when it ends, within a minute either way: Claude
// Code's refresh may set the end up to a second short of the real one. Both
// unknown is one grant; one known and one not is two.
func TestOneGrantIsToldByItsEndWithinAMinute(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"", "", true},
		{"1999999999000", "1999999999000", true},
		{"1999999999000", "1999999998100", true}, // 900 ms short
		{"1999999999000", "1999999939000", true}, // a minute short
		{"1999999999000", "2000000059000", true}, // a minute later
		{"1999999999000", "1999999938999", false},
		{"1999999999000", "2000000059001", false},
		{"1999999999000", "1999999879000", false}, // two minutes apart
		{"1999999999000", "", false},
		{"1999999999000", "soon", false},
	} {
		if sameGrant(c.a, c.b) != c.same || sameGrant(c.b, c.a) != c.same {
			t.Errorf("%q and %q: one grant should be %v", c.a, c.b, c.same)
		}
	}
}

// A rotation whose end Claude Code's refresh set 900 ms short of the
// account's is a rotation — in sync, and after a look that found the home
// without the account's login — and the end it carries is the one kept. Ends
// two minutes apart are two grants: a NewLogin, and passed over in a home
// recorded as holding nothing of the account's.
func TestARotationWhoseEndMovedASecondIsStillTheSameGrant(t *testing.T) {
	later := time.Now().Add(3 * time.Hour).UnixMilli()
	const short, apart = "1999999998100", "1999999879000" // livingClaude ends at 1999999999000
	for _, staged := range []string{fingerprint("R1"), stagedNone} {
		a := livingClaude()
		a.Staged = staged
		if err := AdoptFrom(a, homeWith(freezeLogin("R2", later, short))); err != nil || a.Token.Refresh != "R2" ||
			!a.StagedCurrent() || a.Extra[claudeRefreshUntil] != short {
			t.Fatalf("staged %q: a rotation 900 ms short is taken: %+v %v", staged, a, err)
		}
	}
	a := livingClaude()
	a.Staged = fingerprint("R1")
	err := AdoptFrom(a, homeWith(freezeLogin("R2", later, apart)))
	var nl *NewLogin
	if !errors.As(err, &nl) || a.Token.Refresh != "R1" || a.Staged != fingerprint("R1") {
		t.Fatalf("ends two minutes apart are a new login, nothing of it taken: %+v %v", a, err)
	}
	a = livingClaude()
	a.Staged = stagedNone
	if err := AdoptFrom(a, homeWith(freezeLogin("R2", later, apart))); err != nil || a.Token.Refresh != "R1" ||
		a.Staged != stagedNone {
		t.Fatalf("and passed over in a home recorded as holding nothing of the account's: %+v %v", a, err)
	}
}

// The provider may give a grant a shorter life than the one recorded at sign-in
// (a sign-in reply with no refresh_token_expires_in counts thirty days), and
// then the end in effect (login_ends_at) comes earlier while the grant's own
// end stays. Claude Code's refresh writes that earlier end into its store, so
// in a home recorded as holding nothing of the account's a login ending within
// a minute of the end in effect is the account's own rotation and is taken —
// for an account that never learned its grant's own end too — never passed
// over as the login from before for rota to write its spent copy over. One not
// newer than the account's token, one recorded as another account's, and one
// two minutes off the end in effect are passed over as before.
func TestARotationOfAShortenedGrantIsTakenAfterALookThatFoundTheHomeWithoutTheLogin(t *testing.T) {
	later := time.Now().Add(3 * time.Hour).UnixMilli()
	const inEffect, shaved, off = "1999567999000", "1999567998100", "1999567879000" // five days before livingClaude's 1999999999000
	login := func(exp int64, until, who string) string {
		return `{"accessToken":"A-R2","refreshToken":"R2","expiresAt":` + strconv.FormatInt(exp, 10) +
			`,"refreshTokenExpiresAt":` + until + `,"tokenAccount":` + who + `}`
	}
	shortened := func(identity bool) *Account {
		a := livingClaude()
		a.Staged = stagedNone
		a.Extra[claudeLoginEnds] = inEffect
		if !identity {
			delete(a.Extra, claudeRefreshUntil)
		}
		return a
	}
	for _, identity := range []bool{true, false} {
		a := shortened(identity)
		if err := AdoptFrom(a, homeWith(login(later, shaved, "null"))); err != nil || a.Token.Refresh != "R2" ||
			!a.StagedCurrent() || a.Extra[claudeRefreshUntil] != shaved || a.Dead {
			t.Fatalf("grant end known=%v: the shortened grant's rotation is taken: %+v %v", identity, a, err)
		}
		if end, ok := a.loginEnd(); !ok || strconv.FormatInt(end, 10) != shaved {
			t.Fatalf("grant end known=%v: the end in effect is the home's: %d %v", identity, end, ok)
		}
	}
	for _, c := range []struct {
		what  string
		login string
	}{
		{"no newer than the account's token", login(time.Now().Add(-time.Hour).UnixMilli(), shaved, "null")},
		{"recorded as another account's", login(later, shaved, `{"uuid":"u-other"}`)},
		{"two minutes off the end in effect", login(later, off, "null")},
	} {
		a := shortened(true)
		if err := AdoptFrom(a, homeWith(c.login)); err != nil || a.Token.Refresh != "R1" || a.Staged != stagedNone {
			t.Fatalf("%s: passed over: %+v %v", c.what, a, err)
		}
	}
}

// A grant ending more than a minute after another was signed in after it;
// within a minute, or with either end unknown, nothing says so.
func TestAGrantEndingMoreThanAMinuteLaterWasSignedInAfter(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		after bool
	}{
		{"2000000059001", "1999999999000", true},
		{"2000086399000", "1999999999000", true},
		{"2000000059000", "1999999999000", false}, // a minute: the same grant
		{"1999999999000", "1999999999000", false},
		{"1999913599000", "1999999999000", false}, // earlier
		{"2000086399000", "", false},
		{"", "1999999999000", false},
	} {
		if endsAfter(c.a, c.b) != c.after {
			t.Errorf("%q after %q should be %v", c.a, c.b, c.after)
		}
	}
}

// Claude Code may record whose login it is (tokenAccount). A login ending in
// the same minute as the account's but recorded as another account's is never
// a rotation of it: it is a NewLogin, in sync or after a look that found the
// home without the account's login. The same account recorded, or nothing to
// go by — no record, one of another shape, an account whose uuid is not known
// — leaves the one-minute rule alone, with no network call, and never makes
// the login unreadable.
func TestALoginRecordedAsAnotherAccountsIsNeverARotation(t *testing.T) {
	later := time.Now().Add(3 * time.Hour).UnixMilli()
	login := func(until, who string) string {
		return `{"accessToken":"A-R2","refreshToken":"R2","expiresAt":` + strconv.FormatInt(later, 10) +
			`,"refreshTokenExpiresAt":` + until + `,"tokenAccount":` + who + `}`
	}
	const sameMinute, other = "1999999998100", `{"uuid":"u-other","emailAddress":"other@x"}` // livingClaude: u-5, ends 1999999999000
	for _, staged := range []string{fingerprint("R1"), stagedNone} {
		a := livingClaude()
		a.Staged = staged
		var nl *NewLogin
		if err := AdoptFrom(a, homeWith(login(sameMinute, other))); !errors.As(err, &nl) || a.Token.Refresh != "R1" ||
			a.Staged != staged {
			t.Fatalf("staged %q: another account's login, however close its end, is a new login: %+v %v", staged, a, err)
		}
		for _, who := range []string{`{"uuid":"u-5","emailAddress":"five@x"}`, `{"uuid":"U-5"}`, `null`,
			`{"emailAddress":"five@x"}`, `"u-other"`, `[1]`} {
			a := livingClaude()
			a.Staged = staged
			if err := AdoptFrom(a, homeWith(login(sameMinute, who))); err != nil || a.Token.Refresh != "R2" || !a.StagedCurrent() {
				t.Fatalf("staged %q, tokenAccount %s: a rotation as before: %+v %v", staged, who, a, err)
			}
		}
	}
	a := livingClaude()
	a.Staged, a.UUID = fingerprint("R1"), ""
	if err := AdoptFrom(a, homeWith(login(sameMinute, other))); err != nil || a.Token.Refresh != "R2" {
		t.Fatalf("an account whose uuid is not known has nothing to compare: a rotation as before: %+v %v", a, err)
	}
	a = livingClaude()
	a.Staged = stagedNone
	if err := AdoptFrom(a, homeWith(login("1999913599000", other))); err != nil || a.Token.Refresh != "R1" ||
		a.Staged != stagedNone {
		t.Fatalf("ending a day earlier it is the login from before, passed over whoever's: %+v %v", a, err)
	}
}

// Refuse remembers the refused grant by when it ends. In a home recorded as
// holding nothing of the account's, a login of that grant — rotated or not —
// is passed over rather than handed back to be asked about again, while a
// later grant still is. The account's own login going back in (StagedWritten,
// or a stage) and a login accepted as the account's forget it.
func TestARefusedGrantIsPassedOverUntilTheAccountsOwnLoginIsBackIn(t *testing.T) {
	later := time.Now().Add(3 * time.Hour).UnixMilli()
	const refused, afterThat = "2000086399000", "2000172799000" // livingClaude ends at 1999999999000
	a := livingClaude()
	a.Staged = fingerprint("R1")
	var nl *NewLogin
	if !errors.As(AdoptFrom(a, homeWith(freezeLogin("RX", later, refused))), &nl) {
		t.Fatal("setup: another grant is a new login")
	}
	_ = nl.Refuse(a, &Identity{UUID: "u-other", Email: "other@x"})
	if a.Extra[claudeRefusedGrant] != refused+" u-other" || a.Staged != stagedNone {
		t.Fatalf("the refused grant is remembered, with whose it is: %v %q", a.Extra, a.Staged)
	}
	for _, rt := range []string{"RX", "RY"} {
		if err := AdoptFrom(a, homeWith(freezeLogin(rt, later+3_600_000, refused))); err != nil || a.Token.Refresh != "R1" ||
			a.Staged != stagedNone {
			t.Fatalf("%s: a login of the refused grant is passed over, not asked about again: %+v %v", rt, a, err)
		}
	}
	if !errors.As(AdoptFrom(a, homeWith(freezeLogin("RZ", later, afterThat))), &nl) {
		t.Fatal("a later grant than that is a new login still")
	}
	a.StagedWritten()
	if _, ok := a.Extra[claudeRefusedGrant]; ok {
		t.Fatalf("the account's own login written in forgets the refusal: %v", a.Extra)
	}

	b := livingClaude()
	b.Staged, b.Extra[claudeRefusedGrant] = fingerprint("R1"), refused
	errors.As(AdoptFrom(b, homeWith(freezeLogin("R9", later, afterThat))), &nl)
	nl.Accept(b)
	if _, ok := b.Extra[claudeRefusedGrant]; ok || b.Token.Refresh != "R9" {
		t.Fatalf("a login accepted as the account's forgets it: %v", b.Extra)
	}

	onDisk(t)
	c := livingClaude()
	c.Staged, c.Extra[claudeRefusedGrant] = stagedNone, refused
	if _, err := Stage(c, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Extra[claudeRefusedGrant]; ok || !c.StagedCurrent() {
		t.Fatalf("a stage that writes the account's login forgets it: %v", c.Extra)
	}
}

// A login is taken for the refused grant only when Claude Code records it as
// the account refused, or records nothing; one recorded as this account — a
// /login of its own in the minute after the refusal — or as a third account is
// handed back as a new login. A dead account passes the refused grant over
// too, rather than asking about it at every look.
func TestARefusalIsTakenOnlyForTheAccountRefusedOrNobody(t *testing.T) {
	later := time.Now().Add(3 * time.Hour).UnixMilli()
	const refused = "2000086399000"
	login := func(until, who string) string {
		return `{"accessToken":"A-RX","refreshToken":"RX","expiresAt":` + strconv.FormatInt(later, 10) +
			`,"refreshTokenExpiresAt":` + until + `,"tokenAccount":` + who + `}`
	}
	for _, c := range []struct {
		record, who string
		passed      bool
	}{
		{refused + " u2", `{"uuid":"u2"}`, true},
		{refused + " u2", `null`, true},
		{refused + " u2", `{"uuid":"u-5"}`, false}, // this account's own /login (livingClaude is u-5)
		{refused + " u2", `{"uuid":"u3"}`, false},
		{refused, `null`, true},
		{refused, `{"uuid":"u3"}`, false},
		// A refusal whose record fell back on the login's own record of this
		// account (the provider named an e-mail only): a login recorded as
		// this account is still never taken for it.
		{refused + " u-5", `{"uuid":"u-5"}`, false},
	} {
		for _, dead := range []bool{false, true} {
			a := livingClaude()
			a.Staged, a.Dead, a.Extra[claudeRefusedGrant] = stagedNone, dead, c.record
			err := AdoptFrom(a, homeWith(login("2000086429000", c.who))) // thirty seconds after the refused one
			var nl *NewLogin
			if got := !errors.As(err, &nl); got != c.passed || a.Token.Refresh != "R1" {
				t.Errorf("record %q, tokenAccount %s, dead=%v: passed over %v, want %v (%v)", c.record, c.who, dead, got, c.passed, err)
			}
		}
	}
}

// A fresh login forgets a refusal along with everything else of the login
// before it.
func TestAFreshLoginForgetsARefusal(t *testing.T) {
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	setURL(t, &ClaudeEndpoints.Profile, f.URL+"/profile")
	f.reply["/token"] = func(*http.Request, map[string]any) (int, any) {
		return 200, map[string]any{"access_token": "A", "refresh_token": "R", "expires_in": 3600}
	}
	f.reply["/profile"] = func(*http.Request, map[string]any) (int, any) { return 500, nil }
	p, _ := Lookup("claude")
	_, state, _ := p.Begin(context.Background())
	tok, err := p.Complete(context.Background(), "CODE#ST", state)
	if err != nil {
		t.Fatal(err)
	}
	a := livingClaude()
	a.Extra[claudeRefusedGrant] = "2000086399000 u2"
	a.Apply(tok)
	if _, ok := a.Extra[claudeRefusedGrant]; ok {
		t.Fatalf("forgotten: %v", a.Extra)
	}
}
