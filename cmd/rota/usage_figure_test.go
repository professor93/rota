package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// usageBy stands in for the usage endpoint, answering each bearer token
// with its own body, and counts what it was asked.
func usageBy(t *testing.T, bodies map[string]string) func() int {
	t.Helper()
	var (
		mu    sync.Mutex
		asked int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		b, ok := bodies[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, b)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Usage
	rota.ClaudeEndpoints.Usage = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Usage = old })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return asked
	}
}

// A usage reply that gives none of its windows a figure has given no
// reading: the account keeps the reading it had, dated when it was taken, the
// listing says why on stderr, and nothing reads as 0%. An account that had no
// reading still has none. And the endpoint is left alone for a backoff, as it
// is after a 429, so the next look does not ask it again.
func TestAReadingWithNoFigureKeepsTheLastOne(t *testing.T) {
	none := `{"five_hour":{"utilization":null,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":null}}`
	asked := usageBy(t, map[string]string{"t1": none, "t2": none})
	answering(t, "claude")
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
	if !strings.Contains(errOut, "no window a figure") || !strings.Contains(errOut, "not asked again until") {
		t.Fatalf("stderr says why there is no new reading, and until when: %q", errOut)
	}
	if n := asked(); n != 2 {
		t.Fatalf("each account asked once: %d", n)
	}
	if _, _, code := call(t, "list"); code != 0 || asked() != 2 {
		t.Fatalf("the next look asks nothing during the backoff: %d asks, code %d", asked(), code)
	}
	if _, errOut, code := call(t, "run", "-v", "a question"); code != 0 || asked() != 2 || !strings.Contains(errOut, "read@x") {
		t.Fatalf("nor does a run: %d asks, code %d, %q", asked(), code, errOut)
	}
}

// A window with no figure is left out of a reading whose other windows have
// one, so what those say counts: a spent one is spent, and one with room has
// room, whatever reading the account kept from before.
func TestTheWindowsWithAFigureAreTheReading(t *testing.T) {
	const healthy = `{"five_hour":{"utilization":10,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":10,"resets_at":"2099-01-01T00:00:00Z"}}`
	now := time.Now()
	reset := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct {
		what, first, second, store, runsOn string
	}{
		{
			"never read, the reply says five hours are spent",
			`{"five_hour":{"utilization":100,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":null,"resets_at":null}}`,
			healthy,
			`{"ordered":true,"nextId":3,"accounts":[
			 {"id":1,"provider":"claude","email":"first@x","order":1,"token":{"accessToken":"t1"}},
			 {"id":2,"provider":"claude","email":"second@x","order":2,"token":{"accessToken":"t2"}}]}`,
			"second@x",
		},
		{
			"a kept reading with room, the reply says seven days are spent",
			`{"five_hour":{"utilization":null,"resets_at":null},"seven_day":{"utilization":100,"resets_at":"2099-01-01T00:00:00Z"}}`,
			healthy,
			fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
			 {"id":1,"provider":"claude","email":"first@x","order":1,"token":{"accessToken":"t1"},"quotaAt":%d,
			  "quota":{"windows":[{"name":"5h","percent":40,"primary":true},{"name":"7d","percent":60}]}},
			 {"id":2,"provider":"claude","email":"second@x","order":2,"token":{"accessToken":"t2"}}]}`, now.Add(-time.Hour).UnixMilli()),
			"second@x",
		},
		{
			"both kept spent on a window that has reset, both replies say there is room",
			`{"five_hour":{"utilization":5,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":null,"resets_at":null}}`,
			`{"five_hour":{"utilization":5,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":null,"resets_at":null}}`,
			fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
			 {"id":1,"provider":"claude","email":"first@x","order":1,"token":{"accessToken":"t1"},"quotaAt":%[1]d,
			  "quota":{"windows":[{"name":"5h","percent":100,"primary":true,"resetsAt":"%[2]s"}]}},
			 {"id":2,"provider":"claude","email":"second@x","order":2,"token":{"accessToken":"t2"},"quotaAt":%[1]d,
			  "quota":{"windows":[{"name":"5h","percent":100,"primary":true,"resetsAt":"%[2]s"}]}}]}`, now.Add(-6*time.Hour).UnixMilli(), reset),
			"first@x",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			usageBy(t, map[string]string{"t1": c.first, "t2": c.second})
			home := t.TempDir()
			t.Setenv("ROTA_HOME", home)
			writeStore(t, home, c.store)
			answering(t, "claude")
			out, errOut, code := call(t, "run", "-v", "a question")
			if code != 0 || out != "ANSWERED\n" || !strings.Contains(errOut, c.runsOn) {
				t.Fatalf("runs on %s: %d %q %q", c.runsOn, code, out, errOut)
			}
		})
	}
}
