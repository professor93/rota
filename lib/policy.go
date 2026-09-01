package rota

// Status is an account's one-word health.
type Status string

const (
	StatusOK      Status = "ok"
	StatusLimited Status = "limited" // an unscoped window is spent and has not reset
	StatusReauth  Status = "reauth"  // the credential is dead; only a login helps
)

// Status reports the account's health from what is already known; it does
// no network calls.
func (a *Account) Status() Status {
	switch {
	case a.Dead:
		return StatusReauth
	case a.Quota.blocked():
		return StatusLimited
	}
	return StatusOK
}

// blocked reports whether an unscoped window is spent and has not reset.
// Scoped windows never block: they cover one model, and nobody knows which
// model the session will use.
func (q *Quota) blocked() bool {
	if q == nil {
		return false
	}
	for _, w := range q.Windows {
		if !w.Scoped && w.Percent >= 100 && (w.ResetsAt.IsZero() || Now().Before(w.ResetsAt.Time)) {
			return true
		}
	}
	return false
}
