package message

import (
	"fmt"
	"strings"

	rota "github.com/professor93/rota/lib"
)

// With is what a caller asked to have beside the answer, beyond the answer
// itself. Nothing is read, copied or asked of the CLI unless it was named:
// the reply is the answer as the CLI gave it, and every reading here is a
// caller's choice, not rota's.
//
// Three kinds of name live here. Readings derive something from the answer
// or the stream (blocks, ask, code, files, timing, tools, stats, plain,
// links). Copies carry something rota already holds (raw, account, argv,
// quota, stderr). Requests change what the CLI is asked for (deltas, hooks,
// subagents, suggestions); those are applied to the Spec before the run.
type With struct {
	Blocks      bool // the answer split at fences, on the reply and on whole text events
	Ask         bool // the question the answer ends with, on the reply
	Raw         bool // the provider's own line on each event; every line in a buffered reply
	Deltas      bool // each fragment as the model writes it; implies a stream
	Code        bool // the fenced code alone, in order
	Files       bool // paths the agent's tools read and wrote
	Timing      bool // at on each event; first text, first tool and total on the reply
	Quota       bool // the account's usage windows after the run
	Tools       bool // a tally of tool calls, and of the blocked ones
	Stats       bool // event counts, fragments, bytes read
	Argv        bool // the command line rota ran, and the names of the variables it set
	Plain       bool // the answer with markdown flattened
	Links       bool // the URLs in the answer
	Account     bool // the account's label, place and threshold
	Hooks       bool // the CLI's hook lifecycle as events
	Subagents   bool // what delegated subagents say and think, as events
	Suggestions bool // the CLI's predicted follow-up, when it prints one
	Stderr      bool // on a failed run with no answer, stderr copied into result
}

// Names are the readings ParseWith knows, in the order they are listed to a
// caller who named one it does not.
var Names = []string{
	"blocks", "ask", "raw", "deltas", "code", "files", "timing", "quota", "tools", "stats",
	"argv", "plain", "links", "account", "hooks", "subagents", "suggestions", "stderr",
}

// ParseWith reads the names a caller gave, each argument a name or a comma
// list of them, so `--with ask --with blocks` and `--with ask,blocks` say the
// same thing. Spaces around a name and empty entries are nothing. A name
// nobody knows is refused by name, with the known ones beside it.
func ParseWith(names ...string) (With, error) {
	var w With
	for _, arg := range names {
		for name := range strings.SplitSeq(arg, ",") {
			switch strings.TrimSpace(name) {
			case "":
			case "blocks":
				w.Blocks = true
			case "ask":
				w.Ask = true
			case "raw":
				w.Raw = true
			case "deltas":
				w.Deltas = true
			case "code":
				w.Code = true
			case "files":
				w.Files = true
			case "timing":
				w.Timing = true
			case "quota":
				w.Quota = true
			case "tools":
				w.Tools = true
			case "stats":
				w.Stats = true
			case "argv":
				w.Argv = true
			case "plain":
				w.Plain = true
			case "links":
				w.Links = true
			case "account":
				w.Account = true
			case "hooks":
				w.Hooks = true
			case "subagents":
				w.Subagents = true
			case "suggestions":
				w.Suggestions = true
			case "stderr":
				w.Stderr = true
			default:
				return With{}, fmt.Errorf("with: unknown reading %q; known: %s", strings.TrimSpace(name), strings.Join(Names, ", "))
			}
		}
	}
	return w, nil
}

// Apply turns the requests among the readings into what the CLI is asked
// for. The rest are read out of what comes back and need nothing here.
func (w With) Apply(s *rota.Spec) {
	if w.Raw {
		s.IncludeEvents = true
	}
	if w.Deltas {
		s.IncludePartialMessages = true
	}
	if w.Hooks {
		s.IncludeHookEvents = true
	}
	if w.Subagents {
		s.ForwardSubagentText = true
	}
	if w.Suggestions {
		s.PromptSuggestions = true
	}
	if w.Argv {
		s.IncludeArgv = true
	}
}

// NeedsJSON reports whether any reading asked for has nowhere to go but a
// JSON reply. Deltas, hooks, subagents and suggestions only change what the
// CLI prints, and text mode can show that.
func (w With) NeedsJSON() bool {
	return w.Blocks || w.Ask || w.Raw || w.Code || w.Files || w.Timing || w.Quota || w.Tools ||
		w.Stats || w.Argv || w.Plain || w.Links || w.Account || w.Stderr
}

// NeedsStream reports whether a reading is a piece of a stream: fragments
// cannot arrive in a buffered reply.
func (w With) NeedsStream() bool { return w.Deltas }

// Reply is a finished run on the wire, the same shape on the command line
// and over HTTP: what the SDK produced, and beside it only the readings
// that were asked for.
type Reply struct {
	*rota.Result
	Blocks []Block `json:"blocks,omitzero"`
	Ask    *Ask    `json:"ask,omitzero"`
	Readings
}

// ReplyFor reads out of a result what w asked for, and nothing else. src is
// what the caller holds that the result does not: the tally of the stream,
// the account, its threshold, and a quota reading it fetched when asked.
func ReplyFor(res *rota.Result, w With, src Sources) *Reply {
	r := &Reply{Result: res}
	if w.Stderr && res.IsError && res.Result == "" && res.Stderr != "" {
		// The one reading that touches result, on request only: a copy of
		// the result with the reason where a one-field client will look.
		copied := *res
		copied.Result = res.Stderr
		r.Result = &copied
	}
	if w.Blocks {
		r.Blocks = Blocks(r.Result.Result)
	}
	if w.Ask {
		r.Ask = Asked(r.Result.Result)
	}
	r.Readings = Read(r.Result, w, src)
	return r
}
