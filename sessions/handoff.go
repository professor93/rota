package sessions

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// CopyForResume lets a conversation follow the rotation: the target account
// asked to resume a session resumes the newest copy of it that keeps
// everything the target's own copy holds. Credentials never move; only the
// conversation.
//
// When the target's home does not hold the session, the newest copy a sibling
// account of the same provider holds is copied in, at the same relative path,
// so the CLI finds it exactly where it would have written it. Whatever is
// already at that path is left as it is.
//
// When the target's home holds it, a sibling's copy that is newer (written
// later, or as recently and larger) and extends the target's (the target's
// bytes, then more) gives the target only what it has more, appended to the
// target's own file. The file stays the same file, so a CLI still writing to
// it loses nothing, and a chat moved to another account and back keeps what
// was said there. A conversation that is a folder, grok's, is extended when
// each of its logs (*.jsonl) is, and its other files then come along. A log
// is copied to its last complete line, never half a record.
//
// An older or equal copy leaves the target's alone, unless the target's ends
// part-way through a record, as only a copy cut short leaves it: then it is
// finished from a copy that extends it. A copy the conversation has forked
// into, taken on separately on two accounts, leaves it alone too: the
// target's copy is kept as it is, its CLI resumes what was said on it, and
// the store's Warn is told where the rest was said.
//
// Quiet by design when there is nothing to do: the session already home and
// newest, no sibling holding a copy that extends it, or a provider whose
// sessions rota cannot read — the CLI stays the judge of what resumes. An
// error means a copy was attempted and failed, which is worth stopping for;
// whatever the target had is still there, and a later resume finishes the
// copy.
//
// A copy taken from a CLI that is running holds the records it had finished.
// A target whose file was written to after it was read, by a CLI running on
// it, is not appended to.
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
	type sibling struct {
		path, rel string
		from      *rota.Account
		stamp     handoffStamp
	}
	var copies []sibling
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
		rel, err := filepath.Rel(home, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue // a session outside its own home is not one to trust
		}
		if held && handoffAlias(path, dst) {
			continue // the target's own copy, reached another way
		}
		if stamp, ok := handoffMeasure(path); ok {
			copies = append(copies, sibling{path, rel, a, stamp})
		}
	}
	// Newest first; of copies equally new, the first account's.
	slices.SortStableFunc(copies, func(x, y sibling) int {
		switch {
		case x.stamp.newer(y.stamp):
			return -1
		case y.stamp.newer(x.stamp):
			return 1
		}
		return 0
	})

	if !held {
		if len(copies) == 0 {
			return nil
		}
		c := copies[0]
		dst = filepath.Join(targetHome, c.rel)
		if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
			return nil // something the listing did not see is there: the CLI's to judge
		}
		if err := handoffCreate(c.path, dst); err != nil {
			return fmt.Errorf("resuming %s from %s: %w", id, c.from, err)
		}
		return nil
	}

	// A target cut short part-way through a record was being extended when
	// the copy stopped; it is newer only for that, and is finished from
	// whichever copy extends it.
	cut := handoffCutShort(dst)
	var fork *rota.Account
	for _, c := range copies {
		newer := c.stamp.newer(mine)
		if !newer && !cut {
			break // and neither is any after it
		}
		did, apart, err := handoffExtend(c.path, dst)
		if err != nil {
			return fmt.Errorf("resuming %s from %s: %w", id, c.from, err)
		}
		if apart && newer && fork == nil {
			fork = c.from
		}
		if did {
			break
		}
	}
	if fork != nil && st.Warn != nil {
		st.Warn(fmt.Sprintf("conversation %s went separate ways on %s and on %s; %s resumes its own, and what was said on %s stays there",
			id[:min(len(id), 8)], target, fork, target, fork))
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
// itself. A folder, grok's conversation, is measured by its newest file and
// all of its files' sizes together: a folder's own time changes when an entry
// comes or goes, not when a file in it grows.
func handoffMeasure(path string) (handoffStamp, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return handoffStamp{}, false
	}
	if !info.IsDir() {
		return handoffStamp{info.ModTime(), info.Size()}, true
	}
	files, ok := handoffFiles(path)
	var s handoffStamp
	for _, f := range files {
		if f.ModTime().After(s.at) {
			s.at = f.ModTime()
		}
		s.size += f.Size()
	}
	return s, ok
}

