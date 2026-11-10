package sessions

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// CopyForResume lets a conversation follow the rotation: the target account
// asked to resume a session resumes the newest copy of it that any account of
// the same provider holds, as long as nothing the target holds is lost. When
// the target's home does not hold the session, the newest copy a sibling
// holds is copied in, at the same relative path, so the CLI finds it exactly
// where it would have written it. When the target's home already holds it, a
// sibling's copy replaces the target's only when it is newer and extends it,
// so a chat moved to another account and back keeps what was said there.
// Credentials never move; only the conversation.
//
// A copy is newer when it was written later, and of two written at the same
// moment, the larger is newer. A copy keeps its source's modification time,
// so a copy made later is never taken for newer work. A copy extends the
// target's when it holds more and the target's bytes are the start of it: for
// a conversation that is a folder, every file the target's has is in it too,
// and begins with the target's file. An older or equal copy leaves the
// target's alone, and so does one the conversation has forked into, taken on
// separately on two accounts: then the target's copy is kept exactly as it
// is, and its CLI resumes what was said on it.
//
// Quiet by design when there is nothing to do: the session already home and
// newest, no sibling holding a copy that extends it, or a provider whose
// sessions rota cannot read — the CLI stays the judge of what resumes. An
// error means a copy was attempted and failed, which is worth stopping for;
// each file the target already had is then still whole.
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
	dst, held := locate(flavor, targetHome, id)
	var mine handoffStamp
	if held {
		var ok bool
		if mine, ok = handoffMeasure(dst); !ok {
			return nil // a copy that cannot be measured is left to the CLI
		}
	}
	var (
		src, rel string
		from     *rota.Account
		best     handoffStamp
	)
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
		path, found := locate(flavor, home, id)
		if !found {
			continue
		}
		r, err := filepath.Rel(home, path)
		if err != nil || strings.HasPrefix(r, "..") {
			continue // a session outside its own home is not one to trust
		}
		stamp, ok := handoffMeasure(path)
		if !ok || (src != "" && !stamp.newer(best)) {
			continue // of copies equally new, the first account's is taken
		}
		if held && (!stamp.newer(mine) || stamp.size <= mine.size || !handoffExtends(path, dst)) {
			continue // older, no larger, or forked: it would lose the target's
		}
		src, rel, from, best = path, r, a, stamp
	}
	if src == "" {
		return nil
	}
	if !held {
		dst = filepath.Join(targetHome, rel)
	}
	if err := copyTree(src, dst); err != nil {
		return fmt.Errorf("resuming %s from %s: %w", id, from, err)
	}
	return nil
}

// handoffStamp is how new a copy of a conversation is: when it was last
// written, and how much it holds.
type handoffStamp struct {
	at   time.Time
	size int64
}

// newer reports whether s was written after t, or at the same moment and
// holds more.
func (s handoffStamp) newer(t handoffStamp) bool {
	return s.at.After(t.at) || s.at.Equal(t.at) && s.size > t.size
}

// handoffMeasure measures one copy of a conversation. A file is measured by
// itself. A directory, grok's session, is measured by its newest file and all
// of its files' sizes together: a directory's own time changes when an entry
// comes or goes, not when a file in it grows.
func handoffMeasure(path string) (handoffStamp, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return handoffStamp{}, false
	}
	if !info.IsDir() {
		return handoffStamp{info.ModTime(), info.Size()}, true
	}
	var s handoffStamp
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(s.at) {
			s.at = info.ModTime()
		}
		s.size += info.Size()
		return nil
	})
	return s, err == nil
}

// handoffExtends reports whether the copy at newer begins with everything the
// copy at older holds: a file whose first bytes are older's, or a folder
// holding every file older's does, each beginning with older's. Anything it
// cannot read, or that is neither a file nor a folder, counts as not, so that
// whatever is in doubt is kept.
func handoffExtends(newer, older string) bool {
	info, err := os.Stat(older)
	if err != nil {
		return false
	}
	if info.Mode().IsRegular() {
		return handoffPrefix(newer, older)
	}
	if !info.IsDir() {
		return false
	}
	err = filepath.WalkDir(older, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(older, p)
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || !handoffPrefix(filepath.Join(newer, rel), p) {
			return fs.ErrInvalid
		}
		return nil
	})
	return err == nil
}

// handoffPrefix reports whether the file at newer begins with every byte of
// the file at older.
func handoffPrefix(newer, older string) bool {
	o, err := os.Open(older)
	if err != nil {
		return false
	}
	defer o.Close()
	n, err := os.Open(newer)
	if err != nil {
		return false
	}
	defer n.Close()
	if info, err := n.Stat(); err != nil || !info.Mode().IsRegular() {
		return false
	}
	ob, nb := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		k, err := io.ReadFull(o, ob)
		if k > 0 {
			if _, rerr := io.ReadFull(n, nb[:k]); rerr != nil || !bytes.Equal(ob[:k], nb[:k]) {
				return false
			}
		}
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return true
		case err != nil:
			return false
		}
	}
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

// copyTree copies a file, or a directory recursively, creating parents. A
// file that was there is replaced whole. Nothing is removed: a copy replaces
// only one it extends, so every file the old copy had, src has too.
// Everything lands 0600/0700: these are homes that hold credentials, and the
// transcript inherits the same caution.
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

// copyFile writes src's bytes and modification time to dst. They go to a
// temporary file beside dst first, which then takes dst's name, so a copy
// that fails half-way leaves dst as it was, never half of src.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	// A name no CLI takes for a conversation: not a .jsonl, not a rollout.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".rota-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, in); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chtimes(tmp.Name(), time.Time{}, info.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
