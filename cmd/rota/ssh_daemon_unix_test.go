//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	nethttp "net/http"
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
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
)

// The SSH server as the daemon and foreground rota serve run it, reached
// the way a person reaches it: golang.org/x/crypto/ssh's client, with a key
// made here, on a loopback port. Nothing here reads ~/.ssh or an agent.

/* ------------------------------------------------------------ the rig --- */

// sshwRig is the daemon rig with a server.toml that turns SSH on for one
// person, me, who may control and signs in with key alone.
type sshwRig struct {
	*daemonRig
	key  ssh.Signer
	addr string // [ssh] listen
}

func newSSHRig(t *testing.T) *sshwRig {
	t.Helper()
	sshwNoAgent(t)
	r := &sshwRig{daemonRig: newDaemonRig(t), key: sshwKey(t), addr: sshwFreePort(t)}
	r.write(sshwFile(r.addr, r.key))
	return r
}

// write puts body in the store's server.toml, its owner's alone.
func (r *sshwRig) write(body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.home, "server.toml"), []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// fingerprint is the SHA256 fingerprint of the host key in the store.
func (r *sshwRig) fingerprint() string {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.home, "ssh_host_ed25519_key"))
	if err != nil {
		r.t.Fatal(err)
	}
	k, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		r.t.Fatal(err)
	}
	return ssh.FingerprintSHA256(k.PublicKey())
}

// signIn is me, signed in to the daemon's SSH server, which must show the
// host key in the store.
func (r *sshwRig) signIn() *ssh.Client {
	r.t.Helper()
	return sshwDial(r.t, r.addr, r.fingerprint(), r.key)
}

// sshwNoAgent leaves this test no SSH agent to find.
func sshwNoAgent(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	os.Unsetenv("SSH_AUTH_SOCK")
}

func sshwKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// sshwFreePort is a loopback address nothing listens on now.
func sshwFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// sshwFile is a server.toml with SSH on at addr for me, who may control and
// signs in by key, with no password and no auth.local_user.
func sshwFile(addr string, key ssh.Signer) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key.PublicKey())))
	return "[ssh]\nenabled = true\nlisten = \"" + addr + "\"\n\n" +
		"[[users]]\nname = \"me\"\nrole = \"control\"\nssh_keys = [\"" + line + "\"]\n"
}

// sshwDial signs in to addr as me with key, where the server must show the
// host key fp.
func sshwDial(t *testing.T, addr, fp string, key ssh.Signer) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: "me",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(k); got != fp {
				return errors.New("host key " + got + ", want " + fp)
			}
			return nil
		},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("signing in over SSH at %s: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// sshwRun runs one command line on a fresh session channel, with no pty.
func sshwRun(t *testing.T, c *ssh.Client, line string) (stdout, stderr string, code int) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	err = sess.Run(line)
	var exit *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitStatus()
	default:
		t.Fatalf("ssh %q: %v", line, err)
	}
	return out.String(), errb.String(), code
}

// sshwBuf is what a channel has written, read while it writes.
type sshwBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *sshwBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *sshwBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// sshwTerm is one command on a session channel with a pty, as `ssh -t host
// <line>` runs it.
type sshwTerm struct {
	t        *testing.T
	sess     *ssh.Session
	in       io.WriteCloser
	out, err sshwBuf
	done     chan error
}

func sshwOpen(t *testing.T, c *ssh.Client, line string, cols, rows int) *sshwTerm {
	t.Helper()
	a := &sshwTerm{t: t, done: make(chan error, 1)}
	var err error
	if a.sess, err = c.NewSession(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.sess.Close() })
	a.sess.Stdout, a.sess.Stderr = &a.out, &a.err
	if a.in, err = a.sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := a.sess.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := a.sess.Start(line); err != nil {
		t.Fatal(err)
	}
	go func() { a.done <- a.sess.Wait() }()
	return a
}

func (a *sshwTerm) typ(s string) {
	a.t.Helper()
	if _, err := io.WriteString(a.in, s); err != nil {
		a.t.Fatalf("typing %q over SSH: %v", s, err)
	}
}

func (a *sshwTerm) wait(want string) {
	a.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(a.out.String(), want); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			a.t.Fatalf("%q never showed over SSH; it showed:\n%q\nstderr %q", want, a.out.String(), a.err.String())
		}
	}
}

