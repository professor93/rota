// Package claudeset reads Claude Code's settings files the way Claude Code
// does, for the parts of rota that look into one before a launch: the
// store's warnings about what a settings file brings back, and the check of
// a CLI's arguments for Remote Control (holdwire.AsksRemoteControl).
package claudeset

import (
	"bytes"
	"encoding/binary"
	"encoding/json/jsontext"
	"io"
	"os"
	"unicode/utf16"

	rota "github.com/professor93/rota/lib"
)

// MaxFile bounds what is read of one settings file: 2 MiB, the most Claude
// Code reads of one — it refuses a larger file whole — so what rota passes
// over is what Claude Code does not apply.
const MaxFile = 2 << 20

// ReadSmall reads a regular file of at most limit bytes, and nothing else:
// a FIFO would block the launch on its open, and a device or a huge file is
// not a settings file. The shape is looked at before the open, so that
// nothing else is opened at all, and checked again on what was opened
// (OpenSmall).
func ReadSmall(path string, limit int64) ([]byte, bool) {
	if fi, err := os.Stat(path); err != nil || !smallFile(fi, limit) {
		return nil, false
	}
	return OpenSmall(path, limit)
}

// OpenSmall is ReadSmall on whatever is at path by the time it is opened,
// which may no longer be what was looked at. The open does not wait
// (noWait): a FIFO put there since opens at once, rather than when somebody
// writes to it, and is refused like anything else that is not a regular file.
// A regular file ignores the flag, and is read as it always was.
func OpenSmall(path string, limit int64) ([]byte, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|noWait, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !smallFile(fi, limit) {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, false
	}
	return raw, true
}

// smallFile reports whether fi is a regular file of at most limit bytes.
func smallFile(fi os.FileInfo, limit int64) bool { return fi.Mode().IsRegular() && fi.Size() <= limit }

// Text is a settings file's bytes as the JSON text Claude Code parses: a
// file that starts FF FE is UTF-16 little-endian, any other is UTF-8, and
// one byte-order mark at the start of the text is dropped (Claude Code
// 2.1.319 decodes a file so, and strips U+FEFF before JSON.parse). A
// big-endian file is not decoded, as Claude Code does not decode one; a last
// odd byte of a little-endian one is dropped, as Node's decoder drops it.
func Text(raw []byte) []byte {
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		units := make([]uint16, len(raw)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(raw[2*i:])
		}
		raw = []byte(string(utf16.Decode(units)))
	}
	return bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) // U+FEFF in UTF-8
}

// AtStartupKey is the setting that connects every interactive session to
// Remote Control as it starts (code.claude.com/docs/en/settings-reference).
const AtStartupKey = "remoteControlAtStartup"

// AtStartup reads remoteControlAtStartup from one JSON object, a settings
// file's text or Claude Code's .claude.json: set says the object names it
// as true or false, on what it says. ok is false when the text is not one
// JSON object.
func AtStartup(text []byte) (on, set, ok bool) {
	var doc map[string]jsontext.Value
	if rota.UnmarshalLenient(text, &doc) != nil || doc == nil {
		return false, false, false
	}
	switch string(bytes.TrimSpace(doc[AtStartupKey])) {
	case "true":
		return true, true, true
	case "false":
		return false, true, true
	}
	return false, false, true
}
