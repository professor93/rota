package store

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	rota "github.com/professor93/rota/lib"
)

// The refresh journal is refresh.log in the store's directory: one line of
// JSON for every refresh this store makes, for any provider — a line as the
// token goes out and one for what came of it — and for every change a look at
// a claude home finds there: a rotation taken, a login made inside Claude Code
// taken, the account's login found blanked, the record of what the home holds
// demoted; and for every login rota writes into a home. A codex login's repair
// inside staging is lib's own refresh, not the store's, and is not journalled.
// It is rota's own record, written nowhere near a home, and nothing a CLI sees
// changes with it.
//
// A token is never written, only its fingerprint: the same short hash the
// account's Staged keeps, so a line can be held against the store. What came
// of a refresh is a code — the provider's OAuth error code, an HTTP status, or
// one of a few fixed words — and never a message, which could carry what a
// reply held. A line that cannot be written is lost, and the refresh it was
// about goes on as if it had been: the journal is evidence, never a step.
//
// At one MiB it is moved aside to refresh.log.1, replacing the one there. Any
// number of rota processes write it at once, so the move is made under a lock
// of its own, by whichever takes it first, and only of a journal that is still
// full once the lock is held: a second process would otherwise move the new,
// nearly empty journal over the full one just moved aside. A process that finds
// the lock taken writes its line where it is, and a later line moves it. The
// journal is always a regular file, mode 0600: anything else at its path — a
// link, a pipe — is not written at all. A store kept anywhere but in files
// keeps no journal.

const (
	journalName = "refresh.log"
	// lastRefusalKey is where an account keeps the last time rota presented a
	// refresh token the provider refused and then took the home's login by
	// profile instead (keepRefusal), until a later rotation shows the login
	// lived through it.
	lastRefusalKey = "last_refusal"
	// pendingRefusalKey is such a refusal while the home's login waits to be
	// confirmed: the account is dead and in the hold, and the look that
	// confirms that login makes it the account's last_refusal.
	pendingRefusalKey = "pending_refusal"
	// refreshUntilKey is where a claude account keeps its login's
	// refreshTokenExpiresAt: what tells one login from the next.
	refreshUntilKey = "refresh_token_expires_at"
)

// journalCap is how large the journal grows before it is moved aside.
var journalCap int64 = 1 << 20

// journalTail is how much of the journal's end is read to tell whether a
// blanked home was already journalled as it stands.
const journalTail = 64 << 10

// journalMu keeps this process's lines whole and its rotation single.
var journalMu sync.Mutex

// heldLocks are the homes whose refresh lock this process holds right now, by
// the lock's path, so a line can say whether rota held it.
var heldLocks sync.Map

// callerKey carries the caller WithCaller names.
type callerKey struct{}

// WithCaller returns a copy of ctx that names the caller for the refresh
// journal, as one short word: a renew Refresh or Maintain makes under it — a
// refresh of a claude login kept in a home, or of any other provider's login
// — is journalled as <name>.renew. A claude login on a token is journalled as
// token.env and a rescue as rescue.blank or rescue.stranded whoever asks, and
// a store kept anywhere but in files keeps no journal. A name is lower-case
// letters, digits, '_', '.' and '-', up to 40 of them; anything else is
// "refresh", which still counts as a name given, and is also the name of a
// caller that gives none to Refresh. A launch under it (Run, Start, Prepare,
// PrepareIn) goes by the name too, and by "launch" with none. rota names its
// own: "list" for `rota list`, "choose" for the rotation choosing an account,
// "api" for the server's ?refresh=1, "daemon" for a daemon's sweep, "holder" for
// a held session's launch and "detach" for one `rota run --detach` asked for;
// Maintain is "serve" unless its caller named itself.
func WithCaller(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, callerKey{}, codeOr(name, "refresh"))
}

// callerOf is the caller ctx names, "refresh" when it names none.
func callerOf(ctx context.Context) string { return callerOr(ctx, "refresh") }

// callerOr is the caller ctx names, otherwise when it names none.
func callerOr(ctx context.Context, otherwise string) string {
	if name, _ := ctx.Value(callerKey{}).(string); name != "" {
		return name
	}
	return otherwise
}

