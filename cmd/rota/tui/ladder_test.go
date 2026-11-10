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
		// At full width every column shows at its own width.
		{125, map[string]int{"place": 2, "id": 3, "account": 20, "provider": 6, "5h": 23, "7d": 23, "reset": 18, "state": 16}},
		// Spare width goes to the state up to its 16, then to the account up to
		// 24, then to the state again.
		{129, map[string]int{"place": 2, "id": 3, "account": 24, "provider": 6, "5h": 23, "7d": 23, "reset": 18, "state": 16}},
		{141, map[string]int{"place": 2, "id": 3, "account": 24, "provider": 6, "5h": 23, "7d": 23, "reset": 18, "state": 28}},
		// Step 1: the account to 15, and the four left over back to it.
		{124, map[string]int{"place": 2, "id": 3, "account": 19, "provider": 6, "5h": 23, "7d": 23, "reset": 18, "state": 16}},
		// At 120 every column shows, with the account at 15.
		{120, map[string]int{"place": 2, "id": 3, "account": 15, "provider": 6, "5h": 23, "7d": 23, "reset": 18, "state": 16}},
		// Step 2: no id.
		{119, map[string]int{"place": 2, "account": 19, "provider": 6, "5h": 23, "7d": 23, "reset": 18, "state": 16}},
		// Step 3: no provider.
		{114, map[string]int{"place": 2, "account": 22, "5h": 23, "7d": 23, "reset": 18, "state": 16}},
		// Step 4: bars of 10, and the account at its 24.
		{106, map[string]int{"place": 2, "account": 24, "5h": 17, "7d": 17, "reset": 18, "state": 18}},
		// Step 5: the reset short.
		{94, map[string]int{"place": 2, "account": 24, "5h": 17, "7d": 17, "reset": 8, "state": 16}},
		// Step 6: the account to 12.
		{84, map[string]int{"place": 2, "account": 14, "5h": 17, "7d": 17, "reset": 8, "state": 16}},
		// Step 7: bars of 8.
		{81, map[string]int{"place": 2, "account": 15, "5h": 15, "7d": 15, "reset": 8, "state": 16}},
		// At 80: no id and no provider, bars of 8, the reset short. The two
		// columns left over go to the account.
		{80, map[string]int{"place": 2, "account": 14, "5h": 15, "7d": 15, "reset": 8, "state": 16}},
		// Step 8: the state to 8, given back what is left over.
		{77, map[string]int{"place": 2, "account": 12, "5h": 15, "7d": 15, "reset": 8, "state": 15}},
		// Step 9: no 7d, and sixteen left over.
		{69, map[string]int{"place": 2, "account": 20, "5h": 15, "reset": 8, "state": 16}},
		// At 60: no id, provider or 7d, bars of 8, and what is left over gives
		// the state back its room.
		{60, map[string]int{"place": 2, "account": 12, "5h": 15, "reset": 8, "state": 15}},
		// Steps 10 and 11 come only below the 60 columns the screen draws at.
		{52, map[string]int{"place": 2, "account": 12, "5h": 13, "reset": 8, "state": 9}},
		{50, map[string]int{"place": 2, "account": 10, "5h": 13, "reset": 8, "state": 9}},
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
	// Every width down to the narrowest the steps reach fits, and narrowing
	// never shows a column a wider screen shed.
	prev := ladderOf(accountsLayout(200))
	for w := 199; w >= 49; w-- {
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
