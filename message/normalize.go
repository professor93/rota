package message

import (
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
	// done, error, other. "other" is an event rota recognises as real but
	// has nothing general to say about — it is delivered, not dropped,
	// because a vendor adding an event type must not make one disappear.
	Type string `json:"type"`
	// Seq, Account and Provider are stamped by whoever sends the stream,
	// which is the only party that knows them.
	Seq      int    `json:"seq,omitzero"`
	Account  int    `json:"account,omitzero"`
	Provider string `json:"provider,omitempty"`

	SessionID string `json:"session_id,omitempty"`

	// Model, Effort and Cwd belong to the opening event: what the run is
	// about to do, before it has done any of it.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Cwd    string `json:"cwd,omitempty"`

	// Text is what was said; Blocks is the same text split into prose and
	// code, so a client need not parse markdown to show them differently.
	Text   string  `json:"text,omitempty"`
	Blocks []Block `json:"blocks,omitzero"`

	// Tool, ToolID and Reason describe a tool call, its result, or the
	// refusal of it.
	Tool   string `json:"tool,omitempty"`
	ToolID string `json:"tool_id,omitempty"`
	Reason string `json:"reason,omitempty"`

	// Raw is the provider's own event, verbatim, for a caller that asked to
	// see it. Empty by default: the point of this type is that most clients
	// never need to look.
	Raw jsontext.Value `json:"raw,omitzero"`
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

	// codex
	ThreadID string `json:"thread_id"`
	Item     *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`

	// grok answers with one object and no type at all
	GrokText   string `json:"text"`
	StopReason string `json:"stopReason"`
}

// content is what a claude message holds: a list of pieces, each its own
// kind of happening.
type content struct {
	Content []piece `json:"content"`
}

// piece is one element of a claude message's content.
type piece struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
	Result    string `json:"content"`
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

	switch {
	case w.Type == "assistant":
		return pieces(w.Message, session, assistantPiece)

	case w.Type == "user":
		return pieces(w.Message, session, toolResultPiece)

	case w.Type == "system" && w.Subtype == "permission_denied":
		// Headless CLIs do not ask permission; they refuse and tell the
		// model. This is the only trace a client gets that a tool the agent
		// wanted was not allowed, so it is named rather than buried.
		var why string
		_ = jsonv2.Unmarshal(w.Message, &why, rota.LenientOptions())
		return []Event{{Type: "blocked", Tool: w.ToolName, ToolID: w.ToolUseID,
			Reason: why, SessionID: session}}

	case w.Type == "rate_limit_event", w.Type == "turn.completed":
		return one("usage")

	case w.Type == "result":
		// claude repeats its final answer here, having already said it as
		// text, and a run ends exactly once — with rota's own done, which is
		// the only event that knows the exit code. So this is not that.
		return one("other")

	case w.Type == "item.completed" && w.Item != nil && w.Item.Type == "agent_message":
		return []Event{{Type: "text", Text: w.Item.Text, Blocks: Blocks(w.Item.Text), SessionID: session}}

	case w.Type == "turn.failed", w.Type == "error":
		return one("error")

	case w.Type == "" && w.GrokText != "":
		// grok prints one object and no type: the whole answer at once.
		if w.StopReason == "error" {
			return []Event{{Type: "error", Text: w.GrokText}}
		}
		return []Event{{Type: "text", Text: w.GrokText, Blocks: Blocks(w.GrokText)}}

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

func assistantPiece(p piece, session string) (Event, bool) {
	switch p.Type {
	case "text":
		return Event{Type: "text", Text: p.Text, Blocks: Blocks(p.Text), SessionID: session}, true
	case "thinking":
		return Event{Type: "thinking", Text: p.Thinking, SessionID: session}, true
	case "tool_use":
		return Event{Type: "tool", Tool: p.Name, ToolID: p.ID, SessionID: session}, true
	}
	return Event{}, false
}

func toolResultPiece(p piece, session string) (Event, bool) {
	if p.Type != "tool_result" {
		return Event{}, false
	}
	return Event{Type: "tool_result", ToolID: p.ToolUseID, Text: p.Result, SessionID: session}, true
}
