package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// keeper is a provider with a usage endpoint and nothing behind it, so the
// background sweep can be watched without a network.
type keeper struct{}

func (keeper) Name() string                                               { return "t-api-keep" }
func (keeper) Begin(_ context.Context) (string, map[string]string, error) { return "", nil, nil }
func (keeper) Complete(context.Context, string, map[string]string) (*rota.Token, error) {
	return &rota.Token{Access: "k"}, nil
}
func (keeper) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}
func (keeper) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	return &rota.Token{Access: "k"}, nil
}
func (keeper) Quota(context.Context, string) (*rota.Quota, error) {
	return &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: 42}}}, nil
}

func init() { rota.Register(keeper{}) }

func TestTheServerKeepsUsageFreshWhileItRuns(t *testing.T) {
	dir := t.TempDir()
	blob := `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"t-api-keep","order":1,"token":{"accessToken":"k"}}]}`
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{Token: "t", Dir: dir, RefreshEvery: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	// Nobody has asked this server for anything; the reading must appear
	// anyway, which is the whole point of the sweep.
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
		if err == nil && strings.Contains(string(raw), `"quotaAt"`) {
			if !strings.Contains(string(raw), `42`) {
				t.Fatalf("the reading was stored without its number: %s", raw)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no usage was read in the background: %s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackgroundRefreshCanBeTurnedOff(t *testing.T) {
	dir := t.TempDir()
	blob := `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"t-api-keep","order":1,"token":{"accessToken":"k"}}]}`
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{Token: "t", Dir: dir, RefreshEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(120 * time.Millisecond)
	raw, _ := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if strings.Contains(string(raw), `"quotaAt"`) {
		t.Fatalf("a negative interval must ask the provider nothing: %s", raw)
	}
}

// accountsDoc is the listing as a caller sees it.
type accountsDoc struct {
	Accounts []struct {
		ID        int     `json:"id"`
		Order     int     `json:"order"`
		Threshold int     `json:"threshold"`
		Percent   float64 `json:"percent"`
	} `json:"accounts"`
	Default int `json:"default"`
}

func (h *harness) accounts() accountsDoc {
	h.t.Helper()
	resp, raw := h.do("GET", "/v1/accounts", nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var doc accountsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		h.t.Fatal(err, string(raw))
	}
	return doc
}

// setRotation is PATCH /v1/accounts/{id}.
func (h *harness) setRotation(id int, body map[string]any) (int, string) {
	h.t.Helper()
	resp, raw := h.do("PATCH", fmt.Sprintf("/v1/accounts/%d", id), body)
	return resp.StatusCode, string(raw)
}

func TestAccountsAreListedInRotationOrderWithTheirSettings(t *testing.T) {
	h := newHarness(t, Options{})
	doc := h.accounts()
	if len(doc.Accounts) != 4 {
		t.Fatalf("%+v", doc)
	}
	// A store written before rotation existed is numbered by id.
	for i, a := range doc.Accounts {
		if a.Order != i+1 || a.Threshold != 100 {
			t.Fatalf("account %d: order %d threshold %d", a.ID, a.Order, a.Threshold)
		}
	}
	if doc.Default != 1 {
		t.Fatalf("the listing says which account a run with no id would use: %+v", doc)
	}

	if code, body := h.setRotation(3, map[string]any{"order": 1, "threshold": 60}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if code, body := h.setRotation(1, map[string]any{"order": 0}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	doc = h.accounts()
	if doc.Accounts[0].ID != 3 || doc.Accounts[0].Threshold != 60 {
		t.Fatalf("the listing follows the rotation: %+v", doc.Accounts)
	}
	if last := doc.Accounts[len(doc.Accounts)-1]; last.ID != 1 || last.Order != 0 {
		t.Fatalf("an account out of the rotation is listed last: %+v", doc.Accounts)
	}
	if doc.Default != 3 {
		t.Fatalf("default: %+v", doc)
	}
}

func TestPatchRefusesSettingsThatCannotMean_Anything(t *testing.T) {
	h := newHarness(t, Options{})
	for _, body := range []map[string]any{
		{"order": -1}, {"order": "sideways"}, {"order": 1.5}, {"order": true}, {"order": "after:1"},
		{"threshold": 0}, {"threshold": 101}, {"threshold": -5}, {},
	} {
		if code, raw := h.setRotation(1, body); code != 400 {
			t.Fatalf("%v was accepted: %d %s", body, code, raw)
		}
	}
	if code, raw := h.setRotation(99, map[string]any{"order": 1}); code != 404 {
		t.Fatalf("%d %s", code, raw)
	}
}

// The same places the command line takes, as a number or a string, and the
// same shifting: the queue is a list, so a PATCH never leaves two accounts
// on one number or a gap between them.
func TestPatchOrderTakesAPlaceAndShiftsTheRest(t *testing.T) {
	h := newHarness(t, Options{})
	queue := func(want ...int) {
		t.Helper()
		doc := h.accounts()
		got := make([]int, 0, len(doc.Accounts))
		for i, a := range doc.Accounts {
			if a.Order > 0 {
				got = append(got, a.ID)
				if a.Order != i+1 {
					t.Fatalf("the queue must read 1, 2, 3: %+v", doc.Accounts)
				}
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("queue %v, want %v", got, want)
		}
	}
	set := func(id int, order any) {
		t.Helper()
		if code, body := h.setRotation(id, map[string]any{"order": order}); code != 200 {
			t.Fatalf("%d -> %v: %d %s", id, order, code, body)
		}
	}
	queue(1, 2, 3, 4)
	set(4, "first")
	queue(4, 1, 2, 3)
	set(1, "down")
	queue(4, 2, 1, 3)
	set(3, "before:4")
	queue(3, 4, 2, 1)
	set(2, 1)
	queue(2, 3, 4, 1)
	set(3, "out")
	queue(2, 4, 1)
	set(3, "after:4")
	queue(2, 4, 3, 1)
	set(2, 99)
	queue(4, 3, 1, 2)

	// up needs a place to move from.
	set(1, 0)
	if code, body := h.setRotation(1, map[string]any{"order": "up"}); code != 400 {
		t.Fatalf("%d %s", code, body)
	}
	queue(4, 3, 2)
}

func TestRunWithNoAccountUsesTheRotation(t *testing.T) {
	h := newHarness(t, Options{})
	resp, raw := h.do("POST", "/v1/run", map[string]any{"prompt": "hi"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var res struct {
		Account int    `json:"account"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Account != 1 {
		t.Fatalf("order 1 answers, and the result says which account did: %s", raw)
	}

	// Take account 1 out of the rotation; the next one answers instead.
	if code, body := h.setRotation(1, map[string]any{"order": 0}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	_, raw = h.do("POST", "/v1/run", map[string]any{"prompt": "hi"})
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Account != 2 {
		t.Fatalf("%s", raw)
	}
}

func TestRunWithNoAccountSaysWhenTheRotationIsEmpty(t *testing.T) {
	h := newHarness(t, Options{})
	for id := 1; id <= 4; id++ {
		if code, raw := h.setRotation(id, map[string]any{"order": 0}); code != 200 {
			t.Fatalf("%d %s", code, raw)
		}
	}
	resp, raw := h.do("POST", "/v1/run", map[string]any{"prompt": "hi"})
	if resp.StatusCode != 409 {
		t.Fatalf("an empty rotation is a conflict, not a 404 or a 500: %d %s", resp.StatusCode, raw)
	}
	// Naming an account still works.
	if code, _, body := h.run(1, map[string]any{"prompt": "hi"}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
}
