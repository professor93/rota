package tui

import (
	"maps"
	"slices"
	"testing"
)

// ladderOf reads a layout as each shown column's width, by key.
func ladderOf(cols []col) map[string]int {
	m := map[string]int{}
	for _, c := range cols {
		if c.Shown {
			m[c.Key] = c.W
		}
	}
	return m
}

func TestTheLadderShedsInOrder(t *testing.T) {
	keys := []string{"place", "id", "account", "provider", "5h", "7d", "reset", "state"}
	for _, c := range []struct {
		width int
		want  map[string]int
	}{
		{124, map[string]int{"place": 2, "id": 3, "account": 20, "provider": 6, "5h": 23, "7d": 23, "reset": 17, "state": 16}},
		// At 120 every column shows, with the account at 16.
		{120, map[string]int{"place": 2, "id": 3, "account": 16, "provider": 6, "5h": 23, "7d": 23, "reset": 17, "state": 16}},
		// At 80: no id and no provider, bars of 8, the reset short. The
		// two columns left over go to the account.
		{80, map[string]int{"place": 2, "account": 14, "5h": 15, "7d": 15, "reset": 8, "state": 16}},
		// At 60: no id, provider or 7d, bars of 8, and what is left over
		// gives the state back its room.
		{60, map[string]int{"place": 2, "account": 12, "5h": 15, "reset": 8, "state": 15}},
		// Spare width goes to the state, then to the account up to 24,
		// then to the state again.
		{128, map[string]int{"place": 2, "id": 3, "account": 24, "provider": 6, "5h": 23, "7d": 23, "reset": 17, "state": 16}},
		{140, map[string]int{"place": 2, "id": 3, "account": 24, "provider": 6, "5h": 23, "7d": 23, "reset": 17, "state": 28}},
	} {
		cols := accountsLayout(c.width)
		var got []string
		for _, col := range cols {
			got = append(got, col.Key)
		}
		if !slices.Equal(got, keys) {
			t.Errorf("at %d the columns are %v, want %v in that order", c.width, got, keys)
		}
		if m := ladderOf(cols); !maps.Equal(m, c.want) {
			t.Errorf("at %d the shown columns are %v, want %v", c.width, m, c.want)
		}
		if total := ladderTotal(cols); total > c.width {
			t.Errorf("at %d the columns take %d", c.width, total)
		} else if total != c.width {
			t.Errorf("at %d the columns take %d, leaving the rest unused", c.width, total)
		}
	}
	// Every width from the screen's narrowest on fits, and narrowing never
	// shows a column a wider screen shed.
	prev := ladderOf(accountsLayout(200))
	for w := 199; w >= 60; w-- {
		cols := accountsLayout(w)
		if total := ladderTotal(cols); total > w {
			t.Errorf("at %d the columns take %d", w, total)
		}
		m := ladderOf(cols)
		for k := range m {
			if _, ok := prev[k]; !ok {
				t.Errorf("at %d column %s shows, which %d shed", w, k, w+1)
			}
		}
		prev = m
	}
	// The same width gives the same layout, always.
	if !slices.Equal(accountsLayout(97), accountsLayout(97)) {
		t.Error("one width gave two layouts")
	}
	// A width no screen draws at still gives every column, none negative.
	for _, w := range []int{0, 1, 30, 48} {
		for _, col := range accountsLayout(w) {
			if col.W < 0 || col.Shown && col.W == 0 {
				t.Errorf("at %d column %s is %d wide, shown %v", w, col.Key, col.W, col.Shown)
			}
		}
	}
}