// withCallerOr is ctx naming name as its caller, unless it names one already.
func withCallerOr(ctx context.Context, name string) context.Context {
	if _, named := ctx.Value(callerKey{}).(string); named {
		return ctx
	}
	return WithCaller(ctx, name)
}

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
	PresentedAt  string       `json:"presented_at,omitempty"`

	s *Store
	a *rota.Account
	// skip says why the refresh about to be made cannot reach the provider —
	// not due, no refresh token to present, a provider that cannot refresh, a
	// login past its end — and is "" when it can.
	skip string
	// accepted is whether the provider refused the token presented and the
	// home's login was taken by profile after it (keepRefusal).
	accepted bool
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
	e := &journalEntry{
		Account: a.ID, Provider: a.Provider, Path: path,
		Before: fingerprintOf(a.Token.Refresh), StagedBefore: a.Staged, ExpBefore: journalTime(a.Token.ExpiresAt),
		s: s, a: a,
	}
	p, err := rota.Lookup(a.Provider)
	_, refreshes := p.(rota.Refresher)
	switch {
	case err != nil || !refreshes:
		e.skip = "cannot-refresh"
	case a.Token.Refresh == "":
		e.skip = "no-refresh-token"
	case !a.Expired():
		e.skip = "not-due"
	case a.LoginEnded():
		e.skip = "login-ended" // rota.Refresh presents it to no one
	}
	return e
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

// refreshStart is journalStart for a refresh of the account's own login in a
// claude home, or a skip in its place. A rescue forces its copy's expiry into
// the past to present it, and its line says when the copy truly expires.
func (h *claudeHome) refreshStart(rescue bool) *journalEntry {
	e := h.journalStart(h.refreshPath(rescue))
	if h.copyExpires != 0 {
		e.ExpBefore = journalTime(h.copyExpires)
	}
	return e
}

