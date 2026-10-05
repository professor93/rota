package claudecode

import (
	"context"
	"errors"
	"os/exec"
)

// runSecurity is Security for real: the system's own keychain tool, by its
// absolute path, so nothing earlier on PATH can stand in for it. Claude Code
// reads and writes its item through this same tool, which is why the item
// lets it in without asking anybody.
//
// rota only ever finds and deletes. Writing an item through security resets
// who may read it, and the item is Claude Code's to write.
func runSecurity(ctx context.Context, args ...string) ([]byte, int, error) {
	if inTest() {
		return nil, 0, errInTest
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/security", args...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode(), nil
	}
	if err != nil {
		return nil, 0, err
	}
	return out, 0, nil
}