// end waits for the command to end and gives its exit-status.
func (a *sshwTerm) end() int {
	a.t.Helper()
	select {
	case err := <-a.done:
		var exit *ssh.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &exit):
			return exit.ExitStatus()
		}
		a.t.Fatalf("the command over SSH ended with no exit-status: %v; stderr %q", err, a.err.String())
	case <-time.After(30 * time.Second):
		a.t.Fatalf("the command over SSH never ended; stderr %q", a.err.String())
	}
	return -1
}

// sshwKeyboards is who held the keyboard, by connection, in each keyboard
// frame p has been sent, in order.
func sshwKeyboards(p *peer) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, d := range p.doc {
		if d["type"] == "keyboard" {
			conn, _ := d["holder_conn"].(string)
			out = append(out, conn)
		}
	}
	return out
}

// sshwHolder waits until the last keyboard frame p was sent names a holder
// that is not one of not, and says it.
func sshwHolder(t *testing.T, p *peer, not ...string) string {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if k := sshwKeyboards(p); len(k) > 0 {
			if last := k[len(k)-1]; last != "" && !slices.Contains(not, last) {
				return last
			}
		}
	}
	t.Fatalf("the keyboard never went to a connection other than %q: %q", not, sshwKeyboards(p))
	return ""
}

/* ---------------------------------------------------------- the daemon --- */

// The daemon serves SSH where server.toml turns it on: serve start says
// where, the host key is made in the store, serve status names both, and a
// person signed in by key sees and attaches to a session made over
// ctl.sock, and leaves it running.
func TestTheDaemonServesSSH(t *testing.T) {
	r := newSSHRig(t)
	if _, http := r.start(); http != "" || r.ssh != r.addr {
		t.Fatalf("serve start said http on %q and ssh on %q, want no http and ssh on %s", http, r.ssh, r.addr)
	}
	key := filepath.Join(r.home, "ssh_host_ed25519_key")
	if m := modeOf(t, key); m != 0o600 {
		t.Fatalf("%s is %o, want 600", key, m)
	}
	fp := r.fingerprint()
	if !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("the host key's fingerprint is %q", fp)
	}
	doc, code := r.status()
	if code != 0 || doc.SSH == nil || doc.SSH.Addr != r.addr || doc.SSH.HostKey != fp || doc.HTTP != nil {
		t.Fatalf("status --json: exit %d, ssh %+v, http %v; want ssh on %s with %s", code, doc.SSH, doc.HTTP, r.addr, fp)
	}
	if out, errb, code := r.rota("serve", "status"); code != 0 || !strings.Contains(out, "\nssh on "+r.addr+", host key "+fp+"\n") {
		t.Fatalf("serve status: exit %d %q %q", code, out, errb)
	}
	if said := regexp.MustCompile(`msg="rota daemon serves ssh" .*addr=(\S+) fingerprint=(\S+)`).FindStringSubmatch(r.log()); said == nil || said[1] != r.addr || said[2] != fp {
		t.Fatalf("daemon.log does not say where it serves ssh:\n%s", r.log())
	}

	id := r.newSession("api")
	c := r.signIn()
	out, errb, code := sshwRun(t, c, "list")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "  api  ") {
		t.Fatalf("list over SSH: exit %d %q %q; want %s, api", code, out, errb, id)
	}

	a := sshwOpen(t, c, "attach api", 120, 40)
	a.wait("ready 100x30")
	a.typ("hi\r")
	a.wait("got:hi")
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d over SSH: exit %d; stderr %q", code, a.err.String())
	}
	if got, want := a.err.String(), "rota: detached from api; attach api returns to it\r\n"; got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
	if s := r.session(id); s.State != "live" || s.Ended != nil {
		t.Fatalf("after a detach over SSH the session is %+v", s)
	}
	// me was shown the chord, and the store's terminals remember it.
	if raw, err := os.ReadFile(filepath.Join(r.terminals, "taught")); err != nil || string(raw) != "me\n" {
		t.Fatalf("the taught file says %q (%v), want me", raw, err)
	}
}

