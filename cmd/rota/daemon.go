package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	nethttp "net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/termenv"
	"github.com/professor93/rota/store"
)

// rota serve start | stop | status: one daemon per store, holding sessions
// that outlive every terminal and every daemon. What is the same on every
// platform is here — the words, the flags, the configuration a daemon
// serves with; what needs a session of its own, signals and a unix socket is
// daemon_unix.go's, and daemon_other.go refuses.

const daemonUsage = `The store's daemon keeps held sessions running with no terminal open and
takes up the ones a daemon before it left; linux and macOS only:

  rota serve start [--ctl-only] [--config FILE]   start it in the background
  rota serve stop [--all] [--yes]                 stop it; its sessions go on
  rota serve status                               whether it runs; 3 if not

It listens on <store>/terminals/ctl.sock, which only you can reach, and on a
TCP port only where server.toml sets [server] listen and --ctl-only is not
given. It logs to <store>/daemon.log, or to [server] log_file. stop --all ends
every session too, and asks first; off a terminal it needs --yes.

  rota serve install     make it a systemd user unit or a launchd agent
  rota serve uninstall   take that away; its sessions go on

An installed daemon starts at login and keeps its sessions through a
restart. It is given PATH as it is where install runs and, as
CLAUDE_CONFIG_DIR, your own Claude Code directory when a variable names it:
never an account's, even when install runs inside a CLI rota launched. It is
not given ROTA_TOKEN, so a port it opens wants auth.token_file or [[users]]
in server.toml. Run install again to take up a new PATH or binary; install,
like stop and uninstall, is refused inside a held session.
`

// daemonReady is how long serve start waits for the daemon it started to
// answer, and daemonStopWait how long serve stop waits for it to let go of
// the store: long enough for what a stopping daemon may be finishing, the
// listeners' serveGrace and then a refresh under way (store.RefreshTimeout, and
// a margin for its save). Variables so a test can watch either run out.
var (
	daemonReady    = 10 * time.Second
	daemonStopWait = 60 * time.Second
)

// daemonGoodbye is the part of serveGrace a stopping daemon keeps for the
// WebSockets it ends last, after its listeners have had the rest: their
// goodbyes are written before the process exits.
const daemonGoodbye = time.Second

// inFlight counts the requests a daemon's listeners are serving — a
// WebSocket for as long as it is open — so that a daemon that has ended
// them can wait for each to have said goodbye before it exits. Closing a
// listener does not end a socket it handed over as a WebSocket.
type inFlight struct{ wg sync.WaitGroup }

// count is h, counted.
func (f *inFlight) count(h nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		f.wg.Add(1)
		defer f.wg.Done()
		h.ServeHTTP(w, r)
	})
}

// wait waits until every request counted is over, or until, and says
// whether they all were. It is asked once the listeners are closed, so the
// count only goes down.
func (f *inFlight) wait(until time.Time) bool {
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	t := time.NewTimer(time.Until(until))
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// serveStart is `rota serve start`. --foreground is the daemon itself, in
// this process: what serve start runs in the background, and what a
// supervisor runs.
func (c *cli) serveStart(args []string) error {
	fs := flag.NewFlagSet("serve start", flag.ContinueOnError)
	foreground := fs.Bool("foreground", false, "be the daemon in this process, as a supervisor runs it")
	ctlOnly := fs.Bool("ctl-only", false, "serve the control socket alone, whatever server.toml says about a port")
	conf := fs.String("config", "", "read this file, not <store dir>/server.toml")
	if ok, err := c.daemonFlags(fs, args); !ok {
		return err
	}
	if *foreground {
		return c.runDaemon(*ctlOnly, *conf)
	}
	return c.startDaemon(*ctlOnly, *conf)
}

// serveStop is `rota serve stop`.
func (c *cli) serveStop(args []string) error {
	fs := flag.NewFlagSet("serve stop", flag.ContinueOnError)
	all := fs.Bool("all", false, "end every session as well, after asking")
	yes := fs.Bool("yes", false, "with --all, end them without asking")
	if ok, err := c.daemonFlags(fs, args); !ok {
		return err
	}
	if *yes && !*all {
		return usageErr("--yes goes with --all: it answers the question --all asks, and a plain stop asks none")
	}
	return c.stopDaemon(*all, *yes)
}

// serveStatus is `rota serve status`: 0 when a daemon runs, 3 when none
// does.
func (c *cli) serveStatus(args []string) error {
	fs := flag.NewFlagSet("serve status", flag.ContinueOnError)
	if ok, err := c.daemonFlags(fs, args); !ok {
		return err
	}
	return c.daemonStatus()
}

// daemonFlags parses one daemon verb's flags. Asked for help, it prints what
// the daemon's verbs do and this one's flags, and says not to go on; none of
// them takes an argument.
func (c *cli) daemonFlags(fs *flag.FlagSet, args []string) (ok bool, err error) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(c.out, daemonUsage)
			fmt.Fprintln(c.out, "\nFlags:")
			fs.SetOutput(c.out)
			fs.PrintDefaults()
			return false, nil
		}
		return false, usageErr("%v", err)
	}
	if fs.NArg() > 0 {
		return false, usageErr("rota %s takes no arguments, got %q", fs.Name(), fs.Arg(0))
	}
	return true, nil
}

