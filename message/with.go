package message

import (
	"fmt"
	"strings"

	rota "github.com/professor93/rota/lib"
)

// Reply is a finished run on the wire, the same shape on the command line
// and over HTTP: what the SDK produced, and beside it only the readings
// that were asked for.
type Reply struct {
	*rota.Result
	Blocks []Block `json:"blocks,omitzero"`
	Ask    *Ask    `json:"ask,omitzero"`
}

// ReplyFor reads out of a result what w asked for, and nothing else.
func ReplyFor(res *rota.Result, w With) *Reply {
	r := &Reply{Result: res}
	if w.Blocks {
		r.Blocks = Blocks(res.Result)
	}
	if w.Ask {
		r.Ask = Asked(res.Result)
	}
	return r
}

// With is what a caller asked to have read out of an answer, beyond the
// answer itself. Nothing is read unless it was asked for: the reply is the
// answer as the CLI gave it, and a reading is a caller's choice, not rota's.
type With struct {
	// Blocks splits the answer at fences into prose and code, on the reply
	// and on every whole text event.
	Blocks bool
	// Ask reads the question an answer ends with, on the reply only.
	Ask bool
}

// Readings are the names ParseWith knows, in the order they are listed to
// a caller who named one it does not.
var Readings = []string{"blocks", "ask"}

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
			default:
				return With{}, fmt.Errorf("with: unknown reading %q; known: %s", strings.TrimSpace(name), strings.Join(Readings, ", "))
			}
		}
	}
	return w, nil
}
