// Package livecover measures how much of the tool surface a live run
// drives, per tool option.
//
// Two halves answer different questions. FromSource reads the driver's
// source and says which tools a step calls and which options it sends;
// that runs in CI, where there are no credentials, and is what
// `gates live-cover` holds. A Recorder says what a run actually sent,
// which is not the same thing, and the driver prints the difference at
// the end of every run.
package livecover

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The step shape the driver uses, by field name.
const (
	toolField    = "tool"
	argsField    = "args"
	pendingField = "pending"
)

// FromSource reads a driver's source and returns, per tool some step
// calls, the options it sends. A tool present with an empty set is
// called with no options.
//
// known is the published surface. A string literal counts as a tool only
// when it names one, so a helper taking a name and a map is not mistaken
// for a call.
//
// Two shapes are read: a step literal `{tool: "x", args: ...}`, where
// args is a map literal, a map variable, or a function literal whose map
// literals are the arguments; and a helper call whose arguments are a
// tool name and a map. A step literal carrying a non-empty `pending`
// reason is a step not yet written, and drives nothing.
func FromSource(dir string, known map[string][]string) (map[string]map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	sent := map[string]map[string]bool{}
	fset := token.NewFileSet()
	read := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s: %w", name, parseErr)
		}
		read++
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					readNode(d, mapsIn(d), known, sent)
				}
			case *ast.GenDecl:
				readNode(d, map[string][]string{}, known, sent)
			}
		}
	}
	if read == 0 {
		return nil, fmt.Errorf("no Go source in %s; has the driver moved?", dir)
	}
	return sent, nil
}

// readNode collects what one declaration sends. Map variables are
// resolved within the declaration, which is the scope they live in.
func readNode(n ast.Node, maps map[string][]string, known map[string][]string, sent map[string]map[string]bool) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CompositeLit:
			tool, args, pending := stepLiteral(v)
			if tool != "" && !pending {
				record(sent, known, tool, keysOf(args, maps), true)
			}
		case *ast.CallExpr:
			tool := ""
			for _, arg := range v.Args {
				if lit, ok := stringLit(arg); ok {
					tool = lit
					break
				}
			}
			if tool == "" {
				return true
			}
			for _, arg := range v.Args {
				if isArgs(arg, maps) {
					record(sent, known, tool, keysOf(arg, maps), true)
				}
			}
		}
		return true
	})
}

// stepLiteral reads a `{tool: "x", args: ..., pending: "..."}` literal.
func stepLiteral(lit *ast.CompositeLit) (tool string, args ast.Expr, pending bool) {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case toolField:
			if s, ok := stringLit(kv.Value); ok {
				tool = s
			}
		case argsField:
			args = kv.Value
		case pendingField:
			if s, ok := stringLit(kv.Value); !ok || strings.TrimSpace(s) != "" {
				pending = true
			}
		}
	}
	return tool, args, pending
}

// mapsIn collects every map[string]any a function builds, by variable
// name, including keys assigned into it afterwards.
func mapsIn(fn *ast.FuncDecl) map[string][]string {
	out := map[string][]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			if i >= len(as.Rhs) {
				break
			}
			if id, ok := lhs.(*ast.Ident); ok {
				if lit, ok := as.Rhs[i].(*ast.CompositeLit); ok && isArgMap(lit) {
					out[id.Name] = append(out[id.Name], literalKeys(lit)...)
				}
				continue
			}
			if idx, ok := lhs.(*ast.IndexExpr); ok {
				key, ok := stringLit(idx.Index)
				if !ok {
					continue
				}
				switch target := idx.X.(type) {
				case *ast.Ident:
					out[target.Name] = append(out[target.Name], key)
				case *ast.SelectorExpr:
					out[target.Sel.Name] = append(out[target.Sel.Name], key)
				}
			}
		}
		return true
	})
	return out
}

// isArgs reports whether an expression can carry a tool's arguments.
func isArgs(e ast.Expr, maps map[string][]string) bool {
	switch v := e.(type) {
	case *ast.CompositeLit:
		return isArgMap(v)
	case *ast.FuncLit:
		return true
	case *ast.Ident:
		_, ok := maps[v.Name]
		return ok
	}
	return false
}

// keysOf reads the option names an argument expression carries.
func keysOf(e ast.Expr, maps map[string][]string) []string {
	switch v := e.(type) {
	case *ast.CompositeLit:
		if isArgMap(v) {
			return literalKeys(v)
		}
	case *ast.Ident:
		return maps[v.Name]
	case *ast.SelectorExpr:
		return maps[v.Sel.Name]
	case *ast.FuncLit:
		// Every map literal the function builds, and every key assigned
		// into a map inside it.
		var keys []string
		ast.Inspect(v.Body, func(n ast.Node) bool {
			switch w := n.(type) {
			case *ast.CompositeLit:
				if isArgMap(w) {
					keys = append(keys, literalKeys(w)...)
				}
			case *ast.AssignStmt:
				for _, lhs := range w.Lhs {
					if idx, ok := lhs.(*ast.IndexExpr); ok {
						if key, ok := stringLit(idx.Index); ok {
							keys = append(keys, key)
						}
					}
				}
			}
			return true
		})
		return keys
	}
	return nil
}

// isArgMap reports whether a literal is a map[string]any. `any` is an
// identifier to the parser, not an interface node.
func isArgMap(lit *ast.CompositeLit) bool {
	m, ok := lit.Type.(*ast.MapType)
	if !ok {
		return false
	}
	key, ok := m.Key.(*ast.Ident)
	if !ok || key.Name != "string" {
		return false
	}
	switch value := m.Value.(type) {
	case *ast.Ident:
		return value.Name == "any"
	case *ast.InterfaceType:
		return value.Methods == nil || len(value.Methods.List) == 0
	}
	return false
}

func literalKeys(lit *ast.CompositeLit) []string {
	var out []string
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := stringLit(kv.Key); ok {
			out = append(out, key)
		}
	}
	return out
}

// record notes a call and its options against a tool the server
// publishes. called marks the tool as called even with no options.
func record(sent map[string]map[string]bool, known map[string][]string, tool string, keys []string, called bool) {
	if _, ok := known[tool]; !ok {
		return
	}
	if len(keys) == 0 && !called {
		return
	}
	if sent[tool] == nil {
		sent[tool] = map[string]bool{}
	}
	for _, k := range keys {
		sent[tool][k] = true
	}
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// Sorted is the keys of a map, in order.
func Sorted[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
