package rota

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// A claude account reaches Claude Code by one of two routes, and this file
// is both of them.
//
// The stored route gives the account a login of its own inside its home:
// Claude Code's own credential store there holds the access token and the
// refresh token behind it, nothing that authenticates is set in the
// environment, and Claude Code refreshes the login by itself. That is the
// only arrangement in which a home's processes stay signed in. Claude Code
// runs many of them in one home — every window, the daemon, the background
// sessions the daemon hosts — and it shares a stored login between them by
// its own protocol: whichever refreshes first writes the store, and the rest
// read it back. A token in the environment is frozen into the process it was
// given to. A daemon keeps the one it started with for life and hands it to
// every background session it starts after; eight hours later the provider
// has revoked it, and everything the daemon hosts says "OAuth token revoked"
// while the window that launched it is long gone. Remote Control, too, is
// refused for any token from the environment.
//
// The environment route is the old one, kept for where a home is not the
// answer: a run with no home at all (Stage(a, "")), a hermetic run, an
// account whose login is dead but which holds a long-lived token, and
// Windows, where rota keeps to what it has always done.
//
// On the stored route rota writes the login into the home once and then only
// reads: Claude Code owns it from the first start. What a home's processes
// rotated is taken back by Adopt, and the login is written again only when
// the home does not hold the account's current one — after a fresh login
// with rota, or when nothing is there.

// The one file of a Claude Code configuration directory this package reads
// and writes, and the entry in it that is the login. On macOS Claude Code
// keeps the credential store in the keychain first and in this file only
// when the keychain item is absent or refused a write; an application
// reading a home there hands Adopt an fs.FS that answers .credentials.json
// with the keychain item, or with the file when that is the newer login.
// rota's store does.
const (
	claudeCredentials = ".credentials.json"
	claudeOAuthKey    = "claudeAiOauth"
)

// claudeCleared is why an account whose login Claude Code blanked is dead. It
// blanks a login when a refresh of its own comes back refused, which is Claude
// Code's verdict and not proof the login is over, and "Login expired · Please
// run /login" is no proof either: rota has refreshed a login successfully after
// that message. AdoptFS marks the account dead all the same, because it reads
// and never refreshes. rota's own store looks first: it reads the store's
// claudeAiOauth entry itself, and while the account's current login is blanked
// it leaves AdoptFS out and presents the account's refresh token once with
// Refresh. An application that wants the same does it the same way, before it
// calls AdoptFS for that home.
const claudeCleared = "Claude Code cleared the login in its home after a refresh of its own was refused"

// ClaudeCredentialVars are the inherited variables that would give Claude
// Code a credential, send one somewhere else, or move its credential store —
// the token variable itself included — as a copy. It is for a caller that
// starts Claude Code for something other than a run, stopping a home's
// daemon say, and must hand it no credential at all.
func ClaudeCredentialVars() []string {
	return dropList(append([]string{"CLAUDE_CODE_OAUTH_TOKEN"}, claudeCompeting...)...)
}

