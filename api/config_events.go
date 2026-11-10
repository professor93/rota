package api

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/professor93/rota/internal/events"
)

// EventsSection is [events]: where the daemon sends what happens to the
// accounts and sessions it holds. A file without it sends nothing.
type EventsSection struct {
	// Link is the address a phone opens this server at, such as
	// https://box.tail1234.ts.net:8787. A pushed message about a session
	// links to that session's page under it. Empty sends no link: the
	// daemon cannot know which of its addresses somebody else reaches.
	Link string `toml:"link"`
	// Sinks is each [[events.sink]], in the order the file has them.
	Sinks []EventSink `toml:"sink"`
}

// EventSink is one [[events.sink]]: one place events go. Type says which
// keys it has: a telegram sink has BotTokenFile and ChatID, a webhook URL and
// SecretFile, and a command Command. On is which events it is sent, as
// types or "<prefix>.*" or "*"; empty is every event.
//
// A sink is called by its type, and the second and later sinks of one type
// by their type and their place among them: telegram, telegram-2, telegram-3.
// Adding a sink never renames one written above it.
type EventSink struct {
	Type         string   `toml:"type"`
	On           []string `toml:"on"`
	BotTokenFile string   `toml:"bot_token_file"`
	ChatID       string   `toml:"chat_id"`
	// URL is secret: a webhook's address often holds its key in the path.
	URL        string   `toml:"url,secret"`
	SecretFile string   `toml:"secret_file"`
	Command    []string `toml:"command"`
}

// The sink types, and the words a message uses for a key that belongs to one.
const (
	sinkTelegram = "telegram"
	sinkWebhook  = "webhook"
	sinkCommand  = "command"
)

// sinkKeys is, for each type, the keys only that type has, in the order the
// file and Print write them, and how that type's own keys are said.
var sinkKeys = map[string]struct {
	keys  []string
	whose string // "a webhook's"
	has   string // "a webhook sink has url and secret_file"
}{
	sinkTelegram: {[]string{"bot_token_file", "chat_id"}, "a telegram sink's", "a telegram sink has bot_token_file and chat_id"},
	sinkWebhook:  {[]string{"url", "secret_file"}, "a webhook's", "a webhook sink has url and secret_file"},
	sinkCommand:  {[]string{"command"}, "a command sink's", "a command sink has command"},
}

// sinkTypes is the order the types are named in a message.
var sinkTypes = []string{sinkTelegram, sinkWebhook, sinkCommand}

// field is the value of the type-specific key k: a string, or for command a
// list.
func (s EventSink) field(k string) (string, []string) {
	switch k {
	case "bot_token_file":
		return s.BotTokenFile, nil
	case "chat_id":
		return s.ChatID, nil
	case "url":
		return s.URL, nil
	case "secret_file":
		return s.SecretFile, nil
	}
	return "", s.Command
}

// set reports whether the sink has the type-specific key k.
func (s EventSink) set(k string) bool {
	v, l := s.field(k)
	return v != "" || len(l) > 0
}

// checkEvents says what is wrong with [events]. A sink is refused here when
// rota could not run it safely, so a mistake shows while somebody is looking
// at the start rather than as a message that never arrives. What is only
// known once the sink runs — a token file nobody else may read, a program on
// PATH, a network — is the sink's to report, and never stops the daemon.
func (c *Config) checkEvents() error {
	if l := c.Events.Link; l != "" && !pageAddress(l) {
		return errors.New("events.link must be an http or https address, as a phone opens it")
	}
	for i, s := range c.Events.Sinks {
		if err := s.check(); err != nil {
			return fmt.Errorf("%s: %w", entryName("events.sink", i, ""), err)
		}
	}
	return nil
}

// pageAddress reports whether link is an address a browser opens a page at.
func pageAddress(link string) bool {
	u, err := url.Parse(link)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") &&
		u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// check says what is wrong with one sink. No message repeats the url: it is a
// secret, and a refusal is written to the daemon's log.
func (s EventSink) check() error {
	own, ok := sinkKeys[s.Type]
	if !ok {
		words := `write "telegram", "webhook" or "command"`
		if s.Type == "" {
			return errors.New("type is empty; " + words)
		}
		return fmt.Errorf("type %q is not a sink; %s", s.Type, words)
	}
	for _, t := range sinkTypes {
		if t == s.Type {
			continue
		}
		for _, k := range sinkKeys[t].keys {
			if s.set(k) {
				return fmt.Errorf("%s is %s; %s", k, sinkKeys[t].whose, own.has)
			}
		}
	}
	if err := events.CheckPatterns(s.On); err != nil {
		return err
	}
	switch s.Type {
	case sinkTelegram:
		if s.BotTokenFile == "" {
			return errors.New("bot_token_file is empty; a telegram sink reads its bot's token from a file")
		}
		if !filepath.IsAbs(s.BotTokenFile) {
			return errors.New("bot_token_file must be an absolute path")
		}
		if strings.TrimSpace(s.ChatID) == "" {
			return errors.New("chat_id is empty; a telegram sink sends to one chat")
		}
	case sinkWebhook:
		if err := webhookURL(s.URL); err != nil {
			return err
		}
		if s.SecretFile != "" && !filepath.IsAbs(s.SecretFile) {
			return errors.New("secret_file must be an absolute path")
		}
	case sinkCommand:
		if len(s.Command) == 0 || s.Command[0] == "" {
			return errors.New("command is empty; give the program and its arguments")
		}
		// A relative path would be found from wherever the daemon was
		// started, which is not a thing this file can promise.
		if p := s.Command[0]; !filepath.IsAbs(p) && strings.ContainsAny(p, `/\`) {
			return errors.New("command's program must be a name on PATH or an absolute path")
		}
	}
	return nil
}

// webhookURL says what is wrong with a webhook's address, without repeating
// it: url.Parse's own error quotes the whole of it.
//
// It goes over https anywhere, or over http to this machine only. Events
// carry account labels and folder names, which a signature does not hide.
// The sink checks the address it dials again, so a name that resolves
// elsewhere sends nothing in clear text.
func webhookURL(raw string) error {
	if raw == "" {
		return errors.New("url is empty; a webhook sink posts to one address")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url is not an address")
	}
	if u.User != nil {
		return errors.New("url holds a user or password; a webhook's key goes in secret_file")
	}
	clear := errors.New("a webhook goes over https, or over http only to this machine (127.0.0.1, ::1, localhost)")
	if u.Scheme != "https" && u.Scheme != "http" {
		return clear
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("url has no host")
	}
	if u.Scheme == "http" && !loopback(net.JoinHostPort(u.Hostname(), "0")) {
		return clear
	}
	return nil
}

// printEvents writes [events] and each [[events.sink]] for Print, with Print's
// own writers. A sink prints its type, its on and the keys of its type; a key
// of another type is printed only if it is set, which only a configuration
// nobody has checked can be.
func (c *Config) printEvents(body *strings.Builder, str func(name, v string), list func(name string, v []string)) {
	body.WriteString("\n[events]\n")
	str("events.link", c.Events.Link)
	for i, s := range c.Events.Sinks {
		at := func(k string) string { return fmt.Sprintf("events.sink.%d.%s", i, k) }
		body.WriteString("\n[[events.sink]]\n")
		str(at("type"), s.Type)
		list(at("on"), s.On)
		for _, t := range sinkTypes {
			for _, k := range sinkKeys[t].keys {
				if t != s.Type && !s.set(k) {
					continue
				}
				if v, l := s.field(k); k == "command" {
					list(at(k), l)
				} else {
					str(at(k), v)
				}
			}
		}
	}
}
