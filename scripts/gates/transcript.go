package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// transcript holds the programs that talk to a real mailbox to one way of
// printing.
//
// The live driver and the evals print what the server answered, which is
// mail. The rule is not "call the redactor"; it is that these packages
// cannot reach a terminal at all. os.Stdout and os.Stderr name the
// terminal, fmt.Print* and the print builtins write to it without naming
// it, and log and log/slog are the same thing again. The transcript
// package is the one exemption, and it has to redact through exactly one
// write.
func transcript(out io.Writer, _ []string) error {
	return transcriptCheck(out, ".", transcriptPackages, transcriptExempt)
}

// transcriptPackages may not reach a terminal. Besides the two drivers,
// the packages every line they print passes through: a debugging Println
// in any of them would leak past everything else here.
var transcriptPackages = []string{
	"scripts/livemail",
	"scripts/evals",
	"scripts/internal/mcpstdio",
	"scripts/internal/redact",
	"scripts/internal/livecover",
}

// transcriptExempt is the one place a line reaches a terminal.
const transcriptExempt = "scripts/internal/transcript"

// transcriptTags are the build tags the drivers compile under; without
// them the scan would skip every file of both.
var transcriptTags = []string{"live", "evals"}

// The floors. A scan that found nothing and a scan that read nothing
// print the same thing otherwise.
const (
	// transcriptMinFiles is one per listed package.
	transcriptMinFiles = 5
	// transcriptMinMentions is the exempt package naming os.Stdout and
	// os.Stderr: if the scanner cannot see those two, it cannot see
	// them anywhere else either.
	transcriptMinMentions = 2
)

// transcriptWrite is one way out of a package that should have none.
type transcriptWrite struct {
	line int
	what string
}

func transcriptCheck(out io.Writer, root string, packages []string, exempt string) error {
	var problems []string
	read := 0
	fset := token.NewFileSet()
	for _, dir := range packages {
		// A listed package that is missing fails: skipping it would let
		// the list name anything.
		files, err := transcriptGoFiles(fset, filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s is listed and cannot be read (%v); a gate that "+
				"skips a missing package can list anything", dir, err))
			continue
		}
		for _, file := range files {
			read++
			for _, w := range transcriptWrites(fset, file) {
				problems = append(problems, fmt.Sprintf("%s:%d: %s reaches the terminal directly; "+
					"print through %s, which redacts", transcriptRel(root, fileName(fset, file)), w.line, w.what, exempt))
			}
		}
	}

	unlisted, err := transcriptUnlisted(root, packages, exempt)
	if err != nil {
		return err
	}
	problems = append(problems, unlisted...)

	writes, redacting, mentions, err := transcriptExemptWrites(filepath.Join(root, filepath.FromSlash(exempt)))
	if err != nil {
		return err
	}
	if writes != 1 || redacting != 1 {
		problems = append(problems, fmt.Sprintf("%s is the one package allowed to write to a terminal, "+
			"through exactly one write that redacts; it has %d write(s), %d of them redacting",
			exempt, writes, redacting))
	}
	if err := problemsError(out, "the ways to a terminal", problems); err != nil {
		return err
	}
	if read < transcriptMinFiles {
		return fmt.Errorf("read %d Go file(s) across %d packages, want at least %d; this check is not "+
			"looking at the code it is meant to", read, len(packages), transcriptMinFiles)
	}
	if mentions < transcriptMinMentions {
		return fmt.Errorf("found %d mention(s) of the terminal in %s, want at least %d; the scanner "+
			"has stopped recognizing them", mentions, exempt, transcriptMinMentions)
	}
	_, _ = fmt.Fprintf(out, "transcript ok (%d files in %d packages; %d terminal mentions and one "+
		"redacting write in %s)\n", read, len(packages), mentions, exempt)
	return nil
}

// transcriptWrites finds every way a file can reach a terminal: naming
// os.Stdout or os.Stderr however it is then used, fmt.Print*, the print
// builtins, and importing log or log/slog.
func transcriptWrites(fset *token.FileSet, file *ast.File) []transcriptWrite {
	var found []transcriptWrite
	note := func(pos token.Pos, what string) {
		found = append(found, transcriptWrite{line: fset.Position(pos).Line, what: what})
	}
	for _, imp := range file.Imports {
		name, err := strconv.Unquote(imp.Path.Value)
		if err == nil && (name == "log" || name == "log/slog") {
			note(imp.Pos(), "the "+name+" package")
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			if transcriptIsTerminal(v) {
				note(v.Pos(), v.X.(*ast.Ident).Name+"."+v.Sel.Name)
			}
		case *ast.CallExpr:
			if id, ok := v.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
				note(v.Pos(), "the builtin "+id.Name)
			}
		}
		return true
	})
	return found
}

// transcriptIsTerminal reports os.Stdout, os.Stderr and fmt.Print*.
func transcriptIsTerminal(sel *ast.SelectorExpr) bool {
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch pkg.Name {
	case "os":
		return sel.Sel.Name == "Stdout" || sel.Sel.Name == "Stderr"
	case "fmt":
		return sel.Sel.Name == "Print" || sel.Sel.Name == "Printf" || sel.Sel.Name == "Println"
	}
	return false
}

// transcriptExemptWrites counts the exempt package's fmt.Fprint* calls,
// how many pass their text through the redactor's Do, and how many times
// it names the terminal.
func transcriptExemptWrites(dir string) (writes, redacting, mentions int, err error) {
	files, err := transcriptGoFiles(token.NewFileSet(), dir)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && transcriptIsTerminal(sel) {
				mentions++
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" || !strings.HasPrefix(sel.Sel.Name, "Fprint") {
				return true
			}
			writes++
			if slices.ContainsFunc(call.Args, transcriptCallsDo) {
				redacting++
			}
			return true
		})
	}
	return writes, redacting, mentions, nil
}

// transcriptCallsDo reports whether an expression passes through a Do
// call, which is the redactor's method.
func transcriptCallsDo(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Do" {
				found = true
			}
		}
		return !found
	})
	return found
}

// transcriptGoFiles parses a package's non-test Go source as its own
// build compiles it. A package with none is an error.
func transcriptGoFiles(fset *token.FileSet, dir string) ([]*ast.File, error) {
	files, err := parseGoDir(fset, dir, transcriptTags...)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go source in %s", dir)
	}
	return files, nil
}

// transcriptUnlisted finds a program under scripts/ that prints what a
// real account answered and is not listed. What such a program has in
// common is that it imports the redactor, the transcript or the stdio
// client, so that is the mark.
func transcriptUnlisted(root string, packages []string, exempt string) ([]string, error) {
	var problems []string
	fset := token.NewFileSet()
	scripts := filepath.Join(root, "scripts")
	err := filepath.WalkDir(scripts, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := transcriptRel(root, filepath.Dir(path))
		if slices.Contains(packages, dir) || dir == exempt {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		for _, imp := range file.Imports {
			name, _ := strconv.Unquote(imp.Path.Value)
			for _, mark := range []string{"/scripts/internal/redact", "/scripts/internal/transcript",
				"/scripts/internal/mcpstdio"} {
				if strings.HasSuffix(name, mark) {
					problems = append(problems, fmt.Sprintf("%s imports %s and is not a package this "+
						"gate covers; add it to transcriptPackages", dir, name))
				}
			}
		}
		return nil
	})
	slices.Sort(problems)
	return slices.Compact(problems), err
}

// transcriptRel is path relative to root, slash-separated.
func transcriptRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