// claudeCompeting are the variables that would authenticate Claude Code
// some other way, send its requests or its credential somewhere else, change
// which login, organization or configuration it uses, or tell it that it
// runs inside another Claude Code: every route drops them, so a shell
// profile, a host or an outer session cannot quietly bill another account or
// read another account's login. A stray ANTHROPIC_BASE_URL alone would send
// an OAuth token to a third party, CLAUDE_CODE_OAUTH_CLIENT_ID would have
// Claude Code refresh the login under a client that is refused, and
// CLAUDE_CODE_CHILD_SESSION would leave an interactive run out of --resume,
// --continue, the prompt history and the agents list.
//
// They are exact names, never prefixes, and each changes how Claude Code
// itself signs in or where it sends its requests — as
// code.claude.com/docs/en/env-vars documents them, and as Claude Code's own
// lists of its credentials, endpoints and provider switches name them. What
// configures a provider stays: its project, region, resource or workspace,
// its skip-auth switch, and the cloud vendors' own variables (AWS_PROFILE,
// GOOGLE_CLOUD_PROJECT and their like). Claude Code reads those only once a
// CLAUDE_CODE_USE_* switch below has chosen that provider, and the programs a
// run starts — Anthropic's SDKs, the clouds' own tools — read them too. A
// provider's base URL is an endpoint, and goes with ANTHROPIC_BASE_URL.
// Workload identity federation is treated as a provider too: what selects
// it — ANTHROPIC_PROFILE, ANTHROPIC_CONFIG_DIR, the rule and organization
// pair — and its identity token are dropped, while its workspace, service
// account, scope and cache directory stay as its configuration. Claude Code
// reads those only while it resolves a federation, and one it resolves
// outranks the login whether they are there or not.
// CLAUDE_CONFIG_DIR stays as well; a route that points Claude Code somewhere
// sets it, and one that does not leaves the person's own.
var claudeCompeting = []string{
	// A credential of another kind.
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_AWS_API_KEY",
	"ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_FOUNDRY_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	"CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR",
	"CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_OAUTH_SCOPES",
	"CLAUDE_CODE_SUBSCRIPTION_TYPE", "CLAUDE_CODE_RATE_LIMIT_TIER",
	"CLAUDE_CODE_SESSIONS_OAUTH_TOKEN", "CLAUDE_CODE_SESSION_ACCESS_TOKEN", "CLAUDE_SESSION_INGRESS_TOKEN_FILE",
	"CLAUDE_BRIDGE_OAUTH_TOKEN", "CLAUDE_TRUSTED_DEVICE_TOKEN",
	// An Anthropic profile or workload identity federation, which outrank the
	// login: what selects one, and its identity token.
	"ANTHROPIC_PROFILE", "ANTHROPIC_CONFIG_DIR", "ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID",
	"ANTHROPIC_IDENTITY_TOKEN", "ANTHROPIC_IDENTITY_TOKEN_FILE",
	// A host that signs Claude Code in, or refreshes its login for it.
	"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "CLAUDE_CODE_HOST_AUTH_ENV_VAR", "CLAUDE_CODE_HOST_CREDS_FILE",
	"CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH", "CLAUDE_CODE_SDK_HAS_SESSIONS_OAUTH_REFRESH",
	"CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH",
	// The OAuth client, where its login is kept, and whose login it is.
	"CLAUDE_CODE_OAUTH_CLIENT_ID", "CLAUDE_CODE_CUSTOM_OAUTH_URL", "CLAUDE_SECURESTORAGE_CONFIG_DIR",
	"CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_ACCOUNT_UUID", "CLAUDE_CODE_USER_EMAIL", "CLAUDE_CODE_ORGANIZATION_UUID",
	// Another provider.
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_MANTLE",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_USE_GATEWAY",
	// Another endpoint.
	"ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL",
	"ANTHROPIC_AWS_BASE_URL", "ANTHROPIC_GOOGLE_CLOUD_BASE_URL", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
	"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", "ANTHROPIC_UNIX_SOCKET", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_API_BASE_URL",
	"CLAUDE_BRIDGE_BASE_URL", "CLAUDE_BRIDGE_SESSION_INGRESS_URL", "CLAUDE_REMOTE_TOOLS_BRIDGE_URL",
	"CLAUDE_CODE_ARTIFACTS_API_BASE_URL", "CLAUDE_CODE_ARTIFACTS_API_TOKEN", "CLAUDE_CODE_ARTIFACT_ASSET_BASE_URL",
	"CLAUDE_CODE_ARTIFACT_LIVE_BASE_URL", "CLAUDE_CODE_ARTIFACT_SYNC_BASE_URL", "CLAUDE_CODE_ARTIFACT_VIEWER_BASE_URL",
	"CLAUDE_CODE_MEMORY_API_BASE_URL", "CLAUDE_CODE_MEMORY_API_TOKEN", "CLAUDE_CODE_GB_BASE_URL", "VOICE_STREAM_BASE_URL",
	// The marks of a process another Claude Code started: a run on an account
	// is not one by being started from one.
	"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SPAWNED_BY_SERVER", "CLAUDE_CODE_REMOTE", "CLAUDE_CODE_REMOTE_SESSION_ID",
	"CLAUDE_CODE_REMOTE_ENVIRONMENT_TYPE", "CLAUDE_CODE_ENVIRONMENT_KIND", "CLAUDE_CODE_CONTAINER_ID",
}

// storedLoginPlatform is whether this platform takes the stored route at
// all. Windows does not: rota keeps it on the environment route it has
// always had there. A variable rather than a constant so a test on any
// machine can see what Windows does.
var storedLoginPlatform = runtime.GOOS != "windows"

// diskLoginPlatform is whether this package's own on-disk conveniences —
// Stage and Launch writing a login into a home, Adopt reading one back —
// may take the stored route. macOS may not: Claude Code moves the login
// into a keychain item there, which a file read cannot see, and an
// application that adopted from the file and then refreshed would present a
// refresh token Claude Code had already spent. On macOS the stored route is
// reached through StagePlan and AdoptFrom, where the application hands over
// the keychain item's content and removes the item before it writes — as
// rota's store does.
var diskLoginPlatform = runtime.GOOS != "darwin"

