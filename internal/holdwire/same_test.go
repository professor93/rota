package holdwire

import (
	"io/fs"
	"strconv"
	"testing"
	"time"

	"github.com/professor93/rota/internal/proc"
)

// linuxKernel is a Linux /proc with one process, pid 4242, on a machine
// whose clock the test sets: a step of the clock moves btime, and nothing
// else.
type linuxKernel struct {
	btime int64
	ticks int64
}

func (k *linuxKernel) read(name string) ([]byte, error) {
	switch name {
	case "/proc/stat":
		return []byte("btime " + strconv.FormatInt(k.btime, 10) + "\n"), nil
	case "/proc/4242/stat":
		return []byte("4242 (rota) S 1 4242 4242 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 " +
			strconv.FormatInt(k.ticks, 10) + " 1 2\n"), nil
	case "/proc/sys/kernel/random/boot_id":
		return []byte("b0b0b0b0-0000-4000-8000-000000000001\n"), nil
	}
	return nil, fs.ErrNotExist
}

// be makes SameProcess ask k rather than this system, until the test ends.
func (k *linuxKernel) be(t *testing.T) proc.Procfs {
	t.Helper()
	kernel := proc.Procfs{Read: k.read}
	st, sk, bi := startTime, startTicks, bootID
	t.Cleanup(func() { startTime, startTicks, bootID = st, sk, bi })
	startTime, startTicks, bootID = kernel.StartTime, kernel.StartTicks, kernel.BootID
	return kernel
}

// A daemon or holder recorded on Linux is still itself after the clock is
// stepped, however far: the start time its record keeps no longer matches
// the one read now, which btime moved, but its start in ticks since boot
// does. A process given the pid since started at another tick.
func TestSameProcessSurvivesAClockStep(t *testing.T) {
	k := &linuxKernel{btime: 1_700_000_000, ticks: 123456}
	kernel := k.be(t)
	start, err := kernel.StartTime(4242)
	if err != nil {
		t.Fatal(err)
	}
	ticks, err := kernel.StartTicks(4242)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := kernel.BootID()
	if err != nil {
		t.Fatal(err)
	}
	if !SameProcess(4242, start, ticks, boot) {
		t.Fatal("a process just recorded is not itself")
	}
	for _, step := range []int64{2, -2, 3600, -86400} {
		k.btime = 1_700_000_000 + step
		if now, _ := kernel.StartTime(4242); now.Equal(start) {
			t.Fatalf("a step of %ds did not move the start time", step)
		}
		if !SameProcess(4242, start, ticks, boot) {
			t.Fatalf("after the clock was stepped %ds the process is not itself", step)
		}
	}
	k.ticks++
	if SameProcess(4242, start, ticks, boot) {
		t.Fatal("a process that started a tick later is the one recorded")
	}
	k.ticks--
	if SameProcess(4242, start, ticks, boot+"-another") {
		t.Fatal("a process from another boot is the one recorded")
	}
	// A record without ticks, as macOS writes, is told by its start time.
	k.btime = 1_700_000_000
	if !SameProcess(4242, start, 0, boot) || SameProcess(4242, start.Add(5*time.Second), 0, boot) {
		t.Fatal("a record without ticks is not told by its start time")
	}
	if SameProcess(4242, time.Time{}, 0, boot) || SameProcess(0, start, ticks, boot) || SameProcess(4242, start, ticks, "") {
		t.Fatal("a record with something missing is trusted")
	}
}
