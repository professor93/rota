package message

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"

	rota "github.com/professor93/rota/lib"
)

// Event is one thing that happened during a run, in rota's own words.
//
// Four CLIs describe the same handful of happenings — the agent said
// something, it wants to use a tool, it was refused, it finished — in four
// unrelated vocabularies. A client should have to learn one. Only what every
// provider has is named here; a provider's own detail stays in Raw, which
// the sender attaches when it was asked for and leaves out otherwise.
type Event struct {
	// Type is one of: text, thinking, tool, tool_result, blocked, usage,
	// input, interrupted, idle, done, error, other. "other" is an event rota
	// recognises as real but has nothing general to say about — it is
	// delivered, not dropped, because a vendor adding an event type must not
	// make one disappear.
	//
	// The three about an open run say what became of what was sent into it:
	// input is one message's progress — state is accepted, answered or
	// failed, with its id, and reason when it failed; interrupted is an
	// interrupt the CLI acknowledged, with its id; idle is every message
	// answered and the run waiting for more.
	Type string `json:"type"`
	// Seq, Account and Provider are stamped by whoever sends the stream,
	// which is the only party that knows them.
	Seq      int    `json:"seq,omitzero"`
	Account  int    `json:"account,omitzero"`
	Provider string `json:"provider,omitempty"`

	SessionID string `json:"session_id,omitempty"`
	// RunID names an open run on the machine rota is on, so another terminal
	// can send into it. It belongs to the opening event; a run that takes no
	// more messages has none.
	RunID string `json:"run_id,omitempty"`

	// Model, Effort and Cwd belong to the opening event: what the run is
	// about to do, before it has done any of it.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Cwd    string `json:"cwd,omitempty"`

	// Text is what was said. Blocks is the same text split into prose and
	// code, present only when the reading was asked for (see With), so a
	// client that wants it need not parse markdown, and one that does not
	// gets the text alone.
	Text   string  `json:"text,omitempty"`
	Blocks []Block `json:"blocks,omitzero"`
	// Delta marks a text or thinking event that is one fragment of a piece
	// still being written, sent only when the run asked for partial
	// messages. The whole piece follows as its own event once it is done, so
	// a client that ignores deltas still sees everything once, and one that
	// shows them must skip the whole it has already shown in parts.
	Delta bool `json:"delta,omitzero"`

	// Tool, ToolID and Reason describe a tool call, its result, or the
	// refusal of it. Input is what the tool was asked to do — the file a
	// Read opened, the command a Bash ran — in the tool's own shape,
	// verbatim: it is the fact a client wants to show, and it is not the
	// same fact for any two tools.
	Tool   string         `json:"tool,omitempty"`
	ToolID string         `json:"tool_id,omitempty"`
	Input  jsontext.Value `json:"input,omitzero"`
	Reason string         `json:"reason,omitempty"`

	// ID and State belong to a message sent into an open run: the id Send
	// gave it, and what became of it — accepted, answered or failed. An
	// interrupted event carries the interrupt's own id and no state.
	ID    string `json:"id,omitempty"`
	State string `json:"state,omitempty"`

	// Usage is the token numbers a usage event was made of, when it was
	// made of any: a limit reading going by has none.
	Usage *Usage `json:"usage,omitzero"`

	// Subagent names the tool call that delegated this piece, when a
	// subagent said it rather than the lead. claude sets it only when asked
	// to forward subagent text; otherwise every event is the lead's.
	Subagent string `json:"subagent,omitempty"`

	// At is when the event arrived, in milliseconds after the stream's
	// first event, present only when timing was asked for.
	At int64 `json:"at,omitzero"`

	// Raw is the provider's own event, verbatim, for a caller that asked to
	// see it. Empty by default: the point of this type is that most clients
	// never need to look.
	Raw jsontext.Value `json:"raw,omitzero"`
}

// Usage is a token reading in one vocabulary. The names are the Anthropic
// API's, which claude's own accounting already uses; codex's
// cached_input_tokens is the same reading as cache_read_input_tokens and
// lands there.
type Usage struct {
	InputTokens              int `json:"input_tokens,omitzero"`
	OutputTokens             int `json:"output_tokens,omitzero"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitzero"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitzero"`
}