// claudeStored reports whether this launch takes the stored route: there is
// a home to keep the login in, the login is one Claude Code can keep — a
// refresh token and an expiry, which is what makes it a login rather than a
// bare token — and it is not dead. It needs an access token too: a login
// written without one would be Claude Code's to refresh, with a refresh token
// rota also holds, so an account held as a refresh token alone stays off this
// route until it has been refreshed.
func claudeStored(a *Account, home string) bool {
	return storedLoginPlatform && home != "" && !a.Dead && !a.APIKey() &&
		a.Token.Access != "" && a.Token.Refresh != "" && a.Token.ExpiresAt != 0
}

// claudeEnvCommand is the environment route: the token in
// CLAUDE_CODE_OAUTH_TOKEN, and the account's own configuration directory
// when it names one.
func claudeEnvCommand(a *Account) *Command {
	if a.APIKey() {
		return claudeKeyCommand(a)
	}
	// A long-lived token wins whenever the account has one worth using. A
	// token in the environment cannot be replaced inside a process that
	// already holds it — Claude Code refuses by design to adopt another
	// after a 401 on one — so on this route the ordinary eight-hour token
	// simply stops a long process when it expires. The long one is the same
	// account by a different key.
	access := a.Token.Access
	if long := a.LongAccess(); long != "" {
		access = long
	}
	env := []string{"CLAUDE_CODE_OAUTH_TOKEN=" + access}
	if a.ConfigDir != "" {
		// Claude Code keeps memory, skills and settings here. Unset, it
		// finds the person's own — which is right until an account is meant
		// for one project, and then it is exactly wrong. The value is the
		// account's own field, not the home argument: on this route there may
		// be no home at all, and an SDK must not depend on one caller's habit
		// of passing ConfigDir as the home.
		env = append(env, "CLAUDE_CONFIG_DIR="+a.ConfigDir)
	}
	return &Command{Bin: "claude", Env: env, Drop: dropList(claudeCompeting...)}
}

// claudeKeyCommand is the environment route for an account that runs on an
// API key: the key in ANTHROPIC_API_KEY, which Claude Code sends as X-Api-Key,
// and nothing else that could authenticate. CLAUDE_CODE_OAUTH_TOKEN is
// dropped with the rest, and a long-lived token the account somehow holds is
// not used: a key account is billed to its key and to nothing else.
func claudeKeyCommand(a *Account) *Command {
	env := []string{"ANTHROPIC_API_KEY=" + a.Token.Access}
	if a.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+a.ConfigDir)
	}
	var drop []string
	for _, v := range ClaudeCredentialVars() {
		if v != "ANTHROPIC_API_KEY" {
			drop = append(drop, v)
		}
	}
	return &Command{Bin: "claude", Env: env, Drop: drop}
}

// claudeStoredCommand is the stored route: Claude Code pointed at the home
// its login is kept in, and nothing that authenticates. A token inherited in
// CLAUDE_CODE_OAUTH_TOKEN would outrank the stored login and freeze the
// process again, so it is dropped with the rest.
//
// The directory is the home argument, not the account's ConfigDir: the
// login is wherever it was written, and the CLI has to be pointed there.
func claudeStoredCommand(home string) *Command {
	return &Command{
		Bin:  "claude",
		Env:  []string{"CLAUDE_CONFIG_DIR=" + home},
		Drop: dropList(append([]string{"CLAUDE_CODE_OAUTH_TOKEN"}, claudeCompeting...)...),
	}
}

// Plan is Launch as values. On the stored route the login is a file to
// write only when the home does not already hold the account's current one
// — Staged says, once adoption has run — and none otherwise; on the
// environment route there is never a file.
//
// The file is the credential store with nothing in it but the login. The
// real store holds more than that — Claude Code keeps MCP servers' OAuth
// tokens and other secrets of its own beside the login — so whoever writes
// the planned file must replace only the login in what is already there:
// MergeClaudeCredentials does exactly that. And whoever writes it must know
// that no Claude Code process is alive in the home at that moment, because
// from its first start the store is Claude Code's.
func (claudeProvider) Plan(_ context.Context, a *Account, home string) (*Command, []StagedFile, error) {
	if !claudeStored(a, home) {
		return claudeEnvCommand(a), nil, nil
	}
	cmd := claudeStoredCommand(home)
	if a.Staged == fingerprint(a.Token.Refresh) {
		return cmd, nil, nil
	}
	raw, err := claudeCredentialDoc(a)
	if err != nil {
		return nil, nil, err
	}
	return cmd, []StagedFile{{Path: claudeCredentials, Mode: 0o600, Content: raw}}, nil
}

