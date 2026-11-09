//go:build linux || darwin

package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	nethttp "net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/professor93/rota/api"
	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/cmd/rota/hold"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/wire"
)

// holderKillAfter is how long a holder that has been hung up is given
// before it is killed: its own hold.StopAfter to stop its CLI, and two
// seconds more to record how the CLI ended, as a daemon gives it.
// stopAllWait is how long stop --all waits for each session to end: that,
// and one more second for its lock to be seen free. Both are read when they
// are used, so they follow hold.StopAfter.
func holderKillAfter() time.Duration { return hold.StopAfter + 2*time.Second }

func stopAllWait() time.Duration { return holderKillAfter() + time.Second }

/* ---------------------------------------------------------------- start --- */

// startDaemon is `rota serve start`: spawnDaemon, said. A daemon already
// there, before this start began or by winning the store from this start's
// own, is named, and the command is content: there is one daemon, which is
// what was asked for. Each path and address it says is drawn with a space
// for each control character in it (see oneLine), as in every line the
// daemon's commands print.
func (c *cli) startDaemon(ctlOnly bool, conf string) error {
	r, err := c.spawnDaemon(ctlOnly, conf)
	if err != nil {
		return err
	}
	if r.running != nil {
		fmt.Fprintf(c.out, "rota daemon already running for %s%s\n", oneLine(r.home), pidOf(r.running))
		return nil
	}
	line := fmt.Sprintf("rota daemon started for %s (pid %d); control socket %s", oneLine(r.home), r.started.PID, oneLine(r.sock))
	if r.started.HTTP != "" {
		line += "; http on " + oneLine(r.started.HTTP)
	}
	fmt.Fprintln(c.out, line)
	return nil
}

