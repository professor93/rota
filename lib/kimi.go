package rota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Kimi (Moonshot) — the Kimi Code CLI, which holds its own credentials.
//
// `kimi login` runs the device-code flow and keeps what it gets inside
// KIMI_CODE_HOME, and there is no supported way to hand it a token from
// outside: the API-key variable it reads is not enough on its own (a run
// with one still reports "No model configured"), and KIMI_SHARE_DIR, which
// rota used to stage a credential file into, does not appear in this
// binary at all. rota tried that route against the published kimi-cli
// wheel; Kimi Code is a different program.
//
// So rota does here what it does for grok: it reserves a private directory,
// runs the CLI's own login inside it, and holds no token. That is what keeps
// two Kimi accounts from treading on each other, which is the part rota is
// actually for.
const (
	kimiHomeVar = "KIMI_CODE_HOME"
	// kimiCredential is what a finished login leaves behind. The login also
	// writes model entries into the home's config.toml, and a login that
	// stored the token but failed before that leaves the CLI able to
	// authenticate and unable to choose a model — worth telling apart.
	kimiCredential = "credentials/kimi-code.json"
	kimiConfig     = "config.toml"
)

type kimiProvider struct{}

func init() { Register(kimiProvider{}) }

func (kimiProvider) Name() string { return "kimi" }

// Begin has no authorization round trip of rota's own: the CLI runs the
// device flow itself, inside the home rota gives it.
func (kimiProvider) Begin(_ context.Context) (string, map[string]string, error) {
	return "https://moonshotai.github.io/kimi-code/", map[string]string{"kind": "delegated"}, nil
}

// Complete registers the account. Nothing is pasted: the credential belongs
// to the CLI, obtained by running the delegated login in the account's home.
func (kimiProvider) Complete(_ context.Context, code string, _ map[string]string) (*Token, error) {
	if strings.TrimSpace(code) != "" {
		return nil, failf(ErrInvalidRequest,
			"kimi takes nothing pasted: the CLI signs itself in; finish the login with no code")
	}
	return &Token{Delegated: true, Identity: &Identity{UUID: "kimi-" + randID()}}, nil
}

// LoginPlan signs the account in, in its own directory.
func (kimiProvider) LoginPlan(_ *Account, home string) LoginPlan {
	return LoginPlan{
		Bin:  "kimi",
		Args: []string{"login"},
		Env:  []string{kimiHomeVar + "=" + home},
		Drop: dropList("KIMI_API_KEY", "KIMI_BASE_URL", "KIMI_CODE_BASE_URL", "KIMI_CODE_OAUTH_HOST", "OPENAI_API_KEY", "ANTHROPIC_BASE_URL"),
	}
}

// SignedIn reports whether this account can authenticate yet.
func (kimiProvider) SignedIn(a *Account, home string) error {
	if !a.Delegated {
		return nil
	}
	if _, err := os.Stat(filepath.Join(home, kimiCredential)); err != nil {
		return failf(ErrReauth,
			"%s has not been signed in yet: its delegated login has not run in its private home", a)
	}
	// A login writes the token first and the model entries second. If the
	// second half did not happen the CLI authenticates fine and then
	// refuses to run, saying only "No model configured" — which does not
	// hint that the login is the thing to repeat.
	if _, err := os.Stat(filepath.Join(home, kimiConfig)); err != nil {
		return failf(ErrReauth,
			"%s signed in but its delegated login did not finish: the token is stored, the models it may use "+
				"are not, and only running that login again completes it", a)
	}
	return nil
}

// Launch gives the CLI a directory of its own and nothing else.
//
// Everything that could point it at another account's credentials, another
// endpoint or another configuration is dropped, so a shell profile cannot
// quietly bill someone else.
func (kimiProvider) Launch(a *Account, home string) (*Command, error) {
	if home == "" {
		return nil, errors.New("kimi keeps its credentials in a directory of its own and needs a private home")
	}
	if !a.Delegated {
		// An account registered by rota's former device flow holds a token
		// this CLI has no way to accept. Say so, rather than letting it
		// fail as "No model configured", which explains nothing.
		return nil, failf(ErrReauth,
			"%s predates delegated Kimi Code support and holds a token its CLI cannot accept; "+
				"remove the account and register it again, delegated", a)
	}
	return &Command{
		Bin:  "kimi",
		Env:  []string{kimiHomeVar + "=" + home},
		Drop: dropList("KIMI_API_KEY", "KIMI_BASE_URL", "KIMI_CODE_BASE_URL", "KIMI_CODE_OAUTH_HOST", "OPENAI_API_KEY", "ANTHROPIC_BASE_URL"),
	}, nil
}

// Kimi Code publishes no model catalogue rota can check against: -m takes an
// alias defined in the account's own config.toml, so the list belongs to
// whoever wrote that file. Refusing a name rota has not heard of would be
// rota inventing a rule, so it implements no Catalog at all and passes the
// model through.
