package api

import (
	"bytes"
	"context"
	"encoding/base64"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/rotation"
	"github.com/professor93/rota/sessions"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// request is a Spec plus the two fields only a transport knows about: files
// carried with the request, and the readings to add beside the answer.
type request struct {
	rota.Spec
	Files []wire.Upload `json:"files,omitempty"`
	// With names readings of the answer — blocks, ask — each entry a name
	// or a comma list. Nothing is read unless named: the reply is the
	// answer as the CLI gave it.
	With []string `json:"with,omitempty"`
}

// run executes one account's CLI. The account is named in the path and is
// never chosen for the caller: which account to spend is a decision the
// caller owns.
func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	req, err := decode(w, r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// What this request holds until it returns — unless the run stays open,
	// in which case the run takes them over.
	var hold held
	defer hold.release()
	p, err := s.prepare(r, req, &hold)
	if err != nil {
		if errors.Is(err, errGone) {
			return // the caller stopped waiting; there is nobody to answer
		}
		s.report(w, r, err)
		return
	}
	defer p.st.Close()
	st, a, with, tally := p.st, p.a, p.with, p.tally
	if req.Input {
		// A run that stays open is a different shape of answer: it is streamed
		// like any other, but it is also addressable while it runs, and it
		// outlives this request rather than ending with it.
		s.startInput(w, r, p, &hold)
		return
	}

	// The events are read whether or not they are sent. Sending them is what
	// stream asks for; reading them is how the conversation id reaches the
	// entry above.
	//
	// It reaches it while the run is going only for a streamed one. A
	// buffered run's CLI prints a single document when it is finished, so
	// there is nothing to read until there is nothing left to say, and the id
	// arrives as the entry is being taken away. That is the CLIs' shape
	// rather than rota's, and worth reading here anyway: it costs one pass
	// that is already being made, and it is right when it can be.
	watch := newEventWriter(io.Discard, false, a.ID, a.Provider, message.With{}, tally)
	watch.quiet = true
	watch.learn = p.entry.Learned
	out := io.Writer(watch)
	if p.streaming {
		model, effort, _ := rota.Resolved(a, st.Home(a), req.Spec)
		live := s.startStream(w, r, message.Event{
			Type: "init", Account: a.ID, Provider: a.Provider,
			Model: model, Effort: effort, Cwd: req.Cwd, SessionID: req.Resume,
		}, with, tally)
		live.learn = p.entry.Learned
		out = live
	}
	// The run ends when the caller goes away, when it times out, or when
	// the server is stopping — whichever comes first.
	ctx, cancel := joinContexts(r.Context(), s.ctx)
	defer cancel()
	res, err := st.Run(ctx, a, req.Spec, p.lim, out)
	s.log.Info("run finished", "account", a.ID, "provider", a.Provider,
		"stream", p.streaming, "err", err, "exit", exitOf(res))
	// What the readings are read from, beyond the result: the stream's
	// tally, the account, and a fresh quota reading when one was asked for.
	src := message.Sources{Tally: tally, Account: a, Threshold: rotation.Cutoff(a)}
	if with.Quota {
		for _, e := range st.Refresh(ctx, true, a) {
			s.log.Warn("quota reading failed", "account", a.ID, "err", e)
		}
		src.Quota = a.Quota
	}
	if p.streaming {
		s.endStream(w, r, res, err, with, src)
		return
	}
	switch {
	case err != nil:
		s.report(w, r, err)
	case res.IsError || res.ExitCode != 0:
		writeJSON(w, http.StatusBadGateway, message.ReplyFor(res, with, src))
	default:
		writeJSON(w, http.StatusOK, message.ReplyFor(res, with, src))
	}
}

// prepared is a run that has been checked and is ready to start: everything
// needed to spend an account, and nothing about how the answer is carried.
// The two transports that start runs — a POST and a WebSocket's first frame —
// ask for the same preparation and differ only afterwards.
type prepared struct {
	st    *store.Store
	a     *rota.Account
	req   *request
	lim   *rota.Limits
	with  message.With
	tally *message.Tally
	entry *sessions.Run
	// streaming is the caller's own stream field as it arrived, which is not
	// the same question as whether the CLI streams.
	streaming bool
}

// errGone is a caller that stopped waiting while its run was queued. Nothing
// is answered: there is nobody left to answer.
var errGone = errors.New("the caller went away")

// prepare does everything that happens before an account is spent: the
// readings resolved, a slot taken, the store opened, the account chosen, the
// uploads staged and the whole request checked against that account. What it
// takes on the way — the slot, the upload directory, the entry saying the run
// is happening — goes into hold, whose owner lets go of it.
//
// It answers nothing itself. Every refusal is an error the transport renders,
// because the two transports render one in different words.
func (s *Server) prepare(r *http.Request, req *request, hold *held) (*prepared, error) {
	if req.TimeoutSeconds < 0 {
		return nil, refuse(http.StatusBadRequest, "timeout_seconds must not be negative")
	}
	// A reading nobody knows is refused before anything is spent. The
	// readings that ask the CLI for more become request fields here; the
	// fields a caller set directly count as the same asking.
	with, err := message.ParseWith(req.With...)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, err.Error())
	}
	with.Apply(&req.Spec)
	with.Raw = with.Raw || req.IncludeEvents
	// The transport is the caller's stream field as it arrived. Readings
	// made from events ask the CLI to stream even when the reply stays one
	// document: the CLI streams, rota reads, the caller sees a document.
	streaming := req.Stream
	req.Spec.Stream = req.Stream || with.NeedsEvents()
	// The slot comes before the store: a run waiting its turn must hold
	// nothing another request needs. With the order reversed, one queued
	// run kept the store locked for everyone — every listing, patch and
	// login, and every rota command on the host — until a slot freed.
	if !s.acquire(r.Context()) {
		return nil, errGone
	}
	hold.add(s.release)
	st, err := s.openStore()
	if err != nil {
		s.log.Error("opening the store", "err", err)
		return nil, refuse(http.StatusInternalServerError, "the account store could not be opened")
	}
	// A store written before rotation existed is numbered here rather than
	// by the store itself, which has no opinion about queues.
	rotation.Backfill(st)
	ok := false
	defer func() {
		if !ok {
			st.Close()
		}
	}()
	id, err := pathID(r)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, err.Error())
	}
	a, err := rotation.Choose(r.Context(), st, id)
	if err != nil {
		return nil, err
	}
	if a.Dead {
		return nil, refuse(http.StatusConflict, "account "+strconv.Itoa(a.ID)+" needs re-auth")
	}
	// A server session is hermetic by default: no settings sources unless
	// the request names them. The server's policy, not the SDK's — nil means
	// "leave the CLI alone", which is right for a person at a terminal. Only
	// claude models the field, so only claude gets the default.
	if req.SettingSources == nil && rota.Flavor(a.Provider) == "claude" {
		req.SettingSources = []string{}
	}

	// Uploads land in a directory private to this request, handed to the
	// session as an extra readable root and removed when it ends.
	roots := s.opts.Roots
	if len(req.Files) > 0 {
		dir, err := wire.StageUploads(req.Files)
		if dir != "" {
			hold.add(func() { os.RemoveAll(dir) })
		}
		if err != nil {
			return nil, err
		}
		// The upload directory is the server's own, not something the caller
		// named, so it is allowed for this request by construction.
		roots = append(append([]string{}, roots...), dir)
		req.AddDirs = append(req.AddDirs, dir)
		for i, img := range req.Images {
			req.Images[i] = filepath.Join(dir, filepath.Base(img))
		}
	}
	// What the account already knows, before anything is validated: the
	// limits must be checked against the directory that will actually be
	// used, not the empty one the request arrived with.
	req.Spec = req.Spec.For(a)
	if req.Cwd == "" && len(s.opts.Roots) > 0 {
		req.Cwd = s.opts.Roots[0]
	}
	// A run that stays open is capped by InputTimeout instead: the two are
	// asked for different things, and holding a conversation to the one-shot
	// timeout would kill it between messages.
	bound := s.opts.Timeout
	if req.Input {
		bound = s.opts.InputTimeout
	}
	if max := int(bound.Seconds()); req.TimeoutSeconds <= 0 || req.TimeoutSeconds > max {
		req.TimeoutSeconds = max
	}
	lim := &rota.Limits{Roots: roots, AllowDangerous: s.opts.AllowDangerous, AllowRawFlags: s.opts.AllowRawFlags}

	// Validate before taking a slot: a bad request should not wait behind
	// real work, and the error should name the field. Checking against the
	// account, not just its provider, catches a model that account's plan
	// does not include.
	if err := req.CheckFor(a, st.Home(a), lim); err != nil {
		return nil, err
	}
	if req.Resume != "" && req.Resume != "last" {
		// The conversation may live in a sibling account's home; copy it in
		// so a resume follows the rotation across accounts. Only now that
		// the request is known to be allowed: a refused one must leave the
		// target's home as it was.
		if err := sessions.CopyForResume(st, a, req.Resume); err != nil {
			return nil, err
		}
	}

	// Write down whose run this is. A server is where this matters most: it
	// takes several agents at once and, unless each account names a project
	// of its own, they all read the same ~/.claude — so neither the process
	// list nor the transcripts say whose quota is paying. rota knows,
	// because rota started it.
	run, rerr := sessions.RegistryFor(st).Add(sessions.Instance{
		Account: a.ID, Label: a.Label(), Provider: a.Provider,
		Dir: req.Cwd, Session: req.Resume,
	})
	if rerr != nil {
		s.log.Warn("could not record this run", "account", a.ID, "err", rerr)
	}
	hold.add(func() { _ = run.End() })
	ok = true
	return &prepared{
		st: st, a: a, req: req, lim: lim, with: with,
		tally: &message.Tally{}, entry: run, streaming: streaming,
	}, nil
}