// StoresLogin reports whether planning this account into home takes the
// stored route (StagePlan; Stage too, except on macOS).
func (claudeProvider) StoresLogin(a *Account, home string) bool { return claudeStored(a, home) }

// Join is the stored route's command whatever state the account is in:
// Claude Code pointed at the home, on whatever login is there, with nothing
// written and nothing of the account's handed over. nil where this platform
// keeps the environment route, and for an account that runs on an API key,
// which no login in a home stands for.
func (claudeProvider) Join(a *Account, home string) *Command {
	if !storedLoginPlatform || home == "" || a.APIKey() {
		return nil
	}
	return claudeStoredCommand(home)
}

// HoldsLogin reports whether a home's credential store holds a login Claude
// Code can use: a refresh token and an expiry. Without either Claude Code
// treats a credential as good for inference only and never refreshes it,
// and an empty refresh token is how it marks a login it has given up on.
func (claudeProvider) HoldsLogin(fsys fs.FS) bool {
	l, ok := readClaudeLogin(fsys)
	return ok && l.usable()
}

// Adopt reads back what Claude Code left in a home on this disk. On macOS it
// reads nothing, because Stage never stores a login there (see
// diskLoginPlatform): an application that keeps one there itself reads it
// with AdoptFrom, keychain item included.
func (c claudeProvider) Adopt(a *Account, home string) error {
	if !diskLoginPlatform {
		return nil
	}
	return c.AdoptFS(a, os.DirFS(home))
}

