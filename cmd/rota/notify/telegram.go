package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/events"
)

// telegramAPI is where Telegram's Bot API is, a variable so a test can
// stand in for it. It is not a key of server.toml.
var telegramAPI = "https://api.telegram.org"

const (
	// tgMaxText is the most Telegram takes in one message, in UTF-16 units.
	tgMaxText = 4096
	// tgMaxToken is the longest bot token a token file may hold.
	tgMaxToken = 256
	// tgMaxAnswer bounds what is read of an answer.
	tgMaxAnswer = 64 << 10
	// tgMaxSaid bounds Telegram's own description of a failure, as said.
	tgMaxSaid = 200
)

// telegram is a sink that sends each event to one chat as a plain message:
// its sentence, and a link to its session's terminal when [events] link is
// set. It is made once and never changed, so sends need no lock.
type telegram struct {
	token    string
	endpoint string          // <telegramAPI>/bot<token>/sendMessage
	chat     json.RawMessage // a number when chat_id is one, else its string
	link     string          // [events] link, without its closing slash
	maxAfter time.Duration
	client   *http.Client
}

// newTelegram makes a Telegram sink. Its error may name the token file but
// never quotes what is in it (newSender's contract).
func newTelegram(s api.EventSink, e env) (sender, error) {
	token, err := tgReadToken(s.BotTokenFile)
	if err != nil {
		return nil, err
	}
	chat := strings.TrimSpace(s.ChatID)
	if chat == "" {
		return nil, errors.New("chat_id is empty; a telegram sink sends to one chat")
	}
	var id json.RawMessage
	if n, err := strconv.ParseInt(chat, 10, 64); err == nil {
		// As JSON writes the number: "+123" and "0123" are not JSON.
		id = json.RawMessage(strconv.FormatInt(n, 10))
	} else if id, err = json.Marshal(chat); err != nil {
		return nil, errors.New("chat_id cannot be sent")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Through a proxy, an https request is a CONNECT tunnel: the path that
	// holds the token stays inside TLS.
	tr.Proxy = http.ProxyFromEnvironment
	tr.MaxResponseHeaderBytes = tgMaxAnswer
	tr.MaxIdleConnsPerHost = 1
	tr.IdleConnTimeout = 30 * time.Second
	return &telegram{
		token:    token,
		endpoint: strings.TrimRight(telegramAPI, "/") + "/bot" + token + "/sendMessage",
		chat:     id,
		link:     strings.TrimRight(e.link, "/"),
		maxAfter: maxAfter,
		client: &http.Client{
			Transport: tr,
			// A redirect is not followed: the token would go wherever it
			// points. Its 3xx is the answer.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// tgReadToken reads the bot token from path (readSecretFile), and refuses
// what is not one: a token is the bot's number, a colon and its key, at
// most tgMaxToken bytes. Its error names path and never what is in it.
func tgReadToken(path string) (string, error) {
	const key = "bot_token_file"
	token, err := readSecretFile(key, path, "bot token", 4096)
	if err != nil {
		return "", err
	}
	id, rest, _ := strings.Cut(token, ":")
	switch {
	case len(token) > tgMaxToken:
		return "", fmt.Errorf("%s %s holds more than %d bytes; a bot token is shorter", key, path, tgMaxToken)
	case id == "" || rest == "" || strings.ContainsFunc(id, func(r rune) bool { return r < '0' || r > '9' }),
		strings.ContainsFunc(token, func(r rune) bool { return !tgTokenRune(r) }):
		// The token goes into the URL's path as it is: a byte that would
		// change the path, or the URL, is not a token's; nor is a token
		// pasted with the "bot" the URL puts before it.
		return "", fmt.Errorf("%s %s does not hold a bot token: a token is the bot's number, a colon and its key, of letters, digits and \"_-.~\"", key, path)
	}
	return token, nil
}

func tgTokenRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(":_-.~", r)
}

// secrets is the token, and the path segment it is in.
func (t *telegram) secrets() []string { return []string{t.token, "bot" + t.token} }

// tgReply is what of Telegram's answer is read.
type tgReply struct {
	OK          *bool  `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int64 `json:"retry_after"`
	} `json:"parameters"`
}

// send posts e to the chat as one message. A 2xx is sent, unless Telegram
// says ok is false in it. Then, by Telegram's code: 429 is tried again
// after its retry_after, up to maxAfter, and 408 and 5xx after backoff's
// own wait, as a failure to reach Telegram is; any other answer, a
// refusal (400, 401, 403, 404) or a redirect, is dropped. No error says
// the request's URL, which holds the token.
func (t *telegram) send(ctx context.Context, e events.Event) error {
	body, err := json.Marshal(struct {
		ChatID    json.RawMessage `json:"chat_id"`
		Text      string          `json:"text"`
		NoPreview bool            `json:"disable_web_page_preview"`
	}{t.chat, t.text(e), true})
	if err != nil {
		return dropped(fmt.Errorf("telegram: the message cannot be made: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		// Its error quotes the URL: say none of it.
		return dropped(errors.New("telegram: the request cannot be made"))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			err = ue.Err // the same failure, without the URL
		}
		return again(fmt.Errorf("telegram: %w", err), 0)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, tgMaxAnswer))
	var a tgReply
	parsed := json.Unmarshal(raw, &a) == nil
	code := resp.StatusCode
	if code >= 200 && code < 300 {
		if !parsed || a.OK == nil || *a.OK {
			return nil
		}
		code = a.ErrorCode
		if code < 300 || code > 599 {
			code = http.StatusBadRequest
		}
	}
	err = t.failed(code, a, parsed)
	switch {
	case code == http.StatusTooManyRequests:
		secs := a.Parameters.RetryAfter
		if secs <= 0 {
			secs, _ = strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64)
		}
		return again(err, t.after(secs))
	case code == http.StatusRequestTimeout, code >= 500:
		return again(err, 0)
	}
	return dropped(err)
}

// after is a wait of secs seconds, at most maxAfter, compared before it is
// multiplied so that no number Telegram sends overflows it; 0 when secs is
// not above 0.
func (t *telegram) after(secs int64) time.Duration {
	switch {
	case secs <= 0:
		return 0
	case secs >= int64(t.maxAfter/time.Second):
		return t.maxAfter
	}
	return time.Duration(secs) * time.Second
}

// failed says that Telegram answered code: in Telegram's own words when
// the answer was Telegram's JSON, else in the status line's. Nothing else
// of a body is said, since a proxy's page may echo the path. The words are
// rid of the token before they are cut, so that a cut never leaves a part
// of it that no longer matches.
func (t *telegram) failed(code int, a tgReply, parsed bool) error {
	said := http.StatusText(code)
	if d := strings.TrimSpace(a.Description); parsed && d != "" {
		said = events.Clean(redact(d, t.secrets()), tgMaxSaid)
	}
	if said == "" {
		return fmt.Errorf("telegram answered %d", code)
	}
	return fmt.Errorf("telegram answered %d: %s", code, said)
}

// text is e as a message: its sentence, and on a line of its own the link
// to its session's terminal when there is a link and a session. Past
// tgMaxText it is cut on a rune's boundary and ends with "…". Telegram
// counts a message's length in UTF-16 units, so they are what is counted:
// never fewer than its runes.
func (t *telegram) text(e events.Event) string {
	s := events.Sentence(e)
	if t.link != "" && e.Session != "" {
		s += "\n" + t.link + "/terminal#" + e.Session
	}
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	if n <= tgMaxText {
		return s
	}
	n = 0
	for i, r := range s {
		if n+utf16.RuneLen(r) > tgMaxText-1 { // room for "…", one unit
			return s[:i] + "…"
		}
		n += utf16.RuneLen(r)
	}
	return s
}
