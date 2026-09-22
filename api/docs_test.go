package api

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The manual is part of what this server is, and a route nobody wrote down
// is a route nobody can call. This is the half of that promise that belongs
// to the api package: every path this server can register has a row in the
// endpoint table of docs/reference.md. The other half — every command and
// every flag — is in cmd/rota, beside the code that registers those.
//
// Both sides are read from the code rather than from a list kept by hand,
// because a list kept by hand is the thing that rotted.

// TestReferenceDocumentsEveryRoute compares the patterns a server with every
// group switched on registers against the table a reader is sent to.
func TestReferenceDocumentsEveryRoute(t *testing.T) {
	documented := endpointTable(t)
	for _, pattern := range registeredPatterns(t) {
		if path := routePath(pattern); !documented[path] {
			t.Errorf("%s is registered but has no row in the endpoint table of docs/reference.md", pattern)
		}
	}
}

// registeredPatterns is every pattern this server can put on its mux: the
// ones Handler writes out itself, read from the source because they are
// literals inside a function rather than a table, and the guarded and socket
// routes, read from the maps that hold them.
func registeredPatterns(t *testing.T) []string {
	t.Helper()
	// Every group on, the terminal included: a group this server happened to
	// be built without would otherwise never be looked at.
	s, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1,
		Routes: &Routes{API: true, Playground: true, WebSocket: true, Health: true, Terminal: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	out := handlerPatterns(t)
	for pattern := range s.guarded() {
		out = append(out, pattern)
	}
	for pattern := range s.sockets() {
		out = append(out, pattern)
	}
	if len(out) < 20 {
		t.Fatalf("only %d routes found; the way they are registered must have changed", len(out))
	}
	return out
}

// handlerPatterns reads the mux patterns written as literals in Handler.
// mux.Handle(pattern, …) inside the two loops takes a variable and is not
// one of these; those patterns come from the maps instead.
func handlerPatterns(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(gotoken.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
			return true
		}
		if s, ok := literal(call.Args[0]); ok {
			out = append(out, s)
		}
		return true
	})
	return out
}

// routePath is the path half of a mux pattern. "{$}" is the standard
// library's way of spelling "this path exactly", and the path it means is
// the one a reader types.
func routePath(pattern string) string {
	path := pattern
	if _, rest, found := strings.Cut(pattern, " "); found {
		path = rest
	}
	return strings.TrimSuffix(path, "{$}")
}

// endpointTable is the paths of the one table in docs/reference.md that
// lists every endpoint, found by its header rather than by its line number.
func endpointTable(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "docs", "reference.md"))
	if err != nil {
		t.Fatal(err)
	}
	paths, inTable := map[string]bool{}, false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "| Method | Path | Role |") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 {
			continue
		}
		if p := strings.Trim(strings.TrimSpace(cells[1]), "`"); strings.HasPrefix(p, "/") {
			paths[p] = true
		}
	}
	if len(paths) == 0 {
		t.Fatal("docs/reference.md has no endpoint table headed `| Method | Path | Role |`")
	}
	return paths
}

// literal reads a string constant written in the source.
func literal(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != gotoken.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
