package client

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/professor93/rota/api"
)

// me is the principal every in-process handler here is served to.
var me = api.Principal{Name: "me", Role: api.RoleControl, Via: "user"}

// A handler reached in process answers as it would over ctl.sock, to the
// principal it was mounted for; once stopped, nothing reaches it, and
// stopping twice is stopping once.
func TestInProcessServesTheHandler(t *testing.T) {
	srv, err := api.New(api.Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1, Routes: &api.Routes{API: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	c, stop := InProcess(srv.LocalHandler(&me))
	t.Cleanup(stop)

	resp, err := c.HTTP().Get("http://rota/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var who struct{ Name, Role, Via string }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &who) != nil ||
		who.Name != "me" || who.Role != "control" || who.Via != "user" {
		t.Fatalf("GET /v1/session in process: %d %s, want me, control, user", resp.StatusCode, raw)
	}

	stop()
	if resp, err := c.HTTP().Get("http://rota/v1/session"); err == nil {
		resp.Body.Close()
		t.Fatalf("a request after stop was answered %d", resp.StatusCode)
	}
	stop()
}
