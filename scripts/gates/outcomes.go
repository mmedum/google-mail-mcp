package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// outcomes holds the rule that a result states what happened, never what
// was asked for (docs/architecture.md §4.12), to the one shape of it a
// machine can check.
//
// The shape: a branch tests a boolean field of the request and then
// states, in prose, what is now true, without consulting Gmail. The
// argument is treated as its own evidence. Three shapes are honest and
// recognized structurally: the branch calls the client (or enters the
// no-writes context of a dry run); the branch refuses with an error; the
// prose is hypothetical ("would …"). A branch that is honest anyway takes
// a row in testdata/outcome-claims.tsv with its reason.
//
// The unit is the branch, not the function: a handler may read a field
// and then ask Gmail before it speaks, and that is exactly right.
func outcomes(out io.Writer, _ []string) error {
	return outcomesCheck(out, ".", outcomesDirs, outcomesClaims)
}

const (
	// outcomesDir is the MCP surface: where the input types and handlers
	// live.
	outcomesDir = "internal/tools"
	// outcomesServiceDir is where the handlers' requests are acted on; a
	// branch on a request field lives here as often as in the handler.
	outcomesServiceDir = "internal/service"
	// outcomesClaims records branches judged honest: file:Field, reason.
	outcomesClaims = "testdata/outcome-claims.tsv"
	// outcomesMinFiles is the floor on files read.
	outcomesMinFiles = 2
)

// outcomesDirs are read together: a tool's input is declared in the
// first and acted on in the second.
var outcomesDirs = []string{outcomesDir, outcomesServiceDir}

// outcomesWriteKinds are the Kinds whose tools change something. Until a
// Spec literal names one, there are no writes to hold and the floors on
// fields do not apply; from the first, each write tool must bring at
// least one boolean input (its dry_run) for the gate to read.
var outcomesWriteKinds = map[string]bool{"Write": true, "WriteForGood": true, "Send": true, "Destructive": true}

// outcomesFile is one parsed file with its slash path.
type outcomesFile struct {
	path string
	file *ast.File
}

// outcomesClaim is one branch that states an outcome from the request.
type outcomesClaim struct {
	file, field, quote string
	line               int
}

func outcomesCheck(out io.Writer, root string, dirs []string, claims string) error {
	claimsPath := filepath.Join(root, filepath.FromSlash(claims))
	fset := token.NewFileSet()
	var files []outcomesFile
	for _, d := range dirs {
		fs, err := outcomesParse(fset, root, d)
		if err != nil {
			return err
		}
		files = append(files, fs...)
	}
	dir := strings.Join(dirs, " and ")
	if len(files) < outcomesMinFiles {
		return fmt.Errorf("read %d Go file(s) in %s, want at least %d; this check is not looking at the "+
			"code it is meant to", len(files), dir, outcomesMinFiles)
	}
	writes := outcomesWriteSpecs(files)
	fields, pairs := outcomesBoolFields(files)
	if pairs < writes {
		return fmt.Errorf("%d write tool(s) registered in %s and %d boolean input field(s): every write "+
			"takes dry_run, so the gate is not reading their inputs", writes, dir, pairs)
	}
	exempt, problems := outcomesReadClaims(claimsPath)

	found, branches := outcomesFind(files, fields, fset)
	if writes > 0 && branches == 0 {
		return fmt.Errorf("%d write tool(s) registered and no branch in %s tests a boolean of a request; "+
			"every write has at least a dry run or a confirm to branch on, so the gate is not reading the code "+
			"that acts on requests", writes, dir)
	}
	used := map[string]int{}
	for _, c := range found {
		key := c.file + ":" + c.field
		if _, ok := exempt[key]; ok {
			used[key]++
			continue
		}
		problems = append(problems, fmt.Sprintf("%s:%d: this branch tests the request field %s and then "+
			"states an outcome — %q — without asking Gmail. Read it back, word it as what was asked for, or "+
			"record it in %s with the reason", c.file, c.line, c.field, c.quote, claims))
	}
	for _, key := range outcomesSorted(exempt) {
		switch {
		case used[key] == 0:
			problems = append(problems, fmt.Sprintf("%s excuses %s and nothing there states an outcome from "+
				"the request any more; delete the row", claims, key))
		case used[key] > 1:
			problems = append(problems, fmt.Sprintf("%s excuses %s once and %d branches there state an "+
				"outcome after testing it; one argument cannot cover two branches", claims, key, used[key]))
		}
	}
	sort.Strings(problems)
	if err := problemsError(out, dir, problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "outcomes ok (%d files in %s, %d write tool(s), %d boolean input field(s), "+
		"%d branch(es) testing one, %d excused)\n", len(files), dir, writes, pairs, branches, len(exempt))
	return nil
}

