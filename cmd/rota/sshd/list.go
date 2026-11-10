package sshd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/professor93/rota/cmd/rota/attach"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/holdwire"
)

// clientFor is a client of the API as the channel's person, in process:
// the connection's Handler with no socket between, reached as ctl.sock
// reaches the daemon's. The func lets it go, WebSockets and all, and so
// does the end of ctx, the command's: a request under way when the channel
// closes or the server stops ends then, rather than holding the command,
// its places and Server.Close until the API answers.
func clientFor(ctx context.Context, ch *Channel) (*client.Client, func()) {
	h := ch.Handler
	if h == nil {
		// A server started with no API refuses every request, rather than
		// have net/http serve its own default handler in its place.
		h = http.NotFoundHandler()
	}
	c, stop := client.InProcess(h)
	after := context.AfterFunc(ctx, stop)
	return c, func() {
		after()
		stop()
	}
}

// listTakes is said, before usage, of a list given anything but --json.
const listTakes = "list takes --json and nothing else\n"

// listRun is `list [--json]`: every session the daemon holds or lists, one
// line each, or none; with --json, {"terminals":[…]} holding each session's
// own document, as GET /v1/terminals answers.
func listRun(ctx context.Context, ch *Channel, args []string) int {
	asJSON := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--json":
		asJSON = true
	default:
		dispatchText(ch.Err, ch.PTY, listTakes+usage)
		return 2
	}
	c, stop := clientFor(ctx, ch)
	defer stop()
	all, err := c.List()
	if err != nil {
		listFailed(ch, err)
		return 1
	}
	if asJSON {
		return listJSON(ch, struct {
			Terminals []client.Session `json:"terminals"`
		}{all})
	}
	if len(all) == 0 {
		dispatchText(ch.Out, ch.PTY, "no sessions\n")
		return 0
	}
	var b strings.Builder
	for _, s := range all {
		b.WriteString(listLine(s))
		b.WriteByte('\n')
	}
	dispatchText(ch.Out, ch.PTY, b.String())
	return 0
}

// listLine is one session as list prints it (Ruling 12): its id; how it is,
// its status, or once it has stopped its state; its name; its provider and
// account; and its folder. The values are two spaces apart, each made one
// line by attach.OneLine, and one nobody gave is "-".
//
// A terminal the server runs itself, rather than a held one, says no status
// and no name: it is running until it has ended, and is called by its
// label, its folder or its id, the order the daemon names a held one by.
func listLine(s client.Session) string {
	how := s.Status
	switch {
	case s.Ended != nil:
		how = s.State
		if how == "" {
			how = "stopped"
		}
	case how == "":
		how = "running"
	}
	who := "-"
	switch {
	case s.Provider != "" || s.AccountLabel != "":
		who = s.Provider + "/" + s.AccountLabel
	case s.Kind == holdwire.KindShell:
		who = holdwire.KindShell
	}
	cwd := s.Cwd
	if cwd == "" {
		cwd = "-"
	}
	vals := []string{s.ID, how, listName(s), who, cwd}
	for i, v := range vals {
		vals[i] = attach.OneLine(v)
	}
	return strings.Join(vals, "  ")
}

// listName is what a session is called: the name the daemon gives a held
// one, and for one the server runs itself, which has none, its label, its
// folder or its id.
func listName(s client.Session) string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Label != "":
		return s.Label
	case s.Cwd != "":
		return filepath.Base(s.Cwd)
	}
	return s.ID
}

// listJSON prints v as JSON on the channel's stdout, exit 0, with every
// control character escaped: encoding/json escapes C0 but writes C1 as it
// is, and a terminal shown U+009B or U+009D acts on it as on ESC [ or
// ESC ]. The escape is JSON's own, so what is read back is the same.
func listJSON(ch *Channel, v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		listFailed(ch, err)
		return 1
	}
	var b bytes.Buffer
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if unicode.IsControl(r) {
			// Marshal's output holds no whitespace between its values, so
			// any control is inside a string, where this is its escape.
			fmt.Fprintf(&b, `\u%04x`, r)
		} else {
			b.Write(raw[i : i+size])
		}
		i += size
	}
	b.WriteByte('\n')
	dispatchText(ch.Out, ch.PTY, b.String())
	return 0
}

// listFailed says on the channel's stderr why a request to the API failed:
// a refusal in the API's own sentence, which is its Error, and anything
// else as it is, on one line.
func listFailed(ch *Channel, err error) {
	dispatchText(ch.Err, ch.PTY, attach.OneLine(err.Error())+"\n")
}
