package rota

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// claudeInheritedNever is every variable that would sign Claude Code in some
// other way, send its requests or its credential somewhere else, change
// whose login or which configuration it uses, or tell it that it runs inside
// another Claude Code. They are written out here rather than
// read from the list the code keeps, so that a name dropped from that list
// fails this test. Measured against Claude Code 2.1.319: its documented
// variables, and the lists it keeps of its own credentials, endpoints and
// provider switches.
var claudeInheritedNever = []string{
	// A credential of another kind.
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_AWS_API_KEY",
	"ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_FOUNDRY_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	"CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR",
	"CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_OAUTH_SCOPES",
	"CLAUDE_CODE_SUBSCRIPTION_TYPE", "CLAUDE_CODE_RATE_LIMIT_TIER",
	"CLAUDE_CODE_SESSIONS_OAUTH_TOKEN", "CLAUDE_CODE_SESSION_ACCESS_TOKEN", "CLAUDE_SESSION_INGRESS_TOKEN_FILE",
	"CLAUDE_BRIDGE_OAUTH_TOKEN", "CLAUDE_TRUSTED_DEVICE_TOKEN",
	// What selects an Anthropic profile or workload identity federation, and
	// its identity token.
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
	"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", "ANTHROPIC_UNIX_SOCKET", "ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CODE_API_BASE_URL",
	"CLAUDE_BRIDGE_BASE_URL", "CLAUDE_BRIDGE_SESSION_INGRESS_URL", "CLAUDE_REMOTE_TOOLS_BRIDGE_URL",
	"CLAUDE_CODE_ARTIFACTS_API_BASE_URL", "CLAUDE_CODE_ARTIFACTS_API_TOKEN", "CLAUDE_CODE_ARTIFACT_ASSET_BASE_URL",
	"CLAUDE_CODE_ARTIFACT_LIVE_BASE_URL", "CLAUDE_CODE_ARTIFACT_SYNC_BASE_URL", "CLAUDE_CODE_ARTIFACT_VIEWER_BASE_URL",
	"CLAUDE_CODE_MEMORY_API_BASE_URL", "CLAUDE_CODE_MEMORY_API_TOKEN", "CLAUDE_CODE_GB_BASE_URL", "VOICE_STREAM_BASE_URL",
	// The marks of a process another Claude Code started.
	"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SPAWNED_BY_SERVER", "CLAUDE_CODE_REMOTE", "CLAUDE_CODE_REMOTE_SESSION_ID",
	"CLAUDE_CODE_REMOTE_ENVIRONMENT_TYPE", "CLAUDE_CODE_ENVIRONMENT_KIND", "CLAUDE_CODE_CONTAINER_ID",
	// Interception, for every provider.
	"HTTPS_PROXY", "NODE_EXTRA_CA_CERTS",
}

// codexInheritedNever is the same for codex, measured against its source
// (github.com/openai/codex, 2026-11-05).
var codexInheritedNever = []string{
	"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_BASE_URL",
	"OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE", "OPENAI_WORKLOAD_IDENTITY_CONTEXT",
	"CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_REVOKE_TOKEN_URL_OVERRIDE",
	"CODEX_APP_SERVER_LOGIN_CLIENT_ID", "CODEX_APP_SERVER_LOGIN_ISSUER",
	"CODEX_AUTHAPI_BASE_URL", "CODEX_AGENT_IDENTITY_AUTHAPI_BASE_URL", "CODEX_AGENT_IDENTITY_JWKS_BASE_URL",
	"CODEX_CONNECTORS_TOKEN", "CODEX_CA_CERTIFICATE",
	"CODEX_INTERNAL_ORIGINATOR_OVERRIDE",
	"HTTPS_PROXY", "NODE_EXTRA_CA_CERTS",
}

