package hold

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// Registry is what Claude Code says about one of its sessions: which
// conversation it is in, what that conversation is called, the folder it
// really works in, and whether it is idle or waiting for the person. It is
// the protocol's own type, so what is read is what the daemon is told.
// UpdatedAt is the file's updatedAt as written: milliseconds since the epoch.
type Registry = holdwire.Registry

// registryFile is one <home>/sessions/<pid>.json read: what it says about
// the session, and which process says it wrote it.
type registryFile struct {
	Registry
	PID int
	// ProcStart is the writer's own start, zero when the file gave none in
	// the layout Claude Code writes it in.
	ProcStart time.Time
}

// registrySlack is how far a file's procStart may be from the child's start
// and still be the child's. procStart holds whole seconds and the child's
// start may not, so a second either way is the reading's own error; a pid
// reused by another claude later is minutes or hours off.
const registrySlack = time.Second

// waitingUnknown is what a waitingFor nobody can name reads as. The file
// says the session waits, so it waits: Enter is never typed at a prompt rota
// failed to recognise.
const waitingUnknown = "unknown"

// maxField bounds each string a registry file gives, as the scanner bounds a
// title. Claude Code writes a few dozen bytes in each; a longer one is not
// believed, so that the State every link begins with always fits in a frame,
// even with each string at the bound and written six bytes to the byte.
const maxField = 4096

// registryPath is where Claude Code keeps the record of the session running
// as pid in an account's home.
func registryPath(home string, pid int) string {
	return filepath.Join(home, "sessions", strconv.Itoa(pid)+".json")
}

// parseRegistry reads one registry file. The file is undocumented, so each
// key is read on its own and one that is missing, or not what Claude Code
// 2.1.318 writes there, leaves only its own field empty, as does a string
// longer than maxField. Only a file that is
// not one JSON object — one caught half written — is an error, so the caller
// keeps what it last knew. loc is the zone the writer's procStart is in:
// Claude Code writes it in its own local time, so the caller passes the zone
// of the child's TZ, or time.Local when that names none.
func parseRegistry(b []byte, loc *time.Location) (registryFile, error) {
	if loc == nil {
		loc = time.Local
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return registryFile{}, err
	}
	if m == nil {
		return registryFile{}, errors.New("a session registry file holds no JSON object")
	}
	f := registryFile{
		Registry: Registry{
			SessionID:  str(m["sessionId"]),
			Name:       str(m["name"]),
			Cwd:        str(m["cwd"]),
			Status:     str(m["status"]),
			WaitingFor: waitingFor(m["waitingFor"]),
			Kind:       str(m["kind"]),
			UpdatedAt:  number(m["updatedAt"]),
		},
		PID: int(number(m["pid"])),
	}
	// procStart is a wall clock with no zone. belongsTo compares wall clocks,
	// so the hour a clock goes back cannot shift the reading.
	if t, err := time.ParseInLocation(time.ANSIC, str(m["procStart"]), loc); err == nil {
		f.ProcStart = t
	}
	return f, nil
}

// belongsTo says whether the file is the one written by the child that is
// pid and started at start: its procStart is within registrySlack of that
// start, and it names no other pid. The two starts are compared as wall
// clocks in the file's zone, as the file wrote them, so a start in the
// repeated hour of a clock going back matches in either of its two offsets.
func (f registryFile) belongsTo(pid int, start time.Time) bool {
	if f.ProcStart.IsZero() || start.IsZero() || (f.PID != 0 && f.PID != pid) {
		return false
	}
	d := wall(f.ProcStart).Sub(wall(start.In(f.ProcStart.Location())))
	return d >= -registrySlack && d <= registrySlack
}

// wall is t's clock reading in its own zone, as a time in UTC, so that two
// readings differ by what the clocks say whatever the zone did between them.
func wall(t time.Time) time.Time {
	y, mo, d := t.Date()
	h, mi, sec := t.Clock()
	return time.Date(y, mo, d, h, mi, sec, t.Nanosecond(), time.UTC)
}

// str is a JSON string's value, and empty for anything else and for a
// string longer than maxField.
func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil || len(s) > maxField {
		return ""
	}
	return s
}

// number is a non-negative JSON number's whole part, and zero for anything
// else.
func number(raw json.RawMessage) int64 {
	var n float64
	if json.Unmarshal(raw, &n) != nil || n < 0 || n >= math.MaxInt64 {
		return 0
	}
	return int64(n)
}

// waitingFor names what the session waits for: a string as written, an
// object by its type or else its kind, nothing for null, false or an empty
// string, and waitingUnknown for any other answer, a name longer than
// maxField among them.
func waitingFor(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch w := v.(type) {
	case nil:
		return ""
	case bool:
		if !w {
			return ""
		}
	case string:
		if len(w) <= maxField {
			return w
		}
	case map[string]any:
		for _, k := range []string{"type", "kind"} {
			if s, ok := w[k].(string); ok && s != "" {
				if len(s) > maxField {
					break
				}
				return s
			}
		}
	}
	return waitingUnknown
}
