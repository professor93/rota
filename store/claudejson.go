package store

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	rota "github.com/professor93/rota/lib"
)

// A claude account shares the person's whole Claude Code configuration by
// default, .claude.json included: the MCP servers a person signed up for,
// the per-project approvals, the folders they trust. That file also carries
// an identity — oauthAccount, who Claude Code thinks it is signed in as —
// and Claude Code's Remote Control acts as whoever it names. Shared, it names
// whoever signed in there last, which for several accounts is wrong for all
// but one. So an account with Remote Control on keeps its own copy: started
// from the person's, then its own, with the one identity in it that is the
// account's, and a one-way fold that keeps bringing over what the person's
// file gains. With the setting off again the copy is put aside, never
// deleted, and the person's file is linked back.
//
// Every change here is made only when nothing is alive in the home: a
// running Claude Code reads and rewrites this file, and swapping it
// underneath would split its state.

const (
	claudeConfigFile = ".claude.json"
	// claudeConfigAside is where an account's own .claude.json waits while
	// Remote Control is off, so turning it on again picks up where it was.
	claudeConfigAside = ".claude.json.own"
	// configBackups is Claude Code's directory of .claude.json backups,
	// which it restores from: shared while the file is, the account's own
	// while the file is.
	configBackups      = "backups"
	configBackupsAside = "backups.own"
	// ownConfigKey records on the account that the .claude.json in its home
	// is its own copy, put there by rota — as distinct from a real file
	// Claude Code left by renaming over a link, which is a fork rota did not
	// make and says so.
	ownConfigKey = "own_claude_json"
)

// ownConfig arranges an account's .claude.json for its Remote Control
// setting and reports whether the account's own copy is the one in place.
// While anything is alive in the home nothing is switched, and the person is
// told when the setting will take effect.
func (s *Store) ownConfig(a *rota.Account, dst, personal string, quiet bool) (bool, error) {
	path := filepath.Join(dst, claudeConfigFile)
	fi, err := os.Lstat(path)
	exists := err == nil
	isFile := exists && fi.Mode().IsRegular()
	inEffect := isFile && a.Extra[ownConfigKey] != ""
	switch {
	case a.RemoteControl && inEffect:
		if !quiet {
			return true, nil
		}
		return true, foldConfig(a, path, personal)
	case a.RemoteControl:
		if !quiet {
			s.say(remoteControlWaits(a))
			return false, nil
		}
		return true, turnOwnConfigOn(a, dst, personal, fi, exists)
	case inEffect:
		if !quiet {
			s.say(remoteControlWaits(a))
			return true, nil
		}
		return false, turnOwnConfigOff(a, dst)
	}
	if a.Extra[ownConfigKey] != "" {
		delete(a.Extra, ownConfigKey) // the copy it named is gone
	}
	return false, nil
}

// remoteControlWaits is what a person is told when the setting cannot change
// anything yet.
func remoteControlWaits(a *rota.Account) string {
	return fmt.Sprintf("%s: Claude Code is running in its home, so its remote control setting takes effect when "+
		"the account's running sessions end", a)
}

// RemoteControlNow says whether an account's remote control setting can
// work by its next launch. It is nil when it can, or when the setting is off
// and already in effect. Otherwise it says why, as one of two verdicts:
// rota.ErrUnsupported when the account does not run on a login of its own in
// its home — Claude Code refuses Remote Control for a token in its
// environment — and rota.ErrBusy when the setting is waiting for the
// account's running sessions to end before its configuration file can
// change.
func (s *Store) RemoteControlNow(a *rota.Account) error {
	if rota.Flavor(a.Provider) != "claude" {
		return nil
	}
	if err := s.CheckRemoteControl(a); err != nil {
		return err
	}
	if a.RemoteControl && !s.keepsLogin(a) {
		return fmt.Errorf("%w: Remote Control needs %s's own stored login, and it does not run on one: %s",
			rota.ErrUnsupported, a, s.whyNoLogin(a))
	}
	if a.ConfigDir != "" {
		return nil // an account's own directory is its own world already
	}
	fi, err := os.Lstat(filepath.Join(s.ownHome(a), claudeConfigFile))
	inEffect := err == nil && fi.Mode().IsRegular() && a.Extra[ownConfigKey] != ""
	if inEffect == a.RemoteControl || !s.InUse(a) {
		return nil
	}
	return fmt.Errorf("%w: %s", rota.ErrBusy, remoteControlWaits(a))
}

// CheckRemoteControl refuses Remote Control for an account told a
// configuration directory of the person's choosing: rota keeps no login
// there, so the account runs on a token in its environment, which Claude
// Code refuses Remote Control for. It is asked of the account as it is about
// to be, before the setting is saved, and is nil for everything else.
func (s *Store) CheckRemoteControl(a *rota.Account) error {
	if !a.RemoteControl || rota.Flavor(a.Provider) != "claude" || s.owns(a) {
		return nil
	}
	return fmt.Errorf("%w: %s cannot have Remote Control: it runs on a token because its configuration directory is one "+
		"you chose, and Remote Control needs the home rota keeps for an account (clear `--config`)", rota.ErrInvalidRequest, a)
}

