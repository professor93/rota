package fakecli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// vendors are the CLIs no test ever starts for real: every one a provider
// rota builds in launches, which a test in lib holds this list to, and
// every one the session listing recognises, which a test in sessions does.
var vendors = []string{"claude", "codex", "grok", "kimi"}

// Vendors is the names of the CLIs no test ever starts for real.
func Vendors() []string { return slices.Clone(vendors) }

// scrubbed says scrubPath has run in this process: Maybe found it was not
// a fake.
var scrubbed bool

// scrubPath leaves out of PATH every directory that holds a vendor CLI,
// so that a test which forgot to install a fake finds none at all,
// rather than the real one somebody installed and is signed in to. PATH
// names directories, not files, so a whole directory goes, with whatever
// else is in it; the rest stay where they were, so git and sh are still
// found. A test that needs a vendor CLI installs a fake and puts its
// directory on PATH itself, after TestMain.
//
// The one vendor CLI that stays is this very binary, a fake. A process a
// test starts from it — a holder, a daemon, rota run as a child — runs
// TestMain too, with the test's fakes already on PATH, and needs them
// there.
func scrubPath() {
	scrubbed = true
	path, ok := os.LookupEnv("PATH")
	if !ok {
		return
	}
	var self os.FileInfo
	if exe, err := os.Executable(); err == nil {
		self, _ = os.Stat(exe)
	}
	var kept []string
	for _, dir := range filepath.SplitList(path) {
		if !holdsVendor(dir, self) {
			kept = append(kept, dir)
		}
	}
	if now := joinList(kept); now != path {
		os.Setenv("PATH", now)
	}
}

// holdsVendor says whether dir holds a vendor CLI that is not this binary,
// self, under any name exec.LookPath would start it by.
func holdsVendor(dir string, self os.FileInfo) bool {
	for _, name := range vendors {
		for _, file := range names(name) {
			path := filepath.Join(dir, file)
			fi, err := os.Stat(path)
			if err != nil || fi.IsDir() || !executable(fi) {
				continue
			}
			if !isSelf(path, fi, self) {
				return true
			}
		}
	}
	return false
}

// names are the files a CLI called name can be started from. On Windows
// that is name with each extension PATHEXT lists, and the ones it lists by
// default, whatever it says; name itself as well, which npm puts beside
// its shims for a shell to run.
func names(name string) []string {
	if runtime.GOOS != "windows" {
		return []string{name}
	}
	out := []string{name}
	exts := []string{".com", ".exe", ".bat", ".cmd"}
	for _, ext := range strings.Split(os.Getenv("PATHEXT"), ";") {
		if ext = strings.ToLower(strings.TrimSpace(ext)); strings.HasPrefix(ext, ".") {
			exts = append(exts, ext)
		}
	}
	seen := map[string]bool{}
	for _, ext := range exts {
		if !seen[ext] {
			seen[ext] = true
			out = append(out, name+ext)
		}
	}
	return out
}

// executable says whether a file could be run: on unix, any execute bit,
// which is more than exec.LookPath asks for; on Windows, any file.
func executable(fi os.FileInfo) bool {
	return runtime.GOOS == "windows" || fi.Mode()&0o111 != 0
}

// isSelf says whether the file at path, fi, is this binary: the same file,
// as a link Link made or a symbolic link is, or a copy Link made where it
// could not link, with its spec beside it and this binary's size. A spec
// beside a CLI that is not this binary leaves it what it is.
func isSelf(path string, fi, self os.FileInfo) bool {
	if self == nil {
		return false
	}
	if os.SameFile(fi, self) {
		return true
	}
	_, err := os.Stat(specPath(path))
	return err == nil && fi.Size() == self.Size()
}

// joinList is filepath.SplitList undone. On Windows a directory with the
// separator in its name goes back in the quotes that kept it whole.
func joinList(dirs []string) string {
	sep := string(os.PathListSeparator)
	if runtime.GOOS == "windows" {
		for i, dir := range dirs {
			if strings.Contains(dir, sep) {
				dirs[i] = `"` + dir + `"`
			}
		}
	}
	return strings.Join(dirs, sep)
}

// NoneOnPath fails t unless Maybe left every real vendor CLI out of PATH in
// this process and exec.LookPath finds none of them on it now.
// Every package whose TestMain calls Maybe has a test that calls this
// before installing anything, so a TestMain that forgot Maybe fails on
// every machine, not only on one with a real CLI installed.
func NoneOnPath(t testing.TB) {
	t.Helper()
	if !scrubbed {
		t.Fatal("TestMain did not call fakecli.Maybe first: PATH is still whatever the person running the tests has")
		return
	}
	for _, name := range vendors {
		if path, err := exec.LookPath(name); err == nil {
			t.Errorf("a test here would start %s from %s", name, path)
		}
	}
}
