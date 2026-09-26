package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// classes holds the error vocabulary closed from every side: the Class
// constants in internal/gapi/errors.go, the Classes list beside them,
// §6.5's table, and the classes the code actually emits. All four are
// read from source, so the gate works while the package does not build.

const (
	classesErrorsFile = "internal/gapi/errors.go"
	classesDocFile    = "docs/architecture.md"
	classesDocHeading = "### 6.5 Error classes"
	// classesMinDeclared is §6.5's count.
	classesMinDeclared = 12
	// classesMinFiles is how few Go files the emitted side may read
	// before it is not looking at the server.
	classesMinFiles = 5
)

// classesPlan is a class declared now and emitted by a later phase.
type classesPlan struct {
	phase  int
	reason string
}

// classesPlanned names every declared class nothing emits yet. It only
// shrinks: an entry whose class is now emitted fails, and so does one
// whose phase has begun.
var classesPlanned = map[string]classesPlan{
	"unsupported": {2, "modify_labels meets §2.12's first cannot-do: a draft cannot be labeled"},
}

// classesLimits are the floors, a parameter so a test can use a small fixture.
type classesLimits struct{ declared, files int }

var classesFloors = classesLimits{classesMinDeclared, classesMinFiles}

func classes(out io.Writer, _ []string) error {
	return classesCheck(".", classesPlanned, classesFloors, out)
}

func classesCheck(root string, planned map[string]classesPlan, floor classesLimits, out io.Writer) error {
	errorsFile := filepath.Join(root, filepath.FromSlash(classesErrorsFile))
	declared, listed, err := classesDeclared(errorsFile)
	if err != nil {
		return err
	}
	documented, err := classesDocumented(filepath.Join(root, filepath.FromSlash(classesDocFile)))
	if err != nil {
		return err
	}
	emitted, files, err := classesEmitted(root, declared)
	if err != nil {
		return err
	}
	phase := apiCoveragePhase(filepath.Join(root, "CHANGELOG.md"))

	var problems []string
	values := map[string]int{}
	for _, v := range declared {
		values[v]++
	}
	problems = append(problems, classesDuplicates("constants in "+classesErrorsFile, values)...)
	listedValues := map[string]int{}
	for _, name := range listed {
		v, ok := declared[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("Classes lists %s, which is not a declared class", name))
			continue
		}
		listedValues[v]++
	}
	problems = append(problems, classesDuplicates("Classes", listedValues)...)
	problems = append(problems, classesDuplicates("§6.5", documented)...)

	for _, v := range slices.Sorted(maps.Keys(values)) {
		if listedValues[v] == 0 {
			problems = append(problems, fmt.Sprintf("%q is declared and Classes does not list it", v))
		}
		if documented[v] == 0 {
			problems = append(problems, fmt.Sprintf("%q is declared and §6.5 does not tabulate it", v))
		}
		_, isPlanned := planned[v]
		_, isEmitted := emitted[v]
		switch {
		case isEmitted && isPlanned:
			problems = append(problems, fmt.Sprintf("%q is planned and %s emits it; remove its plan", v, emitted[v]))
		case !isEmitted && !isPlanned:
			problems = append(problems, fmt.Sprintf("%q is declared and nothing emits it; "+
				"emit it, plan it with a phase, or remove it", v))
		}
	}
	for _, v := range slices.Sorted(maps.Keys(emitted)) {
		if values[v] == 0 {
			problems = append(problems, fmt.Sprintf("%s emits %q, which is not a declared class", emitted[v], v))
		}
	}
	for _, v := range slices.Sorted(maps.Keys(documented)) {
		if values[v] == 0 {
			problems = append(problems, fmt.Sprintf("§6.5 tabulates %q and the code declares no such class", v))
		}
	}
	for _, v := range slices.Sorted(maps.Keys(planned)) {
		p := planned[v]
		switch {
		case values[v] == 0:
			problems = append(problems, fmt.Sprintf("%q is planned and not declared", v))
		case p.phase < phase:
			problems = append(problems, fmt.Sprintf("%q was planned for phase %d and phase %d has begun", v, p.phase, phase))
		case strings.TrimSpace(p.reason) == "":
			problems = append(problems, fmt.Sprintf("%q is planned with no reason", v))
		}
	}
	if len(values) < floor.declared {
		problems = append(problems, fmt.Sprintf("%d classes declared, below the floor of %d", len(values), floor.declared))
	}
	if files < floor.files {
		problems = append(problems, fmt.Sprintf("read %d Go files, below the floor of %d; "+
			"this gate is not looking at the server", files, floor.files))
	}
	if err := problemsError(out, "the error vocabulary", problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "classes ok: %d declared, %d emitted, %d planned, in %d files\n",
		len(values), len(emitted), len(planned), files)
	return nil
}

// classesDuplicates reports names counted more than once. Counting, not
// comparing neighbors, so order cannot hide one.
func classesDuplicates(where string, counts map[string]int) []string {
	var out []string
	for _, v := range slices.Sorted(maps.Keys(counts)) {
		if counts[v] > 1 {
			out = append(out, fmt.Sprintf("%s names %q %d times", where, v, counts[v]))
		}
	}
	return out
}

