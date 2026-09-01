package rota

import (
	"errors"
	"strings"
)

// oauthTokenResp is an RFC 6749 token reply, success or error.
type oauthTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// grant says which exchange a reply answers; the same error code means
// different things in each.
type grant int

const (
	grantCode    grant = iota // authorization code pasted by the user
	grantRefresh              // refresh token
)

var oauthErrorCodes = []string{
	"invalid_grant", "refresh_token_reused", "authorization_pending",
	"slow_down", "expired_token", "access_denied",
}

// OAuthError is a terminal RFC 6749 refusal: the provider answered, and the
// answer is no. Code is the protocol's own word — access_denied,
// expired_token, invalid_grant — and Description whatever the server added.
// Error() stays a human sentence; the fields are for applications, which
// branch, localize, or map the outcome onto their own vocabulary without
// parsing prose.
type OAuthError struct {
	Code        string
	Description string
	msg         string
}

func (e *OAuthError) Error() string { return e.msg }

// verdict turns a token reply plus its transport error into this package's
// vocabulary: ErrAuthPending, ErrDeadToken, a typed OAuthError, or nil when
// the reply carries a usable access token.
func (r *oauthTokenResp) verdict(err error, g grant) error {
	code := r.Error
	var he *HTTPError
	if code == "" && errors.As(err, &he) {
		// Some servers wrap the code in an object; find it in the raw body.
		for _, c := range oauthErrorCodes {
			if strings.Contains(he.Body, `"`+c+`"`) {
				code = c
				break
			}
		}
	}
	refuse := func(msg string) error {
		return &OAuthError{Code: code, Description: r.ErrorDesc, msg: msg}
	}
	switch code {
	case "":
	case "authorization_pending", "slow_down":
		return ErrAuthPending
	case "invalid_grant", "refresh_token_reused":
		if g == grantRefresh {
			return ErrDeadToken
		}
		return refuse("authorization code was rejected (expired, already used, or mismatched); start a new login" + detail(r.ErrorDesc))
	case "expired_token":
		return refuse("login expired; start a new one")
	case "access_denied":
		return refuse("login was denied")
	default:
		return refuse(code + detail(r.ErrorDesc))
	}
	if err != nil {
		return err
	}
	if r.AccessToken == "" {
		return errors.New("token reply carried no access token")
	}
	return nil
}

func detail(desc string) string {
	if desc == "" {
		return ""
	}
	return ": " + desc
}

// token normalizes the reply. Expiry comes from expires_in, else from the
// access token's own exp claim when it is a JWT.
func (r *oauthTokenResp) token() *Token {
	t := &Token{Access: r.AccessToken, Refresh: r.RefreshToken, Scopes: strings.Fields(r.Scope)}
	if r.ExpiresIn > 0 {
		t.ExpiresAt = nowMS() + r.ExpiresIn*1000
	} else {
		t.ExpiresAt = jwtExpiryMS(r.AccessToken)
	}
	t.Identity = identityFromJWT(r.IDToken)
	return t
}