// daemonStart is which daemon serves the store once a start is over: the
// one it started, once that one answered as the process the start made, or
// the one already running, found before anything started or having won the
// store from the start's own daemon. Exactly one of the two is set.
type daemonStart struct {
	home, sock       string
	started, running *holdwire.Daemon
}

// pidOf is " (pid N)" for a daemon whose record names it, and nothing for
// one whose does not.
func pidOf(d *holdwire.Daemon) string {
	if d.PID == 0 {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", d.PID)
}

// launchedVars are what a CLI rota launched carries for its account, which
// a daemon started from inside one must not keep for its life and hand on
// to every shell it holds: whose account it is, the account's own home for
// its CLI, and the credential the CLI was handed. CLAUDE_CONFIG_DIR, the
// account's home too, is daemonEnv's to decide by the person's rule.
var launchedVars = []string{
	"ROTA_ACCOUNT_ID", "ROTA_ACCOUNT", "ROTA_PROVIDER",
	"CODEX_HOME", "GROK_HOME", "KIMI_CODE_HOME",
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "XAI_API_KEY",
}

// daemonEnv is the environment `serve start`, and a start `rota run
// --detach` makes, hand the daemon for the store at home: this process's,
// less what describes the terminal it was typed at (termenv.Scrub), which
// the daemon outlives, and, inside a CLI rota launched, less what that CLI
// carries for its account (launchedVars). CLAUDE_CONFIG_DIR is the person's
// own directory, only where a variable names it, by the rule every rota
// finds it by (store.PersonalClaudeWorld): inside a CLI rota launched, the
// one it has is the account's.
func daemonEnv(home string) []string {
	env := termenv.Scrub(termenv.Set(os.Environ(), "ROTA_HOME", home))
	launched := envValue(env, "ROTA_ACCOUNT_ID") != "" || envValue(env, "ROTA_PROVIDER") != ""
	env = slices.DeleteFunc(env, func(e string) bool {
		k, _, _ := strings.Cut(e, "=")
		return k == "CLAUDE_CONFIG_DIR" || launched && slices.Contains(launchedVars, k)
	})
	if dir, named := store.PersonalClaudeWorld(); named && dir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	}
	return env
}

// absoluteDirs is env with the Claude Code directories it names relatively
// named absolutely: a daemon, and each holder it starts, runs in /, where a
// relative one would be another directory.
func absoluteDirs(env []string) ([]string, error) {
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "ROTA_CLAUDE_HOME"} {
		for _, e := range env {
			if dir, ok := strings.CutPrefix(e, name+"="); ok && dir != "" && !filepath.IsAbs(dir) {
				abs, err := filepath.Abs(dir)
				if err != nil {
					return nil, err
				}
				env = termenv.Set(env, name, abs)
				break
			}
		}
	}
	return env, nil
}

// daemonStore is the store this command's daemon serves, by its absolute
// directory, and that store's terminals directory, where the daemon's lock,
// pid file and control socket are.
func daemonStore() (home, terminals string, err error) {
	st, err := store.Open("")
	if err != nil {
		return "", "", err
	}
	defer st.Close()
	if terminals, err = st.TerminalDir(); err != nil {
		return "", "", err
	}
	return filepath.Dir(terminals), terminals, nil
}

