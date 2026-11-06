package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/wire"
)

// client never waits for ever, and takes the throwaway certificate a TLS
// test serves with.
var client = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

// serveFor starts `rota serve` on a free loopback port with the arguments
// given, waits until it answers, hands its address to while, then stops it
// the way an interrupt would and returns everything it printed.
func serveFor(t *testing.T, args []string, while func(base string)) (stdout, stderr string, code int) {
	t.Helper()
	t.Setenv("ROTA_HOME", t.TempDir())
	t.Setenv("ROTA_TOKEN", "the-token")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	was := serveUntil
	serveUntil = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	defer func() { serveUntil = was }()

	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(append([]string{"serve", addr, "--refresh-every=0"}, args...), &out, &errb)
	}()
	base := "http://" + addr
	if slices.Contains(args, "--tls-cert") {
		base = "https://" + addr
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case code := <-done:
			t.Fatalf("serve stopped before it answered: %d\nstdout %q\nstderr %q", code, out.String(), errb.String())
		default:
		}
		resp, err := client.Get(base + "/v1/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never answered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	while(base)
	cancel()
	select {
	case code = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
	return out.String(), errb.String(), code
}

// refusing is for a test that expects serve to refuse before it serves: it
// gets an address that is already taken, and every serve it calls stops by
// itself after thirty seconds, so a regression that does start ends instead
// of binding 127.0.0.1:8787 and waiting for ever. Each call gets a context
// of its own, and a window long enough that the taken address, not the
// clock, is what ends the one test that wants it.
func refusing(t *testing.T) string {
	t.Helper()
	t.Setenv("ROTA_HOME", t.TempDir())
	t.Setenv("ROTA_TOKEN", "the-token")
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { taken.Close() })
	was := serveUntil
	serveUntil = func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 30*time.Second)
	}
	t.Cleanup(func() { serveUntil = was })
	return taken.Addr().String()
}

// badRequest is a request the server logs: a token it refuses.
func badRequest(t *testing.T, base string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer not-the-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong token answered %d", resp.StatusCode)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// eventually waits for the file at path to say want; a log line written by
// another goroutine lands a moment after the request that caused it.
func eventually(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(readFile(t, path), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never said %q:\n%s", want, readFile(t, path))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServeWithoutALogFileLogsToStderr: nothing set is the server it always
// was, saying everything on stderr in the very words it always used — no
// date in front, and the blank line before "shutting down" kept.
func TestServeWithoutALogFileLogsToStderr(t *testing.T) {
	var addr string
	out, errb, code := serveFor(t, nil, func(base string) {
		addr = strings.TrimPrefix(base, "http://")
		badRequest(t, base)
	})
	if code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	for _, want := range []string{
		"rejected request",
		"\nrota: shutting down; waiting for runs in flight (interrupt again to stop now)\n",
	} {
		if !strings.Contains(errb, want) {
			t.Fatalf("stderr must say %q:\n%s", want, errb)
		}
	}
	if first := "rota " + wire.Version + " serving on http://" + addr + "\n"; !strings.HasPrefix(errb, first) {
		t.Fatalf("the first line is not %q:\n%s", first, errb)
	}
}

// lineOf is the first line of s that holds want.
func lineOf(s, want string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}

// TestServeWithALogFileIsSilent: with a log file, every line the server says
// goes there, its own plain lines dated, and nothing reaches the terminal.
func TestServeWithALogFileIsSilent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rota.log")
	out, errb, code := serveFor(t, []string{"--log-file", path}, func(base string) {
		badRequest(t, base)
		eventually(t, path, "rejected request")
	})
	if code != 0 || out != "" || errb != "" {
		t.Fatalf("a server with a log file must say nothing: exit %d, stdout %q, stderr %q", code, out, errb)
	}
	logged := readFile(t, path)
	for _, want := range []string{"serving on", "rejected request", "no --root given", "shutting down"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the log file must say %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "\n\n") || strings.HasPrefix(logged, "\n") {
		t.Fatalf("the log file has a blank line:\n%s", logged)
	}
	for _, want := range []string{"serving on", "no --root given", "shutting down"} {
		line := lineOf(logged, want)
		stamp, _, _ := strings.Cut(line, " ")
		if _, err := time.Parse(time.RFC3339, stamp); err != nil {
			t.Fatalf("a plain line in the file is not dated: %q", line)
		}
	}
	if strings.Contains(logged, "the-token") || strings.Contains(logged, "not-the-token") {
		t.Fatalf("a token reached the log:\n%s", logged)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("the log file was made %04o", perm)
		}
	}

	// A second start appends rather than starting over.
	serveFor(t, []string{"--log-file", path}, func(string) {})
	if n := strings.Count(readFile(t, path), "serving on"); n != 2 {
		t.Fatalf("two starts, %d starts logged", n)
	}
}

// TestServeErrorLogReachesTheFile: what the http server says on its own — a
// TLS handshake that was not one — goes to the file too.
func TestServeErrorLogReachesTheFile(t *testing.T) {
	cert, key := selfSigned(t)
	path := filepath.Join(t.TempDir(), "rota.log")
	_, errb, code := serveFor(t, []string{"--log-file", path, "--tls-cert", cert, "--tls-key", key}, func(base string) {
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "https://"), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		conn.Write([]byte("this is not a TLS handshake\r\n\r\n"))
		io.Copy(io.Discard, conn)
		conn.Close()
		eventually(t, path, "TLS handshake error")
	})
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
}

