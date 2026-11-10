package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/wire"
)

// Made-up credentials in the shapes Claude Code tells apart. None of them is
// anybody's.
const (
	backendHTTPKey   = "sk-ant-api03-TESTONLY-not-a-real-key-0000000000-WXYZ"
	backendHTTPSetup = "sk-ant-oat01-TESTONLY-not-a-real-setup-token-2222"
)

// backendHTTPAccounts is the store seedStore writes when it is given none:
// two claude accounts with made-up tokens, so the next one made is 3.
const backendHTTPAccounts = `{"accounts":[
 {"id":1,"provider":"claude","email":"a@x","uuid":"u1","token":{"accessToken":"tok-1"}},
 {"id":2,"provider":"claude","email":"b@x","uuid":"u2","token":{"accessToken":"tok-2"}}]}`

// seedStore makes a throwaway store holding doc as its accounts.json, or the
// two accounts of backendHTTPAccounts when doc is empty, and returns its
// directory.
func seedStore(t testing.TB, doc string) string {
	t.Helper()
	if doc == "" {
		doc = backendHTTPAccounts
	}
	// Short on purpose, rather than t.TempDir: the store holds a unix
	// socket, whose address has little room, and a per-test directory is
	// named after the test.
	dir, err := os.MkdirTemp("", "rota")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// approvals stands in for claude's token endpoint, as the program's own
// approvalsByCode does: each code in who is approved by the account it names
// ({uuid, email}), and any other code is refused as the real endpoint
// refuses one. The profile endpoint fails, so the identity comes from the
// token's answer alone.
func approvals(t testing.TB, who map[string][2]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/profile") {
			w.WriteHeader(500)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		code, _ := body["code"].(string)
		a, ok := who[code]
		if !ok {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Invalid authorization code"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "NEW-" + a[0], "refresh_token": "R-" + a[0], "expires_in": 3600,
			"account": map[string]string{"uuid": a[0], "email_address": a[1]}})
	}))
	t.Cleanup(srv.Close)
	for _, e := range []*string{&rota.ClaudeEndpoints.Token, &rota.ClaudeEndpoints.Profile} {
		old := *e
		t.Cleanup(func() { *e = old })
	}
	rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile = srv.URL+"/token", srv.URL+"/profile"
}

// backendHTTPCall is one request as the server received it.
type backendHTTPCall struct {
	Method, Path, Body string
}

// backendHTTPRecorder keeps every request that passes it, body included,
// and hands each on unchanged.
type backendHTTPRecorder struct {
	next  http.Handler
	mu    sync.Mutex
	calls []backendHTTPCall
}

func (rec *backendHTTPRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	rec.mu.Lock()
	rec.calls = append(rec.calls, backendHTTPCall{r.Method, r.URL.RequestURI(), string(raw)})
	rec.mu.Unlock()
	rec.next.ServeHTTP(w, r)
}

// last is the latest request to path, whatever its query.
func (rec *backendHTTPRecorder) last(t testing.TB, method, path string) backendHTTPCall {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i := len(rec.calls) - 1; i >= 0; i-- {
		if c := rec.calls[i]; c.Method == method && (c.Path == path || strings.HasPrefix(c.Path, path+"?")) {
			return c
		}
	}
	t.Fatalf("no %s %s among %+v", method, path, rec.calls)
	return backendHTTPCall{}
}

// backendHTTPRig is a real in-process API on a throwaway store, served to
// one principal the way the TUI's own is, with a Backend pointed at it.
type backendHTTPRig struct {
	b   Backend
	rec *backendHTTPRecorder
	dir string
}

