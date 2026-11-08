package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	rota "github.com/professor93/rota/lib"
)

// The refresh journal is refresh.log in the store's directory: one line of
// JSON for every refresh rota makes, whatever came of it, and for every change
// a look at a claude home finds there — a rotation taken, a login made inside
// Claude Code taken, the account's login blanked, the record of what the home
// holds demoted — and every login rota writes into a home. It is rota's own
// record, written nowhere near a home, and nothing a CLI sees changes with it.
//
// A token is never written, only its fingerprint: the same short hash the
// account's Staged keeps, so a line can be held against the store. A line that
// cannot be written is lost, and the refresh it was about goes on as if it had
// been: the journal is evidence, never a step. It is moved aside to
// refresh.log.1 at one MiB, replacing the one there, as the watcher's log is.
// A store kept anywhere but in files keeps no journal.

const (
	journalName = "refresh.log"
	// refreshUntilKey is where a claude account keeps its login's
	// refreshTokenExpiresAt: what tells one login from the next.
	refreshUntilKey = "refresh_token_expires_at"
)

// journalCap is how large the journal grows before it is moved aside.
var journalCap int64 = 1 << 20

// journalMu keeps this process's lines whole and its rotation single.
var journalMu sync.Mutex

// heldLocks are the homes whose refresh lock this process holds right now, by
// the lock's path, so a line can say whether rota held it.
var heldLocks sync.Map

// journalEntry is one line. Its fields are what the line says and nothing
// more; the unexported ones are what writing it needs.
type journalEntry struct {
	TS           string       `json:"ts"`
	PID          int          `json:"pid"`
	Version      string       `json:"version"`
	Account      int          `json:"account"`
	Provider     string       `json:"provider"`
	Path         string       `json:"path"`
	Before       string       `json:"before"`
	After        string       `json:"after"`
	Home         *journalHome `json:"home,omitempty"`
	StagedBefore string       `json:"staged_before"`
	StagedAfter  string       `json:"staged_after"`
	Alive        []string     `json:"alive,omitempty"`
	Lock         bool         `json:"lock"`
	Outcome      string       `json:"outcome"`
	ExpBefore    string       `json:"exp_before,omitempty"`
	ExpAfter     string       `json:"exp_after,omitempty"`

	s *Store
	a *rota.Account
	// presented is whether the refresh about to be made can reach the
	// provider at all: a token due, a refresh token to present, a provider
	// that refreshes.
	presented bool
}

// journalHome is what rota's last read of the home found there: the
// fingerprint of the refresh token in it, "" when it holds none or a blanked
// one, and where it came from — the keychain item, the file, nowhere, or a
// store that could not be read.
type journalHome struct {
	FP     string `json:"fp"`
	Source string `json:"source"`
}

// homeSeen is what readStore last found, kept for the journal, or the
// fingerprint of the login rota wrote into the home's file since (wrote).
type homeSeen struct {
	raw, item []byte
	err       error
	read      bool
	written   string
}

// fingerprintOf is the fingerprint the account's Staged keeps for a refresh
// token, taken from lib itself so the two can never differ: "" for none.
func fingerprintOf(secret string) string {
	probe := rota.Account{Token: rota.Token{Refresh: secret}}
	probe.StagedWritten()
	return probe.Staged
}

