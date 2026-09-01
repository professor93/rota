package rota

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
)

// Identity names whoever a token belongs to. All fields are best-effort.
type Identity struct {
	UUID  string `json:"uuid,omitempty"`
	Email string `json:"email,omitempty"`
	Org   string `json:"org,omitempty"`
}

// Token is one provider's credential, normalized.
type Token struct {
	Access    string   `json:"accessToken"`
	Refresh   string   `json:"refreshToken,omitempty"`
	ExpiresAt int64    `json:"expiresAt,omitzero"` // unix ms; 0 means "never"
	Scopes    []string `json:"scopes,omitempty"`
	// Delegated marks a credential rota does not hold: the vendor CLI signs
	// itself in and keeps its own tokens, and rota supplies only the
	// isolated directory it keeps them in. Access is empty for such a
	// token, and nothing about it expires or refreshes here.
	Delegated bool `json:"delegated,omitzero"`
	// Identity is set only when the token response itself carried it.
	Identity *Identity `json:"identity,omitzero"`
	// Extra carries provider state the account must remember across runs —
	// a device id, an id_token. It is merged into Account.Extra.
	Extra map[string]string `json:"extra,omitempty"`
}

// Window is one quota bucket. Providers report wildly different shapes, so
// they all normalize to a list of named percentages.
type Window struct {
	Name     string  `json:"name"`
	Percent  float64 `json:"percent"`
	ResetsAt When    `json:"resetsAt,omitzero"`
	// Scoped marks a window covering only part of the account — one model,
	// say — shown but never treated as a hard block.
	Scoped bool `json:"scoped,omitzero"`
	// Primary marks the window accounts are ranked by; at most one.
	Primary bool `json:"primary,omitzero"`
}

// Quota is what an account has left.
type Quota struct {
	Windows []Window `json:"windows"`
	// Note is free text for anything that is not a percentage.
	Note string `json:"note,omitempty"`
	// Extra is metered overflow spend, for providers that report one:
	// structured, so a caller formats or bills it its own way.
	Extra *ExtraUsage `json:"extra,omitzero"`
}

// ExtraUsage is pay-per-use spend beyond the plan's included quota.
type ExtraUsage struct {
	Used     float64 `json:"used"`
	Limit    float64 `json:"limit"`
	Currency string  `json:"currency,omitempty"`
}

// When is a lenient RFC 3339 timestamp: anything unparseable decodes to the
// zero value instead of failing the document around it, so one odd field
// from a provider can never blank a whole quota reading or store.
type When struct{ time.Time }

func (w *When) UnmarshalJSON(b []byte) error {
	var s string
	if decodeLenient(b, &s) != nil {
		*w = When{}
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		*w = When{}
		return nil
	}
	w.Time = t
	return nil
}

// jwtClaims decodes a JWT payload without verifying it. That is deliberate:
// these tokens arrive straight from the provider's token endpoint over TLS
// and the claims only label an account; nothing is authorized by them.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var m map[string]any
	if decodeLenient(raw, &m) != nil {
		return nil
	}
	return m
}

func claimString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// identityFromJWT reads the account out of an id_token, or out of an access
// token for providers that issue no id_token and name the user there.
func identityFromJWT(token string) *Identity {
	m := jwtClaims(token)
	uuid := claimString(m, "sub")
	if uuid == "" {
		uuid = claimString(m, "user_id")
	}
	email := claimString(m, "email")
	if uuid == "" && email == "" {
		return nil
	}
	return &Identity{UUID: uuid, Email: email, Org: claimString(m, "organization_id")}
}

// jwtExpiryMS returns the `exp` claim in unix milliseconds, or 0.
func jwtExpiryMS(token string) int64 {
	exp, _ := jwtClaims(token)["exp"].(float64)
	return int64(exp) * 1000
}

// chatgptAccountID reads the ChatGPT account a token is scoped to; it lives
// under a namespaced claim.
func chatgptAccountID(idToken string) string {
	auth, _ := jwtClaims(idToken)["https://api.openai.com/auth"].(map[string]any)
	return claimString(auth, "chatgpt_account_id")
}

// fingerprint is a short, non-reversible handle for a secret, used to record
// which refresh token was staged into a CLI's private home without writing
// the secret twice.
func fingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}