func backendHTTPServe(t testing.TB, role api.Role, dir string) *backendHTTPRig {
	t.Helper()
	srv, err := api.New(api.Options{Dir: dir, Token: "t", RefreshEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	rec := &backendHTTPRecorder{next: srv.LocalHandler(&api.Principal{Name: "local", Role: role, Via: "local"})}
	hs := httptest.NewServer(rec)
	// Last of all, so nothing the API opened outlives the test.
	t.Cleanup(srv.Stop)
	t.Cleanup(hs.Close)
	return &backendHTTPRig{b: NewHTTP(hs.Client(), hs.URL), rec: rec, dir: dir}
}

// backendHTTPClaude puts a fake claude first on PATH.
func backendHTTPClaude(t testing.TB, spec fakecli.Spec) {
	t.Helper()
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", spec)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// backendHTTPRefusal is err as the daemon's refusal, or a failed test.
func backendHTTPRefusal(t testing.TB, err error) *client.Refusal {
	t.Helper()
	var no *client.Refusal
	if !errors.As(err, &no) {
		t.Fatalf("want a *client.Refusal, got %T %v", err, err)
	}
	return no
}

func TestWhoSaysTheRole(t *testing.T) {
	ctx := context.Background()
	dir := seedStore(t, "")
	got, err := backendHTTPServe(t, api.RoleControl, dir).b.Who(ctx)
	if err != nil || got != (Principal{Name: "local", Role: "control", Via: "local"}) {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = backendHTTPServe(t, api.RoleWatch, dir).b.Who(ctx)
	if err != nil || got.Role != "watch" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestAccountsReadTheListing(t *testing.T) {
	ctx := context.Background()
	// Account 2 is first in the queue although its id is the higher.
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, `{"ordered":true,"accounts":[
	 {"id":1,"provider":"claude","email":"a@x","uuid":"u1","order":2,"token":{"accessToken":"tok-1"}},
	 {"id":2,"provider":"claude","email":"b@x","uuid":"u2","order":1,"token":{"accessToken":"tok-2"}}]}`))
	l, err := r.b.Accounts(ctx, RefreshNone)
	if err != nil || len(l.Accounts) != 2 || l.Accounts[0].ID != 2 || l.Accounts[1].ID != 1 ||
		l.Accounts[0].Email != "b@x" || l.Default != 2 {
		t.Fatalf("%+v %v", l, err)
	}
	if c := r.rec.last(t, "GET", "/v1/accounts"); c.Path != "/v1/accounts" {
		t.Fatalf("no refresh asked for, and one sent: %s", c.Path)
	}
	// What the server does with them is Task 3's: here only what is asked.
	for how, want := range map[Refresh]string{RefreshForce: "/v1/accounts?refresh=1", RefreshStale: "/v1/accounts?refresh=stale"} {
		if _, err := r.b.Accounts(ctx, how); err != nil {
			t.Fatal(err)
		}
		if c := r.rec.last(t, "GET", "/v1/accounts"); c.Path != want {
			t.Fatalf("%v: %s, want %s", how, c.Path, want)
		}
	}
}

func TestChangeSendsOnlyWhatChanged(t *testing.T) {
	ctx := context.Background()
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, ""))
	a, err := r.b.Change(ctx, 1, Change{Threshold: new(80)})
	if err != nil || a.ID != 1 || a.Threshold != 80 {
		t.Fatalf("%+v %v", a, err)
	}
	if c := r.rec.last(t, "PATCH", "/v1/accounts/1"); c.Body != `{"threshold":80}` {
		t.Fatalf("sent %s", c.Body)
	}
	if _, err := r.b.Change(ctx, 1, Change{ForgetLong: true}); err != nil {
		t.Fatal(err)
	}
	if c := r.rec.last(t, "PATCH", "/v1/accounts/1"); c.Body != `{"long":"forget"}` {
		t.Fatalf("sent %s", c.Body)
	}
	a, err = r.b.Change(ctx, 2, Change{Order: new("first"), RemoteControl: new(false), Cwd: new("")})
	if err != nil || a.ID != 2 || a.Order != 1 {
		t.Fatalf("%+v %v", a, err)
	}
	if c := r.rec.last(t, "PATCH", "/v1/accounts/2"); c.Body != `{"cwd":"","order":"first","remote_control":false}` {
		t.Fatalf("sent %s", c.Body)
	}
}

func TestRefusalsKeepStatusAndReason(t *testing.T) {
	ctx := context.Background()
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, ""))
	if no := backendHTTPRefusal(t, r.b.Remove(ctx, 99)); no.Status != 404 || no.Message == "" {
		t.Fatalf("%+v", no)
	}
	if no := backendHTTPRefusal(t, r.b.SendRun(ctx, "no-such-run", "hi")); no.Status != 404 {
		t.Fatalf("%+v", no)
	}
	if no := backendHTTPRefusal(t, r.b.InterruptRun(ctx, "no-such-run")); no.Status != 404 {
		t.Fatalf("%+v", no)
	}
	// A watcher is told no by the role table, and the Backend says it as
	// the server did.
	watch := backendHTTPServe(t, api.RoleWatch, r.dir)
	if no := backendHTTPRefusal(t, watch.b.Remove(ctx, 1)); no.Status != 403 {
		t.Fatalf("%+v", no)
	}

	// A refusal quoting a key, as a hostile or careless server might.
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"error":"the provider said: bad key %s","reason":"unfinishable"}`, backendHTTPKey)
	}))
	defer hostile.Close()
	_, err := NewHTTP(hostile.Client(), hostile.URL).FinishLogin(ctx, "l-1", "CODE")
	no := backendHTTPRefusal(t, err)
	if no.Status != 400 || no.Reason != "unfinishable" || strings.Contains(no.Message, "TESTONLY") ||
		!strings.Contains(no.Message, "the provider said") || strings.Contains(err.Error(), "TESTONLY") {
		t.Fatalf("%+v", no)
	}
	// An answer that is not rota's own JSON is still a refusal with its
	// status, in a sentence of the Backend's.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		io.WriteString(w, "<html>bad gateway "+backendHTTPKey+"</html>")
	}))
	defer plain.Close()
	_, err = NewHTTP(plain.Client(), plain.URL).Accounts(ctx, RefreshNone)
	if no := backendHTTPRefusal(t, err); no.Status != 502 || no.Message == "" || strings.Contains(no.Message, "TESTONLY") {
		t.Fatalf("%+v", no)
	}
}

// backendHTTPDevice is a device login: approved in a browser, nothing typed,
// and asked about until it is.
type backendHTTPDevice struct{ asked *atomic.Int32 }

var backendHTTPDeviceAsked = &atomic.Int32{}

func (backendHTTPDevice) Name() string { return "t-tui-device" }
func (backendHTTPDevice) Begin(context.Context) (string, map[string]string, error) {
	return "https://example.test/device?user_code=ABCD", map[string]string{"kind": "device"}, nil
}
func (p backendHTTPDevice) Complete(context.Context, string, map[string]string) (*rota.Token, error) {
	if p.asked.Add(1) < 3 {
		return nil, rota.ErrAuthPending
	}
	return &rota.Token{Access: "DEV", Identity: &rota.Identity{UUID: "dev-1"}}, nil
}
func (backendHTTPDevice) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}

func init() { rota.Register(backendHTTPDevice{backendHTTPDeviceAsked}) }

func TestBeginAndFinishALogin(t *testing.T) {
	ctx := context.Background()
	approvals(t, map[string][2]string{"AS13": {"u13", "d@e.f"}})
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, ""))
	l, err := r.b.BeginLogin(ctx, LoginBegin{Provider: "claude"})
	if err != nil || l.ID == "" || l.URL == "" || l.Kind != "code" || l.Provider != "claude" {
		t.Fatalf("%+v %v", l, err)
	}
	if c := r.rec.last(t, "POST", "/v1/login"); c.Body != `{"provider":"claude"}` {
		t.Fatalf("sent %s", c.Body)
	}
	_, err = r.b.FinishLogin(ctx, l.ID, "WRONG")
	// The plan wants 400. The API at this base says a code the provider
	// rejects as 500 "internal error" (an OAuthError of no kind), which is
	// the server's to change; either way it is a refusal with its status.
	if no := backendHTTPRefusal(t, err); no.Status != 400 && no.Status != 500 {
		t.Fatalf("%d %+v", no.Status, no)
	}
	// The login stays parked after a wrong code, and the right one ends it.
	end, err := r.b.FinishLogin(ctx, l.ID, "AS13")
	if err != nil || end != (LoginEnd{Account: 3, Provider: "claude", Email: "d@e.f", Status: "added"}) {
		t.Fatalf("%+v %v", end, err)
	}
	if c := r.rec.last(t, "POST", "/v1/login/"+l.ID); c.Body != `{"code":"AS13"}` {
		t.Fatalf("sent %s %s", c.Path, c.Body)
	}

	backendHTTPDeviceAsked.Store(0)
	d, err := r.b.BeginLogin(ctx, LoginBegin{Provider: "t-tui-device"})
	if err != nil || d.Kind != "device" || d.URL == "" {
		t.Fatalf("%+v %v", d, err)
	}
	for range 2 {
		if end, err := r.b.FinishLogin(ctx, d.ID, ""); err != nil || end != (LoginEnd{Pending: true}) {
			t.Fatalf("%+v %v", end, err)
		}
	}
	if end, err := r.b.FinishLogin(ctx, d.ID, ""); err != nil ||
		end != (LoginEnd{Account: 4, Provider: "t-tui-device", Status: "added"}) {
		t.Fatalf("%+v %v", end, err)
	}
}

func TestBeginForAnAccountSendsTheAccount(t *testing.T) {
	ctx := context.Background()
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, `{"accounts":[
	 {"id":12,"provider":"claude","email":"a@x","uuid":"u12","token":{"accessToken":"tok-12"}}]}`))
	l, err := r.b.BeginLogin(ctx, LoginBegin{Account: 12})
	if err != nil || l.Account != 12 || l.Kind != "code" || l.ID == "" {
		t.Fatalf("%+v %v", l, err)
	}
	if c := r.rec.last(t, "POST", "/v1/login"); c.Body != `{"account":12}` {
		t.Fatalf("sent %s", c.Body)
	}
	if _, err := r.b.BeginLogin(ctx, LoginBegin{Provider: "claude", Long: true}); err != nil {
		t.Fatal(err)
	}
	if c := r.rec.last(t, "POST", "/v1/login"); c.Body != `{"long":true,"provider":"claude"}` {
		t.Fatalf("sent %s", c.Body)
	}
}

func TestPastedSecretsNeverComeBack(t *testing.T) {
	ctx := context.Background()
	var refuse atomic.Bool
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if refuse.Load() {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	defer console.Close()
	old := rota.ClaudeModelsEndpoint
	rota.ClaudeModelsEndpoint = console.URL
	defer func() { rota.ClaudeModelsEndpoint = old }()
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, ""))
	// Error() is a refusal's Message alone, so its reason and holder are
	// looked at by themselves.
	clean := func(what string, v ...any) {
		t.Helper()
		s := fmt.Sprintf("%+v", v)
		for _, x := range v {
			var no *client.Refusal
			if err, ok := x.(error); ok && errors.As(err, &no) {
				s += fmt.Sprintf(" %#v", *no)
			}
		}
		if strings.Contains(s, "TESTONLY") {
			t.Fatalf("%s: a credential came back: %s", what, s)
		}
	}

	end, err := r.b.PasteKey(ctx, backendHTTPKey, "")
	clean("key", end, err)
	if err != nil || end.Status != "added" || end.Account != 3 || end.Provider != "claude" {
		t.Fatalf("%+v %v", end, err)
	}
	if c := r.rec.last(t, "POST", "/v1/login"); c.Body != `{"key":"`+backendHTTPKey+`"}` {
		t.Fatalf("sent %s", c.Body)
	}
	refuse.Store(true)
	end, err = r.b.PasteKey(ctx, backendHTTPKey, "work")
	clean("refused key", end, err)
	if err == nil {
		t.Fatalf("a key the console refuses was taken: %+v", end)
	}
	_, err = r.b.PasteKey(ctx, backendHTTPSetup, "")
	clean("a setup token as a key", err)
	backendHTTPRefusal(t, err)

	end, err = r.b.PasteToken(ctx, 1, backendHTTPSetup)
	clean("token", end, err)
	if err != nil || end.Status != "long" || end.Account != 1 || end.LongUntil == "" || end.Warning == "" {
		t.Fatalf("%+v %v", end, err)
	}
	for _, c := range []struct {
		account int
		token   string
	}{{1, backendHTTPKey}, {99, backendHTTPSetup}, {3, backendHTTPSetup}} {
		end, err := r.b.PasteToken(ctx, c.account, c.token)
		clean("refused token", end, err)
		backendHTTPRefusal(t, err)
	}
}

func TestAskStreamsEvents(t *testing.T) {
	backendHTTPClaude(t, fakecli.Lines(
		`{"type":"system","subtype":"init","session_id":"s-1","model":"m"}`,
		`{"type":"assistant","session_id":"s-1","message":{"content":[{"type":"text","text":"four"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-1","result":"four","num_turns":1}`))
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, ""))
	var seen []message.Event
	end, err := r.b.Ask(context.Background(), Question{Account: 1, Prompt: "2+2?"}, func(e message.Event) { seen = append(seen, e) })
	if err != nil || end.Type != "done" || end.ExitCode != 0 || end.Account != 1 {
		t.Fatalf("%+v %v\n%+v", end, err, seen)
	}
	said := false
	for _, e := range seen {
		said = said || e.Type == "text" && e.Text == "four"
	}
	if !said || seen[0].Type != "init" {
		t.Fatalf("%+v", seen)
	}
	if c := r.rec.last(t, "POST", "/v1/accounts/1/run"); c.Body != `{"prompt":"2+2?","stream":true}` {
		t.Fatalf("sent %s", c.Body)
	}
}

func TestAnOpenAskTakesMoreMessages(t *testing.T) {
	backendHTTPClaude(t, fakecli.Spec{Echo: true})
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, ""))
	ctx := context.Background()
	events := make(chan message.Event, 256)
	type ended struct {
		end wire.End
		err error
	}
	done := make(chan ended, 1)
	go func() {
		end, err := r.b.Ask(ctx, Question{Prompt: "hello", Input: true, With: []string{"blocks"}}, func(e message.Event) { events <- e })
		done <- ended{end, err}
	}()
	next := func(want func(message.Event) bool) message.Event {
		t.Helper()
		for {
			select {
			case e := <-events:
				if want(e) {
					return e
				}
			case d := <-done:
				t.Fatalf("the run ended first: %+v %v", d.end, d.err)
			case <-time.After(15 * time.Second):
				t.Fatal("nothing more arrived")
			}
		}
	}
	run := next(func(e message.Event) bool { return e.Type == "init" }).RunID
	if run == "" {
		t.Fatal("the opening event names no run")
	}
	next(func(e message.Event) bool { return e.Type == "text" && e.Text == "echo: hello" })
	if err := r.b.SendRun(ctx, run, "again"); err != nil {
		t.Fatal(err)
	}
	if c := r.rec.last(t, "POST", "/v1/runs/"+run+"/messages"); c.Body != `{"text":"again"}` {
		t.Fatalf("sent %s %s", c.Path, c.Body)
	}
	next(func(e message.Event) bool { return e.Type == "text" && e.Text == "echo: again" })
	if err := r.b.CloseRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-done:
		if d.err != nil || d.end.Type != "done" || d.end.Account != 1 {
			t.Fatalf("%+v %v", d.end, d.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("closing the run did not end the answer")
	}
	if c := r.rec.last(t, "POST", "/v1/run"); c.Body != `{"input":true,"prompt":"hello","stream":true,"with":["blocks"]}` {
		t.Fatalf("sent %s %s", c.Path, c.Body)
	}
}

