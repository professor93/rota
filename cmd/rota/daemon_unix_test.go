//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"net"
	nethttp "net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// rota serve start | stop | status as a person types them. Each command is
// this test binary acting as rota in a process of its own, so is every
// daemon and holder they start, and the CLI in a held session is the tty
// fake. Nothing any of them starts outlives the test: the rig kills whatever
// the store still names when it is done.

/* ------------------------------------------------------------------ the rig --- */

type daemonRig struct {
	t                     *testing.T
	home, terminals, sock string
	// env is what every rota the test starts runs with: acting as rota,
	// with the fake claude first on PATH.
	env []string
	// ssh is where the last start said its daemon serves SSH, if it did.
	ssh string
}

func newDaemonRig(t *testing.T) *daemonRig {
	t.Helper()
	shortHome(t)
	home := os.Getenv("ROTA_HOME")
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	terminals, err := st.TerminalDir()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{holdwire.CtlSock(terminals), holdwire.SockPath(holdwire.Dir(terminals, "0123456789abcdef"))} {
		if err := holdwire.SockFits(p); err != nil {
			t.Skipf("no daemon fits here: %v", err)
		}
	}
	bin := holdFake(t, "claude", fakecli.Spec{})
	r := &daemonRig{t: t, home: home, terminals: terminals, sock: holdwire.CtlSock(terminals),
		env: append(os.Environ(), "ROTA_AS_RUN=1", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))}
	t.Cleanup(r.reap)
	return r
}

// cmd is one rota command, not yet started.
func (r *daemonRig) cmd(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = r.env
	return cmd
}

// run runs cmd to its end and says what it printed and how it exited.
func (r *daemonRig) run(cmd *exec.Cmd) (stdout, stderr string, code int) {
	r.t.Helper()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		r.t.Fatalf("rota %s never finished; it said %q %q", strings.Join(cmd.Args[1:], " "), out.String(), errb.String())
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

func (r *daemonRig) rota(args ...string) (stdout, stderr string, code int) {
	r.t.Helper()
	return r.run(r.cmd(args...))
}

var startedLine = regexp.MustCompile(`^rota daemon started for (\S+) \(pid (\d+)\); control socket (\S+?)(?:; http on (\S+?))?(?:; ssh on (\S+))?\n$`)

// start is `rota serve start` with args, which must say it started a daemon
// for this store on its control socket; the daemon's pid, and its http
// address if it said one.
func (r *daemonRig) start(args ...string) (pid int, http string) {
	r.t.Helper()
	return r.startWith(r.cmd(append([]string{"serve", "start"}, args...)...))
}

func (r *daemonRig) startWith(cmd *exec.Cmd) (pid int, http string) {
	r.t.Helper()
	out, errb, code := r.run(cmd)
	m := startedLine.FindStringSubmatch(out)
	if code != 0 || m == nil || m[1] != r.home || m[3] != r.sock || errb != "" {
		r.t.Fatalf("serve start: exit %d\nstdout %q\nstderr %q\nlog:\n%s", code, out, errb, r.log())
	}
	pid, _ = strconv.Atoi(m[2])
	if d, ok := holdwire.RunningDaemon(r.terminals); !ok || d.PID != pid {
		r.t.Fatalf("serve start said pid %d, and daemon.pid says %+v", pid, d)
	}
	r.ssh = m[5]
	return pid, m[4]
}

func (r *daemonRig) log() string {
	raw, _ := os.ReadFile(filepath.Join(r.home, "daemon.log"))
	return string(raw)
}

// newSession starts a held session over the control socket, as the local
// user, and says its id.
func (r *daemonRig) newSession(label string) string {
	r.t.Helper()
	body := `{"account":1,"cols":100,"rows":30,"label":"` + label + `"}`
	resp, err := ctlClient(r.sock).Post("http://rota/v1/terminals", "application/json", strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	id, _ := doc["id"].(string)
	if resp.StatusCode != nethttp.StatusCreated || !holdwire.IsID(id) {
		r.t.Fatalf("POST /v1/terminals over ctl.sock answered %d %v\nlog:\n%s", resp.StatusCode, doc, r.log())
	}
	return id
}

// attach opens a session's socket over ctl.sock, as the local user.
func (r *daemonRig) attach(id string) *peer {
	r.t.Helper()
	conn, err := net.DialTimeout("unix", r.sock, 10*time.Second)
	if err != nil {
		r.t.Fatal(err)
	}
	return attachOver(r.t, conn, "rota", "/v1/terminals/"+id+"/ws", "")
}

// session is what the daemon now says of one session.
func (r *daemonRig) session(id string) *rotaclient.Session {
	r.t.Helper()
	c, err := rotaclient.Dial(r.terminals)
	if err != nil {
		r.t.Fatal(err)
	}
	all, err := c.List()
	if err != nil {
		r.t.Fatal(err)
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i]
		}
	}
	r.t.Fatalf("the daemon lists no session %s: %+v", id, all)
	return nil
}

