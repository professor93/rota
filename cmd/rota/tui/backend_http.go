package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/professor93/rota/cmd/rota/client"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/wire"
)

// backendHTTPLimit is the most of one answer that is read: a whole reply,
// or one line of a stream. A listing of every account is a few kilobytes,
// so anything past this is not rota answering.
const backendHTTPLimit = 8 << 20

// backendHTTP is the Backend over rota's HTTP API, at base, through hc.
type backendHTTP struct {
	hc   *http.Client
	base string
}

// NewHTTP is a Backend asking the rota API at base through hc: the
// in-process server's handler locally, the daemon's over SSH. A nil hc is
// http.DefaultClient. hc should set no Timeout, which would cut an Ask that
// is still answering; each call's context is its deadline.
func NewHTTP(hc *http.Client, base string) Backend {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &backendHTTP{hc: hc, base: strings.TrimRight(base, "/")}
}

func (b *backendHTTP) Who(ctx context.Context) (Principal, error) {
	var doc struct {
		Name string `json:"name"`
		Role string `json:"role"`
		Via  string `json:"via"`
	}
	if err := b.call(ctx, http.MethodGet, "/v1/session", nil, &doc); err != nil {
		return Principal{}, err
	}
	return Principal{Name: doc.Name, Role: doc.Role, Via: doc.Via}, nil
}

func (b *backendHTTP) Accounts(ctx context.Context, how Refresh) (Listing, error) {
	path := "/v1/accounts"
	switch how {
	case RefreshStale:
		path += "?refresh=stale"
	case RefreshForce:
		path += "?refresh=1"
	}
	var doc struct {
		Accounts []wire.Account `json:"accounts"`
		Default  int            `json:"default"`
	}
	if err := b.call(ctx, http.MethodGet, path, nil, &doc); err != nil {
		return Listing{}, err
	}
	return Listing{Accounts: doc.Accounts, Default: doc.Default}, nil
}

func (b *backendHTTP) Change(ctx context.Context, id int, c Change) (wire.Account, error) {
	body := map[string]any{}
	if c.Order != nil {
		body["order"] = *c.Order
	}
	if c.Threshold != nil {
		body["threshold"] = *c.Threshold
	}
	if c.Cwd != nil {
		body["cwd"] = *c.Cwd
	}
	if c.ConfigDir != nil {
		body["config_dir"] = *c.ConfigDir
	}
	if c.Sessions != nil {
		body["sessions"] = *c.Sessions
	}
	if c.RemoteControl != nil {
		body["remote_control"] = *c.RemoteControl
	}
	if c.ForgetLong {
		body["long"] = "forget"
	}
	var a wire.Account
	err := b.call(ctx, http.MethodPatch, "/v1/accounts/"+strconv.Itoa(id), body, &a)
	return a, err
}

func (b *backendHTTP) Remove(ctx context.Context, id int) error {
	return b.call(ctx, http.MethodDelete, "/v1/accounts/"+strconv.Itoa(id), nil, nil)
}

func (b *backendHTTP) BeginLogin(ctx context.Context, lb LoginBegin) (backendLogin, error) {
	body := map[string]any{}
	if lb.Provider != "" {
		body["provider"] = lb.Provider
	}
	if lb.Long {
		body["long"] = true
	}
	if lb.Account != 0 {
		body["account"] = lb.Account
	}
	var l backendLogin
	err := b.call(ctx, http.MethodPost, "/v1/login", body, &l)
	return l, err
}

func (b *backendHTTP) FinishLogin(ctx context.Context, id, code string) (LoginEnd, error) {
	body := map[string]any{}
	if code != "" {
		body["code"] = code
	}
	return b.login(ctx, "/v1/login/"+url.PathEscape(id), body, code)
}

func (b *backendHTTP) PasteKey(ctx context.Context, key, label string) (LoginEnd, error) {
	body := map[string]any{"key": key}
	if label != "" {
		body["label"] = label
	}
	return b.login(ctx, "/v1/login", body, key)
}

func (b *backendHTTP) PasteToken(ctx context.Context, account int, token string) (LoginEnd, error) {
	return b.login(ctx, "/v1/login", map[string]any{"account": account, "token": token}, token)
}

// login posts one step of a login that ends in an account, and reads how it
// ended. secret is what the person typed or pasted: it is taken out of a
// refusal's sentence whatever its shape, as a key-shaped value already is.
func (b *backendHTTP) login(ctx context.Context, path string, body map[string]any, secret string) (LoginEnd, error) {
	var doc struct {
		// The id is a login's while it is pending and an account's after.
		ID           any    `json:"id"`
		Provider     string `json:"provider"`
		Email        string `json:"email"`
		Status       string `json:"status"`
		Delegated    bool   `json:"delegated"`
		LoginCommand string `json:"loginCommand"`
		LongUntil    string `json:"long_until"`
		Warning      string `json:"warning"`
		// A pasted setup token carries a note: rota cannot tell which
		// claude.ai account it belongs to. It is said as the warning.
		Note string `json:"note"`
	}
	if err := b.call(ctx, http.MethodPost, path, body, &doc); err != nil {
		var no *client.Refusal
		if secret != "" && errors.As(err, &no) {
			no.Message = strings.ReplaceAll(no.Message, secret, "[not repeated]")
		}
		return LoginEnd{}, err
	}
	if doc.Status == "pending" {
		return LoginEnd{Pending: true}, nil
	}
	end := LoginEnd{Provider: doc.Provider, Email: doc.Email, Status: doc.Status, Delegated: doc.Delegated,
		LoginCommand: doc.LoginCommand, LongUntil: doc.LongUntil, Warning: doc.Warning}
	if end.Warning == "" {
		end.Warning = doc.Note
	}
	if id, ok := doc.ID.(float64); ok {
		end.Account = int(id)
	}
	return end, nil
}