func TestAStreamCutShortIsAnError(t *testing.T) {
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, `{"type":"init","account":1}`+"\n"+`{"type":"error","text":"a turn failed"}`+"\n")
	}))
	defer cut.Close()
	var seen []message.Event
	_, err := NewHTTP(cut.Client(), cut.URL).Ask(context.Background(), Question{Prompt: "p"}, func(e message.Event) { seen = append(seen, e) })
	// A CLI's own error event is an event; only the stream's last word,
	// which carries the exit code, ends it.
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(seen) != 2 || seen[1].Type != "error" {
		t.Fatalf("%v %+v", err, seen)
	}
}

// The heartbeat an open run's stream sends is the transport's: nobody
// reading events sees it.
func TestAStreamsHeartbeatIsNoEvent(t *testing.T) {
	beat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, `{"type":"init","account":1,"run_id":"r1"}`+"\n"+`{"type":"ping"}`+"\n"+
			`{"type":"done","exit_code":0,"is_error":false,"account":1}`+"\n")
	}))
	defer beat.Close()
	var seen []string
	end, err := NewHTTP(beat.Client(), beat.URL).Ask(context.Background(), Question{Prompt: "p", Input: true},
		func(e message.Event) { seen = append(seen, e.Type) })
	if err != nil || end.Type != "done" || strings.Join(seen, ",") != "init" {
		t.Fatalf("%+v %v %v", end, err, seen)
	}
}

