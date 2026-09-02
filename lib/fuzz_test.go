package rota

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// The output parser reads whatever a vendor CLI prints, and a CLI can print
// anything: a half line when it is killed, a debug dump, a file's bytes. The
// targets below feed it arbitrary bytes under arbitrary small caps and hold
// it to what must be true of every Result, valid JSON or not:
//
//   - it never panics and never runs away;
//   - Events never outgrows the events cap, and Truncated says when a cap bit;
//   - what is forwarded to a streaming caller is what the CLI printed;
//   - the Result can be written back out. A reply that cannot be encoded
//     is lost in the transport, which is a run gone for nothing.

// fuzzCaps maps three fuzzed bytes onto output bounds, zero meaning the
// package default, so a small input can hit every cap.
var (
	fuzzEventCaps = []int{0, 1, 2, 3, 5, 8, 100, 20000}
	fuzzLineCaps  = []int{0, 1, 64 << 10, 100000, 1 << 20}
	fuzzBufCaps   = []int{0, 1, 7, 64, 4096, 1 << 20}
)

func fuzzCaps(ev, line, buf uint8) caps {
	return (&Limits{
		MaxEvents:         fuzzEventCaps[int(ev)%len(fuzzEventCaps)],
		MaxEventLine:      fuzzLineCaps[int(line)%len(fuzzLineCaps)],
		MaxBufferedOutput: fuzzBufCaps[int(buf)%len(fuzzBufCaps)],
	}).caps()
}

// parserSeeds are the exact shapes each CLI flavor prints, and the edges a
// parser is most often wrong about.
func parserSeeds() [][]byte {
	twoMB := "{\"type\":\"result\",\"result\":\"" + strings.Repeat("a", 2<<20) + "\"}\n"
	million := "[" + strings.Repeat("{},", 999999) + "{\"type\":\"result\",\"result\":\"R\"}]"
	deepArr := strings.Repeat("[", 10000) + strings.Repeat("]", 10000)
	deepObj := strings.Repeat(`{"a":`, 9999) + `{"type":"result","result":"deep"}` + strings.Repeat("}", 9999)
	// A fat payload at the bottom of a deep nest, in case a level is ever
	// decoded more than once.
	deepFat := strings.Repeat("[", 9990) + "[" + strings.Repeat("1,", 50000) + "1]" + strings.Repeat("]", 9990)
	seeds := []string{
		// claude, streaming
		"{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s1\"}\n" +
			"{\"type\":\"assistant\",\"message\":{\"type\":\"message\"}}\n" +
			"{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"ANSWER\",\"structured_output\":{},\"is_error\":false,\"session_id\":\"s1\",\"num_turns\":3,\"total_cost_usd\":0.01,\"usage\":{}}\n",
		// claude, buffered, indented
		"[{\"type\":\"system\",\"subtype\":\"init\"},\n {\"type\":\"result\",\"result\":\"ANSWER\",\"session_id\":\"s1\",\"total_cost_usd\":0.5}]",
		// codex
		"{\"type\":\"thread.started\",\"thread_id\":\"t-1\"}\n" +
			"{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"CODEX\"}}\n" +
			"{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":5}}\n" +
			"{\"type\":\"turn.failed\"}\n",
		// grok, buffered
		"{\n  \"text\": \"SHAPE\",\n  \"stopReason\": \"end_turn\",\n  \"sessionId\": \"01a00000-0000-7000-8000-000000000001\",\n  \"usage\": {\"input_tokens\": 14221},\n  \"num_turns\": 1,\n  \"total_cost_usd\": 0.030798\n}\n",
		// kimi, prose
		"just words\nand more\n",
		// edges
		"", "\n", "   \n\t\n", "[", "{", "]", "}", "[]", "{}", "null", "[[", "[[]]", "[[[]],[]]",
		"[[{\"type\":\"result\",\"result\":\"X\"}]]",
		"{\"type\":\"result\"",
		"{\"type\":\"result\",\"result\":\"unterminated\n",
		"{\"type\":\"result\",\"result\":\"ok\"}\r\n{\"type\":\"system\"}\r\n",
		"\xef\xbb\xbf{\"type\":\"result\",\"result\":\"bom\"}\n",
		"{\"type\":\"result\",\"result\":\"\xff\xfe\"}\n",
		"caf\xe9\n",
		"{\"type\":\"result\",\"result\":\"a\x00b\"}\n",
		"a\x00b\n\x00\n",
		"{\"type\":\"result\",\"usage\":{\"k\":\"\xff\"},\"structured_output\":{\"a\":1,\"a\":2}}\n",
		"{\"a\":1,\"a\":2,\"type\":\"result\",\"result\":\"dup\"}\n",
		"{not json\n{\"type\":\"result\",\"result\":\"after\"}\n",
		"{\"type\":\"error\",\"error\":\"boom\"}\n",
		"{\"type\":\"result\",\"is_error\":true,\"result\":\"\"}\n",
		"{\"text\":\"typeless\",\"stopReason\":\"error\"}\n",
		"{\"type\":\"item.completed\",\"item\":{\"type\":\"reasoning\",\"text\":\"no\"}}\n",
		"[1,2,3]\n[\"a\"]\n[{\"type\":\"result\",\"result\":\"in-array\"}]\n",
		twoMB, million, deepArr, deepObj, deepFat,
	}
	out := make([][]byte, len(seeds))
	for i, s := range seeds {
		out[i] = []byte(s)
	}
	return out
}

