package rota

import (
	"errors"
	"fmt"
)

// The verdicts rota can reach. They are sentinel values matched with
// errors.Is, so a transport can map them onto its own vocabulary — an HTTP
// status, an exit code — without reading error text. Matching on a message
// would turn every reworded sentence into a breaking change.
var (
	// ErrInvalidRequest: the request itself is wrong — an unknown field
	// value, a model the provider does not have, a path that is not a
	// directory. Nothing was attempted.
	ErrInvalidRequest = errors.New("invalid request")
	// ErrDangerous: the request asked for something that bypasses a
	// permission check, and the caller did not allow it.
	ErrDangerous = errors.New("dangerous option not allowed")
	// ErrOutsideRoots: a path lies outside the directories the caller
	// permitted.
	ErrOutsideRoots = errors.New("path outside the allowed directories")
	// ErrUnsupported: this provider cannot do what was asked at all — it
	// has no headless interface, no usage endpoint, no effort setting.
	ErrUnsupported = errors.New("not supported by this provider")
	// ErrReauth: the credential is finished and only a fresh login revives
	// it.
	ErrReauth = errors.New("needs re-auth")
	// ErrDeadToken: a refresh lineage is permanently over. Providers
	// reject a reused refresh token, so this is never worth retrying.
	ErrDeadToken = errors.New("token lineage is dead")
	// ErrAuthPending: a device login has not been approved yet. Not a
	// failure — ask again.
	ErrAuthPending = errors.New("not approved yet")
	// ErrNoLogin: no login is in flight under that id.
	ErrNoLogin = errors.New("no pending login")
	// ErrNoAccount: no such account.
	ErrNoAccount = errors.New("no such account")

	// ErrBusy is returned when an account is already running and cannot
	// safely run twice at once. See OwnsCredentials.
	ErrBusy = errors.New("account is already running")
)

// verdictError carries a machine-readable kind and a human-readable
// message, and shows only the message. Wrapping the sentinel into the text
// instead would prefix every sentence a person reads with "invalid request:",
// which says nothing they did not already know.
type verdictError struct {
	kind error
	msg  string
}

func (e *verdictError) Error() string { return e.msg }
func (e *verdictError) Unwrap() error { return e.kind }

// failf builds an error carrying one of the verdicts above plus a message a
// person can act on.
func failf(kind error, format string, a ...any) error {
	return &verdictError{kind: kind, msg: fmt.Sprintf(format, a...)}
}

// The wrappers below let another package — an application's own storage, or
// the store package here — report rota's verdicts without reaching into
// unexported helpers, and without inventing its own wording for a condition
// rota already names.

// WrapNoAccount reports that no account carries this id.
func WrapNoAccount(id int) error { return failf(ErrNoAccount, "no such account: %d", id) }

// WrapNoLogin reports that no login is in flight under this id.
func WrapNoLogin(id string) error {
	return failf(ErrNoLogin, "no pending login %q (expired?)", id)
}

// WrapReauth reports that an account's credential is finished.
func WrapReauth(a *Account) error { return failf(ErrReauth, "%s: log in again", a) }

// WrapNoBinary reports that a provider's CLI is not installed.
func WrapNoBinary(bin string, err error) error {
	return failf(ErrUnsupported, "%s not found in PATH: %v", bin, err)
}

// Invalid builds an ErrInvalidRequest with a message. It is exported so a
// transport can report a request it rejected before rota ever saw it — a
// malformed body, an unreadable upload — in the same vocabulary.
func Invalid(format string, a ...any) error { return failf(ErrInvalidRequest, format, a...) }
