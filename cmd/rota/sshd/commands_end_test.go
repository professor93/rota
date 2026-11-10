//go:build linux || darwin

package sshd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
)

/* ---------------------------------------------- what ends a command --- */

// commandsSlow makes every create wait: until the request's own context
// ends, which it says on ended, or five seconds. asked says each create has
// come, by its label.
func commandsSlow(asked, ended chan<- string) func(*Options) {
	return func(o *Options) {
		next := o.Handler
		o.Handler = func(p *api.Principal) http.Handler {
			h := next(p)
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodPost || req.URL.Path != "/v1/terminals" {
					h.ServeHTTP(w, req)
					return
				}
				var doc struct{ Label string }
				_ = json.NewDecoder(req.Body).Decode(&doc)
				asked <- doc.Label
				select {
				case <-req.Context().Done():
					ended <- doc.Label
				case <-time.After(5 * time.Second):
				}
				http.Error(w, `{"error":"slow"}`, http.StatusServiceUnavailable)
			})
		}
	}
}

// commandsAwait waits for label on c, for at most within.
func commandsAwait(t *testing.T, what string, c <-chan string, label string, within time.Duration) {
	t.Helper()
	select {
	case got := <-c:
		if got != label {
			t.Fatalf("%s: %q, want %q", what, got, label)
		}
	case <-time.After(within):
		t.Fatalf("%s: nothing within %s", what, within)
	}
}

// A new waiting on the API ends as soon as its person leaves or the server
// stops, and Close does not wait for the holder.
func TestANewEndsWithItsChannel(t *testing.T) {
	asked, ended := make(chan string, 4), make(chan string, 4)
	r := commandsStart(t, commandsTty, commandsSlow(asked, ended))
	c := r.signIn(r.me)
	start := func(line string) *ssh.Session {
		t.Helper()
		var sess *ssh.Session
		sshdEventually(t, func() (err error) {
			sess, err = c.NewSession()
			return err
		})
		if err := sess.Start(line); err != nil {
			t.Fatal(err)
		}
		return sess
	}

	sess := start("new --label gone")
	commandsAwait(t, "the create", asked, "gone", 5*time.Second)
	sess.Close()
	commandsAwait(t, "the create's end once its person left", ended, "gone", time.Second)

	start("new --label stopped")
	commandsAwait(t, "the create", asked, "stopped", 5*time.Second)
	began := time.Now()
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("Close took %s with a create under way", took)
	}
	commandsAwait(t, "the create's end once the server stopped", ended, "stopped", time.Second)
}

// A TUI's context ends when its person leaves and when the server stops, and
// it is given no TERM but one rota takes.
func TestTheTUIEndsWithItsChannel(t *testing.T) {
	envs, ends := make(chan []string, 4), make(chan string, 4)
	tui := func(ctx context.Context, s TUISession) error {
		envs <- s.Env
		select {
		case <-ctx.Done():
			ends <- "ended"
		case <-time.After(5 * time.Second):
			ends <- "never told"
		}
		return nil
	}
	r := commandsStart(t, commandsTty, func(o *Options) { o.TUI = tui })
	c := r.signIn(r.me)
	open := func(term string) *ssh.Session {
		t.Helper()
		var sess *ssh.Session
		sshdEventually(t, func() (err error) {
			sess, err = c.NewSession()
			return err
		})
		if err := sess.RequestPty(term, 24, 80, ssh.TerminalModes{}); err != nil {
			t.Fatal(err)
		}
		if err := sess.Shell(); err != nil {
			t.Fatal(err)
		}
		select {
		case env := <-envs:
			if want := "TERM=" + term; channelTerm(term) && !slices.Equal(env, []string{want}) || !channelTerm(term) && env != nil {
				t.Errorf("TERM %q gave the TUI %q", term, env)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the TUI never ran")
		}
		return sess
	}

	open("not a term;").Close()
	commandsAwait(t, "the TUI once its person left", ends, "ended", time.Second)

	open("xterm")
	began := time.Now()
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("Close took %s with a TUI open", took)
	}
	commandsAwait(t, "the TUI once the server stopped", ends, "ended", time.Second)
}

// commandsDefaultMux puts a route on net/http's default handler once, so a
// command that served it in place of a missing API would be seen to.
var commandsDefaultMux sync.Once

// A list the API cannot give is exit 1 with the API's sentence, and a bare
// command says the same, then the commands. A server with no API refuses
// every request rather than serve net/http's default handler.
func TestAFailedListSaysWhy(t *testing.T) {
	r := sshdStart(t, func(o *Options) {
		o.Handler = func(*api.Principal) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"the store cannot be read"}`)
			})
		}
	})
	c := r.signIn(r.me)
	for _, line := range []string{"list", "list --json", "new"} {
		if out, errOut, code := sshdRun(t, c, line); code != 1 || out != "" || errOut != "the store cannot be read\n" {
			t.Errorf("%s with the API failing: %d %q %q", line, code, out, errOut)
		}
	}
	if out, errOut, code := sshdRun(t, c, ""); code != 1 || out != "\n"+usage || errOut != "the store cannot be read\n" {
		t.Errorf("bare with the API failing: %d %q %q", code, out, errOut)
	}

	commandsDefaultMux.Do(func() {
		http.HandleFunc("/v1/", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"terminals":[]}`)
		})
	})
	r = sshdStart(t, func(o *Options) { o.Handler = nil })
	c = r.signIn(r.me)
	for _, line := range []string{"list", "new"} {
		if out, errOut, code := sshdRun(t, c, line); code != 1 || out != "" || !strings.Contains(errOut, "404 Not Found") {
			t.Errorf("%s with no API: %d %q %q", line, code, out, errOut)
		}
	}
}

// commandsRaw says s holds a control character other than the line's end.
func commandsRaw(s string) bool {
	return strings.ContainsFunc(strings.TrimRight(s, "\r\n"), unicode.IsControl)
}

// --json writes no control character as it is, C1 among them, which
// encoding/json leaves be: each is escaped, and reads back the same.
func TestJSONEscapesEveryControl(t *testing.T) {
	r := commandsStart(t, commandsTty)
	c := r.signIn(r.me)
	const label = "a\u009b2Jb\u009d52;c\u009c\x1bd\u007fe"
	if _, err := r.c.Create(client.Create{Account: 1, Label: label, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	out, _, code := sshdRun(t, c, "list --json")
	var all struct {
		Terminals []struct{ Label string }
	}
	if code != 0 || commandsRaw(out) || !strings.Contains(out, `\u009b`) || !strings.Contains(out, `\u009d`) ||
		json.Unmarshal([]byte(out), &all) != nil || len(all.Terminals) != 1 || all.Terminals[0].Label != label {
		t.Errorf("list --json: %d %q", code, out)
	}
	// On a pty as well, and the text list cleans the same label.
	if out, _, _ := commandsPty(t, c, "list --json", 80, 24); commandsRaw(out) {
		t.Errorf("list --json on a pty: %q", out)
	}
	if out, _, _ := sshdRun(t, c, "list"); commandsRaw(out) || !strings.Contains(out, "a 2Jb 52;c  d e") {
		t.Errorf("list: %q", out)
	}

	out, _, code = sshdRun(t, c, "new --json --label 'x\u009by'")
	var doc struct{ Label string }
	if code != 0 || commandsRaw(out) || !strings.Contains(out, `\u009b`) || json.Unmarshal([]byte(out), &doc) != nil || doc.Label != "x\u009by" {
		t.Errorf("new --json: %d %q", code, out)
	}
}
