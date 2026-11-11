//go:build linux || darwin

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/cmd/rota/notify"
	"github.com/professor93/rota/internal/fakecli"
)

// The daemon's sinks as a person configures them: a webhook on a loopback
// httptest server, signed with a key from a file of the owner's, and a
// command sink whose program is a fake that keeps what it was handed. The
// daemon, each holder and each command are this test binary in processes of
// their own, on the daemon rig; nothing here leaves this machine.

/* --------------------------------------------------------------- the rig --- */

// sinkGot is one event as a sink received it.
type sinkGot struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Exit   *int   `json:"exit"`
	Signed bool   `json:"-"` // the webhook's signature was right
}

// hook is a webhook receiver on loopback. Each request is answered after
// delay, and counted as heard just before it is answered.
type hook struct {
	t     *testing.T
	srv   *httptest.Server
	key   string
	delay time.Duration
	mu    sync.Mutex
	got   []sinkGot
}

func newHook(t *testing.T, key string, delay time.Duration) *hook {
	h := &hook{t: t, key: key, delay: delay}
	h.srv = httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, _ := io.ReadAll(r.Body)
		var e sinkGot
		_ = json.Unmarshal(body, &e)
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write(body)
		e.Signed = r.Header.Get("X-Rota-Signature") == "sha256="+hex.EncodeToString(mac.Sum(nil)) &&
			r.Header.Get("X-Rota-Event") == e.Type
		time.Sleep(h.delay)
		h.mu.Lock()
		h.got = append(h.got, e)
		h.mu.Unlock()
		w.WriteHeader(nethttp.StatusNoContent)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// find is the first event of typ (and name, where name is not "") heard.
func (h *hook) find(typ, name string) (sinkGot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.got {
		if e.Type == typ && (name == "" || e.Name == name) {
			return e, true
		}
	}
	return sinkGot{}, false
}

func (h *hook) wait(typ, name string) sinkGot {
	h.t.Helper()
	within(h.t, "the webhook hears "+typ+" "+name, 30*time.Second, func() bool { _, ok := h.find(typ, name); return ok })
	e, _ := h.find(typ, name)
	return e
}

// fileWatch keeps each event a command sink's program wrote to its file:
// the fake writes what it was handed over the last one, so it is read often.
type fileWatch struct {
	t    *testing.T
	path string
	mu   sync.Mutex
	got  []sinkGot
	last string
}

func watchFile(t *testing.T, path string) *fileWatch {
	f := &fileWatch{t: t, path: path}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) == f.last {
				continue
			}
			var e sinkGot
			if json.Unmarshal(raw, &e) != nil {
				continue
			}
			f.mu.Lock()
			f.last = string(raw)
			f.got = append(f.got, e)
			f.mu.Unlock()
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
	return f
}

func (f *fileWatch) find(typ, name string) (sinkGot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.got {
		if e.Type == typ && (name == "" || e.Name == name) {
			return e, true
		}
	}
	return sinkGot{}, false
}

func (f *fileWatch) wait(typ, name string) sinkGot {
	f.t.Helper()
	within(f.t, "the command is handed "+typ+" "+name, 30*time.Second, func() bool { _, ok := f.find(typ, name); return ok })
	e, _ := f.find(typ, name)
	return e
}

// sinkRig is the daemon rig with a server.toml naming a webhook and a
// command sink, and, when broken, a webhook to a port nobody listens on.
type sinkRig struct {
	*daemonRig
	hook    *hook
	key     string
	prog    *fileWatch
	cmdFile string
}

type sinkOpts struct {
	delay  time.Duration // the webhook's answer
	path   string        // the webhook's URL path
	broken string        // the path of a webhook to a closed port; "" for none
}

const hookKey = "HMACKEY-5f1d0c9a7e"

