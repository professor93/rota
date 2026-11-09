//go:build darwin

package proc

import (
	"testing"
	"unsafe"
)

// macOS names each boot with a UUID of its own. The boot time is no name
// for it: setting the clock moves kern.boottime, and a moved clock is not a
// reboot.
func TestBootIDIsThisBootsUUID(t *testing.T) {
	id, err := BootID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 {
		t.Fatalf("boot id %q is not a UUID", id)
	}
	for i, c := range []byte(id) {
		dash := i == 8 || i == 13 || i == 18 || i == 23
		hex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if dash && c != '-' || !dash && !hex {
			t.Fatalf("boot id %q is not a UUID", id)
		}
	}
}

// PeerUID reads a struct xucred whole, so its layout here must be the
// kernel's: four bytes of version, four of uid, a short and its padding, and
// sixteen groups.
func TestXucredIsTheKernelsSize(t *testing.T) {
	if n := unsafe.Sizeof(xucred{}); n != 76 {
		t.Fatalf("xucred is %d bytes, want 76", n)
	}
	if off := unsafe.Offsetof(xucred{}.uid); off != 4 {
		t.Fatalf("xucred's uid is at %d, want 4", off)
	}
}