// startInput starts a run that stays open and streams it to this request.
func (s *Server) startInput(w http.ResponseWriter, r *http.Request, p *prepared, hold *held) {
	lr, err := s.beginInput(p, hold)
	if err != nil {
		s.report(w, r, err)
		return
	}
	lr.serve(w, r, 0)
}

// beginInput launches a run that stays open and returns it, with nobody
// reading it yet: whoever asked for it attaches to it, over a streamed
// response or over a socket.
//
// Two things are deliberately not the request's here. The session's context is
// the server's, bounded by InputTimeout, never r.Context(): a run that ends
// when its connection does is exactly what this is not. And what the request
// took — the slot, the upload directory, the entry saying the run is happening
// — is handed to the goroutine waiting for the session, because the CLI is
// still running, still holding the account, when the handler returns.
func (s *Server) beginInput(p *prepared, hold *held) (*liveRun, error) {
	st, a, req, with, tally, entry := p.st, p.a, p.req, p.with, p.tally, p.entry
	lr := s.newLiveRun(a, with, tally)
	lr.learn = entry.Learned
	model, effort, _ := rota.Resolved(a, st.Home(a), req.Spec)
	// Said before the CLI starts, so it is first in the stream and carries the
	// id every other endpoint reaches this run by — an id a client needs
	// before it can send anything into the run it has just asked for.
	lr.Send(message.Event{
		Type: "init", Model: model, Effort: effort, Cwd: req.Cwd,
		SessionID: req.Resume, RunID: lr.id,
	})
	ctx, cancel := context.WithTimeout(s.ctx, s.opts.InputTimeout)
	sess, err := st.Start(ctx, a, req.Spec, p.lim, lr)
	if err != nil {
		cancel()
		return nil, err
	}
	lr.sess = sess
	s.addRun(lr)
	handover := hold.take()
	s.log.Info("run started", "account", a.ID, "provider", a.Provider, "run", lr.id, "input", true)

	// Drained to the end before the run is reported over, so every message's
	// fate is in the stream before the event that closes it.
	told := make(chan struct{})
	go func() {
		defer close(told)
		lr.notices()
	}()
	go func() {
		defer cancel()
		res, err := sess.Wait()
		<-told
		// What the readings are read from, beyond the result: the stream's
		// tally, the account, and a fresh quota reading when one was asked for.
		src := message.Sources{Tally: tally, Account: a, Threshold: rotation.Cutoff(a)}
		if with.Quota {
			for _, e := range st.Refresh(s.ctx, true, a) {
				s.log.Warn("quota reading failed", "account", a.ID, "err", e)
			}
			src.Quota = a.Quota
		}
		lr.finish(wire.Ended(res, err, with, src))
		s.log.Info("run finished", "account", a.ID, "provider", a.Provider,
			"run", lr.id, "stream", true, "err", err, "exit", exitOf(res))
		handover()
		// Kept for a while after it ended, so a reader that reattaches late is
		// given the tail and the terminal event rather than a 404 it could not
		// tell from a wrong id.
		time.AfterFunc(keepEnded, func() { s.dropRun(lr.id) })
	}()
	return lr, nil
}

