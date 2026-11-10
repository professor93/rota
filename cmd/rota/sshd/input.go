package sshd

import (
	"bytes"
	"io"
	"sync"
)

// inputChunk is the most one read of a channel's input takes: an SSH
// packet's worth.
const inputChunk = 32 << 10

// input is the one reader of a channel's input, for a command that hands
// what the person types to more than one receiver in turn: the TUI and then
// an attach, and the TUI again after it. Its goroutine reads a chunk and
// holds it until somebody receives it, and only then reads on. So the next
// receiver, whoever it is, gets the chunk, and a receiver that has stopped
// receiving takes nothing with it: an SSH channel's Read cannot be
// interrupted, and two goroutines reading one would each take a key the
// other was owed.
//
// An attach receives on Input, as attach.Terminal asks, and anything that
// takes an io.Reader reads through Read. What a Read could not fit is the
// next Read's, and Input never sees it, so a reader that hands over to an
// attach is one that receives on Input itself.
type input struct {
	c    chan []byte
	done chan struct{}
	once sync.Once

	mu   sync.Mutex
	left []byte // what the last Read could not fit
}

// inputPump starts the one reader of r. stop ends it.
func inputPump(r io.Reader) *input {
	in := &input{c: make(chan []byte), done: make(chan struct{})}
	go in.pump(r)
	return in
}

// pump reads r until it ends or the pump is stopped, handing on each chunk
// before it reads the next, and closes Input when it is done. A pump
// stopped while it waits in Read ends when that Read does: when the
// channel closes, which it does as its command returns.
func (in *input) pump(r io.Reader) {
	defer close(in.c)
	buf := make([]byte, inputChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			select {
			case in.c <- bytes.Clone(buf[:n]):
			case <-in.done:
				return
			}
		}
		if err != nil {
			return
		}
		select {
		case <-in.done:
			return
		default:
		}
	}
}

// Input is each chunk the person types, a chunk the receiver keeps. It is
// closed when their input ends, or once the pump is stopped.
func (in *input) Input() <-chan []byte { return in.c }

// Read is Input as an io.Reader: io.EOF once the input has ended or the
// pump has stopped, and a Read waiting then ends with it.
func (in *input) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.left) == 0 {
		select {
		case b, ok := <-in.c:
			if !ok {
				return 0, io.EOF
			}
			in.left = b
		case <-in.done:
			return 0, io.EOF
		}
	}
	n := copy(p, in.left)
	in.left = in.left[n:]
	return n, nil
}

// stop ends the pump: a chunk it holds is dropped, and nothing more is
// read once the Read it may be waiting in returns. A second stop does
// nothing.
func (in *input) stop() { in.once.Do(func() { close(in.done) }) }