// longestLine is the length of the longest '\n'-delimited line, which is
// what the line cap is measured against.
func longestLine(data []byte) int {
	longest := 0
	for _, l := range bytes.Split(data, []byte{'\n'}) {
		longest = max(longest, len(l))
	}
	return longest
}

// checkResult holds a Result to what is true of every one of them.
func checkResult(t *testing.T, res *Result, cp caps, keep bool) {
	t.Helper()
	if len(res.Events) > cp.events {
		t.Fatalf("%d events kept, cap is %d", len(res.Events), cp.events)
	}
	if !keep && len(res.Events) > 0 {
		t.Fatalf("events kept without being asked: %d", len(res.Events))
	}
	if !utf8.ValidString(res.Result) || !utf8.ValidString(res.SessionID) || !utf8.ValidString(res.Subtype) {
		t.Fatalf("a Result string is not UTF-8: %q %q %q", res.Result, res.SessionID, res.Subtype)
	}
	if _, err := Encode(res); err != nil {
		t.Fatalf("the reply cannot be written back out: %v", err)
	}
}

func FuzzReadOutputStreaming(f *testing.F) {
	for _, s := range parserSeeds() {
		f.Add(s, true, uint8(0), uint8(0), uint8(0), true)
		f.Add(s, false, uint8(2), uint8(2), uint8(3), false)
	}
	f.Fuzz(func(t *testing.T, data []byte, keep bool, ev, line, buf uint8, withOut bool) {
		cp := fuzzCaps(ev, line, buf)
		var got bytes.Buffer
		var out io.Writer
		if withOut {
			out = &got
		}
		res := &Result{}
		err := readOutput(bytes.NewReader(data), out, true, keep, cp, res)
		checkResult(t, res, cp, keep)

		// The scanner's own buffer is 64K, so a smaller line cap is that.
		maxTok := max(cp.eventLine, 64<<10)
		if tooLong := longestLine(data) >= maxTok; errors.Is(err, bufio.ErrTooLong) != tooLong {
			t.Fatalf("longest line %d, cap %d, err %v", longestLine(data), maxTok, err)
		}
		if err != nil && !errors.Is(err, bufio.ErrTooLong) {
			t.Fatalf("a bounded reader can only fail on the line cap: %v", err)
		}
		if res.Truncated && !(keep && len(res.Events) == cp.events) {
			t.Fatalf("Truncated with %d of %d events kept (keep=%v)", len(res.Events), cp.events, keep)
		}
		if withOut && !bytes.Contains(data, []byte{'\r'}) {
			// Line by line, each with its newline: what the CLI printed, up
			// to the line that broke the cap.
			want := bytes.Clone(data)
			if len(want) > 0 && want[len(want)-1] != '\n' {
				want = append(want, '\n')
			}
			if !bytes.HasPrefix(want, got.Bytes()) {
				t.Fatalf("forwarded %q for %q", got.Bytes(), data)
			}
			if err == nil && !bytes.Equal(want, got.Bytes()) {
				t.Fatalf("forwarded %d bytes of %d", got.Len(), len(want))
			}
		}
	})
}

func FuzzReadOutputBuffered(f *testing.F) {
	for _, s := range parserSeeds() {
		f.Add(s, true, uint8(0), uint8(0), uint8(0), true)
		f.Add(s, false, uint8(1), uint8(2), uint8(3), false)
		f.Add(s, true, uint8(3), uint8(0), uint8(4), true)
	}
	f.Fuzz(func(t *testing.T, data []byte, keep bool, ev, line, buf uint8, withOut bool) {
		cp := fuzzCaps(ev, line, buf)
		var got bytes.Buffer
		var out io.Writer
		if withOut {
			out = &got
		}
		res := &Result{}
		err := readOutput(bytes.NewReader(data), out, false, keep, cp, res)
		checkResult(t, res, cp, keep)

		raw := data
		if int64(len(raw)) > cp.buffered {
			raw = raw[:cp.buffered]
			if !res.Truncated {
				t.Fatalf("%d bytes read under a %d cap without saying so", len(data), cp.buffered)
			}
		}
		if withOut && !bytes.Equal(got.Bytes(), raw) {
			t.Fatalf("forwarded %d bytes, read %d", got.Len(), len(raw))
		}
		if err != nil {
			// The fallback line scanner is the only thing that can fail.
			maxTok := max(cp.eventLine, 64<<10)
			if !errors.Is(err, bufio.ErrTooLong) || longestLine(raw) < maxTok {
				t.Fatalf("err %v with longest line %d under cap %d", err, longestLine(raw), maxTok)
			}
		}
		if res.Truncated && int64(len(data)) <= cp.buffered && len(res.Events) != cp.events {
			t.Fatalf("Truncated under both caps: %d bytes, %d events", len(data), len(res.Events))
		}
	})
}

