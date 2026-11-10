package tui

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rulesFile is one Go file of the package, as written and with its
// comments blanked, so a rule about code is not broken by a sentence about
// it.
type rulesFile struct {
	name       string
	src, code  []byte
	test       bool
	lines      int
	parsed     *ast.File
	fset       *token.FileSet
	parseError error
}

func rulesFiles(t *testing.T) []rulesFile {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no Go files here: %v", err)
	}
	var files []rulesFile
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f := rulesFile{name: path, src: src, test: strings.HasSuffix(path, "_test.go"), lines: strings.Count(string(src), "\n")}
		if len(src) > 0 && src[len(src)-1] != '\n' {
			f.lines++
		}
		f.code = rulesUncommented(src)
		f.fset = token.NewFileSet()
		f.parsed, f.parseError = parser.ParseFile(f.fset, path, src, 0)
		files = append(files, f)
	}
	return files
}

// rulesUncommented is src with every comment replaced by spaces, its
// newlines kept, so line numbers still match.
func rulesUncommented(src []byte) []byte {
	code := append([]byte(nil), src...)
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.COMMENT {
			continue
		}
		off := file.Offset(pos)
		for i := off; i < off+len(lit) && i < len(code); i++ {
			if code[i] != '\n' {
				code[i] = ' '
			}
		}
	}
	return code
}

// rulesLine names the line an offset falls on, and shows it, for a failure.
func rulesLine(f rulesFile, off int) string {
	line := strings.Count(string(f.code[:off]), "\n") + 1
	text := strings.Split(string(f.src), "\n")[line-1]
	return fmt.Sprintf("%s:%d %s", f.name, line, strings.TrimSpace(text))
}

func TestNoFileIsPastSixHundredLines(t *testing.T) {
	for _, f := range rulesFiles(t) {
		if !f.test && f.lines > 600 {
			t.Errorf("%s has %d lines; a file of this package has at most 600 (spec section 9)", f.name, f.lines)
		}
	}
}

func TestKeysComeOnlyFromBindings(t *testing.T) {
	compare := regexp.MustCompile(`\.String\(\)\s*(==|!=)|switch\s+\w+\.String\(\)`)
	for _, f := range rulesFiles(t) {
		if f.test || f.name == "keys.go" {
			continue
		}
		for _, loc := range compare.FindAllIndex(f.code, -1) {
			t.Errorf("%s compares a key by its name; match a key.Binding instead", rulesLine(f, loc[0]))
		}
		if f.parseError != nil {
			t.Errorf("%s: %v", f.name, f.parseError)
			continue
		}
		// Every binding is made inside a function whose name ends in Keys,
		// so the keys of a view are in one place and its help is drawn
		// from them. The key package is found by its path, under whatever
		// name the file imports it.
		pkg := ""
		for _, imp := range f.parsed.Imports {
			if imp.Path.Value == `"charm.land/bubbles/v2/key"` {
				pkg = "key"
				if imp.Name != nil {
					pkg = imp.Name.Name
				}
			}
		}
		if pkg == "" {
			continue
		}
		ast.Inspect(f.parsed, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.FuncDecl:
				if strings.HasSuffix(d.Name.Name, "Keys") {
					return false
				}
			case *ast.CallExpr:
				if sel, ok := d.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewBinding" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
						t.Errorf("%s:%d makes a key.Binding outside a function whose name ends in Keys", f.name, f.fset.Position(d.Pos()).Line)
					}
				}
			}
			return true
		})
	}
}

// What the package never does, in any file but where each is allowed.
func TestTheBannedCallsStayOut(t *testing.T) {
	rules := []struct {
		re      *regexp.Regexp
		allowed []string
		why     string
	}{
		{regexp.MustCompile(`\btea\.Sequence\(`), []string{"harness_test.go"}, "the test driver cannot take a sequence apart; use tea.Batch"},
		{regexp.MustCompile(`\btextinput\.New\(`), []string{"theme.go"}, "a text field comes from newInput, with a steady cursor and no clipboard"},
		{regexp.MustCompile(`\btextarea\.New\(`), []string{"theme.go"}, "a text area comes from newArea, with a steady cursor and no clipboard"},
		{regexp.MustCompile(`"charm\.land/bubbles/v2/list"`), nil, "the list component brings keys and a filter of its own"},
		{regexp.MustCompile(`\b(textinput|textarea)\.Paste\b`), nil, "Paste reads the clipboard of the machine the TUI runs on"},
		{regexp.MustCompile(`\.CopySelection\(`), nil, "CopySelection writes the clipboard of the machine the TUI runs on"},
		{regexp.MustCompile(`\bDefaultKeyMap\b`), []string{"theme_test.go"}, "a default key map binds the clipboard again"},
		// R31: every terminal effect is a message of this package, which the
		// shell turns into Bubble Tea's own command in effects.go; a test
		// compares what it became with Bubble Tea's.
		{regexp.MustCompile(`\btea\.(Exec|ExecProcess|SetClipboard|Raw)\(`), []string{"effects.go", "shell_test.go"}, "a terminal effect goes through the shell: handOff, copyText or bellMsg (R31)"},
	}
	for _, f := range rulesFiles(t) {
		if f.name == "rules_test.go" {
			continue
		}
		for _, r := range rules {
			if rulesAllowed(f.name, r.allowed) {
				continue
			}
			for _, loc := range r.re.FindAllIndex(f.code, -1) {
				t.Errorf("%s: %s", rulesLine(f, loc[0]), r.why)
			}
		}
	}
}

func rulesAllowed(name string, allowed []string) bool {
	for _, a := range allowed {
		if a == name {
			return true
		}
	}
	return false
}
