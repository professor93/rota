package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/toml"
)

// sshString is one string of the SSH wire format: a four-byte big-endian
// length, then the bytes.
func sshString(b []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	return append(out, b...)
}

// edKey is one freshly made ed25519 public key as an authorized_keys line,
// with a comment.
func edKey(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob := append(sshString([]byte("ssh-ed25519")), sshString(pub)...)
	line := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
	if comment != "" {
		line += " " + comment
	}
	return line
}

// keyUser is a [[users]] block for name, with the keys given and, when
// password is not empty, that password.
func keyUser(name, role, password string, keys ...string) string {
	b := "[[users]]\nname = \"" + name + "\"\nrole = \"" + role + "\"\n"
	if password != "" {
		b += "password = \"" + password + "\"\n"
	}
	if len(keys) > 0 {
		b += "ssh_keys = [" + quoteAll(keys) + "]\n"
	}
	return b
}

// rsaBlob is the wire form of an ssh-rsa public key whose modulus is bits
// long: the type, the exponent 65537, and the modulus, each length-prefixed.
func rsaBlob(bits int) []byte {
	n := make([]byte, (bits+7)/8)
	for i := range n {
		n[i] = 0x5a
	}
	n[0] = 0xff >> (len(n)*8 - bits) // the top bit set, so it is bits long
	n = append([]byte{0}, n...)      // an mpint whose top bit is set leads with a zero
	out := append(sshString([]byte("ssh-rsa")), sshString([]byte{1, 0, 1})...)
	return append(out, sshString(n)...)
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = quote(s)
	}
	return strings.Join(q, ", ")
}

// TestSSHDefaultsAndPrint: SSH is off and on the loopback unless the file
// says otherwise, --print-config shows every key of it between [terminal]
// and [store], and a user's keys are printed only when there are some.
func TestSSHDefaultsAndPrint(t *testing.T) {
	if got, want := DefaultConfig().SSH, (SSHSection{Enabled: false, Listen: "127.0.0.1:2222", HostKey: ""}); got != want {
		t.Fatalf("the default [ssh] is %+v, want %+v", got, want)
	}
	key := edKey(t, "me@phone")
	c := DefaultConfig()
	c.Users = []UserEntry{
		{Name: "me", Role: "control", SSHKeys: []string{key}},
		{Name: "you", Role: "watch", Password: goodHash()},
	}
	var b bytes.Buffer
	if err := c.Print(&b, ""); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"\n[ssh]\nenabled         = false              # default\n" +
			"listen          = \"127.0.0.1:2222\"   # default\n" +
			"host_key        = \"\"                 # default\n\n[store]\n",
		"\nshare           = true               # default\n\n[ssh]\n",
		"password        = \"\"                 # default\nssh_keys        = [" + quote(key) + "]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output must have %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "ssh_keys"); n != 1 {
		t.Fatalf("ssh_keys is printed %d times; only the user with keys has the line:\n%s", n, out)
	}
	back := DefaultConfig()
	if err := toml.Unmarshal([]byte(out), back); err != nil {
		t.Fatalf("what it printed is not a file it reads: %v\n%s", err, out)
	}
	if len(back.Users) != 2 || len(back.Users[0].SSHKeys) != 1 || back.Users[0].SSHKeys[0] != key ||
		back.Users[1].SSHKeys != nil {
		t.Fatalf("read back as %+v", back.Users)
	}
}

// TestKeysMakeThePasswordOptional: a person who signs in by key alone needs
// no password, and a person with neither is told what they always were.
func TestKeysMakeThePasswordOptional(t *testing.T) {
	key := edKey(t, "")
	c, err := LoadConfig(write(t, keyUser("me", "control", "", key)))
	if err != nil {
		t.Fatalf("a user with a key and no password: %v", err)
	}
	if len(c.Users) != 1 || len(c.Users[0].SSHKeys) != 1 || c.Users[0].SSHKeys[0] != key {
		t.Fatalf("users %+v", c.Users)
	}
	if _, err := LoadConfig(write(t, keyUser("me", "control", "", key)+"[[users]]\nname = \"you\"\nrole = \"watch\"\nssh_keys = []\n")); err == nil ||
		!strings.Contains(err.Error(), "[[users]] \"you\": password is empty; make one with `rota serve passwd you`") {
		t.Fatalf("an empty list of keys is no key: %v", err)
	}
	_, err = LoadConfig(write(t, keyUser("me", "control", "")))
	if want := "[[users]] \"me\": password is empty; make one with `rota serve passwd me`"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("neither key nor password answers %v, want %q", err, want)
	}
	// With both, both are read: a bad password is still a bad password.
	if _, err := LoadConfig(write(t, keyUser("me", "control", "hunter2", key))); err == nil ||
		!strings.Contains(err.Error(), `[[users]] "me": password: must be pbkdf2-sha256`) {
		t.Fatalf("a key does not excuse a bad password: %v", err)
	}
}

