package api

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
)

// A password in server.toml is never the password. It is written as one
// field in a shape that says how it was derived, so the numbers can be
// raised later without a second key to say which entries are old:
//
//	pbkdf2-sha256$600000$<salt, base64>$<hash, base64>
//
// PBKDF2 is the choice because it is the one password KDF in the standard
// library (crypto/pbkdf2, Go 1.24), and rota takes no dependencies. It is
// weaker per unit of work than scrypt or argon2 against custom hardware;
// what it buys here is that the whole verification is four lines of code
// nobody has to trust a third party for. The iteration count is the dial,
// and the floor below is what makes a copied-in entry with a small one an
// error rather than a quiet weakening.
const (
	pwAlgorithm = "pbkdf2-sha256"
	// pwIterations is what `rota serve passwd` writes today. A file may say
	// more; it may not say less than pwMinIterations.
	pwIterations = 600000
	// pwMinIterations is the floor a file is held to. OWASP's figure for
	// PBKDF2-SHA256 at the time of writing; below it, the stored hash is
	// not worth the ceremony around it.
	pwMinIterations = 100000
	pwSaltLen       = 16
	pwKeyLen        = 32
)

// kdfRuns counts every password derivation this process has done. Nothing
// reads it but a test, which needs to see that an unknown name costs the
// same work as a known one — the fact wall-clock timing is too noisy to
// assert on directly.
var kdfRuns atomic.Int64

// password is one parsed `password = "…"` field: the cost, the salt, and
// what the right password derives to.
type password struct {
	iterations int
	salt       []byte
	hash       []byte
}

// parsePassword reads the stored form, or says which part of it is wrong.
// Every message names the part rather than the whole, because the person
// reading it is looking at one long line in a file.
func parsePassword(s string) (*password, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 4 {
		return nil, fmt.Errorf("must be %s$<iterations>$<salt>$<hash>, four parts separated by $", pwAlgorithm)
	}
	if parts[0] != pwAlgorithm {
		return nil, fmt.Errorf("%q is not an algorithm rota knows; it writes %s", parts[0], pwAlgorithm)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%q is not a number of iterations", parts[1])
	}
	if n < pwMinIterations {
		return nil, fmt.Errorf("%d iterations is fewer than the %d rota insists on", n, pwMinIterations)
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("the salt is not base64: %v", err)
	}
	if len(salt) < pwSaltLen {
		return nil, fmt.Errorf("the salt is %d bytes; rota insists on at least %d", len(salt), pwSaltLen)
	}
	hash, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, fmt.Errorf("the hash is not base64: %v", err)
	}
	if len(hash) != pwKeyLen {
		return nil, fmt.Errorf("the hash is %d bytes; a %s hash is %d", len(hash), pwAlgorithm, pwKeyLen)
	}
	return &password{iterations: n, salt: salt, hash: hash}, nil
}

// matches derives from the offered password and compares in constant time.
// The derivation happens whatever the answer turns out to be, which is the
// whole point of running one for a name that does not exist either.
func (p *password) matches(given string) bool {
	kdfRuns.Add(1)
	got, err := pbkdf2.Key(sha256.New, given, p.salt, p.iterations, pwKeyLen)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, p.hash) == 1
}

// String is the stored form again, which is what `rota serve passwd` prints.
func (p *password) String() string {
	return fmt.Sprintf("%s$%d$%s$%s", pwAlgorithm, p.iterations,
		base64.StdEncoding.EncodeToString(p.salt), base64.StdEncoding.EncodeToString(p.hash))
}

// HashPassword derives the field a `[[users]]` entry carries, with a fresh
// random salt. It is exported because the command that prints the block is
// in another package, and the shape of the field is this one's to decide.
func HashPassword(plain string) (string, error) {
	if plain == "" {
		return "", errors.New("a password with nothing in it is not a password")
	}
	salt := make([]byte, pwSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	kdfRuns.Add(1)
	key, err := pbkdf2.Key(sha256.New, plain, salt, pwIterations, pwKeyLen)
	if err != nil {
		return "", err
	}
	p := &password{iterations: pwIterations, salt: salt, hash: key}
	return p.String(), nil
}

// decoyFor is what a login for an unknown name is checked against. Such a
// login still costs one derivation, so the time an answer takes cannot be
// read as "that name exists".
//
// Its cost is the first configured user's rather than a constant: entries
// all come out of the same `rota serve passwd`, and a decoy at some other
// figure would put the difference straight back. Its salt is random per
// process and its hash is 32 zero bytes, which no password derives to.
func decoyFor(users []user) *password {
	n := pwIterations
	if len(users) > 0 {
		n = users[0].pw.iterations
	}
	salt := make([]byte, pwSaltLen)
	_, _ = rand.Read(salt)
	return &password{iterations: n, salt: salt, hash: make([]byte, pwKeyLen)}
}