// One person, two terminals: rota run --attach at the desk types as local,
// whom the file makes me, and me attached over SSH from a phone. Whichever
// types takes the keyboard, and what each types reaches the CLI.
func TestTheKeyboardFollowsMeFromDeskToPhone(t *testing.T) {
	r := newSSHRig(t)
	r.start()
	id := r.newSession("api")
	// A viewer of the same person that never types, to see each keyboard
	// frame every viewer is sent.
	eye := r.attach(id)
	eye.waitDoc("hello")

	desk := r.attachHere(100, 30, true, "api")
	desk.waitAfter(0, "ready 100x30", 20*time.Second)
	desk.typ("desk\r")
	eye.waitOut("got:desk")
	atDesk := sshwHolder(t, eye)

	phone := sshwOpen(t, r.signIn(), "attach api", 100, 30)
	phone.wait("got:desk")
	phone.typ("phone\r")
	eye.waitOut("got:phone")
	onPhone := sshwHolder(t, eye, atDesk)
	desk.waitAfter(0, "got:phone", 20*time.Second)

	desk.typ("back\r")
	eye.waitOut("got:back")
	if again := sshwHolder(t, eye, onPhone); again != atDesk {
		t.Fatalf("typing at the desk again gave the keyboard to %q, want the desk's %q", again, atDesk)
	}
	phone.wait("got:back")

	phone.typ("\x1cd")
	if code := phone.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d on the phone: exit %d; stderr %q", code, phone.err.String())
	}
	desk.typ("\x1cd")
	if code := desk.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d at the desk: exit %d; stderr %q", code, desk.stderr.String())
	}
}