func newSinkRig(t *testing.T, o sinkOpts) *sinkRig {
	t.Helper()
	r := newDaemonRig(t)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "hook.secret")
	if err := os.WriteFile(keyFile, []byte(hookKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmdFile := filepath.Join(dir, "handed.json")
	prog := fakecli.Install(t, t.TempDir(), "rota-notify-fake", fakecli.Spec{StdinTo: cmdFile})
	h := newHook(t, hookKey, o.delay)
	path := o.path
	if path == "" {
		path = "/hook"
	}
	body := fmt.Sprintf("[[events.sink]]\ntype = \"webhook\"\nurl = %q\nsecret_file = %q\n\n"+
		"[[events.sink]]\ntype = \"command\"\non = [\"session.*\"]\ncommand = [%q]\n", h.srv.URL+path, keyFile, prog)
	if o.broken != "" {
		body += fmt.Sprintf("\n[[events.sink]]\ntype = \"webhook\"\nurl = %q\n", "http://"+closedPort(t)+o.broken)
	}
	if err := os.WriteFile(filepath.Join(r.home, "server.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &sinkRig{daemonRig: r, hook: h, key: hookKey, prog: watchFile(t, cmdFile), cmdFile: cmdFile}
}

// closedPort is a loopback address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// sinks is sinks.json as the daemon keeps it.
func (r *daemonRig) sinks() []notify.Status {
	f, err := notify.ReadStatus(r.terminals)
	if err != nil || f == nil {
		return nil
	}
	return f.Sinks
}

func (r *daemonRig) sinkState(name string) notify.Status {
	for _, s := range r.sinks() {
		if s.Name == name {
			return s
		}
	}
	return notify.Status{}
}

// statusRaw is serve status --json as keys and values.
func (r *daemonRig) statusRaw() (map[string]json.RawMessage, int) {
	r.t.Helper()
	out, errb, code := r.rota("--json", "serve", "status")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		r.t.Fatalf("status --json: exit %d %q %q: %v", code, out, errb, err)
	}
	return doc, code
}

func eventLines(out string) []string {
	var lines []string
	for l := range strings.Lines(out) {
		if strings.HasPrefix(l, "events: ") {
			lines = append(lines, strings.TrimSuffix(l, "\n"))
		}
	}
	return lines
}

/* ------------------------------------------------------------- the tests --- */

// The daemon sends what happens to its sinks: daemon.started to the
// webhook, signed; a detached session's start to both, by the session's
// name; and its end, with the CLI's exit, to the command.
func TestTheDaemonPushesItsEvents(t *testing.T) {
	r := newSinkRig(t, sinkOpts{})
	r.start()
	if e := r.hook.wait("daemon.started", ""); !e.Signed {
		t.Fatalf("daemon.started came unsigned, or signed wrongly: %+v", e)
	}
	if !strings.Contains(r.log(), "events: 2 sinks: webhook, command") {
		t.Fatalf("the log does not name the sinks:\n%s", r.log())
	}
	r.detach(r.cmd("run", "1", "--detach", "--label", "e2e"))
	if e := r.hook.wait("session.started", "e2e"); !e.Signed {
		t.Fatalf("session.started came unsigned: %+v", e)
	}
	r.prog.wait("session.started", "e2e")
	if out, errb, code := r.rota("send", "e2e", "bye"); code != 0 {
		t.Fatalf("send: exit %d %q %q", code, out, errb)
	}
	if e := r.prog.wait("session.ended", "e2e"); e.Exit == nil || *e.Exit != 0 {
		t.Fatalf("session.ended says exit %v", e.Exit)
	}
	if _, ok := r.prog.find("daemon.started", ""); ok {
		t.Fatal("the command, on session.*, was handed daemon.started")
	}
}

// Owner ruling Q2: a daemon --detach starts on its own runs the sinks too,
// and the line saying it started names them.
func TestAnAutoStartedDaemonSendsToo(t *testing.T) {
	r := newSinkRig(t, sinkOpts{})
	_, _, errb := r.detach(r.cmd("run", "1", "--detach", "--label", "auto"))
	said := "rota: started the rota daemon for " + r.home + " (control socket only; rota serve stop ends it); events to webhook, command\n"
	if !strings.HasPrefix(errb, said) {
		t.Fatalf("stderr is %q; want it to start %q", errb, said)
	}
	r.hook.wait("daemon.started", "")
	r.hook.wait("session.started", "auto")
	r.prog.wait("session.started", "auto")
}

// serve status has a line for each sink, and its JSON their status; --test
// sends each a test event and says what each did; a sink it does not have
// is refused, naming those it has.
func TestServeStatusShowsAndTestsSinks(t *testing.T) {
	r := newSinkRig(t, sinkOpts{})
	r.start()
	r.hook.wait("daemon.started", "")
	within(t, "sinks.json says the webhook sent", 10*time.Second, func() bool { return r.sinkState("webhook").Sent > 0 })

	out, errb, code := r.rota("serve", "status")
	lines := eventLines(out)
	if code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "events: webhook ok, ") || lines[1] != "events: command waiting" {
		t.Fatalf("serve status: exit %d %q %q", code, out, errb)
	}
	doc, code := r.statusRaw()
	var sinks []notify.Status
	if err := json.Unmarshal(doc["sinks"], &sinks); code != 0 || err != nil || len(sinks) != 2 ||
		sinks[0].Name != "webhook" || sinks[0].State != "ok" || sinks[1].Name != "command" || sinks[1].Target != "rota-notify-fake" {
		t.Fatalf("status --json: exit %d sinks %s (%v)", code, doc["sinks"], err)
	}

	out, errb, code = r.rota("serve", "status", "--test", "all")
	if code != 0 || out != "webhook: delivered\ncommand: delivered\n" {
		t.Fatalf("--test all: exit %d %q %q", code, out, errb)
	}
	if _, ok := r.hook.find("test", ""); !ok {
		t.Fatal("the webhook sinkGot no test")
	}
	if _, ok := r.prog.find("test", ""); !ok {
		t.Fatal("the command was handed no test, though a test goes to every sink whatever its on")
	}

	out, errb, code = r.rota("--json", "serve", "status", "--test", "command")
	var outcomes []map[string]any
	if err := json.Unmarshal([]byte(out), &outcomes); code != 0 || err != nil || len(outcomes) != 1 ||
		!reflect.DeepEqual(outcomes[0], map[string]any{"sink": "command", "answered": true, "ok": true}) {
		t.Fatalf("--json --test command: exit %d %q %q", code, out, errb)
	}

	out, errb, code = r.rota("serve", "status", "--test", "nope")
	if code != 1 || out != "" || !strings.Contains(errb, `"nope"`) || !strings.Contains(errb, "webhook, command") {
		t.Fatalf("--test nope: exit %d %q %q", code, out, errb)
	}
}

// Review Focus 5: serve stop returns only once daemon.stopped has been
// answered, the drain running while the daemon still holds the store; a
// webhook that takes a second over each answer still lets it stop within
// daemonStopWait.
func TestStopDeliversDaemonStopped(t *testing.T) {
	r := newSinkRig(t, sinkOpts{delay: time.Second})
	r.start()
	r.hook.wait("daemon.started", "")
	began := time.Now()
	out, errb, code := r.rota("serve", "stop")
	took := time.Since(began)
	if code != 0 || out != "rota daemon stopped; 0 sessions keep running\n" {
		t.Fatalf("serve stop: exit %d %q %q", code, out, errb)
	}
	if _, ok := r.hook.find("daemon.stopped", ""); !ok {
		t.Fatalf("serve stop returned after %s, before the webhook had answered daemon.stopped:\n%s", took, r.log())
	}
	if took >= daemonStopWait {
		t.Fatalf("serve stop took %s", took)
	}
	if strings.Contains(r.log(), "events were still being sent") {
		t.Fatalf("the drain ran out:\n%s", r.log())
	}
}

// Review Focus 1: a webhook nobody answers is failing, and holds up neither
// the other sinks, nor the session, nor the stop.
func TestABrokenSinkHurtsNothing(t *testing.T) {
	r := newSinkRig(t, sinkOpts{broken: "/dead"})
	r.start()
	id, _, _ := r.detach(r.cmd("run", "1", "--detach", "--label", "e2e"))
	r.hook.wait("session.started", "e2e")
	r.prog.wait("session.started", "e2e")
	within(t, "sinks.json says webhook-2 is failing", 20*time.Second, func() bool { return r.sinkState("webhook-2").State == "failing" })

	out, errb, code := r.rota("serve", "status")
	lines := eventLines(out)
	if code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[2], "events: webhook-2 failing since ") {
		t.Fatalf("serve status: exit %d %q %q", code, out, errb)
	}
	if s := r.session(id); s.State != "live" {
		t.Fatalf("the session is %q", s.State)
	}
	if out, errb, code := r.rota("serve", "stop"); code != 0 {
		t.Fatalf("serve stop: exit %d %q %q", code, out, errb)
	}
	r.hook.wait("daemon.stopped", "")
	if strings.Contains(r.log(), "events were still being sent") {
		t.Fatalf("a dead sink held the stop up:\n%s", r.log())
	}
}

