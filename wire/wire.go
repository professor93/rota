// Package wire holds the shapes a transport needs: an account rendered for
// a listing, a file travelling with a request, the last event of a stream.
//
// It is outside lib on purpose. lib is a Go library — you call Begin,
// Complete, Refresh, Usage, Stage and Run with Go values and get Go values
// back — and it has no opinion about JSON, about how a timestamp should read
// to a person, or about what a form ought to show. Those are a transport's
// questions, and rota's own transports are the command and the HTTP server.
// Anything else that imports rota/lib is free to answer them differently, or
// not at all.
package wire

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
)

/* ------------------------------------------------------- an account seen -- */

// Account is one account as shown to people and programs: no secrets, health
// and quota rendered.
type Account struct {
	ID       int         `json:"id"`
	Provider string      `json:"provider"`
	Email    string      `json:"email,omitempty"`
	UUID     string      `json:"uuid,omitempty"`
	Status   rota.Status `json:"status"`
	Windows  []Window    `json:"windows,omitempty"`
	Note     string      `json:"note,omitempty"`
	// Order and Threshold are what the store holds. A zero threshold means
	// whatever the application deciding the rotation says it means, and that
	// application fills the resolved value in, over Cutoff. Percent
	// is the headline usage they are judged against, which lib does compute:
	// it is a reading of the provider's own quota rather than a rule.
	Order     int     `json:"order"`
	Threshold int     `json:"threshold"`
	Percent   float64 `json:"percent"`
	// Cwd and ConfigDir are the project this account is tied to, if any:
	// where its runs start, and where its own memory, skills and credentials
	// live. Absent means neither has been chosen.
	Cwd       string `json:"cwd,omitempty"`
	ConfigDir string `json:"config_dir,omitempty"`
	// Metered says whether this provider publishes a usage endpoint at all.
	// When it does not, there are no limits to report and no check to make.
	Metered bool `json:"metered"`
	// CheckedAt is when the quota was last read from the provider, RFC 3339;
	// empty when it never was. CheckedAgo is the same instant rendered as
	// "just now", "4m ago", "2d ago".
	CheckedAt  string `json:"checkedAt,omitempty"`
	CheckedAgo string `json:"checkedAgo,omitempty"`
}

// Window is one quota window with its countdown rendered.
type Window struct {
	Name     string  `json:"name"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt,omitempty"`
	ResetIn  string  `json:"resetIn,omitempty"`
	Scoped   bool    `json:"scoped,omitzero"`
	Primary  bool    `json:"primary,omitzero"`
}

// hidden names the providers no login surface offers. The SDK still carries
// them and an account already on one still runs; what is withheld is the
// invitation. kimi is here while its service refuses this build's logins.
var hidden = map[string]bool{"kimi": true}

// Hidden reports whether a provider is withheld from login.
func Hidden(provider string) bool { return hidden[provider] }

// LoginProviders is what a person may log into: every registered provider
// that is not hidden, sorted.
func LoginProviders() []string {
	var out []string
	for _, name := range rota.Providers() {
		if !hidden[name] {
			out = append(out, name)
		}
	}
	return out
}

// Describe renders an account for display. It does no network calls.
func Describe(a *rota.Account) Account {
	v := Account{ID: a.ID, Provider: a.Provider, Email: a.Email, UUID: a.UUID, Status: a.Status(),
		Metered: rota.Metered(a.Provider), Order: a.Order, Threshold: a.Threshold, Percent: a.Percent(),
		Cwd: a.Cwd, ConfigDir: a.ConfigDir}
	if a.QuotaAt > 0 {
		t := time.UnixMilli(a.QuotaAt)
		v.CheckedAt = t.UTC().Format(time.RFC3339)
		v.CheckedAgo = Since(t)
	}
	if q := a.Quota; q != nil {
		v.Note = q.Note
		for _, w := range q.Windows {
			wv := Window{Name: w.Name, Percent: w.Percent, ResetIn: Countdown(w.ResetsAt),
				Scoped: w.Scoped, Primary: w.Primary}
			if !w.ResetsAt.IsZero() {
				wv.ResetsAt = w.ResetsAt.Format(time.RFC3339)
			}
			v.Windows = append(v.Windows, wv)
		}
	}
	return v
}

// Since renders how long ago an instant was, for someone reading a list.
func Since(t time.Time) string {
	d := max(time.Since(t), 0)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours())/24)
}

/* ------------------------------------------------ files sent with a run -- */

