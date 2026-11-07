package rota

import (
	"context"
	"errors"
	"testing"
	"time"
)

func win(pct float64, resetIn time.Duration, primary, scoped bool) Window {
	w := Window{Name: "w", Percent: pct, Primary: primary, Scoped: scoped}
	if resetIn != 0 {
		w.ResetsAt = When{time.Now().Add(resetIn)}
	}
	return w
}

func TestStatusFollowsDeathAndSpentWindows(t *testing.T) {
	cases := []struct {
		a    Account
		want Status
	}{
		{Account{Dead: true}, StatusReauth},
		{Account{}, StatusOK},
		{Account{Quota: &Quota{Windows: []Window{win(100, time.Hour, true, false)}}}, StatusLimited},
		{Account{Quota: &Quota{Windows: []Window{win(100, -time.Hour, true, false)}}}, StatusOK},
		{Account{Quota: &Quota{Windows: []Window{win(100, 0, true, false)}}}, StatusLimited},
		{Account{Quota: &Quota{Windows: []Window{win(100, time.Hour, false, true)}}}, StatusOK},
		{Account{Quota: &Quota{Windows: []Window{win(99.9, time.Hour, true, false)}}}, StatusOK},
	}
	for i, c := range cases {
		if got := c.a.Status(); got != c.want {
			t.Fatalf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestRefreshVerdicts(t *testing.T) {
	fresh := &fakeProvider{name: "t-fresh", refreshTok: &Token{Access: "new", Refresh: "r2", ExpiresAt: nowMS() + 3_600_000}}
	Register(fakeRefresher{fresh})
	a := &Account{ID: 1, Provider: "t-fresh"}
	a.Token = Token{Access: "old", Refresh: "r1", ExpiresAt: nowMS() + 3_600_000}
	if changed, err := Refresh(context.Background(), a); changed || err != nil || a.Token.Access != "old" {
		t.Fatal("a valid token must be left alone")
	}
	a.Token.ExpiresAt = 1
	if changed, err := Refresh(context.Background(), a); !changed || err != nil || a.Token.Access != "new" || a.Token.Refresh != "r2" || a.Expired() {
		t.Fatalf("changed=%v err=%v a=%+v", changed, err, a.Token)
	}

	fresh.refreshTok, fresh.refreshErr = nil, errors.New("503")
	a.Token.ExpiresAt = 1
	if changed, err := Refresh(context.Background(), a); changed || err == nil || a.Dead {
		t.Fatalf("transient failure must keep the account: changed=%v err=%v", changed, err)
	}
	fresh.refreshErr = ErrDeadToken
	if changed, err := Refresh(context.Background(), a); !changed || err == nil || !a.Dead {
		t.Fatalf("dead verdict must mark the account: changed=%v err=%v", changed, err)
	}

	fresh.refreshErr, fresh.refreshTok = nil, &Token{}
	a.Dead = false
	if changed, err := Refresh(context.Background(), a); changed || err == nil || a.Token.Access != "new" {
		t.Fatalf("empty access token must be rejected: changed=%v err=%v", changed, err)
	}

	Register(&fakeProvider{name: "t-static"})
	b := &Account{ID: 1, Provider: "t-static"}
	b.Token = Token{Access: "key", ExpiresAt: 1}
	if changed, err := Refresh(context.Background(), b); !changed || err == nil || !b.Dead ||
		b.DeadReason != "t-static cannot refresh a credential" {
		t.Fatalf("expired token without a refresher is dead: changed=%v err=%v reason=%q", changed, err, b.DeadReason)
	}
	c := &Account{ID: 1, Provider: "t-fresh"}
	c.Token = Token{Access: "x", ExpiresAt: 1}
	if changed, err := Refresh(context.Background(), c); !changed || err == nil || !c.Dead ||
		c.DeadReason != "no refresh token" {
		t.Fatalf("no refresh token is dead: changed=%v err=%v reason=%q", changed, err, c.DeadReason)
	}
}

// An account holding a refresh token and no access token has nothing to run
// on, whatever its expiry says, so it is due for a refresh, and the refresh
// gives it one. An account holding neither, a token with no expiry, and a
// delegated account are left alone, as ever.
func TestARefreshTokenWithNoAccessTokenIsDueForARefresh(t *testing.T) {
	p := &fakeProvider{name: "t-refresh-only", refreshTok: &Token{Access: "new", Refresh: "r2", ExpiresAt: nowMS() + 3_600_000}}
	Register(fakeRefresher{p})
	for _, tok := range []Token{{Refresh: "r1"}, {Refresh: "r1", ExpiresAt: nowMS() + 3_600_000}} {
		a := &Account{ID: 1, Provider: "t-refresh-only", Token: tok}
		if !a.Expired() {
			t.Fatalf("a refresh token alone is due for a refresh: %+v", tok)
		}
		if changed, err := Refresh(context.Background(), a); !changed || err != nil || a.Token.Access != "new" ||
			a.Token.Refresh != "r2" || a.Expired() {
			t.Fatalf("refreshed: changed=%v err=%v %+v", changed, err, a.Token)
		}
	}
	p.refreshTok = nil
	p.refreshErr = errors.New("asked")
	for _, a := range []*Account{
		{ID: 1, Provider: "t-refresh-only"},
		{ID: 1, Provider: "t-refresh-only", Token: Token{Access: "key"}},
		{ID: 1, Provider: "t-refresh-only", Delegated: true, Token: Token{Refresh: "r1"}},
	} {
		if a.Expired() {
			t.Fatalf("not due: %+v", a)
		}
		if changed, err := Refresh(context.Background(), a); changed || err != nil || a.Dead {
			t.Fatalf("left alone: changed=%v err=%v dead=%v %+v", changed, err, a.Dead, a)
		}
	}
}

// A refused refresh is the only record of why a lineage ended, so the
// server's own words are kept on the account until a login revives it.
func TestADeadAccountKeepsTheRefusalUntilTheNextLogin(t *testing.T) {
	refusal := (&oauthTokenResp{Error: "invalid_grant", ErrorDesc: "refresh token reused"}).verdict(nil, grantRefresh)
	if !errors.Is(refusal, ErrDeadToken) {
		t.Fatalf("a refused refresh is still the dead-token verdict: %v", refusal)
	}
	p := &fakeProvider{name: "t-reason", refreshErr: refusal, identity: &Identity{UUID: "u-1"}}
	Register(fakeRefresher{p})
	a := &Account{ID: 1, Provider: "t-reason", UUID: "u-1",
		Token: Token{Access: "old", Refresh: "r1", ExpiresAt: 1}}
	if changed, err := Refresh(context.Background(), a); !changed || err == nil || !a.Dead {
		t.Fatalf("changed=%v err=%v dead=%v", changed, err, a.Dead)
	}
	if a.DeadReason != "invalid_grant: refresh token reused" {
		t.Fatalf("the refusal must be kept verbatim, got %q", a.DeadReason)
	}

	tok, err := p.Complete(context.Background(), "code", map[string]string{"verifier": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if got := MatchIdentity([]*Account{a}, "t-reason", tok.Identity); got != a {
		t.Fatalf("a login of the same identity must land on the dead account, got %v", got)
	}
	a.Apply(tok)
	if a.Dead || a.DeadReason != "" {
		t.Fatalf("a fresh login revives the account: dead=%v reason=%q", a.Dead, a.DeadReason)
	}
}