// handoffFiles lists a conversation folder's regular files by their path
// inside it. A temporary file a copy left behind is not one of them: it is
// skipped, and removed once it is an hour old, when no copy can still be
// writing it.
func handoffFiles(dir string) (map[string]fs.FileInfo, bool) {
	files := map[string]fs.FileInfo{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if handoffLeftover(p, info) {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files[rel] = info
		return nil
	})
	return files, err == nil
}

// handoffLeftover reports whether a file is one of the temporary files a
// copy writes first, and removes it when it is an hour old.
func handoffLeftover(path string, info fs.FileInfo) bool {
	name := filepath.Base(path)
	if !strings.HasPrefix(name, ".rota-") || !strings.HasSuffix(name, ".tmp") {
		return false
	}
	if time.Since(info.ModTime()) > time.Hour {
		_ = os.Remove(path)
	}
	return true
}

// handoffSweep removes what copies left behind in one folder.
func handoffSweep(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			handoffLeftover(filepath.Join(dir, e.Name()), info)
		}
	}
}

// handoffLog reports whether a file is a log, written a record to a line and
// only ever added to: a claude transcript, a codex rollout, grok's history.
func handoffLog(name string) bool { return strings.HasSuffix(name, ".jsonl") }

// handoffCutShort reports whether a copy ends part-way through a record: a
// log whose last byte is not a newline. A CLI writes whole records, so only a
// write cut short leaves one.
func handoffCutShort(path string) bool {
	logs := []string{path}
	if info, err := os.Stat(path); err != nil {
		return false
	} else if info.IsDir() {
		files, _ := handoffFiles(path)
		logs = logs[:0]
		for rel := range files {
			if handoffLog(rel) {
				logs = append(logs, filepath.Join(path, rel))
			}
		}
	}
	for _, p := range logs {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		last := make([]byte, 1)
		info, err := f.Stat()
		if err == nil && info.Size() > 0 {
			if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
				f.Close()
				return true
			}
		}
		f.Close()
	}
	return false
}

// handoffAlias reports whether two paths reach one file or folder.
func handoffAlias(a, b string) bool {
	x, err := os.Stat(a)
	if err != nil {
		return false
	}
	y, err := os.Stat(b)
	return err == nil && os.SameFile(x, y)
}

// How a newer copy of a log stands to the target's.
const (
	handoffEqual  = iota // neither holds a complete record the other lacks
	handoffAhead         // the newer copy goes on from the target's last byte
	handoffBehind        // the target's goes on from the newer copy's last byte
	handoffApart         // they differ: the conversation forked
	handoffUnread        // one of them could not be read through
)

// handoffTail reads the log at newer against the target's log at older, once
// each, and says how the two stand. When newer is ahead, tail is what it holds
// past older's end, up to its last complete line, and was is older as it
// stood when it was read.
func handoffTail(newer, older string) (tail []byte, was fs.FileInfo, how int) {
	o, err := os.Open(older)
	if err != nil {
		return nil, nil, handoffUnread
	}
	defer o.Close()
	was, err = o.Stat()
	if err != nil || !was.Mode().IsRegular() {
		return nil, nil, handoffUnread
	}
	n, err := os.Open(newer)
	if err != nil {
		return nil, nil, handoffUnread
	}
	defer n.Close()
	if info, err := n.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, nil, handoffUnread
	}
	ob, nb := make([]byte, 64<<10), make([]byte, 64<<10)
	for left := was.Size(); left > 0; left -= int64(len(ob)) {
		k := int(min(left, int64(len(ob))))
		if _, err := io.ReadFull(o, ob[:k]); err != nil {
			return nil, nil, handoffUnread // shorter than it was: being rewritten
		}
		m, err := io.ReadFull(n, nb[:k])
		if !bytes.Equal(ob[:m], nb[:m]) {
			return nil, was, handoffApart
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, was, handoffBehind
		}
		if err != nil {
			return nil, nil, handoffUnread
		}
	}
	rest, err := io.ReadAll(n)
	if err != nil {
		return nil, nil, handoffUnread
	}
	end := bytes.LastIndexByte(rest, '\n')
	if end < 0 {
		return nil, was, handoffEqual
	}
	return rest[:end+1], was, handoffAhead
}

