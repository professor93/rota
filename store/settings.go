package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	rota "github.com/professor93/rota/lib"
)

// Claude Code reads settings files of its own every time it starts, and it
// writes the `env` block of each one into its environment over whatever it
// inherited (code.claude.com/docs/en/env-vars, "Precedence"). So every
// variable a claude launch drops from what it inherits — those that would
// send the account's login to another host, sign Claude Code in some other
// way, or move it to another directory's login — comes straight back from a
// settings file that sets it. `apiKeyHelper` does the same from the top
// level: it is a command whose output Claude Code sends as the credential,
// ranked above the account's own login (code.claude.com/docs/en/
// authentication, "Authentication precedence"). The person's settings.json
// reaches every account through its mirror; a project's comes with the
// directory a run starts in; the organisation's is on the machine.
//
// rota edits none of these files and refuses no run for them: they are the
// person's, a project's or an organisation's, and what they say may be
// meant. It says so instead, in one warning per launch that names each file
// and the keys it sets, and never a value.

// What --setting-sources calls the files a person and a project keep. The
// machine's managed settings are read whatever it says.
const (
	sourceUser    = "user"
	sourceProject = "project"
	sourceLocal   = "local"
)

// maxSettingsFile bounds what is read of one settings file: 2 MiB, the most
// Claude Code reads of one — it refuses a larger file whole — so what rota
// passes over is what Claude Code does not apply.
const maxSettingsFile = 2 << 20

// managedSettingsDir is the directory Claude Code reads the machine's managed
// settings from: managed-settings.json, and the drop-ins in
// managed-settings.d beside it (code.claude.com/docs/en/managed-settings).
// Empty in a test binary, where an administrator's file would make a test
// depend on the machine it runs on; a test that wants one sets it.
var managedSettingsDir = defaultManagedSettingsDir()

func defaultManagedSettingsDir() string {
	if testing.Testing() {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "linux":
		return "/etc/claude-code"
	case "windows":
		return `C:\Program Files\ClaudeCode`
	}
	return ""
}

// settingsEnvKeys are the variables a settings file's env block is named
// for: every one a claude launch drops from what it inherits, except
// CLAUDECODE, which says what a process runs inside rather than how it signs
// in; and CLAUDE_CONFIG_DIR, which rota sets itself and which, set by a
// settings file, moves Claude Code to another directory and the login kept
// there.
func settingsEnvKeys() []string {
	keys := slices.DeleteFunc(rota.ClaudeCredentialVars(), func(k string) bool { return k == "CLAUDECODE" })
	return append(keys, "CLAUDE_CONFIG_DIR")
}

// sayRunSettings is sayRiskySettings for a run: the directory, the sources
// and the hermetic flag are the spec's, and a run that names no directory
// starts in this process's. The active Anthropic profile is said after it.
func (s *Store) sayRunSettings(a *rota.Account, cmd *rota.Command, spec rota.Spec) {
	cwd, err := filepath.Abs(spec.Cwd)
	if err != nil {
		cwd = ""
	}
	env := rota.Environ(cmd.BaseEnv, cmd)
	s.sayRiskySettings(a, env, cwd, spec.SettingSources, spec.Hermetic)
	s.sayFederationProfile(a, env)
}