// AdoptFS reads a home's credential store into the account. It only reads,
// so it is safe while Claude Code runs there, and it always comes before any
// use of the account's tokens: a login the home's processes rotated is the
// newer one, and the provider refuses the one it replaced.
//
// What it finds decides what the account believes about the home, recorded
// in Staged:
//   - no store, or a store with no login in it: the home holds no login, and
//     the next staging writes one;
//   - a store that is there and cannot be read — an empty or half-written
//     file, a read that failed: nothing changes, and ErrUnreadableLogin says
//     so. Claude Code may be in the middle of writing it, and a store taken
//     for empty is one written over;
//   - the login Claude Code blanks when a refresh of its own was refused: if
//     it was the account's current login, the login is dead, and only a fresh
//     one — `rota login`, or `/login` inside a window — brings it back; if it
//     was an older one, the home simply holds nothing usable. Whether to
//     present the account's own refresh token first is the application's
//     call, made before AdoptFS: see claudeCleared;
//   - the account's own refresh token: in sync;
//   - another refresh token of the same login: a rotation by Claude Code,
//     taken when it is newer than what the account has. A refresh keeps the
//     login's refreshTokenExpiresAt, give or take the second a reply in
//     whole seconds may take off it (sameGrant), so a value within a minute
//     of the account's is how a rotation is told apart, with no network
//     call — unless Claude Code records the login as another account's
//     (tokenAccount), which no rotation of this one can be. One that is not
//     newer is behind: the home does not hold the account's current login,
//     and the next staging writes it. What this package itself wrote there
//     before a refresh of its own stays recorded as that write — it is what
//     proves the login there spent (ClaudeHomeSpent) — and any other older
//     login is recorded as nothing of the account's. A rotation is taken
//     even when the home is recorded as holding nothing of the account's,
//     provided the account knows its login's refreshTokenExpiresAt and the
//     rotation expires later than its own token: a look that found the
//     store empty, blanked, without an expiry, behind or unreadable says
//     nothing about what Claude Code wrote there after it, and an account
//     that passed over its own login rotated would go on to present the
//     refresh token that rotation replaced;
//   - a new login — a refreshTokenExpiresAt more than a minute from the
//     account's, one where only one side has any, a login Claude Code
//     records as another account's, or any living login in the home of a
//     dead account — which somebody made by running /login inside that
//     home. Nothing of it is taken here, because the home says
//     nothing trustworthy about whose it is: it comes back as a *NewLogin
//     for the application to check (NewLogin.Identify) and then Accept or
//     Refuse. In a home recorded as holding nothing of the account's — it was
//     logged in again with rota since, say — the ends tell the two apart,
//     because a login ends a fixed time after it was signed in: one ending
//     more than a minute after the account's login was signed in after it,
//     with /login inside the home, and comes back as a *NewLogin like any
//     other, so nothing writes over it before it is checked — as does one
//     Claude Code records as another account's; one ending more than a
//     minute before is the login from before the account's current one,
//     older whatever else it says, and the next staging writes over it. So
//     is any login there when either end is unknown, and one of a grant
//     already refused (Refuse), which is not asked about again.
func (claudeProvider) AdoptFS(a *Account, fsys fs.FS) error {
	if a.APIKey() {
		// A key account keeps no login in its home, so nothing found there is
		// its own — and a login somebody made there is not a key.
		return nil
	}
	l, ok, err := loadClaudeLogin(fsys)
	if err != nil {
		return failf(ErrUnreadableLogin, "%s: the credential store in its home cannot be read: %v", a, err)
	}
	current := fingerprint(a.Token.Refresh)
	switch {
	case !ok:
		a.Staged = stagedNone
		return nil
	case l.RefreshToken == "":
		held := current != "" && a.Staged == current
		if held {
			a.Dead = true
		} else {
			a.Staged = stagedNone
		}
		if a.Dead && a.DeadReason == "" {
			a.DeadReason = claudeCleared
		}
		return nil
	case l.ExpiresAt == 0:
		// A refresh token with no expiry is not a login Claude Code keeps;
		// it reads it as inference-only. Nothing in it is worth taking, and
		// the next staging writes a real one over it.
		a.Staged = stagedNone
		return nil
	case l.RefreshToken == a.Token.Refresh:
		a.Staged = current
		// A refresh that did not rotate the refresh token still replaced the
		// access token, and the provider has revoked the one it replaced.
		if l.AccessToken != "" && int64(l.ExpiresAt) > a.Token.ExpiresAt {
			a.Token.Access, a.Token.ExpiresAt = l.AccessToken, int64(l.ExpiresAt)
		}
		l.keep(a)
		return nil
	}
	switch {
	case a.Dead:
		// The login this account had is over, so a living one in its home
		// is somebody signing in again there — whatever it says about itself.
		return &NewLogin{Access: l.AccessToken, login: l}
	case a.Staged == stagedNone:
		// The last look found the home not holding the account's current
		// login, and what is there now is told by when it ends. The account's
		// own grant, expiring later than the account's token, is Claude Code
		// having refreshed the account's own login since that look, whatever
		// the look found: it is the newest there is, and the account's token
		// is spent. A grant the provider already said is somebody else's
		// (Refuse) is passed over without asking again, rotated or not, and the
		// next write replaces it. One ending earlier predates the account's
		// login — it was logged in again with rota since — so it is older
		// whoever's it says it is, and the next write replaces it too; and so,
		// as ever, is anything when either end is unknown. A grant ending later
		// was signed in after the account's login, with /login in the home, and
		// is checked like any new login rather than written over; so is one
		// Claude Code records as another account's.
		home, own, refused := l.refreshUntil(), a.Extra[claudeRefreshUntil], a.Extra[claudeRefusedGrant]
		switch {
		case home == "" || own == "":
		case l.ofGrant(a):
			if int64(l.ExpiresAt) > a.Token.ExpiresAt {
				l.adopt(a)
			}
		case refused != "" && sameGrant(home, refused):
		case endsAfter(own, home):
		case endsAfter(home, own) || l.otherAccount(a):
			return &NewLogin{Access: l.AccessToken, login: l}
		}
		return nil
	case !l.ofGrant(a):
		return &NewLogin{Access: l.AccessToken, login: l}
	}
	if fingerprint(l.RefreshToken) == a.Staged {
		// Behind, and the very login this package wrote: the account has
		// refreshed it away since. Staged already says the home does not
		// hold the current login, and goes on saying which one it does hold,
		// because that is the only proof that login is spent — a look at the
		// home must not erase what the write that replaces it relies on.
		return nil
	}
	if int64(l.ExpiresAt) < a.Token.ExpiresAt {
		// Behind: an older login, not this package's write. A Claude Code
		// started on it would present a token the account has moved past.
		// Recorded, so the next staging puts the current one in.
		a.Staged = stagedNone
		return nil
	}
	l.adopt(a)
	return nil
}

// ClaudeHomeSpent reports whether a home's credential store holds the login
// this package recorded writing there (Staged) and the account has since
// refreshed away. That login is certainly spent — the provider issued its
// successor, which the account holds — so every Claude Code process still
// on it is signed out at its next refresh anyway, and an application may
// write the account's current login over it even while something runs
// there. It reads only, and changes nothing.
func ClaudeHomeSpent(a *Account, fsys fs.FS) bool {
	l, ok := readClaudeLogin(fsys)
	return ok && l.usable() && l.RefreshToken != a.Token.Refresh &&
		a.Staged != "" && a.Staged != stagedNone && fingerprint(l.RefreshToken) == a.Staged
}

// adopt takes the whole login into the account.
func (l *claudeLogin) adopt(a *Account) {
	a.Token.Refresh = l.RefreshToken
	if l.AccessToken != "" {
		a.Token.Access = l.AccessToken
	}
	a.Token.ExpiresAt = int64(l.ExpiresAt)
	if len(l.Scopes) > 0 {
		a.Token.Scopes = l.Scopes
	}
	l.keep(a)
	a.Staged = fingerprint(l.RefreshToken)
	a.Dead, a.DeadReason = false, ""
}

