package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/api"
)

// runCLIStdin runs one command with something on its standard input, which
// is what these three read a password from. The package variable is what the
// command takes its input from, so a test replaces it rather than opening a
// pipe.
func runCLIStdin(t *testing.T, in string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	before := stdin
	stdin = strings.NewReader(in)
	t.Cleanup(func() { stdin = before })
	return call(t, args...)
}

// blockOf pulls one `[[…]]` block out of what a command printed, so a test
// can hand it to the parser that will actually read it.
func blockOf(t *testing.T, out, table string) string {
	t.Helper()
	i := strings.Index(out, "[["+table+"]]")
	if i < 0 {
		t.Fatalf("no [[%s]] block in:\n%s", table, out)
	}
	return out[i:]
}

// loadServer writes one server.toml, at the permission rota insists on, and
// reads it back the way the server does.
func loadServer(t *testing.T, body string) *api.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := api.LoadConfig(path)
	if err != nil {
		t.Fatalf("what the command printed is not a file rota reads: %v\n%s", err, body)
	}
	return c
}

// TestServePasswdPrintsABlockThatWorks: the block loads, and the password
// that made it is the one that opens it.
func TestServePasswdPrintsABlockThatWorks(t *testing.T) {
	out, errOut, code := runCLIStdin(t, "hunter2 is fine\n", "serve", "passwd", "inoyat", "--role", "control")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if strings.Contains(out, "hunter2 is fine") || strings.Contains(errOut, "hunter2 is fine") {
		t.Fatalf("the password was printed:\n%s\n%s", out, errOut)
	}
	if !strings.Contains(errOut, "not a terminal") {
		t.Fatalf("a piped password must be said to be echoed:\n%s", errOut)
	}
	cfg := loadServer(t, blockOf(t, out, "users"))
	if len(cfg.Users) != 1 || cfg.Users[0].Name != "inoyat" || cfg.Users[0].Role != "control" {
		t.Fatalf("loaded %+v", cfg.Users)
	}
	// And a server built from it admits that password and no other.
	opts, _, err := cfg.Serve()
	if err != nil {
		t.Fatal(err)
	}
	opts.Token = "t"
	opts.Dir = t.TempDir()
	opts.RefreshEvery = -1
	s, err := api.New(opts)
	if err != nil {
		t.Fatalf("a server from that block: %v", err)
	}
	defer s.Stop()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	signIn := func(pw string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/session",
			strings.NewReader(fmt.Sprintf(`{"name":"inoyat","password":%q}`, pw)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := signIn("hunter2 is fine"); code != http.StatusOK {
		t.Fatalf("the password that made the block did not open it: %d", code)
	}
	if code := signIn("hunter3"); code != http.StatusUnauthorized {
		t.Fatalf("another password opened it: %d", code)
	}
}

func TestServePasswdRefusesTheObviousMistakes(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		args           []string
	}{
		{name: "no name", in: "x\n", args: []string{"serve", "passwd"}, want: "takes one name"},
		{name: "two names", in: "x\n", args: []string{"serve", "passwd", "a", "b"}, want: "takes one name"},
		{name: "an invented role", in: "x\n", args: []string{"serve", "passwd", "a", "--role", "root"},
			want: `--role is "control" or "watch"`},
		{name: "nothing typed", in: "\n", args: []string{"serve", "passwd", "a"},
			want: "a password with nothing in it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, errOut, code := runCLIStdin(t, c.in, c.args...)
			if code == 0 || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d\n%s\nmust say %q", code, errOut, c.want)
			}
		})
	}
}

// TestServeTokenPrintsATokenAndItsHash: the token is printed once, the block
// holds its digest, and the two agree.
func TestServeTokenPrintsATokenAndItsHash(t *testing.T) {
	out, errOut, code := runCLIStdin(t, "", "serve", "token", "ci-watch", "--role", "watch")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	token := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if len(token) < 40 {
		t.Fatalf("that is not 32 random bytes: %q", token)
	}
	cfg := loadServer(t, blockOf(t, out, "tokens"))
	if len(cfg.Tokens) != 1 || cfg.Tokens[0].Name != "ci-watch" || cfg.Tokens[0].Role != "watch" {
		t.Fatalf("loaded %+v", cfg.Tokens)
	}
	sum := sha256.Sum256([]byte(token))
	if cfg.Tokens[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("the block does not hold the digest of the token it printed:\n%s", out)
	}
	if strings.Contains(blockOf(t, out, "tokens"), token) {
		t.Fatalf("the token itself is in the block:\n%s", out)
	}
}

// TestServeInviteAsksARunningServer: the whole command against a real
// server, ending in a link that works exactly once.
func TestServeInviteAsksARunningServer(t *testing.T) {
	dir := t.TempDir()
	s, err := api.New(api.Options{Token: "the-token", Dir: dir, RefreshEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	conf := filepath.Join(t.TempDir(), "server.toml")
	body := fmt.Sprintf("[server]\nlisten = %q\n\n[auth]\ntoken = \"the-token\"\n", addr)
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runCLIStdin(t, "", "serve", "invite", "--ttl", "5m", "--config", conf)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	link := strings.TrimSpace(out)
	if !strings.HasPrefix(link, srv.URL+"/invite/") {
		t.Fatalf("the link is %q", link)
	}
	if !strings.Contains(errOut, "One use") && !strings.Contains(errOut, "one use") {
		t.Fatalf("it must say what it is:\n%s", errOut)
	}
	// It works once...
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("following the link: %d", resp.StatusCode)
	}
	// ...and the cookie it set is a watcher.
	var cookie *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == "rota_session" {
			cookie = ck
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/session", nil)
	req.AddCookie(cookie)
	who, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer who.Body.Close()
	var got struct {
		Role string `json:"role"`
		Via  string `json:"via"`
	}
	json.NewDecoder(who.Body).Decode(&got)
	if got.Role != "watch" || got.Via != "invite" {
		t.Fatalf("the invited principal is %+v", got)
	}
	// ...and only once.
	again, err := client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusNotFound {
		t.Fatalf("the link worked twice: %d", again.StatusCode)
	}
}

func TestServeInviteSaysWhatItNeeds(t *testing.T) {
	t.Setenv("ROTA_TOKEN", "")
	conf := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(conf, []byte("[server]\nlisten = \"127.0.0.1:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := runCLIStdin(t, "", "serve", "invite", "--config", conf)
	if code == 0 || !strings.Contains(errOut, "needs the control token") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}
