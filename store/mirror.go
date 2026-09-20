package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	rota "github.com/professor93/rota/lib"
)

// mirrorClaude builds or refreshes an account's own view of the person's
// Claude Code configuration, and returns the directory to run it in. An
// account this does not apply to gets an empty directory and no error.
//
// It exists because the token is not the only thing that decides who pays.
// Claude Code hosts background sessions, the agent view, attached sessions
// and the "N ⧉" sub-sessions in a helper process — one daemon per
// configuration directory — and that daemon signs in for itself. The one
// already running for the person's own directory was started by a plain
// `claude`, has none of rota's tokens, and falls back to the keychain login:
// so hours of background work are billed to that account no matter which
// window rota launched. A configuration directory of its own gives an
// account a daemon of its own, and that daemon inherits the environment rota
// gave the client — token included.
//
// A directory of its own would otherwise mean an empty world: no settings,
// no memory, no skills, no plugins, no trusted folders, and none of the
// conversations there are to resume. So it is a mirror rather than a copy —
// one symlink per entry of the person's own directory, refreshed on every
// launch. Claude Code writes through the links, so the two remain the same
// files and nothing has to be merged back. What is left out is what must not
// be shared: the `daemon*` files, which are the state this whole exercise
// separates, and `.credentials.json`, a vendor credential store rota never
// reads or writes.
func (s *Store) mirrorClaude(a *rota.Account) (string, error) {
	if a.Provider != "claude" || a.ConfigDir != "" {
		// An account told where its configuration lives was given a world of
		// its own deliberately, and lib already points the CLI at it. Adding
		// a mirror would be a second CLAUDE_CONFIG_DIR and a second answer.
		return "", nil
	}
	src, srcJSON := claudeConfigSource()
	dst := s.ownHome(a)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return dst, err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing to mirror is not a failure. Claude Code makes itself a
			// fresh world in the directory, which is what it would have done
			// in the missing one.
			return dst, nil
		}
		return dst, err
	}
	have, err := os.ReadDir(dst)
	if err != nil {
		return dst, err
	}
	// What is already here is never replaced, link or not: the account's
	// staged credential, its run lock, and anything Claude Code wrote here
	// itself are its own, and a mirror that overwrote them would be handing
	// one account's files to another.
	mine := make(map[string]fs.DirEntry, len(have))
	for _, e := range have {
		mine[e.Name()] = e
	}
	link := func(from, name string) error {
		if _, ok := mine[name]; ok {
			return nil
		}
		err := os.Symlink(from, filepath.Join(dst, name))
		if err != nil && errors.Is(err, fs.ErrExist) {
			// Another run of the same account got there between the listing
			// and now. It wrote the same link; there is nothing to correct.
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !sharedWithClaude(e.Name()) {
			continue
		}
		if err := link(filepath.Join(src, e.Name()), e.Name()); err != nil {
			return dst, err
		}
	}
	// .claude.json is the one file that does not live inside the directory
	// unless CLAUDE_CONFIG_DIR says so, and it is the one holding trust
	// decisions, project history and the identity the CLI shows.
	if _, err := os.Stat(srcJSON); err == nil {
		if err := link(srcJSON, ".claude.json"); err != nil {
			return dst, err
		}
	}
	// An entry the person deleted from their own directory would linger here
	// as a link to nothing, which Claude Code reads as a broken file rather
	// than as an absent one. Only links are ever removed.
	for name, e := range mine {
		if e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		p := filepath.Join(dst, name)
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(p)
		}
	}
	return dst, nil
}

// sharedWithClaude reports whether one entry of the person's Claude Code
// directory may be shared with an account.
//
// Every file the daemon keeps is named for it — the lock, the log, the
// status, the auth cooldown — so the prefix is the rule rather than a list
// that would go stale the next time Claude Code invents one.
// `.credentials.json` is where some platforms keep the CLI's own login, and
// rota hands credentials to Claude Code through the environment and never
// through a vendor's credential store: an account must not be reading the
// person's. `.claude.json` is dealt with on its own, and `.DS_Store` is
// noise a file manager leaves behind.
func sharedWithClaude(name string) bool {
	switch {
	case strings.HasPrefix(name, "daemon"):
		return false
	case name == ".credentials.json", name == ".claude.json", name == ".DS_Store":
		return false
	}
	return true
}

// claudeConfigSource is the Claude Code configuration this process would
// itself have used, and the .claude.json that belongs with it: inside the
// directory when CLAUDE_CONFIG_DIR names one, beside the home directory
// when it does not, which is where Claude Code looks for it.
//
// Reading the environment is this package's business and not the SDK's. What
// is worth mirroring is the world the person running rota is actually in —
// including one an outer CLAUDE_CONFIG_DIR has already moved.
func claudeConfigSource() (dir, json string) {
	if own := os.Getenv("CLAUDE_CONFIG_DIR"); own != "" {
		return own, filepath.Join(own, ".claude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	return filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")
}
