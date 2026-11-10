package notify

import (
	"cmp"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/events"
)

// minSecret is the shortest secret redact takes out: a shorter one, such as
// a webhook's path "/", would take letters out of every sentence.
const minSecret = 4

// maxErr bounds an error as the log and sinks.json say it.
const maxErr = 300

// redact takes every secret out of s, the longest first so that a secret
// inside another is not left half there, and also as a URL's path or query
// would escape it. A secret shorter than minSecret is not looked for.
func redact(s string, secrets []string) string {
	var all []string
	for _, x := range secrets {
		for _, v := range []string{x, url.PathEscape(x), url.QueryEscape(x)} {
			if len(v) >= minSecret && !slices.Contains(all, v) {
				all = append(all, v)
			}
		}
	}
	slices.SortStableFunc(all, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, v := range all {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

// cleanErr is err as a sink's failure is said, in the log and in
// sinks.json: net/http's *url.Error, which repeats the whole URL, unwrapped
// to the error inside it; every secret taken out; and the rest one line of
// at most 300 bytes with no control bytes.
func cleanErr(err error, secrets []string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		msg = strings.ReplaceAll(msg, ue.Error(), ue.Err.Error())
	}
	msg = redact(msg, secrets)
	var lines []string
	for l := range strings.FieldsFuncSeq(msg, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return events.Clean(strings.Join(lines, "; "), maxErr)
}

// configSecrets is what of a sink's own keys is secret, for an error said
// before the sink exists to say its own: a webhook's URL, whole and by its
// path and query, which often hold its key.
func configSecrets(s api.EventSink) []string {
	if s.URL == "" {
		return nil
	}
	out := []string{s.URL}
	if u, err := parseURL(s.URL); err == nil {
		out = append(out, u.String(), u.Path, u.RawPath, u.EscapedPath(), u.RawQuery, u.Fragment)
	}
	return out
}

// parseURL is url.Parse, whose error is never said: it quotes the address.
func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }
