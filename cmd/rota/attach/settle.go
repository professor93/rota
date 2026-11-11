package attach

import (
	"regexp"
	"time"

	"github.com/professor93/rota/internal/vtscan"
)

// settleDA1 is a terminal's answer to ESC [ c: ESC [ ? and its attributes,
// then c.
var settleDA1 = regexp.MustCompile(`\x1b\[\?[0-9;]*c`)

// settleMax bounds what settle holds while it waits. Past it, nothing a
// terminal still owed can be told from typing, and what was held is filtered
// and typed as though the wait had ended.
const settleMax = 64 << 10

// settle drops the replies the person's terminal still owes to whatever
// used it before the attach. It asks for the terminal's attributes, ESC [ c,
// and holds what is read until the answer comes: the answer and everything
// before it are dropped, since a terminal answers in order, and what came
// after it is the person's. With no answer within settleFor, what was held
// is typed less every reply ReplyFilter knows.
//
// What is to be typed is kept in r.early for the session once it is open.
// It says false when the attach ended meanwhile.
func (r *runner) settle() bool {
	r.write([]byte("\x1b[c"))
	if r.out.err != nil {
		r.end(Lost, 0, r.out.err)
		return false
	}
	timer := time.NewTimer(settleFor)
	defer timer.Stop()
	var held []byte
	for {
		select {
		case <-r.ctx.Done():
			r.end(Lost, 0, r.ctx.Err())
			return false
		case p, ok := <-r.t.Input():
			if !ok {
				r.inEnded = true
				r.end(Lost, 0, runInputEnd(r.t))
				return false
			}
			held = append(held, p...)
			if loc := settleDA1.FindIndex(held); loc != nil {
				// What follows the answer is filtered too: a terminal that
				// owed a DA1 answer already answers this one after it.
				r.early = settleStrip(held[loc[1]:])
				return true
			}
			if len(held) > settleMax {
				r.early = settleStrip(held)
				return true
			}
		case <-timer.C:
			r.early = settleStrip(held)
			return true
		}
	}
}

// settleStrip is held less the replies in it.
func settleStrip(held []byte) []byte {
	var f vtscan.ReplyFilter
	return f.Strip(held)
}
