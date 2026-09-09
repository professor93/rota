package wire

import (
	"encoding/json"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// The end of a stream says what the run cost, the way a buffered reply
// does: a client watching a run should not have to make a second request
// to learn what it paid for.
func TestTheEndOfAStreamCarriesTheTotals(t *testing.T) {
	res := &rota.Result{
		ExitCode: 0, SessionID: "s", DurationMS: 12, NumTurns: 2, CostUSD: 0.25,
		Usage: json.RawMessage(`{"input_tokens":3,"output_tokens":9}`),
	}
	end := Ended(res, nil)
	raw, err := rota.Encode(end)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"done"`, `"exit_code":0`, `"num_turns":2`, `"cost_usd":0.25`, `"usage":{"input_tokens":3,"output_tokens":9}`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
	// A run that never produced a result has no totals to report, and does
	// not report empty ones.
	raw, _ = rota.Encode(Ended(nil, nil))
	if strings.Contains(string(raw), "cost_usd") || strings.Contains(string(raw), "usage") || strings.Contains(string(raw), "num_turns") {
		t.Fatalf("nothing to total: %s", raw)
	}
}
