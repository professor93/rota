package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/events"
	"github.com/professor93/rota/internal/holdwire"
)

// The states a sink is in.
const (
	stateWaiting = "waiting" // made, and nothing tried yet
	stateOK      = "ok"      // its last attempt was delivered
	stateFailing = "failing" // its last attempt failed, or it could not be made
	stateOff     = "off"     // no daemon runs it: Describe's
)

// Status is how one sink stands: what it is called and where it sends, the
// events it is for, its state, how many events it sent and dropped, when it
// last sent, its last error and when, and how its last test went. Nothing
// in it is a secret: the target of a webhook is its scheme and host alone,
// and every error has been through cleanErr.
type Status struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Target      string      `json:"target"`
	On          []string    `json:"on"`
	State       string      `json:"state"`
	Sent        int         `json:"sent"`
	Dropped     int         `json:"dropped"`
	LastSent    time.Time   `json:"last_sent,omitzero"`
	LastError   string      `json:"last_error,omitempty"`
	LastErrorAt time.Time   `json:"last_error_at,omitzero"`
	LastTest    *TestResult `json:"last_test,omitzero"`
}

// TestResult is how a sink answered the test event ID, and when.
type TestResult struct {
	ID    int64     `json:"id"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

// StatusFile is sinks.json: the daemon that wrote it, when, and its sinks
// in the order server.toml has them.
type StatusFile struct {
	PID     int       `json:"pid"`
	Updated time.Time `json:"updated"`
	Sinks   []Status  `json:"sinks"`
}

// Outcome is one sink's answer to Test: whether it answered in time, and if
// so whether the test was delivered, or why not.
type Outcome struct {
	Sink     string `json:"sink"`
	Answered bool   `json:"answered"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// testPoll is how often Test reads sinks.json while it waits.
var testPoll = 50 * time.Millisecond

// StatusPath is sinks.json in the store's terminals directory.
func StatusPath(terminals string) string { return filepath.Join(terminals, "sinks.json") }

// ReadStatus is sinks.json as the daemon serving the store keeps it. A file
// that is not there, or that a daemon no longer running wrote, is nil with
// no error: no running daemon sends events.
func ReadStatus(terminals string) (*StatusFile, error) {
	f, err := readFile(StatusPath(terminals))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !holdwire.IsDaemon(terminals, f.PID) {
		return nil, nil
	}
	return f, nil
}

// readFile reads a status file, whoever wrote it.
func readFile(path string) (*StatusFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f StatusFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// writeFile lands f at path, its owner's alone, all at once.
func writeFile(path string, f StatusFile) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(raw, '\n'))
}

// writeAtomic lands data at path at 0600 through a temporary file in the
// same directory and a rename, so a reader sees the old file or the new one
// and never part of one. The temporary file goes on any failure.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cherr := f.Chmod(0o600) // CreateTemp's own mode, said rather than assumed
	if errors.Is(cherr, errors.ErrUnsupported) {
		cherr = nil
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(cherr, werr, serr, cerr); err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// writeStatus writes sinks.json as the sinks stand now. A write that fails
// is said once, until one works again.
func (n *Notifier) writeStatus() {
	n.writeMu.Lock()
	defer n.writeMu.Unlock()
	err := writeFile(n.path, StatusFile{PID: os.Getpid(), Updated: time.Now().UTC(), Sinks: n.status()})
	if err != nil && !n.writeFail {
		n.log.Warn("events: sinks.json could not be written: " + cleanErr(err, nil))
	}
	n.writeFail = err != nil
}

// keepStatus writes sinks.json when something changed, at most once each
// statusEvery, and says the drops a minute has passed on, until Wait.
func (n *Notifier) keepStatus() {
	defer close(n.writerDone)
	t := time.NewTicker(n.k.every)
	defer t.Stop()
	for {
		select {
		case <-n.writerStop:
			return
		case <-t.C:
		}
		n.sayDrops(false)
		if n.dirty.Swap(false) {
			n.writeStatus()
		}
	}
}

// Describe is the sinks of e as server.toml has them, each off, for a store
// no daemon sends events for. It reads no file: a token file or a program
// is the running sink's to find.
func Describe(e api.EventsSection) []Status {
	names := sinkNames(e.Sinks)
	out := make([]Status, 0, len(e.Sinks))
	for i, s := range e.Sinks {
		out = append(out, Status{Name: names[i], Type: s.Type, Target: targetOf(s), On: patterns(s.On), State: stateOff})
	}
	return out
}

// patterns is a sink's on as sinks.json says it: [] for every event.
func patterns(on []string) []string {
	if len(on) == 0 {
		return []string{}
	}
	return slices.Clone(on)
}

// targetOf is where a sink sends, said without a secret: "chat <id>" for
// Telegram, a webhook's scheme and host, a program's name.
func targetOf(s api.EventSink) string {
	var t string
	switch s.Type {
	case "telegram":
		t = "chat " + strings.TrimSpace(s.ChatID)
	case "webhook":
		if u, err := parseURL(s.URL); err == nil && u.Host != "" {
			t = u.Scheme + "://" + u.Host
		}
	case "command":
		if len(s.Command) > 0 && s.Command[0] != "" {
			t = filepath.Base(s.Command[0])
		}
	}
	return events.Clean(t, 200)
}

// Test sends a test event to the sink called sink, or to every sink when
// sink is "", through the daemon c talks to, and waits up to within for
// each to answer in sinks.json. A sink the daemon does not have is refused
// before anything is sent.
func Test(c *client.Client, terminals, sink string, within time.Duration) ([]Outcome, error) {
	f, err := ReadStatus(terminals)
	if err != nil {
		return nil, err
	}
	if f == nil || len(f.Sinks) == 0 {
		return nil, errors.New("the rota daemon sends events to no sink")
	}
	names := make([]string, 0, len(f.Sinks))
	for _, s := range f.Sinks {
		names = append(names, s.Name)
	}
	want := names
	if sink != "" {
		if !slices.Contains(names, sink) {
			return nil, fmt.Errorf("no event sink is called %q; the sinks are %s", sink, strings.Join(names, ", "))
		}
		want = []string{sink}
	}
	id, err := c.TestEvent(sink)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(within)
	for {
		out, all := answers(f, want, id)
		if all || !time.Now().Before(deadline) {
			return out, nil
		}
		time.Sleep(min(testPoll, time.Until(deadline)))
		// A read that fails keeps what was read last; the deadline ends it.
		if g, err := ReadStatus(terminals); err == nil && g != nil {
			f = g
		}
	}
}

// answers is each wanted sink's answer to the test id as f has it, and
// whether all of them have answered. A later test's answer stands for it.
func answers(f *StatusFile, want []string, id int64) ([]Outcome, bool) {
	out := make([]Outcome, 0, len(want))
	all := true
	for _, name := range want {
		o := Outcome{Sink: name}
		for _, s := range f.Sinks {
			if s.Name == name && s.LastTest != nil && s.LastTest.ID >= id {
				o.Answered, o.OK, o.Error = true, s.LastTest.OK, s.LastTest.Error
			}
		}
		all = all && o.Answered
		out = append(out, o)
	}
	return out, all
}