// claudeRefusedGrant is where an account remembers, by when it ends, the grant
// of the last login in its home the provider said is somebody else's (Refuse).
const claudeRefusedGrant = "refused_grant"

// NewLogin is a login found in an account's home that is not a rotation of
// the one the account holds: somebody ran /login inside that home. It is
// what AdoptFS returns instead of taking it, because nothing in the home can
// be trusted to say whose login it is — the configuration file there may be
// the person's own, shared by every account — and taking another account's
// login into this one would bill the wrong account for good.
//
// The application asks the provider whose it is (Identify, one read of the
// profile with the login's own access token) and then calls Accept when it
// is this account's or Refuse when it is not. If it cannot ask right now it
// does neither: nothing has changed, the home goes on working on the login
// Claude Code keeps there, and the next adoption finds the same login again.
type NewLogin struct {
	// Access is the new login's access token, the only thing that can say
	// whose it is.
	Access string
	login  claudeLogin
}

func (n *NewLogin) Error() string {
	return "the account's home holds a new login, not yet known to be this account's"
}

// Unwrap makes a NewLogin an ErrNewLogin.
func (n *NewLogin) Unwrap() error { return ErrNewLogin }

// Identify asks the provider whose this login is.
func (n *NewLogin) Identify(ctx context.Context) (*Identity, error) {
	return claudeProvider{}.Identify(ctx, n.Access)
}

// Accept takes the login into the account — the application has found it
// is this account's. A dead account is alive again: someone signed it in.
func (n *NewLogin) Accept(a *Account) {
	n.login.adopt(a)
	delete(a.Extra, claudeRefusedGrant)
}

// Refuse records that the home holds somebody else's login. Nothing of it is
// taken and the account's tokens stay exactly as they were; the home is
// marked as holding nothing of this account's, so the account's own login
// goes back in the first time nothing runs there. Which grant was refused is
// remembered, by when it ends, so that later looks pass it over without asking
// again — rotated, or with its access token lapsed — until the account's own
// login is written in or the account signs in afresh. The error says so,
// naming both, and is an ErrForeignLogin for an application to show rather
// than fail on.
func (n *NewLogin) Refuse(a *Account, found *Identity) error {
	a.Staged = stagedNone
	if v := n.login.refreshUntil(); v != "" {
		a.setExtra(claudeRefusedGrant, v)
	}
	who := "another account"
	if found != nil {
		switch {
		case found.Email != "":
			who = found.Email
		case found.UUID != "":
			who = found.UUID
		}
	}
	return failf(ErrForeignLogin,
		"%s: Claude Code in this account's home is signed in as %s instead, so that login was not taken; "+
			"this account's own goes back in the first time nothing runs there", a, who)
}

// claudeLogin is the claudeAiOauth entry of Claude Code's credential store,
// as Claude Code writes it. Its numbers are read as floats because the file
// is Claude Code's, written by JavaScript, and a whole number written with a
// fraction must not make the entire login unreadable.
type claudeLogin struct {
	AccessToken           string   `json:"accessToken"`
	RefreshToken          string   `json:"refreshToken"`
	ExpiresAt             float64  `json:"expiresAt"`
	Scopes                []string `json:"scopes"`
	SubscriptionType      string   `json:"subscriptionType"`
	RateLimitTier         string   `json:"rateLimitTier"`
	RefreshTokenExpiresAt float64  `json:"refreshTokenExpiresAt"`
	ClientID              string   `json:"clientId"`
	// TokenAccount is whose login Claude Code says this is, when it says —
	// {"uuid": …, "emailAddress": …} — kept as it came, so that a shape this
	// package does not expect never makes the whole login unreadable.
	TokenAccount jsontext.Value `json:"tokenAccount"`
}

func (l *claudeLogin) usable() bool { return l.RefreshToken != "" && l.ExpiresAt != 0 }

// grantSlack is how far apart two refreshTokenExpiresAt values may be and
// still be one grant's, in milliseconds. On a refresh Claude Code keeps the
// earlier of the one it had and now plus the reply's refresh_token_expires_in,
// and a reply in whole remaining seconds lands up to a second short of the
// real end — never further, since each refresh starts from the real end
// again. A sign-in ends a fixed time after it was made, so two of them this
// close together were made within the same minute.
const grantSlack = 60_000