// daemonConfig is what a daemon for the store at home serves with: the
// file conf names, or the store's server.toml, or the defaults. A daemon
// serves the store it is started for, which is the one ROTA_HOME names and
// the one serve stop and serve status look at, so a [store] dir naming any
// other is refused rather than served where neither could find it.
func daemonConfig(conf, home string) (*api.Config, api.Options, api.Listener, error) {
	cfg, err := serverConfig(conf)
	if err != nil {
		return nil, api.Options{}, api.Listener{}, err
	}
	// Whatever reads a path the file names reads it from where it runs, and
	// a daemon runs in /: a file or folder under / instead of the one meant.
	// log_file is refused relative everywhere already, by Check.
	for _, p := range []struct {
		key   string
		paths []string
	}{
		{"runs.roots", cfg.Runs.Roots},
		{"tls.cert", []string{cfg.TLS.Cert}},
		{"tls.key", []string{cfg.TLS.Key}},
		{"auth.token_file", []string{cfg.Auth.TokenFile}},
		{"store.dir", []string{cfg.Store.Dir}},
	} {
		for _, path := range p.paths {
			if path != "" && !filepath.IsAbs(path) {
				return nil, api.Options{}, api.Listener{}, fmt.Errorf(
					"%s: %q is relative; a daemon runs in /, so every path in its file must be absolute in daemon mode", p.key, path)
			}
		}
	}
	opts, listener, err := cfg.Serve()
	if err != nil {
		return nil, api.Options{}, api.Listener{}, err
	}
	// The control socket's principal is local, so no user or token of a
	// daemon may be; foreground serve's may. Every [[users]] entry counts,
	// those who sign in by key alone too, whom opts.Users leaves out.
	people := make([]api.User, len(cfg.Users))
	for i, u := range cfg.Users {
		people[i] = api.User{Name: u.Name}
	}
	if err := api.ReservedNames(people, opts.Tokens); err != nil {
		return nil, api.Options{}, api.Listener{}, err
	}
	if dir := opts.Dir; dir != "" {
		if abs, err := filepath.Abs(dir); err != nil || abs != home {
			return nil, api.Options{}, api.Listener{}, fmt.Errorf(
				"store.dir names %s, but a daemon serves the store it is started for, %s; start it with ROTA_HOME=%s instead",
				oneLine(dir), oneLine(home), oneLine(dir))
		}
	}
	opts.Dir = home
	return cfg, opts, listener, nil
}

// daemonLog is where a daemon logs: [server] log_file, or daemon.log in its
// store.
func daemonLog(home string, cfg *api.Config) string {
	if cfg != nil && cfg.Server.LogFile != "" {
		return cfg.Server.LogFile
	}
	return filepath.Join(home, "daemon.log")
}

// daemonListens says whether a daemon opens a TCP port: only where the
// file asked for one and the command did not say --ctl-only. A daemon is
// for this machine's user, and a port is a door for everybody else.
func daemonListens(cfg *api.Config, ctlOnly bool) bool {
	return !ctlOnly && cfg.From["server.listen"] == "file"
}

// daemonHoldCommand is how a daemon starts each holder: this binary, as
// `rota serve hold`. Its path is resolved now and kept, so that a rota
// replaced in place — whose running binary's own path then reads as deleted
// — starts new holders from the binary that is there now. An upgrade that
// takes that file away altogether, as a versioned install does when it
// moves its link on, is followed by resolving exe again; with nothing there
// either, exe is run as given, and exec says what is missing.
func daemonHoldCommand(exe string) func() *exec.Cmd {
	var mu sync.Mutex
	resolved := exe
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		resolved = real
	}
	return func() *exec.Cmd {
		mu.Lock()
		if _, err := os.Stat(resolved); err != nil {
			resolved = exe
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				resolved = real
			}
		}
		path := resolved
		mu.Unlock()
		return exec.Command(path, "serve", "hold")
	}
}

// invokedAs is the path this rota was started by, its links left as they
// are: what was typed, found on PATH as the shell found it, so that the
// daemon, and each holder it starts, can follow a link an upgrade moves on.
// On linux os.Executable has followed every link already. A path that is
// not this very binary — an argv[0] its parent made up — is not believed.
func invokedAs() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := os.Args[0]
	if !strings.ContainsRune(path, os.PathSeparator) {
		if path, err = exec.LookPath(path); err != nil {
			return self, nil
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return self, nil
	}
	a, aerr := os.Stat(abs)
	b, berr := os.Stat(self)
	if aerr != nil || berr != nil || !os.SameFile(a, b) {
		return self, nil
	}
	return abs, nil
}

