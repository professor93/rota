package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/professor93/rota/api"
	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/cmd/rota/notify"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/wire"
)

// The daemon's events as server.toml's [[events.sink]] sends them: started
// with the daemon, given until eventsDrain to finish as it stops, shown by
// `rota serve status` and tried by its --test. Only a daemon sends them; a
// foreground serve says so (eventNotes) and sends none. A file with no sink
// changes nothing anywhere: no sinks.json, no line in the log or the status.

// eventsDrain is how long, from the start of its shutdown, a daemon waits
// for its sinks to finish before it lets go of the store: daemon.stopped is
// sent in that time, or not at all. A variable so a test can shorten it.
var eventsDrain = 10 * time.Second

// sinkTestWithin is how long `serve status --test` waits for each sink.
const sinkTestWithin = 20 * time.Second

// startEvents starts the sinks cfg names, sending what the daemon serving
// terminals publishes; nil when it names none, and then a sinks.json an
// earlier daemon left is taken away, so that nothing says a sink runs. A
// command sink's program never sees ROTA_TOKEN or the variable
// auth.token_env names.
func startEvents(cfg *api.Config, terminals string, log *slog.Logger) *notify.Notifier {
	if cfg == nil || len(cfg.Events.Sinks) == 0 {
		_ = os.Remove(notify.StatusPath(terminals))
		return nil
	}
	hide := []string{"ROTA_TOKEN"}
	if name := cfg.Auth.TokenEnv; name != "" && name != "ROTA_TOKEN" {
		hide = append(hide, name)
	}
	names := sinkNames(cfg)
	log.Info(fmt.Sprintf("events: %s: %s", sinksN(len(names)), strings.Join(names, ", ")))
	n, err := notify.Start(notify.Options{Events: cfg.Events, Terminals: terminals, Log: log, Version: wire.Version, HideEnv: hide})
	if err != nil {
		log.Warn("events: no sink runs: " + unsecret(err.Error()))
		return nil
	}
	return n
}

// sinkNames is what each sink cfg names is called, as notify calls it.
func sinkNames(cfg *api.Config) []string {
	if cfg == nil {
		return nil
	}
	var names []string
	for _, s := range notify.Describe(cfg.Events) {
		names = append(names, s.Name)
	}
	return names
}

// sinksN is "1 sink" or "n sinks".
func sinksN(n int) string {
	if n == 1 {
		return "1 sink"
	}
	return fmt.Sprintf("%d sinks", n)
}

// eventsTo is what the line saying a daemon started adds when it sends
// events: "; events to telegram, webhook", or nothing.
func eventsTo(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "; events to " + oneLine(strings.Join(names, ", "))
}

// eventNotes is what a foreground serve says of [events]: sinks are the
// daemon's to send, so a file that names any is told it sends none here.
func eventNotes(cfg *api.Config) []string {
	if cfg == nil || len(cfg.Events.Sinks) == 0 {
		return nil
	}
	return []string{"events: [[events.sink]] is sent by the rota daemon (rota serve start); this foreground server sends none"}
}

// statusSinks is each sink the daemon pid runs, as its sinks.json says: nil
// when the file is not there, or another process wrote it.
func statusSinks(terminals string, pid int) []notify.Status {
	f, err := notify.ReadStatus(terminals)
	if err != nil || f == nil || pid == 0 || f.PID != pid {
		return nil
	}
	return f.Sinks
}

// sinkLine is one sink in serve status: "events: telegram ok, 12 sent, last
// 3m ago", "events: webhook failing since 10:14: webhook answered 502 Bad
// Gateway", "events: command waiting". Drops are said where there are any.
// What sinks.json holds was cleaned of secrets by the daemon, and is drawn
// here with a space for each control character in it (see oneLine).
func sinkLine(s notify.Status, now time.Time) string {
	line := "events: " + oneLine(s.Name) + " " + oneLine(s.State)
	switch s.State {
	case "ok":
		line += fmt.Sprintf(", %d sent", s.Sent)
		if s.Dropped > 0 {
			line += fmt.Sprintf(", %d dropped", s.Dropped)
		}
		if !s.LastSent.IsZero() {
			line += ", last " + agoFrom(now.Sub(s.LastSent))
		}
	case "failing":
		if !s.LastErrorAt.IsZero() {
			line += " since " + clockAt(s.LastErrorAt, now)
		}
		if s.LastError != "" {
			line += ": " + oneLine(s.LastError)
		}
	}
	return line
}

// agoFrom is how long ago something was, d before now: "5s ago", "3m ago".
func agoFrom(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours())/24)
}

// clockAt is t on this machine's clock: its time of day when it was today,
// else its date as well.
func clockAt(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// testSinks is `rota serve status --test SINK`: a test event to the sink so
// called, or to every sink for "all", through the daemon serving terminals,
// and what each did, a line each or as JSON. It exits 0 only when every
// sink delivered it, and 1 otherwise or when no sink runs.
func (c *cli) testSinks(terminals, sink string) error {
	var running []notify.Status
	if d, ok := holdwire.RunningDaemon(terminals); ok {
		running = statusSinks(terminals, d.PID)
	}
	if len(running) == 0 {
		return errors.New("no event sinks are running; [[events.sink]] in server.toml adds one")
	}
	if sink == "all" {
		sink = ""
	}
	cl, err := rotaclient.Dial(terminals)
	if err != nil {
		return err
	}
	outs, err := notify.Test(cl, terminals, sink, sinkTestWithin)
	if err != nil {
		return err
	}
	if c.json {
		if err := c.emit(outs); err != nil {
			return err
		}
	} else {
		for _, o := range outs {
			fmt.Fprintln(c.out, outcomeLine(o))
		}
	}
	for _, o := range outs {
		if !o.Answered || !o.OK {
			return exitCode(1)
		}
	}
	return nil
}

// outcomeLine is one sink's answer to a test: "webhook: delivered",
// "webhook: failed: <why>", "webhook: no answer within 20s".
func outcomeLine(o notify.Outcome) string {
	name := oneLine(o.Sink)
	switch {
	case !o.Answered:
		return fmt.Sprintf("%s: no answer within %s", name, sinkTestWithin)
	case o.OK:
		return name + ": delivered"
	}
	return name + ": failed: " + oneLine(o.Error)
}
