package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// resetter is a provider whose usage endpoint says the window has reset:
// account "a" is at 7% now, and every read is noted with when it came.
type resetter struct{}

var resetReads struct {
	sync.Mutex
	at []time.Time
}

func (resetter) Name() string                                               { return "t-api-reset" }
func (resetter) Begin(_ context.Context) (string, map[string]string, error) { return "", nil, nil }
func (resetter) Complete(context.Context, string, map[string]string) (*rota.Token, error) {
	return &rota.Token{Access: "a"}, nil
}
func (resetter) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}
func (resetter) Quota(_ context.Context, access string) (*rota.Quota, error) {
	resetReads.Lock()
	defer resetReads.Unlock()
	resetReads.at = append(resetReads.at, time.Now())
	if access != "a" {
		return nil, fmt.Errorf("only account a is due, and %q was read", access)
	}
	return &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: 7, Primary: true,
		ResetsAt: rota.When{Time: time.Now().Add(5 * time.Hour)}}}}, nil
}

func init() { rota.Register(resetter{}) }

// A server reads an account again a moment after its window resets, not up
// to a whole sweep later: the sweep is an hour apart here, and the reading
// arrives all the same, and the rotation's choice moves back to the account
// that was spent. It is the sweep that was coming anyway, woken early, and
// nothing reads again before the reset or after it.
func TestTheServerReadsAnAccountAgainOnceItsWindowResets(t *testing.T) {
	resetReads.Lock()
	resetReads.at = nil
	resetReads.Unlock()
	start := time.Now()
	// Far enough off that a busy machine still starts the server first: a
	// reading already due when the server looks is left to the next look.
	due := start.Add(1500 * time.Millisecond)
	// Inside the cache's lifetime and before the reset, so the reading is
	// fresh until the reset makes it due — and it does at `due`.
	at := start.Add(-10 * time.Second).UnixMilli()
	reset := due.Add(-store.ResetGrace).Format(time.RFC3339Nano)
	blob := fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"t-api-reset","order":1,"token":{"accessToken":"a"},"quotaAt":%d,
		 "quota":{"windows":[{"name":"5h","percent":100,"primary":true,"resetsAt":%q}]}},
		{"id":2,"provider":"t-api-reset","order":2,"token":{"accessToken":"b"},"quotaAt":%d,
		 "quota":{"windows":[{"name":"5h","percent":50,"primary":true}]}}]}`, at, reset, at)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{Token: "t", Dir: dir, RefreshEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	web := httptest.NewServer(srv.Handler())
	defer web.Close()
	choice := func() int {
		t.Helper()
		req, _ := http.NewRequest("GET", web.URL+"/v1/accounts", nil)
		req.Header.Set("Authorization", "Bearer t")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var doc struct {
			Default int `json:"default"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		return doc.Default
	}
	reads := func() []time.Time {
		resetReads.Lock()
		defer resetReads.Unlock()
		return append([]time.Time(nil), resetReads.at...)
	}

	if got := choice(); got != 2 {
		t.Fatalf("before the reset the spent account is passed by: %d", got)
	}
	for deadline := time.Now().Add(10 * time.Second); len(reads()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the window reset and nothing read the account again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := reads(); got[0].Before(due) {
		t.Fatalf("read %v before the reset made the reading due", due.Sub(got[0]))
	}
	if got := choice(); got != 1 {
		t.Fatalf("once read again, the account that was spent is the rotation's choice: %d", got)
	}
	time.Sleep(300 * time.Millisecond)
	if got := reads(); len(got) != 1 {
		t.Fatalf("one reset is one read: %v", got)
	}
}
