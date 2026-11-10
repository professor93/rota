package sshd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/api"
)

// The server's limits: what a peer that has not signed in, one address, one
// key, one connection and every attach together may hold, and what a panic
// or an idle connection gives back.

const limitsBanner = "SSH-2.0-rota\r\n"

// limitsEachPortAPeer counts each connection as from an address of its own,
// for a test that needs more connections than one address may hold.
func limitsEachPortAPeer(t *testing.T) {
	old := sshdPeer
	sshdPeer = func(a net.Addr) string { return a.String() }
	t.Cleanup(func() { sshdPeer = old })
}

// limitsPerson adds a person with a key of their own.
func limitsPerson(t *testing.T, o *Options, name string, role api.Role) ssh.Signer {
	k := sshdKey(t)
	o.Users = append(o.Users, User{Name: name, Role: role, Keys: []string{sshdLine(k.PublicKey())}})
	return k
}

// limitsSilent opens n connections that say nothing, each once it has read
// the server's banner, so each is counted before the next is made.
func limitsSilent(t *testing.T, addr string, n int) []net.Conn {
	t.Helper()
	var silent []net.Conn
	t.Cleanup(func() {
		for _, c := range silent {
			c.Close()
		}
	})
	for i := range n {
		nc, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		silent = append(silent, nc)
		nc.SetDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, len(limitsBanner))
		if _, err := io.ReadFull(nc, b); err != nil || string(b) != limitsBanner {
			t.Fatalf("connection %d: %q %v", i+1, b, err)
		}
	}
	return silent
}

// limitsClosedAtOnce says a new connection is closed before a byte is
// written to it.
func limitsClosedAtOnce(t *testing.T, addr string) {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	start := time.Now()
	nc.SetDeadline(start.Add(3 * time.Second))
	if got, _ := io.ReadAll(nc); len(got) != 0 || time.Since(start) > 500*time.Millisecond {
		t.Errorf("read %q after %v; want closed at once", got, time.Since(start))
	}
}