// held is what a run holds beyond the request that asked for it: the slot in
// the concurrency limit, the directory its uploads went to, the entry saying
// this run is happening. An ordinary run lets go of all three when the handler
// returns, which is what defer is for. A run that stays open cannot, so it
// takes them over instead.
type held struct{ fns []func() }

func (h *held) add(f func()) { h.fns = append(h.fns, f) }

// release lets go of everything, last taken first, as the defers it stands in
// for would have.
func (h *held) release() {
	for i := len(h.fns) - 1; i >= 0; i-- {
		h.fns[i]()
	}
	h.fns = nil
}

// take hands everything to a new owner and leaves this one holding nothing,
// so the deferred release becomes a no-op.
func (h *held) take() func() {
	taken := &held{fns: h.fns}
	h.fns = nil
	return taken.release
}

// joinContexts returns a context cancelled when either parent is.
func joinContexts(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	stop := context.AfterFunc(b, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func exitOf(res *rota.Result) int {
	if res == nil {
		return -1
	}
	return res.ExitCode
}

// decode reads either a JSON body or a multipart body whose "request" part
// is that same JSON and whose "files" parts are uploads. Unknown fields are
// rejected: a misspelled option must not be silently ignored on a request
// that costs money.
func decode(w http.ResponseWriter, r *http.Request) (*request, error) {
	req := &request{}
	// Cap the whole body, not just the part that is read into memory: a
	// multipart request otherwise spills to disk without limit.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxMemory); err != nil {
			return nil, rota.Invalid("bad multipart body: %v", err)
		}
		// The part, not r.FormValue, which would also read the query string
		// and put prompts in every access log on the way.
		if v := r.MultipartForm.Value["request"]; len(v) > 0 && v[0] != "" {
			raw := v[0]
			if err := strictJSON(strings.NewReader(raw), req); err != nil {
				return nil, err
			}
		}
		parts := r.MultipartForm.File["files"]
		if len(parts) > maxFiles {
			return nil, rota.Invalid("too many files: %d, the limit is %d", len(parts), maxFiles)
		}
		for _, fh := range parts {
			u, err := readPart(fh)
			if err != nil {
				return nil, err
			}
			req.Files = append(req.Files, u)
		}
		return req, nil
	}
	if err := strictJSON(r.Body, req); err != nil {
		return nil, err
	}
	// net/http only starts watching for a disconnect once the body has been
	// read to EOF, and a JSON decoder stops at the closing brace. Without
	// this the request context is never cancelled when the caller hangs up,
	// and an agent keeps running — and spending — for nobody.
	_, _ = io.Copy(io.Discard, r.Body)
	return req, nil
}