func outcomesParse(fset *token.FileSet, root, dir string) ([]outcomesFile, error) {
	parsed, err := parseGoDir(fset, filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return nil, fmt.Errorf("%w (the MCP surface is %s)", err, dir)
	}
	out := make([]outcomesFile, 0, len(parsed))
	for _, f := range parsed {
		out = append(out, outcomesFile{path: dir + "/" + filepath.Base(fileName(fset, f)), file: f})
	}
	return out, nil
}

// outcomesWriteSpecs counts Spec literals whose Kind is a write kind.
func outcomesWriteSpecs(files []outcomesFile) int {
	n := 0
	for _, pf := range files {
		ast.Inspect(pf.file, func(node ast.Node) bool {
			// Any literal with a Kind field, since a slice of Spec elides
			// the type on each element.
			lit, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Kind" {
					if v, ok := kv.Value.(*ast.Ident); ok && outcomesWriteKinds[v.Name] {
						n++
					}
				}
			}
			return true
		})
	}
	return n
}

// outcomesRequestType names a tool's input type: this repository's
// FooIn, or FooInput as the shared standard writes it. The gate read
// nothing until it knew the first spelling, which its floor caught the
// day the first write tool was registered.
func outcomesRequestType(name string) bool {
	return strings.HasSuffix(name, "In") || strings.HasSuffix(name, "Input")
}

// outcomesServiceTypes are the struct types an exported method takes as
// a parameter: the requests a handler passes on to the service, whatever
// they are called.
func outcomesServiceTypes(files []outcomesFile) map[string]bool {
	structs := map[string]bool{}
	for _, pf := range files {
		ast.Inspect(pf.file, func(n ast.Node) bool {
			if spec, ok := n.(*ast.TypeSpec); ok {
				if _, ok := spec.Type.(*ast.StructType); ok {
					structs[spec.Name.Name] = true
				}
			}
			return true
		})
	}
	out := map[string]bool{}
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() {
				continue
			}
			for _, p := range fn.Type.Params.List {
				if id, ok := p.Type.(*ast.Ident); ok && structs[id.Name] {
					out[id.Name] = true
				}
			}
		}
	}
	return out
}

// outcomesBoolFields is every boolean field on every input type, from
// the source, so a tool added later is covered by declaring its input;
// and how many there are counted per type, which the floor compares with
// the write tools, since every write's input has at least its dry_run.
func outcomesBoolFields(files []outcomesFile) (map[string]bool, int) {
	out := map[string]bool{}
	pairs := 0
	service := outcomesServiceTypes(files)
	for _, pf := range files {
		ast.Inspect(pf.file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || !outcomesRequestType(spec.Name.Name) && !service[spec.Name.Name] {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, f := range st.Fields.List {
				if id, ok := f.Type.(*ast.Ident); ok && id.Name == "bool" {
					for _, name := range f.Names {
						out[name.Name] = true
						pairs++
					}
				}
			}
			return true
		})
	}
	return out, pairs
}

// outcomesRequestNames is every identifier bound to a request in the
// file: a parameter of an input or service request type, by a function
// or a function literal, and apart from those, the bare bool parameters
// of exported methods — a confirm passed on by itself.
func outcomesRequestNames(file *ast.File, service map[string]bool) (requests, bools map[string]bool) {
	requests, bools = map[string]bool{}, map[string]bool{}
	collect := func(params *ast.FieldList, exported bool) {
		if params == nil {
			return
		}
		for _, p := range params.List {
			t := p.Type
			if star, ok := t.(*ast.StarExpr); ok {
				t = star.X
			}
			id, ok := t.(*ast.Ident)
			if !ok {
				continue
			}
			for _, name := range p.Names {
				switch {
				case outcomesRequestType(id.Name) || service[id.Name]:
					requests[name.Name] = true
				case exported && id.Name == "bool":
					bools[name.Name] = true
				}
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncDecl:
			collect(v.Type.Params, v.Recv != nil && v.Name.IsExported())
		case *ast.FuncLit:
			collect(v.Type.Params, false)
		}
		return true
	})
	return requests, bools
}