// Close to the dropped names and not dropped, because none changes how the
// CLI itself signs in or where it sends its requests: a variable that shares
// a prefix with one; a provider's configuration, which Claude Code reads only
// behind a dropped CLAUDE_CODE_USE_* switch and Anthropic's SDKs and the
// clouds' own tools read too; workload identity federation's configuration,
// which Claude Code reads only behind the dropped names that select it;
// Claude Code's proxies for the package managers a cloud session starts; a
// telemetry label; codex's request headers and its sandbox's own fact. Each
// stays for everything the run starts.
var inheritedKept = []string{
	"CLAUDE_CODE_USE_POWERSHELL_TOOL", "ANTHROPIC_MODEL", "CLAUDE_CODE_MAX_OUTPUT_TOKENS",
	"AWS_PROFILE", "AWS_BEARER_TOKEN_BEDROCK", "GOOGLE_CLOUD_PROJECT", "CLOUD_ML_REGION",
	"ANTHROPIC_VERTEX_PROJECT_ID", "ANTHROPIC_FOUNDRY_RESOURCE", "ANTHROPIC_AWS_WORKSPACE_ID",
	"ANTHROPIC_GOOGLE_CLOUD_PROJECT", "ANTHROPIC_GOOGLE_CLOUD_LOCATION", "ANTHROPIC_GOOGLE_CLOUD_WORKSPACE_ID",
	"ANTHROPIC_WORKSPACE_ID", "ANTHROPIC_SERVICE_ACCOUNT_ID", "ANTHROPIC_SCOPE", "CLAUDE_CODE_FEDERATION_CACHE_DIR",
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH", "CLAUDE_CODE_SKIP_VERTEX_AUTH", "CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
	"CLAUDE_CODE_SKIP_MANTLE_AUTH", "CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH", "CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH",
	"CLAUDE_CODE_HTTP_PROXY", "CLAUDE_CODE_HTTPS_PROXY", "CLAUDE_CODE_ACCOUNT_TAGGED_ID",
	"OPENAI_ORGANIZATION", "OPENAI_PROJECT", "CODEX_SANDBOX",
	"CODEX_SQLITE_HOME", "CODEX_THREAD_ID",
}

