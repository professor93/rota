package tui

import (
	"context"

	"github.com/professor93/rota/message"
	"github.com/professor93/rota/wire"
)

// Backend is everything the TUI asks of rota about accounts, logins and
// runs. It has one implementation, over rota's own HTTP API (NewHTTP), so
// the TUI on this machine and the TUI over SSH ask the same questions of
// the same server, and the server's role table is the one judge of what
// either may do.
//
// Every call ends when its context does. A refusal is a *client.Refusal
// carrying the status, the reason a program can act on and the server's
// sentence, with anything shaped like a key taken out of the sentence.
type Backend interface {
	// Who is the principal the server sees: GET /v1/session.
	Who(ctx context.Context) (Principal, error)
	// Accounts lists the accounts in rotation order: GET /v1/accounts,
	// reading usage as how says.
	Accounts(ctx context.Context, how Refresh) (Listing, error)
	// Change sends what c says is changed, and nothing else, as one
	// PATCH /v1/accounts/{id}, and answers with the account as it now is.
	Change(ctx context.Context, id int, c Change) (wire.Account, error)
	// Remove forgets an account: DELETE /v1/accounts/{id}.
	Remove(ctx context.Context, id int) error
	// BeginLogin starts a login: POST /v1/login. What comes back is the
	// login as a reply shows it, which carries no state.
	BeginLogin(ctx context.Context, b LoginBegin) (backendLogin, error)
	// FinishLogin finishes a login begun earlier with what the person
	// brought back, or with nothing for a device login: POST /v1/login/{id}.
	// A login not approved yet is LoginEnd{Pending: true} and no error.
	FinishLogin(ctx context.Context, id, code string) (LoginEnd, error)
	// PasteKey makes or updates the account an API key belongs to, named
	// label when label is not empty: POST /v1/login {"key","label"}.
	PasteKey(ctx context.Context, key, label string) (LoginEnd, error)
	// PasteToken puts a token from `claude setup-token` on an account as its
	// long-lived token: POST /v1/login {"account","token"}.
	PasteToken(ctx context.Context, account int, token string) (LoginEnd, error)
	// Ask runs a prompt, on q.Account or on the rotation's choice when it is
	// zero, streaming each event to each as it arrives, and answers with
	// how the run ended: POST /v1/run or /v1/accounts/{id}/run.
	Ask(ctx context.Context, q Question, each func(message.Event)) (wire.End, error)
	// SendRun, InterruptRun and CloseRun reach a run that stays open, by the
	// id its opening event carried: POST /v1/runs/{id}/messages, /interrupt
	// and /close.
	SendRun(ctx context.Context, run, text string) error
	InterruptRun(ctx context.Context, run string) error
	CloseRun(ctx context.Context, run string) error
}

// Principal is who the server says is asking, and with which role: control
// or watch.
type Principal struct{ Name, Role, Via string }

// Refresh is how much a listing reads usage before it answers.
type Refresh int

const (
	// RefreshNone answers from what is already known.
	RefreshNone Refresh = iota
	// RefreshStale reads usage that is old first, as `rota list` does.
	RefreshStale
	// RefreshForce reads every account's usage first.
	RefreshForce
)

// Listing is the accounts in rotation order, and the one the rotation would
// spend next, zero when there is none.
type Listing struct {
	Accounts []wire.Account
	Default  int
}

// Change is an edit of one account. A nil field is left as it is, and
// ForgetLong throws the account's long-lived token away.
type Change struct {
	Order                    *string
	Threshold                *int
	Cwd, ConfigDir, Sessions *string
	RemoteControl            *bool
	ForgetLong               bool
}

// LoginBegin is what a login is begun for: a provider, with Long for a
// long-lived token, or an existing account, which is then the only one that
// can finish it.
type LoginBegin struct {
	Provider string
	Long     bool
	Account  int
}

// backendLogin is a login just begun, as POST /v1/login answers it:
// everything about it but its state, which stays with the parked login on
// the server. Only the id is needed to finish it. CreatedAt is Unix seconds;
// how long the login stays open is the server's to say, not this package's.
//
// It is the TUI's own reading of that JSON rather than the SDK's type, so
// the SDK exports nothing for it.
type backendLogin struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	URL       string `json:"url"`
	Kind      string `json:"kind"` // code, device, apikey or delegated
	Delegated bool   `json:"delegated"`
	Long      bool   `json:"long"`
	Account   int    `json:"account"`
	CreatedAt int64  `json:"createdAt"`
}

// LoginEnd is how a login ended: still pending, or an account added,
// refreshed, given a long-lived token ("long") or updated. Delegated says
// rota holds no credential for it, and LoginCommand what signs it in.
// LongUntil is when a long-lived token stops working, RFC 3339. Warning is
// something the person should know although the login succeeded.
type LoginEnd struct {
	Pending                          bool
	Account                          int
	Provider, Email, Status          string
	Delegated                        bool
	LoginCommand, LongUntil, Warning string
}

// Question is one Ask: the prompt, the account (zero for the rotation's
// choice), the folder it runs in (empty for the server's own), the readings
// asked for beside the answer, and Input for a run that stays open for more
// messages.
type Question struct {
	Account     int
	Prompt, Cwd string
	With        []string
	Input       bool
}
