package api

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/toml"
)

// TestNoSinkIsTheDefault: a server nobody told about events sends none, and
// says what the section would hold.
func TestNoSinkIsTheDefault(t *testing.T) {
	c := DefaultConfig()
	if !reflect.DeepEqual(c.Events, EventsSection{}) {
		t.Fatalf("the default [events] must be empty: %#v", c.Events)
	}
	var b bytes.Buffer
	if err := c.Print(&b, ""); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"\n[store]\n", "\n[events]\n", `link            = ""`} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output must have %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[[events.sink]]") {
		t.Fatalf("no sink is configured, so none is printed:\n%s", out)
	}
	if strings.Index(out, "[events]") < strings.Index(out, "[store]") {
		t.Fatalf("[events] comes after [store]:\n%s", out)
	}
}

// sinkFile is the three blocks the plan reads, with paths that are absolute
// on whatever machine runs the test.
func sinkFile(t *testing.T) (body, token, secret string) {
	t.Helper()
	dir := t.TempDir()
	token, secret = filepath.Join(dir, "telegram.token"), filepath.Join(dir, "hook.secret")
	body = `[events]
link = "https://box.tail1234.ts.net:8787"

[[events.sink]]
type = "telegram"
on = ["account.*", "session.attention", "session.ended"]
bot_token_file = ` + quote(token) + `
chat_id = "123"

[[events.sink]]
type = "webhook"
url = "https://example.com/hook"
secret_file = ` + quote(secret) + `

[[events.sink]]
type = "command"
command = ["notify-send", "rota"]
`
	return body, token, secret
}

