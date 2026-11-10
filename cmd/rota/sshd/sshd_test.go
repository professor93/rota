package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/api"
)

// sshdRig is one server on 127.0.0.1 with a throwaway host key and two
// people, me (control) and eye (watch), each with a key made for the test.
// It registers test commands of its own: echo, who, tty and block.
type sshdRig struct {
	t       *testing.T
	s       *Server
	me, eye ssh.Signer
	log     *sshdLog
	ran     chan []string // what echo ran with
	blocked chan struct{} // block has started
	ended   chan struct{} // block has seen its context end
	serve   chan error
}

// sshdLog is the server's log, safe to read while it writes.
type sshdLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *sshdLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *sshdLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func sshdKey(t *testing.T) ssh.Signer {
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

func sshdLine(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

// sshdKnob sets a timing knob for one test and puts it back after.
func sshdKnob(t *testing.T, knob *time.Duration, d time.Duration) {
	old := *knob
	*knob = d
	t.Cleanup(func() { *knob = old })
}

// sshdPrincipal is the Handler: GET /v1/session answers with the person.
func sshdPrincipal(p *api.Principal) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/session", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(p)
	})
	return mux
}

func sshdStart(t *testing.T, edit ...func(*Options)) *sshdRig {
	t.Helper()
	r := &sshdRig{
		t: t, me: sshdKey(t), eye: sshdKey(t), log: &sshdLog{},
		ran: make(chan []string, 16), blocked: make(chan struct{}, 16),
		ended: make(chan struct{}, 16), serve: make(chan error, 1),
	}
	commands["echo"] = func(_ context.Context, ch *Channel, args []string) int {
		select {
		case r.ran <- args:
		default:
		}
		fmt.Fprintf(ch.Out, "%q", args)
		return 0
	}
	commands["who"] = func(_ context.Context, ch *Channel, _ []string) int {
		w := httptest.NewRecorder()
		ch.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/v1/session", nil))
		fmt.Fprintf(ch.Out, "%s%s", w.Body.String(), ch.Login)
		return 0
	}
	commands["tty"] = func(_ context.Context, ch *Channel, _ []string) int {
		fmt.Fprintf(ch.Out, "%v %s %dx%d", ch.PTY, ch.Term, ch.Cols, ch.Rows)
		for {
			select {
			case z := <-ch.Resize:
				fmt.Fprintf(ch.Out, " %dx%d", z[0], z[1])
			default:
				return 3
			}
		}
	}
	commands["block"] = func(ctx context.Context, _ *Channel, _ []string) int {
		r.blocked <- struct{}{}
		<-ctx.Done()
		r.ended <- struct{}{}
		return 0
	}
	t.Cleanup(func() {
		for _, c := range []string{"echo", "who", "tty", "block"} {
			delete(commands, c)
		}
	})
	o := Options{
		Listen:     "127.0.0.1:0",
		HostKey:    filepath.Join(t.TempDir(), "ssh_host_ed25519_key"),
		NewHostKey: true,
		Users: []User{
			{Name: "me", Role: api.RoleControl, Keys: []string{sshdLine(r.me.PublicKey())}},
			{Name: "eye", Role: api.RoleWatch, Keys: []string{sshdLine(r.eye.PublicKey())}},
		},
		Handler: sshdPrincipal,
		Log:     slog.New(slog.NewTextHandler(r.log, nil)),
	}
	for _, e := range edit {
		e(&o)
	}
	s, err := Listen(o)
	if err != nil {
		t.Fatal(err)
	}
	r.s = s
	go func() { r.serve <- s.Serve() }()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		select {
		case err := <-r.serve:
			if err != nil {
				t.Errorf("Serve after Close: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after Close")
		}
	})
	return r
}

func (r *sshdRig) config(user string, auth ...ssh.AuthMethod) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: user,
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(k); got != r.s.Fingerprint() {
				return fmt.Errorf("host key %s, want %s", got, r.s.Fingerprint())
			}
			return nil
		},
		Timeout: 5 * time.Second,
	}
}

func (r *sshdRig) dial(user string, auth ...ssh.AuthMethod) (*ssh.Client, error) {
	return ssh.Dial("tcp", r.s.Addr(), r.config(user, auth...))
}

