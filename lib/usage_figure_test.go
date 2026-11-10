package rota

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/nofigure"
)

// A window the usage endpoint reports with no figure — utilization null or
// missing, or a model's row whose percent is — is left out of the reading,
// as Claude Code leaves it out, never read as 0%: read as 0%, a spent window
// would look unspent. The windows that do have a figure are the reading. A
// reply that leaves no window with a figure is a reading that failed, and so
// is one that reported the account's own windows with none of them a figure. A
// window that is null or absent is a limit the account does not have (an
// Ultra account has no five-hour one).
func TestAWindowWithNoFigureIsLeftOut(t *testing.T) {
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Usage, f.URL+"/usage")
	body := ""
	f.reply["/usage"] = func(*http.Request, map[string]any) (int, any) { return 200, body }
	p, _ := Lookup("claude")
	m := p.(Meter)

	for _, c := range []struct{ what, body, names string }{
		{"five_hour null, seven_day absent", `{"five_hour":{"utilization":null,"resets_at":"2099-01-01T00:00:00Z"}}`, "five_hour"},
		{"both null", `{"five_hour":{"utilization":null},"seven_day":{"utilization":null}}`, "five_hour, seven_day"},
		{"both without one", `{"five_hour":{"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{}}`, "five_hour, seven_day"},
		{"a model's row null, nothing else", `{"seven_day":null,"limits":[{"kind":"weekly_scoped","percent":null,"scope":{"model":{"display_name":"Fable"}}}]}`, "the Fable window"},
		// A model's row is no reading of the whole account: with both of the
		// account's own windows figure-less, it would read 0% and replace a
		// kept reading that was spent.
		{"only a model's row has one", `{"five_hour":{"utilization":null},"seven_day":{"utilization":null},"limits":[{"kind":"weekly_scoped","percent":7,"scope":{"model":{"display_name":"Fable"}}}]}`, "five_hour, seven_day"},
		{"five_hour null, a model's row has one", `{"five_hour":{"utilization":null},"limits":[{"kind":"weekly_scoped","percent":7,"scope":{"model":{"display_name":"Fable"}}}]}`, "five_hour"},
	} {
		body = c.body
		q, err := m.Quota(context.Background(), "A1")
		if err == nil {
			t.Errorf("%s: read as %+v", c.what, q)
			continue
		}
		if q != nil || !errors.Is(err, nofigure.Err) || !strings.Contains(err.Error(), c.names) {
			t.Errorf("%s: %+v %v", c.what, q, err)
		}
	}

	for _, c := range []struct {
		what, body string
		windows    []string
		percents   []float64
	}{
		{"five_hour null", `{"five_hour":{"utilization":null,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":40}}`, []string{"7d"}, []float64{40}},
		{"five_hour without one", `{"five_hour":{"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":100}}`, []string{"7d"}, []float64{100}},
		{"seven_day null", `{"five_hour":{"utilization":100},"seven_day":{"utilization":null}}`, []string{"5h"}, []float64{100}},
		{"a model's row null", `{"seven_day":{"utilization":40},"limits":[{"kind":"weekly_scoped","percent":null,"scope":{"model":{"display_name":"Fable"}}}]}`, []string{"7d"}, []float64{40}},
		{"a model's row without one", `{"seven_day":{"utilization":40},"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}}}]}`, []string{"7d"}, []float64{40}},
		{"no window of the account's own reported, a model's row has one", `{"limits":[{"kind":"weekly_scoped","percent":7,"scope":{"model":{"display_name":"Fable"}}}]}`, []string{"Fable"}, []float64{7}},
		{"Ultra, with no five-hour window", `{"five_hour":null,"seven_day":{"utilization":25.0}}`, []string{"7d"}, []float64{25}},
		{"a row for no model, with no figure", `{"seven_day":{"utilization":25},"limits":[{"kind":"session","percent":null,"scope":null}]}`, []string{"7d"}, []float64{25}},
		{"a Team seat, its weekly window only per model", `{"five_hour":{"utilization":41,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":null,
		  "limits":[{"kind":"session","group":"session","percent":41,"resets_at":"2099-01-01T00:00:00Z","scope":null},
		            {"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2099-01-05T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}]}`,
			[]string{"5h", "Fable"}, []float64{41, 0}},
	} {
		body = c.body
		q, err := m.Quota(context.Background(), "A1")
		if err != nil {
			t.Errorf("%s: %v", c.what, err)
			continue
		}
		var got []string
		for i, w := range q.Windows {
			got = append(got, w.Name)
			if i < len(c.percents) && w.Percent != c.percents[i] {
				t.Errorf("%s: %s at %v, want %v", c.what, w.Name, w.Percent, c.percents[i])
			}
		}
		if strings.Join(got, ",") != strings.Join(c.windows, ",") {
			t.Errorf("%s: windows %v, want %v", c.what, got, c.windows)
		}
	}
}
