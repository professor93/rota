package rotation

import (
	"errors"
	"testing"

	rota "github.com/professor93/rota/lib"
)

func TestParsePlaceReadsNumbersAndWords(t *testing.T) {
	for in, want := range map[string]Place{
		"1":        {kind: placeAt, n: 1},
		"12":       {kind: placeAt, n: 12},
		"0":        {kind: placeOut},
		"out":      {kind: placeOut},
		"first":    {kind: placeFirst},
		"last":     {kind: placeLast},
		"up":       {kind: placeUp},
		"down":     {kind: placeDown},
		"before:3": {kind: placeBefore, n: 3},
		"after:7":  {kind: placeAfter, n: 7},
		" First ":  {kind: placeFirst},
	} {
		got, err := ParsePlace(in)
		if err != nil || got != want {
			t.Fatalf("%q: got %+v, %v; want %+v", in, got, err, want)
		}
	}
}

func TestParsePlaceRejectsWhatCannotBeAPlace(t *testing.T) {
	for _, in := range []string{"", "-1", "x", "1.5", "before", "before:", "before:x", "after:0", "between:2", "1 2"} {
		if _, err := ParsePlace(in); err == nil {
			t.Fatalf("%q was accepted", in)
		}
	}
}

// three is a queue of accounts 1, 2, 3 in that order, plus 4 outside it.
func three() []*rota.Account {
	return []*rota.Account{acct(1, 1, 0), acct(2, 2, 0), acct(3, 3, 0), acct(4, 0, 0)}
}

func orders(list []*rota.Account) map[int]int {
	out := map[int]int{}
	for _, a := range list {
		out[a.ID] = a.Order
	}
	return out
}

func mustMove(t *testing.T, list []*rota.Account, id int, place string) Moved {
	t.Helper()
	p, err := ParsePlace(place)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Move(list, rota.FindID(list, id), p)
	if err != nil {
		t.Fatalf("move %d to %s: %v", id, place, err)
	}
	return m
}

func TestMoveToANumberShiftsLaterAccountsDown(t *testing.T) {
	list := three()
	m := mustMove(t, list, 3, "1")
	eq(t, ids(Queue(list)), []int{3, 1, 2})
	if got := orders(list); got[3] != 1 || got[1] != 2 || got[2] != 3 || got[4] != 0 {
		t.Fatalf("%v", got)
	}
	if m.Was != 3 || m.Now != 1 {
		t.Fatalf("%+v", m)
	}
	eq(t, ids(m.Shifted), []int{1, 2})
}

func TestMovePastTheEndMeansLast(t *testing.T) {
	list := three()
	m := mustMove(t, list, 1, "50")
	eq(t, ids(Queue(list)), []int{2, 3, 1})
	if got := orders(list); got[1] != 3 || got[2] != 1 || got[3] != 2 {
		t.Fatalf("a number past the end must not leave a gap: %v", got)
	}
	if m.Now != 3 {
		t.Fatalf("%+v", m)
	}
}

func TestMoveOutClosesTheGap(t *testing.T) {
	for _, word := range []string{"0", "out"} {
		list := three()
		m := mustMove(t, list, 1, word)
		eq(t, ids(Queue(list)), []int{2, 3})
		if got := orders(list); got[1] != 0 || got[2] != 1 || got[3] != 2 {
			t.Fatalf("%s: %v", word, got)
		}
		if m.Was != 1 || m.Now != 0 {
			t.Fatalf("%+v", m)
		}
		eq(t, ids(m.Shifted), []int{2, 3})
	}
}

func TestMoveFirstAndLast(t *testing.T) {
	list := three()
	mustMove(t, list, 3, "first")
	eq(t, ids(Queue(list)), []int{3, 1, 2})
	mustMove(t, list, 3, "last")
	eq(t, ids(Queue(list)), []int{1, 2, 3})
	// An account outside the queue joins it.
	m := mustMove(t, list, 4, "last")
	eq(t, ids(Queue(list)), []int{1, 2, 3, 4})
	if m.Was != 0 || m.Now != 4 || len(m.Shifted) != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestMoveUpAndDownTradePlacesWithANeighbour(t *testing.T) {
	list := three()
	m := mustMove(t, list, 2, "up")
	eq(t, ids(Queue(list)), []int{2, 1, 3})
	eq(t, ids(m.Shifted), []int{1})
	m = mustMove(t, list, 2, "down")
	eq(t, ids(Queue(list)), []int{1, 2, 3})
	eq(t, ids(m.Shifted), []int{1})
}

func TestMoveUpAtTheTopOrDownAtTheBottomChangesNothing(t *testing.T) {
	list := three()
	for id, word := range map[int]string{1: "up", 3: "down"} {
		m := mustMove(t, list, id, word)
		eq(t, ids(Queue(list)), []int{1, 2, 3})
		if m.Was != m.Now || len(m.Shifted) != 0 {
			t.Fatalf("%s: %+v", word, m)
		}
	}
}

func TestMoveUpOrDownNeedsAPlaceInTheQueue(t *testing.T) {
	list := three()
	for _, word := range []string{"up", "down"} {
		p, _ := ParsePlace(word)
		if _, err := Move(list, rota.FindID(list, 4), p); !errors.Is(err, rota.ErrInvalidRequest) {
			t.Fatalf("%s from outside the queue: %v", word, err)
		}
	}
	eq(t, ids(Queue(list)), []int{1, 2, 3})
}

func TestMoveBeforeAndAfterAnotherAccount(t *testing.T) {
	list := three()
	mustMove(t, list, 4, "before:2")
	eq(t, ids(Queue(list)), []int{1, 4, 2, 3})
	mustMove(t, list, 1, "after:3")
	eq(t, ids(Queue(list)), []int{4, 2, 3, 1})
	mustMove(t, list, 1, "after:2")
	eq(t, ids(Queue(list)), []int{4, 2, 1, 3})
	if got := orders(list); got[4] != 1 || got[2] != 2 || got[1] != 3 || got[3] != 4 {
		t.Fatalf("%v", got)
	}
}

func TestMoveRelativeToItselfOrToNothingInTheQueueIsAnError(t *testing.T) {
	list := three()
	for _, word := range []string{"before:1", "after:1", "before:4", "after:9"} {
		p, _ := ParsePlace(word)
		if _, err := Move(list, rota.FindID(list, 1), p); !errors.Is(err, rota.ErrInvalidRequest) {
			t.Fatalf("%s: %v", word, err)
		}
	}
	eq(t, ids(Queue(list)), []int{1, 2, 3})
}

func TestMoveRepairsTiesAndGapsLeftByOlderStores(t *testing.T) {
	list := []*rota.Account{acct(1, 1, 0), acct(2, 1, 0), acct(3, 5, 0)}
	m := mustMove(t, list, 3, "first")
	eq(t, ids(Queue(list)), []int{3, 1, 2})
	if got := orders(list); got[3] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("the queue must read 1, 2, 3 afterwards: %v", got)
	}
	if m.Was != 3 {
		t.Fatalf("the place it held is its position in the queue, not the number it carried: %+v", m)
	}
	eq(t, ids(m.Shifted), []int{1, 2})
}

func TestMoveToWhereItAlreadyIsShiftsNothing(t *testing.T) {
	list := three()
	m := mustMove(t, list, 2, "2")
	if m.Was != 2 || m.Now != 2 || len(m.Shifted) != 0 {
		t.Fatalf("%+v", m)
	}
	m = mustMove(t, list, 4, "out")
	if m.Was != 0 || m.Now != 0 || len(m.Shifted) != 0 {
		t.Fatalf("%+v", m)
	}
}
