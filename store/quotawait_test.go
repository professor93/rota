package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// usageFake stands in for claude's usage endpoint, answering each request
// with the next answer it was given and counting them.
type usageFake struct {
	mu      sync.Mutex
	answers []usageAnswer
	asked   int
}

type usageAnswer struct {
	status     int
	retryAfter string
	body       string
}

func fakeUsage(t *testing.T) *usageFake {
	t.Helper()
	f := &usageFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.asked++
		a := usageAnswer{status: 500}
		if len(f.answers) > 0 {
			a, f.answers = f.answers[0], f.answers[1:]
		}
		f.mu.Unlock()
		if a.retryAfter != "" {
			w.Header().Set("Retry-After", a.retryAfter)
		}
		w.WriteHeader(a.status)
		fmt.Fprint(w, a.body)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Usage
	rota.ClaudeEndpoints.Usage = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Usage = old })
	return f
}

func (f *usageFake) next(status int, retryAfter, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, usageAnswer{status, retryAfter, body})
}

func (f *usageFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

// percent is a usage reply whose five-hour window stands at p.
func percent(p int) string { return fmt.Sprintf(`{"five_hour":{"utilization":%d}}`, p) }

// heldClock holds rota's clock at a moment the test moves by hand.
type heldClock struct{ now time.Time }

func holdClock(t *testing.T) *heldClock {
	t.Helper()
	c := &heldClock{now: time.Now().Truncate(time.Second)}
	old := rota.Now
	rota.Now = func() time.Time { return c.now }
	t.Cleanup(func() { rota.Now = old })
	return c
}

func (c *heldClock) pass(d time.Duration) { c.now = c.now.Add(d) }

// tokenClaude is a claude account on a token rota holds, in a directory the
// person chose, so reading its usage is one request and nothing else.
func tokenClaude(t *testing.T, s *Store) *rota.Account {
	t.Helper()
	a := s.add("claude")
	a.Email = "w@x"
	a.Token = rota.Token{Access: "A-w", Refresh: "R-w", ExpiresAt: rota.Now().Add(8 * time.Hour).UnixMilli()}
	a.ConfigDir = t.TempDir()
	return a
}

// The usage endpoint's 429 with a Retry-After in seconds is kept to: nothing
// asks it again before then — not a forced reading, not another store opened
// on the same directory, not a server's sweep — and every reading that wanted
// asking says why it was not, with the reading it has. The refusal kills
// nothing. Once the wait is over the endpoint is asked again, and its answer
// ends the wait.
func TestTheUsageEndpointIsLeftAloneForAsLongAsItSays(t *testing.T) {
	clock := holdClock(t)
	u := fakeUsage(t)
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := tokenClaude(t, s)
	a.Quota = &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: 30, Primary: true}}}
	a.QuotaAt = clock.now.Add(-20 * time.Minute).UnixMilli()
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	u.next(429, "120", `{"error":{"type":"rate_limit_error"}}`)
	errs := s.Refresh(context.Background(), true, a)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "asked rota to wait") || !strings.Contains(errs[0].Error(), "429") {
		t.Fatalf("one line, saying the provider asked to wait: %v", errs)
	}
	var he *rota.HTTPError
	if !errors.As(errs[0], &he) || he.Status != 429 {
		t.Fatalf("the refusal itself is still there to classify: %v", errs[0])
	}
	want := rota.QuotaWait{Until: clock.now.Add(2 * time.Minute).UnixMilli(), Status: 429, Count: 1}
	if a.QuotaWait != want {
		t.Fatalf("wait %+v, want %+v", a.QuotaWait, want)
	}
	if a.Dead || a.Status() != rota.StatusOK || a.Quota.Windows[0].Percent != 30 {
		t.Fatalf("the account lives on with its last reading: dead=%v %s %+v", a.Dead, a.Status(), a.Quota)
	}
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	b := s2.Find(a.ID)
	if b.QuotaWait != want {
		t.Fatalf("the wait is in the store: %+v", b.QuotaWait)
	}
	for _, force := range []bool{true, false} {
		errs = s2.Refresh(context.Background(), force, b)
		if u.count() != 1 {
			t.Fatalf("force=%v: asked %d times", force, u.count())
		}
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "asked rota to wait") || !strings.Contains(errs[0].Error(), "reading shown was taken") {
			t.Fatalf("force=%v: says why it did not ask, and how old the reading is: %v", force, errs)
		}
	}
	if errs := s2.Maintain(context.Background()); len(errs) != 0 || u.count() != 1 {
		t.Fatalf("a sweep asks nothing and has nothing new to say: %v, asked %d", errs, u.count())
	}

	clock.pass(2*time.Minute + time.Second)
	u.next(200, "", percent(42))
	if errs := s2.Refresh(context.Background(), true, b); len(errs) != 0 || u.count() != 2 {
		t.Fatalf("asked again once the wait is over: %v, asked %d", errs, u.count())
	}
	if b.Quota.Windows[0].Percent != 42 || b.QuotaWait != (rota.QuotaWait{}) {
		t.Fatalf("a reading ends the wait: %+v %+v", b.Quota, b.QuotaWait)
	}
	s2.Close()
	s3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if c := s3.Find(a.ID); c.Quota.Windows[0].Percent != 42 || c.QuotaWait != (rota.QuotaWait{}) {
		t.Fatalf("on disk: %+v %+v", c.Quota, c.QuotaWait)
	}
}

