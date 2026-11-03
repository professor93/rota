package rota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// A claude account can also run on a credential somebody pastes, in two
// kinds, and this file is both.
//
// An API key from the Claude Console makes an account of its own. Claude Code
// takes it in ANTHROPIC_API_KEY and sends it as X-Api-Key; the work is billed
// to the Console's API credit, per token, with no plan behind it — so there is
// no usage endpoint to read, nothing to refresh and nothing that expires.
// Claude Code asks once, in its first interactive session, whether to use the
// key; a headless run uses it without asking. Remote Control refuses it.
//
// A token from `claude setup-token` is a year-long, inference-only token for a
// claude.ai login — the very credential BeginLong obtains. It cannot say whose
// it is (the profile endpoint refuses it), so it never makes an account: the
// caller puts it on one already logged in, as that account's long-lived token.
//
// The two are told apart by their prefix, which is how Claude Code itself
// tells them apart: it reads the three characters after "sk-ant-" as "api"
// for a key and "oat" for an OAuth token. The prefix is only ever used to
// refuse the wrong one with a sentence; the provider is what says a key is
// good.
const (
	claudeKeyPrefix   = "sk-ant-api"
	claudeSetupPrefix = "sk-ant-oat"
	// claudeAPIVersion is the API version a key is checked with.
	claudeAPIVersion = "2023-06-01"
	// claudeKeyCheckTime bounds the one request that checks a pasted key. A
	// caller wanting less passes a context with an earlier deadline; the
	// check fails open, so a short one costs only the check.
	claudeKeyCheckTime = 5 * time.Second
)

// TakeKey takes a Console API key: its shape first, then one question to the
// API, which only a 401 or a 403 answers with a refusal.
func (claudeProvider) TakeKey(ctx context.Context, key string) (*Token, error, error) {
	if err := claudeShape(key, true); err != nil {
		return nil, nil, err
	}
	refused, unverified := checkClaudeKey(ctx, key)
	if refused != nil {
		return nil, nil, refused
	}
	return &Token{
		Access:     key,
		Credential: CredentialAPIKey,
		Identity:   &Identity{UUID: "apikey-" + fingerprint(key)},
	}, unverified, nil
}

// TakeLong takes a token `claude setup-token` printed. It is good for a year
// from when it was made, which is about now: the person ran it to paste it.
func (claudeProvider) TakeLong(token string) (*Token, error) {
	if err := claudeShape(token, false); err != nil {
		return nil, err
	}
	return &Token{Access: token, ExpiresAt: nowMS() + claudeLongSeconds*1000}, nil
}

// claudeShape refuses what cannot be the credential asked for — key says
// which — in words that never repeat what was pasted.
func claudeShape(s string, key bool) error {
	what, prefix := "a token from `claude setup-token`", claudeSetupPrefix
	if key {
		what, prefix = "a Claude Console API key", claudeKeyPrefix
	}
	switch {
	case s == "":
		return failf(ErrInvalidRequest, "nothing was pasted; %s starts with %s", what, prefix)
	case strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "":
		return failf(ErrInvalidRequest, "%s is letters, digits, - and _ only, and what was pasted has something else in it", what)
	case key && strings.HasPrefix(s, claudeSetupPrefix):
		return failf(ErrInvalidRequest, "that is a token from `claude setup-token` (%s...), not a Console API key (%s...): "+
			"it is a claude.ai login's long-lived token, and goes on an account already logged in", claudeSetupPrefix, claudeKeyPrefix)
	case !key && strings.HasPrefix(s, claudeKeyPrefix):
		return failf(ErrInvalidRequest, "that is a Console API key (%s...), not a token from `claude setup-token` (%s...): "+
			"a key is an account of its own", claudeKeyPrefix, claudeSetupPrefix)
	case !strings.HasPrefix(s, prefix):
		return failf(ErrInvalidRequest, "%s starts with %s, and what was pasted does not", what, prefix)
	}
	return nil
}

// checkClaudeKey asks the API's model list whether a key is good. refused is
// the API's own refusal; unverified is any other reason the question got no
// answer. Neither carries the key, nor anything the server said: a body may
// quote what it was sent.
//
// The request follows no redirect. The key travels in a header of its own,
// which Go's client would carry along to wherever a redirect pointed.
func checkClaudeKey(ctx context.Context, key string) (refused, unverified error) {
	ctx, cancel := context.WithTimeout(ctx, claudeKeyCheckTime)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ClaudeModelsEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("the key was not checked: %v", scrub(err, key))
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", claudeAPIVersion)
	req.Header.Set("Accept", "application/json")
	client := *HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, err = doWith(&client, req)
	var he *HTTPError
	switch {
	case err == nil:
		return nil, nil
	case errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden):
		return failf(ErrInvalidRequest, "the Claude API refused this key (%d %s): it is wrong, revoked or disabled, "+
			"or its organisation is", he.Status, http.StatusText(he.Status)), nil
	case errors.As(err, &he):
		return nil, fmt.Errorf("the Claude API answered %d %s when asked about the key, so it was not checked",
			he.Status, http.StatusText(he.Status))
	}
	return nil, fmt.Errorf("the Claude API could not be asked about the key, so it was not checked: %v", scrub(err, key))
}

// scrub is an error's text with a secret taken out, for an error whose words
// this package did not choose.
func scrub(err error, secret string) string {
	if secret == "" {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), secret, "[key]")
}