// decodeJSON reads a small optional JSON body, ignoring an empty one.
func decodeJSON(r *http.Request, v any) error {
	return rota.DecodeLenient(io.LimitReader(r.Body, maxMemory), v)
}

// decodeOptional reads a body that may be absent: nothing at all means
// nothing to set, anything else must decode.
func decodeOptional(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxMemory))
	if err != nil {
		return rota.Invalid("bad request body: %v", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := rota.UnmarshalLenient(raw, v); err != nil {
		return rota.Invalid("bad request body: %v", err)
	}
	return nil
}

func strictJSON(r io.Reader, v any) error {
	// Lenient about how a name is spelled, strict about names nobody knows:
	// a misspelled option that is silently ignored is how a caller ends up
	// believing a permission gate is on when it is not.
	err := jsonv2.UnmarshalRead(r, v, rota.LenientOptions(), jsonv2.RejectUnknownMembers(true))
	if err != nil {
		return rota.Invalid("bad request body: %v", err)
	}
	return nil
}

func readPart(fh *multipart.FileHeader) (wire.Upload, error) {
	f, err := fh.Open()
	if err != nil {
		return wire.Upload{}, err
	}
	defer f.Close()
	// One more than the limit, so a file that is too big is refused rather
	// than silently truncated to something the agent would read as whole.
	raw, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if err != nil {
		return wire.Upload{}, err
	}
	if len(raw) > maxUpload {
		return wire.Upload{}, rota.Invalid("%s is larger than the %d MB limit", fh.Filename, maxUpload>>20)
	}
	return wire.Upload{Path: fh.Filename, Content: base64.StdEncoding.EncodeToString(raw)}, nil
}

const (
	// maxRequest bounds a whole request body, uploads included.
	maxRequest = 64 << 20
	// maxMemory is how much of a multipart body is held in memory before
	// the rest spills to a temp file.
	maxMemory = 8 << 20
	// maxUpload bounds one uploaded file, and maxFiles how many may travel
	// with one request: the staging's own limits, met here before a
	// multipart part is read into memory.
	maxUpload = wire.MaxUploadBytes
	maxFiles  = wire.MaxUploads
)

// startStream switches the response to Server-Sent Events, or to NDJSON when
// the caller asked for it, and returns the writer the CLI's events go to.
// startStream opens the reply and says, before anything runs, who is
// answering and with what. A client should not have to wait for the end to
// learn which model it is paying for, and the CLI's own opening event knows
// nothing about the account or the rotation's choice of it.
func (s *Server) startStream(w http.ResponseWriter, r *http.Request, init message.Event, with message.With, tally *message.Tally) *eventWriter {
	ndjson := wantsNDJSON(r)
	streamHeaders(w, ndjson)
	ev := newEventWriter(w, !ndjson, init.Account, init.Provider, with, tally)
	_ = ev.stream.Send(init)
	flush(w)
	return ev
}

// streamHeaders opens a streamed reply in whichever framing was asked for.
// Both of them stream a run, whether it is this request's own or one it has
// reattached to, so both say so in the same words.
func streamHeaders(w http.ResponseWriter, ndjson bool) {
	h := w.Header()
	if ndjson {
		h.Set("Content-Type", "application/x-ndjson")
	} else {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Connection", "keep-alive")
	}
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Accel-Buffering", "no") // a proxy must not sit on the events
	w.WriteHeader(http.StatusOK)
}

func wantsNDJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/x-ndjson")
}