// A refusal waits a minute, twice as long each time in a row, up to an hour —
// a 429 without a Retry-After, or the endpoint failing on its side — and a
// Retry-After asks for at least as long as it says: one that is a date and
// longer than the backoff is kept to the second, one longer than an hour is
// kept for an hour, and one shorter than the backoff gives way to it. A
// reading starts the count again. Anything else that goes wrong is reported
// as ever and asks no wait.
func TestAUsageWaitIsTheLongerOfTheProvidersWordAndABackoffBoundedByAnHour(t *testing.T) {
	clock := holdClock(t)
	u := fakeUsage(t)
	s := openTemp(t)
	a := tokenClaude(t, s)
	refuse := func(status int, retryAfter string, wait time.Duration, count int) {
		t.Helper()
		a.Token.ExpiresAt = clock.now.Add(8 * time.Hour).UnixMilli() // hours pass here
		u.next(status, retryAfter, "")
		before := u.count()
		errs := s.Refresh(context.Background(), true, a)
		if u.count() != before+1 || len(errs) != 1 {
			t.Fatalf("%d %q: asked once, said once: %d %v", status, retryAfter, u.count()-before, errs)
		}
		want := rota.QuotaWait{Until: clock.now.Add(wait).UnixMilli(), Status: status, Count: count}
		if a.QuotaWait != want {
			t.Fatalf("%d %q: %+v, want %+v", status, retryAfter, a.QuotaWait, want)
		}
		clock.pass(wait + time.Second)
	}

	refuse(429, clock.now.Add(3*time.Minute).UTC().Format(http.TimeFormat), 3*time.Minute, 1)
	refuse(503, "", 2*time.Minute, 2)
	refuse(429, "soon", 4*time.Minute, 3)
	refuse(429, "86400", time.Hour, 4)
	refuse(500, "0", 16*time.Minute, 5)
	refuse(502, "", 32*time.Minute, 6)
	refuse(429, "", time.Hour, 7)
	for n := 8; n < 70; n++ {
		refuse(429, "", time.Hour, n)
	}

	u.next(200, "", percent(5))
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 || a.QuotaWait != (rota.QuotaWait{}) {
		t.Fatalf("a reading ends it: %v %+v", errs, a.QuotaWait)
	}
	refuse(429, "", time.Minute, 1)
	refuse(429, "1", 2*time.Minute, 2)
	refuse(503, "30", 4*time.Minute, 3)
	refuse(429, "300", 8*time.Minute, 4)
	refuse(429, "1200", 20*time.Minute, 5)

	u.next(401, "", "")
	errs := s.Refresh(context.Background(), true, a)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "http 401") || a.QuotaWait.Waiting() || a.QuotaWait.Count != 5 {
		t.Fatalf("a 401 is reported as before and asks no wait: %v %+v", errs, a.QuotaWait)
	}
}