// spawnDaemon starts the daemon in a session of its own, in /, on the store
// this command names, written down by absolute path, with nothing of this
// terminal's or of an account a CLI rota launched runs as (daemonEnv), and
// waits until it answers on its control socket as the
// process this started. It says which daemon serves the store then: the one
// it started, or one found already running — before anything started, or
// having won the store from the one this started, which then stops.
func (c *cli) spawnDaemon(ctlOnly bool, conf string) (*daemonStart, error) {
	home, terminals, err := daemonStore()
	if err != nil {
		return nil, err
	}
	sock := holdwire.CtlSock(terminals)
	if err := holdwire.SocketsFit(terminals); err != nil {
		return nil, err
	}
	already := func(d *holdwire.Daemon) (*daemonStart, error) {
		return &daemonStart{home: home, sock: sock, running: d}, nil
	}
	if d, ok := daemonOf(terminals); ok {
		return already(d)
	}
	// Read here as the daemon will read it, so a file it would refuse is
	// refused at this terminal rather than in a log; the daemon runs in /,
	// so a file named relatively is named absolutely.
	if conf != "" {
		if conf, err = filepath.Abs(conf); err != nil {
			return nil, err
		}
	}
	cfg, _, _, err := daemonConfig(conf, home)
	if err != nil {
		return nil, err
	}
	logPath := daemonLog(home, cfg)
	lf, err := openLogFile(logPath, c.err)
	if err != nil {
		return nil, err
	}
	lf.Close()
	var from int64
	if fi, err := os.Stat(logPath); err == nil {
		from = fi.Size()
	}

	exe, err := invokedAs()
	if err != nil {
		return nil, err
	}
	args := []string{"serve", "start", "--foreground"}
	if ctlOnly {
		args = append(args, "--ctl-only")
	}
	if conf != "" {
		args = append(args, "--config", conf)
	}
	// The daemon runs in /, so every directory its environment names is
	// named absolutely, as the store is: the person's own Claude Code
	// directory too, which every claude account's home is made from.
	env, err := absoluteDirs(daemonEnv(home))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = "/"
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the rota daemon: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	pid := cmd.Process.Pid
	// One try at a time, none of which waits, and the whole bounded by
	// daemonReady even should a try be slow: a try still out when the time
	// is up is left to finish on its own.
	deadline := time.NewTimer(daemonReady)
	defer deadline.Stop()
	for {
		tried := make(chan *holdwire.Daemon, 1)
		go func() { tried <- daemonAnswering(terminals) }()
		select {
		case <-exited:
			if d, ok := daemonOf(terminals); ok {
				return already(d)
			}
			if lines := logTail(logPath, from, 5); len(lines) > 0 {
				for i, l := range lines {
					lines[i] = oneLine(l)
				}
				return nil, fmt.Errorf("the rota daemon did not start; the end of %s:\n  %s", oneLine(logPath), strings.Join(lines, "\n  "))
			}
			return nil, fmt.Errorf("the rota daemon did not start, and wrote nothing to %s", oneLine(logPath))
		case d := <-tried:
			if d != nil && d.PID == pid {
				return &daemonStart{home: home, sock: sock, started: d}, nil
			}
		case <-deadline.C:
			// Another daemon answering, which this one is slow to lose the
			// store to, is still one daemon running (P6). Asked without
			// waiting, so the whole stays within daemonReady.
			if d, ok := holdwire.RunningDaemon(terminals); ok && d.PID != 0 && d.PID != pid {
				return already(d)
			}
			return nil, fmt.Errorf("the rota daemon (pid %d) has not answered on %s within %s; rota serve status says when it does, and %s what it is doing",
				pid, oneLine(sock), daemonReady, oneLine(logPath))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// daemonAnswering is the daemon serving the store, if one answers on its
// control socket at once, as daemon.pid says once it has: whatever it
// records before it listens is there by then. It does not wait for a
// socket that is slow to answer; its caller tries again.
func daemonAnswering(terminals string) *holdwire.Daemon {
	c, err := rotaclient.DialNow(terminals)
	if err != nil || c.Health() != nil {
		return nil
	}
	if d, ok := holdwire.RunningDaemon(terminals); ok {
		return d
	}
	return nil
}

// daemonOf is the daemon serving the store, if one is. One that has taken
// the lock writes who it is a moment later, so a record that names nobody
// yet is waited for, briefly; after that its pid is 0, and nobody is named
// rather than a pid that may be anybody's.
func daemonOf(terminals string) (*holdwire.Daemon, bool) {
	for try := 0; ; try++ {
		d, ok := holdwire.RunningDaemon(terminals)
		if !ok || d.PID != 0 || try == 20 {
			return d, ok
		}
		time.Sleep(50 * time.Millisecond)
	}
}

/* ------------------------------------------------------------ the daemon --- */

// runDaemon is `rota serve start --foreground`: the daemon itself. Its log
// is opened before anything else can go wrong, and from then on nothing is
// said on stderr, which under serve start nobody reads: every line goes to
// the log, a failure to start included, and the exit status says the rest.
//
// It claims the store (api.New, for a server with HoldCommand) — and exits
// 0, saying so in the log, when another daemon already has — opens a TCP
// port only where server.toml asks and --ctl-only does not forbid, and
// serves ctl.sock until SIGTERM or an interrupt. A hangup is not a stop. On
// the way out, all within serveGrace, both listeners close, then Stop ends
// their WebSockets and lets go of every session, still running, for the
// next daemon to take up, and of the claim last; then the WebSockets' goodbyes
// are waited for.
func (c *cli) runDaemon(ctlOnly bool, conf string) error {
	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	cfg, opts, listener, cfgErr := daemonConfig(conf, home)
	lf, err := openLogFile(daemonLog(home, cfg), c.err)
	if err != nil {
		return err
	}
	defer lf.Close()
	quiet := cfg != nil && cfg.Server.Quiet
	log := slog.New(slog.NewTextHandler(lf, &slog.HandlerOptions{Level: level(quiet)})).With("pid", os.Getpid())
	log.Info("rota daemon starting", "version", wire.Version, "store", home)
	failed := func(err error) error {
		log.Error("the rota daemon did not start", "err", unsecret(err.Error()))
		return exitCode(1)
	}
	if cfgErr != nil {
		return failed(cfgErr)
	}

	// Both before the claim, so that a stop asked for while this starts is
	// a stop once it has, and holders it starts take a hangup as theirs to
	// act on: a signal taken rather than ignored goes back to its default
	// in a child.
	ctx, stop := serveUntil()
	defer stop()
	hups := make(chan os.Signal, 1)
	signal.Notify(hups, syscall.SIGHUP)
	defer func() { signal.Stop(hups); close(hups) }()
	go func() {
		for range hups {
			log.Info("a hangup does not stop the rota daemon; rota serve stop or SIGTERM does")
		}
	}()

	tok, _, err := cfg.Token()
	if err != nil {
		return failed(err)
	}
	if tok == "" {
		// The API refuses to serve without one, and nobody needs this one:
		// the control socket asks for none, and a port takes the file's
		// users and tokens.
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return failed(err)
		}
		tok = hex.EncodeToString(b[:])
	}
	exe, err := invokedAs()
	if err != nil {
		return failed(err)
	}
	opts.Token, opts.Log, opts.HoldCommand, opts.HoldDeadline = tok, log, daemonHoldCommand(exe), daemonHoldDeadline
	srv, err := api.New(opts)
	var running holdwire.DaemonRunning
	if errors.As(err, &running) {
		// The store has its one daemon, which is no failure: a supervisor
		// that started this one beside it would otherwise start it again
		// every few seconds for as long as that one runs.
		log.Info("another rota daemon serves this store, so this one exits 0 rather than fail", "daemon", running.PID)
		return nil
	}
	if err != nil {
		return failed(err)
	}
	// The claim New took must be on the store this daemon names, or its
	// pid file and socket would be another store's.
	if !srv.HoldsDaemon(terminals) {
		srv.Stop()
		return failed(fmt.Errorf("the server did not claim %s", holdwire.DaemonLock(terminals)))
	}
	requests := &inFlight{}
	port, err := daemonPort(cfg, ctlOnly, listener, srv, requests, log)
	if err != nil {
		srv.Stop()
		return failed(err)
	}
	if port != nil {
		if err := holdwire.RecordHTTP(terminals, port.addr); err != nil {
			log.Warn("daemon.pid could not say where else this daemon serves", "err", err)
		}
		for _, note := range serveNotes(&opts, listener, true) {
			log.Info(note)
		}
	}
	stopCtl, err := listenCtl(srv, terminals, log, requests)
	if err != nil {
		if port != nil {
			port.ln.Close()
		}
		srv.Stop()
		return failed(err)
	}
	served := make(chan error, 1)
	said := []any{"version", wire.Version, "store", home, "ctl", holdwire.CtlSock(terminals)}
	if port != nil {
		go func() { served <- port.serve() }()
		said = append(said, "http", port.addr)
	}
	log.Info("rota daemon started", said...)

	var portErr error
	select {
	case <-ctx.Done():
	case portErr = <-served:
	}
	stop()
	began := time.Now()
	log.Info("rota daemon stopping; its sessions go on running")
	// No sweep starts while the listeners wait for what they are serving, as
	// for a foreground serve; Stop, below, waits for any refresh still under
	// way before this process is gone.
	srv.StopRefreshing()
	// The listeners first, together: neither takes another connection, and
	// what each is serving has serveGrace, less the goodbye, to finish. The
	// control socket goes while this daemon still holds the claim, so no
	// successor finds it answering there.
	var wg sync.WaitGroup
	if port != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shut, cancel := context.WithDeadline(context.Background(), began.Add(serveGrace-daemonGoodbye))
			defer cancel()
			_ = port.server.Shutdown(shut)
			_ = port.server.Close()
		}()
	}
	stopCtl()
	wg.Wait()
	// Then the server, which ends every WebSocket either listener handed
	// over, lets go of every held session, still running, and lets go of
	// the claim last; and what it ended has the rest of serveGrace to say
	// goodbye before this process is gone.
	srv.Stop()
	if !requests.wait(began.Add(serveGrace)) {
		log.Warn("requests were still open as the rota daemon stopped")
	}
	if portErr != nil && !errors.Is(portErr, nethttp.ErrServerClosed) {
		log.Error("rota daemon stopped: its port did", "err", unsecret(portErr.Error()))
		return exitCode(1)
	}
	log.Info("rota daemon stopped")
	return nil
}

