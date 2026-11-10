// Package logins is what the store, the HTTP API and the rota command share
// about a login between its two halves, and keep out of the SDK: how long a
// parked login is kept, which refusals no approval can put right, and how a
// login just begun is shown in a reply.
package logins

import (
	"errors"
	"time"

	rota "github.com/professor93/rota/lib"
)

// TTL bounds how long a half-finished login is kept, counted from its
// CreatedAt; the authorization code dies server-side within minutes anyway.
// Past it the store no longer finds the login (rota.ErrNoLogin), so a command
// waiting on somebody to approve one can say how long is left.
const TTL = 15 * time.Minute

// ErrDamagedBindings is the store finding the file that says which parked
// logins were begun for an account unreadable. Nothing finishes until it is
// put right, which is the operator's to do, so a server answers it with its
// words rather than as an internal error.
var ErrDamagedBindings = errors.New("the record of which logins were begun for an account is damaged")

// MarkUnfinishable marks a refusal of a login as one no approval can finish,
// which Unfinishable then reports. It is otherwise the refusal it carries, the
// rota.ErrInvalidRequest beneath it and all.
func MarkUnfinishable(err error) error { return unfinishable{err} }

// Unfinishable reports whether err is the store turning down a login no
// approval can finish: one begun for an account that has since been removed,
// or approved by that account's e-mail under another of the provider's ids,
// which approving again would only repeat. The login stays parked, as after
// every refusal, and runs out by itself; a command waiting for somebody to
// approve it again stops waiting. Every other error, rota.ErrNoLogin and a
// rejected code included, is not this.
func Unfinishable(err error) bool {
	var u unfinishable
	return errors.As(err, &u)
}

type unfinishable struct{ error }

func (u unfinishable) Unwrap() error { return u.error }

// Reply is a login just begun as `rota login --json` and POST /v1/login show
// it: everything rota.Login says about it but its State, and the account it
// was begun for. The state is what Complete needs, the PKCE verifier among
// it, and it stays with the parked login: whoever opens the URL and brings
// back the code needs only the id to finish it, and a verifier that left the
// machine is half of what a stolen code needs.
type Reply struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	URL       string `json:"url"`
	Kind      string `json:"kind"`
	Delegated bool   `json:"delegated,omitzero"`
	Long      bool   `json:"long,omitzero"`
	// Account is the id of the account the login was begun for, which only
	// that account's approval finishes; zero for a login bound to nobody.
	Account   int   `json:"account,omitzero"`
	CreatedAt int64 `json:"createdAt"`
}

// ReplyOf is l as a reply shows it, begun for account (zero for none).
func ReplyOf(l *rota.Login, account int) Reply {
	return Reply{ID: l.ID, Provider: l.Provider, URL: l.URL, Kind: l.Kind, Delegated: l.Delegated,
		Long: l.Long, Account: account, CreatedAt: l.CreatedAt}
}