// sayRiskySettings warns, once, about every settings file this launch of a's
// Claude Code will read that sets apiKeyHelper or one of settingsEnvKeys in
// its env block. env is the child's environment, which says the
// configuration directory it runs in; cwd the directory it starts in;
// sources the run's setting_sources, nil for all of them; and hermetic that
// the run gets a fresh configuration directory with no settings in it.
//
// The warning says two things apart. A credential or a host is a risk to
// the account and is said as one. A proxy or a certificate in the person's
// own settings or the organisation's managed ones is what a corporate
// network sets on purpose — Claude Code's own docs put them there — so it is
// said as what it is: rota drops them too, and the person should know they
// are back, without being told the login is leaving. In a project's files,
// which come with whatever repository is checked out, a proxy and a
// certificate are how that repository could intercept the token, and they
// are said as the risk with the rest.
func (s *Store) sayRiskySettings(a *rota.Account, env []string, cwd string, sources []string, hermetic bool) {
	if s.Warn == nil || rota.Flavor(a.Provider) != "claude" {
		return
	}
	reads := func(source string) bool { return sources == nil || slices.Contains(sources, source) }
	// own marks the files that are the person's or the organisation's
	// rather than a project's.
	type file struct {
		path string
		own  bool
	}
	var files []file
	if dir := configDirOf(env); dir != "" && reads(sourceUser) && !hermetic {
		files = append(files, file{filepath.Join(dir, "settings.json"), true})
	}
	if cwd != "" && reads(sourceProject) {
		files = append(files, file{filepath.Join(cwd, ".claude", "settings.json"), false})
	}
	if cwd != "" && reads(sourceLocal) {
		// Claude Code keeps the local file at the repository's root when it
		// starts below it, and beside the shared one otherwise; which of the
		// two depends on rules of its own (ownership, the home directory,
		// the platform), so both are read.
		files = append(files, file{filepath.Join(cwd, ".claude", "settings.local.json"), false})
		if root := repoRoot(cwd); root != "" {
			files = append(files, file{filepath.Join(root, ".claude", "settings.local.json"), false})
		}
	}
	for _, m := range managedSettingsFiles() {
		files = append(files, file{m, true})
	}
	network := map[string]bool{}
	for _, k := range rota.NetworkRedirecting() {
		network["env."+k] = true
	}
	var risks, routes []string
	seen := map[string]bool{}
	for _, sf := range files {
		// The file as it is, links resolved: through the mirror that is the
		// person's own, which is the one to open. One that is not there is
		// nothing to read. A file reached twice keeps its first source, and
		// the person's own comes first.
		f, err := filepath.EvalSymlinks(sf.path)
		if err != nil || seen[f] {
			continue
		}
		seen[f] = true
		keys := riskyKeys(f)
		routing := func(k string) bool { return sf.own && network[k] }
		if risk := slices.DeleteFunc(slices.Clone(keys), routing); len(risk) > 0 {
			risks = append(risks, f+" sets "+strings.Join(risk, ", "))
		}
		if route := slices.DeleteFunc(keys, func(k string) bool { return !routing(k) }); len(route) > 0 {
			routes = append(routes, f+" sets "+strings.Join(route, ", "))
		}
	}
	var said []string
	if len(risks) > 0 {
		said = append(said, "settings Claude Code reads put back what rota keeps out of this account's environment, "+
			"so its login may go to another host or another credential be used in its place: "+strings.Join(risks, "; "))
	}
	if len(routes) > 0 {
		said = append(said, "Claude Code will use the proxy or the certificates its settings name, which rota otherwise "+
			"keeps out of an account's environment; nothing is wrong if they are your network's: "+strings.Join(routes, "; "))
	}
	if len(said) > 0 {
		s.Warn(fmt.Sprintf("%s: %s", a, strings.Join(said, ". ")))
	}
}

// configDirOf is the configuration directory a child with this environment
// runs Claude Code in, or "" when there is none to name: no
// CLAUDE_CONFIG_DIR and no home directory. "" is never a directory to read
// from — joined to a name it is the process's own directory.
func configDirOf(env []string) string {
	dir := ""
	for _, e := range env {
		if k, v, _ := strings.Cut(e, "="); k == "CLAUDE_CONFIG_DIR" {
			dir = v
		}
	}
	if dir == "" {
		return defaultClaudeDir()
	}
	return dir
}