// daemonListener is a daemon's TCP port: bound, and its certificate read,
// before the daemon says it has started, so that a port it cannot serve is
// a daemon that did not start rather than one that stops.
type daemonListener struct {
	addr   string
	ln     net.Listener
	server *nethttp.Server
	tls    bool
}

func (p *daemonListener) serve() error {
	if p.tls {
		return p.server.ServeTLS(p.ln, "", "")
	}
	return p.server.Serve(p.ln)
}

// daemonPort opens the port a daemon serves on besides ctl.sock, or none.
func daemonPort(cfg *api.Config, ctlOnly bool, listener api.Listener, srv *api.Server, requests *inFlight, log *slog.Logger) (*daemonListener, error) {
	if !daemonListens(cfg, ctlOnly) {
		return nil, nil
	}
	server := newHTTPServer(listener.Addr, requests.count(srv.Handler()), log)
	if listener.Cert != "" {
		pair, err := tls.LoadX509KeyPair(listener.Cert, listener.Key)
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		server.TLSConfig.Certificates = []tls.Certificate{pair}
	}
	ln, err := net.Listen("tcp", listener.Addr)
	if err != nil {
		return nil, err
	}
	return &daemonListener{addr: ln.Addr().String(), ln: ln, server: server, tls: listener.Cert != ""}, nil
}

