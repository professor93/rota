package proc

import (
	"io/fs"
	"strconv"
	"testing"
	"time"
)

// fakeKernel is a /proc of one process, pid 4242, whose command name has
// spaces and parentheses of its own, on a machine whose clock the test sets.
type fakeKernel struct {
	btime int64
	ticks string
}

func (k *fakeKernel) read(name string) ([]byte, error) {
	switch name {
	case "/proc/stat":
		return []byte("cpu  1 2 3\nbtime " + strconv.FormatInt(k.btime, 10) + "\nprocesses 9\n"), nil
	case "/proc/4242/stat":
		// Fields 3 to 22: state, ppid, then sixteen more, the start last.
		return []byte("4242 (a (b) c) S 7 4242 4242 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 " + k.ticks + " 1 2\n"), nil
	case "/proc/sys/kernel/random/boot_id":
		return []byte("b0b0b0b0-0000-4000-8000-000000000001\n"), nil
	}
	return nil, fs.ErrNotExist
}

// Setting the clock moves Linux's boot time, and with it every start time
// made from it: a process that started at 12.34 s since boot reads as
// starting five seconds later once the clock has been stepped five seconds
// on. Its start in ticks since boot is the same number before and after,
// which is why a process is told by that.
func TestAClockStepMovesTheStartTimeButNotTheTicks(t *testing.T) {
	k := &fakeKernel{btime: 1_700_000_000, ticks: "1234"}
	p := Procfs{Read: k.read}
	start, err := p.StartTime(4242)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Unix(1_700_000_000, 0).Add(12340 * time.Millisecond); !start.Equal(want) {
		t.Fatalf("started at %v, want %v", start, want)
	}
	ticks, err := p.StartTicks(4242)
	if err != nil || ticks != 1234 {
		t.Fatalf("started at tick %d (%v), want 1234", ticks, err)
	}

	k.btime += 5 // timesyncd steps the clock five seconds on
	after, err := p.StartTime(4242)
	if err != nil {
		t.Fatal(err)
	}
	if d := after.Sub(start); d != 5*time.Second {
		t.Fatalf("after the step the start time moved %v, want the step's 5s", d)
	}
	if again, err := p.StartTicks(4242); err != nil || again != ticks {
		t.Fatalf("after the step the start is tick %d (%v), want %d", again, err, ticks)
	}
	if ppid, err := p.Parent(4242); err != nil || ppid != 7 {
		t.Fatalf("the parent is %d (%v), want 7", ppid, err)
	}
	if id, err := p.BootID(); err != nil || id != "b0b0b0b0-0000-4000-8000-000000000001" {
		t.Fatalf("boot id %q, %v", id, err)
	}
}

// What cannot be read is an error, never a start of zero: a process that
// is not there, a pid that names nothing, and a stat line cut short.
func TestProcfsRefusesWhatItCannotRead(t *testing.T) {
	cut := Procfs{Read: func(string) ([]byte, error) { return []byte("4242 (a) S 7 4242 4242 0 -1 4194560 1 0\n"), nil }}
	if _, err := cut.StartTicks(4242); err == nil {
		t.Error("a stat line without field 22 has a start")
	}
	k := &fakeKernel{btime: 1_700_000_000, ticks: "1234"}
	p := Procfs{Read: k.read}
	if _, err := p.StartTicks(99); err == nil {
		t.Error("a process that is not there has a start")
	}
	if _, err := p.StartTicks(0); err == nil {
		t.Error("pid 0 has a start")
	}
	k.ticks = "x"
	if _, err := p.StartTime(4242); err == nil {
		t.Error("a start that is not a number is read")
	}
}