func journalTime(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// journalStart is the first half of a line about an account: the account as
// it is before the step.
func (s *Store) journalStart(path string, a *rota.Account) *journalEntry {
	p, err := rota.Lookup(a.Provider)
	_, refreshes := p.(rota.Refresher)
	return &journalEntry{
		Account: a.ID, Provider: a.Provider, Path: path,
		Before: fingerprintOf(a.Token.Refresh), StagedBefore: a.Staged, ExpBefore: journalTime(a.Token.ExpiresAt),
		s: s, a: a, presented: err == nil && refreshes && a.Token.Refresh != "" && a.Expired(),
	}
}

// journalStart is the same for a claude home, with what the last read of it
// found, what is alive there, and whether rota holds its refresh lock.
func (h *claudeHome) journalStart(path string) *journalEntry {
	e := h.s.journalStart(path, h.a)
	e.Home = h.seenHome()
	e.Alive = h.live()
	_, e.Lock = heldLocks.Load(filepath.Join(h.home, claudeRefreshLock))
	return e
}

func (h *claudeHome) seenHome() *journalHome {
	seen := h.seen
	switch {
	case !seen.read:
		return nil
	case seen.written != "":
		return &journalHome{FP: seen.written, Source: "file"}
	case seen.err != nil:
		return &journalHome{Source: "unreadable"}
	case seen.raw == nil:
		return &journalHome{Source: "none"}
	}
	l, _ := storedLogin(seen.raw)
	source := "file"
	if seen.item != nil && bytes.Equal(seen.item, seen.raw) {
		source = "item"
	}
	return &journalHome{FP: fingerprintOf(l.Refresh), Source: source}
}

// finish is the second half: the account as the step left it, and what came
// of it. The line is written last of all.
func (e *journalEntry) finish(outcome string) {
	a := e.a
	e.Outcome = outcome
	e.After, e.StagedAfter, e.ExpAfter = fingerprintOf(a.Token.Refresh), a.Staged, journalTime(a.Token.ExpiresAt)
	e.s.journal(e)
}

// journal writes one line, and never fails anything: a line that cannot be
// written is lost.
func (s *Store) journal(e *journalEntry) {
	fb, ok := s.backend.(*FileBackend)
	if !ok {
		return
	}
	e.TS, e.PID, e.Version = journalTime(rota.NowMS()), os.Getpid(), rota.Version
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	appendJournal(filepath.Join(fb.Dir, journalName), append(line, '\n'))
}

func appendJournal(path string, line []byte) {
	journalMu.Lock()
	defer journalMu.Unlock()
	if fi, err := os.Stat(path); err == nil && fi.Size() >= journalCap {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
}

/* ---------------------------------------------------------- refreshes --- */

// refreshPath names the way into claudeHome.refresh: a rescue of a blanked or
// a stranded home, or whatever the look set in via, a launch's renew when it
// set nothing.
func (h *claudeHome) refreshPath(rescue bool) string {
	switch {
	case rescue && h.blanked:
		return "rescue.blank"
	case rescue:
		return "rescue.stranded"
	case h.via != "":
		return h.via
	}
	return "launch.renew"
}

// refreshOutcome is what the provider made of a refresh, read straight after
// it answered and before anything else changes the account.
func refreshOutcome(e *journalEntry, changed bool, err error) string {
	a := e.a
	switch {
	case err == nil && changed && !a.Dead:
		return "rotated"
	case !e.presented && (changed || err != nil):
		return "skipped:" + journalWords(nonEmpty(a.DeadReason, errText(err)))
	case !e.presented:
		return "skipped:not due"
	case a.Dead:
		return "refused:" + oauthCode(a.DeadReason)
	case err != nil:
		return "transient:" + transientWhy(err)
	}
	return "skipped:nothing changed"
}

// oauthCode is the protocol's own word for a refusal, which a refusal's reason
// starts with; any other reason is given in a few words.
func oauthCode(reason string) string {
	code, _, _ := strings.Cut(reason, ":")
	code = strings.TrimSpace(code)
	if code != "" && strings.Trim(code, "abcdefghijklmnopqrstuvwxyz_") == "" {
		return code
	}
	return journalWords(reason)
}

// transientWhy says in a word or two why a refresh did not get through: the
// status the provider answered with, the OAuth code it gave, or what stood in
// the way.
func transientWhy(err error) string {
	var he *rota.HTTPError
	var oe *rota.OAuthError
	var ne net.Error
	switch {
	case errors.As(err, &he):
		return strconv.Itoa(he.Status)
	case errors.As(err, &oe) && oe.Code != "":
		return oe.Code
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return journalWords(err.Error())
		}
		err = next
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// journalWords keeps a reason to one short line.
func journalWords(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}

// refreshLogged is rota.Refresh with a line in the journal, for every refresh
// of a token rota alone holds: path is the look's own renew for a provider
// that keeps no shared home, and token.env for a claude account on a token in
// its environment. A refresh that had nothing to do writes nothing.
func (s *Store) refreshLogged(ctx context.Context, look string, a *rota.Account) (bool, error) {
	path := look + ".renew"
	if rota.SharedHome(a.Provider) {
		path = "token.env"
	}
	e := s.journalStart(path, a)
	changed, err := rota.Refresh(ctx, a)
	if changed || err != nil {
		e.finish(refreshOutcome(e, changed, err))
	}
	return changed, err
}

/* ------------------------------------------------- looking and writing --- */

// journalMark is what adoption may change that the journal tells of.
type journalMark struct {
	refresh, access, staged, until string
	expires                        int64
	dead                           bool
}

func (h *claudeHome) mark() journalMark {
	a := h.a
	return journalMark{refresh: a.Token.Refresh, access: a.Token.Access, staged: a.Staged, until: a.Extra[refreshUntilKey],
		expires: a.Token.ExpiresAt, dead: a.Dead}
}

// journalAdopt writes a line for what adopt changed, deferred at its start
// with the account as it was: a login made inside Claude Code taken, a
// rotation taken, the home found blanked, or the record of what the home holds
// demoted.
func (h *claudeHome) journalAdopt(was journalMark) {
	a := h.a
	now := h.mark()
	var outcome string
	switch {
	case now.refresh != was.refresh && (was.dead || now.until != was.until):
		outcome = "new-login"
	case now.refresh != was.refresh || now.access != was.access:
		outcome = "rotation"
	case h.blanked || now.dead && !was.dead:
		outcome = "blanked"
	case now.staged != was.staged && !a.StagedCurrent():
		outcome = "demoted"
	default:
		return
	}
	e := h.journalStart("adopt")
	e.Before, e.StagedBefore, e.ExpBefore = fingerprintOf(was.refresh), was.staged, journalTime(was.expires)
	e.finish(outcome)
}

// wrote writes a line for a login rota has just put into the home, by seed or
// by uncover: the account's current login, which the home's file holds from
// now on, as the next line says until the home is read again.
func (h *claudeHome) wrote(path string) {
	e := h.journalStart(path)
	e.Outcome = "written"
	e.After, e.StagedAfter, e.ExpAfter = e.Before, fingerprintOf(h.a.Token.Refresh), e.ExpBefore
	h.s.journal(e)
	h.seen = homeSeen{read: true, written: e.StagedAfter}
}
