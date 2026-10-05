package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	rota "github.com/professor93/rota/lib"
)

// conversationState is every entry of a Claude Code directory that a
// conversation at rest is keyed by or derived from: the transcripts
// themselves, the file edits, todos and tasks filed under a session id, the
// environment a session left behind, the pastes and shell snapshots it
// referred to, and the prompt history. Moving conversations means moving all
// of it — transcripts alone would resume into a session whose edits and
// todos were somewhere else.
//
// Everything outside this list is settings, memory, skills and plugins, and
// follows the general rule instead — or is live state of one account's own
// Claude Code, which is never shared in any mode. `sessions/` is the registry
// of live processes and the keys that open them; `jobs/` and `teams/` are
// what a daemon is running right now — the background sessions an agent view
// lists, resumes, replies to and deletes, and the agent teams it
// orchestrates. Shared, one account's agent view would offer another
// account's background work to resume and delete, on the wrong account's
// quota. See sharedWithClaude.
var conversationState = []string{
	"projects", "file-history", "todos", "session-env", "tasks",
	"paste-cache", "shell-snapshots", conversationHistory,
}

// conversationHistory is the one entry of conversationState that is a file
// rather than a directory.
const conversationHistory = "history.jsonl"

// liveState are the entries that were once arranged with the conversations
// and are now each account's own, because they are a running daemon's state
// rather than conversations at rest. A link rota made for one of them into a
// conversation folder is pruned once the home is quiet.
var liveState = []string{"jobs", "teams"}

// keepsConversations reports whether an entry is one of those.
func keepsConversations(name string) bool { return slices.Contains(conversationState, name) }

// mirrorClaude builds or refreshes an account's own view of the person's
// Claude Code configuration, and returns the directory to run it in. An
// account this does not apply to gets an empty directory and no error.
//
// It exists because a configuration directory is what Claude Code keeps an
// account's running state in. It hosts background sessions, the agent view,
// attached sessions and the "N ⧉" sub-sessions in a helper process — one
// daemon per configuration directory — and the login it signs them in with
// is the one kept in that directory. A directory of its own gives an account
// a daemon of its own and a login of its own, and nothing of one account's
// running work is visible from another's.
//
// A directory of its own would otherwise mean an empty world: no settings,
// no memory, no skills, no plugins, no trusted folders, and none of the
// conversations there are to resume. So it is a mirror rather than a copy —
// one symlink per entry of the person's own directory, refreshed on every
// launch. Claude Code writes through the links, so the two remain the same
// files and nothing has to be merged back. What is left out is what must not
// be shared: see sharedWithClaude.
//
// .claude.json is the person's own, through a link like the rest, unless the
// account has Remote Control on: then it is the account's own copy, because
// Remote Control reads the identity it acts as out of that file. See
// ownConfig.
//
// Where the conversations themselves go is a setting of the account's:
// shared through the links by default, kept in the account's own home when
// it says `own`, and in a directory it names when it names one. The refresh
// converges on whatever the settings say now, so changing one re-points the
// links on the next launch — except that a link is never taken away from
// under a running daemon because its name stopped being shared: while
// anything is alive in the home (quiet is false) those stay exactly as they
// are, until the first launch that finds the home quiet.
func (s *Store) mirrorClaude(a *rota.Account, quiet bool) (string, error) {
	if a.Provider != "claude" {
		return "", nil
	}
	if a.ConfigDir != "" {
		// An account told where its configuration lives was given a world of
		// its own deliberately, and lib already points the CLI at it: a
		// mirror would be a second CLAUDE_CONFIG_DIR and a second answer.
		// What is still arranged there is a conversation directory the
		// account was told to keep its transcripts in, and — with Remote
		// Control on — who the account is, when that directory does not say.
		if err := s.placeConversations(a, a.ConfigDir, quiet); err != nil {
			return a.ConfigDir, err
		}
		if a.RemoteControl && quiet && !s.personalClaude(a) {
			if err := claimIdentity(a, filepath.Join(a.ConfigDir, claudeConfigFile)); err != nil {
				return a.ConfigDir, err
			}
		}
		return a.ConfigDir, nil
	}
	src, srcJSON := claudeConfigSource()
	dst := s.ownHome(a)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return dst, err
	}
	// want is the target every name in the mirror should be a link to.
	// Nothing else may be there, which is what makes a change of setting
	// take effect: a link the mirror no longer wants is one it removes.
	want := map[string]string{}
	// from is the world each name comes from, which is what a person is told
	// to look in when the link could not be made. It cannot be read back off
	// the targets: .claude.json lives beside the directory rather than in it.
	from := map[string]string{}
	// Nothing to mirror is not a failure: Claude Code makes itself a fresh
	// world in the directory, which is what it would have done in the
	// missing one. A source that is there but is not a directory is another
	// matter — CLAUDE_CONFIG_DIR naming a file — and is said so on every
	// platform: Windows reports reading a file as a directory as "not
	// found", which would otherwise pass for the harmless case.
	info, err := os.Stat(src)
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return dst, err
	case err == nil && !info.IsDir():
		return dst, fmt.Errorf("%s is not a directory", src)
	case err == nil:
		entries, err := os.ReadDir(src)
		if err != nil {
			return dst, err
		}
		// Conversations come from the person's directory only while they are
		// shared. An account keeping its own has no link to them at all, and
		// Claude Code makes the entries it needs inside the mirror itself.
		shared := a.Sessions == ""
		for _, e := range entries {
			name := e.Name()
			if !sharedWithClaude(name) || (!shared && keepsConversations(name)) || (a.RemoteControl && name == configBackups) {
				continue
			}
			want[name] = filepath.Join(src, name)
			from[name] = src
		}
	}
	// .claude.json is the one file that does not live inside the directory
	// unless CLAUDE_CONFIG_DIR says so, and it is the one holding trust
	// decisions, MCP servers, project history and the identity the CLI
	// shows. It is the person's, through a link, unless the account's own
	// copy is in place.
	own, err := s.ownConfig(a, dst, srcJSON, quiet)
	if err != nil {
		return dst, err
	}
	if _, err := os.Stat(srcJSON); err == nil && !own {
		want[claudeConfigFile] = srcJSON
		from[claudeConfigFile] = src
	}
	if dir := a.SessionsDir(); dir != "" {
		if err := makeConversationState(dir); err != nil {
			return dst, err
		}
		for _, name := range conversationState {
			want[name] = filepath.Join(dir, name)
			from[name] = dir
		}
	}
	// While anything is alive here a link is not taken away because its name
	// stopped being shared: the daemon has that directory open through it.
	// Conversation links still follow the sessions setting as they always
	// have.
	hold := func(string) bool { return false }
	if !quiet {
		hold = func(name string) bool { return !keepsConversations(name) }
	}
	blocked, err := relink(dst, want, func(string) bool { return true }, hold)
	if err != nil {
		return dst, err
	}
	if a.RemoteControl {
		// With Remote Control on, a real .claude.json is what the account is
		// meant to have — in place, or about to be once the home is quiet.
		blocked = slices.DeleteFunc(blocked, func(n string) bool { return n == claudeConfigFile })
	}
	s.sayOwnEntriesStay(a, dst, blocked, from)
	return dst, nil
}

