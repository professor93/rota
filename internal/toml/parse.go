package toml

import (
	"fmt"
	"strconv"
	"strings"
)

// kind is what a value turned out to be. It is spelled in words because it
// is read back to a person: "runs.timeout wants a length of time, not a
// boolean".
type kind int

const (
	kindString kind = iota
	kindInt
	kindBool
	kindArray
)

func (k kind) String() string {
	switch k {
	case kindString:
		return "a string"
	case kindInt:
		return "an integer"
	case kindBool:
		return "a boolean"
	}
	return "an array"
}

// value is one value read out of a file, with the line it was written on:
// every complaint about it names that line, so the line travels with it.
type value struct {
	kind kind
	line int
	str  string
	num  int64
	yes  bool
	arr  []value
}

// table is one table and everything directly under it. The names are kept in
// the order the file wrote them, so a file with two mistakes in it is told
// about the first one.
type table struct {
	line    int
	path    string // dotted, "" at the top level
	names   []string
	vals    map[string]value
	subs    map[string]*table
	arrs    map[string][]*table
	defined bool // written as a header, rather than implied by a longer one
}

func newTable(line int, path string) *table {
	return &table{line: line, path: path, vals: map[string]value{},
		subs: map[string]*table{}, arrs: map[string][]*table{}}
}

// has reports whether the name is taken, whatever it was taken by.
func (t *table) has(name string) bool {
	if _, ok := t.vals[name]; ok {
		return true
	}
	if _, ok := t.subs[name]; ok {
		return true
	}
	_, ok := t.arrs[name]
	return ok
}

// lineOf is where a name was written, so that an error about it can say.
func (t *table) lineOf(name string) int {
	if v, ok := t.vals[name]; ok {
		return v.line
	}
	if s, ok := t.subs[name]; ok {
		return s.line
	}
	if a := t.arrs[name]; len(a) > 0 {
		return a[0].line
	}
	return t.line
}

// where names this table the way the file does.
func (t *table) where() string {
	if t.path == "" {
		return "the top level"
	}
	return "[" + t.path + "]"
}

type parser struct {
	src  []byte
	pos  int
	line int
	root *table
	cur  *table
}

// parse reads a whole file into tables and values.
func parse(src []byte) (*table, error) {
	p := &parser{src: src, line: 1, root: newTable(1, "")}
	p.root.defined = true
	p.cur = p.root
	for {
		p.space(true)
		if p.eof() {
			return p.root, nil
		}
		var err error
		if p.src[p.pos] == '[' {
			err = p.header()
		} else {
			err = p.keyval()
		}
		if err != nil {
			return nil, err
		}
		if err := p.lineEnd(); err != nil {
			return nil, err
		}
	}
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) errf(format string, a ...any) error { return p.at(p.line, format, a...) }

func (p *parser) at(line int, format string, a ...any) error {
	return fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, a...))
}

