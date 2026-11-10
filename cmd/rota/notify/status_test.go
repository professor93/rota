package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/events"
	"github.com/professor93/rota/internal/holdwire"
)

// held reports whether this system runs a daemon, and so can claim a store
// and answer on a control socket.
func held() bool { return runtime.GOOS == "linux" || runtime.GOOS == "darwin" }

// sinks.json says how each sink stands and how its last test went, is its
// owner's alone, and is read back only while its daemon runs.
func TestStatusFileSaysWhatHappened(t *testing.T) {
	quick(t)
	good := &fakeSender{}
	bad := &fakeSender{answer: func(context.Context, int, events.Event) error { return dropped(errors.New("410 Gone")) }}
	useSenders(t, map[string]any{"telegram": good, "webhook": bad})
	dir := t.TempDir()
	d := &dialer{plan: []any{newSource(ev(1, events.AccountDead), ev(2, events.Test))}}
	n := startNotifier(t, Options{Terminals: dir, Events: api.EventsSection{Sinks: []api.EventSink{telegramSink, webhookSink}}, Dial: d.dial})
	eventually(t, "both events to be done with", func() bool {
		return statusOf(t, n, "telegram").Sent == 2 && statusOf(t, n, "webhook").Dropped == 2
	})
	n.Wait(time.Now())

	fi, err := os.Stat(StatusPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("sinks.json is %v", fi.Mode().Perm())
	}
	f, err := readFile(StatusPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if f.PID != os.Getpid() || f.Updated.IsZero() || len(f.Sinks) != 2 {
		t.Fatalf("%+v", f)
	}
	tg, hook := f.Sinks[0], f.Sinks[1]
	if tg.Name != "telegram" || tg.Type != "telegram" || tg.Target != "chat 123" || tg.State != stateOK ||
		tg.Sent != 2 || tg.Dropped != 0 || tg.LastSent.IsZero() || tg.LastError != "" || tg.LastTest == nil ||
		tg.LastTest.ID != 2 || !tg.LastTest.OK || tg.LastTest.At.IsZero() {
		t.Fatalf("telegram: %+v %+v", tg, tg.LastTest)
	}
	if hook.Name != "webhook" || hook.Target != "https://hooks.example" || hook.State != stateFailing ||
		hook.Sent != 0 || hook.Dropped != 2 || hook.LastError != "410 Gone" || hook.LastErrorAt.IsZero() ||
		hook.LastTest == nil || hook.LastTest.ID != 2 || hook.LastTest.OK || hook.LastTest.Error != "410 Gone" {
		t.Fatalf("webhook: %+v %+v", hook, hook.LastTest)
	}

	// The keys, as serve status and the web page read them.
	raw, _ := os.ReadFile(StatusPath(dir))
	var doc struct {
		Sinks []map[string]any `json:"sinks"`
	}
	var top map[string]any
	if json.Unmarshal(raw, &top) != nil || json.Unmarshal(raw, &doc) != nil {
		t.Fatalf("sinks.json: %s", raw)
	}
	for _, k := range []string{"pid", "updated", "sinks"} {
		if _, ok := top[k]; !ok {
			t.Fatalf("sinks.json has no %q: %s", k, raw)
		}
	}
	for _, k := range []string{"name", "type", "target", "on", "state", "sent", "dropped", "last_sent", "last_test"} {
		if _, ok := doc.Sinks[0][k]; !ok {
			t.Fatalf("a sink has no %q: %s", k, raw)
		}
	}
	for _, k := range []string{"last_error", "last_error_at"} {
		if _, ok := doc.Sinks[1][k]; !ok {
			t.Fatalf("a failing sink has no %q: %s", k, raw)
		}
	}
	for _, k := range []string{"last_error", "last_error_at"} {
		if _, ok := doc.Sinks[0][k]; ok {
			t.Fatalf("a sink that never failed has %q: %s", k, raw)
		}
	}
	if test := doc.Sinks[1]["last_test"].(map[string]any); test["id"] != float64(2) || test["ok"] != false || test["error"] != "410 Gone" || test["at"] == nil {
		t.Fatalf("last_test: %v", test)
	}

	// Read back only while the daemon that wrote it runs.
	if got, err := ReadStatus(dir); got != nil || err != nil {
		t.Fatalf("with no daemon ReadStatus = %+v, %v", got, err)
	}
	if !held() {
		return
	}
	release, err := holdwire.ClaimDaemon(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	got, err := ReadStatus(dir)
	if err != nil || got == nil || !reflect.DeepEqual(*got, *f) {
		t.Fatalf("ReadStatus = %+v, %v; want %+v", got, err, f)
	}
	if got, err := ReadStatus(t.TempDir()); got != nil || err != nil {
		t.Fatalf("with no file ReadStatus = %+v, %v", got, err)
	}
}

// Describe is the sinks as server.toml has them, off, from the file alone:
// a token file that is not there is not looked for.
func TestDescribeReadsNoFile(t *testing.T) {
	cmd := api.EventSink{Type: "command", Command: []string{"/usr/bin/notify-send", "rota"}, On: []string{"session.*"}}
	hook := api.EventSink{Type: "webhook", URL: "https://hooks.example:8443/services/T0/XYZ?k=v"}
	got := Describe(api.EventsSection{Sinks: []api.EventSink{telegramSink, hook, cmd, telegramSink}})
	want := []Status{
		{Name: "telegram", Type: "telegram", Target: "chat 123", On: []string{}, State: stateOff},
		{Name: "webhook", Type: "webhook", Target: "https://hooks.example:8443", On: []string{}, State: stateOff},
		{Name: "command", Type: "command", Target: "notify-send", On: []string{"session.*"}, State: stateOff},
		{Name: "telegram-2", Type: "telegram", Target: "chat 123", On: []string{}, State: stateOff},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Describe =\n%+v\nwant\n%+v", got, want)
	}
	if got := Describe(api.EventsSection{}); len(got) != 0 {
		t.Fatalf("Describe of no sinks = %+v", got)
	}
}

// fakeDaemon stands in for a daemon on the terminals directory it returns:
// it holds the claim, and answers POST /v1/events with the next of ids.
// Each request's sink goes to the channel.
func fakeDaemon(t *testing.T, ids ...int64) (string, chan string) {
	t.Helper()
	if !held() {
		t.Skip("no daemon runs on " + runtime.GOOS)
	}
	dir, err := os.MkdirTemp("", "n")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := holdwire.SockFits(holdwire.CtlSock(dir)); err != nil {
		t.Skipf("no control socket fits here: %v", err)
	}
	release, err := holdwire.ClaimDaemon(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	ln, err := net.Listen("unix", holdwire.CtlSock(dir))
	if err != nil {
		t.Fatal(err)
	}
	asked := make(chan string, 8)
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Type, Sink string }
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Type != events.Test {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"only a test"}`)
			return
		}
		mu.Lock()
		id := ids[0]
		ids = ids[1:]
		mu.Unlock()
		asked <- in.Sink
		fmt.Fprintf(w, `{"id":%d}`, id)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return dir, asked
}

// Test posts a test event and waits for each sink it went to to say how it
// went, as sinks.json has it; a sink that does not answer in time is said
// to have not answered, and a sink nobody has is refused before anything is
// posted.
func TestTestWaitsForEachAnswer(t *testing.T) {
	dir, asked := fakeDaemon(t, 7, 8)
	write := func(tests ...*TestResult) {
		t.Helper()
		f := StatusFile{PID: os.Getpid(), Updated: time.Now().UTC(), Sinks: []Status{
			{Name: "a", Type: "telegram", State: stateOK, LastTest: tests[0]},
			{Name: "b", Type: "webhook", State: stateOK, LastTest: tests[1]},
		}}
		if err := writeFile(StatusPath(dir), f); err != nil {
			t.Error(err)
		}
	}
	write(nil, nil)
	c, err := client.Dial(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Test(c, dir, "x", time.Second); err == nil || err.Error() != `no event sink is called "x"; the sinks are a, b` {
		t.Fatalf("an unknown sink: %v", err)
	}
	select {
	case s := <-asked:
		t.Fatalf("a test for an unknown sink was posted, for %q", s)
	default:
	}

	go func() {
		<-asked
		time.Sleep(100 * time.Millisecond)
		write(&TestResult{ID: 7, OK: true, At: time.Now().UTC()}, &TestResult{ID: 7, Error: "refused", At: time.Now().UTC()})
	}()
	out, err := Test(c, dir, "", time.Second)
	want := []Outcome{{Sink: "a", Answered: true, OK: true}, {Sink: "b", Answered: true, Error: "refused"}}
	if err != nil || !slices.Equal(out, want) {
		t.Fatalf("Test = %+v, %v; want %+v", out, err, want)
	}

	began := time.Now()
	out, err = Test(c, dir, "a", 150*time.Millisecond)
	if err != nil || !slices.Equal(out, []Outcome{{Sink: "a"}}) || time.Since(began) < 150*time.Millisecond {
		t.Fatalf("Test with nobody answering = %+v, %v after %s", out, err, time.Since(began))
	}
	if s := <-asked; s != "a" {
		t.Fatalf("the test was posted for %q", s)
	}
}

// Test with no daemon keeping sinks.json says there is no sink to test.
func TestTestWithNoSinks(t *testing.T) {
	dir, _ := fakeDaemon(t, 1)
	c, err := client.Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Test(c, dir, "", time.Second); err == nil || err.Error() != "the rota daemon sends events to no sink" {
		t.Fatalf("Test = %v", err)
	}
}

// A sink's target says where it sends without a secret: the chat, the
// webhook's scheme and host, or the program's name.
func TestTargetOf(t *testing.T) {
	for _, c := range []struct {
		s    api.EventSink
		want string
	}{
		{api.EventSink{Type: "telegram", ChatID: " -100123 "}, "chat -100123"},
		{api.EventSink{Type: "webhook", URL: "https://user:pw@h.example/path?q=1"}, "https://h.example"},
		{api.EventSink{Type: "webhook", URL: "http://127.0.0.1:9000/x"}, "http://127.0.0.1:9000"},
		{api.EventSink{Type: "webhook", URL: "::"}, ""},
		{api.EventSink{Type: "command", Command: []string{"/opt/bin/ping-me", "--x"}}, "ping-me"},
		{api.EventSink{Type: "command", Command: []string{"ping-me"}}, "ping-me"},
		{api.EventSink{Type: "command"}, ""},
		{api.EventSink{Type: "pager"}, ""},
	} {
		if got := targetOf(c.s); got != c.want {
			t.Errorf("targetOf(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

// A type newSender does not know is an error, not a sink.
func TestNewSenderRefusesAnUnknownType(t *testing.T) {
	if s, err := newSender(api.EventSink{Type: "pager"}, env{}); s != nil || err == nil {
		t.Fatalf("newSender = %v, %v", s, err)
	}
}
