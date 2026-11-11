package notify

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/professor93/rota/internal/owner"
)

// readSecretFile reads the secret a sink's key names in path, such as
// bot_token_file or secret_file, as every sink reads one: once, at start.
//
// A link is followed, as secret managers make them, and the file it ends at
// is opened without following a link again or waiting on a FIFO
// (owner.OpenFlags). What was opened must be a plain file, so that a FIFO
// or a device never holds up the daemon's start. It must be its owner's
// alone (owner.Only, on the mode of what was opened, said by the path as
// configured), at most max bytes, and hold something besides space, which
// is trimmed. what is what it should hold, as an error says it: "bot token".
//
// Its error names path and never quotes what is in the file.
func readSecretFile(key, path, what string, max int) (string, error) {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	f, err := os.OpenFile(target, os.O_RDONLY|owner.OpenFlags, 0)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s %s is not a plain file", key, path)
	}
	if err := owner.Only(path, fi.Mode()); err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", fmt.Errorf("%s %s cannot be read", key, path)
	}
	if len(raw) > max {
		return "", fmt.Errorf("%s %s is more than %d bytes; it should hold only the %s", key, path, max, what)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("%s %s holds no %s", key, path, what)
	}
	return s, nil
}
