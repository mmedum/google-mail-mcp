package config_test

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/v3/internal/config"
	"github.com/mmedum/google-mail-mcp/v3/internal/credentials"
)

// These tests read the server's code and its readers outside this
// package, internal/credentials among them, which imports config; so
// they are an external test package.

// varsFloor is how many variables the server reads today, on purpose:
// the floor moves with it, so a registry that records nothing cannot
// pass a walk over it.
const varsFloor = 14

// registry is config.Vars, refused below the floor.
func registry(t *testing.T) []config.Var {
	t.Helper()
	vars := config.Vars()
	if len(vars) < varsFloor {
		t.Fatalf("Vars has %d variables, want at least %d", len(vars), varsFloor)
	}
	return vars
}

// A variable of spaces alone is unset, as an empty one is, and the spaces
// around a value are dropped, so a form field cleared to a space starts
// the server with the default. That holds for every variable the server
// reads: Define's through Load, and each one read outside Define through
// its reader. One read outside Define with no reader here fails, so a
// variable added there cannot miss the rule.
func TestASettingOfSpacesIsUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))

	outside := map[string]func(value string) string{
		config.EnvRefreshToken: func(value string) string {
			s := credentials.Store{Profile: "p", Env: func(string) string { return value }}
			tok, src, err := s.Resolve()
			return fmt.Sprintf("%q %q %v", tok, src, err)
		},
	}
	defined := map[string]bool{}
	fs := flag.NewFlagSet("defined", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config.Define(fs, func(name string) string { defined[name] = true; return "" })
	for _, v := range registry(t) {
		read := func(value string) string {
			c, err := config.Load(nil, func(name string) string {
				if name == v.Name {
					return value
				}
				return ""
			})
			return fmt.Sprintf("%+v %v", c, err)
		}
		if !defined[v.Name] {
			var ok bool
			if read, ok = outside[v.Name]; !ok {
				t.Errorf("%s is read outside Define, and this test has no reader for it", v.Name)
				continue
			}
			delete(outside, v.Name)
		}
		for _, pair := range [][2]string{{"  ", ""}, {" x ", "x"}} {
			if got, want := read(pair[0]), read(pair[1]); got != want {
				t.Errorf("%s=%q: %s\nwant %s, as %s=%q", v.Name, pair[0], got, want, v.Name, pair[1])
			}
		}
	}
	for name := range outside {
		t.Errorf("this test has a reader for %s, which Vars does not list as read outside Define", name)
	}
}

// Every GMAIL_ variable the server's code spells is one EnvVars lists, so
// a variable read outside Define cannot escape the bundle and staleness
// gates. So is every one scripts/ spells: the live driver and the smoke
// test set them for the server, and a misspelling there tests a
// configuration nobody asked for. A name is seen as a string literal, or
// as a chain of literals and EnvPrefix joined by +; one built from any
// other constant, or through a format verb, is not seen. The scan must
// read every root and find a name outside the config package, or it read
// too little.
func TestEveryVariableTheCodeNamesIsInEnvVars(t *testing.T) {
	known := map[string]bool{}
	for _, name := range config.EnvVars() {
		known[name] = true
	}
	name := regexp.MustCompile(`^` + config.EnvPrefix + `[A-Z0-9][A-Z0-9_]*$`)
	found := map[string]string{}
	elsewhere := 0
	fset := token.NewFileSet()
	roots := []string{"../../cmd", "../../internal", "../../scripts"}
	files := map[string]int{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			files[root]++
			outside := filepath.Dir(path) != filepath.Join("..", "..", "internal", "config")
			spell := func(s string, at token.Pos) {
				for _, word := range strings.FieldsFunc(s, func(r rune) bool {
					return r != '_' && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
				}) {
					if name.MatchString(word) {
						found[word] = fset.Position(at).String()
						if outside {
							elsewhere++
						}
					}
				}
			}
			var visit func(n ast.Node) bool
			visit = func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BinaryExpr:
					if n.Op != token.ADD {
						return true
					}
					// A + chain is read in order: each run of literals
					// and EnvPrefix is one string, and any other operand
					// ends the run and is read on its own.
					run, at := "", n.Pos()
					for _, op := range operands(n) {
						if s, ok := joined(op); ok {
							if run == "" {
								at = op.Pos()
							}
							run += s
							continue
						}
						spell(run, at)
						run = ""
						ast.Inspect(op, visit)
					}
					spell(run, at)
					return false
				case *ast.BasicLit:
					if s, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil {
						spell(s, n.Pos())
					}
				}
				return true
			}
			ast.Inspect(f, visit)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range roots {
		if files[root] == 0 {
			t.Errorf("the scan parsed no Go file under %s; it read too little", root)
		}
	}
	if elsewhere == 0 {
		t.Errorf("the scan found no %s name outside internal/config; it read too little", config.EnvPrefix)
	}
	for _, v := range slices.Sorted(maps.Keys(found)) {
		if !known[v] {
			t.Errorf("%s names %s, which EnvVars does not list", found[v], v)
		}
	}
}

// operands is a + chain's operands in order, through parentheses.
func operands(e ast.Expr) []ast.Expr {
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return append(operands(e.X), operands(e.Y)...)
		}
	case *ast.ParenExpr:
		if b, ok := e.X.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			return operands(b)
		}
	}
	return []ast.Expr{e}
}

// joined is a + chain of string literals and EnvPrefix as one string.
func joined(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		x, ok := joined(e.X)
		if !ok {
			return "", false
		}
		y, ok := joined(e.Y)
		return x + y, ok
	case *ast.ParenExpr:
		return joined(e.X)
	case *ast.BasicLit:
		s, err := strconv.Unquote(e.Value)
		return s, e.Kind == token.STRING && err == nil
	case *ast.Ident:
		return config.EnvPrefix, e.Name == "EnvPrefix"
	case *ast.SelectorExpr:
		return config.EnvPrefix, e.Sel.Name == "EnvPrefix"
	}
	return "", false
}