func (r *sshdRig) signIn(k ssh.Signer) *ssh.Client {
	r.t.Helper()
	c, err := r.dial("me", ssh.PublicKeys(k))
	if err != nil {
		r.t.Fatalf("sign in: %v", err)
	}
	r.t.Cleanup(func() { c.Close() })
	return c
}

// sshdRun runs one command line on a fresh session channel.
func sshdRun(t *testing.T, c *ssh.Client, line string) (stdout, stderr string, code int) {
	t.Helper()
	// A session closed just before is counted closed by the server a moment
	// after the client sees it closed.
	var sess *ssh.Session
	sshdEventually(t, func() (err error) {
		sess, err = c.NewSession()
		return err
	})
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	err := sess.Run(line)
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

// countingSigner says how often its public key was asked for: the client
// asks for it just before offering it.
type countingSigner struct {
	ssh.Signer
	asked atomic.Int32
}

func (c *countingSigner) PublicKey() ssh.PublicKey {
	c.asked.Add(1)
	return c.Signer.PublicKey()
}

func TestOnlyAListedKeySignsIn(t *testing.T) {
	r := sshdStart(t)
	if c, err := r.dial("me", ssh.PublicKeys(sshdKey(t))); err == nil {
		c.Close()
		t.Fatal("an unlisted key signed in")
	}
	c := r.signIn(r.me)
	if out, _, code := sshdRun(t, c, "echo hi"); code != 0 || out != `["hi"]` {
		t.Fatalf("echo: %q %d", out, code)
	}

	// Three tries: the fourth key is never asked for, and so neither is me's.
	var keys []ssh.Signer
	var counted []*countingSigner
	for range 4 {
		k := &countingSigner{Signer: sshdKey(t)}
		counted = append(counted, k)
		keys = append(keys, k)
	}
	mine := &countingSigner{Signer: r.me}
	keys = append(keys, mine)
	if c, err := r.dial("me", ssh.PublicKeys(keys...)); err == nil {
		c.Close()
		t.Fatal("signed in after three unlisted keys")
	}
	for i, k := range counted[:3] {
		if k.asked.Load() == 0 {
			t.Errorf("key %d was never offered", i+1)
		}
	}
	if n := counted[3].asked.Load(); n != 0 {
		t.Errorf("the fourth key was offered (%d)", n)
	}
	if n := mine.asked.Load(); n != 0 {
		t.Errorf("me's key was offered after three failures (%d)", n)
	}
}

func TestAnRSAKeySignsWithSHA2Only(t *testing.T) {
	if testing.Short() {
		t.Skip("makes an RSA key")
	}
	k := keysRSA(t, 2048)
	r := sshdStart(t, func(o *Options) {
		o.Users[0].Keys = append(o.Users[0].Keys, sshdLine(k.PublicKey()))
	})
	c := r.signIn(k)
	if _, _, code := sshdRun(t, c, "echo"); code != 0 {
		t.Fatalf("echo: %d", code)
	}
	sha1, err := ssh.NewSignerWithAlgorithms(k.(ssh.AlgorithmSigner), []string{ssh.KeyAlgoRSA})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := r.dial("me", ssh.PublicKeys(sha1)); err == nil {
		c.Close()
		t.Fatal("an ssh-rsa (SHA-1) signature signed in")
	}
}

func TestNoPasswordAndNoKeyboardInteractive(t *testing.T) {
	r := sshdStart(t)
	asked := 0
	kbd := ssh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) {
		asked++
		return nil, nil
	})
	c, err := r.dial("me", ssh.Password("x"), kbd)
	if err == nil {
		c.Close()
		t.Fatal("signed in with a password")
	}
	if !strings.Contains(err.Error(), "attempted methods [none]") {
		t.Errorf("error %q: the server offered more than publickey", err)
	}

	// The methods the server names after each failure, and a client that
	// tries password and keyboard-interactive although they are not named.
	var allowed [][]string
	force := []ssh.AuthMethod{ssh.Password("x"), kbd}
	cfg := r.config("me")
	cfg.AuthCallback = func(ctx *ssh.ClientAuthContext) (ssh.AuthMethod, error) {
		allowed = append(allowed, ctx.AllowedMethods)
		if len(force) == 0 {
			return nil, errors.New("done")
		}
		m := force[0]
		force = force[1:]
		return m, nil
	}
	if c, err := ssh.Dial("tcp", r.s.Addr(), cfg); err == nil {
		c.Close()
		t.Fatal("signed in by forcing a password or keyboard-interactive")
	}
	if len(allowed) == 0 {
		t.Fatal("the server named no methods")
	}
	for _, m := range allowed {
		if len(m) != 1 || m[0] != "publickey" {
			t.Errorf("the server offered %q; want [publickey]", m)
		}
	}
	if asked != 0 {
		t.Errorf("keyboard-interactive asked %d questions", asked)
	}
	if strings.Contains(r.log.String(), "ssh signed in") {
		t.Errorf("somebody signed in:\n%s", r.log)
	}
}