// TestKeyLinesAreChecked: each key is one plain authorized_keys line of a
// type rota takes, whose body is the key it says it is, and one key is one
// person's. Each refusal names the user and the key's place, never the key.
func TestKeyLinesAreChecked(t *testing.T) {
	good := edKey(t, "laptop")
	body := strings.Fields(good)[1]
	rsaInside := "ssh-rsa " + body
	noKey := "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte("abc"))
	tooLong := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(append(sshString(bytes.Repeat([]byte("x"), 65)), 1))
	binaryName := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(sshString([]byte("ssh\x00ed")))
	shape := `is not "<type> <base64> [comment]"`
	kind := `is not a key type rota takes; use ssh-ed25519, ecdsa-sha2-nistp256/384/521, ssh-rsa, or a security key's sk- type`
	opts := `options before the key, such as from=, are not read; give the key alone`
	for _, c := range []struct{ name, key, want string }{
		{"a type alone", "ssh-ed25519", shape},
		{"a body with no type", body + " laptop", shape},
		{"nothing", "", shape},
		{"two lines in one", good + "\n" + edKey(t, ""), shape},
		{"a key from an era gone", "ssh-dss " + body, `"ssh-dss" ` + kind},
		{"a certificate", "ssh-ed25519-cert-v01@openssh.com " + body, `"ssh-ed25519-cert-v01@openssh.com" ` + kind},
		{"base64 that is not", "ssh-ed25519 not*base64", "its base64 does not decode"},
		{"a body holding no key", noKey, "its base64 holds no key"},
		{"a name longer than any type", tooLong, "its base64 holds no key"},
		{"a name that is not text", binaryName, "its base64 holds no key"},
		{"a label that lies", rsaInside, "says it is ssh-rsa and holds a ssh-ed25519 key"},
		{"from= before the key", `from="10.0.0.1" ` + good, opts},
		{"a quoted command before the key", `command="echo hi" ` + good, opts},
		{"restrict before the key", "restrict " + good, opts},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(write(t, keyUser("me", "control", "", good, c.key)))
			want := `[[users]] "me": ssh_keys #2: ` + c.want
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%q answers %v, want %q", c.key, err, want)
			}
			if c.key != "" && len(c.key) > 20 && strings.Contains(err.Error(), body) {
				t.Fatalf("the refusal repeats the key: %v", err)
			}
		})
	}
	for _, typ := range []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
		"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com", "ssh-rsa"} {
		blob := append(sshString([]byte(typ)), sshString([]byte("key"))...)
		if typ == "ssh-rsa" {
			blob = rsaBlob(2048)
		}
		line := typ + " " + base64.StdEncoding.EncodeToString(blob)
		if _, err := LoadConfig(write(t, keyUser("me", "control", "", line))); err != nil {
			t.Errorf("a %s key is refused: %v", typ, err)
		}
	}

	// An RSA key is at least 2048 bits, counted from its modulus, and one
	// whose numbers cannot be read is no key.
	for _, c := range []struct {
		name string
		blob []byte
		want string
	}{
		{"1024 bits", rsaBlob(1024), "is an RSA key of 1024 bits; rota takes 2048 bits or more"},
		{"2047 bits", rsaBlob(2047), "is an RSA key of 2047 bits; rota takes 2048 bits or more"},
		{"no modulus", append(sshString([]byte("ssh-rsa")), sshString([]byte{1, 0, 1})...), "its base64 holds no key"},
		{"a modulus cut short", rsaBlob(2048)[:200], "its base64 holds no key"},
	} {
		t.Run("rsa "+c.name, func(t *testing.T) {
			line := "ssh-rsa " + base64.StdEncoding.EncodeToString(c.blob)
			_, err := LoadConfig(write(t, keyUser("me", "control", "", line)))
			if want := `[[users]] "me": ssh_keys #1: ` + c.want; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("answers %v, want %q", err, want)
			}
		})
	}
	if _, err := LoadConfig(write(t, keyUser("me", "control", "", "ssh-rsa "+base64.StdEncoding.EncodeToString(rsaBlob(4096))))); err != nil {
		t.Fatalf("a 4096-bit RSA key is refused: %v", err)
	}

	// The same key under two people is refused, however its comment reads.
	_, err := LoadConfig(write(t, keyUser("me", "control", "", good)+keyUser("you", "watch", "", edKey(t, ""), "ssh-ed25519 "+body+" another comment")))
	if want := `[[users]] "you": ssh_keys #2 is also "me"'s; one key is one person`; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("one key under two users answers %v, want %q", err, want)
	}
	_, err = LoadConfig(write(t, keyUser("me", "control", "", good, "ssh-ed25519 "+body)))
	if want := `[[users]] "me": ssh_keys #2 is also #1; write each key once`; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("one key twice under one user answers %v, want %q", err, want)
	}
}