/* ----------------------------------------------------------------- stop --- */

// refusedInsideHeldSession refuses command — `rota serve stop`, say — run
// from inside a held session, where it is most likely the agent in one by
// mistake: the same rule, and the same guard against accidents, as
// ctl.sock's watch-only one. A process whose session cannot be told is
// refused as inside.
func refusedInsideHeldSession(command, terminals string) error {
	switch id, err := heldSessionOf(os.Getpid(), terminals); {
	case err != nil:
		return fmt.Errorf("%s cannot tell whether it is inside a held session (%s), so it is refused; run it from outside the session", command, oneLine(err.Error()))
	case id != "":
		return fmt.Errorf("%s is refused inside held session %s; run it from outside the session", command, oneLine(id))
	}
	return nil
}

// stopDaemon is `rota serve stop`, and with --all `rota serve stop --all`.
// Neither is run from inside a held session.
func (c *cli) stopDaemon(all, yes bool) error {
	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	if err := refusedInsideHeldSession("rota serve stop", terminals); err != nil {
		return err
	}
	if all {
		return c.stopAll(home, terminals, yes)
	}
	if _, ok := holdwire.RunningDaemon(terminals); !ok {
		fmt.Fprintf(c.out, "no rota daemon is running for %s\n", oneLine(home))
		return nil
	}
	if err := endDaemon(terminals); err != nil {
		return err
	}
	n := len(runningRecords(terminals))
	verb := "keep"
	if n == 1 {
		verb = "keeps"
	}
	fmt.Fprintf(c.out, "rota daemon stopped; %s %s running\n", sessionsN(n), verb)
	return nil
}

// endDaemon asks the store's daemon to stop — SIGTERM, only while its pid is
// still the process daemon.pid names — and waits for it to let go of the
// store. Its sessions go on running; it lets go of them before its claim.
func endDaemon(terminals string) error {
	_, err := signalDaemon(terminals)
	return err
}

// signalDaemon is endDaemon, and says which daemon it sent SIGTERM: nil when
// it sent none, because none was running or the one that was went on its
// own meanwhile. A daemon held by a process daemon.pid does not name, or
// names no checkable process for, is an unnamedDaemon.
func signalDaemon(terminals string) (*holdwire.Daemon, error) {
	d, ok := daemonOf(terminals)
	if !ok {
		return nil, nil
	}
	if d.PID == 0 {
		return nil, unnamedDaemon{fmt.Sprintf("a rota daemon holds %s, but %s names no process this rota can check; nothing was signalled",
			oneLine(holdwire.DaemonLock(terminals)), oneLine(holdwire.DaemonPIDFile(terminals)))}
	}
	err := holdwire.Signal(d.PID, d.ProcStart, d.ProcStartTicks, d.BootID, syscall.SIGTERM)
	switch {
	case errors.Is(err, holdwire.ErrNotSameProcess) && !holdwire.Locked(holdwire.DaemonLock(terminals)):
		return nil, nil // it went on its own meanwhile
	case errors.Is(err, holdwire.ErrNotSameProcess):
		return nil, unnamedDaemon{fmt.Sprintf("pid %d is no longer the rota daemon %s names; nothing was signalled", d.PID, oneLine(holdwire.DaemonPIDFile(terminals)))}
	case err != nil:
		return nil, err
	}
	for deadline := time.Now().Add(daemonStopWait); holdwire.Locked(holdwire.DaemonLock(terminals)); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the rota daemon (pid %d) has not stopped within %s", d.PID, daemonStopWait)
		}
	}
	return d, nil
}

// unnamedDaemon is a daemon holding the store that daemon.pid does not name
// a process for, which so was not signalled.
type unnamedDaemon struct{ why string }

func (u unnamedDaemon) Error() string { return u.why }