func TestOnlySessionChannels(t *testing.T) {
	r := sshdStart(t)
	c := r.signIn(r.me)
	direct := ssh.Marshal(struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}{"127.0.0.1", 22, "127.0.0.1", 5000})
	_, _, err := c.OpenChannel("direct-tcpip", direct)
	var oce *ssh.OpenChannelError
	if !errors.As(err, &oce) || oce.Reason != ssh.Prohibited {
		t.Fatalf("direct-tcpip: %v; want Prohibited", err)
	}
	for _, kind := range []string{"x11", "auth-agent@openssh.com", "forwarded-tcpip", "direct-streamlocal@openssh.com"} {
		if _, _, err := c.OpenChannel(kind, nil); !errors.As(err, &oce) || oce.Reason != ssh.Prohibited {
			t.Errorf("%s: %v; want Prohibited", kind, err)
		}
	}
	forward := ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"127.0.0.1", 0})
	for _, kind := range []string{"tcpip-forward", "streamlocal-forward@openssh.com", "cancel-tcpip-forward", "no-more-sessions@openssh.com"} {
		ok, _, err := c.SendRequest(kind, true, forward)
		if err != nil || ok {
			t.Errorf("%s: %v %v; want refused", kind, ok, err)
		}
	}
	if ok, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil || !ok {
		t.Errorf("the client's keepalive: %v %v; want answered", ok, err)
	}

	var open []*ssh.Session
	for i := range maxChannels {
		s, err := c.NewSession()
		if err != nil {
			t.Fatalf("session %d: %v", i+1, err)
		}
		open = append(open, s)
	}
	if _, err := c.NewSession(); err == nil {
		t.Fatalf("session %d was opened", maxChannels+1)
	}
	// A closed channel's place comes back once the server has counted it
	// closed; the connection is unharmed by all of it.
	open[0].Close()
	sshdEventually(t, func() error {
		s, err := c.NewSession()
		if err != nil {
			return err
		}
		defer s.Close()
		var out bytes.Buffer
		s.Stdout = &out
		if err := s.Run("echo ok"); err != nil || out.String() != `["ok"]` {
			t.Fatalf("echo after: %q %v", out.String(), err)
		}
		return nil
	})
	for _, s := range open[1:] {
		s.Close()
	}
}

func TestARequestIsRefusedByName(t *testing.T) {
	r := sshdStart(t)
	c := r.signIn(r.me)
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Setenv("LD_PRELOAD", "x"); err == nil {
		t.Error("env LD_PRELOAD=x was accepted")
	}
	if err := sess.Setenv("TERM", "xterm\x1b]2;x\a"); err == nil {
		t.Error("a TERM with an escape in it was accepted")
	}
	if err := sess.Setenv("TERM", "xterm"); err != nil {
		t.Errorf("env TERM=xterm: %v", err)
	}
	if err := sess.RequestSubsystem("sftp"); err == nil {
		t.Error("subsystem sftp was accepted")
	}
	x11 := ssh.Marshal(struct {
		Single   bool
		Protocol string
		Cookie   string
		Screen   uint32
	}{false, "MIT-MAGIC-COOKIE-1", "00", 0})
	for _, req := range []struct {
		kind    string
		payload []byte
	}{
		{"auth-agent-req@openssh.com", nil},
		{"x11-req", x11},
		{"signal", ssh.Marshal(struct{ Signal string }{"KILL"})},
		{"break", ssh.Marshal(struct{ Len uint32 }{100})},
		{"keepalive@openssh.com", nil},
	} {
		ok, err := sess.SendRequest(req.kind, true, req.payload)
		if err != nil || ok {
			t.Errorf("%s: %v %v; want refused", req.kind, ok, err)
		}
	}
	var out bytes.Buffer
	sess.Stdout = &out
	err = sess.Run("tty")
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 {
		t.Fatalf("tty: %v", err)
	}
	if got := out.String(); got != "false xterm 0x0" {
		t.Errorf("tty printed %q", got)
	}
}

