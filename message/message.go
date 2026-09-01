// Package message reads a finished answer: what a vendor CLI actually said,
// turned into something a client can show.
//
// It is deliberately outside lib. lib is the SDK — it speaks each vendor's
// transport, builds their command lines and carries their results, and it
// has no business knowing what markdown is. What an answer looks like on a
// page is a consumer's concern, so it lives with the consumers: api and cmd
// both import this, and lib imports neither.
package message

import "strings"

// Block is one piece of an answer: prose, or a fenced code listing with
// whatever language the fence named.
//
// A vendor CLI answers in markdown, and the interesting part is often the
// code in the middle of it. Splitting it here means every client — a web
// page, a terminal, a log viewer — reads the same division instead of each
// writing its own markdown parser and disagreeing about where code starts.
type Block struct {
	Kind string `json:"kind"` // "text" or "code"
	Lang string `json:"lang,omitempty"`
	Text string `json:"text"`
}

// Blocks splits a markdown answer into prose and fenced code.
//
// Only fences are recognised, because only fences change how a client must
// treat the content: everything else markdown marks up is still prose, and
// a client that wants it rendered has the original text to render. A fence
// may be quoted by a longer one, and an answer cut off mid-fence keeps what
// the fence had opened — a killed run should not lose its last listing.
func Blocks(md string) []Block {
	var out []Block
	var prose []string

	flushProse := func() {
		if text := strings.TrimSpace(strings.Join(prose, "\n")); text != "" {
			out = append(out, Block{Kind: "text", Text: text})
		}
		prose = prose[:0]
	}

	lines := strings.Split(md, "\n")
	for i := 0; i < len(lines); i++ {
		fence, lang := openingFence(lines[i])
		if fence == "" {
			prose = append(prose, lines[i])
			continue
		}
		flushProse()
		var code []string
		i++
		for ; i < len(lines); i++ {
			if closesFence(lines[i], fence) {
				break
			}
			code = append(code, lines[i])
		}
		out = append(out, Block{Kind: "code", Lang: lang, Text: strings.Join(code, "\n")})
	}
	flushProse()
	return out
}

// openingFence reports the run of backticks a line opens a code block with,
// and the language written after it. A line that is not a fence returns "".
func openingFence(line string) (fence, lang string) {
	trimmed := strings.TrimLeft(line, " ")
	n := 0
	for n < len(trimmed) && trimmed[n] == '`' {
		n++
	}
	if n < 3 {
		return "", ""
	}
	// An info string cannot itself contain a backtick, which is what keeps
	// `a` in a sentence from reading as a fence.
	info := strings.TrimSpace(trimmed[n:])
	if strings.Contains(info, "`") {
		return "", ""
	}
	return trimmed[:n], info
}

// closesFence reports whether a line ends a block opened by fence. The
// closing run must be at least as long, so a shorter fence inside a longer
// one is content rather than the end of it.
func closesFence(line, fence string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, fence) {
		return false
	}
	return strings.Trim(trimmed, "`") == ""
}