// A store whose lock was released — `--with quota` reads after a run —
// still keeps what it learned: the wait a provider asked for, and a reading,
// go into the store on disk under the lock taken again for that one write,
// and nothing else of the account does. The token another process rotated
// meanwhile stays the one on disk.
func TestAStoreWithoutItsLockStillKeepsTheWaitAndTheReading(t *testing.T) {
	clock := holdClock(t)
	u := fakeUsage(t)
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := tokenClaude(t, s)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Release()

	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	other.Find(a.ID).Token.Access = "A-rotated"
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	other.Close()

	onDisk := func() *rota.Account {
		t.Helper()
		d, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		return d.Find(a.ID)
	}

	u.next(429, "60", "")
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 1 || !strings.Contains(errs[0].Error(), "asked rota to wait") {
		t.Fatalf("the refusal is said: %v", errs)
	}
	if d := onDisk(); d.QuotaWait.Until != clock.now.Add(time.Minute).UnixMilli() || d.Token.Access != "A-rotated" {
		t.Fatalf("the wait is on disk and the token is the one there: %+v %q", d.QuotaWait, d.Token.Access)
	}

	clock.pass(time.Minute + time.Second)
	u.next(200, "", percent(17))
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
		t.Fatal(errs)
	}
	d := onDisk()
	if d.Quota == nil || d.Quota.Windows[0].Percent != 17 || d.QuotaAt != clock.now.UnixMilli() || d.QuotaWait != (rota.QuotaWait{}) || d.Token.Access != "A-rotated" {
		t.Fatalf("the reading is on disk, the wait gone, the token untouched: %+v %d %+v %q", d.Quota, d.QuotaAt, d.QuotaWait, d.Token.Access)
	}
}

// A store written before waits were kept reads as one with none, and a wait
// written down reads back as it was.
func TestAStoreWithoutWaitsReadsAndAWaitRoundTrips(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"claude","token":{"accessToken":"t"},"quotaAt":5}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Find(1)
	if a.QuotaWait != (rota.QuotaWait{}) || a.QuotaAt != 5 {
		t.Fatalf("%+v", a)
	}
	a.QuotaWait = rota.QuotaWait{Until: 99, Status: 503, Count: 2}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Find(1).QuotaWait; got != (rota.QuotaWait{Until: 99, Status: 503, Count: 2}) {
		t.Fatalf("%+v", got)
	}
}

// A store without its lock was opened before the run it served, which may
// have lasted hours. A wait another process recorded meanwhile — and the
// reading it took before that — is read from disk before anything is asked:
// `--with quota` at the end of the run asks nothing, says why, and shows the
// newer reading.
func TestAStoreWithoutItsLockKeepsAWaitRecordedSinceItWasOpened(t *testing.T) {
	clock := holdClock(t)
	u := fakeUsage(t)
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := tokenClaude(t, s)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Release() // the run starts

	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := other.Find(a.ID)
	u.next(200, "", percent(33))
	if errs := other.Refresh(context.Background(), true, b); len(errs) != 0 {
		t.Fatal(errs)
	}
	read := b.QuotaAt
	clock.pass(time.Minute)
	u.next(429, "600", "")
	if errs := other.Refresh(context.Background(), true, b); len(errs) != 1 {
		t.Fatalf("the other process is refused: %v", errs)
	}
	want := b.QuotaWait
	other.Close()

	clock.pass(time.Minute) // the run ends
	errs := s.Refresh(context.Background(), true, a)
	if u.count() != 2 {
		t.Fatalf("asked %d times: the wait on disk must hold the run's reading back", u.count())
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "is not asked again until") || !strings.Contains(errs[0].Error(), "asked rota to wait") {
		t.Fatalf("one line saying why: %v", errs)
	}
	if a.QuotaWait != want || a.QuotaAt != read || a.Quota.Windows[0].Percent != 33 {
		t.Fatalf("the wait and the reading are the ones on disk: %+v %d %+v", a.QuotaWait, a.QuotaAt, a.Quota)
	}
}

