package sshd

import (
	"bytes"
	"crypto/rsa"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
)

// keysKinds are the public keys a person may list (Ruling 6). An ssh-rsa key
// is one of them, but it signs only by keysAlgorithms' SHA-2 names.
var keysKinds = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256,
	ssh.KeyAlgoRSA,
}

// keysAlgorithms are the signatures a sign-in may carry: every kind above,
// with RSA by SHA-256 and SHA-512 alone and never by SHA-1's ssh-rsa.
var keysAlgorithms = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256,
	ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512,
}

// keysRSABits is the shortest RSA key taken.
const keysRSABits = 2048

// keysParse reads every person's ssh_keys, each one authorized_keys line
// holding one plain key, and gives each key's wire form with the index of
// the person it signs in. A line with options, a certificate, a kind not in
// keysKinds, an RSA key under keysRSABits, or a key two entries list is
// refused, naming the entry; the key itself is never repeated.
func keysParse(users []User) (map[string]int, error) {
	known := map[string]int{}
	where := map[string]string{}
	named := map[string]bool{}
	for ui, u := range users {
		// A sign-in names its person, so a name is one person.
		if named[u.Name] {
			return nil, fmt.Errorf("[[users]] %q is named twice", u.Name)
		}
		named[u.Name] = true
		for ki, line := range u.Keys {
			at := fmt.Sprintf("[[users]] %q ssh_keys #%d", u.Name, ki+1)
			k, err := keysLine(line)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
			wire := string(k.Marshal())
			if first, ok := where[wire]; ok {
				return nil, fmt.Errorf("%s is the key %s already lists; one key signs in one person", at, first)
			}
			known[wire], where[wire] = ui, at
		}
	}
	return known, nil
}

// keysLine parses one entry. ParseAuthorizedKey skips a line it cannot read
// and takes the next, so an entry of more than one line is refused before it
// is asked: a mistyped key never hides behind a good one.
func keysLine(line string) (ssh.PublicKey, error) {
	if strings.ContainsAny(line, "\r\n") {
		return nil, fmt.Errorf("holds more than one line; write one key per entry")
	}
	k, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, fmt.Errorf("not an authorized_keys line holding a key (%v)", err)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("holds more than one key; write one key per entry")
	}
	if len(options) > 0 {
		return nil, fmt.Errorf("has options (%s); rota takes a plain key, so remove them",
			keysOptionNames(options))
	}
	if _, ok := k.(*ssh.Certificate); ok {
		return nil, fmt.Errorf("is a certificate; rota takes a plain key")
	}
	if !slices.Contains(keysKinds, k.Type()) {
		return nil, fmt.Errorf("is an %s key; rota takes %s", k.Type(), strings.Join(keysKinds, ", "))
	}
	if k.Type() == ssh.KeyAlgoRSA {
		ck, ok := k.(ssh.CryptoPublicKey)
		if !ok {
			return nil, fmt.Errorf("is an RSA key rota cannot read")
		}
		r, ok := ck.CryptoPublicKey().(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("is an RSA key rota cannot read")
		}
		if bits := r.N.BitLen(); bits < keysRSABits {
			return nil, fmt.Errorf("is an RSA key of %d bits; rota takes %d bits or more", bits, keysRSABits)
		}
	}
	return k, nil
}

// keysOptionNames names the options a line has without their values, which
// may hold anything.
func keysOptionNames(options []string) string {
	names := make([]string, len(options))
	for i, o := range options {
		name, _, _ := strings.Cut(o, "=")
		names[i] = name
	}
	return strings.Join(names, ", ")
}