// Review Focus 2: neither the webhook's address, whose path holds a secret,
// nor its key reaches daemon.log, sinks.json or serve status, from a sink
// that delivers or from one that fails.
func TestTheDaemonLogHoldsNoSinkSecret(t *testing.T) {
	r := newSinkRig(t, sinkOpts{path: "/hook/XYZSECRET", broken: "/XYZSECRET/dead"})
	r.start()
	r.hook.wait("daemon.started", "")
	within(t, "sinks.json says webhook-2 is failing", 20*time.Second, func() bool { return r.sinkState("webhook-2").State == "failing" })
	text, _, _ := r.rota("serve", "status")
	doc, _ := r.statusRaw()
	sinks, err := os.ReadFile(notify.StatusPath(r.terminals))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, code := r.rota("serve", "stop"); code != 0 {
		t.Fatal("serve stop failed")
	}
	if !strings.Contains(r.log(), "webhook-2") {
		t.Fatalf("the log never said the broken sink failed:\n%s", r.log())
	}
	for what, s := range map[string]string{"daemon.log": r.log(), "sinks.json": string(sinks), "serve status": text, "serve status --json": string(doc["sinks"])} {
		for _, secret := range []string{"XYZSECRET", hookKey} {
			if strings.Contains(s, secret) {
				t.Errorf("%s holds %q:\n%s", what, secret, s)
			}
		}
	}
}