func TestAPtyAndItsResizes(t *testing.T) {
	r := sshdStart(t)
	c := r.signIn(r.me)
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm-256color", 30, 100, ssh.TerminalModes{}); err != nil {
		t.Fatalf("pty-req: %v", err)
	}
	// Six changes into a buffer of four keep the newest four; a size past
	// what a terminal can be is held to it.
	for i := range 5 {
		if err := sess.WindowChange(10+i, 20+i); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.WindowChange(1<<20, 1<<20); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sess.Stdout = &out
	err = sess.Run("tty")
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 {
		t.Fatalf("tty: %v", err)
	}
	if got, want := out.String(), "true xterm-256color 100x30 22x12 23x13 24x14 1000x1000"; got != want {
		t.Errorf("tty printed %q, want %q", got, want)
	}

	// A second command on a channel that already ran one is refused.
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	if ok, err := ch.SendRequest("exec", true, ssh.Marshal(struct{ C string }{"block"})); err != nil || !ok {
		t.Fatalf("exec: %v %v", ok, err)
	}
	<-r.blocked
	for _, kind := range []string{"exec", "shell"} {
		if ok, err := ch.SendRequest(kind, true, ssh.Marshal(struct{ C string }{"echo again"})); err != nil || ok {
			t.Errorf("a second %s: %v %v; want refused", kind, ok, err)
		}
	}
	if ok, err := ch.SendRequest("pty-req", true, ssh.Marshal(struct {
		Term             string
		Cols, Rows, W, H uint32
		Modes            string
	}{"xterm", 80, 24, 0, 0, ""})); err != nil || ok {
		t.Errorf("a pty-req once running: %v %v; want refused", ok, err)
	}
	select {
	case args := <-r.ran:
		t.Errorf("echo ran with %q", args)
	default:
	}
}

func TestTheCommandIsAFixedList(t *testing.T) {
	r := sshdStart(t)
	c := r.signIn(r.me)
	out, errOut, code := sshdRun(t, c, "list; rm -rf ~")
	if code != 2 || out != "" || errOut != "rota over ssh runs list, attach, watch and new, and nothing else\n"+usage {
		t.Errorf("list; rm -rf ~: %d %q %q", code, out, errOut)
	}
	for _, line := range []string{`echo "unbalanced`, "rm -rf ~", "echo\x00 x", "sh -c echo"} {
		if _, errOut, code := sshdRun(t, c, line); code != 2 || !strings.HasSuffix(errOut, usage) {
			t.Errorf("%q: %d %q", line, code, errOut)
		}
	}
	select {
	case args := <-r.ran:
		t.Fatalf("echo ran with %q", args)
	default:
	}

	out, _, code = sshdRun(t, c, "echo a 'b c'")
	if code != 0 || out != `["a" "b c"]` {
		t.Errorf("echo a 'b c': %d %q", code, out)
	}
	if args := <-r.ran; len(args) != 2 || args[0] != "a" || args[1] != "b c" {
		t.Errorf("echo ran with %q", args)
	}

	// A line of at most maxLine bytes runs; one past it does not, however
	// long.
	long := "echo " + strings.Repeat("x", maxLine-len("echo "))
	if _, _, code := sshdRun(t, c, long); code != 0 {
		t.Errorf("a line of %d bytes: %d", len(long), code)
	}
	<-r.ran
	for _, n := range []int{maxLine + 1, 5000, 200 << 10} {
		_, errOut, code := sshdRun(t, c, "echo "+strings.Repeat("x", n-len("echo ")))
		if code != 2 || !strings.Contains(errOut, "4096 bytes") {
			t.Errorf("a line of %d bytes: %d %q", n, code, errOut)
		}
	}
	select {
	case <-r.ran:
		t.Fatal("echo ran with a line too long")
	default:
	}

	// A bare command, by shell or by an empty line, is the usage on stdout.
	for _, line := range []string{"", "  \t "} {
		if out, errOut, code := sshdRun(t, c, line); code != 0 || out != usage || errOut != "" {
			t.Errorf("%q: %d %q %q", line, code, out, errOut)
		}
	}
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var shell bytes.Buffer
	sess.Stdout = &shell
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(); err != nil || shell.String() != usage {
		t.Errorf("shell: %v %q", err, shell.String())
	}
	// With a pty, a line ends as a terminal in raw mode needs it to.
	sess, err = c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	shell.Reset()
	sess.Stdout = &shell
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Run(""); err != nil || shell.String() != strings.ReplaceAll(usage, "\n", "\r\n") {
		t.Errorf("bare with a pty: %v %q", err, shell.String())
	}
}

