package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// What Claude Code prints, in print mode, for a run the API refused because
// the key's organisation has no credit left, and the exit it ends with.
const creditRefusal = `{"type":"result","subtype":"success","is_error":true,"api_error_status":400,` +
	`"num_turns":1,"result":"Credit balance is too low","session_id":"s-credit","total_cost_usd":0}`

const answered = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"hello","session_id":"s-ok"}`

// creditStore is a store with a key account first in the rotation and a login
// account behind it, and a claude on PATH that answers with lines.
func creditStore(t *testing.T, lines ...string) (dir string, s *Store) {
	t.Helper()
	dir = t.TempDir()
	writeAccounts(t, dir, `{"accounts":[`+
		`{"id":1,"provider":"claude","uuid":"apikey-test","credential":"apikey","order":1,"token":{"accessToken":"`+testKey+`"}},`+
		`{"id":2,"provider":"claude","uuid":"c2","order":2,"token":{"accessToken":"tok"}}],"nextId":3,"ordered":true}`)
	answer(t, lines...)
	return dir, openDir(t, dir)
}

// answer puts a claude on PATH that prints these lines and exits 1 when the
// first is an error, as Claude Code does.
func answer(t *testing.T, lines ...string) {
	t.Helper()
	bin := t.TempDir()
	spec := fakecli.Lines(lines...)
	if strings.Contains(lines[0], `"is_error":true`) {
		spec.Exit = 1
	}
	fakecli.Install(t, bin, "claude", spec)
	t.Setenv("PATH", bin)
}

func openDir(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// stored is the account as the store on disk has it now.
func stored(t *testing.T, dir string, id int) *rota.Account {
	t.Helper()
	s := openDir(t, dir)
	defer s.Close()
	return s.Find(id)
}

// A run on a key that ends with the API refusing it for want of credit leaves
// the account out of credit for CreditPause, on disk as well as in hand; a run
// on it that is answered ends that at once.
func TestARunOutOfCreditPausesItsKey(t *testing.T) {
	dir, s := creditStore(t, creditRefusal)
	a := s.Find(1)
	before := time.Now()
	res, err := s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil)
	if err != nil || !res.OutOfCredit() || res.ExitCode != 1 {
		t.Fatalf("the run reports the refusal as the CLI gave it: %v %+v", err, res)
	}
	if !a.OutOfCredit() {
		t.Fatal("the account in hand knows")
	}
	got := stored(t, dir, 1)
	until := time.UnixMilli(got.NoCreditUntil)
	if !got.OutOfCredit() || until.Before(before.Add(CreditPause)) || until.After(time.Now().Add(CreditPause)) {
		t.Fatalf("the store keeps it for CreditPause: %v until %v", got.OutOfCredit(), until)
	}

	answer(t, answered)
	s = openDir(t, dir)
	a = s.Find(1)
	if _, err := s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if a.OutOfCredit() || a.NoCreditUntil != 0 {
		t.Fatal("an answered run says the credit is back")
	}
	if got := stored(t, dir, 1); got.NoCreditUntil != 0 {
		t.Fatalf("and the store forgets it: %d", got.NoCreditUntil)
	}
}

// Only a key is paused. A login has no credit of this kind, and a run that
// changes nothing writes nothing.
func TestOnlyAKeyIsPausedForCredit(t *testing.T) {
	dir, s := creditStore(t, creditRefusal)
	login := s.Find(2)
	if _, err := s.Run(context.Background(), login, rota.Spec{Prompt: "hi", Hermetic: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if login.NoCreditUntil != 0 || stored(t, dir, 2).NoCreditUntil != 0 {
		t.Fatal("a login is never paused for credit")
	}

	answer(t, answered)
	s = openDir(t, dir)
	key := s.Find(1)
	if _, err := s.Run(context.Background(), key, rota.Spec{Prompt: "hi"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if err != nil || strings.Contains(string(raw), "noCreditUntil") {
		t.Fatalf("nothing to say, nothing written: %v", err)
	}
}

// A session is judged when it ends, by how its last turn ended.
func TestASessionOutOfCreditPausesItsKeyWhenItEnds(t *testing.T) {
	dir, s := creditStore(t, creditRefusal)
	a := s.Find(1)
	sess, err := s.Start(context.Background(), a, rota.Spec{Prompt: "hi", Input: true, Stream: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	res, _ := sess.Wait()
	if !res.OutOfCredit() {
		t.Fatalf("the session ended on the refusal: %+v", res)
	}
	// The verdict is written by whatever waits for the session to let the
	// account go, which is a goroutine waking rather than a value returned.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		raw, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
		if err == nil && strings.Contains(string(raw), "noCreditUntil") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a session that ended out of credit must pause its key")
		}
	}
	if got := stored(t, dir, 1); !got.OutOfCredit() {
		t.Fatalf("paused: %d", got.NoCreditUntil)
	}
}

// Pasting the key again is somebody saying it is good.
func TestPastingTheKeyAgainEndsThePause(t *testing.T) {
	s := openTemp(t)
	dir := storeDir(s)
	a := keyAccountIn(t, s)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	answer(t, creditRefusal)
	if _, err := s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil); err != nil || !a.OutOfCredit() {
		t.Fatalf("paused: %v", err)
	}
	s = openDir(t, dir)
	again, added, _, err := s.LoginKey(context.Background(), "claude", testKey, "")
	if err != nil || added || again.ID != a.ID || again.OutOfCredit() {
		t.Fatalf("the same account, and good again: %v %v %d", err, added, again.NoCreditUntil)
	}
	s.Close()
	if got := stored(t, dir, a.ID); got.NoCreditUntil != 0 {
		t.Fatalf("on disk too: %d", got.NoCreditUntil)
	}
}
