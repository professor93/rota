package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/wire"
)

// In a living claude account's last seven days, rota list says when its
// login ends and what to do before then, in days and hours, and in hours
// alone in its last day; at seven days it says nothing. Once the login is
// over it says what wire says. A dead account has its own reason to give,
// and an end nobody recorded is not guessed at.
func TestALoginsLastWeekIsNoted(t *testing.T) {
	now := time.UnixMilli(time.Now().UnixMilli())
	old := rota.Now
	rota.Now = func() time.Time { return now }
	t.Cleanup(func() { rota.Now = old })
	ending := func(left time.Duration) *rota.Account {
		return &rota.Account{ID: 13, Provider: "claude",
			Extra: map[string]string{"refresh_token_expires_at": strconv.FormatInt(now.Add(left).UnixMilli(), 10)}}
	}
	const day = 24 * time.Hour
	for _, c := range []struct {
		left time.Duration
		says string // "": nothing is said
	}{
		{30 * day, ""},
		{8 * day, ""},
		{7*day + time.Hour, ""},
		{7 * day, ""},
		{7*day - time.Millisecond, "6d 23h"},
		{5*day + 12*time.Hour + 30*time.Minute, "5d 12h"},
		{3 * day, "3d 0h"},
		{72*time.Hour - time.Millisecond, "2d 23h"},
		{day, "1d 0h"},
		{day - time.Millisecond, "23h"},
		{5*time.Hour + 59*time.Minute, "5h"},
		{20 * time.Minute, "1h"},
		{time.Millisecond, "1h"},
	} {
		want := ""
		if c.says != "" {
			want = fmt.Sprintf("login ends in %s (%s); rota login 13 before then", c.says, now.Add(c.left).UTC().Format(time.DateOnly))
		}
		if got := loginNote(ending(c.left)); got != want {
			t.Errorf("%v left: %q, want %q", c.left, got, want)
		}
	}
	over := ending(-time.Hour)
	if got := loginNote(over); got == "" || got != wire.LoginNote(over) || !strings.Contains(got, "ended") {
		t.Errorf("an ended login: %q", got)
	}
	dead := ending(71 * time.Hour)
	dead.Dead, dead.DeadReason = true, "its login ended"
	if got := loginNote(dead); got != "" {
		t.Errorf("a dead account: %q", got)
	}
	if got := loginNote(&rota.Account{ID: 13, Provider: "claude"}); got != "" {
		t.Errorf("an end nobody recorded: %q", got)
	}
}
