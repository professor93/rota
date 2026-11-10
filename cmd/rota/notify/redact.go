package notify

import (
	"cmp"
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
// inside another is not left half there. It also takes out each secret as
// a URL's path or query would escape it, and with its slashes escaped as
// JSON may write them (\/). A secret shorter than minSecret is not looked
// for, in any form.
func redact(s string, secrets []string) string {
	var all []string
	for _, x := range secrets {
		if len(x) < minSecret {
			continue
		}
		for _, v := range []string{x, url.PathEscape(x), url.QueryEscape(x), strings.ReplaceAll(x, "/", `\/`)} {
			if !strings.Contains(v, "\x00") && !slices.Contains(all, v) {
				all = append(all, v)
			}
		}
	}
	slices.SortStableFunc(all, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	// A NUL stands for each secret until the end, so that no secret is
	// found inside the words that replace another.
	s = strings.ReplaceAll(s, "\x00", " ")
	for _, v := range all {
		s = strings.ReplaceAll(s, v, "\x00")
	}
	return strings.ReplaceAll(s, "\x00", "[redacted]")
}

// cleanErr is err as a sink's failure is said, in the log and in
// sinks.json: each net/http *url.Error in it, however deeply wrapped or
// joined, said by the error inside it rather than with the whole URL it
// repeats; every secret taken out; and the rest one line of at most 300
// bytes with no control bytes.
func cleanErr(err error, secrets []string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, ue := range urlErrors(err) {
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

// urlErrors is every *url.Error in err's tree that has an error inside it,
// the outermost first, so that one inside another is said by what is
// inside them both. The walk is bounded: an error is not a long chain.
func urlErrors(err error) []*url.Error {
	var out []*url.Error
	seen := 0
	var walk func(error)
	walk = func(e error) {
		if e == nil || seen > 256 {
			return
		}
		seen++
		if ue, ok := e.(*url.Error); ok && ue.Err != nil {
			out = append(out, ue)
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, x := range u.Unwrap() {
				walk(x)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

// configSecrets is what of a sink's own keys is secret, for an error said
// before the sink exists to say its own, and beside every secret it says
// itself: a webhook's URL, whole, by its path and query, and by each path
// segment and each query value alone, any of which may be its key.
func configSecrets(s api.EventSink) []string {
	if s.URL == "" {
		return nil
	}
	out := []string{s.URL}
	if u, err := parseURL(s.URL); err == nil {
		out = append(out, u.String(), u.Path, u.RawPath, u.EscapedPath(), u.RawQuery, u.Fragment)
		out = append(out, strings.Split(u.Path, "/")...)
		q, _ := url.ParseQuery(u.RawQuery) // what it could read, even of a bad query
		for _, vs := range q {
			out = append(out, vs...)
		}
	}
	return out
}

// parseURL is url.Parse, whose error is never said: it quotes the address.
func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }
