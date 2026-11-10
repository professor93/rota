//go:build linux || darwin

package sshd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/attach"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
)

/* ------------------------------------------------------------ the rig --- */

// commandsRig is a real server with the terminal group on, whose terminals
// it runs itself (localTerm), over a store whose one account runs a fake as
// claude, behind an SSH server with the real commands and sshdRig's people:
// me (control) and eye (watch). c reaches the same server in process as me,
// to see what a command did, and asked is every create the SSH side sent.
type commandsRig struct {
	*sshdRig
	srv  *api.Server
	work string
	c    *client.Client

	mu    sync.Mutex
	asked []map[string]any
}

// commandsMe is the principal the rig's own client is.
var commandsMe = api.Principal{Name: "me", Role: api.RoleControl, Via: "user"}

// commandsTty is the fake that sits at a terminal and says it is ready.
var commandsTty = fakecli.Spec{Tty: true}

func commandsStart(t *testing.T, spec fakecli.Spec, edit ...func(*Options)) *commandsRig {
	t.Helper()
	// Short, rather than t.TempDir: the store may hold a unix socket.
	home, err := os.MkdirTemp("", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	work, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	store := `{"accounts":[{"id":1,"provider":"claude","email":"a@x","token":{"accessToken":"t"}}]}`
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROTA_HOME", home)
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", spec)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv, err := api.New(api.Options{Token: "x", Dir: home, Roots: []string{work}, RefreshEvery: -1,
		Routes: &api.Routes{API: true, WebSocket: true, Terminal: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	r := &commandsRig{srv: srv, work: work}
	// The API as each person, with every create it is sent kept.
	local := func(o *Options) {
		o.Handler = func(p *api.Principal) http.Handler {
			h := srv.LocalHandler(p)
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodPost && req.URL.Path == "/v1/terminals" {
					raw, _ := io.ReadAll(req.Body)
					var doc map[string]any
					_ = json.Unmarshal(raw, &doc)
					r.mu.Lock()
					r.asked = append(r.asked, doc)
					r.mu.Unlock()
					req.Body = io.NopCloser(bytes.NewReader(raw))
				}
				h.ServeHTTP(w, req)
			})
		}
	}
	// Registered after srv.Stop, so the SSH server closes first.
	r.sshdRig = sshdStart(t, append([]func(*Options){local}, edit...)...)
	c, stop := client.InProcess(srv.LocalHandler(&commandsMe))
	t.Cleanup(stop)
	r.c = c
	return r
}

// created is every create the SSH side has sent, each as its JSON reads.
func (r *commandsRig) created() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.asked))
	for _, doc := range r.asked {
		// fmt prints a map's keys in order.
		out = append(out, fmt.Sprint(doc))
	}
	return out
}

