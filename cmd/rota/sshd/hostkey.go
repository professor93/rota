package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/internal/owner"
)

// hostkeyMax bounds what is read of a host key file: an ed25519 key in
// OpenSSH's format is under half a kilobyte.
const hostkeyMax = 64 << 10

// hostkeyLoad gives the server's host key from path, which must be absolute.
// With create and no file there, an ed25519 key is made first (Ruling 8: only
// the default path is made). Either way the file is read as a file only its
// owner may use, and it must hold an ed25519 key: that is the only kind
// rota's server signs with.
func hostkeyLoad(path string, create bool) (ssh.Signer, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("the SSH host key %q is not an absolute path", path)
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, fmt.Errorf("the SSH host key %s does not exist; rota makes one only at "+
				"its default path, so make it with: ssh-keygen -t ed25519 -N '' -f %s", path, path)
		}
		if err := hostkeyMake(path); err != nil {
			return nil, fmt.Errorf("the SSH host key %s cannot be made: %w", path, err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDONLY|owner.OpenFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("the SSH host key %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("the SSH host key %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("the SSH host key %s is not a plain file", path)
	}
	if err := owner.Only(path, fi.Mode()); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, hostkeyMax+1))
	if err != nil {
		return nil, fmt.Errorf("the SSH host key %s: %w", path, err)
	}
	if len(raw) > hostkeyMax {
		return nil, fmt.Errorf("the SSH host key %s is too large to be a key", path)
	}
	k, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("the SSH host key %s cannot be read: %w", path, err)
	}
	if t := k.PublicKey().Type(); t != ssh.KeyAlgoED25519 {
		return nil, fmt.Errorf("the SSH host key %s is an %s key; rota's server signs with ed25519 alone", path, t)
	}
	return k, nil
}

// hostkeyMake writes a new ed25519 key at path, 0600, through a temporary
// file in the same directory and a rename, so the key is never seen half
// written.
func hostkeyMake(path string) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(pem.EncodeToMemory(block))
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