func FuzzAbsorb(f *testing.F) {
	for _, s := range parserSeeds() {
		f.Add(s, true, uint8(0))
		f.Add(s, false, uint8(1))
		f.Add(s, true, uint8(3))
	}
	f.Fuzz(func(t *testing.T, doc []byte, keep bool, ev uint8) {
		cp := fuzzCaps(ev, 0, 0)
		// Both callers keep only a document a reply can carry; the contract
		// for anything else is keep=false.
		if keep && !encodable(doc) {
			keep = false
		}
		res := &Result{}
		absorb(json.RawMessage(doc), keep, cp, res)
		checkResult(t, res, cp, keep)

		var arr []json.RawMessage
		isArr := len(doc) > 0 && doc[0] == '[' && decodeLenient(doc, &arr) == nil
		if keep && isArr {
			if want := min(len(arr), cp.events); len(res.Events) != want {
				t.Fatalf("kept %d of %d elements under a cap of %d", len(res.Events), len(arr), cp.events)
			}
			if res.Truncated != (len(arr) > cp.events) {
				t.Fatalf("Truncated=%v for %d elements under a cap of %d", res.Truncated, len(arr), cp.events)
			}
		} else if res.Truncated {
			t.Fatal("only the events cap sets Truncated")
		}
		if len(doc) > 0 {
			// interesting is only ever handed a non-empty line.
			_ = interesting(doc, &Result{})
			_ = interesting(doc, res)
		}
	})
}

func FuzzTailBuffer(f *testing.F) {
	f.Add([]byte("fake-stderr\n"), uint16(0), uint8(0))
	f.Add([]byte(strings.Repeat("日本語", 100)), uint16(4), uint8(6))
	f.Add([]byte("\xff\xfe\x00"), uint16(1), uint8(0))
	f.Add(bytes.Repeat([]byte("x"), 100000), uint16(65535), uint8(255))
	f.Fuzz(func(t *testing.T, data []byte, limit uint16, chunk uint8) {
		b := &tailBuffer{limit: (&Limits{MaxStderr: int(limit)}).caps().stderr}
		// Writes of 1..256 bytes, so a chunk both under and over the limit
		// reaches Write.
		step := 1 + int(chunk)
		for i := 0; i < len(data); i += step {
			p := data[i:min(i+step, len(data))]
			if n, err := b.Write(p); err != nil || n != len(p) {
				t.Fatalf("Write(%d) = %d, %v", len(p), n, err)
			}
		}
		if len(b.buf) > b.limit {
			t.Fatalf("holds %d bytes over a limit of %d", len(b.buf), b.limit)
		}
		if b.dropped+len(b.buf) != len(data) {
			t.Fatalf("dropped %d + kept %d != written %d", b.dropped, len(b.buf), len(data))
		}
		if !bytes.Equal(b.buf, data[len(data)-len(b.buf):]) {
			t.Fatal("what is kept is not the tail")
		}
		s := b.String()
		if !utf8.ValidString(s) {
			t.Fatalf("the tail is not UTF-8: %q", s)
		}
		prefix := ""
		if b.dropped > 0 {
			prefix = fmt.Sprintf("[%d earlier bytes dropped]\n", b.dropped)
			if !strings.HasPrefix(s, prefix) {
				t.Fatalf("dropped %d bytes without saying so: %q", b.dropped, s)
			}
		}
		if utf8.RuneCountInString(s) > b.limit+len(prefix) {
			t.Fatalf("%d runes for a limit of %d", utf8.RuneCountInString(s), b.limit)
		}
		if _, err := Encode(&Result{Stderr: strings.TrimSpace(s)}); err != nil {
			t.Fatalf("the reply cannot carry it: %v", err)
		}
	})
}

// What follows came out of the seed corpus above: each input made a Result
// the reply could not encode, or made one line cost minutes. Pinned here
// so the parser cannot drift back.

