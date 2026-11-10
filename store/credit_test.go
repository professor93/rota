package store

import (
	"context"
	"fmt"
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

const answer = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"hello","session_id":"s-ok",` +
	`"total_cost_usd":0.0123,"usage":{"input_tokens":12,"output_tokens":3}}`

// creditStore is a store with a key account first in the rotation and a login
// account behind it, and a claude on PATH that answers with lines.
func creditStore(t *testing.T, lines ...string) (dir string, s *Store) {
	t.Helper()
	dir = t.TempDir()
	writeAccounts(t, dir, `{"accounts":[`+
		`{"id":1,"provider":"claude","uuid":"apikey-test","credential":"apikey","order":1,"token":{"accessToken":"`+testKey+`"}},`+
		`{"id":2,"provider":"claude","uuid":"c2","order":2,"token":{"accessToken":"tok"}}],"nextId":3,"ordered":true}`)
	claudeSays(t, lines...)
	return dir, openDir(t, dir)
}

// claudeSays puts a claude on PATH that prints these lines and exits 1 when
// the first is an error, as Claude Code does.
func claudeSays(t *testing.T, lines ...string) {
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

	claudeSays(t, answer)
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

	claudeSays(t, answer)
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
	// account go, a goroutine rather than a value returned; Wait is how a
	// caller knows it is done.
	s.Wait()
	if got := stored(t, dir, 1); !got.OutOfCredit() {
		t.Fatalf("a session that ended out of credit pauses its key: %d", got.NoCreditUntil)
	}
}

// An answer the CLI called a success is never a refusal, whatever it says and
// however the run then ended: not when the CLI exits 1 after it, and not when
// rota stops the run for a limit.
func TestAnAnswerQuotingTheRefusalPausesNothing(t *testing.T) {
	quoted := `{"type":"result","subtype":"success","is_error":false,"num_turns":1,` +
		`"result":"Your app shows \"Credit balance is too low\" because the org has no prepaid credit.","session_id":"s-ok"}`
	for name, c := range map[string]struct {
		lines []string
		exit  int
		spec  rota.Spec
		lim   *rota.Limits
	}{
		"exit 1": {lines: []string{quoted}, exit: 1, spec: rota.Spec{Prompt: "why"}},
		"over the events cap": {
			lines: []string{
				`{"type":"system","subtype":"init","session_id":"s-ok"}`,
				`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}`,
				quoted,
			},
			spec: rota.Spec{Prompt: "why", Stream: true, IncludeEvents: true},
			lim:  &rota.Limits{MaxEvents: 1},
		},
	} {
		dir, s := creditStore(t, answer)
		bin := t.TempDir()
		spec := fakecli.Lines(c.lines...)
		spec.Exit = c.exit
		fakecli.Install(t, bin, "claude", spec)
		t.Setenv("PATH", bin)
		a := s.Find(1)
		res, err := s.Run(context.Background(), a, c.spec, c.lim, nil)
		if err != nil || !res.IsError {
			t.Fatalf("%s: a failed run all the same: %v %+v", name, err, res)
		}
		if res.OutOfCredit() || a.OutOfCredit() || stored(t, dir, 1).NoCreditUntil != 0 {
			t.Fatalf("%s: a funded key was paused", name)
		}
	}
}

// An answer clears only the verdict its run started under. A refusal another
// process wrote while the run went on is newer, and stands; and a run that
// never reached the API says nothing about credit at all.
func TestAnAnswerClearsOnlyTheVerdictItStartedUnder(t *testing.T) {
	dir := t.TempDir()
	expired := time.Now().Add(-time.Minute).UnixMilli()
	writeAccounts(t, dir, fmt.Sprintf(`{"accounts":[{"id":1,"provider":"claude","uuid":"apikey-test","credential":"apikey",`+
		`"order":1,"noCreditUntil":%d,"token":{"accessToken":"`+testKey+`"}}],"nextId":2,"ordered":true}`, expired))
	bin := t.TempDir()
	t.Setenv("PATH", bin)

	// Started again should macOS kill the CLI on its way in: a run that
	// never reached the API has no verdict to clear, and it ends before the
	// next start opens the store again.
	var done chan error
	fakecli.Up(t, func(ready string) <-chan struct{} {
		fakecli.Install(t, bin, "claude", fakecli.Spec{Stdout: []string{answer}, Ready: ready, Sleep: "2s"})
		s := openDir(t, dir)
		a := s.Find(1)
		done = make(chan error, 1)
		// A start given up on is otherwise gone with the error that says why
		// it never ran.
		t.Cleanup(func() {
			select {
			case err := <-done:
				t.Logf("a run ended with: %v", err)
			default:
			}
		})
		ended := make(chan struct{})
		go func(done chan error) {
			_, err := s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil)
			done <- err
			close(ended)
		}(done)
		return ended
	})
	other := openDir(t, dir)
	newer := time.Now().Add(CreditPause).UnixMilli()
	other.Find(1).NoCreditUntil = newer
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	other.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := stored(t, dir, 1).NoCreditUntil; got != newer {
		t.Fatalf("the newer refusal stands: %d, want %d", got, newer)
	}

	// Answered without the API: no cost, nothing written.
	claudeSays(t, `{"type":"result","subtype":"success","is_error":false,"num_turns":0,"result":"","session_id":"s-local"}`)
	s := openDir(t, dir)
	a := s.Find(1)
	if _, err := s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !a.OutOfCredit() || stored(t, dir, 1).NoCreditUntil != newer {
		t.Fatal("a run the API never answered clears nothing")
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
	claudeSays(t, creditRefusal)
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
