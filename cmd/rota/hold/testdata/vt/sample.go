// Package sample is what vim shows in the emulator spike: enough Go for
// syntax colours, a search and line numbers, and more lines than the screen.
package sample

import (
	"fmt"
	"strings"
)

// Greeting is who is greeted, and how loudly.
type Greeting struct {
	Name  string
	Times int
	Shout bool
}

// String says hello as often as asked.
func (g Greeting) String() string {
	var b strings.Builder
	for i := 0; i < g.Times; i++ {
		fmt.Fprintf(&b, "hello, %s\n", g.Name)
	}
	if g.Shout {
		return strings.ToUpper(b.String())
	}
	return b.String()
}

// count is how many words a line has, as a field split sees them.
func count(line string) int {
	return len(strings.Fields(line))
}

// widest returns the longest line, and 0 for none.
func widest(lines []string) (int, string) {
	n, at := 0, ""
	for _, l := range lines {
		if len(l) > n {
			n, at = len(l), l
		}
	}
	return n, at
}

const banner = `tabs	and "quotes" and 世界 and 0x1f`

var table = map[string]float64{
	"pi":    3.14159,
	"e":     2.71828,
	"sqrt2": 1.41421,
}
