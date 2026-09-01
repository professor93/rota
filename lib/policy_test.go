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
	if changed, err := Refresh(context.Background(), b); !changed || err == nil || !b.Dead {
		t.Fatalf("expired token without a refresher is dead: changed=%v err=%v", changed, err)
	}
	c := &Account{ID: 1, Provider: "t-fresh"}
	c.Token = Token{Access: "x", ExpiresAt: 1}
	if changed, err := Refresh(context.Background(), c); !changed || err == nil || !c.Dead {
		t.Fatalf("no refresh token is dead: changed=%v err=%v", changed, err)
	}
}