func TestSinksAreReadFromTheFile(t *testing.T) {
	body, token, secret := sinkFile(t)
	c, err := LoadConfig(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	want := EventsSection{
		Link: "https://box.tail1234.ts.net:8787",
		Sink: []EventSink{
			{Type: "telegram", On: []string{"account.*", "session.attention", "session.ended"},
				BotTokenFile: token, ChatID: "123"},
			{Type: "webhook", URL: "https://example.com/hook", SecretFile: secret},
			{Type: "command", Command: []string{"notify-send", "rota"}},
		},
	}
	if !reflect.DeepEqual(c.Events, want) {
		t.Fatalf("read\n%#v\nwant\n%#v", c.Events, want)
	}
	for _, k := range []string{"events.link", "events.sink.type", "events.sink.url", "events.sink.command"} {
		if c.From[k] != "file" {
			t.Fatalf("From[%q] = %q, want file", k, c.From[k])
		}
	}
}

// TestSinkRefusals: every sink rota could not run safely is refused at load,
// naming the entry by its place in the file.
func TestSinkRefusals(t *testing.T) {
	abs := quote(filepath.Join(t.TempDir(), "f"))
	tg := func(extra string) string {
		return "[[events.sink]]\ntype = \"telegram\"\nbot_token_file = " + abs + "\nchat_id = \"1\"\n" + extra
	}
	hook := func(url string) string {
		return "[[events.sink]]\ntype = \"webhook\"\nurl = " + quote(url) + "\n"
	}
	cmd := func(argv string) string {
		return "[[events.sink]]\ntype = \"command\"\ncommand = " + argv + "\n"
	}
	for _, tc := range []struct {
		name, file, want string
		never            string // what the message must not repeat
	}{
		{"no type", "[[events.sink]]\nchat_id = \"1\"\n",
			`[[events.sink]] #1: type is empty; write "telegram", "webhook" or "command"`, ""},
		{"unknown type", "[[events.sink]]\ntype = \"slack\"\n",
			`[[events.sink]] #1: type "slack" is not a sink; write "telegram", "webhook" or "command"`, ""},
		{"telegram without chat", "[[events.sink]]\ntype = \"telegram\"\nbot_token_file = " + abs + "\n",
			"[[events.sink]] #1: chat_id is empty; a telegram sink sends to one chat", ""},
		{"telegram without token", "[[events.sink]]\ntype = \"telegram\"\nchat_id = \"1\"\n",
			"[[events.sink]] #1: bot_token_file is empty; a telegram sink reads its bot's token from a file", ""},
		{"telegram relative token", "[[events.sink]]\ntype = \"telegram\"\nbot_token_file = \"t.token\"\nchat_id = \"1\"\n",
			"[[events.sink]] #1: bot_token_file must be an absolute path", ""},
		{"telegram with url", tg("url = \"https://example.com/x\"\n"),
			"[[events.sink]] #1: url is a webhook's; a telegram sink has bot_token_file and chat_id", "example.com"},
		{"telegram with command", tg("command = [\"x\"]\n"),
			"[[events.sink]] #1: command is a command sink's; a telegram sink has bot_token_file and chat_id", ""},
		{"telegram with secret", tg("secret_file = " + abs + "\n"),
			"[[events.sink]] #1: secret_file is a webhook's; a telegram sink has bot_token_file and chat_id", ""},
		{"webhook with chat", hook("https://example.com/x") + "chat_id = \"1\"\n",
			"[[events.sink]] #1: chat_id is a telegram sink's; a webhook sink has url and secret_file", ""},
		{"command with url", cmd(`["x"]`) + "url = \"https://example.com/x\"\n",
			"[[events.sink]] #1: url is a webhook's; a command sink has command", "example.com"},
		{"webhook relative secret", hook("https://example.com/x") + "secret_file = \"hook.secret\"\n",
			"[[events.sink]] #1: secret_file must be an absolute path", ""},
		{"webhook without url", "[[events.sink]]\ntype = \"webhook\"\n",
			"[[events.sink]] #1: url is empty; a webhook sink posts to one address", ""},
		{"webhook over plain http", hook("http://example.com/services/XYZSECRET"),
			"[[events.sink]] #1: a webhook goes over https, or over http only to this machine (127.0.0.1, ::1, localhost)", "XYZSECRET"},
		{"webhook over ftp", hook("ftp://example.com/XYZSECRET"),
			"[[events.sink]] #1: a webhook goes over https, or over http only to this machine (127.0.0.1, ::1, localhost)", "XYZSECRET"},
		{"webhook without host", hook("https:///XYZSECRET"),
			"[[events.sink]] #1: url has no host", "XYZSECRET"},
		{"webhook that is not an address", hook("https://exa mple.com/services/T0/XYZSECRET"),
			"[[events.sink]] #1: url is not an address", "XYZSECRET"},
		{"webhook with a password", hook("https://me:XYZSECRET@example.com/x"),
			"[[events.sink]] #1: url holds a user or password; a webhook's key goes in secret_file", "XYZSECRET"},
		{"empty command", cmd("[]"),
			"[[events.sink]] #1: command is empty; give the program and its arguments", ""},
		{"empty program", cmd(`["", "x"]`),
			"[[events.sink]] #1: command is empty; give the program and its arguments", ""},
		{"relative command", cmd(`["./x"]`),
			"[[events.sink]] #1: command's program must be a name on PATH or an absolute path", ""},
		{"relative command in a folder", cmd(`["bin/x"]`),
			"[[events.sink]] #1: command's program must be a name on PATH or an absolute path", ""},
		{"bad on", tg("on = [\"account.nope\"]\n"),
			`[[events.sink]] #1: on: "account.nope" names no event`, ""},
		{"second entry", cmd(`["x"]`) + "\n" + hook("http://example.com/x"),
			"[[events.sink]] #2: a webhook goes over https", ""},
		{"link that is not an address", "[events]\nlink = \"box:8787\"\n",
			"events.link must be an http or https address, as a phone opens it", ""},
		{"link without host", "[events]\nlink = \"https://\"\n",
			"events.link must be an http or https address, as a phone opens it", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(write(t, tc.file))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v\nwant %q", err, tc.want)
			}
			if tc.never != "" && strings.Contains(err.Error(), tc.never) {
				t.Fatalf("the refusal repeats %q: %v", tc.never, err)
			}
		})
	}
}