// limitsEnded says whether the server closes c within the time given.
func limitsEnded(c *ssh.Client, within time.Duration) bool {
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

func TestAHandshakeHasADeadline(t *testing.T) {
	t.Run("silent", func(t *testing.T) {
		sshdKnob(t, &handshakeWait, 200*time.Millisecond)
		r := sshdStart(t)
		nc, err := net.Dial("tcp", r.s.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		start := time.Now()
		nc.SetDeadline(start.Add(3 * time.Second))
		got, err := io.ReadAll(nc)
		if took := time.Since(start); took > time.Second {
			t.Errorf("a silent connection stayed open %v", took)
		}
		if string(got) != limitsBanner {
			t.Errorf("read %q, %v; want the banner and then the end", got, err)
		}
	})
	t.Run("unauthenticated", func(t *testing.T) {
		sshdKnob(t, &handshakeWait, 1500*time.Millisecond)
		// Every connection here comes from 127.0.0.1; counted by its port,
		// each is an address of its own, so the cap over all of them is
		// what is reached.
		limitsEachPortAPeer(t)
		r := sshdStart(t)
		silent := limitsSilent(t, r.s.Addr(), maxUnauthed)
		limitsClosedAtOnce(t, r.s.Addr())
		for _, c := range silent {
			io.Copy(io.Discard, c)
		}
		sshdEventually(t, func() error {
			c, err := r.dial("me", ssh.PublicKeys(r.me))
			if err == nil {
				c.Close()
			}
			return err
		})
	})
}

func TestAtMostSixtyFourConnections(t *testing.T) {
	var keys []ssh.Signer
	r := sshdStart(t, func(o *Options) {
		for i := range maxConns / maxConnsPerUser {
			// Control people: the last places are kept for them.
			keys = append(keys, limitsPerson(t, o, fmt.Sprintf("p%d", i), api.RoleControl))
		}
	})
	var clients []*ssh.Client
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()
	for _, k := range keys {
		for range maxConnsPerUser {
			c, err := r.dial("me", ssh.PublicKeys(k))
			if err != nil {
				t.Fatalf("connection %d: %v", len(clients)+1, err)
			}
			clients = append(clients, c)
		}
	}
	limitsClosedAtOnce(t, r.s.Addr())
	clients[0].Close()
	sshdEventually(t, func() error {
		c, err := r.dial("me", ssh.PublicKeys(r.me))
		if err == nil {
			clients[0] = c
		}
		return err
	})
}

func TestOneKeyHoldsSixteenConnections(t *testing.T) {
	r := sshdStart(t)
	var clients []*ssh.Client
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()
	for range maxConnsPerUser {
		clients = append(clients, r.signIn(r.eye))
	}
	// The next is signed in and closed at once: the key decides who it is,
	// so it is counted only then.
	if c, err := r.dial("eye", ssh.PublicKeys(r.eye)); err == nil {
		if !limitsEnded(c, 2*time.Second) {
			t.Fatalf("connection %d of one key stayed", maxConnsPerUser+1)
		}
	}
	// Somebody else still signs in.
	c := r.signIn(r.me)
	if out, _, code := sshdRun(t, c, "echo in"); code != 0 || out != `["in"]` {
		t.Fatalf("echo: %q %d", out, code)
	}
	for _, c := range clients {
		if limitsEnded(c, 0) {
			t.Fatal("a connection inside the cap was closed")
		}
	}
	// And a place given back is the key's again.
	clients[0].Close()
	sshdEventually(t, func() error {
		c, err := r.dial("eye", ssh.PublicKeys(r.eye))
		if err != nil {
			return err
		}
		if limitsEnded(c, 300*time.Millisecond) {
			return errors.New("closed")
		}
		clients[0] = c
		return nil
	})
	if !strings.Contains(r.log.String(), "too many connections") {
		t.Errorf("the refusal is not in the log:\n%s", r.log)
	}
}

func TestThreeUnansweredKeepalivesClose(t *testing.T) {
	sshdKnob(t, &keepaliveEvery, 50*time.Millisecond)
	r := sshdStart(t)

	// A client that answers stays.
	c := r.signIn(r.me)
	time.Sleep(500 * time.Millisecond)
	if out, _, code := sshdRun(t, c, "echo still"); code != 0 || out != `["still"]` {
		t.Fatalf("an answering client: %q %d", out, code)
	}

	// One that never answers is closed.
	nc, err := net.Dial("tcp", r.s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	cc, _, _, err := ssh.NewClientConn(nc, r.s.Addr(), r.config("me", ssh.PublicKeys(r.me)))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- cc.Wait() }()
	select {
	case <-done:
		if took := time.Since(start); took < 150*time.Millisecond {
			t.Errorf("closed after %v, before three keepalives went unanswered", took)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a client that never answers stayed")
	}
}

func TestOneAddressHoldsEightUnauthenticated(t *testing.T) {
	sshdKnob(t, &handshakeWait, 1500*time.Millisecond)
	// The first maxUnauthedPerAddr+1 connections are one address's; every
	// later one is an address of its own.
	var n atomic.Int32
	old := sshdPeer
	sshdPeer = func(a net.Addr) string {
		if n.Add(1) <= maxUnauthedPerAddr+1 {
			return "one address"
		}
		return a.String()
	}
	t.Cleanup(func() { sshdPeer = old })
	r := sshdStart(t)
	limitsSilent(t, r.s.Addr(), maxUnauthedPerAddr)
	limitsClosedAtOnce(t, r.s.Addr())
	// Somebody else still signs in meanwhile.
	c := r.signIn(r.me)
	if out, _, code := sshdRun(t, c, "echo in"); code != 0 || out != `["in"]` {
		t.Fatalf("echo: %q %d", out, code)
	}
}

func TestAPeerIsItsHost(t *testing.T) {
	peer := func(s string, port int) string {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("not an address: %s", s)
		}
		return sshdPeer(&net.TCPAddr{IP: ip, Port: port})
	}
	for _, same := range [][2]string{
		{"127.0.0.1", "127.0.0.1"},
		{"::ffff:127.0.0.1", "127.0.0.1"},
		// IPv6 by its /64: one host holds a /64 or more.
		{"2001:db8::1", "2001:db8::2"},
		{"2001:db8::1", "2001:db8:0:0:ffff:ffff:ffff:ffff"},
	} {
		if a, b := peer(same[0], 5000), peer(same[1], 5001); a != b {
			t.Errorf("%s and %s are %q and %q; want one peer", same[0], same[1], a, b)
		}
	}
	for _, other := range [][2]string{
		{"127.0.0.1", "127.0.0.2"},
		{"127.0.0.1", "::1"},
		{"2001:db8::1", "2001:db8:0:1::1"},
		{"10.0.0.1", "::ffff:10.0.0.2"},
	} {
		if a, b := peer(other[0], 5000), peer(other[1], 5000); a == b {
			t.Errorf("%s and %s are both %q; want two peers", other[0], other[1], a)
		}
	}
}

func TestAPanicEndsOneConnection(t *testing.T) {
	r := sshdStart(t, func(o *Options) {
		o.Handler = func(p *api.Principal) http.Handler {
			if p.Name == "eye" {
				panic("a handler went wrong")
			}
			return sshdPrincipal(p)
		}
	})
	c := r.signIn(r.me)
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var exit *ssh.ExitError
	if err := sess.Run("panic"); errors.As(err, &exit) || err == nil {
		t.Errorf("a panicking command gave %v; want the connection closed", err)
	}
	if !limitsEnded(c, 2*time.Second) {
		t.Fatal("the connection of a panicking command stayed")
	}
	// A panic while signing a person in ends that connection alone.
	if cl, err := r.dial("eye", ssh.PublicKeys(r.eye)); err == nil && !limitsEnded(cl, 2*time.Second) {
		t.Fatal("the connection of a panicking handler stayed")
	}
	// The server carries on.
	c = r.signIn(r.me)
	if out, _, code := sshdRun(t, c, "echo on"); code != 0 || out != `["on"]` {
		t.Fatalf("echo after a panic: %q %d", out, code)
	}
	sshdEventually(t, func() error {
		log := r.log.String()
		for _, want := range []string{"ssh panic", "a command went wrong", "a handler went wrong", "goroutine"} {
			if !strings.Contains(log, want) {
				return fmt.Errorf("no %q in:\n%s", want, log)
			}
		}
		return nil
	})
}

func TestAPanicSigningInGivesItsPlaceBack(t *testing.T) {
	log := &sshdLog{}
	me := sshdKey(t)
	o := keysOptions(t, sshdLine(me.PublicKey()))
	o.Log = slog.New(slog.NewTextHandler(log, nil))
	s, err := Listen(o)
	if err != nil {
		t.Fatal(err)
	}
	// Before it serves: the callback that accepts me's key now indexes past
	// the people, and panics inside the handshake.
	s.o.Users = nil
	served := make(chan error, 1)
	go func() { served <- s.Serve() }()
	defer func() {
		s.Close()
		<-served
	}()
	cfg := &ssh.ClientConfig{
		User: "me", Auth: []ssh.AuthMethod{ssh.PublicKeys(me)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	}
	for i := range maxUnauthedPerAddr + 1 {
		if c, err := ssh.Dial("tcp", s.Addr(), cfg); err == nil {
			c.Close()
			t.Fatalf("connection %d signed in", i+1)
		}
	}
	sshdEventually(t, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.unauthed != 0 || len(s.peers) != 0 {
			return fmt.Errorf("after the panics: %d unauthenticated, peers %v; want none", s.unauthed, s.peers)
		}
		return nil
	})
	// The address is served still.
	nc, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, len(limitsBanner))
	if _, err := io.ReadFull(nc, b); err != nil || string(b) != limitsBanner {
		t.Errorf("a connection after the panics read %q %v; want the banner", b, err)
	}
	if !strings.Contains(log.String(), "ssh panic") {
		t.Errorf("no ssh panic in:\n%s", log)
	}
}

// limitsSeat takes an attach's place on c, through the rig's seat command,
// and holds it until the session it gives is closed.
func limitsSeat(t *testing.T, r *sshdRig, c *ssh.Client) *ssh.Session {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Start("seat"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("a seat was not taken")
	}
	return s
}

// limitsNoSeat says a seat on c is refused with want.
func limitsNoSeat(t *testing.T, r *sshdRig, c *ssh.Client, want string) {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var errb bytes.Buffer
	s.Stderr = &errb
	if err := s.Start("seat"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		var exit *ssh.ExitError
		if !errors.As(err, &exit) || exit.ExitStatus() != 1 || errb.String() != want+"\n" {
			t.Errorf("seat: %v %q; want 1 %q", err, errb.String(), want)
		}
	case <-r.blocked:
		t.Fatalf("a seat past the limit was taken; want %q", want)
	case <-time.After(3 * time.Second):
		t.Fatalf("seat neither took a place nor was refused")
	}
}

func TestAttachesAreCounted(t *testing.T) {
	var third ssh.Signer
	r := sshdStart(t, func(o *Options) { third = limitsPerson(t, o, "third", api.RoleControl) })
	var held []*ssh.Session
	take := func(k ssh.Signer, conns int) {
		for range conns {
			c := r.signIn(k)
			for range maxAttachesPerConn {
				held = append(held, limitsSeat(t, r, c))
			}
		}
	}

	// One connection holds two.
	first := r.signIn(r.me)
	held = append(held, limitsSeat(t, r, first), limitsSeat(t, r, first))
	limitsNoSeat(t, r, first, "rota over ssh has 2 sessions attached on this connection; leave one first")
	// One person holds eight, over any connections.
	take(r.me, maxAttachesPerUser/maxAttachesPerConn-1)
	limitsNoSeat(t, r, r.signIn(r.me), "rota over ssh has 8 sessions attached as me; leave one first")
	// And the server sixteen.
	take(r.eye, (maxAttaches-maxAttachesPerUser)/maxAttachesPerConn)
	last := r.signIn(third)
	limitsNoSeat(t, r, last, "rota over ssh has 16 sessions attached; leave one first")

	// A place given back is taken again.
	held[0].Close()
	sshdEventually(t, func() error {
		s, err := last.NewSession()
		if err != nil {
			return err
		}
		var errb bytes.Buffer
		s.Stderr = &errb
		if err := s.Start("seat"); err != nil {
			return err
		}
		// errb is read only once Wait has returned, when the session has
		// finished writing it.
		done := make(chan error, 1)
		go func() { done <- s.Wait() }()
		select {
		case <-r.blocked:
			held = append(held, s)
			return nil
		case err := <-done:
			return fmt.Errorf("no seat: %v %q", err, errb.String())
		case <-time.After(3 * time.Second):
			s.Close()
			err := <-done
			return fmt.Errorf("no seat: %v %q", err, errb.String())
		}
	})
}

func TestAnIdleConnectionIsClosed(t *testing.T) {
	sshdKnob(t, &sshdIdleWait, 200*time.Millisecond)
	r := sshdStart(t)
	if !limitsEnded(r.signIn(r.me), 2*time.Second) {
		t.Fatal("a connection with no channel stayed")
	}
	// A channel that never asks for a command is no use of the place.
	c := r.signIn(r.me)
	empty, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if !limitsEnded(c, 2*time.Second) {
		t.Fatal("a connection with an empty channel stayed")
	}
	// A command running is.
	c = r.signIn(r.me)
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("block"); err != nil {
		t.Fatal(err)
	}
	<-r.blocked
	if limitsEnded(c, 600*time.Millisecond) {
		t.Fatal("a connection running a command was closed")
	}
	sess.Close()
	if !limitsEnded(c, 2*time.Second) {
		t.Fatal("a connection whose commands all ended stayed")
	}
}

func TestOnlyModernAlgorithms(t *testing.T) {
	r := sshdStart(t)
	dial := func(edit func(*ssh.ClientConfig)) error {
		cfg := r.config("me", ssh.PublicKeys(r.me))
		edit(cfg)
		c, err := ssh.Dial("tcp", r.s.Addr(), cfg)
		if err == nil {
			c.Close()
		}
		return err
	}
	for _, kex := range []string{ssh.KeyExchangeDH14SHA256, ssh.KeyExchangeDH16SHA512, ssh.KeyExchangeDHGEXSHA256} {
		if err := dial(func(c *ssh.ClientConfig) { c.KeyExchanges = []string{kex} }); err == nil {
			t.Errorf("%s was taken", kex)
		}
	}
	for _, kex := range []string{ssh.KeyExchangeMLKEM768X25519, ssh.KeyExchangeCurve25519, ssh.KeyExchangeECDHP256} {
		if err := dial(func(c *ssh.ClientConfig) { c.KeyExchanges = []string{kex} }); err != nil {
			t.Errorf("%s: %v", kex, err)
		}
	}
	// A MAC is chosen only with a cipher that is not an AEAD.
	for _, mac := range []string{ssh.HMACSHA1, ssh.InsecureHMACSHA196} {
		if err := dial(func(c *ssh.ClientConfig) { c.Ciphers, c.MACs = []string{ssh.CipherAES128CTR}, []string{mac} }); err == nil {
			t.Errorf("%s was taken", mac)
		}
	}
	for _, mac := range []string{ssh.HMACSHA256ETM, ssh.HMACSHA512ETM, ssh.HMACSHA256, ssh.HMACSHA512} {
		if err := dial(func(c *ssh.ClientConfig) { c.Ciphers, c.MACs = []string{ssh.CipherAES128CTR}, []string{mac} }); err != nil {
			t.Errorf("%s: %v", mac, err)
		}
	}
}

func TestARefusalIsLoggedOncePerMinute(t *testing.T) {
	stranger := sshdKey(t)
	refuse := func(r *sshdRig, n int) {
		for range n {
			if c, err := r.dial("me", ssh.PublicKeys(stranger)); err == nil {
				c.Close()
				t.Fatal("a stranger signed in")
			}
		}
	}
	lines := func(r *sshdRig) []string {
		var out []string
		for _, l := range strings.Split(r.log.String(), "\n") {
			if strings.Contains(l, "ssh refused") {
				out = append(out, l)
			}
		}
		return out
	}

	r := sshdStart(t)
	refuse(r, 5)
	sshdEventually(t, func() error {
		if len(lines(r)) == 0 {
			return errors.New("no refusal logged")
		}
		return nil
	})
	time.Sleep(200 * time.Millisecond)
	if got := lines(r); len(got) != 1 {
		t.Errorf("five refusals from one address in a minute logged %d lines:\n%s", len(got), strings.Join(got, "\n"))
	}

	// Once the minute is up, the next line counts those not logged.
	sshdKnob(t, &limitsRefusedEvery, 300*time.Millisecond)
	r = sshdStart(t)
	refuse(r, 3)
	time.Sleep(400 * time.Millisecond)
	refuse(r, 1)
	sshdEventually(t, func() error {
		got := lines(r)
		if len(got) != 2 || !strings.Contains(got[1], "suppressed=2") {
			return fmt.Errorf("want two lines, the second with suppressed=2:\n%s", strings.Join(got, "\n"))
		}
		return nil
	})
}

func TestAFullRefusalTableStillLimits(t *testing.T) {
	sshdKnob(t, &limitsRefusedEvery, 300*time.Millisecond)
	r := sshdStart(t)
	count := func() int { return strings.Count(r.log.String(), "ssh refused") }
	for i := range limitsRefusedKept {
		r.s.limitsRefused(fmt.Sprintf("filler %d", i), "addr", "filler")
	}
	if got := count(); got != limitsRefusedKept {
		t.Fatalf("%d fillers logged %d lines", limitsRefusedKept, got)
	}
	// The table is full and nothing in it is old enough to forget: new
	// addresses are counted together past the table, and logged once a
	// window all the same, and the table does not grow.
	for i := range 50 {
		r.s.limitsRefused(fmt.Sprintf("new %d", i), "addr", "new")
	}
	if got := count() - limitsRefusedKept; got != 1 {
		t.Errorf("50 refusals from new addresses with the table full logged %d lines; want 1", got)
	}
	r.s.mu.Lock()
	kept := len(r.s.refused)
	r.s.mu.Unlock()
	if kept > limitsRefusedKept+1 {
		t.Errorf("the table holds %d addresses; want at most %d", kept, limitsRefusedKept+1)
	}
	time.Sleep(400 * time.Millisecond)
	r.s.limitsRefused("one more", "addr", "one more")
	if !strings.Contains(r.log.String(), "suppressed=49") {
		t.Errorf("the 49 not logged are never counted:\n%s", r.log.String()[len(r.log.String())-400:])
	}
}

func TestControlKeepsItsShare(t *testing.T) {
	// Three watchers at their full share, and a fourth: together they may
	// not take the places kept for control.
	var watchers []ssh.Signer
	r := sshdStart(t, func(o *Options) {
		for i := range 3 {
			watchers = append(watchers, limitsPerson(t, o, fmt.Sprintf("w%d", i), api.RoleWatch))
		}
	})
	conns := map[int][]*ssh.Client{}
	for i, k := range watchers {
		for range maxConnsPerUser {
			conns[i] = append(conns[i], r.signIn(k))
		}
	}
	if c, err := r.dial("eye", ssh.PublicKeys(r.eye)); err == nil && !limitsEnded(c, 2*time.Second) {
		t.Fatalf("a watcher took connection %d, which is kept for control", maxConns-limitsControlConns+1)
	}
	// Attaches: twelve between the watchers, and the last four are kept.
	for _, c := range conns[0][:maxAttachesPerUser/maxAttachesPerConn] {
		limitsSeat(t, r, c)
		limitsSeat(t, r, c)
	}
	for _, c := range conns[1][:(maxAttaches-limitsControlAttaches-maxAttachesPerUser)/maxAttachesPerConn] {
		limitsSeat(t, r, c)
		limitsSeat(t, r, c)
	}
	limitsNoSeat(t, r, conns[2][0], "rota over ssh keeps the last 4 attached sessions for control; leave one first")

	// The owner still has every place kept: sixteen connections and four
	// attaches.
	var mine []*ssh.Client
	for range limitsControlConns {
		mine = append(mine, r.signIn(r.me))
	}
	for _, c := range mine {
		if limitsEnded(c, 0) {
			t.Fatal("the owner's connection was closed")
		}
	}
	for _, c := range mine[:limitsControlAttaches/maxAttachesPerConn] {
		limitsSeat(t, r, c)
		limitsSeat(t, r, c)
	}
	if out, _, code := sshdRun(t, mine[len(mine)-1], "echo mine"); code != 0 || out != `["mine"]` {
		t.Fatalf("echo: %q %d", out, code)
	}
	if !strings.Contains(r.log.String(), "kept for control") {
		t.Errorf("the refused watcher is not in the log:\n%s", r.log)
	}
}

// limitsChan is a session channel with nobody at the other end: reads wait
// until it is closed, and writes go nowhere.
type limitsChan struct {
	once   sync.Once
	closed chan struct{}
}

func (c *limitsChan) Read([]byte) (int, error)                       { <-c.closed; return 0, io.EOF }
func (c *limitsChan) Write(p []byte) (int, error)                    { return len(p), nil }
func (c *limitsChan) Close() error                                   { c.once.Do(func() { close(c.closed) }); return nil }
func (c *limitsChan) CloseWrite() error                              { return nil }
func (c *limitsChan) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (c *limitsChan) Stderr() io.ReadWriter                          { return limitsDiscard{} }

type limitsDiscard struct{}

func (limitsDiscard) Read([]byte) (int, error)    { return 0, io.EOF }
func (limitsDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestAPanicInTheRequestLoopWaitsForTheCommand(t *testing.T) {
	r := sshdStart(t)
	sc := &sshdConn{s: r.s, user: User{Name: "me", Role: api.RoleControl}, idle: time.AfterFunc(time.Hour, func() {})}
	defer sc.idle.Stop()
	ch := &limitsChan{closed: make(chan struct{})}
	reqs := make(chan *ssh.Request, 2)
	type unwound struct {
		panicked, endedFirst bool
	}
	result := make(chan unwound, 1)
	go func() {
		defer func() {
			p := recover()
			select {
			case <-r.ended:
				result <- unwound{p != nil, true}
			default:
				result <- unwound{p != nil, false}
			}
		}()
		channelServe(r.s.ctx, sc, ch, reqs)
	}()
	reqs <- &ssh.Request{Type: "exec", Payload: ssh.Marshal(struct{ C string }{"block"})}
	<-r.blocked
	// A request made by hand has no connection to answer through, so its
	// Reply panics inside the loop.
	reqs <- &ssh.Request{Type: "env", WantReply: true}
	select {
	case got := <-result:
		if !got.panicked || !got.endedFirst {
			t.Errorf("panicked %v, the command ended before the loop unwound %v; want both", got.panicked, got.endedFirst)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channelServe neither returned nor panicked")
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.running != 0 {
		t.Errorf("%d commands still counted running", sc.running)
	}
}