// sameGrant reports whether two refreshTokenExpiresAt values, as the account
// keeps them, say one grant: neither is known, or both are and lie within
// grantSlack of each other. One known and one not is two grants.
func sameGrant(a, b string) bool {
	if a == b {
		return true
	}
	x, errA := strconv.ParseInt(a, 10, 64)
	y, errB := strconv.ParseInt(b, 10, 64)
	if errA != nil || errB != nil {
		return false
	}
	return x-y <= grantSlack && y-x <= grantSlack
}

// ofGrant reports whether a login found in a home is of the account's own
// grant: it ends within a minute of the account's login (sameGrant), and
// Claude Code does not record it as another account's (otherAccount). A login
// that is not is never taken as a rotation, however close its end.
func (l *claudeLogin) ofGrant(a *Account) bool {
	return sameGrant(l.refreshUntil(), a.Extra[claudeRefreshUntil]) && !l.otherAccount(a)
}

// otherAccount reports whether Claude Code recorded the login as another
// account's (tokenAccount): its uuid and the account's are both known, and
// differ. Nothing recorded, nothing to compare it with, or a record of a shape
// this package does not read says nothing, and costs no network call.
func (l *claudeLogin) otherAccount(a *Account) bool {
	if len(l.TokenAccount) == 0 || a.UUID == "" {
		return false
	}
	var who struct {
		UUID string `json:"uuid"`
	}
	if decodeLenient(l.TokenAccount, &who) != nil || who.UUID == "" {
		return false
	}
	return !strings.EqualFold(who.UUID, a.UUID)
}

// endsAfter reports whether the grant ending at a, as the account keeps such
// a value, ends more than grantSlack after the one ending at b: that it was
// signed in after it. Either end unknown says nothing.
func endsAfter(a, b string) bool {
	x, errA := strconv.ParseInt(a, 10, 64)
	y, errB := strconv.ParseInt(b, 10, 64)
	return errA == nil && errB == nil && x-y > grantSlack
}

// refreshUntil is the login's refreshTokenExpiresAt as the account keeps it:
// decimal milliseconds, or "" when the login carries none.
func (l *claudeLogin) refreshUntil() string {
	if l.RefreshTokenExpiresAt <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(l.RefreshTokenExpiresAt), 10)
}

// keep copies into the account what the login carries beyond its tokens, so
// a login written back later loses none of it. The plan is only ever added
// to — a login that omits it has not changed plans — but when the login
// itself ends is always the store's word, absent included, because it is
// what tells this login from the next one.
func (l *claudeLogin) keep(a *Account) {
	for k, v := range map[string]string{
		claudeSubscription: l.SubscriptionType, claudeRateLimitTier: l.RateLimitTier, claudeClient: l.ClientID,
	} {
		if v != "" {
			a.setExtra(k, v)
		}
	}
	if v := l.refreshUntil(); v != "" {
		a.setExtra(claudeRefreshUntil, v)
	} else {
		delete(a.Extra, claudeRefreshUntil)
	}
}

// readClaudeLogin reads the login out of a home's credential store. ok is
// false when there is no store, no login in it, or nothing readable.
func readClaudeLogin(fsys fs.FS) (claudeLogin, bool) {
	l, ok, err := loadClaudeLogin(fsys)
	return l, ok && err == nil
}

// loadClaudeLogin tells the three answers apart: a login (ok), no login —
// no store, or a store without one — and a store that is there and is not
// one whole JSON object, which is err.
func loadClaudeLogin(fsys fs.FS) (claudeLogin, bool, error) {
	raw, err := fs.ReadFile(fsys, claudeCredentials)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return claudeLogin{}, false, nil
	case err != nil:
		return claudeLogin{}, false, err
	}
	var doc struct {
		Login *claudeLogin `json:"claudeAiOauth"`
	}
	if err := decodeLenient(raw, &doc); err != nil {
		return claudeLogin{}, false, err
	}
	if doc.Login == nil {
		return claudeLogin{}, false, nil
	}
	return *doc.Login, true, nil
}

