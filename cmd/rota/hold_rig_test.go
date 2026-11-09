//go:build linux || darwin

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/internal/share"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// The rig for hold_test.go: a store whose tokens are distinctive, a holder
// started the way the daemon starts one, and the daemon's end of its link.

// holdToken is the accounts' credential, distinctive enough that finding it
// anywhere it must not be is not a coincidence of a short string.
const holdToken = "tok-held-5e1f0c9a3b-never-written"

// holdStore is shortHome with a claude account and a codex account whose
// tokens are holdToken, and says where the store and its terminals are.
func holdStore(t *testing.T) (home, terminals string) {
	t.Helper()
	shortHome(t)
	home = os.Getenv("ROTA_HOME")
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"`+holdToken+`"}},
		{"id":2,"provider":"codex","email":"b@x","order":2,"token":{"accessToken":"`+holdToken+`-codex","refreshToken":"rt-`+holdToken+`"},"extra":{"id_token":"h.p.s"}}]}`)
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	terminals, err = st.TerminalDir()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := holdwire.SockFits(holdwire.SockPath(holdwire.Dir(terminals, "0123456789abcdef"))); err != nil {
		t.Skipf("no held session fits here: %v", err)
	}
	release, err := holdwire.ClaimDaemon(terminals, wire.Version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return home, terminals
}

// holdFake installs the tty fake as name and says the directory to put on
// PATH.
func holdFake(t *testing.T, name string, spec fakecli.Spec) string {
	t.Helper()
	spec.Tty = true
	bin := t.TempDir()
	fakecli.Install(t, bin, name, spec)
	return bin
}

// holderProc is one `rota serve hold` and its answer.
type holderProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	ready  holdwire.Ready
	dir    string
	exited chan struct{}
}

// holdRequest is a request for account 1 at 120×40 in a folder of its own.
func holdRequest(t *testing.T) holdwire.Request {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	return holdwire.Request{
		ID: hex.EncodeToString(id[:]), Kind: holdwire.KindAccount, Account: 1, Name: "held one",
		Args: []string{"--model", "opus", "--resume", "abc"},
		Cwd:  t.TempDir(), Cols: 120, Rows: 40,
	}
}

// startHolderProc starts a holder on req with bin first on PATH and reads its
// one line of answer.
func startHolderProc(t *testing.T, terminals, bin string, req holdwire.Request) *holderProc {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	h := launchHolder(t, terminals, bin, req.ID, bytes.NewReader(requestLine(t, req)), w)
	w.Close()
	t.Cleanup(func() { r.Close() })
	got := make(chan error, 1)
	go func() { got <- json.NewDecoder(r).Decode(&h.ready) }()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the holder gave no answer: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the holder never answered")
	}
	return h
}

// launchHolder starts `rota serve hold` for session id the way the daemon
// does — in a session of its own, with bin first on PATH — reading in and
// answering on out, and leaves reading the answer to the caller.
func launchHolder(t *testing.T, terminals, bin, id string, in io.Reader, out *os.File) *holderProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "serve", "hold")
	cmd.Env = append(os.Environ(), "ROTA_AS_RUN=1", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout = in, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &holderProc{t: t, cmd: cmd, dir: holdwire.Dir(terminals, id), exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(h.exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-h.exited
		if m, err := holdwire.ReadMeta(h.dir); err == nil && proc.Alive(m.ChildPID) {
			_ = syscall.Kill(m.ChildPID, syscall.SIGKILL)
		}
	})
	return h
}

// requestLine is req as the daemon writes it.
func requestLine(t *testing.T, req holdwire.Request) []byte {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

// code waits for the holder to exit and says how.
func (h *holderProc) code() int {
	h.t.Helper()
	select {
	case <-h.exited:
	case <-time.After(20 * time.Second):
		h.t.Fatal("the holder never exited")
	}
	return h.cmd.ProcessState.ExitCode()
}

func (h *holderProc) meta() *holdwire.Meta {
	h.t.Helper()
	m, err := holdwire.ReadMeta(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

// holderLink is the daemon's end of a link to a holder, driven by hand.
type holderLink struct {
	t   *testing.T
	c   *share.Conn
	out strings.Builder
	seq uint64
}

// link dials the holder, is taken, and asks for everything from offset 0.
func (h *holderProc) link() *holderLink {
	h.t.Helper()
	conn, err := net.Dial("unix", holdwire.SockPath(h.dir))
	if err != nil {
		h.t.Fatal(err)
	}
	l := &holderLink{t: h.t, c: share.NewConn(conn, 0)}
	h.t.Cleanup(func() { l.c.Close() })
	if err := l.c.SendJSON(holdwire.Hello, holdwire.HelloMsg{Majors: holdwire.Majors}); err != nil {
		h.t.Fatal(err)
	}
	if kind, b := l.next(); kind != holdwire.Welcome {
		h.t.Fatalf("the holder answered %d %q, not a Welcome", kind, b)
	}
	if err := l.c.SendJSON(holdwire.Since, holdwire.SinceMsg{Offset: 0}); err != nil {
		h.t.Fatal(err)
	}
	return l
}

func (l *holderLink) next() (byte, []byte) {
	l.t.Helper()
	for {
		l.c.Deadline(time.Now().Add(10 * time.Second))
		kind, b, err := l.c.Read()
		if err != nil {
			l.t.Fatalf("reading the link: %v (so far %q)", err, l.out.String())
		}
		if kind != holdwire.Ping {
			return kind, b
		}
	}
}

// wait reads the CLI's output until want has been printed, and returns
// everything up to and including it that was not returned before.
func (l *holderLink) wait(want string) string {
	l.t.Helper()
	for {
		if i := strings.Index(l.out.String(), want); i >= 0 {
			all := l.out.String()
			l.out.Reset()
			l.out.WriteString(all[i+len(want):])
			return all[:i+len(want)]
		}
		kind, b := l.next()
		switch kind {
		case holdwire.Output:
			_, p, err := holdwire.OutputOf(b)
			if err != nil {
				l.t.Fatal(err)
			}
			l.out.Write(p)
		case holdwire.Exit:
			l.t.Fatalf("the CLI ended before printing %q (so far %q)", want, l.out.String())
		}
	}
}

// line asks the fake one question and returns its answer: the line after
// the terminal's own echo of the question.
func (l *holderLink) line(ask string) string {
	l.t.Helper()
	l.typed(ask + "\r")
	l.wait(ask + "\r\n")
	// The fake ends a line in CR LF, and the terminal turns its LF into
	// another CR LF.
	return strings.TrimRight(l.wait("\n"), "\r\n")
}

func (l *holderLink) typed(s string) {
	l.t.Helper()
	l.seq++
	if err := l.c.Send(holdwire.Input, holdwire.PutInput(l.seq, []byte(s))); err != nil {
		l.t.Fatal(err)
	}
}

// exit reads to the Exit frame and says its code.
func (l *holderLink) exit() int {
	l.t.Helper()
	for {
		kind, b := l.next()
		if kind == holdwire.Exit {
			var e holdwire.ExitMsg
			if err := share.Unmarshal(b, &e); err != nil {
				l.t.Fatal(err)
			}
			return e.Code
		}
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// ignores says whether pid ignores sig, as Linux's /proc says.
func ignores(t *testing.T, pid int, sig syscall.Signal) bool {
	t.Helper()
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "SigIgn:"); ok {
			mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			return mask&(1<<(uint(sig)-1)) != 0
		}
	}
	t.Fatalf("/proc/%d/status says nothing of ignored signals", pid)
	return false
}

// gone says pid has exited: it is not there at all, or it is a zombie
// nobody has reaped yet. An orphan's new parent — a container's first
// process, say — may never reap it, and a zombie holds nothing open.
func gone(pid int) bool {
	if !proc.Alive(pid) {
		return true
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the command's name, which is in parentheses and
	// may hold one of its own.
	i := bytes.LastIndexByte(raw, ')')
	return i >= 0 && i+2 < len(raw) && raw[i+2] == 'Z'
}