// FromNotice is what an open run said about one message, as an event.
//
// lib reports delivery on a channel, in the session's own words; a stream has
// one vocabulary for everything that happens in a run, and this is where the
// two meet. A client watching a run therefore learns what became of each
// message from the stream it is already reading, rather than from a second
// shape arriving somewhere else.
//
// A kind rota has not seen before is delivered as an input event carrying it,
// for the same reason an unknown CLI event becomes "other": a new one must
// not disappear.
func FromNotice(n rota.Notice) Event {
	switch n.Kind {
	case "interrupted":
		return Event{Type: "interrupted", ID: n.ID}
	case "idle":
		return Event{Type: "idle"}
	}
	return Event{Type: "input", ID: n.ID, State: n.Kind, Reason: n.Err}
}

// usage is a provider's own token reading, whichever names it used.
type usage struct {
	Input         int `json:"input_tokens"`
	Output        int `json:"output_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	Cached        int `json:"cached_input_tokens"` // codex
}

// event is the reading in rota's vocabulary, or nothing when there was none.
func (u *usage) event() *Usage {
	if u == nil {
		return nil
	}
	return &Usage{
		InputTokens: u.Input, OutputTokens: u.Output,
		CacheReadInputTokens: u.CacheRead + u.Cached, CacheCreationInputTokens: u.CacheCreation,
	}
}

// wire is every field of every provider's vocabulary that rota reads. One
// struct rather than four, because the names do not collide — except
// "message", which claude uses for an object on one event and a sentence on
// another, so it is kept raw and read per branch.
type wire struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// Claude Code
	SessionID string         `json:"session_id"`
	Message   jsontext.Value `json:"message"`
	ToolName  string         `json:"tool_name"`
	ToolUseID string         `json:"tool_use_id"`
	// ParentToolUseID is set on what a subagent said, naming the call that
	// delegated to it; the lead's own events have none.
	ParentToolUseID string `json:"parent_tool_use_id"`
	// Event is the API's own streaming event, which claude relays whole
	// when partial messages were asked for.
	Event *streamEvent `json:"event"`

	// codex
	ThreadID string `json:"thread_id"`
	Item     *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	// Usage is codex's turn accounting.
	Usage *usage `json:"usage"`

	// grok answers with one object and no type at all
	GrokText   string `json:"text"`
	StopReason string `json:"stopReason"`
}

// streamEvent is one of the API's streaming events: what kind of thing
// happened to the message, and for a fragment, the fragment.
type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
	// Usage rides on message_delta: the message's accounting, once it is
	// complete.
	Usage *usage `json:"usage"`
}

// content is what a claude message holds: a list of pieces, each its own
// kind of happening.
type content struct {
	Content []piece `json:"content"`
}

// piece is one element of a claude message's content.
type piece struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Thinking  string         `json:"thinking"`
	Name      string         `json:"name"`
	ID        string         `json:"id"`
	Input     jsontext.Value `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	Result    string         `json:"content"`
}

