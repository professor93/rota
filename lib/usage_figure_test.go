package rota

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A window the usage endpoint reports with no figure — utilization null or
// missing, or a model's row whose percent is — is a reading that failed,
// never one of 0%: read as 0%, a damaged reading would make its account look
// like the one with the most room left. A window that is null or absent is a
// limit the account does not have (an Ultra account has no five-hour one),
// and the rest of the reading stands.
func TestAWindowWithNoFigureFailsTheReading(t *testing.T) {
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Usage, f.URL+"/usage")
	body := ""
	f.reply["/usage"] = func(*http.Request, map[string]any) (int, any) { return 200, body }
	p, _ := Lookup("claude")
	m := p.(Meter)

	for _, c := range []struct{ what, body, names string }{
		{"five_hour null", `{"five_hour":{"utilization":null,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":40}}`, "five_hour"},
		{"five_hour without one", `{"five_hour":{"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":40}}`, "five_hour"},
		{"seven_day null", `{"five_hour":{"utilization":12},"seven_day":{"utilization":null}}`, "seven_day"},
		{"a model's row null", `{"seven_day":{"utilization":40},"limits":[{"kind":"weekly_scoped","percent":null,"scope":{"model":{"display_name":"Fable"}}}]}`, "Fable"},
		{"a model's row without one", `{"seven_day":{"utilization":40},"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}}}]}`, "Fable"},
	} {
		body = c.body
		q, err := m.Quota(context.Background(), "A1")
		if err == nil {
			t.Errorf("%s: read as %+v", c.what, q)
			continue
		}
		if q != nil || !strings.Contains(err.Error(), c.names) || !strings.Contains(err.Error(), "no figure") {
			t.Errorf("%s: %+v %v", c.what, q, err)
		}
	}

	for _, c := range []struct {
		what, body string
		windows    []string
	}{
		{"Ultra, with no five-hour window", `{"five_hour":null,"seven_day":{"utilization":25.0}}`, []string{"7d"}},
		{"a row for no model, with no figure", `{"seven_day":{"utilization":25},"limits":[{"kind":"session","percent":null,"scope":null}]}`, []string{"7d"}},
		{"a Team seat, its weekly window only per model", `{"five_hour":{"utilization":41,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":null,
		  "limits":[{"kind":"session","group":"session","percent":41,"resets_at":"2099-01-01T00:00:00Z","scope":null},
		            {"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2099-01-05T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}]}`,
			[]string{"5h", "Fable"}},
	} {
		body = c.body
		q, err := m.Quota(context.Background(), "A1")
		if err != nil {
			t.Errorf("%s: %v", c.what, err)
			continue
		}
		var got []string
		for _, w := range q.Windows {
			got = append(got, w.Name)
		}
		if strings.Join(got, ",") != strings.Join(c.windows, ",") {
			t.Errorf("%s: windows %v, want %v", c.what, got, c.windows)
		}
	}
}
