package rotation

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	rota "github.com/professor93/rota/lib"
)

// The queue is a list, not a set of numbers. Giving an account a place
// means the accounts after it move down one, leaving means the gap closes,
// and afterwards the queue always reads 1, 2, 3. Two accounts never share a
// number and nothing sits at 7 with nothing at 4 — which is what the order
// field used to allow, and what made "put this one first" a chore of
// renumbering everything else by hand.

type placeKind uint8

const (
	placeAt     placeKind = iota // a number: that place, or last if past the end
	placeOut                     // out of the queue
	placeFirst                   //
	placeLast                    //
	placeUp                      // one place earlier
	placeDown                    // one place later
	placeBefore                  // right before another account, by id
	placeAfter                   // right after another account, by id
)

// Place is where an account should go in the queue: a number, a word, or a
// position relative to another account. Get one from ParsePlace.
type Place struct {
	kind placeKind
	n    int // the number for placeAt, the other account's id for before/after
}

// ParsePlace reads a place as somebody would type it: a number (1 goes
// first; 0 leaves the queue), or one of the words out, first, last, up and
// down, or before:<id> / after:<id>. Case and surrounding space do not
// matter.
func ParsePlace(s string) (Place, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "out":
		return Place{kind: placeOut}, nil
	case "first":
		return Place{kind: placeFirst}, nil
	case "last":
		return Place{kind: placeLast}, nil
	case "up":
		return Place{kind: placeUp}, nil
	case "down":
		return Place{kind: placeDown}, nil
	}
	if word, id, ok := strings.Cut(s, ":"); ok {
		kind := placeBefore
		if word == "after" {
			kind = placeAfter
		} else if word != "before" {
			return Place{}, badPlace(s)
		}
		n, err := strconv.Atoi(id)
		if err != nil || n < 1 {
			return Place{}, fmt.Errorf("%w: %q must name an account id, like %s:2", rota.ErrInvalidRequest, s, word)
		}
		return Place{kind: kind, n: n}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return Place{}, badPlace(s)
	}
	if n == 0 {
		return Place{kind: placeOut}, nil
	}
	return Place{kind: placeAt, n: n}, nil
}

func badPlace(s string) error {
	return fmt.Errorf("%w: %q is not a place; use a number, 0 or out, first, last, up, down, before:<id> or after:<id>",
		rota.ErrInvalidRequest, s)
}

// Moved is what one Move changed: the place the account held before and
// holds now (0 is out of the queue), and the accounts whose number changed
// to make room, in queue order.
type Moved struct {
	Was, Now int
	Shifted  []*rota.Account
}

// Move puts a at the place and renumbers the queue so that it reads 1..N
// with nothing shared and nothing skipped. Accounts outside the queue stay
// outside it. On error nothing has changed.
//
// up and down need a place to move from, so they refuse an account outside
// the queue; before and after only need the other account to be in it.
func Move(accounts []*rota.Account, a *rota.Account, p Place) (Moved, error) {
	queue := Queue(accounts)
	// The place held is the position in the queue as the rotation reads it,
	// not the number carried: an old store may hold ties, and the number an
	// account shares with another is not where it actually is.
	was := slices.Index(queue, a) + 1
	rest := slices.DeleteFunc(queue, func(x *rota.Account) bool { return x == a })

	at := -1 // index in rest to insert at; -1 is out of the queue
	switch p.kind {
	case placeAt:
		at = min(p.n-1, len(rest))
	case placeOut:
	case placeFirst:
		at = 0
	case placeLast:
		at = len(rest)
	case placeUp, placeDown:
		if was == 0 {
			return Moved{}, fmt.Errorf("%w: %s is out of the rotation; give it a place first", rota.ErrInvalidRequest, a)
		}
		if p.kind == placeUp {
			at = max(was-2, 0)
		} else {
			at = min(was, len(rest))
		}
	case placeBefore, placeAfter:
		other := slices.IndexFunc(rest, func(x *rota.Account) bool { return x.ID == p.n })
		if other < 0 {
			if p.n == a.ID {
				return Moved{}, fmt.Errorf("%w: %s cannot be placed relative to itself", rota.ErrInvalidRequest, a)
			}
			return Moved{}, fmt.Errorf("%w: no account %d in the rotation to place %s next to", rota.ErrInvalidRequest, p.n, a)
		}
		at = other
		if p.kind == placeAfter {
			at++
		}
	}

	if at >= 0 {
		rest = slices.Insert(rest, at, a)
	}
	m := Moved{Was: was, Now: at + 1}
	before := make(map[*rota.Account]int, len(rest))
	for _, x := range rest {
		before[x] = x.Order
	}
	for i, x := range rest {
		x.Order = i + 1
	}
	if at < 0 {
		a.Order = 0
	}
	for _, x := range rest {
		if x != a && x.Order != before[x] {
			m.Shifted = append(m.Shifted, x)
		}
	}
	return m, nil
}
