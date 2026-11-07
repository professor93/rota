package store

import (
	"context"
	"slices"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// What rota itself inherited is what an account's CLI would inherit, and a
// shell, a host or an outer session puts there what would sign the CLI in
// some other way, send it elsewhere, or say it is nested. None of it reaches
// the child on any route the store takes; what rota sets on purpose does, and
// so does everything else.
func TestNothingRotaInheritedSignsInAnAccountsCLI(t *testing.T) {
	claudeWorld(t)
	never := map[string][]string{
		"claude": {"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID",
			"CLAUDE_CODE_OAUTH_CLIENT_ID", "ANTHROPIC_CONFIG_DIR", "CLAUDE_CODE_USE_GATEWAY", "ANTHROPIC_UNIX_SOCKET",
			"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "ANTHROPIC_FOUNDRY_AUTH_TOKEN", "CLAUDE_CODE_USER_EMAIL"},
		"codex": {"CODEX_REFRESH_TOKEN_URL_OVERRIDE", "OPENAI_FEDERATION_RULE_ID", "CODEX_CONNECTORS_TOKEN",
			"CODEX_APP_SERVER_LOGIN_CLIENT_ID", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE"},
	}
	for _, names := range never {
		for _, n := range names {
			t.Setenv(n, "inherited")
		}
	}
	kept := []string{"AWS_PROFILE", "ANTHROPIC_VERTEX_PROJECT_ID", "CLAUDE_CODE_SKIP_VERTEX_AUTH", "CODEX_SANDBOX", "OPENAI_ORGANIZATION"}
	for _, n := range kept {
		t.Setenv(n, "inherited")
	}

	s := openTemp(t)
	claude := livingClaude(s, "u1")
	codex := s.add("codex")
	codex.Email = "c@x"
	codex.Token = rota.Token{Access: "A", Refresh: "R"}
	codex.Extra = map[string]string{"id_token": "I", "account_id": "acct"}

	env := func(a *rota.Account, home bool) []string {
		t.Helper()
		cmd, release, err := s.prepare(context.Background(), a, home)
		if err != nil {
			t.Fatal(err)
		}
		release()
		return rota.Environ(HostEnv(), cmd)
	}
	for what, c := range map[string]struct {
		env  []string
		kind string
		own  string
	}{
		"a claude run in its home": {env(claude, true), "claude", "ROTA_ACCOUNT=u1@x"},
		"a hermetic claude run":    {env(claude, false), "claude", "CLAUDE_CODE_OAUTH_TOKEN=A-u1"},
		"a codex run":              {env(codex, true), "codex", "CODEX_HOME=" + s.Home(codex)},
	} {
		for _, n := range never[c.kind] {
			if hasVar(c.env, n) {
				t.Errorf("%s: %s reached the CLI", what, n)
			}
		}
		if !slices.Contains(c.env, c.own) || !slices.Contains(c.env, "ROTA_PROVIDER="+c.kind) {
			t.Errorf("%s: rota's own values reach the CLI: %v", what, c.env)
		}
		for _, n := range kept {
			if !slices.Contains(c.env, n+"=inherited") {
				t.Errorf("%s: %s is inherited as it was", what, n)
			}
		}
	}
}