// reap kills the daemon and every holder and CLI the store's records name,
// each only while it is still the process recorded, and waits for their
// locks to go: nothing this test started may outlive it.
func (r *daemonRig) reap() {
	if d, ok := holdwire.RunningDaemon(r.terminals); ok {
		_ = holdwire.Signal(d.PID, d.ProcStart, d.ProcStartTicks, d.BootID, syscall.SIGKILL)
	}
	recs := holdwire.Scan(r.terminals)
	for _, rec := range recs {
		if m := rec.Meta; m != nil {
			_ = holdwire.Signal(m.HolderPID, m.HolderStart, m.HolderStartTicks, m.BootID, syscall.SIGKILL)
			_ = holdwire.Signal(m.ChildPID, m.ChildStart, 0, m.BootID, syscall.SIGKILL)
		}
	}
	locks := []string{holdwire.DaemonLock(r.terminals)}
	for _, rec := range recs {
		locks = append(locks, holdwire.LockPath(rec.Dir))
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		held := false
		for _, l := range locks {
			held = held || holdwire.Locked(l)
		}
		if !held {
			return
		}
	}
	r.t.Errorf("something of this test's still holds a lock in %s", r.terminals)
}

// statusJSON is serve status --json as a consumer reads it, keys spelt out
// here rather than borrowed from the command's own type.
type statusJSON struct {
	Running bool       `json:"running"`
	PID     int        `json:"pid"`
	Version string     `json:"version"`
	Started *time.Time `json:"started"`
	Store   string     `json:"store"`
	Ctl     string     `json:"ctl"`
	HTTP    *string    `json:"http"`
	SSH     *struct {
		Addr    string `json:"addr"`
		HostKey string `json:"host_key"`
	} `json:"ssh"`
	Sessions struct {
		Live    int `json:"live"`
		Stopped int `json:"stopped"`
	} `json:"sessions"`
}

func (r *daemonRig) status() (statusJSON, int) {
	r.t.Helper()
	out, errb, code := r.rota("--json", "serve", "status")
	var doc statusJSON
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		r.t.Fatalf("status --json: exit %d %q %q: %v", code, out, errb, err)
	}
	return doc, code
}

/* ---------------------------------------------------------------- the tests --- */

// A daemon starts in the background and says so; status names it and exits
// 0, stop ends it, and status then exits 3. The store is named relatively
// and from somewhere else: the daemon, which runs in /, must still be given
// the one the command meant.
func TestStartStatusStop(t *testing.T) {
	r := newDaemonRig(t)
	cmd := r.cmd("serve", "start")
	cmd.Dir = filepath.Dir(r.home)
	cmd.Env = append(cmd.Env, "ROTA_HOME="+filepath.Base(r.home), "PWD="+cmd.Dir)
	pid, http := r.startWith(cmd)
	if http != "" {
		t.Fatalf("with no server.toml a daemon opens no port, and said %q", http)
	}

	out, errb, code := r.rota("serve", "status")
	if code != 0 || !strings.Contains(out, "(pid "+strconv.Itoa(pid)) || !strings.Contains(out, r.sock) {
		t.Fatalf("status with a daemon: exit %d %q %q", code, out, errb)
	}
	doc, code := r.status()
	if code != 0 || !doc.Running || doc.PID != pid || doc.Version != wire.Version || doc.Started == nil ||
		doc.Store != r.home || doc.Ctl != r.sock || doc.HTTP != nil || doc.Sessions.Live != 0 || doc.Sessions.Stopped != 0 {
		t.Fatalf("status --json with a daemon: exit %d %+v", code, doc)
	}

	if out, errb, code := r.rota("serve", "stop"); code != 0 || out != "rota daemon stopped; 0 sessions keep running\n" {
		t.Fatalf("serve stop: exit %d %q %q", code, out, errb)
	}
	within(t, "the daemon's process goes", 10*time.Second, func() bool { return gone(pid) })
	for _, f := range []string{r.sock, holdwire.DaemonPIDFile(r.terminals)} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Fatalf("%s is still there after the daemon stopped: %v", f, err)
		}
	}
	none := "no rota daemon is running for " + r.home + "\n"
	if out, errb, code := r.rota("serve", "status"); code != 3 || out != none {
		t.Fatalf("status with no daemon: exit %d %q %q", code, out, errb)
	}
	if doc, code := r.status(); code != 3 || doc.Running || doc.PID != 0 || doc.Store != r.home || doc.Ctl != r.sock {
		t.Fatalf("status --json with no daemon: exit %d %+v", code, doc)
	}
	if out, errb, code := r.rota("serve", "stop"); code != 0 || out != none {
		t.Fatalf("stop with no daemon: exit %d %q %q", code, out, errb)
	}
}