// outcomesFind returns the dishonest branches and how many branches of
// the shape were examined.
func outcomesFind(files []outcomesFile, fields map[string]bool, fset *token.FileSet) ([]outcomesClaim, int) {
	var found []outcomesClaim
	branches := 0
	service := outcomesServiceTypes(files)
	for _, pf := range files {
		requests, bools := outcomesRequestNames(pf.file, service)
		if len(requests)+len(bools) == 0 {
			continue
		}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			stmt, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			field := outcomesTestedField(stmt.Cond, fields, requests, bools)
			if field == "" {
				return true
			}
			branches++
			if outcomesConsults(stmt.Body) || outcomesRefuses(stmt.Body) {
				return true
			}
			if quote, line := outcomesStated(stmt.Body, fset); quote != "" {
				found = append(found, outcomesClaim{file: pf.path, field: field, quote: quote, line: line})
			}
			return true
		})
	}
	return found, branches
}

// outcomesTestedField is the boolean request field, or bare bool
// parameter, a condition tests.
func outcomesTestedField(cond ast.Expr, fields, requests, bools map[string]bool) string {
	name := ""
	ast.Inspect(cond, func(n ast.Node) bool {
		if name != "" {
			return false
		}
		switch v := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok && requests[id.Name] && fields[v.Sel.Name] {
				name = v.Sel.Name
			}
			return false
		case *ast.Ident:
			if bools[v.Name] {
				name = v.Name
			}
		}
		return name == ""
	})
	return name
}

// outcomesConsults reports whether the branch asks Gmail before it
// speaks: any call reached through something named Client, a service,
// or the gapi package, including entering the no-writes context.
func outcomesConsults(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		ast.Inspect(call.Fun, func(m ast.Node) bool {
			switch v := m.(type) {
			case *ast.Ident:
				lower := strings.ToLower(v.Name)
				if lower == "client" || lower == "gapi" || lower == "svc" || lower == "service" {
					found = true
				}
			case *ast.SelectorExpr:
				if v.Sel.Name == "Client" || v.Sel.Name == "WithoutWrites" {
					found = true
				}
			}
			return !found
		})
		return !found
	})
	return found
}

// outcomesRefuses reports whether the branch ends by returning an error.
func outcomesRefuses(body *ast.BlockStmt) bool {
	if body == nil || len(body.List) == 0 {
		return false
	}
	ret, ok := body.List[len(body.List)-1].(*ast.ReturnStmt)
	if !ok {
		return false
	}
	for _, result := range ret.Results {
		call, ok := result.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "Errorf", "New", "Errf", "Wrap":
				return true
			}
		}
	}
	return false
}

// outcomesStated is the first sentence in the branch that states what is
// now true, and its line. Hypothetical wording is not an outcome.
func outcomesStated(body *ast.BlockStmt, fset *token.FileSet) (string, int) {
	quote, line := "", 0
	ast.Inspect(body, func(n ast.Node) bool {
		if quote != "" {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if !outcomesIsProse(value) || outcomesIsHypothetical(value) {
			return true
		}
		quote, line = outcomesClip(value), fset.Position(lit.Pos()).Line
		return false
	})
	return quote, line
}

// outcomesIsProse separates a sentence from a key or a format verb.
func outcomesIsProse(s string) bool {
	return len(s) >= 16 && strings.Contains(strings.TrimSpace(s), " ")
}

func outcomesIsHypothetical(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	for _, prefix := range []string{"would ", "asked ", "requested ", "dry run"} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

func outcomesClip(s string) string {
	const limit = 70
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// outcomesReadClaims reads the record. A missing file is an empty record:
// no branch has been excused.
func outcomesReadClaims(path string) (map[string]string, []string) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	rows, problems := readTSV(path, 2)
	out := map[string]string{}
	for _, row := range rows {
		if strings.TrimSpace(row.fields[1]) == "" {
			problems = append(problems, fmt.Sprintf("%s:%d: %s carries no reason", path, row.line, row.fields[0]))
			continue
		}
		out[row.fields[0]] = row.fields[1]
	}
	return out, problems
}

func outcomesSorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
