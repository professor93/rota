// Package toml reads the part of TOML a configuration file needs and
// refuses the rest by name.
//
// It exists because a configuration file is not worth a dependency, and
// because a file that decides who may reach a server has to be strict: a key
// this package does not recognise is an error that says which line it is on
// and what would have been recognised there, so a typo in a security setting
// cannot be read as "leave it off". The whole of TOML is a large thing to
// implement well; the half of it people actually write in a settings file is
// small, and everything outside that half is turned away by its own name
// rather than silently misread.
//
// What it reads: comments, [table] and [table.sub] headers, [[array.of
// .tables]] headers, bare and quoted keys, basic and literal strings,
// decimal integers with _ separators, booleans, and arrays of strings or
// integers written on one line or many. What it refuses, saying so: floats,
// dates and times, inline tables, multi-line strings, dotted keys, duplicate
// keys and duplicate tables.
//
// It knows nothing about the program that uses it.
package toml

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Unmarshal parses TOML and fills the struct v points at.
//
// Fields are matched by their `toml:"name"` tag, or by their name in lower
// case when they have none; a field tagged "-" is not matched at all. A
// string, an integer, a bool, a []string, a []int and a time.Duration
// written as "10m" fill from a value; a nested struct fills from a table,
// and a slice of structs from an array of tables. Anything the struct does
// not know is an error.
func Unmarshal(data []byte, v any) error {
	_, err := UnmarshalKeys(data, v)
	return err
}

// UnmarshalKeys is Unmarshal that also reports, in the order the file wrote
// them, the dotted path of every key it set — "runs.timeout" and the like.
// That is how a program tells a value somebody wrote down from a value that
// is merely the default, which is the difference worth printing back.
func UnmarshalKeys(data []byte, v any) ([]string, error) {
	root, err := parse(data)
	if err != nil {
		return nil, err
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return nil, errors.New("toml: the destination must be a pointer to a struct")
	}
	d := &decoder{}
	if err := d.table(root, rv.Elem(), ""); err != nil {
		return nil, err
	}
	return d.keys, nil
}

// decoder carries the one thing a walk of the tree accumulates: which keys
// the file actually set.
type decoder struct{ keys []string }

var durationType = reflect.TypeOf(time.Duration(0))

// table fills one struct from one table, refusing anything in the table the
// struct has no field for.
func (d *decoder) table(t *table, rv reflect.Value, path string) error {
	known := fields(rv.Type())
	for _, name := range t.names {
		f, ok := known[name]
		if !ok {
			return fmt.Errorf("line %d: %q is not something %s has; it knows %s",
				t.lineOf(name), name, t.where(), strings.Join(sortedNames(known), ", "))
		}
		field := rv.FieldByIndex(f.Index)
		sub := join(path, name)
		switch {
		case t.arrs[name] != nil:
			if field.Kind() == reflect.Struct {
				return fmt.Errorf("line %d: %s is one table; write [%s]", t.arrs[name][0].line, sub, sub)
			}
			if field.Kind() != reflect.Slice || field.Type().Elem().Kind() != reflect.Struct {
				return fmt.Errorf("line %d: %s is not an array of tables", t.arrs[name][0].line, sub)
			}
			rows := reflect.MakeSlice(field.Type(), len(t.arrs[name]), len(t.arrs[name]))
			for i, row := range t.arrs[name] {
				if err := d.table(row, rows.Index(i), sub); err != nil {
					return err
				}
			}
			field.Set(rows)
		case t.subs[name] != nil:
			if field.Kind() == reflect.Slice && field.Type().Elem().Kind() == reflect.Struct {
				return fmt.Errorf("line %d: %s is an array of tables; write [[%s]]", t.subs[name].line, sub, sub)
			}
			if field.Kind() != reflect.Struct {
				return fmt.Errorf("line %d: %s is not a table", t.subs[name].line, sub)
			}
			if err := d.table(t.subs[name], field, sub); err != nil {
				return err
			}
		default:
			v := t.vals[name]
			if err := setValue(v, field, sub); err != nil {
				return err
			}
			d.keys = append(d.keys, sub)
		}
	}
	return nil
}

// setValue puts one parsed value in one field, or says what the field wanted
// instead. The message names the dotted key rather than the Go field: the
// person reading it is holding the file, not the source.
func setValue(v value, rv reflect.Value, path string) error {
	mismatch := func() error {
		return fmt.Errorf("line %d: %s wants %s, not %s", v.line, path, wanted(rv.Type()), v.kind)
	}
	if rv.Type() == durationType {
		if v.kind != kindString {
			return mismatch()
		}
		d, err := time.ParseDuration(v.str)
		if err != nil {
			return fmt.Errorf("line %d: %s: %q is not a length of time (try \"10m\")", v.line, path, v.str)
		}
		rv.SetInt(int64(d))
		return nil
	}
	switch rv.Kind() {
	case reflect.String:
		if v.kind != kindString {
			return mismatch()
		}
		rv.SetString(v.str)
	case reflect.Bool:
		if v.kind != kindBool {
			return mismatch()
		}
		rv.SetBool(v.yes)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.kind != kindInt {
			return mismatch()
		}
		if rv.OverflowInt(v.num) {
			return fmt.Errorf("line %d: %s: %d does not fit", v.line, path, v.num)
		}
		rv.SetInt(v.num)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v.kind != kindInt {
			return mismatch()
		}
		if v.num < 0 || rv.OverflowUint(uint64(v.num)) {
			return fmt.Errorf("line %d: %s: %d does not fit", v.line, path, v.num)
		}
		rv.SetUint(uint64(v.num))
	case reflect.Slice:
		if v.kind != kindArray {
			return mismatch()
		}
		out := reflect.MakeSlice(rv.Type(), len(v.arr), len(v.arr))
		for i, e := range v.arr {
			if err := setValue(e, out.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		rv.Set(out)
	default:
		return fmt.Errorf("toml: %s cannot be filled from a file (%s)", path, rv.Kind())
	}
	return nil
}

// wanted says what a field would have taken, in the words the file uses.
func wanted(t reflect.Type) string {
	if t == durationType {
		return "a length of time"
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Slice:
		return "an array of " + strings.TrimPrefix(wanted(t.Elem()), "a ") + "s"
	case reflect.Struct:
		return "a table"
	}
	return "an integer"
}

// fields maps the names a struct answers to onto its fields.
func fields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Tag.Get("toml")
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f
	}
	return out
}

func sortedNames(m map[string]reflect.StructField) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