// Two starts at once make one daemon (Review Focus 3). Both exit 0 and name
// the same pid; daemon.pid names it, it alone is left holding the lock, and
// every other foreground child that started has gone, saying why in the log.
// A start that finds the other's daemon before it starts one of its own
// starts no child at all, so the race is run again until both did.
func TestTwoStartsMakeOneDaemon(t *testing.T) {
	r := newDaemonRig(t)
	starting := regexp.MustCompile(`msg="rota daemon starting".* pid=(\d+)`)
	for try := 1; ; try++ {
		from := len(r.log())
		var wg sync.WaitGroup
		outs := make([]string, 2)
		codes := make([]int, 2)
		for i := range outs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outs[i], _, codes[i] = r.run(r.cmd("serve", "start"))
			}()
		}
		wg.Wait()
		d, ok := holdwire.RunningDaemon(r.terminals)
		if !ok || d.PID <= 0 || gone(d.PID) {
			t.Fatalf("after two starts nothing serves: %+v %v %q\nlog:\n%s", d, codes, outs, r.log())
		}
		named := "(pid " + strconv.Itoa(d.PID) + ")"
		for i := range outs {
			if codes[i] != 0 || !strings.Contains(outs[i], named) {
				t.Fatalf("both starts exit 0 and name the daemon %s: %v %q", named, codes, outs)
			}
		}
		if !strings.HasPrefix(outs[0]+outs[1], "rota daemon started") && !strings.HasPrefix(outs[1]+outs[0], "rota daemon started") {
			t.Fatalf("one of the two started it: %q", outs)
		}
		log := r.log()[from:]
		losers := 0
		for _, m := range starting.FindAllStringSubmatch(log, -1) {
			child, _ := strconv.Atoi(m[1])
			if child == d.PID {
				continue
			}
			losers++
			within(t, "the other foreground child exits", 10*time.Second, func() bool { return gone(child) })
			said := regexp.MustCompile(`pid=` + m[1] + `\b.*another rota daemon serves|another rota daemon serves.* pid=` + m[1] + `\b`)
			if !said.MatchString(log) {
				t.Fatalf("child %d went without saying another daemon serves:\n%s", child, log)
			}
		}
		if losers > 0 {
			break
		}
		t.Logf("try %d: one start found the other's daemon before starting its own; again", try)
		if try == 5 {
			t.Fatalf("five times over, one start found the other's daemon before starting its own:\n%s", r.log())
		}
		if out, errb, code := r.rota("serve", "stop"); code != 0 {
			t.Fatalf("serve stop: exit %d %q %q", code, out, errb)
		}
	}
}

