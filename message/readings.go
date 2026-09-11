package message

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"slices"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
)

// Tally is what a stream saw go by, kept only for the readings that need
// it: which tools ran and which were refused, which files they named, how
// many events of each kind, how many bytes, and when the first text and the
// first tool call arrived. A Stream fills one when given it; nothing else
// looks at the events twice.
type Tally struct {
	Events    int
	ByType    map[string]int
	Fragments int
	Bytes     int64
	Tools     map[string]int
	Blocked   map[string]int
	Read      []string
	Written   []string
	FirstText time.Duration // zero until the first text, a fragment counting
	FirstTool time.Duration // zero until the first tool call
	Total     time.Duration // from the first event to the end of the stream
	sawText   bool
	sawTool   bool
}

// toolInput is the part of a tool's input that names a file. Tools differ
// in what they call it; these are the names claude's tools use.
type toolInput struct {
	FilePath     string `json:"file_path"`
	Path         string `json:"path"`
	NotebookPath string `json:"notebook_path"`
}

// readers and writers are the tools whose input names a file the agent
// read, or one it changed. A Bash command is neither: what it touched is
// inside a command line rota does not parse.
var (
	readers = []string{"Read", "Glob", "Grep"}
	writers = []string{"Edit", "MultiEdit", "Write", "NotebookEdit"}
)

// add records one event, at a time since the stream began. rota's own
// opening event is the clock's zero, not something the CLI said, so it is
// not counted.
func (t *Tally) add(ev Event, at time.Duration) {
	if ev.Type == "init" {
		return
	}
	t.Events++
	if t.ByType == nil {
		t.ByType = map[string]int{}
	}
	t.ByType[ev.Type]++
	if ev.Delta {
		t.Fragments++
	}
	switch ev.Type {
	case "text":
		if !t.sawText {
			t.sawText, t.FirstText = true, at
		}
	case "tool":
		if !t.sawTool {
			t.sawTool, t.FirstTool = true, at
		}
		if t.Tools == nil {
			t.Tools = map[string]int{}
		}
		t.Tools[ev.Tool]++
		t.file(ev.Tool, ev.Input)
	case "blocked":
		if t.Blocked == nil {
			t.Blocked = map[string]int{}
		}
		t.Blocked[ev.Tool]++
	}
}

// file notes the path a tool's input named, under read or written.
func (t *Tally) file(tool string, input jsontext.Value) {
	if len(input) == 0 {
		return
	}
	var in toolInput
	if jsonv2.Unmarshal(input, &in, rota.LenientOptions()) != nil {
		return
	}
	path := in.FilePath
	if path == "" {
		path = in.NotebookPath
	}
	if path == "" {
		path = in.Path
	}
	if path == "" {
		return
	}
	switch {
	case slices.Contains(writers, tool):
		if !slices.Contains(t.Written, path) {
			t.Written = append(t.Written, path)
		}
	case slices.Contains(readers, tool):
		if !slices.Contains(t.Read, path) {
			t.Read = append(t.Read, path)
		}
	}
}

// Sources is what a caller holds that the result does not, for the readings
// that need it. Any of it may be missing; a reading with nothing to read
// from is left out rather than invented.
type Sources struct {
	Tally     *Tally
	Account   *rota.Account
	Threshold int
	Quota     *rota.Quota
}

// Readings are the optional fields a caller asked for, beside the answer on
// a reply and beside the outcome on a stream's done. Each is present only
// when its reading was named and there was something to read.
type Readings struct {
	Code         []Code         `json:"code,omitzero"`
	Files        *Files         `json:"files,omitzero"`
	Timing       *Timing        `json:"timing,omitzero"`
	Quota        *rota.Quota    `json:"quota,omitzero"`
	Tools        map[string]int `json:"tools,omitzero"`
	Blocked      map[string]int `json:"blocked,omitzero"`
	Stats        *Stats         `json:"stats,omitzero"`
	Plain        string         `json:"plain,omitempty"`
	Links        []string       `json:"links,omitzero"`
	AccountLabel string         `json:"account_label,omitempty"`
	Order        int            `json:"order,omitzero"`
	Threshold    int            `json:"threshold,omitzero"`
}

// Code is one fenced listing: the language the fence named, and the text.
type Code struct {
	Lang string `json:"lang,omitempty"`
	Text string `json:"text"`
}

// Files are the paths the agent's tools named, in first-seen order.
type Files struct {
	Read    []string `json:"read,omitzero"`
	Written []string `json:"written,omitzero"`
}

// Timing is when things first happened, in milliseconds from the start.
type Timing struct {
	FirstTextMS int64 `json:"first_text_ms,omitzero"`
	FirstToolMS int64 `json:"first_tool_ms,omitzero"`
	TotalMS     int64 `json:"total_ms,omitzero"`
}

// Stats is how much went by.
type Stats struct {
	Events    int            `json:"events"`
	ByType    map[string]int `json:"by_type,omitzero"`
	Fragments int            `json:"fragments,omitzero"`
	Bytes     int64          `json:"bytes,omitzero"`
	Truncated bool           `json:"truncated,omitzero"`
}

