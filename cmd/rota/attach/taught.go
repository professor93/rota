package attach

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sync"
)

// TaughtFile is an Options.Teach that remembers, in the file taught in the
// daemon's terminals folder, who has been shown the chord: one name per
// line, the file only its owner's to read, each name appended whole. It
// says true the first time it is asked about a name, and records the name,
// so every later call, in this process or another, says false. It says
// false when the file cannot be written, since a card that would show at
// every attach is worse than none, and for a person with no name, who
// cannot be told apart from anybody else.
func TaughtFile(terminals string) func(person string) bool {
	path := filepath.Join(terminals, "taught")
	var mu sync.Mutex
	return func(person string) bool {
		name := OneLine(person)
		if name == "" {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if taughtHas(path, name) {
			return false
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return false
		}
		// A file made by somebody else's umask, or by hand, is made the
		// owner's alone before a name goes in it.
		_ = f.Chmod(0o600)
		_, err = f.Write([]byte(name + "\n"))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err == nil
	}
}

// taughtHas says whether the file at path holds name as one of its lines.
// A file that cannot be read holds nobody.
func taughtHas(path, name string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, len(b)+1)
	for sc.Scan() {
		if sc.Text() == name {
			return true
		}
	}
	return false
}
