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

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/rotation"
	"github.com/professor93/rota/sessions"
	"github.com/professor93/rota/wire"
)

// request is a Spec plus the one field only a transport knows about: files
// carried with the request.
type request struct {
	rota.Spec
	Files []wire.Upload `json:"files,omitempty"`
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
	if req.TimeoutSeconds < 0 {
		fail(w, http.StatusBadRequest, "timeout_seconds must not be negative")
		return
	}
	// The slot comes before the store: a run waiting its turn must hold
	// nothing another request needs. With the order reversed, one queued
	// run kept the store locked for everyone — every listing, patch and
	// login, and every rota command on the host — until a slot freed.
	if !s.acquire(r.Context()) {
		return
	}
	defer s.release()
	st, ok := s.open(w)
	if !ok {
		return
	}
	defer st.Close()
	a, ok := s.account(w, r, st)
	if !ok {
		return
	}
	if a.Dead {
		fail(w, http.StatusConflict, "account "+strconv.Itoa(a.ID)+" needs re-auth")
		return
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
			defer os.RemoveAll(dir)
		}
		if err != nil {
			s.report(w, r, err)
			return
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
	if max := int(s.opts.Timeout.Seconds()); req.TimeoutSeconds <= 0 || req.TimeoutSeconds > max {
		req.TimeoutSeconds = max
	}
	lim := &rota.Limits{Roots: roots, AllowDangerous: s.opts.AllowDangerous, AllowRawFlags: s.opts.AllowRawFlags}

	// Validate before taking a slot: a bad request should not wait behind
	// real work, and the error should name the field. Checking against the
	// account, not just its provider, catches a model that account's plan
	// does not include.
	if err := req.CheckFor(a, st.Home(a), lim); err != nil {
		s.report(w, r, err)
		return
	}
	if req.Resume != "" && req.Resume != "last" {
		// The conversation may live in a sibling account's home; copy it in
		// so a resume follows the rotation across accounts. Only now that
		// the request is known to be allowed: a refused one must leave the
		// target's home as it was.
		if err := sessions.CopyForResume(st, a, req.Resume); err != nil {
			s.report(w, r, err)
			return
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
	defer func() { _ = run.End() }()

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
	streaming := req.Stream
	watch := newEventWriter(io.Discard, false, a.ID, a.Provider, false)
	watch.quiet = true
	watch.learn = run.Learned
	out := io.Writer(watch)
	if streaming {
		model, effort, _ := rota.Resolved(a, st.Home(a), req.Spec)
		live := s.startStream(w, r, message.Event{
			Type: "init", Account: a.ID, Provider: a.Provider,
			Model: model, Effort: effort, Cwd: req.Cwd, SessionID: req.Resume,
		}, req.IncludeEvents)
		live.learn = run.Learned
		out = live
	}
	// The run ends when the caller goes away, when it times out, or when
	// the server is stopping — whichever comes first.
	ctx, cancel := joinContexts(r.Context(), s.ctx)
	defer cancel()
	res, err := st.Run(ctx, a, req.Spec, lim, out)
	s.log.Info("run finished", "account", a.ID, "provider", a.Provider,
		"stream", streaming, "err", err, "exit", exitOf(res))
	if streaming {
		s.endStream(w, r, res, err)
		return
	}
	switch {
	case err != nil:
		s.report(w, r, err)
	case res.IsError || res.ExitCode != 0:
		writeJSON(w, http.StatusBadGateway, replyFor(res))
	default:
		writeJSON(w, http.StatusOK, replyFor(res))
	}
}

// reply is a finished run on the wire: what the SDK produced, and what rota
// read out of it. The reading lives here rather than in the SDK because it
// is presentation — how an answer is shown is a client's problem, not the
// transport's, and lib has no business knowing what markdown is.
type reply struct {
	*rota.Result
	Blocks []message.Block `json:"blocks,omitzero"`
	// Ask is present when the run ended by asking the user something. It is
	// rota's reading of prose, not a structure the CLI provided — headless
	// CLIs do not have one — so it is worth taking as a hint rather than as
	// a contract. The answer it was read from is right there in result.
	Ask *message.Ask `json:"ask,omitzero"`
}

func replyFor(res *rota.Result) *reply {
	return &reply{Result: res, Blocks: message.Blocks(res.Result), Ask: message.Asked(res.Result)}
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
func (s *Server) startStream(w http.ResponseWriter, r *http.Request, init message.Event, raw bool) *eventWriter {
	ndjson := wantsNDJSON(r)
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
	ev := newEventWriter(w, !ndjson, init.Account, init.Provider, raw)
	_ = ev.stream.Send(init)
	flush(w)
	return ev
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
func (s *Server) endStream(w http.ResponseWriter, r *http.Request, res *rota.Result, err error) {
	ev := newEventWriter(w, !wantsNDJSON(r), 0, "", false)
	end := wire.Ended(res, err)
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

func newEventWriter(w io.Writer, sse bool, account int, provider string, raw bool) *eventWriter {
	e := &eventWriter{w: w, sse: sse}
	e.stream = message.Stream{Account: account, Provider: provider, Raw: raw, Emit: e.send}
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

// statusFor maps a library verdict onto HTTP. It matches the typed kinds
// rota returns, never its wording: an error message is written for a person
// and may be reworded at any time, while these sentinels are the contract.
func statusFor(err error) int {
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