// Stores without their locks that were each refused write their waits in any
// order, and the store ends up with the longest and the longest run of
// refusals; a reading goes in only when it is newer. An account removed since,
// or whose id now names an account of another provider, is not written at
// all.
func TestReadingsKeptWithoutTheLockMergeIntoTheStore(t *testing.T) {
	clock := holdClock(t)
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := tokenClaude(t, s)
	a.Quota, a.QuotaAt = &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: 50}}}, clock.now.UnixMilli()
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	at := func(d time.Duration) int64 { return clock.now.Add(d).UnixMilli() }
	keep := func(w rota.QuotaWait, quotaAt int64) {
		t.Helper()
		x := *a
		x.QuotaWait, x.QuotaAt = w, quotaAt
		x.Quota = &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: float64(quotaAt % 100)}}}
		r := &Store{backend: s.backend, released: true}
		if err := r.keepReadings([]*rota.Account{&x}); err != nil {
			t.Fatal(err)
		}
	}
	onDisk := func() *rota.Account {
		t.Helper()
		d, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		return d.Find(a.ID)
	}

	keep(rota.QuotaWait{Until: at(10 * time.Minute), Status: 429, Count: 1}, a.QuotaAt)
	keep(rota.QuotaWait{Until: at(time.Minute), Status: 503, Count: 4}, a.QuotaAt)
	keep(rota.QuotaWait{Until: at(2 * time.Minute), Status: 429, Count: 2}, a.QuotaAt-1)
	d := onDisk()
	if d.QuotaWait != (rota.QuotaWait{Until: at(10 * time.Minute), Status: 429, Count: 4}) || d.QuotaAt != a.QuotaAt || d.Quota.Windows[0].Percent != 50 {
		t.Fatalf("the longest wait, the most refusals, the newest reading: %+v %d %+v", d.QuotaWait, d.QuotaAt, d.Quota)
	}

	writeAccounts(t, dir, `{"accounts":[{"id":`+fmt.Sprint(a.ID)+`,"provider":"codex","token":{"accessToken":"c"}}],"nextId":9}`)
	before := readFile(filepath.Join(dir, "accounts.json"))
	keep(rota.QuotaWait{Until: at(time.Hour), Status: 429, Count: 1}, at(time.Second))
	x := *a
	x.ID = 7
	x.QuotaAt = at(time.Second)
	if err := (&Store{backend: s.backend, released: true}).keepReadings([]*rota.Account{&x}); err != nil {
		t.Fatal(err)
	}
	if after := readFile(filepath.Join(dir, "accounts.json")); after != before {
		t.Fatalf("nothing written for an id another provider has, or one that is gone:\n%s", after)
	}
}

// A refusal long after the last wait ran out — longer than the longest wait —
// is the first of a new run, and waits a minute rather than where the old
// run left off.
func TestARefusalLongAfterTheLastWaitStartsTheBackoffAgain(t *testing.T) {
	clock := holdClock(t)
	u := fakeUsage(t)
	s := openTemp(t)
	a := tokenClaude(t, s)
	a.QuotaWait = rota.QuotaWait{Until: clock.now.Add(-QuotaWaitMax - time.Second).UnixMilli(), Status: 429, Count: 5}
	u.next(429, "", "")
	s.Refresh(context.Background(), true, a)
	if want := (rota.QuotaWait{Until: clock.now.Add(time.Minute).UnixMilli(), Status: 429, Count: 1}); a.QuotaWait != want {
		t.Fatalf("%+v, want %+v", a.QuotaWait, want)
	}
	a.QuotaWait = rota.QuotaWait{Until: clock.now.Add(-time.Minute).UnixMilli(), Status: 429, Count: 5}
	u.next(429, "", "")
	s.Refresh(context.Background(), true, a)
	if want := (rota.QuotaWait{Until: clock.now.Add(32 * time.Minute).UnixMilli(), Status: 429, Count: 6}); a.QuotaWait != want {
		t.Fatalf("a refusal soon after the last is the next in the run: %+v, want %+v", a.QuotaWait, want)
	}
}

