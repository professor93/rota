package rota

import (
	"errors"
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