// whatTheChildSees runs a fake CLI on cmd, with base as everything inherited,
// and returns what it saw of each name: the value, or "-" when unset.
func whatTheChildSees(t *testing.T, a *Account, cmd *Command, base []string, spec Spec, names []string) map[string]string {
	t.Helper()
	var line []string
	for _, n := range names {
		line = append(line, n+"={{env:"+n+"|-}}")
	}
	bin := fakecli.Install(t, t.TempDir(), "envcli", fakecli.Spec{KeepStdin: true, Stdout: []string{
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"` +
			strings.Join(line, `\u0001`) + `","num_turns":1}`,
	}})
	given := *cmd
	given.Bin, given.BaseEnv = bin, base
	spec.Prompt, spec.flavorOverride = "p", "claude"
	var out bytes.Buffer
	res, err := Run(context.Background(), a, "", &given, spec, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, kv := range strings.Split(res.Result, "\x01") {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = v
	}
	return seen
}

// inherited is a base environment holding every name with a value that says
// where it came from.
func inherited(names ...[]string) []string {
	base := []string{"PATH=/usr/bin:/bin"}
	for _, list := range names {
		for _, n := range list {
			base = append(base, n+"=inherited-"+n)
		}
	}
	return base
}

// Nothing a shell, a host or an outer Claude Code exported reaches the CLI of
// a claude account, on any route: the stored login, a token in the
// environment, an API key, a hermetic run on either. What rota itself sets
// does reach it — the home, the token or the key, the account's own
// directory, and the account's name — and so does everything else inherited.
func TestNoInheritedVariableSignsInAClaudeAccountsCLI(t *testing.T) {
	key := &Account{ID: 7, Provider: "claude", Credential: credentialAPIKey, Token: Token{Access: "sk-ant-api03-KEY"}}
	own := livingClaude()
	own.ConfigDir = "/accounts/five"
	home := t.TempDir()
	stored := identify(livingClaude(), claudeStoredCommand(home))
	scratch := t.TempDir()
	if r, err := filepath.EvalSymlinks(scratch); err == nil {
		scratch = r
	}
	base := inherited(claudeInheritedNever, inheritedKept, []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"})
	all := append(append(slices.Clone(claudeInheritedNever), inheritedKept...),
		"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "ROTA_PROVIDER", "ROTA_ACCOUNT_ID", "ROTA_ACCOUNT")

	for _, c := range []struct {
		what string
		a    *Account
		cmd  *Command
		spec Spec
		want map[string]string
	}{
		{"the stored login", livingClaude(), stored, Spec{}, map[string]string{
			"CLAUDE_CONFIG_DIR": home, "CLAUDE_CODE_OAUTH_TOKEN": "-", "ROTA_ACCOUNT": "five@x"}},
		{"a token in the environment", livingClaude(), identify(livingClaude(), claudeEnvCommand(livingClaude())), Spec{},
			map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "A1", "CLAUDE_CONFIG_DIR": "inherited-CLAUDE_CONFIG_DIR"}},
		{"the account's own directory", own, identify(own, claudeEnvCommand(own)), Spec{},
			map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "A1", "CLAUDE_CONFIG_DIR": "/accounts/five"}},
		{"an API key", key, identify(key, claudeEnvCommand(key)), Spec{},
			map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-KEY", "CLAUDE_CODE_OAUTH_TOKEN": "-"}},
		{"a hermetic run on the stored login", livingClaude(), stored, Spec{Hermetic: true, ScratchDir: scratch},
			map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "A1"}},
		{"a hermetic run on a key", key, identify(key, claudeEnvCommand(key)), Spec{Hermetic: true, ScratchDir: scratch},
			map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-KEY", "CLAUDE_CODE_OAUTH_TOKEN": "-"}},
	} {
		seen := whatTheChildSees(t, c.a, c.cmd, base, c.spec, all)
		for _, n := range claudeInheritedNever {
			if _, ok := c.want[n]; !ok && seen[n] != "-" {
				t.Errorf("%s: %s reached the CLI as %q", c.what, n, seen[n])
			}
		}
		for _, n := range inheritedKept {
			if seen[n] != "inherited-"+n {
				t.Errorf("%s: %s must pass through, got %q", c.what, n, seen[n])
			}
		}
		for n, v := range c.want {
			if seen[n] != v {
				t.Errorf("%s: %s must be %q, got %q", c.what, n, v, seen[n])
			}
		}
		if seen["ROTA_PROVIDER"] != "claude" || seen["ROTA_ACCOUNT_ID"] == "-" {
			t.Errorf("%s: the account's name reaches the CLI: %v %v", c.what, seen["ROTA_PROVIDER"], seen["ROTA_ACCOUNT_ID"])
		}
		if c.spec.Hermetic && !strings.HasPrefix(seen["CLAUDE_CONFIG_DIR"], scratch+string(filepath.Separator)) {
			t.Errorf("%s: the throwaway directory, not %q", c.what, seen["CLAUDE_CONFIG_DIR"])
		}
	}

	// A caller that starts Claude Code for something other than a run is
	// handed the same list, the token variable with it.
	env := Environ(base, &Command{Drop: ClaudeCredentialVars()})
	for _, n := range append(slices.Clone(claudeInheritedNever), "CLAUDE_CODE_OAUTH_TOKEN") {
		if slices.Contains(envNames(env), n) {
			t.Errorf("ClaudeCredentialVars keeps %s", n)
		}
	}
}

// The same for a codex account: its CLI reads its login from the auth.json in
// the private CODEX_HOME rota gives it, and nothing inherited takes it
// anywhere else.
func TestNoInheritedVariableSignsInACodexAccountsCLI(t *testing.T) {
	a := &Account{ID: 3, Provider: "codex", Email: "c@x", Token: Token{Access: "A", Refresh: "R"},
		Extra: map[string]string{"id_token": "I", "account_id": "acct"}}
	home := t.TempDir()
	p, _ := Lookup("codex")
	cmd, _, err := p.(codexProvider).Plan(context.Background(), a, home)
	if err != nil {
		t.Fatal(err)
	}
	base := inherited(codexInheritedNever, inheritedKept, []string{"CODEX_HOME"})
	all := append(append(slices.Clone(codexInheritedNever), inheritedKept...), "CODEX_HOME", "ROTA_PROVIDER")
	seen := whatTheChildSees(t, a, identify(a, cmd), base, Spec{}, all)
	for _, n := range codexInheritedNever {
		if seen[n] != "-" {
			t.Errorf("%s reached the CLI as %q", n, seen[n])
		}
	}
	for _, n := range inheritedKept {
		if seen[n] != "inherited-"+n {
			t.Errorf("%s must pass through, got %q", n, seen[n])
		}
	}
	if seen["CODEX_HOME"] != home || seen["ROTA_PROVIDER"] != "codex" {
		t.Errorf("rota's own values reach the CLI: home %q, provider %q", seen["CODEX_HOME"], seen["ROTA_PROVIDER"])
	}
}
