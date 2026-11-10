package tui

// Task 17 fills this file in: w in the login flow serves the one-time page
// that takes the code, a key or a setup token. Until then the step says
// "not yet".

import "github.com/professor93/rota/cmd/rota/loginpage"

// pageAsk is what the page is served for: what it takes, the login it
// finishes (nil for a key), the URL to approve at, and the account and
// label a key or a token is for.
type pageAsk struct {
	Want    loginpage.Want
	Login   *backendLogin
	Approve string
	Account int
	Label   string
}

// newPageStep serves the page p asks for until it is answered or closed.
func newPageStep(e *env, p pageAsk) step { return overlayNotYet() }