// A store without its lock that is refused long after the last wait ran out
// starts a new run on disk too: the count of the old run, which it read from
// the store before asking, is not written back over the new one's, so the
// next refusal waits two minutes rather than thirty-two. A count on disk from
// a wait that ran out within the hour is still the same run, and is kept.
func TestARefusalWithoutTheLockLongAfterTheLastWaitStartsTheBackoffAgainOnDisk(t *testing.T) {
	clock := holdClock(t)
	u := fakeUsage(t)
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := tokenClaude(t, s)
	a.QuotaWait = rota.QuotaWait{Until: clock.now.Add(-2 * time.Hour).UnixMilli(), Status: 429, Count: 5}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Release()
	onDisk := func() rota.QuotaWait {
		t.Helper()
		d, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		return d.Find(a.ID).QuotaWait
	}

	u.next(429, "", "")
	s.Refresh(context.Background(), true, a)
	want := rota.QuotaWait{Until: clock.now.Add(time.Minute).UnixMilli(), Status: 429, Count: 1}
	if a.QuotaWait != want {
		t.Fatalf("in memory: %+v, want %+v", a.QuotaWait, want)
	}
	if got := onDisk(); got != want {
		t.Fatalf("on disk: %+v, want %+v", got, want)
	}
	clock.pass(time.Minute + time.Second)
	u.next(429, "", "")
	s.Refresh(context.Background(), true, a)
	if got, want := onDisk(), (rota.QuotaWait{Until: clock.now.Add(2 * time.Minute).UnixMilli(), Status: 429, Count: 2}); got != want {
		t.Fatalf("the next refusal is the second of the new run: %+v, want %+v", got, want)
	}

	// Written straight in, as a store that read the disk before another
	// process recorded its run would.
	keep := func(onDisk rota.QuotaWait) rota.QuotaWait {
		t.Helper()
		d, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		d.Find(a.ID).QuotaWait = onDisk
		if err := d.Save(); err != nil {
			t.Fatal(err)
		}
		d.Close()
		x := *a
		x.QuotaWait = rota.QuotaWait{Until: clock.now.Add(time.Minute).UnixMilli(), Status: 429, Count: 1}
		if err := (&Store{backend: s.backend, released: true}).keepReadings([]*rota.Account{&x}); err != nil {
			t.Fatal(err)
		}
		return x.QuotaWait
	}
	ran := keep(rota.QuotaWait{Until: clock.now.Add(-30 * time.Minute).UnixMilli(), Status: 503, Count: 5})
	if got, want := onDisk(), (rota.QuotaWait{Until: ran.Until, Status: 429, Count: 5}); got != want {
		t.Fatalf("a wait that ran out within the hour is the same run: %+v, want %+v", got, want)
	}
	ran = keep(rota.QuotaWait{Until: clock.now.Add(-QuotaWaitMax - time.Second).UnixMilli(), Status: 503, Count: 5})
	if got := onDisk(); got != ran {
		t.Fatalf("one that ran out longer ago than that is not: %+v, want %+v", got, ran)
	}
}

// A count is read from a store anything may have written. One below zero
// counts as none, so the next refusal is the first of a run and waits a
// minute, rather than shifting the backoff by a negative amount, which
// panics; one that cannot grow stays where it is, at the longest wait.
func TestAWaitCountOutOfRangeIsTakenAsTheEdgeOfIt(t *testing.T) {
	clock := holdClock(t)
	refused := &rota.HTTPError{Status: http.StatusTooManyRequests}
	for _, c := range []struct {
		count, want int
		wait        time.Duration
	}{
		{-1, 1, time.Minute},
		{-5, 1, time.Minute},
		{math.MinInt, 1, time.Minute},
		{math.MaxInt, math.MaxInt, time.Hour},
	} {
		a := &rota.Account{QuotaWait: rota.QuotaWait{Until: clock.now.Add(-time.Minute).UnixMilli(), Status: 429, Count: c.count}}
		if !turnedAway(a, refused) {
			t.Fatalf("count %d: a 429 is a refusal", c.count)
		}
		if want := (rota.QuotaWait{Until: clock.now.Add(c.wait).UnixMilli(), Status: 429, Count: c.want}); a.QuotaWait != want {
			t.Fatalf("count %d: %+v, want %+v", c.count, a.QuotaWait, want)
		}
	}

	// And through a reading, from the store on disk.
	u := fakeUsage(t)
	s := openTemp(t)
	a := tokenClaude(t, s)
	a.QuotaWait = rota.QuotaWait{Until: clock.now.Add(-time.Minute).UnixMilli(), Status: 429, Count: -3}
	u.next(429, "", "")
	errs := s.Refresh(context.Background(), true, a)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "asked rota to wait") || a.QuotaWait.Count != 1 {
		t.Fatalf("one note and a wait of a minute: %v %+v", errs, a.QuotaWait)
	}
}