// Normalize turns one line of a vendor's output into rota's vocabulary. One
// line can be several events: a claude message carries a list of content,
// and each piece of it is its own happening.
//
// A line that is not JSON, or is JSON saying nothing rota recognises, comes
// back as nothing. Reporting a parse failure as an event would put a
// vendor's debug output into a client's transcript.
func Normalize(raw []byte) []Event {
	var w wire
	if jsonv2.Unmarshal(raw, &w, rota.LenientOptions()) != nil {
		return nil
	}
	session := w.SessionID
	if session == "" {
		session = w.ThreadID
	}
	one := func(kind string) []Event { return []Event{{Type: kind, SessionID: session}} }
	// A subagent's events are named as such; the lead's carry nothing.
	said := func(evs []Event) []Event {
		if w.ParentToolUseID != "" {
			for i := range evs {
				evs[i].Subagent = w.ParentToolUseID
			}
		}
		return evs
	}

	switch {
	case w.Type == "assistant":
		return said(pieces(w.Message, session, assistantPiece))

	case w.Type == "user":
		return said(pieces(w.Message, session, toolResultPiece))

	case w.Type == "stream_event":
		// The API's own streaming events, relayed when partial messages
		// were asked for. A fragment of text or thinking is a delta of the
		// piece it belongs to, which still arrives whole in an assistant
		// message afterwards. Everything else here — a message opening, a
		// block starting or ending, a signature, half a tool call's JSON —
		// is framing around what that message already says, and nothing a
		// client could show: no event at all, rather than noise.
		return said(fragment(w.Event, session))

	case w.Type == "system" && w.Subtype == "permission_denied":
		// Headless CLIs do not ask permission; they refuse and tell the
		// model. This is the only trace a client gets that a tool the agent
		// wanted was not allowed, so it is named rather than buried.
		var why string
		_ = jsonv2.Unmarshal(w.Message, &why, rota.LenientOptions())
		return []Event{{Type: "blocked", Tool: w.ToolName, ToolID: w.ToolUseID,
			Reason: why, SessionID: session}}

	case w.Type == "rate_limit_event", w.Type == "turn.completed":
		// A limit reading has no token numbers; codex's turn does.
		return []Event{{Type: "usage", Usage: w.Usage.event(), SessionID: session}}

	case w.Type == "result":
		// claude repeats its final answer here, having already said it as
		// text, and a run ends exactly once — with rota's own done, which is
		// the only event that knows the exit code. So this is not that.
		return one("other")

	case w.Type == "item.completed" && w.Item != nil && w.Item.Type == "agent_message":
		return []Event{{Type: "text", Text: w.Item.Text, SessionID: session}}

	case w.Type == "turn.failed", w.Type == "error":
		return one("error")

	case w.Type == "" && w.GrokText != "":
		// grok prints one object and no type: the whole answer at once.
		if w.StopReason == "error" {
			return []Event{{Type: "error", Text: w.GrokText}}
		}
		return []Event{{Type: "text", Text: w.GrokText}}

	case w.Type == "":
		return nil
	}
	return one("other")
}

// pieces reads a claude message and turns each piece of its content into an
// event. A message rota can make nothing of is still one event: the run did
// something, and a silent gap in a transcript is worse than a vague entry.
func pieces(msg jsontext.Value, session string, one func(piece, string) (Event, bool)) []Event {
	var c content
	if len(msg) > 0 {
		_ = jsonv2.Unmarshal(msg, &c, rota.LenientOptions())
	}
	var out []Event
	for _, p := range c.Content {
		if ev, ok := one(p, session); ok {
			out = append(out, ev)
		}
	}
	if out == nil {
		return []Event{{Type: "other", SessionID: session}}
	}
	return out
}

// fragment reads one streaming event and returns the delta it carries, if
// it carries one worth showing. An empty fragment is not a happening.
func fragment(e *streamEvent, session string) []Event {
	if e == nil {
		return nil
	}
	if e.Type == "message_delta" {
		// The message is complete, and this is its accounting: the one
		// token reading a run gives while it is still going.
		if u := e.Usage.event(); u != nil {
			return []Event{{Type: "usage", Usage: u, SessionID: session}}
		}
		return nil
	}
	if e.Type != "content_block_delta" {
		return nil
	}
	var kind, text string
	switch e.Delta.Type {
	case "text_delta":
		kind, text = "text", e.Delta.Text
	case "thinking_delta":
		kind, text = "thinking", e.Delta.Thinking
	}
	if text == "" {
		return nil
	}
	return []Event{{Type: kind, Text: text, Delta: true, SessionID: session}}
}

func assistantPiece(p piece, session string) (Event, bool) {
	switch p.Type {
	case "text":
		return Event{Type: "text", Text: p.Text, SessionID: session}, true
	case "thinking":
		return Event{Type: "thinking", Text: p.Thinking, SessionID: session}, true
	case "tool_use":
		// A copy: the line it was read from is a buffer the reader reuses.
		return Event{Type: "tool", Tool: p.Name, ToolID: p.ID, Input: jsontext.Value(bytes.Clone(p.Input)), SessionID: session}, true
	}
	return Event{}, false
}

func toolResultPiece(p piece, session string) (Event, bool) {
	if p.Type != "tool_result" {
		return Event{}, false
	}
	return Event{Type: "tool_result", ToolID: p.ToolUseID, Text: p.Result, SessionID: session}, true
}