// Read derives what w asked for from a finished result and the sources.
func Read(res *rota.Result, w With, src Sources) Readings {
	var r Readings
	text := ""
	if res != nil {
		text = res.Result
	}
	if w.Code {
		for _, b := range Blocks(text) {
			if b.Kind == "code" {
				r.Code = append(r.Code, Code{Lang: b.Lang, Text: b.Text})
			}
		}
	}
	if w.Plain {
		r.Plain = plain(text)
	}
	if w.Links {
		r.Links = links(text)
	}
	if t := src.Tally; t != nil {
		if w.Files && (len(t.Read) > 0 || len(t.Written) > 0) {
			r.Files = &Files{Read: t.Read, Written: t.Written}
		}
		if w.Tools {
			r.Tools, r.Blocked = t.Tools, t.Blocked
		}
		if w.Stats {
			r.Stats = &Stats{Events: t.Events, ByType: t.ByType, Fragments: t.Fragments, Bytes: t.Bytes}
			if res != nil {
				r.Stats.Truncated = res.Truncated
			}
		}
		if w.Timing {
			// One clock for all three: from the stream's first event. A
			// stream that never ended (a buffered run reads its events
			// through a quiet stream) falls back to the child's duration.
			r.Timing = &Timing{FirstTextMS: t.FirstText.Milliseconds(), FirstToolMS: t.FirstTool.Milliseconds(), TotalMS: t.Total.Milliseconds()}
			if r.Timing.TotalMS == 0 && res != nil {
				r.Timing.TotalMS = res.DurationMS
			}
		}
	} else if w.Timing && res != nil {
		// A buffered run has no events to time; the total is still known.
		r.Timing = &Timing{TotalMS: res.DurationMS}
	}
	if w.Quota && src.Quota != nil {
		r.Quota = src.Quota
	}
	if w.Account && src.Account != nil {
		r.AccountLabel = src.Account.String()
		r.Order = src.Account.Order
		r.Threshold = src.Threshold
	}
	return r
}

// plain flattens markdown for a place that will not render it: headings
// become their text, emphasis marks go, a link becomes its text with the
// address in parentheses, and a fence keeps its content without the fence
// lines. Tables and nested lists are left as they are. Lossy by design.
func plain(md string) string {
	var out []string
	for _, b := range Blocks(md) {
		if b.Kind == "code" {
			out = append(out, b.Text)
			continue
		}
		for line := range strings.SplitSeq(b.Text, "\n") {
			trimmed := strings.TrimLeft(line, "#")
			if trimmed != line && strings.HasPrefix(trimmed, " ") {
				line = strings.TrimSpace(trimmed)
			}
			line = unlink(line)
			for _, mark := range []string{"**", "__", "~~"} {
				line = strings.ReplaceAll(line, mark, "")
			}
			line = stripSingle(line, '*')
			line = stripSingle(line, '_')
			line = strings.ReplaceAll(line, "`", "")
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// unlink turns [text](url) into "text (url)".
func unlink(line string) string {
	for {
		open := strings.Index(line, "[")
		if open < 0 {
			return line
		}
		close := strings.Index(line[open:], "](")
		if close < 0 {
			return line
		}
		end := strings.Index(line[open+close:], ")")
		if end < 0 {
			return line
		}
		text := line[open+1 : open+close]
		url := line[open+close+2 : open+close+end]
		line = line[:open] + text + " (" + url + ")" + line[open+close+end+1:]
	}
}

// stripSingle removes a lone emphasis mark around a word, leaving marks
// that are part of the text — a bullet's "* " or a bare asterisk — alone.
func stripSingle(line string, mark byte) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c != mark {
			b.WriteByte(c)
			continue
		}
		prev := i > 0 && line[i-1] != ' '
		next := i+1 < len(line) && line[i+1] != ' '
		if prev || next {
			continue // a mark hugging a word is emphasis
		}
		b.WriteByte(c)
	}
	return b.String()
}

// links are the URLs in the prose, in order and once each: the address of
// a markdown link, and any bare http(s) address. Code is not read.
func links(md string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimRight(u, ".,;:!?'\"")
		// A closing parenthesis belongs to the address only when it
		// balances an opening one inside it.
		for strings.HasSuffix(u, ")") && strings.Count(u, "(") < strings.Count(u, ")") {
			u = strings.TrimSuffix(u, ")")
		}
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, b := range Blocks(md) {
		if b.Kind != "text" {
			continue
		}
		text := b.Text
		for {
			i := strings.Index(text, "http://")
			if j := strings.Index(text, "https://"); j >= 0 && (i < 0 || j < i) {
				i = j
			}
			if i < 0 {
				break
			}
			end := strings.IndexAny(text[i:], " \t\n<>\"'`]")
			if end < 0 {
				end = len(text) - i
			}
			// A markdown link's closing ")" is trimmed by add, which keeps
			// a ")" only when the address opened one.
			add(text[i : i+end])
			text = text[i+end:]
		}
	}
	return out
}