// A run that cannot start ends with the run's own error, not as a cut
// stream: here no claude is on PATH, which TestMain left empty.
func TestAnAskThatCannotStartEndsWithItsError(t *testing.T) {
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, ""))
	var seen []string
	end, err := r.b.Ask(context.Background(), Question{Account: 1, Prompt: "p"}, func(e message.Event) { seen = append(seen, e.Type) })
	if err != nil || end.Type != "error" || end.Error == "" {
		t.Fatalf("%+v %v %v", end, err, seen)
	}
}

// The error a run ends with has any key in it taken out, as a refusal's
// sentence has.
func TestARunsErrorEndHoldsNoKey(t *testing.T) {
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintf(w, `{"type":"error","exit_code":0,"is_error":false,"error":"bad key %s"}`+"\n", backendHTTPKey)
	}))
	defer leak.Close()
	end, err := NewHTTP(leak.Client(), leak.URL).Ask(context.Background(), Question{Prompt: "p"}, nil)
	if err != nil || end.Type != "error" || !strings.Contains(end.Error, "bad key") || strings.Contains(end.Error, "TESTONLY") {
		t.Fatalf("%+v %v", end, err)
	}
}

// What the person typed is taken out of a refusal that quotes it, whatever
// its shape and wherever it is quoted, but a value too short to be a code is
// left alone rather than cut out of every word it occurs in.
func TestATypedValueIsNotRepeated(t *testing.T) {
	const code = "my-plain-code-123"
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"error":"code %s was refused","reason":"bad:%s","holder":"%s"}`, code, code, code)
	}))
	defer hostile.Close()
	b := NewHTTP(hostile.Client(), hostile.URL)
	for _, typed := range []string{code, " " + code + "\n"} {
		_, err := b.FinishLogin(context.Background(), "l-1", typed)
		no := backendHTTPRefusal(t, err)
		if no.Message != "code [not repeated] was refused" || no.Reason != "bad:[not repeated]" || no.Holder != "[not repeated]" {
			t.Fatalf("%q: %#v", typed, *no)
		}
	}
	_, err := b.FinishLogin(context.Background(), "l-1", "e")
	if no := backendHTTPRefusal(t, err); no.Message != "code "+code+" was refused" || no.Reason != "bad:"+code {
		t.Fatalf("one letter garbled the sentence: %#v", *no)
	}
}

func TestACallThatHangsEndsAtItsDeadline(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewHTTP(hang.Client(), hang.URL).Accounts(ctx, RefreshNone)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}
