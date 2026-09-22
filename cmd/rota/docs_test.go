package main

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A flag that exists and is written down nowhere is a flag nobody will ever
// type. This is what keeps docs/reference.md from falling behind the command
// again: every verb the switch answers to, and every flag this package
// registers or matches by name, has to appear in the manual.
//
// Everything here is read out of the source. A list kept by hand beside the
// code would rot in exactly the way this is meant to catch, and would be one
// more thing to remember when a flag is added.
//
// The api package holds the other half of the same promise: every route the
// server registers has a row in the manual's endpoint table.

// TestReferenceDocumentsEveryCommand: every word the top-level switch and
// serve's own switch answer to.
func TestReferenceDocumentsEveryCommand(t *testing.T) {
	doc := reference(t)
	for _, name := range commands(t) {
		if !strings.Contains(doc, "rota "+name) && !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("`rota %s` is a command and docs/reference.md never says so", name)
		}
	}
}

// TestReferenceDocumentsEveryFlag: every flag registered in a flag set, and
// every one this package lifts out of the arguments by name before a flag
// set ever sees it — --share, --label, --long, --sessions and the rest.
func TestReferenceDocumentsEveryFlag(t *testing.T) {
	doc := reference(t)
	for _, name := range flags(t) {
		spelt := "--" + name
		if len(name) == 1 {
			spelt = "-" + name
		}
		// A word boundary that counts a dash as part of the word, so
		// --session is not satisfied by --sessions and -s is not satisfied
		// by --short.
		if !regexp.MustCompile(`(^|[^-\w])`+regexp.QuoteMeta(spelt)+`($|[^-\w])`).MatchString(doc) {
			t.Errorf("%s is a flag this command takes and docs/reference.md never mentions it", spelt)
		}
	}
}

func reference(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// pkg parses this package's own sources, tests left out: a flag exists
// because the command registers it, not because a test mentions it.
func pkg(t *testing.T) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, p := range pkgs {
		for _, f := range p.Files {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatal("no sources parsed")
	}
	return out
}

// commands reads the two switches that decide what a word means: the one in
// run over the first argument, and the one in serve over its own. A word
// beginning with a dash is a flag rather than a command and is checked as
// one.
func commands(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range pkg(t) {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || (fn.Name.Name != "run" && fn.Name.Name != "serve") {
				continue
			}
			// serve's own switch names subcommands, and a subcommand is only
			// a command with the verb it lives under in front of it.
			prefix := ""
			if fn.Name.Name == "serve" {
				prefix = "serve "
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				sw, ok := n.(*ast.SwitchStmt)
				if !ok || sw.Tag == nil {
					return true
				}
				if tag := render(sw.Tag); tag != "cmd" && tag != "args[0]" {
					return true
				}
				for _, stmt := range sw.Body.List {
					clause, ok := stmt.(*ast.CaseClause)
					if !ok {
						continue
					}
					for _, e := range clause.List {
						if s, ok := literal(e); ok && !strings.HasPrefix(s, "-") {
							seen[prefix+s] = true
						}
					}
				}
				return true
			})
		}
	}
	if len(seen) < 8 {
		t.Fatalf("only %d commands found; the switch they are read from must have moved", len(seen))
	}
	return sorted(seen)
}

// flagLike is how a flag is written when it is matched as a plain string
// rather than registered: "--share", "-i", "-long". A whole sentence that
// happens to contain a flag does not match, which is why the anchors matter.
var flagLike = regexp.MustCompile(`^-{1,2}[A-Za-z][A-Za-z0-9-]*$`)

// flags reads three things: every flag registered on a flag set, every
// flag-shaped string this package compares an argument against, and the
// cases of a switch over an argument with its dashes already trimmed, which
// is how --share and --label are lifted out before run's flag set is built.
func flags(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range pkg(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if name, ok := registeredFlag(n); ok {
					seen[name] = true
				}
			case *ast.BasicLit:
				if s, ok := literal(n); ok && flagLike.MatchString(s) {
					seen[strings.TrimLeft(s, "-")] = true
				}
			case *ast.SwitchStmt:
				if n.Tag == nil || !strings.Contains(render(n.Tag), "TrimLeft") {
					return true
				}
				for _, stmt := range n.Body.List {
					clause, ok := stmt.(*ast.CaseClause)
					if !ok {
						continue
					}
					for _, e := range clause.List {
						if s, ok := literal(e); ok && s != "" {
							seen[s] = true
						}
					}
				}
			}
			return true
		})
	}
	if len(seen) < 30 {
		t.Fatalf("only %d flags found; the way they are registered must have changed", len(seen))
	}
	return sorted(seen)
}

// registeredFlag is the name a flag.FlagSet call declares: String, Bool, Int
// and Duration take it first, the Var forms take it second.
func registeredFlag(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	at := -1
	switch sel.Sel.Name {
	case "String", "Bool", "Int", "Int64", "Uint", "Float64", "Duration", "Func":
		at = 0
	case "StringVar", "BoolVar", "IntVar", "Int64Var", "UintVar", "Float64Var", "DurationVar", "Var", "TextVar":
		at = 1
	default:
		return "", false
	}
	if at >= len(call.Args) {
		return "", false
	}
	s, ok := literal(call.Args[at])
	if !ok || s == "" || strings.ContainsAny(s, " \t") {
		return "", false
	}
	return s, true
}

func literal(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// render writes an expression back out, so a switch can be recognised by
// what it is switching on.
func render(e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, token.NewFileSet(), e); err != nil {
		return ""
	}
	return b.String()
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