func (b *backendHTTP) Ask(ctx context.Context, q Question, each func(message.Event)) (wire.End, error) {
	path := "/v1/run"
	if q.Account != 0 {
		path = "/v1/accounts/" + strconv.Itoa(q.Account) + "/run"
	}
	body := map[string]any{"prompt": q.Prompt, "stream": true}
	if q.Cwd != "" {
		body["cwd"] = q.Cwd
	}
	if len(q.With) > 0 {
		body["with"] = q.With
	}
	if q.Input {
		body["input"] = true
	}
	resp, err := b.send(ctx, http.MethodPost, path, body, "application/x-ndjson")
	if err != nil {
		return wire.End{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return wire.End{}, b.refused(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), backendHTTPLimit)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var head struct {
			Type     string `json:"type"`
			ExitCode *int   `json:"exit_code"`
		}
		if err := rota.UnmarshalLenient(line, &head); err != nil {
			return wire.End{}, fmt.Errorf("the rota API sent a line that is not an event: %w", err)
		}
		// The stream's last word is rota's own done or error, which always
		// carries the exit code. An error event without one is the CLI's,
		// said along the way.
		if head.Type == "done" || (head.Type == "error" && head.ExitCode != nil) {
			var end wire.End
			if err := rota.UnmarshalLenient(line, &end); err != nil {
				return wire.End{}, fmt.Errorf("the rota API's last event: %w", err)
			}
			return end, nil
		}
		var ev message.Event
		if err := rota.UnmarshalLenient(line, &ev); err != nil {
			return wire.End{}, fmt.Errorf("the rota API sent a line that is not an event: %w", err)
		}
		if each != nil {
			each(ev)
		}
	}
	if err := ctx.Err(); err != nil {
		return wire.End{}, err
	}
	if err := sc.Err(); err != nil {
		return wire.End{}, fmt.Errorf("the answer was cut off: %w", err)
	}
	return wire.End{}, fmt.Errorf("the answer was cut off before the run ended: %w", io.ErrUnexpectedEOF)
}

func (b *backendHTTP) SendRun(ctx context.Context, run, text string) error {
	return b.call(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(run)+"/messages", map[string]any{"text": text}, nil)
}

func (b *backendHTTP) InterruptRun(ctx context.Context, run string) error {
	return b.call(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(run)+"/interrupt", nil, nil)
}

func (b *backendHTTP) CloseRun(ctx context.Context, run string) error {
	return b.call(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(run)+"/close", nil, nil)
}

// call makes one request, with in as its JSON body when it is not nil, and
// decodes the answer into out when out is not nil. An answer that is not a
// success is a *client.Refusal.
func (b *backendHTTP) call(ctx context.Context, method, path string, in, out any) error {
	resp, err := b.send(ctx, method, path, in, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return b.refused(resp)
	}
	raw, err := b.read(resp)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := rota.UnmarshalLenient(raw, out); err != nil {
		return fmt.Errorf("the rota API's answer to %s %s: %w", method, path, err)
	}
	return nil
}

// send starts one request. A request that never got an answer says which,
// without the address, and still is its context's error to errors.Is.
func (b *backendHTTP) send(ctx context.Context, method, path string, in any, accept string) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		raw, err := rota.Encode(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)
	resp, err := b.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("the rota API did not answer %s %s: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	return resp, nil
}

// read is a whole answer, refused past backendHTTPLimit.
func (b *backendHTTP) read(resp *http.Response) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, backendHTTPLimit+1))
	if err != nil {
		return nil, fmt.Errorf("reading the rota API's answer: %w", err)
	}
	if len(raw) > backendHTTPLimit {
		return nil, fmt.Errorf("the rota API's answer is larger than %d MiB", backendHTTPLimit>>20)
	}
	return raw, nil
}

// refused is an answer that is not a success, as the refusal it is: the
// status, and the reason and sentence when rota wrote them, with anything
// shaped like a key taken out. An answer that is not rota's JSON — a proxy's
// page, say — is said by its status alone.
func (b *backendHTTP) refused(resp *http.Response) error {
	no := &client.Refusal{Status: resp.StatusCode}
	if raw, err := b.read(resp); err == nil {
		var doc struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
			Holder string `json:"holder"`
		}
		if rota.UnmarshalLenient(raw, &doc) == nil {
			no.Message, no.Reason, no.Holder = wire.Unsecret(doc.Error), wire.Unsecret(doc.Reason), wire.Unsecret(doc.Holder)
		}
	}
	if no.Message == "" {
		no.Message = fmt.Sprintf("the rota API answered %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return no
}