// flush pushes whatever has been written to the client now rather than when
// a buffer happens to fill: a streamed event that arrives late is no better
// than one that never arrives.
func flush(w any) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// endStream closes the stream with a terminal event, so a client always
// learns how the run ended even though the status line was sent first.
func (s *Server) endStream(w http.ResponseWriter, r *http.Request, res *rota.Result, err error, with message.With, src message.Sources) {
	ev := newEventWriter(w, !wantsNDJSON(r), 0, "", message.With{}, nil)
	end := wire.Ended(res, err, with, src)
	raw, _ := rota.Encode(end)
	// Nothing can be done if this last write fails: the client is gone.
	_ = ev.emit(end.Type, raw)
	flush(w)
}

// eventWriter turns each JSON line the CLI prints into rota's own events.
//
// Four CLIs say the same handful of things in four vocabularies, and a
// client reading the stream should have to learn one. A line rota cannot
// place is still sent, as "other": a vendor adding an event type must not
// make one vanish. The provider's own event rides along in raw only when the
// caller asked for it, by the same field that puts events in a buffered
// reply.
type eventWriter struct {
	w   io.Writer
	sse bool
	buf []byte

	// quiet reads the events without sending any of them, which is what a
	// run that did not ask to stream still needs: the conversation id goes
	// past in them either way.
	quiet bool

	// learn is told the conversation this run turned out to be in.
	learn func(string)

	// stream is the part every transport shares: splitting the CLI's output
	// into lines, reading each one, and numbering what comes out. Only the
	// framing below belongs to this transport.
	stream message.Stream
}