// sessionsN is "1 session" or "n sessions".
func sessionsN(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

// atTerminal says whether standard input is somebody at a terminal, who
// can be asked.
func (c *cli) atTerminal() bool {
	tty, ok := c.in.(*os.File)
	return ok && isTerminal(tty)
}

// confirm asks a yes-or-no question on stderr and reads the answer; only y
// or yes is yes.
func (c *cli) confirm(question string) bool {
	fmt.Fprint(c.err, question)
	line, _ := bufio.NewReader(c.in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// logTail is the last n lines written to a log after offset from, which is
// where it ended before the daemon that wrote them started: what that
// daemon had to say, and nothing an earlier one said. A log that shrank
// meanwhile was rotated, and is read from its start.
func logTail(path string, from int64, n int) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if int64(len(raw)) >= from {
		raw = raw[from:]
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// statusDoc is `rota serve status --json`. Started and HTTP are null when
// there is no daemon, or no port. SSH is left out altogether where no SSH
// server runs, so that a store with no [ssh] says what it always said.
type statusDoc struct {
	Running  bool       `json:"running"`
	PID      int        `json:"pid"`
	Version  string     `json:"version"`
	Started  *time.Time `json:"started"`
	Store    string     `json:"store"`
	Ctl      string     `json:"ctl"`
	HTTP     *string    `json:"http"`
	SSH      *statusSSH `json:"ssh,omitempty"`
	Sessions struct {
		Live    int `json:"live"`
		Stopped int `json:"stopped"`
	} `json:"sessions"`
}

// statusSSH is where the daemon's SSH server listens, and its host key's
// SHA256 fingerprint, which a client asked to trust it is compared with.
type statusSSH struct {
	Addr    string `json:"addr"`
	HostKey string `json:"host_key"`
}

// sayStatus prints a status, as JSON or as lines a person reads. In the
// lines each path, address and version, which the store's directory and
// the daemon's record say, is drawn with a space for each control
// character (see oneLine); JSON carries them as they are.
func (c *cli) sayStatus(doc *statusDoc) error {
	if c.json {
		return c.emit(doc)
	}
	if !doc.Running {
		fmt.Fprintf(c.out, "no rota daemon is running for %s\n", oneLine(doc.Store))
	} else {
		var about []string
		if doc.PID != 0 {
			about = append(about, fmt.Sprintf("pid %d", doc.PID))
		}
		if doc.Version != "" {
			about = append(about, "rota "+oneLine(doc.Version))
		}
		if doc.Started != nil {
			about = append(about, "since "+doc.Started.Local().Format(time.RFC3339))
		}
		fmt.Fprintf(c.out, "rota daemon running for %s", oneLine(doc.Store))
		if len(about) > 0 {
			fmt.Fprintf(c.out, " (%s)", strings.Join(about, ", "))
		}
		fmt.Fprintf(c.out, "\ncontrol socket %s\n", oneLine(doc.Ctl))
		if doc.HTTP != nil {
			fmt.Fprintf(c.out, "http on %s\n", oneLine(*doc.HTTP))
		}
		if doc.SSH != nil {
			fmt.Fprintf(c.out, "ssh on %s, host key %s\n", oneLine(doc.SSH.Addr), oneLine(doc.SSH.HostKey))
		}
	}
	if s := doc.Sessions; s.Live+s.Stopped > 0 || doc.Running {
		fmt.Fprintf(c.out, "sessions: %d running, %d stopped\n", s.Live, s.Stopped)
	}
	return nil
}

// runningRecords is every held session whose holder is running, by its
// lock: one this rota cannot read, and one still starting, as well.
func runningRecords(terminals string) []holdwire.Record {
	var out []holdwire.Record
	for _, r := range holdwire.Scan(terminals) {
		if r.Locked {
			out = append(out, r)
		}
	}
	return out
}

// waitEnded waits up to within for each session's lock to be let go of,
// and says which are still held then.
func waitEnded(terminals string, ids []string, within time.Duration) []string {
	left := slices.Clone(ids)
	for deadline := time.Now().Add(within); ; time.Sleep(50 * time.Millisecond) {
		left = slices.DeleteFunc(left, func(id string) bool {
			return !holdwire.Locked(holdwire.LockPath(holdwire.Dir(terminals, id)))
		})
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
	}
}

// notEnded is the error for sessions stop --all could not end: which they
// are, and why where it was told, each id and reason on a line of its own
// with a space for each control character in it (see oneLine).
func notEnded(said string, asked int, left []string, why map[string]error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s; %s ended, and %s did not:", said, sessionsN(asked-len(left)), sessionsN(len(left)))
	for _, id := range left {
		if err := why[id]; err != nil {
			fmt.Fprintf(&b, "\n  %s: %s", oneLine(id), oneLine(err.Error()))
		} else {
			fmt.Fprintf(&b, "\n  %s: still running", oneLine(id))
		}
	}
	return errors.New(b.String())
}
