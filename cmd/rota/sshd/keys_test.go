package sshd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/api"
)

func keysRSA(t *testing.T, bits int) ssh.Signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// keysOptions is one person, me, with the lines given, on a fresh host key.
func keysOptions(t *testing.T, lines ...string) Options {
	return Options{
		Listen:     "127.0.0.1:0",
		HostKey:    filepath.Join(t.TempDir(), "ssh_host_ed25519_key"),
		NewHostKey: true,
		Users:      []User{{Name: "me", Role: api.RoleControl, Keys: lines}},
	}
}

func TestKeysAreCheckedAtStart(t *testing.T) {
	good := sshdLine(sshdKey(t).PublicKey())

	ca := sshdKey(t)
	cert := &ssh.Certificate{
		Key: sshdKey(t).PublicKey(), CertType: ssh.UserCert, KeyId: "me",
		ValidPrincipals: []string{"me"}, ValidBefore: ssh.CertTimeInfinity,
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ssh.NewPublicKey(&ec.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		line string
		want string
	}{
		{"rsa 1024", sshdLine(keysRSA(t, 1024).PublicKey()), "1024 bits"},
		{"certificate", sshdLine(cert), "certificate"},
		{"options", `from="x" ` + good, "options"},
		{"command", `command="ls" ` + good, "options"},
		{"two keys", good + "\n" + sshdLine(ecKey), "one key"},
		{"nonsense", "ssh-ed25519 notbase64", "authorized_keys"},
		{"empty", "", "authorized_keys"},
	} {
		_, err := Listen(keysOptions(t, sshdLine(ecKey), c.line))
		if err == nil {
			t.Errorf("%s: Listen took %q", c.name, c.line)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, `[[users]] "me" ssh_keys #2`) || !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q; want it to name [[users]] \"me\" ssh_keys #2 and say %q", c.name, msg, c.want)
		}
		if b64 := strings.Fields(good)[1]; strings.Contains(msg, b64) {
			t.Errorf("%s: the refusal repeats the key: %q", c.name, msg)
		}
	}

	// A key two people list would sign in either; it is refused.
	o := keysOptions(t, good)
	o.Users = append(o.Users, User{Name: "eye", Role: api.RoleWatch, Keys: []string{good}})
	if _, err := Listen(o); err == nil || !strings.Contains(err.Error(), `[[users]] "eye" ssh_keys #1`) ||
		!strings.Contains(err.Error(), `[[users]] "me" ssh_keys #1`) {
		t.Errorf("a key listed twice: %v", err)
	}

	// The accepted kinds start, an RSA key of 2048 bits among them.
	if testing.Short() {
		return
	}
	s, err := Listen(keysOptions(t, good, sshdLine(ecKey), sshdLine(keysRSA(t, 2048).PublicKey())+" a comment"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestHostKeyIsMadeOnceAndKeptPrivate(t *testing.T) {
	o := keysOptions(t, sshdLine(sshdKey(t).PublicKey()))
	s, err := Listen(o)
	if err != nil {
		t.Fatal(err)
	}
	first := s.Fingerprint()
	s.Close()
	if !strings.HasPrefix(first, "SHA256:") {
		t.Errorf("Fingerprint %q", first)
	}
	fi, err := os.Stat(o.HostKey)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("host key mode %04o; want 0600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(o.HostKey)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if k.PublicKey().Type() != ssh.KeyAlgoED25519 || ssh.FingerprintSHA256(k.PublicKey()) != first {
		t.Errorf("host key is %s %s", k.PublicKey().Type(), ssh.FingerprintSHA256(k.PublicKey()))
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(o.HostKey), "*")); len(left) != 1 {
		t.Errorf("the directory holds %q; want the key alone", left)
	}

	// Made once: a second start, with or without leave to make one, keeps it.
	for _, create := range []bool{true, false} {
		o.NewHostKey = create
		s, err := Listen(o)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Fingerprint(); got != first {
			t.Errorf("NewHostKey %v: fingerprint %s, want %s", create, got, first)
		}
		s.Close()
	}

	// Named and missing, without leave to make it: refused, and not made.
	missing := o
	missing.HostKey = filepath.Join(t.TempDir(), "named_key")
	missing.NewHostKey = false
	if _, err := Listen(missing); err == nil || !strings.Contains(err.Error(), missing.HostKey) {
		t.Errorf("a missing named host key: %v", err)
	}
	if _, err := os.Stat(missing.HostKey); err == nil {
		t.Error("a named host key was made")
	}

	relative := o
	relative.HostKey = "ssh_host_ed25519_key"
	if _, err := Listen(relative); err == nil {
		t.Error("a relative host key path was taken")
	}

	// Not ed25519: refused.
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(ec, "")
	if err != nil {
		t.Fatal(err)
	}
	other := o
	other.HostKey = filepath.Join(t.TempDir(), "ecdsa_key")
	if err := os.WriteFile(other.HostKey, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(other); err == nil || !strings.Contains(err.Error(), "ed25519") {
		t.Errorf("an ecdsa host key: %v", err)
	}

	if runtime.GOOS == "windows" {
		return
	}
	// Readable by others: refused, whether or not it may be made.
	if err := os.Chmod(o.HostKey, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{true, false} {
		o.NewHostKey = create
		if _, err := Listen(o); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("NewHostKey %v: a 0644 host key: %v", create, err)
		}
	}
	if err := os.Chmod(o.HostKey, 0o600); err != nil {
		t.Fatal(err)
	}
	// A link where the key should be is refused, even to a private key.
	link := o
	link.HostKey = filepath.Join(t.TempDir(), "link_key")
	if err := os.Symlink(o.HostKey, link.HostKey); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{true, false} {
		link.NewHostKey = create
		if _, err := Listen(link); err == nil {
			t.Errorf("NewHostKey %v: a symbolic link was taken", create)
		}
	}
}

func TestAFailedStartBindsNothing(t *testing.T) {
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	free.Close()
	o := keysOptions(t, "ssh-dss AAAA")
	o.Listen = addr
	if _, err := Listen(o); err == nil {
		t.Fatal("Listen took a bad key")
	}
	o = keysOptions(t, sshdLine(sshdKey(t).PublicKey()))
	o.Listen, o.HostKey, o.NewHostKey = addr, filepath.Join(t.TempDir(), "missing"), false
	if _, err := Listen(o); err == nil {
		t.Fatal("Listen took a missing host key")
	}
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("a refused start left %s bound: %v", addr, err)
	}
	again.Close()
}