func TestTheChannelKnowsItsPerson(t *testing.T) {
	r := sshdStart(t)
	for _, c := range []struct {
		key   ssh.Signer
		login string
		want  string
	}{
		{r.me, "whoever", `{"name":"me","role":"control","via":"user"}`},
		{r.eye, "root", `{"name":"eye","role":"watch","via":"user"}`},
	} {
		cl, err := r.dial(c.login, ssh.PublicKeys(c.key))
		if err != nil {
			t.Fatal(err)
		}
		out, _, code := sshdRun(t, cl, "who")
		cl.Close()
		if code != 0 || out != c.want+"\n"+c.login {
			t.Errorf("who: %d %q; want %q then %q", code, out, c.want, c.login)
		}
	}
}

func TestAHandshakeHasADeadline(t *testing.T) {
	banner := "SSH-2.0-rota\r\n"
	t.Run("silent", func(t *testing.T) {
		sshdKnob(t, &handshakeWait, 200*time.Millisecond)
		r := sshdStart(t)
		nc, err := net.Dial("tcp", r.s.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		start := time.Now()
		nc.SetDeadline(start.Add(3 * time.Second))
		got, err := io.ReadAll(nc)
		if took := time.Since(start); took > time.Second {
			t.Errorf("a silent connection stayed open %v", took)
		}
		if string(got) != banner {
			t.Errorf("read %q, %v; want the banner and then the end", got, err)
		}
	})
	t.Run("unauthenticated", func(t *testing.T) {
		sshdKnob(t, &handshakeWait, 1500*time.Millisecond)
		r := sshdStart(t)
		var silent []net.Conn
		defer func() {
			for _, c := range silent {
				c.Close()
			}
		}()
		for i := range maxUnauthed {
			nc, err := net.Dial("tcp", r.s.Addr())
			if err != nil {
				t.Fatal(err)
			}
			silent = append(silent, nc)
			nc.SetDeadline(time.Now().Add(3 * time.Second))
			b := make([]byte, len(banner))
			if _, err := io.ReadFull(nc, b); err != nil || string(b) != banner {
				t.Fatalf("connection %d: %q %v", i+1, b, err)
			}
		}
		one, err := net.Dial("tcp", r.s.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer one.Close()
		start := time.Now()
		one.SetDeadline(start.Add(3 * time.Second))
		got, _ := io.ReadAll(one)
		if len(got) != 0 || time.Since(start) > 500*time.Millisecond {
			t.Errorf("connection %d read %q after %v; want closed at once", maxUnauthed+1, got, time.Since(start))
		}
		for _, c := range silent {
			io.Copy(io.Discard, c)
		}
		sshdEventually(t, func() error {
			c, err := r.dial("me", ssh.PublicKeys(r.me))
			if err == nil {
				c.Close()
			}
			return err
		})
	})
}

func TestAtMostSixtyFourConnections(t *testing.T) {
	r := sshdStart(t)
	var clients []*ssh.Client
	for i := range maxConns {
		c, err := r.dial("me", ssh.PublicKeys(r.me))
		if err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
		clients = append(clients, c)
	}
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()
	nc, err := net.Dial("tcp", r.s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(3 * time.Second))
	if got, _ := io.ReadAll(nc); len(got) != 0 {
		t.Errorf("connection %d read %q; want closed at once", maxConns+1, got)
	}
	clients[0].Close()
	sshdEventually(t, func() error {
		c, err := r.dial("me", ssh.PublicKeys(r.me))
		if err == nil {
			clients[0] = c
		}
		return err
	})
}

func TestThreeUnansweredKeepalivesClose(t *testing.T) {
	sshdKnob(t, &keepaliveEvery, 50*time.Millisecond)
	r := sshdStart(t)

	// A client that answers stays.
	c := r.signIn(r.me)
	time.Sleep(500 * time.Millisecond)
	if out, _, code := sshdRun(t, c, "echo still"); code != 0 || out != `["still"]` {
		t.Fatalf("an answering client: %q %d", out, code)
	}

	// One that never answers is closed.
	nc, err := net.Dial("tcp", r.s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	cc, _, _, err := ssh.NewClientConn(nc, r.s.Addr(), r.config("me", ssh.PublicKeys(r.me)))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- cc.Wait() }()
	select {
	case <-done:
		if took := time.Since(start); took < 150*time.Millisecond {
			t.Errorf("closed after %v, before three keepalives went unanswered", took)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a client that never answers stayed")
	}
}

func TestTheLogNamesNoKey(t *testing.T) {
	r := sshdStart(t)
	c := r.signIn(r.me)
	if _, _, code := sshdRun(t, c, "echo secret-argument"); code != 0 {
		t.Fatal(code)
	}
	sshdRun(t, c, "nonsense-word arg")
	c.Close()
	stranger := sshdKey(t)
	if c, err := r.dial("me", ssh.PublicKeys(stranger)); err == nil {
		c.Close()
		t.Fatal("a stranger signed in")
	}
	var log string
	sshdEventually(t, func() error {
		log = r.log.String()
		for _, want := range []string{"ssh signed in", "ssh refused", "ssh command", "ssh signed out"} {
			if !strings.Contains(log, want) {
				return fmt.Errorf("no %q in:\n%s", want, log)
			}
		}
		return nil
	})
	for _, want := range []string{
		ssh.FingerprintSHA256(r.me.PublicKey()),
		ssh.FingerprintSHA256(stranger.PublicKey()),
		"name=me", "login=me", "word=echo",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log has no %q:\n%s", want, log)
		}
	}
	for _, k := range []ssh.PublicKey{r.me.PublicKey(), r.eye.PublicKey(), stranger.PublicKey(), r.s.key.PublicKey()} {
		b64 := base64.StdEncoding.EncodeToString(k.Marshal())
		if strings.Contains(log, b64) || strings.Contains(log, b64[len(b64)-24:]) {
			t.Errorf("the log has a key's base64:\n%s", log)
		}
	}
	for _, never := range []string{"secret-argument", "nonsense-word"} {
		if strings.Contains(log, never) {
			t.Errorf("the log has %q:\n%s", never, log)
		}
	}
}

func TestCloseEndsEveryConnection(t *testing.T) {
	r := sshdStart(t)
	c := r.signIn(r.me)
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("block"); err != nil {
		t.Fatal(err)
	}
	<-r.blocked
	waited := make(chan error, 1)
	go func() { waited <- c.Wait() }()
	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- r.s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-r.ended:
	default:
		t.Error("Close returned before the command saw its context end")
	}
	select {
	case <-waited:
	case <-time.After(2*time.Second - time.Since(start)):
		t.Fatal("the client never saw the connection end")
	}
	if _, err := net.DialTimeout("tcp", r.s.Addr(), time.Second); err == nil {
		t.Error("the listener still accepts")
	}
}

// sshdEventually retries f for up to five seconds: what a closed
// connection gives back is counted once its goroutine has finished.
func sshdEventually(t *testing.T, f func() error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