// TestSSHNeedsSomebodyToSignIn: an SSH server nobody can sign in to is a
// mistake in the file, and so is one with nowhere to listen. host_key is a
// path that does not depend on where the server was started.
func TestSSHNeedsSomebodyToSignIn(t *testing.T) {
	keyed := keyUser("me", "control", "", edKey(t, ""))
	pw := keyUser("you", "control", goodHash())
	for _, c := range []struct{ name, body, want string }{
		{"no user has keys", "[ssh]\nenabled = true\n" + pw,
			"ssh.enabled is true, and no [[users]] entry has ssh_keys, so nobody could sign in"},
		{"no users at all", "[ssh]\nenabled = true\n",
			"ssh.enabled is true, and no [[users]] entry has ssh_keys, so nobody could sign in"},
		{"an empty listen", "[ssh]\nenabled = true\nlisten = \"\"\n" + keyed,
			"ssh.listen is empty; write a host:port such as 127.0.0.1:2222"},
		{"a listen that is no address", "[ssh]\nenabled = true\nlisten = \"a:b:c\"\n" + keyed,
			`ssh.listen: "a:b:c" is not a host:port`},
		{"a relative host_key", "[ssh]\nhost_key = \"keys/host\"\n",
			`ssh.host_key must be an absolute path, not "keys/host"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(write(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%q answers %v, want %q", c.body, err, c.want)
			}
		})
	}
	// Off, the section asks for nothing: a file may name its keys first and
	// turn SSH on later.
	if _, err := LoadConfig(write(t, "[ssh]\nlisten = \"\"\n"+pw)); err != nil {
		t.Fatalf("SSH off asks nothing of listen or of keys: %v", err)
	}
	c, err := LoadConfig(write(t, "[ssh]\nenabled = true\nlisten = \"0.0.0.0:2200\"\nhost_key = \"/etc/rota/host\"\n"+keyed+pw))
	if err != nil {
		t.Fatal(err)
	}
	if c.SSH != (SSHSection{Enabled: true, Listen: "0.0.0.0:2200", HostKey: "/etc/rota/host"}) {
		t.Fatalf("[ssh] read as %+v", c.SSH)
	}
	for _, k := range []string{"ssh.enabled", "ssh.listen", "ssh.host_key"} {
		if c.From[k] != "file" {
			t.Errorf("%s is not remembered as the file's: %q", k, c.From[k])
		}
	}
}

// TestAKeyOnlyUserIsNotOnThePage: a person with keys and no password is not
// a user the server is given, so the page answers them as it answers a name
// it has never heard of: the same words, one derivation, one more failure
// counted. Such a user first in the file starts a server all the same, and
// a server given a user with no password straight is refused, not crashed.
func TestAKeyOnlyUserIsNotOnThePage(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Users = []UserEntry{
		{Name: "phone", Role: "control", SSHKeys: []string{edKey(t, "")}},
		{Name: "driver", Role: "control", Password: cheapHash()},
	}
	opts, _, err := cfg.Serve()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Users) != 1 || opts.Users[0].Name != "driver" {
		t.Fatalf("the server is given %+v; a user with no password is not one", opts.Users)
	}
	p := &people{harness: newHarness(t, Options{Users: opts.Users, LocalUser: opts.LocalUser})}
	c := p.browser(t)
	before := kdfRuns.Load()
	answers := map[string]bool{}
	for _, body := range []string{
		`{"name":"phone","password":""}`,
		`{"name":"phone","password":"x"}`,
		`{"name":"driver","password":"wrong"}`,
	} {
		resp, raw := p.page(t, c, "POST", "/v1/session", body)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(raw), "that name and password do not go together") {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, raw)
		}
		answers[string(raw)] = true
	}
	if len(answers) != 1 {
		t.Fatalf("a key-only user and a wrong password must read the same: %v", answers)
	}
	if got := kdfRuns.Load() - before; got != 3 {
		t.Fatalf("three sign-ins ran the KDF %d times, want 3", got)
	}
	p.api.limit.mu.Lock()
	var counted int
	for _, hits := range p.api.limit.hits {
		counted += len(hits)
	}
	p.api.limit.mu.Unlock()
	if counted != 3 {
		t.Fatalf("the limiter counted %d failures, want 3", counted)
	}

	if _, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1,
		Users: []User{{Name: "phone", Role: RoleControl}, {Name: "driver", Role: RoleControl, Password: cheapHash()}}}); err == nil ||
		!strings.Contains(err.Error(), `[[users]] "phone": password:`) {
		t.Fatalf("a user with no password given to New answers %v", err)
	}
}

// TestAKeyOnlyControlUserIsTheLocalPerson: the one person who may control is
// the control socket's person even when they sign in by key alone, so the
// keyboard follows them from ssh to this machine's own terminal.
func TestAKeyOnlyControlUserIsTheLocalPerson(t *testing.T) {
	local := &Principal{Name: "local", Role: RoleControl, Via: "local"}
	for _, c := range []struct {
		name  string
		users []UserEntry
		named string
		want  string
	}{
		{"the only controller, by key", []UserEntry{
			{Name: "me", Role: "control", SSHKeys: []string{edKey(t, "")}},
			{Name: "viewer", Role: "watch", Password: cheapHash()}}, "", "me"},
		{"named, by key", []UserEntry{
			{Name: "me", Role: "control", SSHKeys: []string{edKey(t, "")}},
			{Name: "you", Role: "control", SSHKeys: []string{edKey(t, "")}}}, "you", "you"},
		{"two controllers by key, nobody named", []UserEntry{
			{Name: "me", Role: "control", SSHKeys: []string{edKey(t, "")}},
			{Name: "you", Role: "control", SSHKeys: []string{edKey(t, "")}}}, "", "local"},
		{"one by password, one by key, the key named", []UserEntry{
			{Name: "driver", Role: "control", Password: cheapHash()},
			{Name: "phone", Role: "control", SSHKeys: []string{edKey(t, "")}}}, "phone", "phone"},
		{"one by password, one by key, nobody named", []UserEntry{
			{Name: "driver", Role: "control", Password: cheapHash()},
			{Name: "phone", Role: "control", SSHKeys: []string{edKey(t, "")}}}, "", "local"},
		{"two by password, nobody named", []UserEntry{
			{Name: "me", Role: "control", Password: cheapHash()},
			{Name: "you", Role: "control", Password: cheapHash()}}, "", "local"},
		{"the only controller, by password", []UserEntry{
			{Name: "me", Role: "control", Password: cheapHash()}}, "", "me"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Users, cfg.Auth.LocalUser = c.users, c.named
			opts, _, err := cfg.Serve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1, Users: opts.Users, LocalUser: opts.LocalUser})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Stop)
			if got := s.personOf(local); got != c.want {
				t.Fatalf("local is %q, want %q", got, c.want)
			}
		})
	}
}
