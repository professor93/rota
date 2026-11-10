package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// A usage endpoint that gives a window no figure has given no reading: the
// account keeps the reading it had, dated when it was taken, the listing says
// why on stderr, and nothing reads as 0%. An account that had no reading
// still has none.
func TestAReadingWithNoFigureKeepsTheLastOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"five_hour":{"utilization":null,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":null}}`)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Usage
	rota.ClaudeEndpoints.Usage = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Usage = old })
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	taken := time.Now().Add(-2 * time.Hour).UnixMilli()
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"read@x","order":1,"quotaAt":%d,"token":{"accessToken":"t1"},
		 "quota":{"windows":[{"name":"5h","percent":96,"primary":true},{"name":"7d","percent":30}]}},
		{"id":2,"provider":"claude","email":"never@x","order":2,"token":{"accessToken":"t2"}}]}`, taken))

	out, errOut, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "│") {
			continue
		}
		if strings.Contains(line, "read@x") && (!strings.Contains(line, "5h 96%") || !strings.Contains(line, "2h ago")) {
			t.Fatalf("the last reading stays, with its age: %q\n%s", line, out)
		}
		if strings.Contains(line, "never@x") && (!strings.Contains(line, "│ -") || !strings.Contains(line, "never")) {
			t.Fatalf("no reading is still no reading: %q\n%s", line, out)
		}
	}
	if strings.Contains(out, " 0%") {
		t.Fatalf("nothing reads as 0%%:\n%s", out)
	}
	if !strings.Contains(errOut, "no figure") || !strings.Contains(errOut, "five_hour") {
		t.Fatalf("stderr says why there is no new reading: %q", errOut)
	}
}