// Stopping the daemon leaves its sessions running, and the next one takes
// them up (P36: the real holder, not the api tests' fake). It is the same
// session, still live, still typed into. A viewer attached over ctl.sock
// when the daemon stops is told why before its socket goes, within
// serveGrace: closing the control socket does not end a socket it handed
// over as a WebSocket, so the daemon must, before it exits.
func TestStopLeavesSessionsAndAStartReadoptsThem(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id := r.newSession("kept")
	first := r.attach(id)
	first.waitDoc("hello")
	first.waitOut("ready 100x30")

	began := time.Now()
	if out, errb, code := r.rota("serve", "stop"); code != 0 || out != "rota daemon stopped; 1 session keeps running\n" {
		t.Fatalf("serve stop: exit %d %q %q", code, out, errb)
	}
	if code, why := first.closed(serveGrace); code != 1001 || why != "the server is stopping" || time.Since(began) > serveGrace {
		t.Fatalf("the viewer was closed with %d %q, %s after the stop", code, why, time.Since(began))
	}
	if _, ok := holdwire.RunningDaemon(r.terminals); ok {
		t.Fatal("a daemon still runs after stop")
	}
	rec := holdwire.Judge(r.terminals, id)
	if rec.State != holdwire.StateLive || !rec.Locked || gone(rec.Meta.HolderPID) || gone(rec.Meta.ChildPID) {
		t.Fatalf("the session goes on with no daemon: %+v %+v", rec, rec.Meta)
	}

	r.start()
	s := r.session(id)
	if s.Ended != nil || s.Exit != nil || s.State != "live" || s.Label != "kept" || s.Kind != holdwire.KindAccount ||
		s.Account != 1 || s.Provider != "claude" || s.PID != rec.Meta.HolderPID || !s.Attachable {
		t.Fatalf("the new daemon lists it as it was: %+v", s)
	}
	again := r.attach(id)
	again.waitDoc("hello")
	again.send(0x2, []byte("x\r"))
	again.waitOut("got:x")
}