// parsed runs the parser under the default caps and insists the reply can
// be written, which is what every transport does with it next.
func parsed(t *testing.T, streaming, keep bool, in string) *Result {
	t.Helper()
	res := &Result{}
	if err := readOutput(strings.NewReader(in), io.Discard, streaming, keep, (*Limits)(nil).caps(), res); err != nil {
		t.Fatal(err)
	}
	if _, err := Encode(res); err != nil {
		t.Fatalf("the reply cannot be written back out: %v", err)
	}
	return res
}

func TestLinesAReplyCannotCarryAsJSONAreKeptAsText(t *testing.T) {
	tooDeep := strings.Repeat("[", jsonDepth-replyDepth+1) + strings.Repeat("]", jsonDepth-replyDepth+1)
	lines := []string{
		"{not json",
		"{\"type\":\"result\",\"result\":\"caf\xe9\"}", // invalid UTF-8
		"{\"a\":1,\"a\":2}",                            // a repeated name
		"{\"type\":\"system\",\"note\":\"a\x00b\"}",    // a raw control byte
		"{\"type\":\"system\",\"result\":\"cut",        // half a line
		tooDeep,
		"{\"type\":\"system\",\"session_id\":\"s1\"}",
	}
	res := parsed(t, true, true, strings.Join(lines, "\n")+"\n")
	if len(res.Events) != len(lines) {
		t.Fatalf("every line is kept: %d of %d", len(res.Events), len(lines))
	}
	for i, ev := range res.Events[:len(lines)-1] {
		var text string
		if decodeLenient(ev, &text) != nil {
			t.Fatalf("line %d is kept as the text it is: %s", i, ev)
		}
		if want := strings.ToValidUTF8(lines[i], "�"); text != want {
			t.Fatalf("line %d: kept %q, printed %q", i, text, want)
		}
	}
	if string(res.Events[len(lines)-1]) != lines[len(lines)-1] {
		t.Fatalf("a sound line is kept as JSON: %s", res.Events[len(lines)-1])
	}
	// A line the reply cannot carry as JSON still says what it says.
	if res.Result != "caf�" || res.SessionID != "s1" {
		t.Fatalf("the outcome of a quoted line is read: %+v", res)
	}
	// The buffered document goes by the same rule.
	res = parsed(t, false, true, tooDeep)
	if len(res.Events) != 1 || res.Events[0][0] != '"' {
		t.Fatalf("a document too deep to carry is kept as text: %d", len(res.Events))
	}
	// And one just shallow enough is JSON.
	deep := strings.Repeat("[", jsonDepth-replyDepth-1) + strings.Repeat("]", jsonDepth-replyDepth-1)
	res = parsed(t, true, true, deep+"\n")
	if len(res.Events) != 1 || res.Events[0][0] != '[' {
		t.Fatalf("a document the reply can carry is kept as JSON: %d", len(res.Events))
	}
}

func TestOutcomeValuesAReplyCannotCarryAreDropped(t *testing.T) {
	res := parsed(t, true, false,
		"{\"type\":\"result\",\"result\":\"x\",\"usage\":{\"k\":\"\xff\"},\"structured_output\":{\"a\":1,\"a\":2}}\n")
	if res.Result != "x" || res.Usage != nil || res.Structured != nil {
		t.Fatalf("the answer stays, the raw values go: %+v", res)
	}
	res = parsed(t, true, false,
		"{\"type\":\"result\",\"result\":\"x\",\"usage\":{\"k\":1},\"structured_output\":{\"a\":1}}\n")
	if string(res.Usage) != `{"k":1}` || string(res.Structured) != `{"a":1}` {
		t.Fatalf("sound values are untouched: %+v", res)
	}
}

func TestProseResultIsUTF8(t *testing.T) {
	res := parsed(t, true, false, "caf\xe9\nok\n")
	if res.Result != "caf�\nok" {
		t.Fatalf("%q", res.Result)
	}
}

func TestStderrTailCutMidRuneIsUTF8(t *testing.T) {
	b := &tailBuffer{limit: 4}
	b.Write([]byte("日本語")) // nine bytes; the last four start inside 本
	got := b.String()
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "�語") {
		t.Fatalf("%q", got)
	}
	if _, err := Encode(&Result{Stderr: got}); err != nil {
		t.Fatal(err)
	}
}

func TestNestedArraysAreNotEvents(t *testing.T) {
	// An array's elements are events; an array among them is nobody's.
	// Descending into one decoded it again at every level, which made
	// one 8MB line of ten thousand nested arrays cost minutes.
	if res := parsed(t, false, false, `[[{"type":"result","result":"X"}]]`); res.Result != "" {
		t.Fatalf("a nested array is not read: %q", res.Result)
	}
	if res := parsed(t, false, false, `[{"type":"result","result":"X"}]`); res.Result != "X" {
		t.Fatalf("the array's own elements are: %q", res.Result)
	}
}
