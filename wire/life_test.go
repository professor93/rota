package wire

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// In a living claude account's last seven days, a listing says when its
// login ends and what to do before then, in days and hours, and in hours
// alone in its last day; at seven days it says nothing. A dead account has
// its own reason to give, and an end nobody recorded is not guessed at.
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
	if LoginSoon != 7*day {
		t.Errorf("LoginSoon is %v", LoginSoon)
	}
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

// The STATUS cell and the note read one clock, the one the account's end is
// kept by, so under any clock an application sets they say the same thing.
func TestTheStatusCellAndTheNoteReadOneClock(t *testing.T) {
	at := time.UnixMilli(time.Now().Add(10 * 24 * time.Hour).UnixMilli())
	old := rota.Now
	rota.Now = func() time.Time { return at }
	t.Cleanup(func() { rota.Now = old })
	a := &rota.Account{ID: 4, Provider: "claude",
		Extra: map[string]string{"refresh_token_expires_at": strconv.FormatInt(at.Add(50*time.Hour).UnixMilli(), 10)}}
	if s, n := LoginStatus(a), LoginNote(a); s != "login ends in 2d" || n == "" {
		t.Fatalf("50 hours on the account's clock: status %q, note %q", s, n)
	}
	a.Extra["refresh_token_expires_at"] = strconv.FormatInt(at.Add(-3*time.Hour).UnixMilli(), 10)
	if s := LoginStatus(a); s != "login ended 3h ago" {
		t.Fatalf("three hours past on the account's clock: %q", s)
	}
}

// The note's date is UTC's, as the reason an account gives once its login has
// ended is, wherever the machine is: an end shortly after midnight UTC is that
// day, even west of UTC, where it is still the day before.
func TestALoginsNoteGivesTheDateInUTC(t *testing.T) {
	end := time.Date(2026, 11, 10, 0, 30, 0, 0, time.UTC)
	west := time.FixedZone("west", -8*60*60)
	oldLocal, oldNow := time.Local, rota.Now
	time.Local = west
	rota.Now = func() time.Time { return end.Add(-47 * time.Hour) }
	t.Cleanup(func() { time.Local, rota.Now = oldLocal, oldNow })
	a := &rota.Account{ID: 13, Provider: "claude",
		Extra: map[string]string{"refresh_token_expires_at": strconv.FormatInt(end.UnixMilli(), 10)}}
	if got, want := LoginNote(a), "login ends in 1d 23h (2026-11-10); rota login 13 before then"; got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
}
