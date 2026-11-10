//go:build linux || darwin

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// changeAsked is one PATCH as the daemon stand-in saw it.
type changeAsked struct {
	method, path, body string
}

// changedDoc is a session as the daemon describes it once it has resumed or
// moved: the same id, live, on account 13.
const changedDoc = `{"id":"0123456789abcdef","held":true,"name":"api","label":"api","kind":"account",
	"cwd":"/src/api","started":"2026-11-10T09:00:00Z","ended":false,"state":"live","status":"running",
	"attachable":true,"reachable":true,"pid":77,"conversation":"c1","conversation_cwd":"/src/api",
	"resumable":false,"account":{"id":13,"label":"home@x","provider":"claude"},
	"cols":100,"rows":30,"holder":null,"viewers":[]}`

// A change is PATCH /v1/terminals/{id} with what it was given and nothing
// else: a resume with the size it starts at, a move with its confirm, a
// forget alone. A resume or a move answers the session's description, and a
// forget's {"ok":true} is no session at all.
func TestChangeSendsOneOfResumeMoveOrForget(t *testing.T) {
	dir := terminals(t)
	asked := make(chan changeAsked, 1)
	daemonAt(t, dir, twoSessions, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/terminals/{id}", func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			asked <- changeAsked{r.Method, r.URL.EscapedPath(), string(raw)}
			if strings.Contains(string(raw), `"forget"`) {
				fmt.Fprint(w, `{"ok":true}`)
				return
			}
			fmt.Fprint(w, changedDoc)
		})
	})
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ch   Change
		body string
	}{
		{Change{Resume: true, Cols: 100, Rows: 30}, `{"resume":true,"cols":100,"rows":30}`},
		{Change{Account: 13, Confirm: true}, `{"account":13,"confirm":true}`},
		{Change{Resume: true, Env: []string{"PATH=/bin"}}, `{"resume":true,"env":["PATH=/bin"]}`},
		{Change{Forget: true}, `{"forget":true}`},
	} {
		s, err := c.Change("0123456789abcdef", tc.ch)
		if err != nil {
			t.Fatalf("Change(%+v): %v", tc.ch, err)
		}
		got := <-asked
		if got.method != http.MethodPatch || got.path != "/v1/terminals/0123456789abcdef" || got.body != tc.body {
			t.Fatalf("Change(%+v) sent %s %s %s, want PATCH /v1/terminals/0123456789abcdef %s",
				tc.ch, got.method, got.path, got.body, tc.body)
		}
		if tc.ch.Forget {
			if !reflect.DeepEqual(s, Session{}) {
				t.Fatalf("a forget answered {\"ok\":true} and gave %+v, want no session", s)
			}
			continue
		}
		if s.ID != "0123456789abcdef" || s.State != "live" || s.Account != 13 || s.AccountLabel != "home@x" ||
			s.Provider != "claude" || s.Conversation != "c1" || s.ConversationCwd != "/src/api" ||
			!s.Reachable || s.Cols != 100 || s.Rows != 30 {
			t.Fatalf("Change(%+v) read %+v", tc.ch, s)
		}
	}
}