// stopAll is `rota serve stop --all`: every session ended, then the daemon.
// With a daemon, it is asked to end each, by the same route as anybody's
// stop, and waited for; with none, each holder is hung up directly, by the
// rule a daemon stops one by. Either way a session counts as ended once its
// lock is free, which its holder lets go of only after its CLI has gone.
func (c *cli) stopAll(home, terminals string, yes bool) error {
	if !yes && !c.atTerminal() {
		return usageErr("serve stop --all ends every session as well, so off a terminal it needs --yes")
	}
	if _, ok := holdwire.RunningDaemon(terminals); !ok {
		return c.stopHolders(home, terminals, yes)
	}
	cl, err := rotaclient.Dial(terminals)
	if errors.Is(err, rotaclient.ErrNoDaemon) {
		return c.stopHolders(home, terminals, yes)
	}
	if err != nil {
		return err
	}
	all, err := cl.List()
	if err != nil {
		return err
	}
	var ids []string
	for _, s := range all {
		if s.Ended == nil {
			ids = append(ids, s.ID)
		}
	}
	if len(ids) > 0 && !yes && !c.confirm(fmt.Sprintf("stop the rota daemon and end %s? [y/N] ", sessionsN(len(ids)))) {
		fmt.Fprintln(c.err, "rota: nothing was stopped")
		return exitCode(1)
	}
	why := map[string]error{}
	for _, id := range ids {
		if err := cl.Stop(id); err != nil {
			why[id] = err
		}
	}
	left := waitEnded(terminals, ids, stopAllWait())
	if err := endDaemon(terminals); err != nil {
		return err
	}
	if len(left) > 0 {
		return notEnded("rota daemon stopped", len(ids), left, why)
	}
	fmt.Fprintf(c.out, "rota daemon stopped; %s ended\n", sessionsN(len(ids)))
	return nil
}

// stopHolders is stop --all with no daemon: each running holder is hung up,
// and killed holderKillAfter later if its lock is still held then — each
// signal only while its pid is still the holder its record names.
func (c *cli) stopHolders(home, terminals string, yes bool) error {
	live := runningRecords(terminals)
	where := oneLine(home)
	if len(live) == 0 {
		fmt.Fprintf(c.out, "no rota daemon is running for %s, and no session is\n", where)
		return nil
	}
	if !yes && !c.confirm(fmt.Sprintf("no rota daemon is running for %s; end %s? [y/N] ", where, sessionsN(len(live)))) {
		fmt.Fprintln(c.err, "rota: nothing was stopped")
		return exitCode(1)
	}
	why := map[string]error{}
	ids := make([]string, 0, len(live))
	var wg sync.WaitGroup
	for i := range live {
		ids = append(ids, live[i].ID)
		kill, err := holdwire.StopHolder(&live[i], holderKillAfter())
		if err != nil {
			why[live[i].ID] = err
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); kill() }()
	}
	wg.Wait()
	left := waitEnded(terminals, ids, time.Second)
	said := fmt.Sprintf("no rota daemon was running for %s", where)
	if len(left) > 0 {
		return notEnded(said, len(ids), left, why)
	}
	fmt.Fprintf(c.out, "%s; %s ended\n", said, sessionsN(len(ids)))
	return nil
}

/* --------------------------------------------------------------- status --- */

// daemonStatus is `rota serve status`: what daemon.pid says of the daemon,
// and what the daemon says of its sessions, or with no daemon what their
// records say. A daemon that holds the store and does not answer is an
// error, never no daemon.
func (c *cli) daemonStatus() error {
	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	doc := &statusDoc{Store: home, Ctl: holdwire.CtlSock(terminals)}
	if _, ok := holdwire.RunningDaemon(terminals); !ok {
		for _, r := range holdwire.Scan(terminals) {
			switch {
			case r.Locked:
				doc.Sessions.Live++
			case r.State == holdwire.StateEnded || r.State == holdwire.StateInterrupted:
				doc.Sessions.Stopped++
			}
		}
		if err := c.sayStatus(doc); err != nil {
			return err
		}
		return exitCode(3)
	}
	cl, err := rotaclient.Dial(terminals)
	if err == nil {
		err = cl.Health()
	}
	var all []rotaclient.Session
	if err == nil {
		all, err = cl.List()
	}
	if err != nil {
		return err
	}
	d, ok := daemonOf(terminals)
	if !ok {
		return errors.New("the rota daemon stopped while it was being asked about")
	}
	doc.Running, doc.PID, doc.Version = true, d.PID, d.Version
	if !d.Started.IsZero() {
		doc.Started = &d.Started
	}
	if d.HTTP != "" {
		doc.HTTP = &d.HTTP
	}
	for _, s := range all {
		if s.Ended == nil {
			doc.Sessions.Live++
		} else {
			doc.Sessions.Stopped++
		}
	}
	return c.sayStatus(doc)
}