// TestSinksThatLoad: the addresses and programs a sink may have.
func TestSinksThatLoad(t *testing.T) {
	for _, file := range []string{
		"[[events.sink]]\ntype = \"webhook\"\nurl = \"http://127.0.0.1:9/hook\"\n",
		"[[events.sink]]\ntype = \"webhook\"\nurl = \"http://localhost/hook\"\n",
		"[[events.sink]]\ntype = \"webhook\"\nurl = \"http://[::1]:9/hook\"\n",
		"[[events.sink]]\ntype = \"webhook\"\nurl = \"https://x/y?z\"\n",
		"[[events.sink]]\ntype = \"command\"\ncommand = [\"notify-send\", \"rota\"]\non = []\n",
		"[[events.sink]]\ntype = \"command\"\ncommand = [" + quote(filepath.Join(t.TempDir(), "x")) + "]\non = [\"*\"]\n",
		"[[events.sink]]\ntype = \"command\"\ncommand = [\"x\"]\n\n[[events.sink]]\ntype = \"command\"\ncommand = [\"y\"]\n",
		"[events]\nlink = \"http://127.0.0.1:8787\"\n",
		"[events]\nlink = \"https://box.example/rota\"\n",
	} {
		if _, err := LoadConfig(write(t, file)); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
}

// TestPrintMasksTheWebhookURL: a webhook's address may hold its key in the
// path, so --print-config says only that there is one.
func TestPrintMasksTheWebhookURL(t *testing.T) {
	token := filepath.Join(t.TempDir(), "telegram.token")
	c := DefaultConfig()
	c.Events.Sink = []EventSink{
		{Type: "telegram", BotTokenFile: token, ChatID: "123", On: []string{"account.*"}},
		{Type: "webhook", URL: "https://hooks.example/services/T0/B0/XYZSECRET"},
	}
	if err := c.Check(); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := c.Print(&b, ""); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "XYZSECRET") || strings.Contains(out, "hooks.example") {
		t.Fatalf("the webhook's address was printed:\n%s", out)
	}
	for _, want := range []string{
		`# A secret shows as "(set)": this is a report of what rota would`,
		"[[events.sink]]",
		`type            = "webhook"`,
		`url             = "(set)"`,
		`bot_token_file  = ` + quote(token),
		`on              = ["account.*"]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output must have %q:\n%s", want, out)
		}
	}
}

// TestPrintedSinksReadBack: sinks with no secret print as a file the parser
// reads back to the same sinks.
func TestPrintedSinksReadBack(t *testing.T) {
	c := DefaultConfig()
	c.Events = EventsSection{
		Link: "https://box.example",
		Sink: []EventSink{
			{Type: "telegram", BotTokenFile: filepath.Join(t.TempDir(), "t"), ChatID: "123", On: []string{"session.*"}},
			{Type: "command", Command: []string{"logger"}, On: []string{}},
			{Type: "command", Command: []string{"notify-send", "rota"}, On: []string{}},
		},
	}
	var b bytes.Buffer
	if err := c.Print(&b, ""); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "report") {
		t.Fatalf("nothing is secret, so this is a file:\n%s", out)
	}
	if strings.Contains(out, "url") || strings.Contains(out, "chat_id         = \"\"") {
		t.Fatalf("a sink prints only the keys of its type:\n%s", out)
	}
	back := DefaultConfig()
	if err := toml.Unmarshal([]byte(out), back); err != nil {
		t.Fatalf("what it printed is not a file it reads: %v\n%s", err, out)
	}
	if err := back.Check(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Events, c.Events) {
		t.Fatalf("read back\n%#v\nwant\n%#v", back.Events, c.Events)
	}
}
