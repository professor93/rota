package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/professor93/rota/internal/owner"
)

// logFileMax is how large a server's log file may grow before it is moved
// aside to <name>.1, and logFileKeep how many of those older files are kept:
// four of twenty mebibytes, so a server left running for a year fills a
// hundred at most. logFileRetry is how long a log that had to go to stderr
// waits before the file is tried again, unless another logFileMax has gone
// there first. Variables so a test need not write twenty mebibytes or wait
// a minute.
var (
	logFileMax   int64 = 20 << 20
	logFileKeep        = 4
	logFileRetry       = time.Minute
)

// logFile is `rota serve --log-file`: everything the server says, appended
// to one file nobody but its owner can read or write, and moved aside before
// it can fill a disk.
//
// One mutex covers writing and rotating both, because the slog handler, the
// http server's own error log and the lines the command prints itself all
// write from goroutines of their own, and a rotation half done under one of
// them must not be written into by another.
type logFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	// limit is the size at which the next rotation is tried: logFileMax,
	// or after a rotation that could not move the file, another logFileMax
	// further on, so a directory that refuses renames is not asked again on
	// every line.
	limit int64
	// fallback is where the log goes while the file cannot be opened after
	// a rotation — stderr, rather than lines lost. lost says that is now,
	// and since when and how much has gone there, which is what decides
	// when the file is tried again.
	fallback  io.Writer
	lost      bool
	lostSince time.Time
	lostBytes int64
	closed    bool
}

// openLogFile opens a log file for appending, making it if it is not there.
// The path is already absolute: Config.Check insists on that. Its directory
// must exist — rota makes no directories for a log.
func openLogFile(path string, fallback io.Writer) (*logFile, error) {
	dir := filepath.Dir(path)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("server.log_file: %s is not an existing directory; make it first", dir)
	}
	l := &logFile{path: path, limit: logFileMax, fallback: fallback}
	l.foldAside()
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// open opens the file at l.path, the first time and after every rotation,
// append-only and made 0600. It never follows a link at the path, never
// waits on a FIFO there, and it checks what it actually opened rather than
// what was there a moment before: a plain file, and on unix nobody's but
// its owner's. Otherwise somebody who can write the directory could point
// the log at a file of the owner's — a credential — and have it appended
// to, or stop the server dead on a pipe nobody reads.
func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|owner.OpenFlags, 0o600)
	if err != nil {
		if fi, lerr := os.Lstat(l.path); lerr == nil {
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				return fmt.Errorf("server.log_file: %s is a link; rota writes its log only to a plain file", l.path)
			case !fi.Mode().IsRegular():
				return fmt.Errorf("server.log_file: %s is not a plain file", l.path)
			}
		}
		return fmt.Errorf("server.log_file: %w", err)
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a plain file", l.path)
	}
	if err == nil {
		err = owner.Only(l.path, fi.Mode())
	}
	if err == nil {
		err = owner.Blocking(f)
	}
	if err != nil {
		f.Close()
		return fmt.Errorf("server.log_file: %w", err)
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// Write appends p whole, rotating first when p would take the file past its
// limit. A file with nothing in it takes any p, so one line longer than the
// limit is written rather than lost.
//
// When the file cannot be opened after a rotation, the log goes to stderr,
// and the file is tried again after logFileRetry or once another
// logFileMax has gone to stderr, whichever is first. Each switch, one way
// or the other, is said once on stderr.
func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, os.ErrClosed
	}
	if l.lost && (l.lostBytes >= logFileMax || time.Since(l.lostSince) >= logFileRetry) {
		if err := l.open(); err == nil {
			l.lost = false
			fmt.Fprintf(l.fallback, "rota: the log goes to %s again\n", oneLine(l.path))
		} else {
			l.lostSince, l.lostBytes = time.Now(), 0
		}
	}
	if !l.lost && l.f != nil && l.size > 0 && l.size+int64(len(p)) > l.limit {
		l.rotate()
	}
	if !l.lost && l.f == nil {
		if err := l.open(); err != nil {
			l.lost, l.lostSince, l.lostBytes = true, time.Now(), 0
			fmt.Fprintf(l.fallback, "rota: the log goes to stderr until the file can be opened again: %s\n", printable(err.Error()))
		}
	}
	if l.lost {
		n, err := l.fallback.Write(p)
		l.lostBytes += int64(n)
		return n, err
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// onFile reports whether the log is going to the file right now rather
// than to stderr.
func (l *logFile) onFile() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.lost && !l.closed
}

// rotate moves the file aside first and only then makes room for it: if the
// file itself cannot be moved, no older file has been shifted or dropped,
// and the next write goes on appending to it until another logFileMax has
// been written. Once it is aside, it takes its place in the chain as <name>.1.
// The next write opens a new file.
func (l *logFile) rotate() {
	l.f.Close()
	l.f = nil
	// A <name>.0 left by a rotation that never finished goes into the chain
	// before this one takes its name, so it is not written over.
	if !l.foldAside() {
		l.limit = l.size + logFileMax
		return
	}
	if err := os.Rename(l.path, l.old(0)); err != nil {
		l.limit = l.size + logFileMax
		return
	}
	l.foldAside()
	l.limit = logFileMax
}

// old is the name of the i-th older file; <name>.0 is the one being moved.
func (l *logFile) old(i int) string { return fmt.Sprintf("%s.%d", l.path, i) }

// foldAside moves a <name>.0 — the log in the middle of a rotation, or one a
// crash left there — into the chain: the oldest goes, <name>.1..<name>.N-1
// move up by one, and <name>.0 becomes <name>.1. It reports whether the name
// is free afterwards. Something at <name>.0 that is not a plain file is not
// a log rota left, and is left alone; a rename onto it is what fails then.
//
// <name>.0 is first moved to <name>.folding, a name of the fold's own, and
// only once that has worked is anything older shifted or dropped. A .0 that
// cannot be moved — in a shared directory with the sticky bit, one another
// user put there — so costs nothing: every old file stays. A .folding that
// an interrupted fold left is folded first, as the older of the two, but
// only when it is provably this user's: only a file's owner may change its
// mode, so a chmod that works says the rename after the shift will too. One
// that is somebody else's is never shifted around; renaming .0 onto it is
// refused, and then nothing is.
func (l *logFile) foldAside() bool {
	plain := func(name string) bool {
		fi, err := os.Lstat(name)
		return err == nil && fi.Mode().IsRegular()
	}
	folding := l.path + ".folding"
	for {
		if !ownPlainFile(folding) {
			if !plain(l.old(0)) {
				return true
			}
			if os.Rename(l.old(0), folding) != nil {
				return false
			}
		}
		_ = os.Remove(l.old(logFileKeep))
		for i := logFileKeep - 1; i >= 1; i-- {
			_ = os.Rename(l.old(i), l.old(i+1))
		}
		if os.Rename(folding, l.old(1)) != nil {
			return false
		}
	}
}

// ownPlainFile reports whether name is a plain file this user owns, asked
// of the file itself rather than of the name: it is opened without
// following a link or waiting on a FIFO, the handle must be a plain file,
// and the chmod that proves ownership goes through the handle. A check of
// the name and then a chmod by name would let somebody swap in a link
// between the two and have the chmod land on a file of this user's.
func ownPlainFile(name string) bool {
	f, err := os.OpenFile(name, os.O_RDONLY|owner.OpenFlags, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	return err == nil && fi.Mode().IsRegular() && f.Chmod(0o600) == nil
}

// Close closes the file for good: something still logging after the server
// has stopped is refused rather than reopening it.
func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