// turnOwnConfigOn puts the account's own .claude.json in place: the one put
// aside the last time the setting was on, when there is one, and otherwise a
// copy of the person's — without their identity, which is replaced by the
// account's. A real file already there is a fork Claude Code made by
// renaming over the link, and is the account's own already: it stays as it
// is. Claude Code's backups of the file come back with it.
func turnOwnConfigOn(a *rota.Account, dst, personal string, fi fs.FileInfo, exists bool) error {
	path := filepath.Join(dst, claudeConfigFile)
	if !exists || !fi.Mode().IsRegular() {
		if exists && fi.Mode()&fs.ModeSymlink != 0 {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		aside := filepath.Join(dst, claudeConfigAside)
		if _, err := os.Lstat(aside); err == nil {
			if err := os.Rename(aside, path); err != nil {
				return err
			}
		} else {
			if err := writeAtomic(path, personalCopy(personal)); err != nil {
				return err
			}
		}
	}
	if err := swapIn(dst, configBackupsAside, configBackups); err != nil {
		return err
	}
	if a.Extra == nil {
		a.Extra = map[string]string{}
	}
	a.Extra[ownConfigKey] = "1"
	return foldConfig(a, path, personal)
}

// turnOwnConfigOff puts the account's own .claude.json aside and leaves the
// place for the link to the person's, which the mirror makes next. Nothing
// is deleted: what the account added is waiting for the setting to come
// back on.
func turnOwnConfigOff(a *rota.Account, dst string) error {
	if err := putAside(dst, claudeConfigFile, claudeConfigAside); err != nil {
		return err
	}
	if fi, err := os.Lstat(filepath.Join(dst, configBackups)); err == nil && fi.IsDir() {
		if err := putAside(dst, configBackups, configBackupsAside); err != nil {
			return err
		}
	}
	delete(a.Extra, ownConfigKey)
	return nil
}

// putAside renames name to aside within dir. An older copy already under
// the aside name is kept too, under a name of its own.
func putAside(dir, name, aside string) error {
	to := filepath.Join(dir, aside)
	if _, err := os.Lstat(to); err == nil {
		if err := os.Rename(to, to+"-"+strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
			return err
		}
	}
	return os.Rename(filepath.Join(dir, name), to)
}

// swapIn brings an entry put aside back to its own name, replacing a link
// there. A real entry already under the name is the account's and stays.
func swapIn(dir, aside, name string) error {
	from := filepath.Join(dir, aside)
	if _, err := os.Lstat(from); err != nil {
		return nil
	}
	to := filepath.Join(dir, name)
	if fi, err := os.Lstat(to); err == nil {
		if fi.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		if err := os.Remove(to); err != nil {
			return err
		}
	}
	return os.Rename(from, to)
}

// personalCopy is the person's .claude.json without the identity in it, the
// starting point of an account's own: `{}` when they have none, or none that
// can be read.
func personalCopy(personal string) []byte {
	raw, err := os.ReadFile(personal)
	if err != nil {
		return []byte("{}")
	}
	obj, ok := parseObject(raw)
	if !ok {
		return []byte("{}")
	}
	obj.drop("oauthAccount")
	return obj.indented()
}

// foldConfig brings an account's own .claude.json up to date: the identity
// in it is the account's, and what the person's file has gained since is
// added — one way and only ever added. A top-level key the account's file
// lacks, a project it has no entry for, an MCP server it does not have, and
// a folder the person has since trusted. Nothing the account's file holds is
// removed or overwritten, the person's identity is never brought over, and
// the person's file is only read. The file is written only when something
// changed.
func foldConfig(a *rota.Account, path, personal string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	own, ok := parseObject(raw)
	if !ok {
		return nil // not something rota can add to without losing what is there
	}
	changed := false
	if praw, err := os.ReadFile(personal); err == nil {
		if theirs, ok := parseObject(praw); ok {
			changed = fold(own, theirs)
		}
	}
	if setIdentity(own, a) {
		changed = true
	}
	if !changed {
		return nil
	}
	return writeAtomic(path, own.indented())
}

// fold adds to own what theirs has and own lacks, as foldConfig describes.
func fold(own, theirs *jsonObject) bool {
	changed := false
	for i, name := range theirs.names {
		if name == "oauthAccount" {
			continue
		}
		mine, have := own.get(name)
		switch {
		case !have:
			own.set(name, theirs.values[i])
			changed = true
		case name == "mcpServers":
			if merged, ok := addMissing(mine, theirs.values[i], nil); ok {
				own.set(name, merged)
				changed = true
			}
		case name == "projects":
			if merged, ok := addMissing(mine, theirs.values[i], trustFolded); ok {
				own.set(name, merged)
				changed = true
			}
		}
	}
	return changed
}

// addMissing adds to the object mine the entries of theirs it lacks, and
// lets both reconcile an entry they each have. It reports whether anything
// changed.
func addMissing(mine, theirs jsontext.Value, both func(mine, theirs jsontext.Value) (jsontext.Value, bool)) (jsontext.Value, bool) {
	m, ok := parseObject(mine)
	if !ok {
		return nil, false
	}
	t, ok := parseObject(theirs)
	if !ok {
		return nil, false
	}
	changed := false
	for i, name := range t.names {
		v, have := m.get(name)
		switch {
		case !have:
			m.set(name, t.values[i])
			changed = true
		case both != nil:
			if nv, ok := both(v, t.values[i]); ok {
				m.set(name, nv)
				changed = true
			}
		}
	}
	return m.compact(), changed
}

// trustFolded marks a project trusted in the account's file when the person
// has trusted it since. Trust only ever spreads this way: a folder the
// person trusts is one they meant to trust, and a folder the account
// trusted stays trusted.
func trustFolded(mine, theirs jsontext.Value) (jsontext.Value, bool) {
	const key = "hasTrustDialogAccepted"
	t, ok := parseObject(theirs)
	if !ok {
		return nil, false
	}
	if v, ok := t.get(key); !ok || string(bytes.TrimSpace(v)) != "true" {
		return nil, false
	}
	m, ok := parseObject(mine)
	if !ok {
		return nil, false
	}
	if v, ok := m.get(key); ok && string(bytes.TrimSpace(v)) == "true" {
		return nil, false
	}
	m.set(key, jsontext.Value("true"))
	return m.compact(), true
}

// setIdentity makes oauthAccount the account's own. An identity already
// naming this account keeps everything else Claude Code wrote into it, and
// is only given what it lacks or has wrong; anything else is replaced whole.
// An account without a uuid has no identity to give, and the file is left
// as it is.
func setIdentity(obj *jsonObject, a *rota.Account) bool {
	if a.UUID == "" {
		return false
	}
	// The names come from the profile read at login, under the keys lib
	// keeps them in.
	fields := []struct {
		key, value string
		fill       bool // only where the file has nothing; Claude Code keeps it current itself
	}{
		{"accountUuid", a.UUID, false},
		{"emailAddress", a.Email, false},
		{"organizationUuid", a.Org, false},
		{"organizationName", a.Extra["organization_name"], true},
		{"displayName", a.Extra["display_name"], true},
	}
	cur, have := obj.get("oauthAccount")
	id, ok := parseObject(cur)
	if !have || !ok || stringField(id, "accountUuid") != a.UUID {
		id = &jsonObject{}
	}
	changed := false
	for _, f := range fields {
		if f.value == "" {
			continue
		}
		if _, has := id.get(f.key); has && (f.fill || stringField(id, f.key) == f.value) {
			continue
		}
		q, err := jsontext.AppendQuote(nil, f.value)
		if err != nil {
			continue
		}
		id.set(f.key, q)
		changed = true
	}
	if changed {
		obj.set("oauthAccount", id.compact())
	}
	return changed
}

/* --------------------------------------------- a JSON object, in order --- */

// jsonObject is one JSON object as a list of members in the order they were
// written, each value exactly as it was. Claude Code's configuration file is
// rewritten here only by adding to it, and a rewrite should not shuffle what
// somebody may be reading with their own eyes.
type jsonObject struct {
	names  []string
	values []jsontext.Value
}

// parseObject reads one whole JSON object, leniently: Claude Code writes
// these files, not rota.
func parseObject(raw []byte) (*jsonObject, bool) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil, false
	}
	obj := &jsonObject{}
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil || tok.Kind() != '"' {
			return nil, false
		}
		name := tok.String() // a token is only good until the next read
		v, err := dec.ReadValue()
		if err != nil {
			return nil, false
		}
		obj.set(name, v.Clone())
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, false
	}
	return obj, true
}

