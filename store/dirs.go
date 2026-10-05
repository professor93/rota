package store

import (
	"os"
	"path/filepath"
)

// sameDir reports whether two paths name one directory. Where both exist the
// answer is by file identity, which is the only answer on a volume that
// ignores case — macOS's, Windows' — where …/Shared and …/shared are one
// directory under two spellings. Where one does not exist yet, by the
// cleaned path its existing parents lead to.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(fa, fb)
	}
	return realDir(a) == realDir(b)
}

// inside reports whether path is root or lies inside it, by file identity
// wherever the directories exist, so a second spelling of root does not get
// past it.
func inside(root, path string) bool {
	if within(realDir(root), realDir(path)) {
		return true
	}
	fr, err := os.Stat(root)
	if err != nil {
		return false
	}
	for p := realDir(path); ; {
		if fp, err := os.Stat(p); err == nil && os.SameFile(fr, fp) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// claudeHomeVar is how rota hands the person's own Claude Code directory
// down to everything it launches: the directory when the person's world came
// from an explicit CLAUDE_CONFIG_DIR, and set but empty when it was the
// default. It is not a secret — it names a directory, not a credential — so
// it is not among the variables hidden from agents.
const claudeHomeVar = "ROTA_CLAUDE_HOME"

// personalWorld is the person's own Claude Code directory as the environment
// this process was started with says it, and whether a variable named it.
//
// The answer must not depend on who started rota. A rota launched by rota
// inherits a CLAUDE_CONFIG_DIR that is an account's home, a hermetic run's
// throwaway directory, or the home of another store — none of them the
// person's, and not all of them anything this store could recognise. So:
//   - ROTA_CLAUDE_HOME, when set, is the answer: the directory it names, or
//     the default when it is empty;
//   - unset, but ROTA_ACCOUNT_ID set, this rota was launched by an older or
//     another rota, and its CLAUDE_CONFIG_DIR is not the person's: the
//     default;
//   - with neither, CLAUDE_CONFIG_DIR is the person's own, and without it
//     the default.
//
// The default is ~/.claude, beside ~/.claude.json.
func personalWorld() (dir string, explicit bool) {
	if v, ok := os.LookupEnv(claudeHomeVar); ok {
		if v != "" {
			return v, true
		}
		return defaultClaudeDir(), false
	}
	if os.Getenv("ROTA_ACCOUNT_ID") != "" {
		return defaultClaudeDir(), false
	}
	if own := os.Getenv("CLAUDE_CONFIG_DIR"); own != "" {
		return own, true
	}
	return defaultClaudeDir(), false
}

func defaultClaudeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// PersonalClaudeDir is the person's own Claude Code configuration directory,
// as this process's environment says it (see personalWorld): the place whose
// conversations are everybody's when accounts share theirs.
func PersonalClaudeDir() string {
	dir, _ := personalWorld()
	return dir
}

// personalDir is personalWorld, with the one thing only a store can add: a
// directory inside its own homes is an account's, never the person's.
func (s *Store) personalDir() (dir string, explicit bool) {
	dir, explicit = personalWorld()
	if explicit && inside(s.homeRoot, dir) {
		return defaultClaudeDir(), false
	}
	return dir, explicit
}

// personalSource is the person's own Claude Code configuration and the
// .claude.json that belongs with it: inside the directory when a variable
// named it, beside the home directory when it is the default, which is where
// Claude Code looks for it.
func (s *Store) personalSource() (dir, json string) {
	dir, explicit := s.personalDir()
	if explicit {
		return dir, filepath.Join(dir, ".claude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dir, ""
	}
	return dir, filepath.Join(home, ".claude.json")
}

// handDown is the value of ROTA_CLAUDE_HOME for what this store launches.
func (s *Store) handDown() string {
	if dir, explicit := s.personalDir(); explicit {
		return dir
	}
	return ""
}
