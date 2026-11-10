//go:build linux || darwin

package hold

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// A holder records how its CLI ended before it goes, however the CLI ends:
// by itself, stopped, or hung up on a signal to the holder. A holder killed
// outright records nothing, and its daemon, when one runs, sees it go and
// writes that moment into its record. With no daemon, its witness does: a
// second `rota serve hold`, started beside the holder in a session of its
// own, which waits on a pipe only the holder writes to. The pipe ends when
// the holder does, however it ends, and a holder that went without
// recording an ending is then stamped with the moment it went.
//
// The witness holds nothing of the session's — not its lock, its claim or
// its terminal — so it never keeps a session that has gone looking alive,
// and it outlives a kill of the holder's process group. What it cannot
// outlive is what takes both at once, a reboot or the power or a kill of
// both; then the record says the last time the holder touched it, at most
// touchEvery before it went.

// witnessed starts the holder's witness, before anything the CLI inherits
// is open, and keeps the pipe's end until the holder exits. A witness that
// cannot be started costs the moment its holder goes, never the session.
func (h *holder) witnessed(env []string) {
	self, err := os.Executable()
	if err != nil {
		h.log("no witness: this program cannot be found", "err", err)
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		h.log("no witness", "err", err)
		return
	}
	defer r.Close()
	cmd := exec.Command(self, "serve", "hold")
	cmd.Env, cmd.Dir, cmd.Stdin = env, "/", r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		h.log("no witness", "err", err)
		return
	}
	go func() { _ = cmd.Wait() }()
	raw, _ := json.Marshal(holdwire.Request{ID: h.req.ID, Witness: h.dir})
	if _, err := w.Write(append(raw, '\n')); err != nil {
		_ = w.Close()
		h.log("no witness: it was not told which session", "err", err)
		return
	}
	h.witness = w
	h.log("witnessed", "pid", cmd.Process.Pid)
}

// witness is the witness's whole life: it waits for the holder of session
// id, in dir, to go, and stamps its record with that moment when the holder
// went without recording an ending and no daemon is there to do it.
func witness(in io.Reader, id, dir string) {
	_, _ = io.Copy(io.Discard, in)
	gone := time.Now()
	m, err := holdwire.ReadMeta(dir)
	if err != nil || m.ID != id || m.Exit != nil {
		return
	}
	// A daemon running sees the holder go and stamps the moment itself; two
	// stamps a moment apart could read as different seconds before and
	// after it stops keeping the session.
	if holdwire.Locked(holdwire.DaemonLock(filepath.Dir(dir))) {
		return
	}
	// Only ever a time, and never a file made: the directory may be going.
	_ = os.Chtimes(holdwire.MetaPath(dir), gone, gone)
}