// A move that costs something is the daemon asking first: a refusal like
// any other, whose reason, confirm, a program acts on and whose sentence is
// the question to put to the person.
func TestAConfirmIsARefusalWithItsReason(t *testing.T) {
	dir := terminals(t)
	daemonAt(t, dir, twoSessions, func(mux *http.ServeMux) {
		mux.HandleFunc("PATCH /v1/terminals/{id}", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"Move x to #13 h?","reason":"confirm","warnings":["turn"]}`)
		})
	})
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Change("0123456789abcdef", Change{Account: 13})
	var no *Refusal
	if !errors.As(err, &no) || *no != (Refusal{Status: http.StatusConflict, Reason: "confirm", Message: "Move x to #13 h?"}) ||
		err.Error() != "Move x to #13 h?" {
		t.Fatalf("a move asked about: %#v", err)
	}
	if !reflect.DeepEqual(s, Session{}) {
		t.Fatalf("a refused change gave %+v", s)
	}
}

// A change waits as long as a move takes (M3): longer than a create or a
// send is given, and longer than the TUI's 100 seconds for a move, so it is
// never the client that gives up on a move the daemon is still making. With
// its wait cut to 300 ms, an answer at 200 ms is waited for and one at
// 500 ms is the deadline.
func TestAChangeOutwaitsAMove(t *testing.T) {
	if changeWait <= holderWait || changeWait <= 100*time.Second {
		t.Fatalf("a change waits %s, which a move can outlast: a create waits %s, and the TUI a move 100s", changeWait, holderWait)
	}
	dir := terminals(t)
	daemonAt(t, dir, twoSessions, func(mux *http.ServeMux) {
		mux.HandleFunc("PATCH /v1/terminals/{id}", func(w http.ResponseWriter, r *http.Request) {
			after := 200 * time.Millisecond
			if r.PathValue("id") == "slow000000000000" {
				after = 500 * time.Millisecond
			}
			select {
			case <-time.After(after):
				fmt.Fprint(w, changedDoc)
			case <-r.Context().Done():
			}
		})
	})
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	const wait = 300 * time.Millisecond
	if s, err := c.change("0123456789abcdef", Change{Account: 13, Confirm: true}, wait); err != nil || s.ID != "0123456789abcdef" {
		t.Fatalf("an answer inside the wait: %+v %v", s, err)
	}
	s, err := c.change("slow000000000000", Change{Account: 13, Confirm: true}, wait)
	var no *Refusal
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &no) || !reflect.DeepEqual(s, Session{}) {
		t.Fatalf("an answer after the wait: %+v %v, want the deadline", s, err)
	}
}

// A create on a conversation says which one as resume, and a create on none
// says nothing of it.
func TestCreateCarriesResume(t *testing.T) {
	dir := terminals(t)
	got := make(chan map[string]any, 1)
	daemonAt(t, dir, twoSessions, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /v1/terminals", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			got <- body
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, changedDoc)
		})
	})
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(Create{Account: 13, Resume: "c1"}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(<-got); string(raw) != `{"account":13,"resume":"c1"}` {
		t.Fatalf("a create on conversation c1 sent %s", raw)
	}
	if _, err := c.Create(Create{Account: 13}); err != nil {
		t.Fatal(err)
	}
	if body := <-got; body["resume"] != nil || len(body) != 1 {
		t.Fatalf("a create on no conversation sent %v", body)
	}
}

// A session's description says where its conversation lives, what its CLI
// waits for, who holds its keyboard and who watches, at what size it runs,
// who made a holder this rota cannot speak to, whether its holder can be
// reached, and when the login inside lapses. A keyboard nobody holds is
// null, and read as nobody.
func TestSessionReadsWhoWatchesAndWhereItsConversationIs(t *testing.T) {
	dir := terminals(t)
	daemonAt(t, dir, `{"terminals":[
		{"id":"00000000000000a1","held":true,"name":"api","label":"","kind":"account","cwd":"/src",
		 "started":"2026-11-10T09:00:00Z","ended":false,"state":"live","status":"needs_you",
		 "attachable":true,"reachable":true,"pid":4242,"conversation":"c1","conversation_cwd":"/src/.wt/a",
		 "waiting_for":"permission","holder":"alice","made_by":"1.4.0-beta.10",
		 "viewers":[{"name":"alice","role":"control","since":"2026-11-10T09:05:00Z"},
		            {"name":"bob","role":"watch","since":"2026-11-10T09:06:30Z"}],
		 "cols":132,"rows":43,"token_until":"2026-12-01T00:00:00Z",
		 "account":{"id":2,"label":"work@x","provider":"claude"}},
		{"id":"00000000000000b2","held":true,"name":"old","kind":"account","state":"ended","status":"stopped",
		 "ended":true,"reachable":false,"holder":null,"viewers":[]}]}`, nil)
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.List()
	if err != nil || len(all) != 2 {
		t.Fatalf("List: %v %+v", err, all)
	}
	s, old := all[0], all[1]
	at := func(day, hour, min, sec int) time.Time { return time.Date(2026, 11, day, hour, min, sec, 0, time.UTC) }
	if s.ConversationCwd != "/src/.wt/a" || s.WaitingFor != "permission" || s.Holder != "alice" ||
		s.MadeBy != "1.4.0-beta.10" || !s.Reachable || s.Cols != 132 || s.Rows != 43 ||
		!s.TokenUntil.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the live one: %+v", s)
	}
	if len(s.Viewers) != 2 ||
		s.Viewers[0].Name != "alice" || s.Viewers[0].Role != "control" || !s.Viewers[0].Since.Equal(at(10, 9, 5, 0)) ||
		s.Viewers[1].Name != "bob" || s.Viewers[1].Role != "watch" || !s.Viewers[1].Since.Equal(at(10, 9, 6, 30)) {
		t.Fatalf("who watches the live one: %+v", s.Viewers)
	}
	if old.Holder != "" || old.Reachable || len(old.Viewers) != 0 || old.Cols != 0 || old.Rows != 0 ||
		!old.TokenUntil.IsZero() || old.ConversationCwd != "" || old.WaitingFor != "" || old.MadeBy != "" {
		t.Fatalf("the stopped one: %+v", old)
	}
}

// A session made in Go, rather than read from the daemon, is written as a
// description document that reads back as the very same session, the new
// fields with the old; nobody at the keyboard is written as null.
func TestAMadeSessionWritesTheNewFields(t *testing.T) {
	ended := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	exit := 0
	for _, made := range []Session{
		{
			ID: "00000000000000a1", Name: "api", Title: "Fix the tests", Label: "api", Kind: "account",
			State: "live", Status: "needs_you", Provider: "claude", Account: 2, AccountLabel: "work@x",
			Cwd: "/src", Conversation: "c1", ConversationCwd: "/src/.wt/a", PID: 4242,
			Attachable: true, Started: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC),
			WaitingFor: "permission", Holder: "alice", MadeBy: "1.4.0-beta.10", Reachable: true,
			Viewers: []SessionViewer{
				{Name: "alice", Role: "control", Since: time.Date(2026, 11, 10, 9, 5, 0, 0, time.UTC)},
				{Name: "bob", Role: "watch", Since: time.Date(2026, 11, 10, 9, 6, 30, 500, time.UTC)},
			},
			Cols: 132, Rows: 43, TokenUntil: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			ID: "00000000000000b2", Name: "old", Kind: "account", State: "ended", Status: "stopped",
			Conversation: "c2", Resumable: true, Started: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC),
			Ended: &ended, Exit: &exit,
		},
	} {
		raw, err := json.Marshal(made)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		wantHolder := any(nil)
		if made.Holder != "" {
			wantHolder = made.Holder
		}
		if h, ok := doc["holder"]; !ok || h != wantHolder {
			t.Fatalf("%s is written with holder %v (present %v), want %v: %s", made.ID, h, ok, wantHolder, raw)
		}
		for _, key := range []string{"conversation_cwd", "reachable", "viewers"} {
			if _, ok := doc[key]; !ok {
				t.Fatalf("%s is written without %s: %s", made.ID, key, raw)
			}
		}
		if made.Holder != "" {
			for _, key := range []string{"waiting_for", "made_by", "cols", "rows", "token_until"} {
				if _, ok := doc[key]; !ok {
					t.Fatalf("%s is written without %s: %s", made.ID, key, raw)
				}
			}
		}
		var back Session
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		back.doc = nil
		if !reflect.DeepEqual(back, made) {
			t.Fatalf("%s reads back as\n%+v\nnot\n%+v\nfrom %s", made.ID, back, made, raw)
		}
	}
}
