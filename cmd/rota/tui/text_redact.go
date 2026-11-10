package tui

// The redaction half of clean: a key an escape sequence was glued to, split
// or ate into, found and taken out once textEscapes has replaced the
// sequences.

import (
	"sort"
	"strings"
)

// textRedactKey is the start of an Anthropic credential: wire.Unsecret's
// shape, which takes "sk-ant-" and at least 16 more key characters
// (textKeyByte) as a key when it stands as a word of its own.
const textRedactKey = "sk-ant-"

// textRedact is s, in which each seam marks where an escape sequence stood
// (on the space that stands for it, or between two bytes where none does)
// and holds the key characters the sequence took, with every key taken out
// that either of two readings finds: the sequences gone, so a key a
// sequence split shows as neither half, and each sequence's key characters
// put back, so a key whose "s" or "-a" a sequence took is found too.
//
// A key starts where s starts, after a character that is not a letter or
// digit, as wire.Unsecret has it, and at any seam, spaced or not, or
// inside or right after what a sequence took: a sequence stands between
// words whether or not it left a space, so a key a sequence was glued to
// is taken out too.
// Its replacement is wire.Unsecret's. The second result is how many bytes
// of key bodies it read: each once, as a body is read only where a key
// starts.
func textRedact(s string, seams []textSeam) (string, int) {
	if len(seams) == 0 {
		return s, 0
	}
	looked := 0
	ateAt := make(map[int]string, len(seams)) // what the seams at a place took
	space := make(map[int]bool, len(seams))   // a seam's own space
	for x := 0; x < len(seams); {
		var k strings.Builder
		at := seams[x].at
		for ; x < len(seams) && seams[x].at == at; x++ {
			k.WriteString(seams[x].ate)
			if seams[x].space {
				space[at] = true
			}
		}
		ateAt[at] = k.String()
	}
	type span struct{ from, to int } // in s
	var spans []span
	// Two readings: the sequences gone, and each sequence's key bytes kept.
	for _, keep := range []bool{false, true} {
		var j strings.Builder
		at := make([]int, 0, len(s))
		var eaten []bool
		for i := 0; i <= len(s); i++ {
			if k := ateAt[i]; keep {
				for x := 0; x < len(k); x++ {
					j.WriteByte(k[x])
					at = append(at, i)
					eaten = append(eaten, true)
				}
			}
			if i == len(s) || space[i] {
				continue
			}
			j.WriteByte(s[i])
			at = append(at, i)
			eaten = append(eaten, false)
		}
		js := j.String()
		for k := 0; k < len(js); {
			off := strings.Index(js[k:], textRedactKey)
			if off < 0 {
				break
			}
			start := k + off
			// Right after what a sequence took is right after its seam:
			// at the seam's place when it left no space (seamHere), past
			// its space when it did.
			_, seamHere := ateAt[at[start]]
			word := start == 0 || eaten[start] || seamHere || space[at[start]-1] || !(textKeyByte(js[start-1]) && js[start-1] != '_' && js[start-1] != '-')
			if !word {
				k = start + 1
				continue
			}
			end := start + len(textRedactKey)
			for end < len(js) && textKeyByte(js[end]) {
				end++
			}
			looked += end - start
			if end-start-len(textRedactKey) >= 16 {
				// A key ending on what a sequence took ends at the seam,
				// past its space if it has one.
				to := at[end-1] + 1
				if eaten[end-1] && !space[at[end-1]] {
					to--
				}
				spans = append(spans, span{at[start], to})
				k = end
				continue
			}
			k = start + 1
		}
	}
	if len(spans) == 0 {
		return s, looked
	}
	sort.Slice(spans, func(x, y int) bool { return spans[x].from < spans[y].from })
	var b strings.Builder
	done := 0
	for _, sp := range spans {
		if sp.to <= done {
			continue
		}
		if sp.from < done {
			sp.from = done
		} else {
			b.WriteString(s[done:sp.from])
		}
		b.WriteString(textRedactKey + "[not repeated]")
		done = sp.to
	}
	b.WriteString(s[done:])
	return b.String(), looked
}