func newEventWriter(w io.Writer, sse bool, account int, provider string, with message.With, tally *message.Tally) *eventWriter {
	e := &eventWriter{w: w, sse: sse}
	e.stream = message.Stream{Account: account, Provider: provider, With: with, Tally: tally, Emit: e.send}
	return e
}

func (e *eventWriter) Write(p []byte) (int, error) { return e.stream.Write(p) }

// send writes one finished event in this transport's framing.
func (e *eventWriter) send(ev message.Event) error {
	if ev.SessionID != "" && e.learn != nil {
		e.learn(ev.SessionID)
	}
	if e.quiet {
		return nil
	}
	raw, err := rota.Encode(ev)
	if err != nil {
		return err
	}
	return e.emit(ev.Type, raw)
}

// emit writes one event. The buffer is reused across a stream, so a long
// run does not allocate once per line.
func (e *eventWriter) emit(name string, data []byte) error {
	e.buf = e.buf[:0]
	if e.sse {
		e.buf = append(e.buf, "event: "...)
		e.buf = append(e.buf, name...)
		e.buf = append(e.buf, "\ndata: "...)
	}
	e.buf = append(e.buf, data...)
	if e.sse {
		e.buf = append(e.buf, '\n')
	}
	e.buf = append(e.buf, '\n')
	_, err := e.w.Write(e.buf)
	flush(e.w)
	return err
}

func (e *eventWriter) Flush() { flush(e.w) }

// refusal is the server's own no, with the status it is said in: the few
// refusals that are this package's rather than one of rota's typed kinds.
// They are errors so that the transports — a JSON response, a socket frame —
// render one refusal in their own words instead of each writing its own.
type refusal struct {
	code int
	msg  string
}

func (e *refusal) Error() string { return e.msg }

func refuse(code int, msg string) error { return &refusal{code: code, msg: msg} }

// statusFor maps a library verdict onto HTTP. It matches the typed kinds
// rota returns, never its wording: an error message is written for a person
// and may be reworded at any time, while these sentinels are the contract.
func statusFor(err error) int {
	var no *refusal
	if errors.As(err, &no) {
		return no.code
	}
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusGatewayTimeout
	case errors.Is(err, rota.ErrDangerous):
		return http.StatusForbidden
	case errors.Is(err, rota.ErrOutsideRoots), errors.Is(err, rota.ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, rota.ErrReauth), errors.Is(err, rota.ErrDeadToken),
		errors.Is(err, rota.ErrBusy), errors.Is(err, rotation.ErrNone):
		// Conflict rather than 503: the account is in a state this request
		// cannot have, and waiting for the server will not change it. Naming
		// another account, or the same one later, will.
		return http.StatusConflict
	case errors.Is(err, rota.ErrNoAccount), errors.Is(err, rota.ErrNoLogin):
		return http.StatusNotFound
	case errors.Is(err, rota.ErrUnsupported):
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}