// An SSH server that cannot listen is a daemon that did not start, and
// serve start says why from its log.
func TestASSHPortTakenIsADaemonThatDidNotStart(t *testing.T) {
	r := newSSHRig(t)
	taken, err := net.Listen("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	out, errb, code := r.rota("serve", "start")
	if code == 0 || !strings.Contains(errb, "the rota daemon did not start") || !strings.Contains(errb, "the SSH server cannot start") {
		t.Fatalf("serve start with the SSH port taken: exit %d %q %q", code, out, errb)
	}
	if _, ok := holdwire.RunningDaemon(r.terminals); ok {
		t.Fatal("a daemon runs whose SSH server could not listen")
	}
}

// A daemon a session started by itself (--ctl-only) opens no SSH port,
// whatever the file says, makes no host key, and says nothing of SSH.
func TestCtlOnlyOpensNoSSH(t *testing.T) {
	r := newSSHRig(t)
	r.start("--ctl-only")
	if r.ssh != "" {
		t.Fatalf("--ctl-only said it serves ssh on %q", r.ssh)
	}
	if c, err := net.DialTimeout("tcp", r.addr, time.Second); err == nil {
		c.Close()
		t.Fatalf("--ctl-only left %s open", r.addr)
	}
	raw, err := os.ReadFile(holdwire.DaemonPIDFile(r.terminals))
	if err != nil || strings.Contains(string(raw), `"ssh`) {
		t.Fatalf("daemon.pid names ssh: %s %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(r.home, "ssh_host_ed25519_key")); !os.IsNotExist(err) {
		t.Fatalf("--ctl-only made a host key: %v", err)
	}
	sshwNoSSHKey(t, r.daemonRig)
}

// serve start finding a daemon that does not run the SSH server the file
// turns on, one started on its control socket alone, says how to have it;
// finding one that runs it, it says only that it runs.
func TestStartSaysWhySSHIsOff(t *testing.T) {
	r := newSSHRig(t)
	r.start("--ctl-only")
	already := "rota daemon already running for " + r.home + " ("
	out, errb, code := r.rota("serve", "start")
	if code != 0 || !strings.HasPrefix(out, already) || !strings.HasSuffix(out,
		")\nit does not run the SSH server [ssh] turns on: a daemon started with --ctl-only, as rota run --detach starts one, never does; rota serve stop, then rota serve start, runs it\n") {
		t.Fatalf("serve start with a --ctl-only daemon running: exit %d %q %q", code, out, errb)
	}
	if out, errb, code := r.rota("serve", "start", "--ctl-only"); code != 0 || strings.Count(out, "\n") != 1 {
		t.Fatalf("serve start --ctl-only with one running: exit %d %q %q", code, out, errb)
	}
	if _, _, code := r.rota("serve", "stop"); code != 0 {
		t.Fatal("serve stop failed")
	}
	r.start()
	if out, errb, code := r.rota("serve", "start"); code != 0 || !strings.HasPrefix(out, already) || strings.Count(out, "\n") != 1 {
		t.Fatalf("serve start with an SSH daemon running: exit %d %q %q", code, out, errb)
	}
}

// serve start finding a daemon, with a file that does not turn SSH on, says
// the one line it always said, and nothing of SSH: with no file at all
// against a daemon on its port, and with [ssh] enabled = false against one
// on its control socket alone.
func TestStartWithoutSSHSaysOnlyThatItRuns(t *testing.T) {
	r := newSSHRig(t)
	if err := os.Remove(filepath.Join(r.home, "server.toml")); err != nil {
		t.Fatal(err)
	}
	pid, _ := r.start()
	want := fmt.Sprintf("rota daemon already running for %s (pid %d)\n", r.home, pid)
	if out, errb, code := r.rota("serve", "start"); code != 0 || out != want || errb != "" {
		t.Fatalf("serve start with no file and a daemon running: exit %d %q %q, want %q", code, out, errb, want)
	}
	if _, _, code := r.rota("serve", "stop"); code != 0 {
		t.Fatal("serve stop failed")
	}
	pid, _ = r.start("--ctl-only")
	r.write(strings.Replace(sshwFile(r.addr, r.key), "enabled = true", "enabled = false", 1))
	want = fmt.Sprintf("rota daemon already running for %s (pid %d)\n", r.home, pid)
	if out, errb, code := r.rota("serve", "start"); code != 0 || out != want || errb != "" {
		t.Fatalf("serve start with [ssh] off and a --ctl-only daemon running: exit %d %q %q, want %q", code, out, errb, want)
	}
}

// [ssh] alone opens no HTTP port: status says http null, and ssh.
func TestOnlySSHOpensNoHTTP(t *testing.T) {
	r := newSSHRig(t)
	if _, http := r.start(); http != "" {
		t.Fatalf("[ssh] alone said http on %q", http)
	}
	out, errb, code := r.rota("--json", "serve", "status")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 {
		t.Fatalf("status --json: exit %d %q %q: %v", code, out, errb, err)
	}
	if string(doc["http"]) != "null" {
		t.Fatalf(`status --json says "http": %s, want null`, doc["http"])
	}
	var ssh struct {
		Addr    string `json:"addr"`
		HostKey string `json:"host_key"`
	}
	if err := json.Unmarshal(doc["ssh"], &ssh); err != nil || ssh.Addr != r.addr || ssh.HostKey != r.fingerprint() {
		t.Fatalf(`status --json says "ssh": %s (%v)`, doc["ssh"], err)
	}
}

// With no [ssh], serve status says what it always said: no ssh key in its
// JSON and no ssh line in its text.
func TestNoSSHSaysNothingOfIt(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	if r.ssh != "" {
		t.Fatalf("a daemon with no server.toml said ssh on %q", r.ssh)
	}
	sshwNoSSHKey(t, r)
	if out, errb, code := r.rota("serve", "status"); code != 0 || strings.Contains(out, "ssh") {
		t.Fatalf("serve status with no [ssh]: exit %d %q %q", code, out, errb)
	}
}

// sshwNoSSHKey says serve status --json has no ssh key.
func sshwNoSSHKey(t *testing.T, r *daemonRig) {
	t.Helper()
	out, errb, code := r.rota("--json", "serve", "status")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 {
		t.Fatalf("status --json: exit %d %q %q: %v", code, out, errb, err)
	}
	if v, ok := doc["ssh"]; ok {
		t.Fatalf(`status --json has "ssh": %s`, v)
	}
}

// Stopping the daemon ends SSH first: an attach over it is told, ends lost
// with its line and its exit-status, and the session goes on, its holder
// alive and holding its lock.
func TestStoppingTheDaemonEndsSSHFirst(t *testing.T) {
	r := newSSHRig(t)
	r.start()
	id := r.newSession("api")
	a := sshwOpen(t, r.signIn(), "attach api", 120, 40)
	a.wait("ready 100x30")
	if out, errb, code := r.rota("serve", "stop"); code != 0 {
		t.Fatalf("serve stop: exit %d %q %q", code, out, errb)
	}
	if code := a.end(); code != 1 {
		t.Fatalf("an attach over SSH as the daemon stopped exited %d, want 1; stderr %q", code, a.err.String())
	}
	if got, want := a.err.String(), "rota: lost api: the SSH server is stopping; attach api tries again\r\n"; got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
	rec := holdwire.Judge(r.terminals, id)
	if rec.State != holdwire.StateLive || !rec.Locked || rec.Meta == nil || gone(rec.Meta.HolderPID) {
		t.Fatalf("the session did not go on: %+v %+v", rec, rec.Meta)
	}
	if c, err := net.DialTimeout("tcp", r.addr, time.Second); err == nil {
		c.Close()
		t.Fatalf("%s is still open after the daemon stopped", r.addr)
	}
}

/* ---------------------------------------------------- foreground serve --- */

// serveSSHFor starts foreground `rota serve` on a free loopback port with
// the file body names and the token x, waits for it to say it serves ssh,
// hands its HTTP address, its SSH address and its host key's fingerprint
// to while, and then stops it as an interrupt would.
func serveSSHFor(t *testing.T, body string, while func(base, addr, fp string)) (stderr string, code int) {
	t.Helper()
	conf := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	httpAddr := sshwFreePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	was := serveUntil
	serveUntil = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	defer func() { serveUntil = was }()

	errb := &sshwBuf{}
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"serve", httpAddr, "--token", "x", "--config", conf, "--refresh-every=0"}, io.Discard, errb)
	}()
	said := regexp.MustCompile(`rota \S+ serving ssh on (\S+) \(host key (SHA256:\S+)\)\n`)
	var m []string
	for deadline := time.Now().Add(20 * time.Second); m == nil; time.Sleep(10 * time.Millisecond) {
		select {
		case code := <-done:
			t.Fatalf("serve stopped before it served ssh: %d\n%s", code, errb.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve never said it serves ssh:\n%s", errb.String())
		}
		m = said.FindStringSubmatch(errb.String())
	}
	while("http://"+httpAddr, m[1], m[2])
	cancel()
	select {
	case code = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
	return errb.String(), code
}

// Foreground rota serve with [ssh] and [routes] terminal serves SSH on the
// store's own host key, and SSH reaches the terminals it runs.
func TestForegroundServeServesSSH(t *testing.T) {
	sshwNoAgent(t)
	shortHome(t)
	t.Setenv("PATH", holdFake(t, "claude", fakecli.Spec{})+string(os.PathListSeparator)+os.Getenv("PATH"))
	key, addr := sshwKey(t), sshwFreePort(t)
	body := "[routes]\nterminal = true\n\n" + sshwFile(addr, key)
	var a *sshwTerm
	stderr, code := serveSSHFor(t, body, func(base, said, fp string) {
		if said != addr {
			t.Errorf("serve said ssh on %s, want %s", said, addr)
		}
		raw, err := os.ReadFile(filepath.Join(os.Getenv("ROTA_HOME"), "ssh_host_ed25519_key"))
		if err != nil {
			t.Fatalf("the store has no host key: %v", err)
		}
		if k, err := ssh.ParsePrivateKey(raw); err != nil || ssh.FingerprintSHA256(k.PublicKey()) != fp {
			t.Errorf("serve said host key %s, and the store's is another (%v)", fp, err)
		}
		req, _ := nethttp.NewRequest("POST", base+"/v1/terminals", strings.NewReader(`{"account":1,"cols":100,"rows":30,"label":"api"}`))
		req.Header.Set("Authorization", "Bearer x")
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&nethttp.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != nethttp.StatusCreated {
			t.Fatalf("POST /v1/terminals answered %d", resp.StatusCode)
		}
		c := sshwDial(t, addr, fp, key)
		out, errb, code := sshwRun(t, c, "list")
		if code != 0 || !strings.Contains(out, "  api  ") {
			t.Errorf("list over SSH: exit %d %q %q", code, out, errb)
		}
		a = sshwOpen(t, c, "attach api", 120, 40)
		a.wait("ready 100x30")
	})
	if code != 0 {
		t.Fatalf("serve exited %d:\n%s", code, stderr)
	}
	// The SSH server went before the terminals it reaches: the attach was
	// told the server is stopping, not that its terminal went, and its
	// connection was over before the terminal was killed.
	out, killed := strings.Index(stderr, `msg="ssh signed out"`), strings.Index(stderr, `msg="terminal killed"`)
	if out < 0 || killed < 0 || out > killed {
		t.Fatalf("the SSH connection did not end before the terminal was killed:\n%s", stderr)
	}
	if code := a.end(); code != 1 {
		t.Fatalf("an attach over SSH as serve stopped exited %d, want 1; stderr %q", code, a.err.String())
	}
	if got, want := a.err.String(), "rota: lost api: the SSH server is stopping; attach api tries again\r\n"; got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatalf("%s is still open after serve stopped", addr)
	}
}