// selfSigned writes a throwaway certificate for 127.0.0.1 and its key.
func selfSigned(t *testing.T) (cert, key string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// TestServeThatStopsBadlySaysSoOnStderr: a shutdown that runs out of time
// is a server stopping on its own terms, and that is never silent either.
func TestServeThatStopsBadlySaysSoOnStderr(t *testing.T) {
	was := serveGrace
	serveGrace = 0
	defer func() { serveGrace = was }()
	path := filepath.Join(t.TempDir(), "rota.log")
	var conn net.Conn
	out, errb, code := serveFor(t, []string{"--log-file", path, "--timeout=1ms"}, func(base string) {
		// Half a request: a connection the shutdown has to wait for.
		var err error
		conn, err = net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte("GET /v1/health HTTP/1.1\r\n"))
		// One whole request on another connection: the server accepts in
		// order, so once this is answered the half-sent one is accepted
		// and tracked, and the shutdown below has to wait for it. A
		// transport of its own, without keep-alives, so this can never
		// ride a connection the start-up's polling left open.
		fresh := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := fresh.Get(base + "/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	})
	if conn != nil {
		conn.Close()
	}
	if code == 0 || out != "" || !strings.HasPrefix(errb, "error: shutting down") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
	if logged := readFile(t, path); !strings.Contains(logged, "error: shutting down") {
		t.Fatalf("the log must say why it stopped:\n%s", logged)
	}
}

// TestServeThatCannotStartSaysSoOnStderr: a server that fails to start is
// never silent, whatever its log file says.
func TestServeThatCannotStartSaysSoOnStderr(t *testing.T) {
	addr := refusing(t)

	// The address is taken: the log has the start and the reason, and
	// stderr has the reason too.
	path := filepath.Join(t.TempDir(), "rota.log")
	out, errb, code := call(t, "serve", addr, "--refresh-every=0", "--log-file", path)
	if code == 0 || out != "" || !strings.HasPrefix(errb, "error:") {
		t.Fatalf("an address in use: exit %d, stdout %q, stderr %q", code, out, errb)
	}
	logged := readFile(t, path)
	reason := strings.TrimSpace(strings.SplitN(errb, "\n", 2)[0])
	if !strings.Contains(logged, "serving on") || !strings.Contains(logged, reason) {
		t.Fatalf("the log must hold the start and %q:\n%s", reason, logged)
	}

	// The log file cannot be opened: there is no log to write to, so stderr
	// is the only place the reason can go.
	missing := filepath.Join(t.TempDir(), "no such dir", "rota.log")
	_, errb, code = call(t, "serve", addr, "--log-file", missing)
	if code == 0 || !strings.Contains(errb, "server.log_file") || !strings.Contains(errb, "no such dir") {
		t.Fatalf("a directory that is not there: exit %d, stderr %q", code, errb)
	}

	// The configuration is wrong: refused on stderr before any log exists.
	conf := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(conf, []byte("[server]\nlog_file = "+tomlString(path)+"\n[runs]\ntimeout = \"0s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errb, code = call(t, "serve", addr, "--config", conf)
	if code == 0 || !strings.Contains(errb, "runs.timeout") {
		t.Fatalf("a bad configuration: exit %d, stderr %q", code, errb)
	}
}

func TestServeRefusesARelativeLogFile(t *testing.T) {
	addr := refusing(t)
	_, errb, code := call(t, "serve", addr, "--log-file", "rota.log")
	if code == 0 || !strings.Contains(errb, "server.log_file must be an absolute path") {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	if _, err := os.Stat("rota.log"); err == nil {
		os.Remove("rota.log")
		t.Fatal("a relative log file was made anyway")
	}
}

// TestServeRefusesALogFileOthersCanUse: a server's log is its owner's, and
// the refusal is server.toml's, with the one command that fixes it.
func TestServeRefusesALogFileOthersCanUse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has neither the bits nor the convention")
	}
	addr := refusing(t)
	for _, mode := range []os.FileMode{0o644, 0o620} {
		path := filepath.Join(t.TempDir(), "rota.log")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Skipf("this filesystem will not say: %v", err)
		}
		out, errb, code := call(t, "serve", addr, "--log-file", path)
		if code == 0 || out != "" || !strings.Contains(errb, "readable or writable by other users") ||
			!strings.Contains(errb, "chmod 600 "+path) {
			t.Fatalf("%04o: exit %d, stdout %q, stderr %q", mode, code, out, errb)
		}
		if got := readFile(t, path); got != "" {
			t.Fatalf("%04o: it wrote to the file it refused: %q", mode, got)
		}
	}
}

// TestLogFileNeverFollowsALink: somebody who can write the log's directory
// plants a link to a file of the owner's — before the server starts, or in
// the moment after a rotation has moved the log away. Either way the file
// the link points at is left exactly as it was.
func TestLogFileNeverFollowsALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a link needs privileges on windows, and there is no O_NOFOLLOW to test")
	}
	victim := filepath.Join(t.TempDir(), ".credentials.json")
	const secret = `{"refresh":"keep-me"}`
	if err := os.WriteFile(victim, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	untouched := func(when string) {
		t.Helper()
		if got := readFile(t, victim); got != secret {
			t.Fatalf("%s: the file behind the link became %q", when, got)
		}
	}

	// Before the start: refused, by serve as by the file itself.
	dir := t.TempDir()
	path := filepath.Join(dir, "rota.log")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if _, err := openLogFile(path, io.Discard); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Fatalf("a link at the path was opened: %v", err)
	}
	addr := refusing(t)
	if _, errb, code := call(t, "serve", addr, "--log-file", path); code == 0 || !strings.Contains(errb, "is a link") {
		t.Fatalf("serve took a link: exit %d, stderr %q", code, errb)
	}
	untouched("before the start")

	// After a rotation: the reopen refuses it too, and the rest of the log
	// goes to stderr, said once.
	path = filepath.Join(t.TempDir(), "rota.log")
	var fallback bytes.Buffer
	lf, err := openLogFile(path, &fallback)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	fmt.Fprintln(lf, "before")
	lf.mu.Lock()
	lf.rotate()
	lf.mu.Unlock()
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(lf, "after one")
	fmt.Fprintln(lf, "after two")
	untouched("after a rotation")
	got := fallback.String()
	if strings.Count(got, "the log goes to stderr") != 1 || !strings.Contains(got, "is a link") ||
		!strings.Contains(got, "after one\n") || !strings.Contains(got, "after two\n") {
		t.Fatalf("stderr after a refused reopen:\n%s", got)
	}
	if readFile(t, path+".1") != "before\n" {
		t.Fatal("the rotated log lost what it had")
	}
}

// TestLogFileFoldThatCannotMoveKeepsEverything: a <name>.0 that cannot be
// moved — another user's, in a shared directory with the sticky bit — must
// not cost an old file on every try. Here the fold's own name is taken by a
// directory, so the move fails the same way.
func TestLogFileFoldThatCannotMoveKeepsEverything(t *testing.T) {
	smallLogs(t, 100, 4)
	path := filepath.Join(t.TempDir(), "rota.log")
	want := map[string]string{".0": "crashed\n"}
	for i := 1; i <= 4; i++ {
		want[fmt.Sprintf(".%d", i)] = fmt.Sprintf("old %d\n", i)
	}
	for name, body := range want {
		if err := os.WriteFile(path+name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(path+".folding", "in the way"), 0o700); err != nil {
		t.Fatal(err)
	}
	kept := func(when string) {
		t.Helper()
		for name, body := range want {
			if got := readFile(t, path+name); got != body {
				t.Fatalf("%s: %s holds %q, want %q", when, name, got, body)
			}
		}
	}
	lf, err := openLogFile(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	kept("on opening")
	for i := range 3 { // the third would rotate, and the fold cannot clear .0
		line(lf, i)
	}
	kept("at a rotation")
	if got := lines(t, path); len(got) != 3 {
		t.Fatalf("the log must hold all three lines: %q", got)
	}
}

// TestLogFileFoldsAnInterruptedFold: a <name>.folding left by a fold that
// was cut short is this user's, and goes into the chain behind a newer .0.
func TestLogFileFoldsAnInterruptedFold(t *testing.T) {
	smallLogs(t, 100, 4)
	path := filepath.Join(t.TempDir(), "rota.log")
	for name, body := range map[string]string{".folding": "interrupted\n", ".0": "crashed\n", ".1": "old 1\n"} {
		if err := os.WriteFile(path+name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lf, err := openLogFile(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	for name, body := range map[string]string{".1": "crashed\n", ".2": "interrupted\n", ".3": "old 1\n"} {
		if got := readFile(t, path+name); got != body {
			t.Fatalf("%s holds %q, want %q", name, got, body)
		}
	}
	for _, gone := range []string{".folding", ".0"} {
		if _, err := os.Stat(path + gone); err == nil {
			t.Fatalf("%s is still there", gone)
		}
	}
}

// TestLogFileGoesBackToTheFile: stderr is where the log goes while the file
// cannot be opened, not for the rest of the run. The file is tried again
// once another logFileMax has gone to stderr, or after logFileRetry, and
// each switch is said once.
func TestLogFileGoesBackToTheFile(t *testing.T) {
	smallLogs(t, 100, 4)
	was := logFileRetry
	logFileRetry = time.Hour
	defer func() { logFileRetry = was }()
	path := filepath.Join(t.TempDir(), "rota.log")
	var stderr bytes.Buffer
	lf, err := openLogFile(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	// A directory where the new log would be: nothing can open it.
	block := func() {
		lf.mu.Lock()
		lf.rotate()
		lf.mu.Unlock()
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	notices := func() (away, back int) {
		s := stderr.String()
		return strings.Count(s, "the log goes to stderr"), strings.Count(s, "again\n")
	}

	// By size: blocked, unblocked, and the file is only tried again once a
	// hundred bytes have gone to stderr.
	block()
	line(lf, 0)
	if lf.onFile() {
		t.Fatal("a log that cannot be opened is said to be on its file")
	}
	os.Remove(path)
	line(lf, 1)
	line(lf, 2) // 120 bytes to stderr now
	if away, back := notices(); away != 1 || back != 0 {
		t.Fatalf("before the retry: %d away, %d back\n%s", away, back, stderr.String())
	}
	line(lf, 3) // tried, and opened: this line is in the file
	if away, back := notices(); away != 1 || back != 1 || !lf.onFile() {
		t.Fatalf("after the retry: %d away, %d back\n%s", away, back, stderr.String())
	}
	if got := lines(t, path); strings.Join(got, ",") != "line 03" {
		t.Fatalf("the file after going back: %q", got)
	}

	// By time: the same, the retry due at once.
	block()
	line(lf, 4)
	os.Remove(path)
	logFileRetry = 0
	line(lf, 5)
	if away, back := notices(); away != 2 || back != 2 {
		t.Fatalf("by time: %d away, %d back\n%s", away, back, stderr.String())
	}
	if got := lines(t, path); strings.Join(got, ",") != "line 05" {
		t.Fatalf("the file after going back by time: %q", got)
	}
	for _, want := range []string{"line 00", "line 01", "line 02", "line 04"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("%s was lost:\n%s", want, stderr.String())
		}
	}
}

// TestLogFileFoldsALeftoverAside: a <name>.0 left by a rotation that never
// finished takes its place in the chain, when the log is opened and before
// a rotation, rather than being written over.
func TestLogFileFoldsALeftoverAside(t *testing.T) {
	smallLogs(t, 100, 4)
	path := filepath.Join(t.TempDir(), "rota.log")
	put := func(name, body string) {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(path+".0", "crashed\n")
	put(path+".1", "old 1\n")
	put(path+".2", "old 2\n")
	lf, err := openLogFile(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	for name, want := range map[string]string{".1": "crashed\n", ".2": "old 1\n", ".3": "old 2\n"} {
		if got := readFile(t, path+name); got != want {
			t.Fatalf("on opening, %s holds %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(path + ".0"); err == nil {
		t.Fatal(".0 is still there")
	}

	// And one that appears while the server runs is folded in before the
	// rotation takes the name.
	line(lf, 0)
	line(lf, 1)
	put(path+".0", "left mid-run\n")
	line(lf, 2) // rotates
	for name, want := range map[string]string{".2": "left mid-run\n", ".3": "crashed\n", ".4": "old 1\n"} {
		if got := readFile(t, path+name); got != want {
			t.Fatalf("after the rotation, %s holds %q, want %q", name, got, want)
		}
	}
	if got := lines(t, path+".1"); strings.Join(got, ",") != "line 00,line 01" {
		t.Fatalf(".1 after the rotation: %q", got)
	}
}

// smallLogs makes rotation happen at max bytes, keeping keep old files, for
// the length of one test.
func smallLogs(t *testing.T, max int64, keep int) {
	was, wasKeep := logFileMax, logFileKeep
	logFileMax, logFileKeep = max, keep
	t.Cleanup(func() { logFileMax, logFileKeep = was, wasKeep })
}

// line is forty bytes: two fit under a limit of a hundred, a third does not.
func line(w io.Writer, i int) {
	fmt.Fprintf(w, "%-39s\n", fmt.Sprintf("line %02d", i))
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(strings.TrimRight(readFile(t, path), "\n"), "\n") {
		out = append(out, strings.TrimSpace(l))
	}
	return out
}

// TestLogFileRotates: a write that would take the file past its limit moves
// it to .1, the older ones up by one, and the oldest out; exactly four are
// kept, newest first.
func TestLogFileRotates(t *testing.T) {
	smallLogs(t, 100, 4)
	path := filepath.Join(t.TempDir(), "rota.log")
	lf, err := openLogFile(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	for i := range 20 {
		line(lf, i)
	}
	for name, want := range map[string][]string{
		path:        {"line 18", "line 19"},
		path + ".1": {"line 16", "line 17"},
		path + ".2": {"line 14", "line 15"},
		path + ".3": {"line 12", "line 13"},
		path + ".4": {"line 10", "line 11"},
	} {
		if got := lines(t, name); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s holds %q, want %q", filepath.Base(name), got, want)
		}
	}
	for _, gone := range []string{".5", ".0"} {
		if _, err := os.Stat(path + gone); err == nil {
			t.Fatalf("%s was left behind", filepath.Base(path+gone))
		}
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{path, path + ".1", path + ".4"} {
			if fi, err := os.Stat(name); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("%s: %v %v", filepath.Base(name), fi.Mode(), err)
			}
		}
	}
}

// TestLogFileThatCannotMoveKeepsEverything: when the log itself cannot be
// moved aside, no older file is shifted or dropped, the log goes on growing,
// and the next try waits for another logFileMax rather than every line.
func TestLogFileThatCannotMoveKeepsEverything(t *testing.T) {
	smallLogs(t, 100, 4)
	path := filepath.Join(t.TempDir(), "rota.log")
	for i := 1; i <= 4; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i), []byte(fmt.Sprintf("old %d\n", i)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory where the log would be moved to: nothing can be renamed
	// onto it.
	block := path + ".0"
	if err := os.MkdirAll(filepath.Join(block, "in the way"), 0o700); err != nil {
		t.Fatal(err)
	}
	lf, err := openLogFile(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	for i := range 3 { // the third would pass 100, and cannot be moved
		line(lf, i)
	}
	for i := 1; i <= 4; i++ {
		if got := readFile(t, fmt.Sprintf("%s.%d", path, i)); got != fmt.Sprintf("old %d\n", i) {
			t.Fatalf(".%d was touched by a rotation that never happened: %q", i, got)
		}
	}
	if got := lines(t, path); len(got) != 3 {
		t.Fatalf("the log must hold all three lines: %q", got)
	}

	// The way is clear again, but the next try waits for another hundred
	// bytes past where the first failed, at 80: a fourth line makes 160,
	// which is not past 180, and a fifth makes 200, which is.
	if err := os.RemoveAll(block); err != nil {
		t.Fatal(err)
	}
	line(lf, 3)
	if readFile(t, path+".1") != "old 1\n" {
		t.Fatal("it tried again on the very next line")
	}
	line(lf, 4)
	if got := lines(t, path+".1"); strings.Join(got, ",") != "line 00,line 01,line 02,line 03" {
		t.Fatalf(".1 after the retry: %q", got)
	}
	if got := lines(t, path); strings.Join(got, ",") != "line 04" {
		t.Fatalf("the new log: %q", got)
	}
	if readFile(t, path+".2") != "old 1\n" || readFile(t, path+".4") != "old 3\n" {
		t.Fatal("the old files did not move up by one")
	}
}

// TestLogFileTakesConcurrentWriters: the slog handler and the http server's
// own error log write from different goroutines; every line arrives whole,
// in one file, through any number of rotations.
func TestLogFileTakesConcurrentWriters(t *testing.T) {
	smallLogs(t, 4096, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "rota.log")
	lf, err := openLogFile(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(lf, nil))
	errorLog := slog.NewLogLogger(logger.Handler(), slog.LevelWarn)
	plain := log.New(lf, "", 0)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				switch g % 3 {
				case 0:
					logger.Info("from slog", "g", g, "i", i)
				case 1:
					errorLog.Printf("from the error log g=%d i=%d", g, i)
				default:
					plain.Printf("from a plain writer g=%d i=%d", g, i)
				}
			}
		}()
	}
	wg.Wait()
	if err := lf.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(path + "*")
	if len(files) != 5 {
		t.Fatalf("expected the log and four old files, got %v", files)
	}
	for _, name := range files {
		body := readFile(t, name)
		if int64(len(body)) > logFileMax {
			t.Fatalf("%s grew to %d bytes", filepath.Base(name), len(body))
		}
		if !strings.HasSuffix(body, "\n") {
			t.Fatalf("%s ends mid-line", filepath.Base(name))
		}
		for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
			if strings.Count(line, "from ") != 1 || !strings.Contains(line, "i=") {
				t.Fatalf("%s has a broken line: %q", filepath.Base(name), line)
			}
		}
	}
}