// open starts a terminal labelled label, in a folder of that name.
func (r *commandsRig) open(t *testing.T, label string) client.Session {
	t.Helper()
	dir := filepath.Join(r.work, label)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := r.c.Create(client.Create{Account: 1, Label: label, Cwd: dir, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// none fails t unless the server holds no terminal: a refused command
// started nothing.
func (r *commandsRig) none(t *testing.T) {
	t.Helper()
	all, err := r.c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("a refused command started %d terminals", len(all))
	}
}

// commandsLine is how list prints a live terminal the server runs itself.
func commandsLine(s client.Session) string {
	return fmt.Sprintf("%s  running  %s  claude/a@x  %s", s.ID, s.Label, s.Cwd)
}

// commandsPty runs line on a fresh session channel with a pty of cols×rows
// and TERM xterm. An empty line asks for a shell: the bare command.
func commandsPty(t *testing.T, c *ssh.Client, line string, cols, rows int) (stdout, stderr string, code int) {
	t.Helper()
	var sess *ssh.Session
	sshdEventually(t, func() (err error) {
		sess, err = c.NewSession()
		return err
	})
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	if err := sess.RequestPty("xterm", rows, cols, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	var err error
	if line == "" {
		if err = sess.Shell(); err == nil {
			err = sess.Wait()
		}
	} else {
		err = sess.Run(line)
	}
	var exit *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitStatus()
	default:
		t.Fatalf("run %q: %v", line, err)
	}
	return out.String(), errb.String(), code
}

// commandsCRLF is text as a pty channel writes it.
func commandsCRLF(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

/* ------------------------------------------------------------- list --- */

func TestListPrintsEverySession(t *testing.T) {
	r := commandsStart(t, commandsTty)
	c := r.signIn(r.me)
	if out, errOut, code := sshdRun(t, c, "list"); code != 0 || out != "no sessions\n" || errOut != "" {
		t.Fatalf("list with none: %d %q %q", code, out, errOut)
	}

	a, w := r.open(t, "api"), r.open(t, "web")
	out, errOut, code := sshdRun(t, c, "list")
	if code != 0 || errOut != "" {
		t.Fatalf("list: %d %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	slices.Sort(lines)
	want := []string{commandsLine(a), commandsLine(w)}
	slices.Sort(want)
	if !slices.Equal(lines, want) || !strings.HasSuffix(out, "\n") {
		t.Fatalf("list:\n%s\nwant:\n%s", out, strings.Join(want, "\n"))
	}

	// --json is the documents the API lists, each the session's own.
	out, _, code = sshdRun(t, c, "list --json")
	var doc struct {
		Terminals []map[string]any `json:"terminals"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Terminals) != 2 || !strings.HasSuffix(out, "}\n") {
		t.Fatalf("list --json: %d %q", code, out)
	}
	var ids []string
	for _, d := range doc.Terminals {
		ids = append(ids, fmt.Sprint(d["id"]))
		if d["label"] == nil || d["cols"] == nil || d["account"] == nil {
			t.Errorf("list --json gave %v, not the terminal's own document", d)
		}
	}
	slices.Sort(ids)
	wantIDs := []string{a.ID, w.ID}
	slices.Sort(wantIDs)
	if !slices.Equal(ids, wantIDs) {
		t.Errorf("list --json ids %v, want %v", ids, wantIDs)
	}

	// Anything else is a usage error, and lists nothing.
	for _, line := range []string{"list --x", "list --json --json", "list api", "list --json x"} {
		out, errOut, code := sshdRun(t, c, line)
		if code != 2 || out != "" || !strings.HasSuffix(errOut, usage) {
			t.Errorf("%s: %d %q %q", line, code, out, errOut)
		}
	}

	// On a pty each line ends as a terminal in raw mode needs it to.
	out, _, code = commandsPty(t, c, "list", 80, 24)
	if code != 0 || strings.Count(out, "\r\n") != 2 || strings.Count(out, "\n") != 2 {
		t.Errorf("list on a pty: %d %q", code, out)
	}
}

// Each value of a line is cleaned, and a value nobody gave is said as such:
// a held session's status, or its state once stopped; a terminal the server
// runs itself, which says neither, running until it has ended; the name,
// then the label, the folder and the id; a shell runs no account.
func TestAListLineSaysEachSessionOnce(t *testing.T) {
	at := time.Now()
	for _, c := range []struct {
		s    client.Session
		want string
	}{
		{client.Session{ID: "0123", Name: "api", Label: "x", Status: "needs_you", State: "live", Provider: "claude", Account: 1, AccountLabel: "a@x", Cwd: "/w/api"},
			"0123  needs_you  api  claude/a@x  /w/api"},
		{client.Session{ID: "0123", Name: "api", Status: "stopped", State: "exited", Provider: "codex", Account: 2, AccountLabel: "b", Cwd: "/w/api", Ended: &at},
			"0123  exited  api  codex/b  /w/api"},
		{client.Session{ID: "0123", Label: "job", Provider: "claude", Account: 1, AccountLabel: "a@x", Cwd: "/w/job"},
			"0123  running  job  claude/a@x  /w/job"},
		{client.Session{ID: "0123", Label: "job", Provider: "claude", Account: 1, AccountLabel: "a@x", Cwd: "/w/job", Ended: &at},
			"0123  stopped  job  claude/a@x  /w/job"},
		{client.Session{ID: "0123", Kind: "shell", Cwd: "/w/sh"},
			"0123  running  sh  shell  /w/sh"},
		{client.Session{ID: "0123"},
			"0123  running  0123  -  -"},
		{client.Session{ID: "0123", Name: "a\x1b[2Jb\nc", Status: "running", Provider: "claude", Account: 1, AccountLabel: "x\ty", Cwd: "/w/\x07d"},
			"0123  running  a [2Jb c  claude/x y  /w/ d"},
	} {
		if got := listLine(c.s); got != c.want {
			t.Errorf("listLine(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

/* -------------------------------------------------------------- new --- */

func TestNewStartsASession(t *testing.T) {
	// A fake that says what it was given, and stays.
	r := commandsStart(t, fakecli.Spec{KeepStdin: true, Stdout: []string{"ARGS={{args}}", "{{sleep:1m}}"}})
	c := r.signIn(r.me)
	out, errOut, code := sshdRun(t, c, "new --label ' job ' -- --model m")
	m := regexp.MustCompile(`^held as session job \(([0-9a-f]+)\)\n$`).FindStringSubmatch(out)
	if code != 0 || m == nil || errOut != "" {
		t.Fatalf("new: %d %q %q", code, out, errOut)
	}
	tm, err := r.c.Attach(m[1], true)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	if h := tm.Hello(); h.Cols != 120 || h.Rows != 40 {
		t.Errorf("a session started with no pty is %dx%d, want 120x40", h.Cols, h.Rows)
	}
	// The fake says it once and then sleeps, so Next is read by a goroutine
	// of its own, which Close ends.
	frames := make(chan []byte, 64)
	go func() {
		defer close(frames)
		for {
			f, err := tm.Next()
			if err != nil {
				return
			}
			frames <- f.Output
		}
	}()
	var shown strings.Builder
	given := regexp.MustCompile(`ARGS=(.* )?--model m(\r|\n| )`)
	deadline := time.After(10 * time.Second)
	for !given.MatchString(shown.String()) {
		select {
		case b, ok := <-frames:
			if !ok {
				t.Fatalf("the session ended (shown %q)", shown.String())
			}
			shown.Write(b)
		case <-deadline:
			t.Fatalf("the CLI never said it was given --model m: %q", shown.String())
		}
	}

	// With a pty, the session is the window's size, and its folder and
	// account are the ones named.
	dir := filepath.Join(r.work, "sized")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out, errOut, code = commandsPty(t, c, "new --json --account 1 --label sized --cwd "+dir, 70, 20)
	var doc struct {
		ID         string
		Label      string
		Cwd        string
		Cols, Rows int
		Account    struct{ ID int }
	}
	if code != 0 || errOut != "" || !strings.HasSuffix(out, "}\r\n") || json.Unmarshal([]byte(out), &doc) != nil {
		t.Fatalf("new --json on a pty: %d %q %q", code, out, errOut)
	}
	if doc.Cols != 70 || doc.Rows != 20 || doc.Label != "sized" || filepath.Base(doc.Cwd) != "sized" || doc.Account.ID != 1 {
		t.Errorf("new --json on a 70x20 pty: %+v", doc)
	}
	if s, err := r.c.Get(doc.ID); err != nil || s.Label != "sized" {
		t.Errorf("the session new said is not there: %v %+v", err, s)
	}
	// What new asked for: the rotation's choice, then the account named.
	want := []string{
		"map[args:[--model m] cols:120 label:job rows:40]",
		"map[account:1 cols:70 cwd:" + dir + " label:sized rows:20]",
	}
	if got := r.created(); !slices.Equal(got, want) {
		t.Errorf("new asked for\n%q\nwant\n%q", got, want)
	}
}

func TestNewRefusesAPromptAndPrint(t *testing.T) {
	r := commandsStart(t, commandsTty)
	c := r.signIn(r.me)
	const prompt = "new hands the CLI its terminal, so it takes no prompt or -p: give the CLI's arguments after --\n"
	const shell = "--shell runs a login shell, which takes no account or arguments\n"
	for _, x := range []struct{ line, want string }{
		{"new fix it", prompt},
		{"new --label job fix", prompt},
		{"new -- -p hi", prompt},
		{"new -- --print=true", prompt},
		{"new -- -cp", prompt},
		{"new -- --model m -pc", prompt},
		{"new -- --model m --print", prompt},
		{"new -p", prompt},
		{"new --print -- x", prompt},
		{"new --shell -- x", shell},
		{"new --shell --account 1", shell},
		{"new --account 1 --shell --", shell},
	} {
		out, errOut, code := sshdRun(t, c, x.line)
		if code != 2 || out != "" || errOut != x.want {
			t.Errorf("%s: %d %q %q, want 2 and %q", x.line, code, out, errOut, x.want)
		}
	}
	// A flag new does not take, or one without its value, is the usage.
	for _, line := range []string{"new --bogus", "new --label", "new --account x", "new -h"} {
		out, errOut, code := sshdRun(t, c, line)
		if code != 2 || out != "" || !strings.HasSuffix(errOut, usage) {
			t.Errorf("%s: %d %q %q", line, code, out, errOut)
		}
	}
	// Refused before the API was asked anything.
	if got := r.created(); len(got) != 0 {
		t.Errorf("a refused new asked for %q", got)
	}
	r.none(t)
}

func TestAWatcherCannotStartOne(t *testing.T) {
	r := commandsStart(t, commandsTty)
	c := r.signIn(r.eye)
	out, errOut, code := sshdRun(t, c, "new --label x")
	if code != 1 || out != "" || errOut != "this sign-in may only watch; nothing here can be changed from it\n" {
		t.Fatalf("eye's new: %d %q %q", code, out, errOut)
	}
	r.none(t)
	// Watching, eye still lists.
	r.open(t, "api")
	if out, _, code := sshdRun(t, c, "list"); code != 0 || !strings.Contains(out, "  api  ") {
		t.Errorf("eye's list: %d %q", code, out)
	}
}

func TestAShellNeedsTheFileToSaySo(t *testing.T) {
	r := commandsStart(t, commandsTty)
	c := r.signIn(r.me)
	out, errOut, code := sshdRun(t, c, "new --shell --label sh")
	const off = "this server runs accounts' CLIs only; a plain shell needs terminal.shell = true in the server file\n"
	if code != 1 || out != "" || errOut != off {
		t.Fatalf("new --shell: %d %q %q", code, out, errOut)
	}
	// The request names a shell, no account and no arguments, and the size.
	if got := r.created(); !slices.Equal(got, []string{"map[cols:120 kind:shell label:sh rows:40]"}) {
		t.Errorf("new --shell asked for %q", got)
	}
	r.none(t)
}

/* ------------------------------------------------------- a bare ssh --- */

func TestABareSSHListsAndExplains(t *testing.T) {
	r := commandsStart(t, commandsTty)
	c := r.signIn(r.me)
	// With none, it says so.
	if out, _, code := sshdRun(t, c, ""); code != 0 || out != "no sessions\n\n"+usage {
		t.Errorf("bare with none: %d %q", code, out)
	}
	s := r.open(t, "api")
	out, errOut, code := commandsPty(t, c, "", 80, 24)
	if want := commandsCRLF(commandsLine(s) + "\n\n" + usage); code != 0 || out != want || errOut != "" {
		t.Errorf("bare with a pty: %d %q %q, want %q", code, out, errOut, want)
	}
	// An empty line by exec is the same, without a pty as well.
	if out, _, code := sshdRun(t, c, ""); code != 0 || out != commandsLine(s)+"\n\n"+usage {
		t.Errorf("bare by exec: %d %q", code, out)
	}
}

// commandsSaw is what the recording TUI was given and found.
type commandsSaw struct {
	s      TUISession
	who    string
	typed  string
	resize [2]int
	err    error
}

func TestABareSSHOpensTheTUIWhenThereIsOne(t *testing.T) {
	saw := make(chan commandsSaw, 4)
	fail := errors.New("the TUI fell over")
	tui := func(ctx context.Context, s TUISession) error {
		got := commandsSaw{s: s}
		defer func() { saw <- got }()
		resp, err := s.Client.HTTP().Get("http://rota/v1/session")
		if err != nil {
			got.err = err
			return err
		}
		var who struct{ Name string }
		_ = json.NewDecoder(resp.Body).Decode(&who)
		resp.Body.Close()
		got.who = who.Name
		if who.Name == "eye" {
			return fail
		}
		// What is typed reaches the TUI through In.
		typed := make(chan string, 1)
		go func() {
			buf := make([]byte, 2)
			n, _ := io.ReadFull(s.In, buf)
			typed <- string(buf[:n])
		}()
		select {
		case got.typed = <-typed:
		case <-time.After(5 * time.Second):
			got.err = errors.New("nothing typed reached In")
			return got.err
		}
		select {
		case got.resize = <-s.Resize:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			got.err = errors.New("no resize")
			return got.err
		}
		fmt.Fprint(s.Out, "tui done")
		return nil
	}
	r := commandsStart(t, commandsTty, func(o *Options) { o.TUI = tui })
	c := r.signIn(r.me)

	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var out bytes.Buffer
	sess.Stdout = &out
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := sess.WindowChange(30, 100); err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(); err != nil || out.String() != "tui done" {
		t.Fatalf("the bare command with a TUI: %v %q", err, out.String())
	}
	got := <-saw
	s := got.s
	switch {
	case got.err != nil:
		t.Fatalf("the TUI: %v", got.err)
	case s.Cols != 80 || s.Rows != 24 || !slices.Equal(s.Env, []string{"TERM=xterm-256color"}):
		t.Errorf("the TUI was given %dx%d and %q", s.Cols, s.Rows, s.Env)
	case got.who != "me":
		t.Errorf("the TUI's client is %q, want me", got.who)
	case got.typed != "hi":
		t.Errorf("the TUI read %q, want hi", got.typed)
	case got.resize != [2]int{100, 30}:
		t.Errorf("the TUI's resize was %v, want 100x30", got.resize)
	case s.Key != attach.DefaultKey || s.Slot == nil || s.Out == nil:
		t.Errorf("the TUI was given key %#x, slot %v", s.Key, s.Slot != nil)
	}
	// Its client is let go of once it returns.
	if _, err := s.Client.HTTP().Get("http://rota/v1/session"); err == nil {
		t.Error("the TUI's client still answers after it returned")
	}

	// A TUI that fails is exit 1, its error on stderr.
	ce := r.signIn(r.eye)
	sess, err = ce.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var errb bytes.Buffer
	sess.Stderr = &errb
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	var exit *ssh.ExitError
	if err := sess.Run(""); !errors.As(err, &exit) || exit.ExitStatus() != 1 || errb.String() != "the TUI fell over\r\n" {
		t.Errorf("a failing TUI: %v %q", err, errb.String())
	}
	<-saw

	// Without a pty there is no TUI: the list and the commands.
	if out, _, code := sshdRun(t, c, ""); code != 0 || out != "no sessions\n\n"+usage {
		t.Errorf("bare with a TUI and no pty: %d %q", code, out)
	}
	select {
	case got := <-saw:
		t.Errorf("the TUI ran with no pty: %+v", got)
	default:
	}
}