// classesDeclared reads the Class constants (name to value) and the
// identifiers in `var Classes`.
func classesDeclared(path string) (map[string]string, []string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", classesErrorsFile, err)
	}
	declared := map[string]string{}
	var listed []string
	foundList := false
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if gen.Tok == token.VAR && name.Name == "Classes" && i < len(vs.Values) {
					foundList = true
					ast.Inspect(vs.Values[i], func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "Class") && id.Name != "Class" {
							listed = append(listed, id.Name)
						}
						return true
					})
					continue
				}
				if gen.Tok != token.CONST || !strings.HasPrefix(name.Name, "Class") || i >= len(vs.Values) {
					continue
				}
				if v, ok := apiCoverageString(vs.Values[i]); ok {
					declared[name.Name] = v
				}
			}
		}
	}
	if len(declared) == 0 {
		return nil, nil, fmt.Errorf("%s declares no Class constants; has the vocabulary moved?", classesErrorsFile)
	}
	if !foundList {
		return nil, nil, fmt.Errorf("%s has no `var Classes`; has the vocabulary moved?", classesErrorsFile)
	}
	return declared, listed, nil
}

// classesDocumented counts the class names in §6.5's table.
func classesDocumented(path string) (map[string]int, error) {
	doc, err := os.ReadFile(path) //nolint:gosec // a path this repository owns
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for line := range strings.Lines(markdownSection(string(doc), classesDocHeading)) {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cell := strings.Split(line, "|")[1]
		counts[strings.Trim(strings.TrimSpace(cell), "`")]++
	}
	if len(counts) == 0 {
		return nil, fmt.Errorf("found no class rows under %q in %s", classesDocHeading, classesDocFile)
	}
	return counts, nil
}

// classesEmitted finds each class the code emits, with the first file
// that does. A use counts; four things are not emission: a declaration
// (the constants and the Classes list), a method on Class describing the
// vocabulary (Valid, Retryable), a comparison with == or !=, and a case
// label — the last two read a class somebody else emitted.
//
// It reads every non-test Go file of the module outside scripts/ and
// testdata/.
func classesEmitted(root string, declared map[string]string) (map[string]string, int, error) {
	emitted := map[string]string{}
	files := 0
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			switch filepath.ToSlash(rel) {
			case "scripts", "testdata", ".git", "vendor":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		files++
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range file.Decls {
			if classesSkipped(decl) {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool { return classesRecord(n, declared, emitted, rel) })
		}
		return nil
	})
	return emitted, files, err
}

// classesRecord applies the emission rule to one node. A case body is
// code like any other; its labels are not.
func classesRecord(n ast.Node, declared, emitted map[string]string, rel string) bool {
	var name string
	switch v := n.(type) {
	case *ast.BinaryExpr:
		if v.Op == token.EQL || v.Op == token.NEQ {
			return false
		}
	case *ast.CaseClause:
		for _, s := range v.Body {
			ast.Inspect(s, func(n ast.Node) bool { return classesRecord(n, declared, emitted, rel) })
		}
		return false
	case *ast.CallExpr:
		// Class("x"): a class made from a literal, declared or not.
		if classesNamesClass(v.Fun) && len(v.Args) == 1 {
			classesLiteral(v.Args[0], emitted, rel)
		}
	case *ast.KeyValueExpr:
		// Error{Class: "x"}.
		if k, ok := v.Key.(*ast.Ident); ok && k.Name == "Class" {
			classesLiteral(v.Value, emitted, rel)
		}
	case *ast.Ident:
		name = v.Name
	case *ast.SelectorExpr:
		name = v.Sel.Name
	}
	if v, ok := declared[name]; ok {
		if _, seen := emitted[v]; !seen {
			emitted[v] = rel
		}
	}
	return true
}

// classesSkipped reports a declaration that names classes without
// emitting them: a const block, `var Classes`, or a method on Class.
func classesSkipped(decl ast.Decl) bool {
	if gen, ok := decl.(*ast.GenDecl); ok {
		if gen.Tok == token.CONST {
			return true
		}
		for _, spec := range gen.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok && slices.ContainsFunc(vs.Names, func(n *ast.Ident) bool { return n.Name == "Classes" }) {
				return true
			}
		}
		return false
	}
	fn, ok := decl.(*ast.FuncDecl)
	if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.Name == "Class"
}

// classesNamesClass reports an expression naming the Class type.
func classesNamesClass(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == "Class"
	case *ast.SelectorExpr:
		return e.Sel.Name == "Class"
	}
	return false
}

// classesLiteral records a class written as a string literal.
func classesLiteral(e ast.Expr, emitted map[string]string, rel string) {
	if v, ok := apiCoverageString(e); ok {
		if _, seen := emitted[v]; !seen {
			emitted[v] = rel
		}
	}
}
