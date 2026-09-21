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
// conversation is keyed by or derived from: the transcripts themselves, the
// file edits, todos and tasks filed under a session id, the environment and
// jobs a session left behind, the pastes and shell snapshots it referred to,
// and the prompt history. Moving conversations means moving all of it —
// transcripts alone would resume into a session whose edits and todos were
// somewhere else.
//
// Everything outside this list is settings, memory, skills and plugins, and
// follows the general rule instead. Claude Code's own `sessions/` is not
// here and never will be: it is the registry of live processes and their
// socket keys, not conversations, and it stays each account's own in every
// mode. See sharedWithClaude.
var conversationState = []string{
	"projects", "file-history", "todos", "session-env", "tasks",
	"jobs", "teams", "paste-cache", "shell-snapshots", conversationHistory,
}

// conversationHistory is the one entry of conversationState that is a file
// rather than a directory.
const conversationHistory = "history.jsonl"

// keepsConversations reports whether an entry is one of those.
func keepsConversations(name string) bool { return slices.Contains(conversationState, name) }

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
//
// Where the conversations themselves go is a setting of the account's, and
// the only part of this that is: shared through the links by default, kept
// in the account's own home when it says `own`, and in a directory it names
// when it names one. The refresh converges on whatever the setting says
// now, so changing it re-points the links on the next launch.
func (s *Store) mirrorClaude(a *rota.Account) (string, error) {
	if a.Provider != "claude" {
		return "", nil
	}
	if a.ConfigDir != "" {
		// An account told where its configuration lives was given a world of
		// its own deliberately, and lib already points the CLI at it: a
		// mirror would be a second CLAUDE_CONFIG_DIR and a second answer.
		// The one thing still arranged there is a conversation directory the
		// account was told to keep its transcripts in, which is a choice
		// about this account rather than about the world it reads.
		return a.ConfigDir, s.placeConversations(a, a.ConfigDir)
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
			if !sharedWithClaude(name) || (!shared && keepsConversations(name)) {
				continue
			}
			want[name] = filepath.Join(src, name)
			from[name] = src
		}
		// .claude.json is the one file that does not live inside the
		// directory unless CLAUDE_CONFIG_DIR says so, and it is the one
		// holding trust decisions, project history and the identity the CLI
		// shows.
		if _, err := os.Stat(srcJSON); err == nil {
			want[".claude.json"] = srcJSON
			from[".claude.json"] = src
		}
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
	blocked, err := relink(dst, want, func(string) bool { return true })
	if err != nil {
		return dst, err
	}
	s.sayOwnEntriesStay(a, dst, blocked, from)
	return dst, nil
}

// placeConversations arranges only the conversation entries of a directory
// the person chose, for an account that names both a configuration
// directory and somewhere else to keep its transcripts. Nothing else in
// there is rota's to touch: it is the account's own world, not a mirror.
func (s *Store) placeConversations(a *rota.Account, home string) error {
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
	blocked, err := relink(home, want, keepsConversations)
	if err != nil {
		return err
	}
	s.sayOwnEntriesStay(a, home, blocked, from)
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
// answers to want: one pointing somewhere want does not say, or carrying a
// name want no longer holds at all, goes — a name no longer shared, a file
// the person deleted, a setting that changed since. A real entry is the
// account's and is never removed or replaced.
func relink(dir string, want map[string]string, governs func(string) bool) ([]string, error) {
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
		target, wanted := want[name]
		if got, err := os.Readlink(p); !wanted || err != nil || got != target {
			_ = os.Remove(p)
			delete(mine, name)
			continue
		}
		// An entry that went away since leaves a link to nothing, which
		// Claude Code reads as a broken file rather than as an absent one.
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
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
// Every file the daemon keeps is named for it — the lock, the log, the
// status, the auth cooldown — so the prefix is the rule rather than a list
// that would go stale the next time Claude Code invents one.
// `.credentials.json` is where some platforms keep the CLI's own login, and
// rota hands credentials to Claude Code through the environment and never
// through a vendor's credential store: an account must not be reading the
// person's. `sessions/` is the live-session registry and its socket keys,
// which is how one account would reach another's daemon. `.claude.json` is
// dealt with on its own, and `.DS_Store` is noise a file manager leaves
// behind.
func sharedWithClaude(name string) bool {
	switch {
	case strings.HasPrefix(name, "daemon"):
		return false
	case name == ".credentials.json", name == ".claude.json", name == ".DS_Store":
		return false
	case name == "sessions":
		// The registry of live sessions: one record per process with its
		// socket path, and the key that opens it. Shared, it would let a
		// window of one account attach to a session another account's
		// daemon is hosting — and pay for it. Conversations themselves live
		// in projects/, which is shared unless the account says otherwise,
		// so resuming loses nothing.
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
