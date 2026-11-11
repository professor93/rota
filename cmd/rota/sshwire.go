package main

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/attach"
	"github.com/professor93/rota/cmd/rota/sshd"
	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/store"
)

// The SSH server, as the daemon and foreground `rota serve` run it: only
// where [ssh] enabled says so, and never in a daemon a session started by
// itself (--ctl-only). It reaches the API through the server's own
// LocalHandler, as each person whose [[users]] entry lists the key they
// signed in with.

// sshwireHostKey is the host key's file in the store, made on first use,
// where the file names none.
const sshwireHostKey = "ssh_host_ed25519_key"

// sshwireNotHere is the refusal on a platform with no pseudo-terminal, where an
// attach has no terminal to give the person.
var sshwireNotHere = errors.New("the SSH server runs on linux and macOS")

// sshOptions is what the SSH server of the store at home, whose terminals
// directory is terminals, runs with: [ssh] listen, as server.listen is read
// (a bare port is every interface); [ssh] host_key, or the store's own
// made on first use; every [[users]] entry with ssh_keys, with its role;
// srv's LocalHandler; the chord key a local attach reads; and the record of
// who has been shown the chord. Its TUI is phase 3's to set, here.
func sshOptions(cfg *api.Config, home, terminals string, srv *api.Server, log *slog.Logger) (sshd.Options, error) {
	if !pty.Supported {
		return sshd.Options{}, sshwireNotHere
	}
	addr, err := api.ListenAddr(cfg.SSH.Listen)
	if err != nil {
		return sshd.Options{}, fmt.Errorf("ssh.listen: %w", err)
	}
	o := sshd.Options{
		Listen:  addr,
		HostKey: cfg.SSH.HostKey,
		Handler: srv.LocalHandler,
		Log:     log,
		Key:     attach.DaemonKey(terminals),
		Taught:  attach.TaughtFile(terminals),
	}
	if o.HostKey == "" {
		o.HostKey, o.NewHostKey = filepath.Join(home, sshwireHostKey), true
	}
	for _, u := range cfg.Users {
		if len(u.SSHKeys) > 0 {
			o.Users = append(o.Users, sshd.User{Name: u.Name, Role: api.Role(u.Role), Keys: slices.Clone(u.SSHKeys)})
		}
	}
	return o, nil
}

// sshwireListen starts the SSH server sshOptions describes, bound and with its
// host key read, but not yet serving: a start that cannot is refused before
// anything says it serves.
func sshwireListen(cfg *api.Config, home, terminals string, srv *api.Server, log *slog.Logger) (*sshd.Server, error) {
	o, err := sshOptions(cfg, home, terminals, srv, log)
	if err != nil {
		return nil, err
	}
	s, err := sshd.Listen(o)
	if err != nil {
		return nil, fmt.Errorf("the SSH server cannot start: %w", err)
	}
	return s, nil
}

// sshwireServe is sshwireListen for foreground `rota serve`, on the store
// dir names ("" is rota's default), as srv serves it: the host key made in
// that store, and who has been shown the chord recorded in its terminals
// directory.
func sshwireServe(cfg *api.Config, dir string, srv *api.Server, log *slog.Logger) (*sshd.Server, error) {
	if !pty.Supported {
		return nil, sshwireNotHere
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	terminals, err := st.TerminalDir()
	st.Close()
	if err != nil {
		return nil, err
	}
	if terminals, err = filepath.Abs(terminals); err != nil {
		return nil, err
	}
	return sshwireListen(cfg, filepath.Dir(terminals), terminals, srv, log)
}
