package tui

// col is one column of a table laid out for a width: its key, how many
// columns it takes, and whether it shows at all. A ladder gives every key
// it knows, in the order they are drawn, shown or not.
type col struct {
	Key   string
	W     int
	Shown bool
}

// ladderGap is the space between two shown columns.
const ladderGap = 2

// ladderTotal is how wide cols draw: the shown columns and a gap between
// each two.
func ladderTotal(cols []col) int {
	total, shown := 0, 0
	for _, c := range cols {
		if c.Shown {
			total += c.W
			shown++
		}
	}
	if shown > 1 {
		total += ladderGap * (shown - 1)
	}
	return total
}

// ladderAccounts is the Accounts table at its widest, before any step.
func ladderAccounts() []col {
	return []col{
		{"place", 2, true},
		{"id", 3, true},
		{"account", 20, true},
		{"provider", 6, true},
		{"5h", 16 + barsFrame, true},
		{"7d", 16 + barsFrame, true},
		{"reset", 17, true},
		{"state", 16, true},
	}
}

// ladderSteps is what the Accounts table gives up, in order, while it is
// wider than the screen (R12). Each step sets a column's width, or hides
// it with a width of 0.
var ladderSteps = []struct {
	keys []string
	w    int
}{
	{[]string{"account"}, 16},
	{[]string{"id"}, 0},
	{[]string{"provider"}, 0},
	{[]string{"5h", "7d"}, 10 + barsFrame},
	{[]string{"reset"}, 8}, // the countdown alone: resetText's short
	{[]string{"account"}, 12},
	{[]string{"5h", "7d"}, 8 + barsFrame},
	{[]string{"state"}, 8},
	{[]string{"7d"}, 0},
	{[]string{"5h", "7d"}, 6 + barsFrame},
	{[]string{"account"}, 10},
}

// accountsLayout is the Accounts table's columns at width: it sheds by
// ladderSteps while the shown columns are wider, then gives what is left
// over to the state up to its own 16, to the account up to 24, and the rest
// to the state. It depends on width alone, so a session starting or a
// listing changing never reflows the table. Below about 50 columns, which
// the screen never draws at, the table stays wider than width.
func accountsLayout(width int) []col {
	cols := ladderAccounts()
	at := func(key string) *col {
		for i := range cols {
			if cols[i].Key == key {
				return &cols[i]
			}
		}
		panic("ladder: no column " + key)
	}
	for _, s := range ladderSteps {
		if ladderTotal(cols) <= width {
			break
		}
		for _, k := range s.keys {
			c := at(k)
			if !c.Shown {
				continue
			}
			c.W = s.w
			c.Shown = s.w > 0
		}
	}
	spare := width - ladderTotal(cols)
	grow := func(c *col, upTo int) {
		if n := min(spare, upTo-c.W); n > 0 {
			c.W += n
			spare -= n
		}
	}
	state, account := at("state"), at("account")
	grow(state, 16)
	grow(account, 24)
	if spare > 0 {
		state.W += spare
	}
	return cols
}
