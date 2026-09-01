package sessions

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// CopyForResume lets a conversation follow the rotation: when the target
// account is asked to resume a session its own home does not hold, but a
// sibling account of the same provider does, the transcript is copied into
// the target's home — same relative path, so the CLI finds it exactly where
// it would have written it. Credentials never move; only the conversation.
//
// Quiet by design when there is nothing to do: the session already home, no
// sibling holding it, or a provider whose sessions rota cannot read — the
// CLI stays the judge of what resumes. An error means a copy was attempted
// and failed, which is worth stopping for.
//
// Copying an actively-running session is inherently racy; the copy is a
// snapshot of whatever the source CLI had written when it was taken.
func CopyForResume(st *store.Store, target *rota.Account, id string) error {
	flavor := rota.Flavor(target.Provider)
	if !readable(target.Provider) || id == "" {
		return nil
	}
	targetHome, _ := ConfigHome(target, st.Home(target))
	if targetHome == "" {
		return nil
	}
	if _, found := locate(flavor, targetHome, id); found {
		return nil
	}
	seen := map[string]bool{targetHome: true}
	for _, a := range st.Accounts {
		if rota.Flavor(a.Provider) != flavor || a.ID == target.ID {
			continue
		}
		home, _ := ConfigHome(a, st.Home(a))
		if home == "" || seen[home] {
			continue
		}
		seen[home] = true
		src, found := locate(flavor, home, id)
		if !found {
			continue
		}
		rel, err := filepath.Rel(home, src)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue // a session outside its own home is not one to trust
		}
		if err := copyTree(src, filepath.Join(targetHome, rel)); err != nil {
			return fmt.Errorf("resuming %s from %s: %w", id, a, err)
		}
		return nil
	}
	return nil
}

// locate finds the transcript for one session id in one home. For grok the
// answer is the session's directory; for claude and codex the jsonl file.
func locate(flavor, home, id string) (path string, found bool) {
	r, ok := readers[flavor]
	if !ok {
		return "", false
	}
	list, err := r.list(home)
	if err != nil {
		return "", false
	}
	for _, s := range list {
		if !strings.EqualFold(s.ID, id) {
			continue
		}
		if flavor == "grok" {
			return filepath.Dir(s.path), true // the whole session directory
		}
		return s.path, true
	}
	return "", false
}

// copyTree copies a file, or a directory recursively, creating parents.
// Everything lands 0600/0700: these are homes that hold credentials, and
// the transcript inherits the same caution.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst)
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o700)
		}
		return copyFile(p, out)
	})
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o600)
}