// managedSettingsFiles are the machine's managed settings files, in the
// order Claude Code merges them: managed-settings.json, then every *.json
// drop-in that is not hidden, by name.
func managedSettingsFiles() []string {
	if managedSettingsDir == "" {
		return nil
	}
	files := []string{filepath.Join(managedSettingsDir, "managed-settings.json")}
	drop := filepath.Join(managedSettingsDir, "managed-settings.d")
	entries, _ := os.ReadDir(drop) // sorted by name; none is nothing to add
	for _, e := range entries {
		if n := e.Name(); !strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".json") {
			files = append(files, filepath.Join(drop, n))
		}
	}
	return files
}

// riskyKeys are the keys of one settings file that would give Claude Code
// another credential or another host, sorted: "apiKeyHelper", and
// "env.NAME" for each of settingsEnvKeys. A file that cannot be read as one
// JSON object has none, as Claude Code passes over such a file too.
func riskyKeys(path string) []string {
	raw, ok := readSmallFile(path, maxSettingsFile)
	if !ok {
		return nil
	}
	var doc map[string]jsontext.Value
	if rota.UnmarshalLenient(settingsText(raw), &doc) != nil {
		return nil
	}
	var keys []string
	if setting(doc["apiKeyHelper"]) {
		keys = append(keys, "apiKeyHelper")
	}
	var env map[string]jsontext.Value
	if raw := doc["env"]; raw != nil && rota.UnmarshalLenient(raw, &env) == nil {
		for _, k := range settingsEnvKeys() {
			if setting(env[k]) {
				keys = append(keys, "env."+k)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// settingsText is a settings file's bytes as the JSON text Claude Code
// parses: a file that starts FF FE is UTF-16 little-endian, any other is
// UTF-8, and one byte-order mark at the start of the text is dropped
// (Claude Code 2.1.319 decodes a file so, and strips U+FEFF before
// JSON.parse). A big-endian file is not decoded, as Claude Code does not
// decode one; a last odd byte of a little-endian one is dropped, as Node's
// decoder drops it.
func settingsText(raw []byte) []byte {
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		units := make([]uint16, len(raw)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(raw[2*i:])
		}
		raw = []byte(string(utf16.Decode(units)))
	}
	return bytes.TrimPrefix(raw, []byte("\uFEFF"))
}

// setting reports whether a settings value sets anything. "" is how a
// settings file cancels an inherited variable, and null is no value.
func setting(v jsontext.Value) bool {
	switch string(bytes.TrimSpace(v)) {
	case "", `""`, "null":
		return false
	}
	return true
}

// repoRoot is the root of the git repository dir is in — for a worktree,
// the main checkout's, which is where Claude Code keeps a worktree's local
// settings (code.claude.com/docs/en/settings) — or "" outside one.
func repoRoot(dir string) string {
	for {
		fi, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil {
			if !fi.IsDir() {
				if main := mainCheckout(dir); main != "" {
					return main
				}
			}
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			return ""
		}
		dir = up
	}
}

// mainCheckout is the main checkout's root for the worktree at dir: its .git
// file names the worktree's git directory, and the commondir file there the
// main checkout's .git. "" when either is not what git writes.
func mainCheckout(dir string) string {
	raw, ok := readSmallFile(filepath.Join(dir, ".git"), 4096)
	gitdir, found := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok || !found {
		return ""
	}
	gitdir = resolveFrom(dir, strings.TrimSpace(gitdir))
	common, ok := readSmallFile(filepath.Join(gitdir, "commondir"), 4096)
	if !ok {
		return ""
	}
	return filepath.Dir(resolveFrom(gitdir, strings.TrimSpace(string(common))))
}

// resolveFrom is path as git means it: relative to dir unless absolute.
func resolveFrom(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

// readSmallFile reads a regular file of at most limit bytes, and nothing
// else: a FIFO would block the launch on its open, and a device or a huge
// file is not a settings file. The shape is checked before the open and again
// on what was opened.
func readSmallFile(path string, limit int64) ([]byte, bool) {
	small := func(fi os.FileInfo) bool { return fi.Mode().IsRegular() && fi.Size() <= limit }
	if fi, err := os.Stat(path); err != nil || !small(fi) {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !small(fi) {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, false
	}
	return raw, true
}
