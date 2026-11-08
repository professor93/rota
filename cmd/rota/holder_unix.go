//go:build unix

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// What a run and its holder pass each other, besides the claims:
//
//   - fd 3, the word: a pipe from the run. A byte on it is "let go now",
//     because the exec did not happen. Its end closes, with nothing said,
//     when the exec does, since the run's copy is close-on-exec — and when
//     the run dies; either way the holder goes on watching the pid.
//   - fd 4, the answer: a pipe to the run, and one line on it. "ready <pid>"
//     once the holder, pid, has the claims and is watching the pid it was
//     given; "none" where nothing here can watch it, and the run keeps the
//     claims across its exec without a word; "fail <why>" for anything else,
//     which the run says. Its end closes when the holder has let go, which is
//     what a run whose exec failed waits for.
//   - fd 5 on, the claims, as many as the count says.
const (
	wordFD   = 3
	answerFD = 4
	claimFD  = 5
)

// holderWait is how long a run waits for its holder to say it is ready, and,
// when the exec failed, to have let go.
var holderWait = 10 * time.Second

// holdInBackground runs `rota serve claim <pid> <count>` and waits for it,
// which takes only as long as that process needs to start the holder in a
// session of its own and leave; then for the holder to say it is ready. The
// pipes are made here and the claims are passed as they are: everything else
// of this process's is close-on-exec, and so is what it keeps of these.
func holdInBackground(files []*os.File) (claimHolder, error) {
	if !exitWatchable {
		return nil, errNoHolder
	}
	if _, err := sessionOf(0); err != nil {
		// A unix with no getsid: the holder could not tell it had left this
		// session, as the watcher could not (see watchInBackground).
		return nil, errNoHolder
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	wordR, wordW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	answerR, answerW, err := os.Pipe()
	if err != nil {
		_ = wordR.Close()
		_ = wordW.Close()
		return nil, err
	}
	h := &holder{word: wordW, answer: answerR}
	deadline := time.Now().Add(holderWait)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "serve", "claim", strconv.Itoa(os.Getpid()), strconv.Itoa(len(files)))
	cmd.ExtraFiles = append([]*os.File{wordR, answerW}, files...)
	err = cmd.Run()
	// The holder has its own ends now; with these closed, its answer's end
	// is the only one left, and closing it is an end of file here.
	_ = wordR.Close()
	_ = answerW.Close()
	if err != nil {
		h.close()
		return nil, fmt.Errorf("serve claim: %w", err)
	}
	_ = answerR.SetReadDeadline(deadline)
	line, err := bufio.NewReaderSize(answerR, 512).ReadSlice('\n')
	if err != nil {
		h.close()
		return nil, fmt.Errorf("the holder never said it was ready: %w", err)
	}
	if h.pid, err = heard(string(line)); err != nil {
		h.close()
		return nil, err
	}
	return h, nil
}

// heard reads the holder's answer: its pid when it is ready, errNoHolder when
// nothing here can watch the run, or what went wrong.
func heard(line string) (pid int, err error) {
	said := strings.TrimSuffix(line, "\n")
	word, rest, _ := strings.Cut(said, " ")
	switch {
	case word == "ready":
		if pid, err := strconv.Atoi(rest); err == nil && pid > 0 {
			return pid, nil
		}
	case said == "none":
		return 0, errNoHolder
	case word == "fail" && rest != "":
		return 0, errors.New(rest)
	}
	return 0, fmt.Errorf("the holder said %q", said)
}

// holder is the run's side of a holder that is ready.
type holder struct {
	word   *os.File
	answer *os.File
	pid    int
	once   sync.Once
}

func (h *holder) letGo() {
	h.once.Do(func() {
		_, _ = h.word.Write([]byte{'\n'})
		_ = h.answer.SetReadDeadline(time.Now().Add(holderWait))
		_, _ = io.Copy(io.Discard, h.answer)
		h.close()
	})
}

func (h *holder) close() {
	_ = h.word.Close()
	_ = h.answer.Close()
}

// holdLent is the holder, and the step on the way to it.
//
// Started by a run, it is not yet in a session of its own: it starts itself
// again in one, at /, with nothing open but /dev/null and what it was lent,
// and leaves, so that the process that started it — a rota about to become
// the CLI — has no child left behind. That one makes every descriptor it was
// lent close-on-exec, starts watching pid, says it is ready, and lets go when
// pid exits or the run says so.
func holdLent(pid, n int) error {
	files, err := lentFiles(n)
	if err != nil {
		return err
	}
	if !ownSession() {
		// Only the run it is lent by may start a holder: the pid it is to
		// watch is the one that started this process.
		if os.Getppid() != pid {
			return usageErr("serve claim is internal: an interactive rota run starts it")
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, "serve", "claim", strconv.Itoa(pid), strconv.Itoa(n))
		cmd.Dir = "/"
		cmd.ExtraFiles = files
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	// Only the CLI's exit lets go, or the run saying so: a signal sent to
	// every process called rota — `pkill rota` to stop a server — must not
	// free a running session's account. It has no terminal to hang up, and
	// at a logout or a shutdown the CLI it holds for ends as well. SIGKILL
	// still ends it, and lets go early.
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	for _, f := range files {
		syscall.CloseOnExec(int(f.Fd()))
	}
	word, answer, claims := files[0], files[1], files[2:]
	wait, err := exitOf(pid)
	if err != nil {
		say := "fail " + strings.Join(strings.Fields(err.Error()), " ")
		if errors.Is(err, errNoHolder) {
			say = "none"
		}
		_, _ = io.WriteString(answer, say+"\n")
		return nil
	}
	if _, err := fmt.Fprintf(answer, "ready %d\n", os.Getpid()); err != nil {
		// The run stopped waiting and kept its claims across the exec
		// itself, or has gone: nobody needs these from here.
		return nil
	}
	// Let go of by whichever comes first, in the goroutine that learnt of it
	// rather than after handing it on: whoever waited for the CLI may be
	// starting the next run on this account already. The claims first: the
	// answer's end closing is what a run whose exec failed waits for, and by
	// then they must be free.
	var once sync.Once
	gone := make(chan struct{})
	letGo := func() {
		once.Do(func() {
			for _, f := range claims {
				_ = f.Close()
			}
			_ = answer.Close()
			close(gone)
		})
	}
	go func() { wait(); letGo() }()
	go func() {
		var b [1]byte
		if n, _ := word.Read(b[:]); n > 0 {
			letGo()
		}
	}()
	<-gone
	return nil
}

// lentFiles are the descriptors a run lends: the word and the answer, which
// are pipes, and n claims, which are files. Anything else there — a holder
// typed by hand — is refused before it is used.
func lentFiles(n int) ([]*os.File, error) {
	files := make([]*os.File, 0, 2+n)
	for fd := wordFD; fd < claimFD+n; fd++ {
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			return nil, usageErr("serve claim is internal: an interactive rota run starts it")
		}
		want := uint32(syscall.S_IFREG)
		if fd < claimFD {
			want = syscall.S_IFIFO
		}
		if uint32(st.Mode)&syscall.S_IFMT != want {
			return nil, usageErr("serve claim is internal: an interactive rota run starts it")
		}
		files = append(files, os.NewFile(uintptr(fd), "lent-"+strconv.Itoa(fd)))
	}
	return files, nil
}