// --ctl-only opens no port even where server.toml names one; plain start
// opens it and says where.
func TestCtlOnlyOpensNoPort(t *testing.T) {
	r := newDaemonRig(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if err := os.WriteFile(filepath.Join(r.home, "server.toml"), []byte("[server]\nlisten = \""+addr+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, http := r.start("--ctl-only"); http != "" {
		t.Fatalf("--ctl-only said it serves http on %q", http)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatalf("--ctl-only left %s open", addr)
	}
	if doc, _ := r.status(); doc.HTTP != nil {
		t.Fatalf("status says http on %q", *doc.HTTP)
	}
	if _, _, code := r.rota("serve", "stop"); code != 0 {
		t.Fatal("serve stop failed")
	}

	if _, http := r.start(); http != addr {
		t.Fatalf("a plain start said http on %q, want %s", http, addr)
	}
	resp, err := (&nethttp.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("the port server.toml names is not open: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != nethttp.StatusOK {
		t.Fatalf("GET /v1/health on the port answered %d", resp.StatusCode)
	}
	if doc, _ := r.status(); doc.HTTP == nil || *doc.HTTP != addr {
		t.Fatalf("status says http on %v, want %s", doc.HTTP, addr)
	}
}

// stop --all ends the sessions too, and so asks first: off a terminal it
// needs --yes. Every record then reads ended and no daemon runs, and the
// session's claim is let go of only once its CLI has gone (Review Focus 1).
func TestStopAllAsksFirst(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id := r.newSession("doomed")
	p := r.attach(id)
	p.waitOut("ready 100x30")
	rec := holdwire.Judge(r.terminals, id)
	child, holder := rec.Meta.ChildPID, rec.Meta.HolderPID

	// At a terminal it asks, and anything but yes stops nothing.
	asked := r.cmd("serve", "stop", "--all")
	master, err := pty.Start(asked, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = asked.Wait(); close(exited) }()
	t.Cleanup(func() { _ = asked.Process.Kill(); <-exited; master.Close() })
	screen := watchScreen(master)
	screen.wait(t, "stop the rota daemon and end 1 session? [y/N] ")
	if _, err := master.Write([]byte("n\r")); err != nil {
		t.Fatal(err)
	}
	screen.wait(t, "rota: nothing was stopped")
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("stop --all never exited after its answer")
	}
	if code := asked.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("a declined stop --all exited %d", code)
	}
	// Off a terminal there is nobody to ask.
	if out, errb, code := r.rota("serve", "stop", "--all"); code != 2 || !strings.Contains(errb, "--yes") {
		t.Fatalf("stop --all off a terminal without --yes: exit %d %q %q", code, out, errb)
	}
	if _, ok := holdwire.RunningDaemon(r.terminals); !ok || gone(child) {
		t.Fatal("a refused stop --all stopped something")
	}

	// Watched while it stops: the first moment the record says ended, or
	// its lock is free, the CLI must already be gone.
	var (
		mu        sync.Mutex
		freed     bool
		cliThen   bool
		watchDone = make(chan struct{})
		stop      = make(chan struct{})
	)
	go func() {
		defer close(watchDone)
		dir := holdwire.Dir(r.terminals, id)
		for {
			select {
			case <-stop:
				return
			default:
			}
			m, err := holdwire.ReadMeta(dir)
			if !holdwire.Locked(holdwire.LockPath(dir)) || (err == nil && m.Exit != nil) {
				mu.Lock()
				freed, cliThen = true, gone(child)
				mu.Unlock()
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	out, errb, code := r.rota("serve", "stop", "--all", "--yes")
	close(stop)
	<-watchDone
	if code != 0 || out != "rota daemon stopped; 1 session ended\n" {
		t.Fatalf("stop --all --yes: exit %d %q %q\nlog:\n%s", code, out, errb, r.log())
	}
	mu.Lock()
	if !freed || !cliThen {
		t.Fatalf("the session read ended or let go of its lock (%v) while its CLI was still there (gone %v)", freed, cliThen)
	}
	mu.Unlock()
	rec = holdwire.Judge(r.terminals, id)
	if rec.State != holdwire.StateEnded || rec.Locked || rec.Meta.Exit == nil || !gone(child) || !gone(holder) {
		t.Fatalf("after stop --all the record reads ended: %+v %+v", rec, rec.Meta)
	}
	if _, ok := holdwire.RunningDaemon(r.terminals); ok {
		t.Fatal("a daemon still runs after stop --all")
	}
}

// With no daemon, stop --all hangs each holder up itself, by the rule a
// daemon stops one by, and its record reads ended once the CLI has gone.
func TestStopAllWithNoDaemonEndsEachSession(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id := r.newSession("orphan")
	r.attach(id).waitOut("ready 100x30")
	if _, _, code := r.rota("serve", "stop"); code != 0 {
		t.Fatal("serve stop failed")
	}
	rec := holdwire.Judge(r.terminals, id)
	if !rec.Locked {
		t.Fatalf("the session did not outlive its daemon: %+v", rec)
	}
	child := rec.Meta.ChildPID
	want := "no rota daemon was running for " + r.home + "; 1 session ended\n"
	if out, errb, code := r.rota("serve", "stop", "--all", "--yes"); code != 0 || out != want {
		t.Fatalf("stop --all --yes with no daemon: exit %d %q %q", code, out, errb)
	}
	rec = holdwire.Judge(r.terminals, id)
	if rec.State != holdwire.StateEnded || rec.Locked || rec.Meta.Exit == nil || !gone(child) {
		t.Fatalf("the record reads ended and the CLI is gone: %+v %+v", rec, rec.Meta)
	}
	if doc, code := r.status(); code != 3 || doc.Sessions.Live != 0 || doc.Sessions.Stopped != 1 {
		t.Fatalf("status with no daemon counts the records: exit %d %+v", code, doc)
	}
}

// The daemon logs to <store>/daemon.log, its owner's alone, and says
// nothing on stderr; a hangup does not stop it, and SIGTERM does.
func TestTheDaemonLogsToTheStore(t *testing.T) {
	r := newDaemonRig(t)
	cmd := r.cmd("serve", "start", "--foreground")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	answers := func() bool {
		c, err := rotaclient.Dial(r.terminals)
		return err == nil && c.Health() == nil
	}
	within(t, "the daemon answers on its control socket", 20*time.Second, answers)

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !answers() {
		t.Fatalf("a hangup stopped the daemon:\n%s", r.log())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon did not stop on SIGTERM")
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 || out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("the daemon exited %d and said %q on stdout and %q on stderr", code, out.String(), errb.String())
	}
	path := filepath.Join(r.home, "daemon.log")
	if m := modeOf(t, path); m != 0o600 {
		t.Fatalf("daemon.log is %o", m)
	}
	log := r.log()
	for _, want := range []string{
		`msg="rota daemon starting" pid=` + strconv.Itoa(cmd.Process.Pid),
		`msg="rota daemon started"`, "ctl=" + r.sock,
		`msg="rota daemon stopped"`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("daemon.log does not say %q:\n%s", want, log)
		}
	}
	if holdwire.Locked(holdwire.DaemonLock(r.terminals)) || proc.Alive(cmd.Process.Pid) && !gone(cmd.Process.Pid) {
		t.Fatal("the daemon still holds the store")
	}
}