// Upload is one file a caller wants on disk before the CLI starts. Content
// is base64, so a request stays plain JSON; a transport that carries bytes
// natively — a multipart body — encodes them into this same shape.
type Upload struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// StageUploads writes files into a fresh private directory and returns it.
// The caller adds it to Spec.AddDirs and removes it when the run ends.
//
// A path is refused, never quietly rewritten: a caller sending "../id_rsa"
// is not making a typo, and turning it into "id_rsa" would hide that. The
// directory is returned even on failure so a caller can still clean up.
func StageUploads(files []Upload) (dir string, err error) {
	if len(files) == 0 {
		return "", nil
	}
	dir, err = os.MkdirTemp("", "rota-upload-*")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return dir, err
	}
	for _, f := range files {
		// A caller writes paths with forward slashes whatever it runs on;
		// the check is made in this platform's form so that "notes/a.txt"
		// is as plain on Windows as anywhere.
		native := filepath.FromSlash(f.Path)
		clean := filepath.Clean(native)
		if f.Path == "" || filepath.IsAbs(native) || filepath.VolumeName(native) != "" ||
			strings.HasPrefix(native, string(filepath.Separator)) ||
			strings.HasPrefix(f.Path, "~") || clean != native || strings.HasPrefix(clean, "..") {
			return dir, rota.Invalid("upload path %q must be a plain relative path", f.Path)
		}
		target := filepath.Join(dir, clean)
		if !strings.HasPrefix(target, dir+string(os.PathSeparator)) {
			return dir, rota.Invalid("upload path %q is not inside the upload directory", f.Path)
		}
		raw, derr := base64.StdEncoding.DecodeString(f.Content)
		if derr != nil {
			return dir, rota.Invalid("upload %q: content must be base64: %v", f.Path, derr)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return dir, err
		}
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			return dir, err
		}
	}
	return dir, nil
}

/* ------------------------------------------------- the end of a stream --- */

// End is the last event of a streamed run. A stream sends its status line
// before the run finishes, so this is the only place a client learns how it
// ended.
type End struct {
	Type string `json:"type"` // "done" or "error"
	// ExitCode and IsError are always present, including when they are zero
	// and false: success is exactly the case a client needs to read, and a
	// missing field would make it guess.
	ExitCode int  `json:"exit_code"`
	IsError  bool `json:"is_error"`
	// Account is the one that ran. A caller that named it already knows; a
	// caller that left the choice to the rotation learns it here.
	Account    int    `json:"account,omitzero"`
	SessionID  string `json:"session_id,omitempty"`
	DurationMS int64  `json:"duration_ms,omitzero"`
	Error      string `json:"error,omitempty"`
}

// Ended describes how a run finished, from its result and whatever error
// stopped it.
func Ended(res *rota.Result, err error) End {
	if err != nil {
		return End{Type: "error", Error: err.Error()}
	}
	e := End{Type: "done"}
	if res != nil {
		e.ExitCode, e.SessionID, e.IsError, e.DurationMS = res.ExitCode, res.SessionID, res.IsError, res.DurationMS
		e.Account = res.Account
	}
	return e
}

/* ------------------------------------------- a login someone must run --- */

// LoginCommand renders the command that signs an account in, as a line
// someone could run by hand. Empty when the account's provider has no such
// command — most sign in through rota itself.
//
// The plan is a fact about the provider and lib knows it. How it reads is
// this package's business: the joining is naive on purpose, because it is
// shown to a person rather than handed to a shell.
func LoginCommand(a *rota.Account, home string) string {
	plan, ok := rota.LoginPlanFor(a, home)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(plan.Env)+len(plan.Args)+1)
	parts = append(parts, plan.Env...)
	parts = append(parts, plan.Bin)
	return strings.Join(append(parts, plan.Args...), " ")
}

// Countdown renders the time left until w as "2d 1h", "2h 40m" or "5m";
// empty for the zero value, "0m" once passed. Always recomputed at render
// time: a countdown stored an hour ago overstates the wait. It lives here
// rather than on the SDK's When because how a wait should read to a person
// is a presentation decision, and this package is where those are made.
func Countdown(w rota.When) string {
	if w.IsZero() {
		return ""
	}
	d := max(time.Until(w.Time), 0)
	days, hours, mins := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// Version is the applications' release number — the command and the
// server, which share it. The SDK underneath carries its own (rota.Version);
// the two move independently now that lib is a module anyone can take.
const Version = "1.0.4"
