package toml

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// whole is a struct with one field of every shape this package reads, so a
// single decode exercises the lot.
type whole struct {
	Name    string        `toml:"name"`
	Count   int           `toml:"count"`
	On      bool          `toml:"on"`
	Words   []string      `toml:"words"`
	Numbers []int         `toml:"numbers"`
	Wait    time.Duration `toml:"wait"`
	Inner   inner         `toml:"inner"`
	Users   []user        `toml:"users"`
	Skipped string        `toml:"-"`
	Lower   string        // no tag: its own name, lower case
}

type inner struct {
	Deep   deep   `toml:"deep"`
	Listen string `toml:"listen"`
}

type deep struct {
	Yes bool `toml:"yes"`
}

type user struct {
	Name  string   `toml:"name"`
	Roles []string `toml:"roles"`
	Age   int      `toml:"age"`
}

func TestUnmarshalReadsEverythingItPromises(t *testing.T) {
	const src = `# a comment, ignored
name = "a \"quoted\" name\twith\ttabs\nand a line"
count = 1_000
on = true
lower = "untagged"
words = ["one", 'two', # a comment inside an array
  "three",
]
numbers = [1, -2, +3]
wait = "90s"

[inner]
listen = 'C:\not\an\escape'

[inner.deep]
yes = true

[[users]]
name = "ada"
roles = ["admin", "reader"]
age = 36

[[users]]
name = "bob"
`
	var got whole
	keys, err := UnmarshalKeys([]byte(src), &got)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := whole{
		Name: "a \"quoted\" name\twith\ttabs\nand a line", Count: 1000, On: true,
		Lower: "untagged",
		Words: []string{"one", "two", "three"}, Numbers: []int{1, -2, 3},
		Wait:  90 * time.Second,
		Inner: inner{Listen: `C:\not\an\escape`, Deep: deep{Yes: true}},
		Users: []user{{Name: "ada", Roles: []string{"admin", "reader"}, Age: 36}, {Name: "bob"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded\n %#v\nwant\n %#v", got, want)
	}
	// The keys are reported in the order the file wrote them, which is what
	// tells a written-down value from a default.
	wantKeys := []string{"name", "count", "on", "lower", "words", "numbers", "wait",
		"inner.listen", "inner.deep.yes", "users.name", "users.roles", "users.age", "users.name"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("keys %q, want %q", keys, wantKeys)
	}
}

func TestUnmarshalEscapes(t *testing.T) {
	var got struct {
		S string `toml:"s"`
	}
	if err := Unmarshal([]byte(`s = "\u0041\\\"\n\t\r"`), &got); err != nil {
		t.Fatal(err)
	}
	if got.S != "A\\\"\n\t\r" {
		t.Fatalf("escapes decoded to %q", got.S)
	}
}

func TestUnmarshalEmptyArrayIsNotNil(t *testing.T) {
	var got struct {
		Roots []string `toml:"roots"`
	}
	if err := Unmarshal([]byte("roots = []\n"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Roots == nil || len(got.Roots) != 0 {
		t.Fatalf("an empty array decoded to %#v", got.Roots)
	}
}

func TestUnmarshalDurations(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
	}{
		{`"10m"`, 10 * time.Minute},
		{`"1h30m"`, 90 * time.Minute},
		{`"-1s"`, -time.Second},
		{`"0s"`, 0},
	} {
		var got struct {
			D time.Duration `toml:"d"`
		}
		if err := Unmarshal([]byte("d = "+c.in), &got); err != nil {
			t.Fatalf("d = %s: %v", c.in, err)
		}
		if got.D != c.want {
			t.Fatalf("d = %s decoded to %v, want %v", c.in, got.D, c.want)
		}
	}
}

// TestRefusals is the whole point of this package: everything outside the
// subset is turned away by its own name, on the line it was written.
func TestRefusals(t *testing.T) {
	for _, c := range []struct {
		name, src, want string
		line            int
	}{
		{"float", "a = 1\nb = 1.5\n", "floats are not supported", 2},
		{"exponent", "b = 1e6\n", "floats are not supported", 1},
		{"infinity", "b = inf\n", "floats are not supported", 1},
		{"date", "\nb = 1979-05-27\n", "dates and times are not supported", 2},
		{"time", "b = 07:32:00\n", "dates and times are not supported", 1},
		{"datetime", "b = 1979-05-27T07:32:00Z\n", "dates and times are not supported", 1},
		{"hexadecimal", "b = 0xdeadbeef\n", "only decimal integers are supported", 1},
		{"inline table", "b = { x = 1 }\n", "inline tables are not supported", 1},
		{"multi-line string", "b = \"\"\"\nhello\n\"\"\"\n", "multi-line strings are not supported", 1},
		{"multi-line literal", "b = '''x'''\n", "multi-line strings are not supported", 1},
		{"dotted key", "a = 1\nb.c = 2\n", "dotted keys are not supported", 2},
		{"duplicate key", "a = 1\na = 2\n", `"a" is set twice in the top level`, 2},
		{"duplicate key in a table", "[t]\nx = 1\nx = 2\n", `"x" is set twice in [t]`, 3},
		{"duplicate table", "[t]\nx = 1\n[t]\n", "[t] is defined twice", 3},
		{"nested array", "a = [[1], [2]]\n", "arrays inside arrays are not supported", 1},
		{"mixed array", "a = [1,\n \"two\"]\n", "an array holds an integer and a string", 2},
		{"boolean array", "a = [true]\n", "an array holds strings or integers, not booleans", 1},
		{"unclosed string", "a = \"oops\n", "a string is not closed", 1},
		{"unclosed array", "a = [1,\n2\n", "an array is not closed", 1},
		{"unknown escape", `a = "\q"`, `\q is not one of the escapes`, 1},
		{"two keys on a line", "a = 1 b = 2\n", "is not expected here", 1},
		{"underscore at the edge", "a = _1\n", "not an integer", 1},
		{"a table that is a value", "a = 1\n[a]\n", `"a" is a value here, not a table`, 2},
		{"table then array of tables", "[a]\n[[a]]\n", "cannot also be an array of them", 2},
		{"array of tables then table", "[[a]]\n[a]\n", "cannot also be one table", 2},
		{"header not closed", "[a\n", `must end with "]"`, 1},
		{"no value", "a =\n", "a value was expected", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			var into struct {
				A any `toml:"-"`
			}
			err := Unmarshal([]byte(c.src), &into)
			if err == nil {
				t.Fatalf("%q was accepted", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v\nmust say %q", err, c.want)
			}
			if prefix := "line " + strconv.Itoa(c.line) + ":"; !strings.HasPrefix(err.Error(), prefix) {
				t.Fatalf("%v\nmust start with %q", err, prefix)
			}
		})
	}
}

// TestStrictDecoding is the other half: a key the struct does not know is an
// error that says where it is and what would have been right there. A typo
// in a security setting must never read as "leave it off".
func TestStrictDecoding(t *testing.T) {
	var into whole
	err := Unmarshal([]byte("name = \"x\"\n\n[inner]\nlisten = \":1\"\nlisetn = \":2\"\n"), &into)
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	for _, want := range []string{"line 5", `"lisetn"`, "[inner]", "deep, listen"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%v\nmust say %q", err, want)
		}
	}
}

func TestStrictDecodingOfTables(t *testing.T) {
	var into whole
	err := Unmarshal([]byte("[iner]\nlisten = \":1\"\n"), &into)
	if err == nil {
		t.Fatal("an unknown table was accepted")
	}
	for _, want := range []string{"line 1", `"iner"`, "the top level", "users, wait, words"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%v\nmust say %q", err, want)
		}
	}
}

func TestWrongTypes(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`count = "ten"`, "count wants an integer, not a string"},
		{`name = 1`, "name wants a string, not an integer"},
		{`on = "yes"`, "on wants a boolean, not a string"},
		{`words = 1`, "words wants an array of strings, not an integer"},
		{`words = [1]`, "words[0] wants a string, not an integer"},
		{`wait = 10`, "wait wants a length of time, not an integer"},
		{`wait = "soon"`, `"soon" is not a length of time`},
		{"[users]\nname = \"x\"", "users is an array of tables; write [[users]]"},
		{"[[inner]]\nlisten = \"x\"", "inner is one table; write [inner]"},
	} {
		var into whole
		err := Unmarshal([]byte(c.src+"\n"), &into)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%q gave %v, which must say %q", c.src, err, c.want)
		}
	}
}

