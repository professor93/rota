package holdwire

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// A claude terminal on the account's own stored login runs until the login
// itself ends — Claude Code refreshes the token inside — and one on a token
// runs until that token does.
func TestATerminalOnItsOwnLoginLapsesWhenTheLoginEnds(t *testing.T) {
	ends := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Millisecond)
	access := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	a := &rota.Account{Provider: "claude", Token: rota.Token{Access: "A", Refresh: "R", ExpiresAt: access.UnixMilli()},
		Extra: map[string]string{"refresh_token_expires_at": strconv.FormatInt(ends.UnixMilli(), 10)}}
	if got := LoginUntil(a, []string{"CLAUDE_CONFIG_DIR=/h"}); !got.Equal(ends) {
		t.Fatalf("on its own login, the login's end: %v", got)
	}
	if got := LoginUntil(a, []string{"CLAUDE_CODE_OAUTH_TOKEN=A"}); !got.Equal(access) {
		t.Fatalf("on a token, the token's: %v", got)
	}
	delete(a.Extra, "refresh_token_expires_at")
	if got := LoginUntil(a, nil); !got.IsZero() {
		t.Fatalf("an end nobody recorded is not shown: %v", got)
	}
}

// A holder that could not start says why in a word the daemon maps to a
// status, whatever the sentence around it.
func TestClassifyNamesTheKind(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{rota.ErrBusy, KindBusy},
		{fmt.Errorf("account 3: %w", rota.ErrBusy), KindBusy},
		{rota.ErrReauth, KindReauth},
		{rota.ErrDeadToken, KindReauth},
		{rota.WrapNoAccount(9), KindNoAccount},
		{rota.WrapNoBinary("codex", errors.New("not found")), KindUnsupported},
		{rota.ErrOutsideRoots, KindOutside},
		{errors.New("the disk is full"), KindOther},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("Classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
