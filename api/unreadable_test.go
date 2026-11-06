//go:build linux || darwin

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// These are about the running holders a daemon cannot read — another
// rota's, or one whose record cannot be parsed — and about stopping them,
// which only a DELETE ever does. The harness is adopt_test.go's.

// A holder whose record cannot be read at all is running all the same: its
// lock says so. It is listed, counted under max_sessions, and answered
// for; a DELETE signals nothing, since nothing says which process it is.
func TestAHolderWhoseRecordCannotBeReadIsListed(t *testing.T) {
	p := withHeld(t, nil, func(o *Options) { o.Terminal.MaxSessions = 1 })
	id := runID()
	dir := holdwire.Dir(p.terminals(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := holdwire.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if err := os.WriteFile(holdwire.MetaPath(dir), []byte("{not a record"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := p.list()[id]; !ok {
		t.Fatal("a running holder whose record cannot be read is not listed")
	}
	wants(t, "a holder whose record cannot be read", p.describe(id), map[string]any{
		"held": true, "state": "live", "status": "running", "ended": false, "attachable": false, "made_by": "unknown",
	})
	if resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": 1}); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(string(raw), "holding 1 terminals") {
		t.Errorf("starting another past max_sessions answers %d %s", resp.StatusCode, raw)
	}
	if resp, raw := p.do("GET", "/v1/terminals/"+id+"/ws", nil); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(string(raw), "has a record this rota cannot read") {
		t.Errorf("attaching answers %d %s", resp.StatusCode, raw)
	}
	if resp, raw := p.do("DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(string(raw), "names no process this rota can check; nothing was signalled") {
		t.Errorf("DELETE answers %d %s", resp.StatusCode, raw)
	}
	for range 2 {
		p.api.sweepRecords()
	}
	if !exists(holdwire.MetaPath(dir)) {
		t.Fatal("a sweep took the record of a running holder")
	}
}

// One whose record cannot be read whole, but still says which process it
// is — a later rota's, with a field this one reads differently — is
// stopped by a DELETE, after the same check as any other.
func TestAHolderWhoseRecordIsPartlyReadableIsStoppable(t *testing.T) {
	p := withHeld(t, nil)
	signals := filepath.Join(t.TempDir(), "signals")
	id, pid := holdOutside(t, p.dir, "FAKE_HOLDER_SIGNALS="+signals)
	path := holdwire.MetaPath(holdwire.Dir(p.terminals(), id))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["account"], doc["version"] = map[string]any{"id": 1}, "10.0.0"
	if raw, err = json.Marshal(doc); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	wants(t, "a holder whose record is a later rota's", p.describe(id), map[string]any{
		"state": "live", "attachable": false, "made_by": "10.0.0", "pid": float64(pid),
	})
	if resp, raw := p.do("DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE answers %d %s", resp.StatusCode, raw)
	}
	p.waitFor(id, 5*time.Second, func(d map[string]any) bool { return d["state"] == "ended" })
	if raw, _ := os.ReadFile(signals); strings.TrimSpace(string(raw)) != "hangup" {
		t.Fatalf("the holder was sent %q, want one hangup", raw)
	}
}

// However many times a DELETE is asked of a holder this daemon is not
// linked to, it is hung up once and killed once.
func TestRepeatedDeletesHangUpOnce(t *testing.T) {
	wasKill, wasLater := termKillAfter, holdKillLater
	termKillAfter, holdKillLater = 500*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { termKillAfter, holdKillLater = wasKill, wasLater })
	p := withHeld(t, nil)
	signals := filepath.Join(t.TempDir(), "signals")
	id, pid := holdOutside(t, p.dir, "FAKE_HOLDER_MAJOR=9", "FAKE_HOLDER_DEAF=1", "FAKE_HOLDER_SIGNALS="+signals)
	for range 3 {
		if resp, raw := p.do("DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("DELETE answers %d %s", resp.StatusCode, raw)
		}
	}
	p.waitFor(id, 10*time.Second, func(d map[string]any) bool { return d["ended"] == true })
	for deadline := time.Now().Add(5 * time.Second); alive(pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the holder outlived its kill")
		}
	}
	if raw, _ := os.ReadFile(signals); string(raw) != "hangup\n" {
		t.Fatalf("three DELETEs sent %q, want one hangup", raw)
	}
	if resp, raw := p.do("DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a DELETE once it has ended answers %d %s", resp.StatusCode, raw)
	}
}