// placeConversations arranges only the conversation entries of a directory
// the person chose, for an account that names both a configuration
// directory and somewhere else to keep its transcripts. Nothing else in
// there is rota's to touch: it is the account's own world, not a mirror —
// except a link rota itself once made there for jobs/ or teams/, which are a
// running daemon's own state and must not be shared through a conversation
// folder. Those go when the home is quiet, whatever the setting says now.
func (s *Store) placeConversations(a *rota.Account, home string, quiet bool) error {
	if quiet {
		if err := pruneLiveStateLinks(home); err != nil {
			return err
		}
	}
	dir := a.SessionsDir()
	if dir == "" {
		return nil
	}
	if err := makeConversationState(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	want := make(map[string]string, len(conversationState))
	from := make(map[string]string, len(conversationState))
	for _, name := range conversationState {
		want[name] = filepath.Join(dir, name)
		from[name] = dir
	}
	blocked, err := relink(home, want, keepsConversations, func(string) bool { return false })
	if err != nil {
		return err
	}
	s.sayOwnEntriesStay(a, home, blocked, from)
	return nil
}

// pruneLiveStateLinks removes the links to a conversation folder's jobs/ and
// teams/ that rota made in a directory the person chose, when those were
// still arranged with the conversations. Only links of that shape go — a
// link named for the entry it points at — so anything the person made there
// themselves is left alone.
func pruneLiveStateLinks(home string) error {
	for _, name := range liveState {
		p := filepath.Join(home, name)
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		if target, err := os.Readlink(p); err == nil && filepath.Base(target) == name {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// sayOwnEntriesStay reports the entries an account already has of its own
// where a link was meant to go. They are left exactly as they are: an entry
// rota did not make is the account's, and replacing one would be losing
// somebody's work. The person is told which they are, so they can move them
// aside and mean it.
//
// Two rather different things end up here, which is why this reports every
// name in the way rather than the conversations alone. One is conversations
// an account made while it was keeping them to itself and the setting has
// since asked to be shared. The other is quieter: Claude Code writes some
// files by writing a temporary one and renaming it over the path, and a
// rename replaces rota's link with a real file. That entry then stops
// tracking the person's copy altogether — the account reads and writes a
// fork of its own settings.json, say — and nothing else would ever say so.
// The files seen so far survive the trip, but there are sixty-odd entries in
// a Claude Code directory and Claude Code changes.
//
// from names the directory each entry would have been linked into, so the
// person is told where to look rather than only what is in the way.
func (s *Store) sayOwnEntriesStay(a *rota.Account, dir string, blocked []string, from map[string]string) {
	if len(blocked) == 0 || s.Warn == nil {
		return
	}
	var worlds []string
	for _, name := range blocked {
		if w := from[name]; w != "" && !slices.Contains(worlds, w) {
			worlds = append(worlds, w)
		}
	}
	slices.Sort(worlds)
	s.Warn(fmt.Sprintf("%s holds its own %s in %s, where a link to %s is expected; "+
		"the account reads these and not yours. Move them aside to share again.",
		a, strings.Join(blocked, ", "), dir, strings.Join(worlds, " and ")))
}

// relink makes the links in dir say what want says, and returns the names it
// could not because a real entry of the account's own is in the way.
//
// Only names governs allows are considered, so a directory rota merely keeps
// a corner of is not swept. A link there is always rota's own work and
// answers to want: one pointing somewhere want does not say goes or is
// re-pointed, one pointing at nothing goes, and one carrying a name want no
// longer holds at all goes — unless hold says to keep that one as it is for
// now. A real entry is the account's and is never removed or replaced.
func relink(dir string, want map[string]string, governs, hold func(string) bool) ([]string, error) {
	have, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	mine := make(map[string]fs.DirEntry, len(have))
	for _, e := range have {
		mine[e.Name()] = e
	}
	for name, e := range mine {
		if e.Type()&fs.ModeSymlink == 0 || !governs(name) {
			continue
		}
		p := filepath.Join(dir, name)
		// An entry that went away since leaves a link to nothing, which
		// Claude Code reads as a broken file rather than as an absent one.
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(p)
			delete(mine, name)
			continue
		}
		target, wanted := want[name]
		if !wanted && hold(name) {
			continue
		}
		if got, err := os.Readlink(p); !wanted || err != nil || got != target {
			_ = os.Remove(p)
			delete(mine, name)
		}
	}
	var blocked []string
	for name, target := range want {
		if !governs(name) {
			continue
		}
		if e, ok := mine[name]; ok {
			if e.Type()&fs.ModeSymlink == 0 {
				blocked = append(blocked, name)
			}
			continue
		}
		err := os.Symlink(target, filepath.Join(dir, name))
		if err != nil && !errors.Is(err, fs.ErrExist) {
			// fs.ErrExist alone is forgiven: another run of the same account
			// got there between the listing and now, and wrote the same link.
			return blocked, err
		}
	}
	slices.Sort(blocked)
	return blocked, nil
}

// makeConversationState makes sure a directory an account was told to keep
// its conversations in is ready to be linked to.
//
// The entries have to exist first. A link to nothing is pruned on the next
// refresh, and Claude Code cannot create a directory through one either: it
// would file the first conversation of a fresh folder nowhere.
func makeConversationState(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range conversationState {
		if name == conversationHistory {
			continue
		}
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			return err
		}
	}
	// O_CREATE without O_TRUNC: a history that is already there is somebody's
	// prompts, and this runs on every launch.
	f, err := os.OpenFile(filepath.Join(dir, conversationHistory), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// sharedWithClaude reports whether one entry of the person's Claude Code
// directory may be shared with an account.
//
// What is shared is the person's world: settings, memory, skills, plugins,
// and the conversations at rest. What is not is anything that belongs to one
// login or to one running Claude Code, because sharing it would mix two
// accounts — let one see, act on or sign in as the other:
//   - every file the daemon keeps is named for it — the lock, the log, the
//     status, the auth cooldown — so the prefix is the rule rather than a
//     list that would go stale the next time Claude Code invents one;
//   - `.credentials.json` is the login itself, and the person's is theirs;
//   - `sessions/` is the registry of live session processes and the keys
//     that open them, which is how one account would reach another's daemon;
//   - `jobs/` and `teams/` are what a daemon is running: the background
//     sessions an agent view lists and offers to resume, reply to and
//     delete, and the agent teams it orchestrates;
//   - `bridge-spawn` is Remote Control's own state for this login;
//   - `policy-limits.json`, `statsig`, `mcp-needs-auth-cache.json` and
//     `telemetry` are caches and spools kept per login: limits the
//     provider set for this account, its feature flags, which MCP servers
//     still want it to sign in, what it is waiting to report;
//   - any name ending in `.lock` is a lock Claude Code takes for this
//     directory's own processes;
//   - `.claude.json` is dealt with on its own, `.claude.json.own` and
//     `backups.own` are rota's names for an account's own copies put aside,
//     and `.DS_Store` is noise a file manager leaves behind.
//
// `backups` — Claude Code's copies of .claude.json, which it restores from —
// follows .claude.json and is decided where that is: shared while the file
// is, the account's own while the account keeps its own file.
func sharedWithClaude(name string) bool {
	switch {
	case strings.HasPrefix(name, "daemon"), strings.HasSuffix(name, ".lock"):
		return false
	}
	switch name {
	case claudeCredentials, claudeConfigFile, claudeConfigAside, configBackupsAside, ".DS_Store",
		"sessions", "jobs", "teams", "bridge-spawn",
		"policy-limits.json", "statsig", "mcp-needs-auth-cache.json", "telemetry":
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
