//go:build linux || darwin

package api

import (
	"os/exec"
	"syscall"

	"github.com/professor93/rota/internal/holdwire"
)

// detachHolder puts a holder in a session of its own, so that a signal to
// this server — a Ctrl-C, systemd stopping it — never reaches a holder.
func detachHolder(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// signal sends sig to the holder, and only to the holder: through the
// process this server started while it is still this server's child, and
// otherwise to its pid only while that pid is still the process the record
// names.
func (h *heldTerm) signal(sig syscall.Signal) {
	if h.proc != nil {
		_ = h.proc.Signal(sig)
		return
	}
	_ = holdwire.Signal(h.pid, h.start, h.ticks, h.boot, sig)
}
