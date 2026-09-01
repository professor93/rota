package rota

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"io"
)

// rota reads and writes JSON with encoding/json/v2, and this file is the one
// place that says how.
//
// v2 is stricter than the package it replaces, in three ways that all matter
// here and all fail the same quiet way — a field silently left at its zero
// value, or a whole reply refused:
//
//   - object names are matched case-sensitively;
//   - a duplicate name is an error rather than "the last one wins";
//   - invalid UTF-8 is an error rather than a replacement character.
//
// For the JSON rota writes itself — its account store, its own replies —
// strict is right, and every one of those rules catches a real mistake.
// For JSON that arrives from somewhere else it is not: a provider's token
// endpoint, a vendor CLI's event stream and an API client's request body have
// all been parsed leniently for as long as rota has existed, and tightening
// that in a patch release would break them at the least convenient moment.
// So foreign JSON keeps exactly the old rules, named here rather than
// scattered across nine files.

// lenient restores what encoding/json always did, for input rota did not
// produce.
var lenient = jsonv2.JoinOptions(
	jsonv2.MatchCaseInsensitiveNames(true),
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)

// asBefore keeps what rota writes byte-for-byte what it was. v2 encodes a
// nil slice as [] and a nil map as {}, where encoding/json wrote null, and
// that is not a free change: the account store on disk, the HTTP replies and
// a consumer of this package are all read by something else, and one — the check
// that works out which Spec fields a caller actually set — reads the
// difference between null and [] as the answer to that question.
var asBefore = jsonv2.JoinOptions(
	jsonv2.FormatNilSliceAsNull(true),
	jsonv2.FormatNilMapAsNull(true),
)

// Encode marshals a value the way rota's own files and replies are written.
func Encode(v any) ([]byte, error) { return jsonv2.Marshal(v, asBefore) }

// EncodeIndent is Encode, indented two spaces, for the documents people open
// in an editor.
func EncodeIndent(v any) ([]byte, error) {
	return jsonv2.Marshal(v, asBefore, jsontext.WithIndent("  "))
}

// EncodeTo writes Encode's output to w followed by a newline, which is what
// an encoding/json Encoder did and what every reader of these streams —
// newline-delimited JSON, a terminal — expects.
func EncodeTo(w io.Writer, v any) error {
	raw, err := Encode(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(raw, '\n'))
	return err
}

// UnmarshalLenient reads JSON that came from somewhere else — a provider, a
// vendor CLI, an API client — under the rules encoding/json used: names
// matched without regard to case, a repeated name taking its last value, and
// invalid UTF-8 replaced rather than refused.
//
// It is exported so a transport built on this library reads request bodies
// the same way rota reads everything else, instead of inventing a second
// answer to the same question.
func UnmarshalLenient(data []byte, v any) error { return jsonv2.Unmarshal(data, v, lenient) }

// DecodeLenient is UnmarshalLenient reading from r.
func DecodeLenient(r io.Reader, v any) error { return jsonv2.UnmarshalRead(r, v, lenient) }

// LenientOptions is the rule set the two functions above apply, for a
// transport that needs to add one of its own — refusing unknown members,
// say — without restating what leniency means here.
func LenientOptions() jsonv2.Options { return lenient }

// decodeLenient is the unexported spelling used inside the package.
func decodeLenient(data []byte, v any) error { return UnmarshalLenient(data, v) }
