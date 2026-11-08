//go:build linux || darwin

package hold

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/lend"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/internal/termenv"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// `rota serve hold` is one held session's process. The daemon starts it in a
// session of its own, writes one request to its standard input, and reads
// one line back: ready, or why not. From then on it answers to nobody but
// the process holding the store's daemon lock, over its socket, and it lives
// exactly as long as the CLI it holds.
//
// Everything it is asked arrives on standard input and nothing in argv,
// since arguments can hold a prompt; and it is asked for an account, never
// handed a credential: it prepares the account itself, so the token never
// crosses between processes and the account's claim is taken here, where it
// can be handed to the CLI.

// ByHand is what a person who types `rota serve hold` is told.
const ByHand = "rota serve hold is started by the rota daemon (rota serve start), never by hand"

// maxRequest bounds the one request read: a request is a few hundred bytes,
// and the environment it may carry a few dozen kilobytes.
const maxRequest = 1 << 20

// maxScrollback bounds the ring a request may ask for. The ring is allocated
// whole at the start, so a size nobody meant must not cost the machine its
// memory; this is thirty-two times the default.
const maxScrollback = 64 << 20

// Main is `rota serve hold`: 2 when a person started it, 1 when the session
// could not start — said on out as a Ready line first — and 0 once the CLI
// has ended and its ending is recorded.
func Main(in io.Reader, out io.Writer, open func() (*store.Store, error)) int {
	keepOnBrokenPipe()
	if atTerminal(in) || !leadsSession() {
		fmt.Fprintln(out, ByHand)
		return 2
	}
	h := &holder{out: out}
	if err := h.start(in, open); err != nil {
		h.fail(err)
		return 1
	}
	return h.hold()
}

// keepOnBrokenPipe makes a write to a reader that went away an error rather
// than this process's end. Go ends a program whose standard output or error
// breaks, and the daemon that reads the Ready line may be gone before the
// holder writes it — stopped, or crashed, while the session started.
//
// The signal is taken rather than ignored: an ignored signal stays ignored
// across exec, so the CLI, and the shell and every command it starts, would
// begin with SIGPIPE ignored, and a pipeline such as `yes | head` would
// complain of a broken pipe instead of ending quietly. A taken one goes back
// to its default in a child. Nothing reads the channel, and nothing needs to.
func keepOnBrokenPipe() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}

// holder is one session being started and then held. Each field is set by
// the step that makes the thing it names, so a failure undoes exactly what
// was made.
type holder struct {
	out io.Writer
	req holdwire.Request

	terminals, dir string
	made           bool   // dir is this holder's: it made it
	unlock         func() // the session's lock, held for life
	logs           *logFile
	log            func(msg string, args ...any)

	release    func() // the account's claim, which a CLI given it holds too
	child      *ptyChild
	ln         *net.UnixListener
	rec        *metaFile
	sess       *Session
	unsignal   func()
	tokenUntil time.Time

	// The CLI's own registry, for claude: where it is, the zone its
	// procStart is written in, and when the CLI started.
	registry   string
	zone       *time.Location
	childStart time.Time
}

// plan is what to run: the executable, its arguments and environment, and
// who it runs as.
type plan struct {
	path      string
	args, env []string
	release   func()
	account   *rota.Account
}

// start is every step up to the Ready line. The first error ends it, and
// fail undoes whatever had been made by then.
func (h *holder) start(in io.Reader, open func() (*store.Store, error)) error {
	if err := h.read(in); err != nil {
		return err
	}
	st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := h.claimDir(st); err != nil {
		return err
	}
	p, err := h.plan(st)
	if err != nil {
		return err
	}
	h.release = p.release
	// Everything the store had to say is on disk; its lock goes before a
	// process that may run for days starts.
	_ = st.Close()

	env := noControl(termenv.Env(termenv.Scrub(p.env)), holdwire.CtlSock(h.terminals), func(name string) {
		h.log("a variable that names the control socket is kept from the CLI", "name", name)
	})
	cwd := h.req.Cwd
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return err
		}
	}
	// The claim is close-on-exec. A CLI given its claim holds it itself, so a
	// holder killed outright does not free an account its CLI still works on,
	// and a claude CLI is given it, as an interactive claude run's CLI is. A
	// codex CLI is not (lend.Lends): Codex leaves its app-server daemon
	// running when the CLI ends, holding whatever the CLI inherited, and the
	// account would stay claimed. This holder, which lives exactly as long as
	// the CLI, keeps that claim itself, as `rota serve claim` does for an
	// interactive codex run.
	if p.account != nil && !lend.Lends(p.account.Provider) {
		store.KeepClaimsAcrossExec()
	}
	cols, rows := holdwire.BoundSize(h.req.Cols, h.req.Rows)
	if h.child, err = startChild(p.path, p.args, env, cwd, cols, rows); err != nil {
		return err
	}
	started := time.Now()
	h.log("the CLI started", "pid", h.child.Pid(), "path", p.path, "cols", cols, "rows", rows)
	if h.childStart, err = proc.StartTime(h.child.Pid()); err != nil {
		h.log("the CLI's start time could not be read: its registry will not be believed, and the daemon cannot tell it from another process with its pid", "err", err)
	}

	if err := h.listen(); err != nil {
		return err
	}
	m := holdwire.Meta{
		ID: h.req.ID, Name: h.req.Name, Kind: h.req.Kind,
		HolderPID: os.Getpid(), ChildPID: h.child.Pid(), ChildStart: h.childStart,
		Protocol: holdwire.Major, Version: wire.Version, Cwd: cwd, Started: started,
	}
	if m.HolderStart, err = proc.StartTime(m.HolderPID); err != nil {
		h.log("the holder's own start time could not be read: the daemon cannot tell it from another process with its pid", "err", err)
	}
	if m.HolderStartTicks, err = proc.StartTicks(m.HolderPID); err != nil {
		h.log("the holder's own start in ticks could not be read: once the clock is set, the daemon may not tell it from another process with its pid", "err", err)
	}
	if m.BootID, err = proc.BootID(); err != nil {
		h.log("the boot id could not be read: the daemon cannot tell this session's processes from another boot's", "err", err)
	}
	if a := p.account; a != nil {
		m.Account, m.Provider = a.ID, a.Provider
		m.Args = holdwire.StoredArgs(a.Provider, h.req.Args)
		h.tokenUntil = holdwire.LoginUntil(a, p.env)
		if rota.Flavor(a.Provider) == "claude" {
			h.registry, h.zone = registryOf(env, h.child.Pid())
		}
	} else {
		m.Args = append([]string{}, h.req.Args...)
	}
	m.TokenUntil = h.tokenUntil
	h.rec = &metaFile{dir: h.dir, m: m}
	if err := h.rec.update(func(*holdwire.Meta) {}); err != nil {
		return err
	}

	scrollback := h.req.Scrollback
	if scrollback > maxScrollback {
		scrollback = maxScrollback
	}
	h.sess = Start(Config{
		ID: h.req.ID, Terminals: h.terminals, Cols: cols, Rows: rows, Scrollback: scrollback,
		Log: h.log, OnExit: h.ended,
	}, h.child)
	go h.sess.Serve(h.ln)
	h.unsignal = h.sess.stopOnSignals()
	return nil
}

// read takes the one request, and the environment it carries.
func (h *holder) read(in io.Reader) error {
	if err := json.NewDecoder(io.LimitReader(in, maxRequest)).Decode(&h.req); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	if !holdwire.IsID(h.req.ID) {
		return fmt.Errorf("the request names no session: %q", h.req.ID)
	}
	if h.req.Kind != holdwire.KindAccount && h.req.Kind != holdwire.KindShell {
		return fmt.Errorf("a holder runs an account or a shell, not %q", h.req.Kind)
	}
	if h.req.Env == nil {
		return nil
	}
	// The session starts from the caller's environment rather than the
	// daemon's — but in the daemon's store: that one is not the caller's to
	// change.
	home, had := os.LookupEnv("ROTA_HOME")
	os.Clearenv()
	for _, e := range h.req.Env {
		if k, v, ok := strings.Cut(e, "="); ok && k != "" {
			_ = os.Setenv(k, v)
		}
	}
	if had {
		return os.Setenv("ROTA_HOME", home)
	}
	return os.Unsetenv("ROTA_HOME")
}

// claimDir makes the session's directory and takes its lock, and from then
// on logs to holder.log in it.
func (h *holder) claimDir(st *store.Store) error {
	terminals, err := st.TerminalDir()
	if err != nil {
		return err
	}
	h.terminals, h.dir = terminals, holdwire.Dir(terminals, h.req.ID)
	if err := holdwire.SockFits(holdwire.SockPath(h.dir)); err != nil {
		return err
	}
	switch err := os.Mkdir(h.dir, 0o700); {
	case err == nil:
		h.made = true
	case !errors.Is(err, os.ErrExist):
		return err
	}
	// A directory somebody put there is used only if it is one: never a
	// link that would take the session's files somewhere else.
	if fi, err := os.Lstat(h.dir); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", h.dir)
	}
	if h.unlock, err = holdwire.Lock(h.dir); err != nil {
		if h.made {
			_ = os.RemoveAll(h.dir)
			h.made = false
		}
		return err
	}
	h.logs = &logFile{path: holdwire.LogPath(h.dir)}
	h.log = h.logs.logger()
	st.Warn = func(msg string) { h.log("warning", "msg", msg) }
	h.log("holding", "session", h.req.ID, "kind", h.req.Kind, "account", h.req.Account, "version", wire.Version)
	return nil
}

// plan is what the session runs: the account's CLI, prepared here so that
// the credential never crosses between processes, or the person's shell.
func (h *holder) plan(st *store.Store) (*plan, error) {
	if h.req.Kind == holdwire.KindShell {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		path, err := exec.LookPath(sh)
		if err != nil {
			return nil, fmt.Errorf("no shell could be found to run: %w", err)
		}
		args := append([]string{"-l"}, h.req.Args...)
		return &plan{path: path, args: args, env: store.HostEnv(), release: func() {}}, nil
	}
	a := st.Find(h.req.Account)
	if a == nil {
		return nil, rota.WrapNoAccount(h.req.Account)
	}
	ctx := context.Background()
	if err := st.Launchable(ctx, a); err != nil {
		return nil, err
	}
	// Prepared for the folder the CLI starts in, whose project settings it
	// reads, rather than for the holder's own.
	path, env, release, err := st.PrepareIn(ctx, a, h.req.Cwd)
	if err != nil {
		return nil, err
	}
	return &plan{path: path, args: append([]string{}, h.req.Args...), env: env, release: release, account: a}, nil
}

