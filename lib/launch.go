package rota

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Environ merges a command's variables into an inherited environment. Every
// variable the command sets and every one the provider wants dropped is
// removed from the inherited set first: runtimes disagree on which duplicate
// wins (libc and Node take the first, Python the last), so the child must see
// exactly one value for each.
//
// What may be inherited at all is the caller's decision: the SDK never reads
// the process environment, so an application passes what it wants a child to
// see — with its own secrets already removed.
func Environ(inherited []string, cmd *Command) []string {
	drop := make(map[string]bool, len(cmd.Drop)+len(cmd.Env))
	for _, k := range cmd.Drop {
		drop[k] = true
	}
	for _, e := range cmd.Env {
		k, _, _ := strings.Cut(e, "=")
		drop[k] = true
	}
	out := make([]string, 0, len(inherited)+len(cmd.Env))
	for _, e := range inherited {
		if k, _, _ := strings.Cut(e, "="); !drop[k] {
			out = append(out, e)
		}
	}
	return append(out, cmd.Env...)
}

// cliRotated reports whether a refresh token found in a staged credential
// file is one the CLI rotated on its own — which must be adopted, because
// providers reject a reused refresh token permanently — rather than one
// rota wrote there itself or one left by a previous login.
func (a *Account) cliRotated(fileRefresh string) bool {
	switch {
	case fileRefresh == "" || fileRefresh == a.Token.Refresh:
		return false
	case a.Staged == stagedNone:
		return false // staged before the current login: older, not newer
	case a.Staged == "":
		return true // unknown provenance: assume the CLI has been busy
	}
	return fingerprint(fileRefresh) != a.Staged
}

// stageRaw writes one planned credential file into the private home and
// records which refresh token it carries, so the next run can tell a
// rotation by the CLI from this package's own write. An application that
// takes the files from StagePlan and writes them itself performs the same
// record by setting a.Staged from the refresh token it wrote — or leaves
// Staged empty and lets adoption treat the file as the CLI's.
func stageRaw(a *Account, path string, f StagedFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, f.Content, f.Mode); err != nil {
		return err
	}
	a.Staged = fingerprint(a.Token.Refresh)
	return nil
}

// readJSON loads a file the CLI may have rewritten. A missing or corrupt
// file just means rota's copy is the one that counts.
func readJSON(path string, out any) bool {
	raw, err := os.ReadFile(path)
	return err == nil && decodeLenient(raw, out) == nil
}

// readJSONFS is readJSON through a filesystem value, for adoption that
// reads homes an application keeps somewhere other than this disk.
func readJSONFS(fsys fs.FS, name string, out any) bool {
	raw, err := fs.ReadFile(fsys, name)
	return err == nil && decodeLenient(raw, out) == nil
}
