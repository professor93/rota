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
// what the person types to more than one receiver in turn: the TUI, then an
// attach, then the TUI again. Its goroutine reads a chunk and holds it until
// all of it has been received, and only then reads on. So whoever receives
// next gets it, and a receiver that has stopped receiving takes nothing with
// it: an SSH channel's Read cannot be interrupted, and two goroutines reading
// one would each take keys the other was owed.
//
// There are two ways to receive, and one receiver at a time. An attach
// receives whole chunks on Input, as attach.Terminal asks. Anything that
// takes an io.Reader, the TUI's among them, reads through Read, and what one
// Read cannot fit stays at the front for whoever receives next, on Input or
// by Read. Before the terminal goes from a reader to an attach, Hold ends the
// Read waiting, if one is, with io.EOF and nothing taken, and refuses Reads
// until Resume.
//
// The goroutine ends when the input ends, or after stop once the read it may
// be waiting in returns. That read is the channel's, which returns when the
// channel closes, as it does when its command returns; until then a stopped
// pump holds nothing for anybody.
type input struct {
	c     chan []byte   // Input
	reqs  chan inputReq // each Read, asking for what it can take
	done  chan struct{} // stop
	ended chan struct{} // the goroutine has returned
	once  sync.Once

	mu      sync.Mutex
	held    bool
	wake    chan struct{} // closed by Hold, for the Reads waiting
	reading int           // Reads under way
	idle    *sync.Cond    // reading has come to 0
}

// inputAsking is what a Read does after it has seen the reads are not held
// and before it asks the pump: nothing. A test knob, which holds a Read
// there while a Hold comes, so that the pump is seen to look again.
var inputAsking = func() {}

// inputReq is one Read: p to fill, and n to say how much was put in it, 0
// for nothing because the reads are held.
type inputReq struct {
	p []byte
	n chan int
}

// inputPump starts the one reader of r. stop ends it.
func inputPump(r io.Reader) *input {
	in := &input{
		c: make(chan []byte), reqs: make(chan inputReq),
		done: make(chan struct{}), ended: make(chan struct{}),
		wake: make(chan struct{}),
	}
	in.idle = sync.NewCond(&in.mu)
	go in.pump(r)
	return in
}

// pump reads r until it ends or the pump is stopped, and hands on each chunk
// before it reads the next: whole to a receiver on Input, or as much as a
// Read can take, keeping the rest at the front. A Read asking while the
// reads are held is given nothing, and the chunk stays.
func (in *input) pump(r io.Reader) {
	defer close(in.ended)
	defer close(in.c)
	buf := make([]byte, inputChunk)
	for {
		n, err := r.Read(buf)
		// A copy: a receiver on Input keeps what it is given, and buf is
		// read into again.
		front := bytes.Clone(buf[:n])
		for len(front) > 0 {
			select {
			case in.c <- front:
				front = nil
			case q := <-in.reqs:
				k := 0
				if !in.isHeld() {
					k = copy(q.p, front)
					front = front[k:]
				}
				q.n <- k
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

// Read is the input as an io.Reader. It is io.EOF while the reads are held,
// once the input has ended, and once the pump has stopped; a Read waiting
// then ends with it, having taken nothing.
func (in *input) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	in.mu.Lock()
	if in.held {
		in.mu.Unlock()
		return 0, io.EOF
	}
	in.reading++
	wake := in.wake
	in.mu.Unlock()
	defer func() {
		in.mu.Lock()
		if in.reading--; in.reading == 0 {
			in.idle.Broadcast()
		}
		in.mu.Unlock()
	}()
	inputAsking()
	q := inputReq{p: p, n: make(chan int, 1)}
	select {
	case in.reqs <- q:
		// The pump answers at once, and with nothing when the reads were
		// held meanwhile.
		if n := <-q.n; n > 0 {
			return n, nil
		}
	case <-wake:
	case <-in.ended:
	case <-in.done:
	}
	return 0, io.EOF
}

// Hold refuses Reads until Resume, and returns once no Read is under way:
// one that was waiting has ended with io.EOF and nothing taken, and one
// already given what was typed before the hold has returned it. What is
// typed meanwhile is for a receiver on Input. A second Hold does nothing.
func (in *input) Hold() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.held {
		in.held = true
		close(in.wake)
	}
	for in.reading > 0 {
		in.idle.Wait()
	}
}

// Resume lets Reads go on after Hold.
func (in *input) Resume() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.held {
		in.held = false
		in.wake = make(chan struct{})
	}
}

func (in *input) isHeld() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.held
}

// stop ends the pump. What it holds is given to nobody who comes after; a
// receiver already waiting on Input may still be handed it as the pump goes.
// A second stop does nothing.
func (in *input) stop() { in.once.Do(func() { close(in.done) }) }
