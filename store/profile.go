package store

import (
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"strings"

	rota "github.com/professor93/rota/lib"
)

// Claude Code has one more way to sign in that is neither a settings file nor
// the environment: the active profile of the Anthropic configuration
// directory it shares with the `ant` CLI and the Anthropic SDKs
// (platform.claude.com/docs/en/manage-claude/wif-reference, "Profile
// configuration file"). A profile whose authentication type is
// oidc_federation is workload identity federation: Claude Code exchanges an
// identity token for an access token and signs in with that, ranked above the
// claude.ai login in its credential store, which is where rota keeps an
// account's. In Claude Code 2.1.319 its sources go, in order:
// ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN, its file descriptor,
// apiKeyHelper, the profile, and only then the stored login; it logs "Using
// Anthropic profile auth (profile-implicit); this takes precedence over any
// stored claude.ai login". An ANTHROPIC_API_KEY it is given goes before the
// profile too. A user_oauth profile is the other kind, and an active one
// gives way to a stored login, so it is not this file's business.
//
// Every claude launch drops ANTHROPIC_PROFILE, which would name a profile,
// ANTHROPIC_CONFIG_DIR, which would name another directory to find it in,
// and the federation variables that would stand in for one. The profile
// that is active without them is a file under the person's home, which every
// account's Claude Code reads alike. rota changes nothing about it and stops
// no launch for it: it is the person's, and a machine signed in that way may
// mean to be. A launch whose Claude Code would sign in with it instead of the
// account's login says so, once, naming the file and nothing in it.

// maxProfileFile bounds what is read of active_config and of a profile: a
// profile is a few hundred bytes, and a file a thousand times that is not
// one rota reads at every launch.
const maxProfileFile = 256 << 10

// ranksAboveProfile are the variables of a child's environment that give its
// Claude Code a credential ranked above the profile. A claude launch on a
// token or a key sets one of them; a launch on the login in the account's
// home sets none.
var ranksAboveProfile = []string{
	"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "ANTHROPIC_API_KEY",
}

// sayFederationProfile warns, once, when this launch of a's Claude Code, with
// env for its environment and cwd for the directory it starts in, would sign
// in with an Anthropic profile instead of the account's login.
func (s *Store) sayFederationProfile(a *rota.Account, env []string, cwd string) {
	if s.Warn == nil || rota.Flavor(a.Provider) != "claude" {
		return
	}
	if file := federationProfile(env, cwd); file != "" {
		s.Warn(fmt.Sprintf("%s: %s is the active Anthropic profile and signs Claude Code in by workload identity "+
			"federation, in place of this account's login and every other account's; activate another profile "+
			"with `ant profile activate <name>`, or move the file away, to turn it off", a, file))
	}
}

// federationProfile is the file of the oidc_federation profile a Claude Code
// with this environment, started in cwd, signs in with, or "" when it signs in
// some other way. The profile is the one active_config names, the one called
// default when it names none, in the configuration directory
// anthropicConfigDir finds — relative to cwd when the variable that names it
// is relative, as the CLI resolves it there, and none at all when cwd is not
// known. Its JSON has "authentication": {"type": "oidc_federation"}, those
// names exactly, as JSON.parse reads them, so a byte-order mark before it is
// no profile. Claude Code 2.1.319 reads them so, and a file it cannot read is
// none.
func federationProfile(env []string, cwd string) string {
	for _, k := range ranksAboveProfile {
		if envValue(env, k) != "" {
			return ""
		}
	}
	dir := anthropicConfigDir(env)
	if dir == "" {
		return ""
	}
	if !filepath.IsAbs(dir) {
		if cwd == "" {
			return ""
		}
		dir = filepath.Join(cwd, dir)
	}
	raw, _ := readSmallFile(filepath.Join(dir, "active_config"), maxProfileFile)
	name := strings.TrimFunc(string(raw), jsSpace)
	if name == "" {
		name = "default"
	}
	file := filepath.Join(dir, "configs", name+".json")
	if raw, ok := readSmallFile(file, maxProfileFile); !ok || profileType(raw) != "oidc_federation" {
		return ""
	}
	return file
}

// profileType is a profile's authentication type, or "" when it has none.
func profileType(raw []byte) string {
	var doc, auth map[string]jsontext.Value
	if rota.UnmarshalLenient(raw, &doc) != nil || rota.UnmarshalLenient(doc["authentication"], &auth) != nil {
		return ""
	}
	var typ string
	if v := auth["type"]; v.Kind() != '"' || rota.UnmarshalLenient(v, &typ) != nil {
		return ""
	}
	return typ
}

// anthropicConfigDir is the Anthropic configuration directory a Claude Code
// with this environment reads: ANTHROPIC_CONFIG_DIR, else anthropic in
// XDG_CONFIG_HOME, else .config/anthropic in HOME, each value trimmed and a
// blank one passed over; "" when there is none, which is no directory at all
// rather than the one the launch starts in. The environment is the child's,
// so a variable the launch drops is not followed, as the CLI never sees it.
// Claude Code 2.1.319 finds it so on every platform; the SDK's %APPDATA% on
// Windows is not where Claude Code looks.
func anthropicConfigDir(env []string) string {
	if dir := strings.TrimFunc(envValue(env, "ANTHROPIC_CONFIG_DIR"), jsSpace); dir != "" {
		return dir
	}
	if xdg := strings.TrimFunc(envValue(env, "XDG_CONFIG_HOME"), jsSpace); xdg != "" {
		return filepath.Join(xdg, "anthropic")
	}
	if home := strings.TrimFunc(envValue(env, "HOME"), jsSpace); home != "" {
		return filepath.Join(home, ".config", "anthropic")
	}
	return ""
}

// envValue is the value env gives name, the first one as Node takes it, or
// "".
func envValue(env []string, name string) string {
	for _, e := range env {
		if k, v, _ := strings.Cut(e, "="); k == name {
			return v
		}
	}
	return ""
}

// jsSpace is what JavaScript's String.prototype.trim takes off either end:
// white space, which includes the byte-order mark, and line terminators.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}