// lockSkipped is the code for a look Claude Code's refresh lock left out:
// somebody holds it, or it could not be made at all, and why.
func lockSkipped(err error) string {
	switch {
	case errors.Is(err, errLockHeld):
		return "lock-held"
	case errors.Is(err, fs.ErrPermission):
		return "lock-denied"
	case errors.Is(err, syscall.EROFS):
		return "lock-read-only"
	case errors.Is(err, syscall.ENOSPC):
		return "lock-no-space"
	}
	return "lock-failed"
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

// presenting writes the line that goes out with the token, just before the
// request, when the refresh is going to reach the provider at all, so a
// process that dies before the answer leaves word of what it presented. The
// line that says what came of it follows.
func (e *journalEntry) presenting() *journalEntry {
	if e.skip != "" {
		return e
	}
	e.PresentedAt = journalTime(rota.NowMS())
	out := *e
	out.Outcome, out.After, out.StagedAfter, out.ExpAfter = "presenting", e.Before, e.StagedBefore, e.ExpBefore
	e.s.journal(&out)
	return e
}

// finish is the second half: the account as the step left it, and what came
// of it. A rotation of rota's own shows the login lived through any refusal
// kept before it, which is forgotten. The line is written last of all.
func (e *journalEntry) finish(outcome string) {
	a := e.a
	if e.accepted {
		outcome = "accepted-after-refusal"
	}
	if outcome == "rotated" {
		forgetRefusals(a)
	}
	e.Outcome = outcome
	e.After, e.StagedAfter, e.ExpAfter = fingerprintOf(a.Token.Refresh), a.Staged, journalTime(a.Token.ExpiresAt)
	e.s.journal(e)
}

// journalPath is where this store keeps its journal, "" when it keeps none.
func (s *Store) journalPath() string {
	fb, ok := s.backend.(*FileBackend)
	if !ok {
		return ""
	}
	return filepath.Join(fb.Dir, journalName)
}

// journal writes one line, its ts the moment it is written, and never fails
// anything: a line that cannot be written is lost.
func (s *Store) journal(e *journalEntry) {
	path := s.journalPath()
	if path == "" {
		return
	}
	e.TS, e.PID, e.Version = journalTime(rota.NowMS()), os.Getpid(), rota.Version
	line, err := rota.Encode(e)
	if err != nil {
		return
	}
	appendJournal(path, append(line, '\n'))
}

// appendJournal appends one line to the journal at path: a regular file, or
// none yet. It is moved aside first when it is full.
func appendJournal(path string, line []byte) {
	journalMu.Lock()
	defer journalMu.Unlock()
	fi, err := os.Lstat(path)
	switch {
	case err == nil && !fi.Mode().IsRegular():
		return // a link, a pipe, a directory: nothing of the journal's
	case err == nil && fi.Size() >= journalCap:
		rotateJournal(path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|journalOpenFlags, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err = f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		_ = f.Chmod(0o600) // a journal restored, or made, wider than rota makes it
	}
	_, _ = f.Write(line)
}

// rotateJournal moves a full journal aside, under refresh.log.lock: only the
// process that takes the lock moves it, and only while the journal at the path
// is still full, so no second process moves a journal begun a moment ago over
// the one moved aside. A lock somebody holds is left to them. A lock that
// cannot be taken at all — a directory at its path, a file somebody else owns
// — does not stop the journal being moved aside: it is moved without the lock,
// as it was before there was one, or it would grow without end.
func rotateJournal(path string) {
	l, ok, err := tryLockFile(path + ".lock")
	switch {
	case err == nil && !ok:
		return
	case err == nil:
		defer l.Close()
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Size() >= journalCap {
		_ = os.Rename(path, path+".1")
	}
}

// journalState is what a line says of the account and its home, beside when
// and by whom: two lines with the same state say the same thing.
func journalState(e *journalEntry) string {
	var fp, source string
	if e.Home != nil {
		fp, source = e.Home.FP, e.Home.Source
	}
	return strings.Join([]string{e.Path, e.Outcome, e.Before, e.After, e.StagedAfter, fp, source}, "\x00")
}

// alreadySaid reports whether the last adopt line of e's account in the end of
// the journal says what e says. Only a blanked home is asked about: it is seen
// at every look until it changes, and once is enough.
func (s *Store) alreadySaid(e *journalEntry) bool {
	path := s.journalPath()
	if fi, err := os.Lstat(path); path == "" || err != nil || !fi.Mode().IsRegular() {
		return false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|journalOpenFlags, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	from := max(fi.Size()-journalTail, 0)
	tail := make([]byte, fi.Size()-from)
	if _, err := f.ReadAt(tail, from); err != nil && err != io.EOF {
		return false
	}
	lines := bytes.Split(tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var last journalEntry
		if rota.UnmarshalLenient(lines[i], &last) != nil || last.Account != e.Account || last.Path != "adopt" {
			continue
		}
		return journalState(&last) == journalState(e)
	}
	return false
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

// refreshOutcome is what came of a refresh, read straight after the provider
// answered and before anything else changes the account, as a code.
func refreshOutcome(e *journalEntry, changed bool, err error) string {
	a := e.a
	switch {
	case err == nil && changed && !a.Dead:
		return "rotated"
	case e.skip != "":
		return "skipped:" + e.skip
	case a.Dead:
		return "refused:" + oauthCode(a.DeadReason)
	case err != nil:
		return "transient:" + transientWhy(err)
	}
	return "skipped:unchanged"
}

// oauthCode is the protocol's own word for a refusal, which a refusal's reason
// starts with, or "unknown".
func oauthCode(reason string) string {
	code, _, _ := strings.Cut(reason, ":")
	return codeOr(strings.TrimSpace(code), "unknown")
}

// codeOr is s when it is a protocol code — a short run of lower-case letters,
// digits, '_', '.' or '-' — and otherwise.
func codeOr(s, otherwise string) string {
	if s == "" || len(s) > 40 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789_.-") != "" {
		return otherwise
	}
	return s
}

// transientWhy says why a refresh did not get through, in a code: the status
// the provider answered with, the OAuth error code it gave, or a fixed word
// for what stood in the way. Never an error's text, which may quote a reply.
func transientWhy(err error) string {
	var (
		he  *rota.HTTPError
		oe  *rota.OAuthError
		ne  net.Error
		se  *jsonv2.SemanticError
		sye *jsontext.SyntacticError
	)
	switch {
	case errors.As(err, &he):
		return strconv.Itoa(he.Status)
	case errors.As(err, &oe):
		return codeOr(oe.Code, "oauth")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &se), errors.As(err, &sye), errors.Is(err, io.ErrUnexpectedEOF):
		return "decode"
	case errors.As(err, &ne):
		return "network"
	}
	return "error"
}

// refreshLogged is rota.Refresh with its lines in the journal, for every
// refresh of a token rota alone holds: token.env for a claude login, on a
// token in its environment, and <caller>.renew for any other provider's. A
// refresh that had nothing to do writes nothing.
func (s *Store) refreshLogged(ctx context.Context, caller string, a *rota.Account) (bool, error) {
	e := s.journalStart(refreshedAs(caller, a), a).presenting()
	changed, err := rota.Refresh(ctx, a)
	if changed || err != nil {
		e.finish(refreshOutcome(e, changed, err))
	}
	return changed, err
}

// refreshGate is beforeRefresh, the last look at whether whoever asked is still
// there before refreshLogged presents a's token, with a line in the journal when
// the token stays where it is because they have gone.
func (s *Store) refreshGate(ctx context.Context, caller string, a *rota.Account) error {
	if err := beforeRefresh(ctx, a); err != nil {
		s.journalStart(refreshedAs(caller, a), a).finish("skipped:caller-left")
		return err
	}
	return nil
}

// refreshedAs is the path refreshLogged journals a's refresh under.
func refreshedAs(caller string, a *rota.Account) string {
	if rota.SharedHome(a.Provider) {
		return "token.env"
	}
	return caller + ".renew"
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
// demoted. A blanked home is written once for as long as it stays as it is.
//
// A rotation of a living login shows it lived through any refusal kept before
// it, which is forgotten — unless it is the very login a refusal left waiting
// to be confirmed (pendRefusal), which makes that refusal the account's
// last_refusal now, said as if the refresh had taken it at once. A death adopt
// found gets what rota knows of one (blameRefusal).
func (h *claudeHome) journalAdopt(was journalMark) {
	a := h.a
	h.blameRefusal()
	now := h.mark()
	var outcome string
	switch {
	case now.refresh != was.refresh && (was.dead || !sameGrant(now.until, was.until)):
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
	if !a.Dead && now.refresh != was.refresh {
		delete(a.Extra, lastRefusalKey)
		if p, ok := readRefusal(a, pendingRefusalKey); ok {
			delete(a.Extra, pendingRefusalKey)
			if was.dead && p.Home != "" && p.Home == fingerprintOf(now.refresh) {
				h.keptRefusal(refusal{At: p.At, Path: p.Path, FP: p.FP})
				outcome = "accepted-after-refusal"
			}
		}
	}
	e := h.journalStart("adopt")
	e.Before, e.StagedBefore, e.ExpBefore = fingerprintOf(was.refresh), was.staged, journalTime(was.expires)
	e.Outcome, e.After, e.StagedAfter = outcome, fingerprintOf(now.refresh), a.Staged
	if outcome == "blanked" && h.s.alreadySaid(e) {
		return
	}
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

/* ------------------------------------------------------------ refusals --- */

// refusal is what an account keeps of the last time rota presented a refresh
// token the provider had already replaced, and then took the home's login.
type refusal struct {
	At   int64  `json:"at"`
	Path string `json:"path"`
	FP   string `json:"fp"`
}

// pendingRefusal is a refusal whose home login is waiting to be confirmed,
// with that login's fingerprint.
type pendingRefusal struct {
	At   int64  `json:"at"`
	Path string `json:"path"`
	FP   string `json:"fp"`
	Home string `json:"home"`
}

// keepRefusal is what refresh does when the provider refused the token rota
// presented and the home's login was then taken by profile. The account is
// alive on that login, but the refusal is not forgotten: under reuse detection
// presenting a replaced token can revoke the whole login, the home's with it,
// and Claude Code's next refresh there would then be refused too. So it is
// said, kept on the account until a later rotation shows the login lived, and
// journalled as accepted after a refusal.
func (h *claudeHome) keepRefusal(e *journalEntry) {
	e.accepted = true
	h.keptRefusal(refusal{At: rota.NowMS(), Path: e.Path, FP: e.Before})
}

func (h *claudeHome) keptRefusal(r refusal) {
	a := h.a
	raw, _ := rota.Encode(r)
	if a.Extra == nil {
		a.Extra = map[string]string{}
	}
	a.Extra[lastRefusalKey] = string(raw)
	h.warn(fmt.Sprintf("%s: rota presented a token the provider had already replaced; the home's login may have been revoked with it", a))
}

// pendRefusal is what refresh does when the provider refused the token rota
// presented and the home's login could not be confirmed yet: the refusal is
// the same event as one the home's login covered, only settled later, so it
// waits on the account for the look that confirms that login.
func (h *claudeHome) pendRefusal(e *journalEntry) {
	a := h.a
	var home string
	if seen := h.seenHome(); seen != nil {
		home = seen.FP
	}
	raw, _ := rota.Encode(pendingRefusal{At: rota.NowMS(), Path: e.Path, FP: e.Before, Home: home})
	if a.Extra == nil {
		a.Extra = map[string]string{}
	}
	a.Extra[pendingRefusalKey] = string(raw)
}

// forgetRefusals forgets every refusal the account kept.
func forgetRefusals(a *rota.Account) {
	delete(a.Extra, lastRefusalKey)
	delete(a.Extra, pendingRefusalKey)
}

// readRefusal reads what keepRefusal or pendRefusal kept under key.
func readRefusal(a *rota.Account, key string) (pendingRefusal, bool) {
	var r pendingRefusal
	raw := a.Extra[key]
	if raw == "" || rota.UnmarshalLenient([]byte(raw), &r) != nil || r.At <= 0 {
		return pendingRefusal{}, false
	}
	return r, true
}

// blameRefusal adds to the reason of an account that died of Claude Code
// clearing its login that rota had presented a replaced token before it, when
// it had and no rotation since has shown the login lived through that.
func (h *claudeHome) blameRefusal() {
	a := h.a
	r, ok := readRefusal(a, lastRefusalKey)
	if !ok || !a.Dead || !strings.HasPrefix(a.DeadReason, "Claude Code cleared the login") ||
		strings.Contains(a.DeadReason, "rota had presented a replaced token") {
		return
	}
	a.DeadReason += fmt.Sprintf(" (rota had presented a replaced token at %s UTC)", time.UnixMilli(r.At).UTC().Format("15:04"))
}
