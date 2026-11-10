package sshd

import "errors"

// words splits a command line the client sent into words: on spaces and
// tabs, with single and double quotes keeping what is between them in one
// word, as a shell does. That is all it does. A backslash, a `$`, a `*`, a
// `;` and a newline are characters like any other, so nothing in a line can
// name a second command, a variable or a file; a quote left open is refused
// rather than guessed at.
func words(line string) ([]string, error) {
	var out []string
	var w []byte
	in := false // inside a word, even an empty quoted one
	var q byte  // the open quote, or 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else {
				w = append(w, c)
			}
		case c == '\'' || c == '"':
			q, in = c, true
		case c == ' ' || c == '\t':
			if in {
				out = append(out, string(w))
				w, in = w[:0], false
			}
		default:
			w, in = append(w, c), true
		}
	}
	if q != 0 {
		return nil, errors.New("a quote is left open")
	}
	if in {
		out = append(out, string(w))
	}
	return out, nil
}
