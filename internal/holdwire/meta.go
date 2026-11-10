package holdwire

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
)

// Meta is a held session's record, meta.json in its directory. It outlives
// the holder, which is what lets a stopped session be listed and resumed:
// whatever is in it is what is known about the session once its process is
// gone. It never holds a credential rota keeps; Args may hold whatever the
// person typed into the command line, as StoredArgs says.
//
// The holder's pid, start and boot id are there so the pid is signalled only
// while it is still the same process (SameProcess; the start in ticks is
// Linux's alone); the child's are there so the CLI's own registry is
// believed only for the process it is about.
type Meta struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Account  int    `json:"account"`
	Provider string `json:"provider"`

	HolderPID        int       `json:"holder_pid"`
	HolderStart      time.Time `json:"holder_start"`
	HolderStartTicks int64     `json:"holder_start_ticks,omitempty"`
	BootID           string    `json:"boot_id"`
	ChildPID         int       `json:"child_pid"`
	ChildStart       time.Time `json:"child_start"`

	// Protocol is the major the holder speaks, and Version the rota that
	// made it: a holder no major agrees with is named by its version.
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`

	Cwd string `json:"cwd"`
	// Args are the CLI arguments the person gave, through StoredArgs.
	Args []string `json:"args"`

	// The conversation, as the CLI's own registry last said: kept when the
	// registry goes away, never guessed.
	Conversation     string `json:"conversation"`
	ConversationName string `json:"conversation_name"`
	ConversationCwd  string `json:"conversation_cwd"`

	TokenUntil time.Time `json:"token_until"`
	Started    time.Time `json:"started"`
	// Ended and Exit are absent until the CLI ends. An exit is what tells a
	// session that ended from one whose holder vanished.
	Ended time.Time `json:"ended,omitzero"`
	Exit  *int      `json:"exit,omitempty"`
}

// maxMeta bounds what is read of a meta.json: a record is a few hundred
// bytes, and one that is not should not be read whole to find that out.
const maxMeta = 1 << 20

// ReadMeta reads a session's record.
func ReadMeta(dir string) (*Meta, error) {
	raw, err := readMeta(dir)
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := jsonv2.Unmarshal(raw, &m, rota.LenientOptions()); err != nil {
		return nil, fmt.Errorf("holdwire: %s: %w", MetaPath(dir), err)
	}
	return &m, nil
}

// readMeta is a record's bytes, up to maxMeta of them.
func readMeta(dir string) ([]byte, error) {
	f, err := os.Open(MetaPath(dir))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxMeta+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxMeta {
		return nil, fmt.Errorf("holdwire: %s is over %d bytes", MetaPath(dir), maxMeta)
	}
	return raw, nil
}

// WriteMeta replaces a session's record whole, readable by its owner alone:
// a temporary file beside it, then a rename, so a reader never finds half of
// one and a failed write leaves the last one as it was.
func WriteMeta(dir string, m *Meta) error {
	// A folder name need not be UTF-8; it is kept with its odd bytes
	// replaced rather than costing the session its record.
	raw, err := jsonv2.Marshal(m, jsontext.AllowInvalidUTF8(true), jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	return writeAtomic(MetaPath(dir), append(raw, '\n'))
}

// renameFile is the last step of a write, a variable so a test can make it
// fail.
var renameFile = os.Rename

// writeAtomic lands data at path at 0600 through a temporary file in the
// same directory and a rename. The temporary file goes on any failure.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cherr := f.Chmod(0o600) // CreateTemp's own mode, said rather than assumed
	if errors.Is(cherr, errors.ErrUnsupported) {
		cherr = nil
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(cherr, werr, serr, cerr); err == nil {
		err = renameFile(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// StoredArgs is the CLI arguments a session keeps for Resume and Move: how
// the CLI was asked to run — model, permission mode, Remote Control — without
// which conversation it was in, which is the one thing Resume and Move
// decide afresh. The caller's slice is left as it was.
//
// It keeps positional arguments, and every flag's value, just as the child's
// argv has them: a prompt given at the start, inline --mcp-config JSON with
// whatever env it carries, an --append-system-prompt. Telling a flag's value
// from a positional needs each CLI's arity, which rota does not keep. So a
// later Resume or Move must not replay these args as they are: a stored
// prompt would run its task again in the resumed conversation. It must drop
// the positionals, or split them out where the CLI's arity is known.
//
// Only flags this rota knows are dropped. A provider whose session flags it
// does not know keeps every argument: carrying one too many is a CLI that
// says so, where guessing would drop something the person asked for.
func StoredArgs(provider string, args []string) []string {
	out := make([]string, 0, len(args))
	switch rota.Flavor(provider) {
	case "claude":
		return claudeStored(args, out)
	case "codex":
		return codexStored(args, out)
	}
	return append(out, args...)
}

// claudeStored drops Claude Code's flags that pick a conversation. --resume
// and --from-pr take a value only when one follows (bare, they open a
// picker); --session-id always does; --continue and --fork-session take
// none. After "--" everything is the prompt's.
func claudeStored(args, out []string) []string {
	withValue := map[string]bool{"--resume": true, "-r": true, "--from-pr": true, "--session-id": true}
	alone := map[string]bool{"--continue": true, "-c": true, "--fork-session": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(out, args[i:]...)
		}
		name, _, inline := strings.Cut(a, "=")
		switch {
		case alone[a]:
		case withValue[name] && inline:
		case withValue[a]:
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		default:
			out = append(out, a)
		}
	}
	return out
}

// codexStored drops codex's resume subcommand, the session it names, and
// the flags that are resume's own.
func codexStored(args, out []string) []string {
	resumed := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(out, args[i:]...)
		case !resumed && a == "resume":
			resumed = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		case resumed && (a == "--last" || a == "--all"):
		default:
			out = append(out, a)
		}
	}
	return out
}
