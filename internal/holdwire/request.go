package holdwire

import (
	"errors"
	"slices"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
)

// The two kinds of session a holder runs, in Request.Kind and Meta.Kind.
const (
	KindAccount = "account" // an account's vendor CLI
	KindShell   = "shell"   // the person's login shell
)

// Request is what the daemon writes to a new holder's standard input: one
// JSON document, never argv, since arguments can hold a prompt. It names an
// account and never carries its credential: the holder prepares the account
// itself, so no token crosses between the two processes.
type Request struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Account int      `json:"account"`
	Args    []string `json:"args"`
	// Keep is what the session's record keeps of its arguments (Meta.Args,
	// through StoredArgs): the ones it was asked for, exactly as given,
	// before the guard against Remote Control that Args may carry. Each
	// launch decides the guard afresh, so the record never holds it, a
	// --settings file stays a path, and a later launch from the record
	// carries no guard onto an account whose Remote Control is on by then.
	// Nil keeps Args.
	Keep []string `json:"keep,omitzero"`
	Cwd  string   `json:"cwd"`
	Name string   `json:"name"`
	Cols uint16   `json:"cols"`
	Rows uint16   `json:"rows"`
	// Env, when set, is the environment the session starts from — the
	// caller's own, already filtered of anything that is a credential — in
	// place of the daemon's.
	Env        []string `json:"env"`
	Scrollback int      `json:"scrollback"`
	// Caller, when set, is who asked for the session, by the name the refresh
	// journal knows it by: CallerDetach, or nothing for "holder".
	Caller string `json:"caller,omitempty"`
}

// CallerHeader is the request header a client names itself in when it asks a
// daemon for a held session, and CallerDetach the one name a daemon takes from
// it. A header, so that a daemon older than its client passes over it, where it
// would refuse a JSON member it does not know.
const (
	CallerHeader = "X-Rota-Caller"
	CallerDetach = "detach"
)

// Ready is the holder's one line of answer on its standard output: OK with
// the holder's pid and when the credential inside will stop working, or why
// it could not start, with Kind saying which sort of why.
type Ready struct {
	OK         bool      `json:"ok"`
	PID        int       `json:"pid,omitempty"`
	TokenUntil time.Time `json:"token_until,omitzero"`
	Error      string    `json:"error,omitempty"`
	Kind       string    `json:"kind,omitempty"`
}

// The kinds of failure a holder reports, so the daemon can answer with a
// status without reading the sentence.
const (
	KindBusy        = "busy"        // the account is running elsewhere and cannot run twice
	KindReauth      = "reauth"      // the account's login is finished
	KindNoAccount   = "noaccount"   // no account has that id
	KindUnsupported = "unsupported" // the provider's CLI cannot do this, or is not installed
	KindOutside     = "outside"     // the folder is outside the ones allowed
	KindOther       = "other"       // anything else
)

// Classify names the kind of err, by the verdicts rota already has. A nil
// error is no failure and has no kind.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, rota.ErrBusy):
		return KindBusy
	case errors.Is(err, rota.ErrReauth), errors.Is(err, rota.ErrDeadToken):
		return KindReauth
	case errors.Is(err, rota.ErrNoAccount):
		return KindNoAccount
	case errors.Is(err, rota.ErrUnsupported):
		return KindUnsupported
	case errors.Is(err, rota.ErrOutsideRoots):
		return KindOutside
	}
	return KindOther
}

// LoginUntil is when the credential inside a session stops working. A
// claude session on the account's own stored login — no token in its
// environment — runs until the login itself ends, because Claude Code
// refreshes it; one on a token runs until that token does: the long-lived
// one's date where that is what it was given, the access token's otherwise.
func LoginUntil(a *rota.Account, env []string) time.Time {
	onToken := slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") })
	switch {
	case rota.Flavor(a.Provider) == "claude" && !onToken:
		return a.LoginUntil()
	case a.LongValid():
		return a.LongUntil()
	case a.Token.ExpiresAt != 0:
		return time.UnixMilli(a.Token.ExpiresAt)
	}
	return time.Time{}
}
