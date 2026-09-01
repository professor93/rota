package message

import "strings"

// Ask is rota's reading of an answer that ends by asking the user something.
//
// It is inference over prose, and it is labelled as such because a headless
// CLI gives nothing better. In an interactive session these arrive as real
// structures — a permission prompt, a question with radio buttons, a text
// box — but none of that survives the headless interface: the tool that asks
// structured questions is not even offered there, so the model asks in
// sentences like anyone else. What can be read reliably is read; what cannot
// is left alone rather than guessed at.
type Ask struct {
	// Kind is "choice" when the answer listed what to pick from, and "text"
	// when it asked in prose and expects a sentence back.
	Kind string `json:"kind"`
	// Question is the sentence that was asked.
	Question string `json:"question"`
	// Options are the listed choices, in the order they were written. Empty
	// for a text question — rota does not split a sentence into options,
	// because "use foo or bar?" would become two choices nobody offered.
	Options []string `json:"options,omitempty"`
	// Multiple says the list was written as a task list, markdown's way of
	// saying more than one may be picked.
	Multiple bool `json:"multiple,omitzero"`
}

// Asked reports the question an answer ends with, or nil when it ends with
// an answer.
//
// Only the last of the prose is read: an agent that asked something early
// and then carried on was not waiting for a reply. Code is not read at all —
// a list inside a fence is a listing, not a menu — but an answer that asks
// and then shows the command it means is still asking.
func Asked(md string) *Ask {
	blocks := Blocks(md)
	last := -1
	for i, b := range blocks {
		if b.Kind == "text" {
			last = i
		}
	}
	if last < 0 {
		return nil
	}

	lines := strings.Split(blocks[last].Text, "\n")
	options, multiple := listItems(lines)
	question := lastQuestion(lines, len(options) > 0)
	if question == "" {
		return nil
	}
	if len(options) == 0 {
		return &Ask{Kind: "text", Question: question}
	}
	return &Ask{Kind: "choice", Question: question, Options: options, Multiple: multiple}
}

// lastQuestion is the sentence still waiting for an answer: the final one
// ending in a question mark, or — when choices were listed — whatever
// introduced them, since "Pick one:" is a question however it is punctuated.
//
// Anything else after the asking means the agent answered itself.
func lastQuestion(lines []string, listed bool) string {
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || isListItem(line) {
			continue
		}
		if !listed && !strings.HasSuffix(line, "?") {
			return ""
		}
		return lastSentence(line)
	}
	return ""
}

// lastSentence trims everything before the final sentence, so a paragraph
// that explains itself and then asks reports only the asking.
func lastSentence(line string) string {
	if i := strings.LastIndexAny(strings.TrimRight(line, "?:"), ".!"); i >= 0 {
		return strings.TrimSpace(line[i+1:])
	}
	return line
}

// listItems reads the choices written under a question. A task list means
// more than one may be picked; a plain list means one.
func listItems(lines []string) (items []string, multiple bool) {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !isListItem(line) {
			continue
		}
		text := strings.TrimSpace(strings.TrimLeft(line, "-*0123456789.)"))
		// "- [ ] thing" and "- [x] thing": markdown's way of saying more
		// than one of these may be picked.
		if strings.HasPrefix(text, "[") {
			if end := strings.Index(text, "]"); end == 1 || end == 2 {
				multiple = true
				text = strings.TrimSpace(text[end+1:])
			}
		}
		if text != "" {
			items = append(items, text)
		}
	}
	return items, multiple
}

func isListItem(line string) bool {
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return true
	}
	// "1. thing", "12) thing"
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	return digits > 0 && digits < len(line) && (line[digits] == '.' || line[digits] == ')')
}