// A foreground serve that cannot serve its port lets go of its SSH server
// too: nothing is left listening once it has returned.
func TestForegroundServeLetsGoOfSSHWhenItStops(t *testing.T) {
	sshwNoAgent(t)
	taken := refusing(t)
	addr := sshwFreePort(t)
	conf := filepath.Join(t.TempDir(), "server.toml")
	body := "[routes]\nterminal = true\n\n" + sshwFile(addr, sshwKey(t))
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	errb := &sshwBuf{}
	done := make(chan int, 1)
	go func() { done <- run([]string{"serve", taken, "--config", conf, "--refresh-every=0"}, io.Discard, errb) }()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(errb.String(), "serving ssh on "+addr) {
			t.Fatalf("serve on a taken port: exit %d\n%s", code, errb.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("serve on a taken port never returned:\n%s", errb.String())
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("serve returned with %s still taken: %v", addr, err)
	}
	l.Close()
}

// Without [routes] terminal there is nothing for SSH to reach in
// foreground serve, which says so and serves nothing.
func TestForegroundSSHNeedsTerminals(t *testing.T) {
	sshwNoAgent(t)
	addr := refusing(t)
	conf := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(conf, []byte(sshwFile(sshwFreePort(t), sshwKey(t))), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run([]string{"serve", addr, "--config", conf}, &out, &errb)
	want := "[ssh] enabled needs [routes] terminal in rota serve: SSH reaches the terminals and nothing else; rota serve start serves held sessions without it"
	if code != 2 || !strings.Contains(errb.String(), want) {
		t.Fatalf("serve with [ssh] and no [routes] terminal: exit %d %q %q", code, out.String(), errb.String())
	}
}

/* ------------------------------------------------------- the options --- */

// sshOptions reads the file as the SSH server needs it: a bare port is
// every interface, the store's host key is made only where the file names
// none, only users with keys sign in, each with their role, and the chord
// is the one a local attach reads.
func TestSSHOptions(t *testing.T) {
	home := t.TempDir()
	terminals := filepath.Join(home, "terminals")
	cfg := api.DefaultConfig()
	cfg.SSH.Enabled, cfg.SSH.Listen = true, "2222"
	cfg.Users = []api.UserEntry{
		{Name: "me", Role: "control", SSHKeys: []string{"ssh-ed25519 AAAA me"}},
		{Name: "eye", Role: "watch", SSHKeys: []string{"ssh-ed25519 BBBB eye"}},
		{Name: "page", Role: "control", Password: "derived"},
	}
	o, err := sshOptions(cfg, home, terminals, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.Listen != "0.0.0.0:2222" || o.HostKey != filepath.Join(home, "ssh_host_ed25519_key") || !o.NewHostKey {
		t.Fatalf("listen %q, host key %q (new %v)", o.Listen, o.HostKey, o.NewHostKey)
	}
	if len(o.Users) != 2 || o.Users[0].Name != "me" || o.Users[0].Role != api.RoleControl ||
		o.Users[1].Name != "eye" || o.Users[1].Role != api.RoleWatch || o.Users[1].Keys[0] != "ssh-ed25519 BBBB eye" {
		t.Fatalf("users %+v", o.Users)
	}
	if o.Key != attach.DaemonKey(terminals) || o.Taught == nil || o.Handler == nil || o.TUI != nil {
		t.Fatalf("key %#x, taught %v, handler %v, tui %v", o.Key, o.Taught != nil, o.Handler != nil, o.TUI != nil)
	}
	cfg.SSH.HostKey, cfg.SSH.Listen = "/etc/rota/host_key", "127.0.0.1:2200"
	if o, err = sshOptions(cfg, home, terminals, nil, nil); err != nil || o.HostKey != "/etc/rota/host_key" || o.NewHostKey || o.Listen != "127.0.0.1:2200" {
		t.Fatalf("a named host key: %+v %v", o, err)
	}
}