func (o *jsonObject) get(name string) (jsontext.Value, bool) {
	for i, n := range o.names {
		if n == name {
			return o.values[i], true
		}
	}
	return nil, false
}

func (o *jsonObject) set(name string, v jsontext.Value) {
	for i, n := range o.names {
		if n == name {
			o.values[i] = v
			return
		}
	}
	o.names = append(o.names, name)
	o.values = append(o.values, v)
}

func (o *jsonObject) drop(name string) {
	for i, n := range o.names {
		if n == name {
			o.names = append(o.names[:i], o.names[i+1:]...)
			o.values = append(o.values[:i], o.values[i+1:]...)
			return
		}
	}
}

func (o *jsonObject) compact() jsontext.Value {
	out := []byte{'{'}
	for i, name := range o.names {
		if i > 0 {
			out = append(out, ',')
		}
		out, _ = jsontext.AppendQuote(out, name)
		out = append(append(out, ':'), o.values[i]...)
	}
	return append(out, '}')
}

// indented is the object as Claude Code writes the file: two spaces, and a
// value that cannot be re-indented written as it was.
func (o *jsonObject) indented() []byte {
	v := o.compact()
	if err := v.Indent(jsontext.WithIndent("  "), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
		return o.compact()
	}
	return append(v, '\n')
}

// stringField is a member's value when it is a JSON string, and "" otherwise.
func stringField(o *jsonObject, name string) string {
	v, ok := o.get(name)
	if !ok {
		return ""
	}
	var s string
	if rota.UnmarshalLenient(v, &s) != nil {
		return ""
	}
	return s
}