// listen opens the session's socket, readable by its owner alone. A socket
// already there is a holder's that is gone: this one holds the lock.
func (h *holder) listen() error {
	sock := holdwire.SockPath(h.dir)
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return err
	}
	h.ln = ln
	return os.Chmod(sock, 0o600)
}

// fail undoes whatever start made, and then says why: no CLI is left
// running, no record and no socket, and the directory goes if it was this
// holder's to make.
func (h *holder) fail(err error) {
	msg := wire.Unsecret(err.Error())
	if h.log != nil {
		h.log("the session could not start", "err", msg)
	}
	if h.child != nil {
		_ = h.child.Signal(killSignal)
		h.child.Wait()
		_ = h.child.Close()
	}
	if h.ln != nil {
		_ = h.ln.Close()
		_ = os.Remove(holdwire.SockPath(h.dir))
	}
	if h.rec != nil {
		_ = os.Remove(holdwire.MetaPath(h.dir))
	}
	if h.release != nil {
		h.release()
	}
	if h.made {
		_ = os.RemoveAll(h.dir)
	}
	if h.unlock != nil {
		h.unlock()
	}
	if err := h.say(holdwire.Ready{Error: msg, Kind: holdwire.Classify(err)}); err != nil && h.log != nil {
		h.log("nobody read the answer", "err", err)
	}
	if h.logs != nil {
		h.logs.close()
	}
}

// hold says the session is ready and keeps it until the CLI has ended.
//
// An answer nobody reads is no reason to stop: the daemon that asked went
// away while the session started. By then the CLI runs, meta.json is whole
// and the socket listens, so the session is exactly what a daemon gone a
// moment later would have left — held, for the next daemon to find — and
// tearing it down would end a CLI that may already be at work.
func (h *holder) hold() int {
	if err := h.say(holdwire.Ready{OK: true, PID: os.Getpid(), TokenUntil: h.tokenUntil}); err != nil {
		h.log("nobody read the answer: the session is held for the next daemon to find", "err", err)
	}
	// Whoever read the answer may go away; nothing written after it may
	// cost this process a broken pipe.
	if err := quiet(); err != nil {
		h.log("standard output could not be let go of", "err", err)
	}
	h.log("ready", "pid", os.Getpid(), "cli", h.child.Pid())
	kept := make(chan struct{})
	go func() {
		defer close(kept)
		h.keep()
	}()
	<-h.sess.Done()
	// The ending is recorded; what is left is tidying up, which a late
	// signal must not cut short.
	signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
	h.unsignal()
	<-kept
	h.release()
	_ = h.ln.Close()
	if err := os.Remove(holdwire.SockPath(h.dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.log("removing the socket", "err", err)
	}
	h.log("the holder is done")
	h.unlock()
	h.logs.close()
	return 0
}

// ended records how the CLI ended, before the lock that says the session
// is alive goes: a reader that finds the lock free finds the exit too.
func (h *holder) ended(code int) {
	if err := h.rec.update(func(m *holdwire.Meta) {
		m.Ended, m.Exit = time.Now(), &code
	}); err != nil {
		h.log("recording the ending", "err", err)
	}
}

// say writes the one line of answer, and says whether it could.
func (h *holder) say(r holdwire.Ready) error {
	raw, err := json.Marshal(r)
	if err != nil {
		raw = []byte(`{"ok":false,"error":"internal error","kind":"other"}`)
	}
	_, err = h.out.Write(append(raw, '\n'))
	return err
}

// ctlSock is the control socket's own name, as a path element.
var ctlSock = filepath.Base(holdwire.CtlSock(""))

// noControl drops every variable that names the daemon's control socket: a
// CLI inside a held session reaches the daemon only the way everybody else
// does, never by being told where its socket is. A value names it when it
// holds ctl, the socket's path, or has ctl.sock for a whole path element;
// a value with those letters only inside a longer name — a store under a
// folder called myctl.sock.d — is kept, so an account's CLAUDE_CONFIG_DIR is
// never lost to a coincidence. dropped hears each variable's name, never
// its value.
func noControl(env []string, ctl string, dropped func(name string)) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		name, value, _ := strings.Cut(e, "=")
		if namesControl(value, ctl) {
			dropped(name)
			continue
		}
		out = append(out, e)
	}
	return out
}

// namesControl says whether value is, or holds, a path to a control socket.
func namesControl(value, ctl string) bool {
	if ctl != "" && strings.Contains(value, ctl) {
		return true
	}
	elems := strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == os.PathListSeparator })
	return slices.Contains(elems, ctlSock)
}
