// Package ring keeps the last bytes a terminal printed. The server's own
// terminals keep one, and so do a holder and the daemon that links to it,
// which is why it is a package of its own: the two ends of a held session
// count offsets the same way because they count them with the same code.
package ring

// DefaultKeep is what a ring told no size keeps: two megabytes, the server's
// default scrollback.
const DefaultKeep = 2 << 20

// Ring is the last bytes a terminal printed, with absolute offsets: a
// client says where it got to as a number that never repeats, and the ring
// answers with everything since — or says how much it no longer has.
//
// It is circular because a terminal prints for hours. A slice that dropped
// its head would copy the whole scrollback on every burst of output.
//
// It has no lock of its own: whoever owns one already holds a lock around
// everything that writes and reads it.
type Ring struct {
	buf   []byte
	size  int   // bytes held
	start int   // index of the oldest byte
	end   int64 // absolute offset just past the newest byte
}

// New is a ring that keeps keep bytes, counting from zero.
func New(keep int) *Ring { return NewAt(keep, 0) }

// NewAt is a ring whose first byte will be at offset end, for a ring that
// continues another's numbering: a daemon linking to a holder mid-session
// answers in the holder's offsets.
func NewAt(keep int, end int64) *Ring {
	if keep <= 0 {
		keep = DefaultKeep
	}
	return &Ring{buf: make([]byte, keep), end: end}
}

// Write keeps p, dropping the oldest bytes to make room.
func (r *Ring) Write(p []byte) {
	r.end += int64(len(p))
	if len(p) >= len(r.buf) {
		// A burst larger than the whole ring: only its tail can be kept.
		copy(r.buf, p[len(p)-len(r.buf):])
		r.start, r.size = 0, len(r.buf)
		return
	}
	at := (r.start + r.size) % len(r.buf)
	n := copy(r.buf[at:], p)
	copy(r.buf, p[n:])
	if r.size += len(p); r.size > len(r.buf) {
		r.start = (r.start + r.size - len(r.buf)) % len(r.buf)
		r.size = len(r.buf)
	}
}

// Oldest is the lowest offset this ring can still answer for.
func (r *Ring) Oldest() int64 { return r.end - int64(r.size) }

// End is the offset just past the newest byte: where the next write starts.
func (r *Ring) End() int64 { return r.end }

// Read is everything from since onwards, and the offset the answer actually
// starts at — which is later than asked for when the ring has moved on. The
// caller compares the two to know whether to say so first.
func (r *Ring) Read(since int64) ([]byte, int64) {
	from := since
	if oldest := r.Oldest(); from < oldest {
		from = oldest
	}
	if from > r.end {
		from = r.end
	}
	skip := int(from - r.Oldest())
	n := r.size - skip
	if n <= 0 {
		return nil, from
	}
	out := make([]byte, 0, n)
	i := (r.start + skip) % len(r.buf)
	if i+n <= len(r.buf) {
		out = append(out, r.buf[i:i+n]...)
	} else {
		out = append(out, r.buf[i:]...)
		out = append(out, r.buf[:n-(len(r.buf)-i)]...)
	}
	return out, from
}