// space skips blanks and comments, and newlines too when told to.
func (p *parser) space(newlines bool) {
	for !p.eof() {
		switch c := p.src[p.pos]; {
		case c == ' ' || c == '\t' || c == '\r':
			p.pos++
		case c == '\n':
			if !newlines {
				return
			}
			p.pos++
			p.line++
		case c == '#':
			for !p.eof() && p.src[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

// lineEnd insists that nothing but a comment follows: one thing to a line is
// the rule that makes a line number worth printing.
func (p *parser) lineEnd() error {
	p.space(false)
	if p.eof() {
		return nil
	}
	if p.src[p.pos] == '\n' {
		p.pos++
		p.line++
		return nil
	}
	return p.errf("%q is not expected here; write one key to a line", string(p.src[p.pos]))
}

// header reads a [table] or [[array of tables]] line and moves the parser
// into what it names.
func (p *parser) header() error {
	line := p.line
	p.pos++
	array := false
	if !p.eof() && p.src[p.pos] == '[' {
		array = true
		p.pos++
	}
	path, err := p.keyPath()
	if err != nil {
		return err
	}
	closing := "]"
	if array {
		closing = "]]"
	}
	p.space(false)
	if !strings.HasPrefix(string(p.src[p.pos:]), closing) {
		return p.at(line, "a table header must end with %q", closing)
	}
	p.pos += len(closing)

	t := p.root
	for i, name := range path[:len(path)-1] {
		t, err = p.descend(t, name, line, strings.Join(path[:i+1], "."))
		if err != nil {
			return err
		}
	}
	name, full := path[len(path)-1], strings.Join(path, ".")
	if _, ok := t.vals[name]; ok {
		return p.at(line, "%q is a value here, not a table", name)
	}
	if array {
		if _, ok := t.subs[name]; ok {
			return p.at(line, "[%s] is a table, so [[%s]] cannot also be an array of them", full, full)
		}
		if _, seen := t.arrs[name]; !seen {
			t.names = append(t.names, name)
		}
		row := newTable(line, full)
		row.defined = true
		t.arrs[name] = append(t.arrs[name], row)
		p.cur = row
		return nil
	}
	if _, ok := t.arrs[name]; ok {
		return p.at(line, "[[%s]] is an array of tables, so [%s] cannot also be one table", full, full)
	}
	sub, ok := t.subs[name]
	if ok && sub.defined {
		return p.at(line, "[%s] is defined twice; it was already written on line %d", full, sub.line)
	}
	if !ok {
		sub = newTable(line, full)
		t.subs[name] = sub
		t.names = append(t.names, name)
	}
	sub.defined, sub.line = true, line
	p.cur = sub
	return nil
}

// descend walks one step of a dotted header, making the table on the way if
// the file never wrote it down. An array of tables is descended into at its
// last row, which is what [[a]] followed by [a.b] means.
func (p *parser) descend(t *table, name string, line int, full string) (*table, error) {
	if _, ok := t.vals[name]; ok {
		return nil, p.at(line, "%q is a value here, not a table", name)
	}
	if rows := t.arrs[name]; len(rows) > 0 {
		return rows[len(rows)-1], nil
	}
	if sub, ok := t.subs[name]; ok {
		return sub, nil
	}
	sub := newTable(line, full)
	t.subs[name] = sub
	t.names = append(t.names, name)
	return sub, nil
}

// keyval reads one `key = value` line into the current table.
func (p *parser) keyval() error {
	line := p.line
	k, err := p.key()
	if err != nil {
		return err
	}
	p.space(false)
	if !p.eof() && p.src[p.pos] == '.' {
		return p.at(line, "dotted keys are not supported; write a [table] header instead")
	}
	if p.eof() || p.src[p.pos] != '=' {
		return p.at(line, "%q must be followed by =", k)
	}
	p.pos++
	v, err := p.value()
	if err != nil {
		return err
	}
	if p.cur.has(k) {
		return p.at(line, "%q is set twice in %s", k, p.cur.where())
	}
	p.cur.vals[k] = v
	p.cur.names = append(p.cur.names, k)
	return nil
}

// keyPath reads a dotted name, which is only ever a header's.
func (p *parser) keyPath() ([]string, error) {
	var out []string
	for {
		p.space(false)
		k, err := p.key()
		if err != nil {
			return nil, err
		}
		out = append(out, k)
		p.space(false)
		if p.eof() || p.src[p.pos] != '.' {
			return out, nil
		}
		p.pos++
	}
}

// key reads one name, bare or quoted.
func (p *parser) key() (string, error) {
	if p.eof() {
		return "", p.errf("a key was expected")
	}
	if c := p.src[p.pos]; c == '"' || c == '\'' {
		v, err := p.str()
		if err != nil {
			return "", err
		}
		if v.str == "" {
			return "", p.errf("a key cannot be empty")
		}
		return v.str, nil
	}
	start := p.pos
	for !p.eof() && bareKeyByte(p.src[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return "", p.errf("%q cannot start a key", string(p.src[p.pos]))
	}
	return string(p.src[start:p.pos]), nil
}

func bareKeyByte(c byte) bool {
	return c == '_' || c == '-' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// value reads one value, whatever kind it is.
func (p *parser) value() (value, error) {
	p.space(false)
	if p.eof() || p.src[p.pos] == '\n' {
		return value{}, p.errf("a value was expected")
	}
	switch c := p.src[p.pos]; c {
	case '"', '\'':
		return p.str()
	case '[':
		return p.array()
	case '{':
		return value{}, p.errf("inline tables are not supported; write a [table] header instead")
	}
	return p.bare()
}

// bare reads a value that is not quoted or bracketed: a boolean, an integer,
// or one of the things this parser turns away by name.
func (p *parser) bare() (value, error) {
	line := p.line
	tok := p.token()
	switch tok {
	case "true":
		return value{kind: kindBool, line: line, yes: true}, nil
	case "false":
		return value{kind: kindBool, line: line}, nil
	case "":
		return value{}, p.at(line, "a value was expected")
	case "inf", "+inf", "-inf", "nan", "+nan", "-nan":
		return value{}, p.at(line, "floats are not supported")
	}
	body := tok
	if body[0] == '+' || body[0] == '-' {
		body = body[1:]
	}
	switch {
	case body == "":
		return value{}, p.at(line, "%q is not a value", tok)
	case strings.HasPrefix(body, "0x"), strings.HasPrefix(body, "0o"), strings.HasPrefix(body, "0b"):
		return value{}, p.at(line, "only decimal integers are supported, so %q is not one", tok)
	case strings.ContainsAny(body, "-:"):
		return value{}, p.at(line, "dates and times are not supported")
	case strings.ContainsAny(body, ".eE"):
		return value{}, p.at(line, "floats are not supported")
	}
	digits, err := plainDigits(body)
	if err != nil {
		return value{}, p.at(line, "%q is not an integer: %v", tok, err)
	}
	if tok[0] == '-' {
		digits = "-" + digits
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return value{}, p.at(line, "%q is not an integer this can hold", tok)
	}
	return value{kind: kindInt, line: line, num: n}, nil
}

// token reads up to whatever ends a bare value.
func (p *parser) token() string {
	start := p.pos
	for !p.eof() {
		switch c := p.src[p.pos]; c {
		case ' ', '\t', '\r', '\n', ',', ']', '#':
			return string(p.src[start:p.pos])
		}
		p.pos++
	}
	return string(p.src[start:p.pos])
}

// plainDigits drops the _ separators an integer may be written with, and
// insists they sit between digits rather than at either end.
func plainDigits(s string) (string, error) {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c == '_' {
			if i == 0 || i == len(s)-1 || s[i-1] == '_' {
				return "", fmt.Errorf("a _ belongs between digits")
			}
			continue
		}
		if c < '0' || c > '9' {
			return "", fmt.Errorf("%q is not a digit", string(c))
		}
		b.WriteByte(c)
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("it has no digits")
	}
	return b.String(), nil
}

// str reads a basic or a literal string. A string runs to the end of its
// line: the multi-line spellings are turned away where they start, so an
// unclosed quote is reported as one rather than swallowing the file.
func (p *parser) str() (value, error) {
	line := p.line
	q := p.src[p.pos]
	if strings.HasPrefix(string(p.src[p.pos:]), strings.Repeat(string(q), 3)) {
		return value{}, p.at(line, "multi-line strings are not supported")
	}
	p.pos++
	if q == '\'' {
		start := p.pos
		for {
			if p.eof() || p.src[p.pos] == '\n' {
				return value{}, p.at(line, "a string is not closed")
			}
			if p.src[p.pos] == '\'' {
				s := string(p.src[start:p.pos])
				p.pos++
				return value{kind: kindString, line: line, str: s}, nil
			}
			p.pos++
		}
	}
	var b strings.Builder
	for {
		if p.eof() || p.src[p.pos] == '\n' {
			return value{}, p.at(line, "a string is not closed")
		}
		c := p.src[p.pos]
		if c == '"' {
			p.pos++
			return value{kind: kindString, line: line, str: b.String()}, nil
		}
		if c != '\\' {
			b.WriteByte(c)
			p.pos++
			continue
		}
		p.pos++
		if p.eof() || p.src[p.pos] == '\n' {
			return value{}, p.at(line, "a string is not closed")
		}
		switch e := p.src[p.pos]; e {
		case '"':
			b.WriteByte('"')
			p.pos++
		case '\\':
			b.WriteByte('\\')
			p.pos++
		case 'n':
			b.WriteByte('\n')
			p.pos++
		case 't':
			b.WriteByte('\t')
			p.pos++
		case 'r':
			b.WriteByte('\r')
			p.pos++
		case 'u':
			p.pos++
			if p.pos+4 > len(p.src) {
				return value{}, p.at(line, `\u needs four hexadecimal digits`)
			}
			hex := string(p.src[p.pos : p.pos+4])
			n, err := strconv.ParseUint(hex, 16, 32)
			if err != nil {
				return value{}, p.at(line, `\u needs four hexadecimal digits, not %q`, hex)
			}
			b.WriteRune(rune(n))
			p.pos += 4
		default:
			return value{}, p.at(line, `\%s is not one of the escapes this reads (\" \\ \n \t \r \uXXXX)`, string(e))
		}
	}
}

// array reads [a, b, c], across as many lines as it takes. It holds strings
// or integers and nothing else: an array of tables is written [[like this]],
// and the rest of TOML's values are not read here anyway.
func (p *parser) array() (value, error) {
	line := p.line
	p.pos++
	out := value{kind: kindArray, line: line}
	for {
		p.space(true)
		if p.eof() {
			return value{}, p.at(line, "an array is not closed")
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return out, nil
		}
		e, err := p.value()
		if err != nil {
			return value{}, err
		}
		switch {
		case e.kind == kindArray:
			return value{}, p.at(e.line, "arrays inside arrays are not supported")
		case e.kind == kindBool:
			return value{}, p.at(e.line, "an array holds strings or integers, not booleans")
		case len(out.arr) > 0 && out.arr[0].kind != e.kind:
			return value{}, p.at(e.line, "an array holds %s and %s; it must hold one or the other",
				out.arr[0].kind, e.kind)
		}
		out.arr = append(out.arr, e)
		p.space(true)
		if p.eof() {
			return value{}, p.at(line, "an array is not closed")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return out, nil
		default:
			return value{}, p.errf("%q is not expected in an array", string(p.src[p.pos]))
		}
	}
}