// A daemon whose file has no [events] is the daemon it was: no sinks.json,
// a stale one taken away, not a word of events in its log or its status,
// and --test says there is nothing to test.
func TestNoSinksMeansNothingNew(t *testing.T) {
	r := newDaemonRig(t)
	if out, errb, code := r.rota("serve", "status", "--test", "all"); code != 3 || out != "no rota daemon is running for "+r.home+"\n" {
		t.Fatalf("--test with no daemon: exit %d %q %q", code, out, errb)
	}
	stale := notify.StatusPath(r.terminals)
	if err := os.WriteFile(stale, []byte(`{"pid":1,"sinks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.detach(r.cmd("run", "1", "--detach", "--label", "plain"))
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("sinks.json is there: %v", err)
	}
	out, errb, code := r.rota("serve", "status")
	if code != 0 || len(eventLines(out)) != 0 {
		t.Fatalf("serve status: exit %d %q %q", code, out, errb)
	}
	doc, _ := r.statusRaw()
	if _, ok := doc["sinks"]; ok {
		t.Fatalf("status --json has sinks: %s", doc["sinks"])
	}
	out, errb, code = r.rota("serve", "status", "--test", "all")
	if code != 1 || out != "" || errb != "error: no event sinks are running; [[events.sink]] in server.toml adds one\n" {
		t.Fatalf("--test with no sinks: exit %d %q %q", code, out, errb)
	}
	if _, _, code := r.rota("serve", "stop"); code != 0 {
		t.Fatal("serve stop failed")
	}
	if strings.Contains(r.log(), "events:") || slices.ContainsFunc(strings.Split(r.log(), "\n"), func(l string) bool {
		return strings.Contains(l, "sinks.json")
	}) {
		t.Fatalf("the log speaks of events:\n%s", r.log())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("sinks.json is there after the stop: %v", err)
	}
}