// TestArrayOfTablesFillsSlicesOfStructs pins what the later phases of the
// server file need — [[auth.users]] with a role each — even though nothing
// in the file has one yet.
func TestArrayOfTablesFillsSlicesOfStructs(t *testing.T) {
	var got struct {
		Auth struct {
			Users []user `toml:"users"`
		} `toml:"auth"`
	}
	src := "[auth]\n\n[[auth.users]]\nname = \"ada\"\nroles = [\"admin\"]\n\n[[auth.users]]\nname = \"bob\"\nroles = []\n"
	if err := Unmarshal([]byte(src), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Auth.Users) != 2 || got.Auth.Users[0].Roles[0] != "admin" || got.Auth.Users[1].Name != "bob" {
		t.Fatalf("decoded %#v", got.Auth.Users)
	}
}

func TestUnmarshalWantsAPointerToAStruct(t *testing.T) {
	var s string
	if err := Unmarshal([]byte("a = 1"), &s); err == nil {
		t.Fatal("a pointer to a string was accepted")
	}
	if err := Unmarshal([]byte("a = 1"), whole{}); err == nil {
		t.Fatal("a struct by value was accepted")
	}
}

func TestQuotedKeysAndComments(t *testing.T) {
	type sub struct {
		X int `toml:"x"`
	}
	var got struct {
		Odd string `toml:"one two"`
		Sub sub    `toml:"a.b"`
	}
	if err := Unmarshal([]byte("\"one two\" = \"yes\" # trailing\n['a.b']\nx = 2\n"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Odd != "yes" || got.Sub.X != 2 {
		t.Fatalf("decoded %#v", got)
	}
}