// handoffExtend gives the target's copy at older what the newer copy holds
// past it. did says something was added; apart says the two have forked, and
// nothing was.
func handoffExtend(newer, older string) (did, apart bool, err error) {
	info, err := os.Stat(older)
	if err != nil {
		return false, false, nil
	}
	if !info.IsDir() {
		tail, was, how := handoffTail(newer, older)
		switch how {
		case handoffApart:
			return false, true, nil
		case handoffAhead:
			did, err := handoffAppend(older, was, tail)
			return did, false, err
		}
		return false, false, nil
	}

	theirs, ok := handoffFiles(newer)
	ours, ok2 := handoffFiles(older)
	if !ok || !ok2 {
		return false, false, nil
	}
	type grow struct {
		rel  string
		tail []byte
		was  fs.FileInfo
	}
	var grows []grow
	ahead, behind := false, false
	for _, rel := range slices.Sorted(maps.Keys(ours)) {
		if !handoffLog(rel) {
			continue
		}
		if _, there := theirs[rel]; !there {
			behind = true // a log only the target has
			continue
		}
		tail, was, how := handoffTail(filepath.Join(newer, rel), filepath.Join(older, rel))
		switch how {
		case handoffUnread:
			return false, false, nil
		case handoffApart:
			return false, true, nil
		case handoffBehind:
			behind = true
		case handoffAhead:
			ahead = true
			grows = append(grows, grow{rel, tail, was})
		}
	}
	for rel := range theirs {
		if _, there := ours[rel]; !there && handoffLog(rel) {
			ahead = true // a log only the newer copy has
		}
	}
	switch {
	case ahead && behind:
		return false, true, nil
	case !ahead:
		return false, false, nil
	}
	for _, g := range grows {
		if _, err := handoffAppend(filepath.Join(older, g.rel), g.was, g.tail); err != nil {
			return false, false, err
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(theirs)) {
		_, there := ours[rel]
		if there && handoffLog(rel) {
			continue // appended to above
		}
		if err := handoffPut(filepath.Join(newer, rel), filepath.Join(older, rel), there); err != nil {
			return false, false, err
		}
	}
	return true, false, nil
}

// handoffCopy writes copied bytes. Tests stand in for it to fail half-way.
var handoffCopy = io.Copy

// handoffAppend adds tail to the end of the file at path, which must still be
// the file was describes: one written to since, by a CLI running on it, is left
// as it is. It stays the same file, with its owner and mode, so a CLI holding
// it open goes on writing to it, and a write cut short leaves it a longer
// start of the copy it was extended from, which the next resume finishes.
func handoffAppend(path string, was fs.FileInfo, tail []byte) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	now, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !os.SameFile(now, was) || now.Size() != was.Size() || !now.ModTime().Equal(was.ModTime()) {
		return false, nil
	}
	if _, err := handoffCopy(f, bytes.NewReader(tail)); err != nil {
		return false, err
	}
	if err := f.Sync(); err != nil {
		return false, err
	}
	return true, f.Close()
}

// handoffCreate copies a conversation the target does not hold to dst, a file
// or a folder of them.
func handoffCreate(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return handoffPut(src, dst, false)
	}
	files, ok := handoffFiles(src)
	if !ok {
		return fmt.Errorf("%s could not be read through", src)
	}
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		if err := handoffPut(filepath.Join(src, rel), filepath.Join(dst, rel), false); err != nil {
			return err
		}
	}
	return nil
}

// handoffPut writes src's bytes to dst, a log's to its last complete line,
// through a temporary file beside dst, so dst is never half written. A new
// file is linked into place, which never replaces one that has appeared
// there meanwhile. Only over, for a folder's file that is not a log, replaces
// what is there. Everything lands 0600/0700: these are homes that hold
// credentials, and the transcript inherits the same caution.
func handoffPut(src, dst string, over bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if handoffLog(dst) {
		if size, err = handoffComplete(in, size); err != nil || size == 0 {
			return err // nothing finished to copy yet
		}
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	handoffSweep(dir)
	// A name no CLI takes for a conversation: not a .jsonl, not a rollout.
	tmp, err := os.CreateTemp(dir, ".rota-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // once linked, only a second name
	defer tmp.Close()
	n, err := handoffCopy(tmp, io.LimitReader(in, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s shrank while it was copied", src)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if over {
		return os.Rename(tmp.Name(), dst)
	}
	if err := os.Link(tmp.Name(), dst); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// handoffComplete is how much of the first size bytes of f is complete
// records: up to and including its last newline, found from the end.
func handoffComplete(f *os.File, size int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for end := size; end > 0; {
		start := max(end-int64(len(buf)), 0)
		k, err := f.ReadAt(buf[:end-start], start)
		if k < int(end-start) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:k], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
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