// claudeCredentialDoc is the credential store holding the account's login
// and nothing else, in Claude Code's own shape. What the account does not
// know is left out rather than written empty: an empty refresh token is how
// Claude Code marks a dead login, and an empty plan is one it would believe.
func claudeCredentialDoc(a *Account) ([]byte, error) {
	if a.Token.Access == "" || a.Token.Refresh == "" || a.Token.ExpiresAt == 0 {
		// Claude Code would read a missing refresh token or expiry as a token
		// good for inference only and never refresh it, and a missing access
		// token is not something to write at all. claudeStored keeps such an
		// account off this route, so reaching here is a caller's mistake.
		return nil, failf(ErrInvalidRequest, "%s: a stored login needs an access token, a refresh token and an expiry", a)
	}
	scopes := a.Token.Scopes
	if len(scopes) == 0 {
		// What rota's login asked for, and so what the token was issued as.
		scopes = claudeScopes
	}
	type login struct {
		AccessToken           string   `json:"accessToken"`
		RefreshToken          string   `json:"refreshToken"`
		ExpiresAt             int64    `json:"expiresAt"`
		Scopes                []string `json:"scopes"`
		SubscriptionType      string   `json:"subscriptionType,omitempty"`
		RateLimitTier         string   `json:"rateLimitTier,omitempty"`
		RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt,omitzero"`
		ClientID              string   `json:"clientId,omitempty"`
	}
	l := login{
		AccessToken: a.Token.Access, RefreshToken: a.Token.Refresh, ExpiresAt: a.Token.ExpiresAt,
		Scopes: scopes, SubscriptionType: a.Extra[claudeSubscription], RateLimitTier: a.Extra[claudeRateLimitTier],
		ClientID: a.Extra[claudeClient],
	}
	if l.ClientID == "" {
		// The client rota's own login used, which is the one this lineage
		// belongs to.
		l.ClientID = claudeClientID
	}
	if v, err := strconv.ParseInt(a.Extra[claudeRefreshUntil], 10, 64); err == nil {
		l.RefreshTokenExpiresAt = v
	}
	return Encode(map[string]any{claudeOAuthKey: l})
}

// MergeClaudeCredentials writes a planned login into an existing credential
// store: the claudeAiOauth entry of staged replaces the one in existing, and
// every other entry of existing is kept as it was, in its place.
//
// It exists because the store is not only the login. Claude Code keeps the
// OAuth tokens of the MCP servers a person has signed in to there, and other
// secrets of its own, and a write that replaced the file wholesale would
// sign every one of them out. An existing store that is empty, missing or
// unreadable is taken as holding nothing — which is how Claude Code reads it
// too.
func MergeClaudeCredentials(existing, staged []byte) ([]byte, error) {
	var login jsontext.Value
	for _, m := range objectMembers(staged) {
		if m.name == claudeOAuthKey {
			login = m.value
		}
	}
	if login == nil {
		return nil, failf(ErrInvalidRequest, "the planned credential store carries no %s entry", claudeOAuthKey)
	}
	// Assembled by hand rather than through an encoder, which reformats every
	// value it is given: what is not the login goes back byte for byte.
	out := []byte{'{'}
	write := func(name string, v jsontext.Value) error {
		if len(out) > 1 {
			out = append(out, ',')
		}
		var err error
		if out, err = jsontext.AppendQuote(out, name); err != nil {
			return err
		}
		out = append(append(out, ':'), v...)
		return nil
	}
	placed := false
	for _, m := range objectMembers(existing) {
		if m.name == claudeOAuthKey {
			if placed {
				continue // a repeated login is one login, the new one
			}
			m.value, placed = login, true
		}
		if err := write(m.name, m.value); err != nil {
			return nil, err
		}
	}
	if !placed {
		if err := write(claudeOAuthKey, login); err != nil {
			return nil, err
		}
	}
	return append(out, '}'), nil
}

type member struct {
	name  string
	value jsontext.Value
}

// objectMembers lists a JSON object's members in order, each value exactly
// as it was written. Anything that is not one whole object reads as empty.
func objectMembers(raw []byte) []member {
	dec := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil
	}
	var out []member
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil || tok.Kind() != '"' {
			return nil
		}
		// A token is only good until the next read, so its text is taken now.
		name := tok.String()
		v, err := dec.ReadValue()
		if err != nil {
			return nil
		}
		out = append(out, member{name: name, value: v.Clone()})
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		return nil // something after the object: not a store Claude Code wrote
	}
	return out
}

// stageMerged writes a planned credential store into a home on this disk,
// keeping what the store there already holds, and records which login it
// carries. The write is a private temporary file renamed over the store, so
// a process reading it sees the old store or the new one and never half of
// either.
func stageMerged(a *Account, home string, f StagedFile) error {
	path := filepath.Join(home, f.Path)
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	merged, err := MergeClaudeCredentials(existing, f.Content)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, merged, f.Mode); err != nil {
		return err
	}
	a.Staged = fingerprint(a.Token.Refresh)
	delete(a.Extra, claudeRefusedGrant) // the account's own login is in, over whatever was refused
	return nil
}

// writeFileAtomic lands data at path through a temporary file in the same
// directory and a rename.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cherr := f.Chmod(mode)
	if runtime.GOOS == "windows" {
		cherr = nil // permission bits are not Windows' vocabulary
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(cherr, werr, serr, cerr); err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
