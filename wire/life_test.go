package wire

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// In a living claude account's last 72 hours, a listing says when its login
// ends and what to do before then, in hours; at 73 hours it says nothing. A
// dead account has its own reason to give, and an end nobody recorded is not
// guessed at.
func TestALoginsLastThreeDaysAreNoted(t *testing.T) {
	now := time.UnixMilli(time.Now().UnixMilli())
	old := rota.Now
	rota.Now = func() time.Time { return now }
	t.Cleanup(func() { rota.Now = old })
	ending := func(left time.Duration) *rota.Account {
		return &rota.Account{ID: 13, Provider: "claude",
			Extra: map[string]string{"refresh_token_expires_at": strconv.FormatInt(now.Add(left).UnixMilli(), 10)}}
	}
	for _, c := range []struct {
		left  time.Duration
		hours int // 0: nothing is said
	}{
		{30 * 24 * time.Hour, 0},
		{73 * time.Hour, 0},
		{72 * time.Hour, 0},
		{72*time.Hour - time.Millisecond, 71},
		{71 * time.Hour, 71},
		{5*time.Hour + 59*time.Minute, 5},
		{20 * time.Minute, 1},
		{time.Millisecond, 1},
	} {
		want := ""
		if c.hours != 0 {
			want = fmt.Sprintf("login ends in %dh (%s); rota login 13 before then", c.hours, now.Add(c.left).Format(time.DateOnly))
		}
		if got := LoginNote(ending(c.left)); got != want {
			t.Errorf("%v left: %q, want %q", c.left, got, want)
		}
	}
	dead := ending(71 * time.Hour)
	dead.Dead, dead.DeadReason = true, "its login ended"
	if got := LoginNote(dead); got != "" {
		t.Errorf("a dead account: %q", got)
	}
	if got := LoginNote(&rota.Account{ID: 13, Provider: "claude"}); got != "" {
		t.Errorf("an end nobody recorded: %q", got)
	}
}
