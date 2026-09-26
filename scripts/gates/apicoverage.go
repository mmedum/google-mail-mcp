package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// api-coverage holds one verdict per published method against the
// snapshot and the client, offline, in three directions: a published
// method with no row, a row for a method that no longer exists, and a
// client call with no used row. The verb and path live only in the
// snapshot, which a machine writes; the record holds verdicts.

const (
	apiCoverageRecordPath = "testdata/api-coverage.tsv"
	// apiCoverageMinReason is the shortest verdict an out row may give.
	apiCoverageMinReason = 20
	// apiCoverageMinMethods is the published count on 2026-09-25.
	apiCoverageMinMethods = 79
	// apiCoverageMinInScope is §8a's used plus gated. Moving a method
	// out is an argued decision, so it moves this floor too: 38, less
	// filters.get and forwardingAddresses.get in phase 1 (§18 row 36).
	apiCoverageMinInScope = 36
	// apiCoverageMinCalls rises as each phase lands its client calls:
	// twenty at the end of phase 1.
	apiCoverageMinCalls = 20
)

// apiCoverageStates are the record's states. A used row has a client
// call; a planned row names the §16 phase that builds it; an out row is
// deferred or written off and says why.
var apiCoverageStates = map[string][]string{
	"used":    {"Used", "Gated"},
	"planned": {"Used", "Gated"},
	"out":     {"Written off", "Deferred"},
}

// apiCoverageLimits are the floors, a parameter so a test can use a small
// fixture and another can watch the real floors fail.
type apiCoverageLimits struct{ methods, inScope, calls int }

var apiCoverageFloors = apiCoverageLimits{apiCoverageMinMethods, apiCoverageMinInScope, apiCoverageMinCalls}

func apiCoverage(out io.Writer, _ []string) error {
	return apiCoverageCheck(".", apiCoverageFloors, out)
}

// apiCoverageCall is one composite literal of gapi.Call.
type apiCoverageCall struct {
	id, verb, path, at string
}

func apiCoverageCheck(root string, floor apiCoverageLimits, out io.Writer) error {
	surface, err := discoveryLoad(filepath.Join(root, discoverySnapshotPath))
	if err != nil {
		return fmt.Errorf("read the snapshot (run `go run ./scripts/gates api-diff`): %w", err)
	}
	if len(surface.Methods) < floor.methods {
		return fmt.Errorf("the snapshot has %d methods, below the floor of %d", len(surface.Methods), floor.methods)
	}
	rows, problems := readTSV(filepath.Join(root, apiCoverageRecordPath), 4)
	calls, files, callProblems, err := apiCoverageCalls(root)
	if err != nil {
		return err
	}
	problems = append(problems, callProblems...)
	if files == 0 {
		return fmt.Errorf("read no Go files under internal/: this gate would be looking at nothing")
	}
	requestable, err := apiCoverageRequestable(filepath.Join(root, "internal", "scopes"))
	if err != nil {
		return err
	}

	a := &apiCoverageAudit{
		methods:     surface.methodByID(),
		callsByID:   map[string][]apiCoverageCall{},
		requestable: requestable,
		phase:       apiCoveragePhase(filepath.Join(root, "CHANGELOG.md")),
		count:       map[string]int{},
		byPhase:     map[string]int{},
		seen:        map[string]string{},
		problems:    problems,
	}
	for _, c := range calls {
		a.callsByID[c.id] = append(a.callsByID[c.id], c)
	}
	for _, r := range rows {
		a.row(r)
	}
	outOfReach := a.unrecorded(surface.Methods)
	for _, c := range calls {
		a.call(c)
	}
	a.floors(floor, len(rows), len(calls))
	if err := problemsError(out, apiCoverageRecordPath, a.problems); err != nil {
		return err
	}
	var planned []string
	for _, p := range slices.Sorted(maps.Keys(a.byPhase)) {
		planned = append(planned, fmt.Sprintf("phase %s: %d", p, a.byPhase[p]))
	}
	_, _ = fmt.Fprintf(out, "api-coverage ok: %d methods (revision %s), %d used, %d planned (%s), %d out "+
		"(%d accept no scope this server requests); %d client calls in %d files; current phase %d\n",
		len(surface.Methods), surface.Revision, a.count["used"], a.count["planned"], strings.Join(planned, ", "),
		a.count["out"], outOfReach, len(calls), files, a.phase)
	return nil
}

// apiCoverageAudit is one run's inputs and what it has found so far.
type apiCoverageAudit struct {
	methods     map[string]discoveryMethod
	callsByID   map[string][]apiCoverageCall
	requestable map[string]bool
	phase       int
	count       map[string]int    // rows per state
	byPhase     map[string]int    // planned rows per phase
	seen        map[string]string // method id to its row's state
	problems    []string
}

func (a *apiCoverageAudit) addf(format string, args ...any) {
	a.problems = append(a.problems, fmt.Sprintf(format, args...))
}

// reachable reports whether a method accepts a scope this server requests.
func (a *apiCoverageAudit) reachable(m discoveryMethod) bool {
	return slices.ContainsFunc(m.Scopes, func(s string) bool { return a.requestable[s] })
}

// row checks one record row against the snapshot and the client.
func (a *apiCoverageAudit) row(r tsvRow) {
	id, state, ph, verdict := r.fields[0], r.fields[1], r.fields[2], r.fields[3]
	at := fmt.Sprintf("%s:%d", apiCoverageRecordPath, r.line)
	a.seen[id] = state
	m, exists := a.methods[id]
	if !exists {
		a.addf("%s: %s is not a published method; remove the row", at, id)
	}
	prefixes, ok := apiCoverageStates[state]
	if !ok {
		a.addf("%s: state %q is not one of used, planned, out", at, state)
		return
	}
	a.count[state]++
	if !slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(verdict, p) }) {
		a.addf("%s: a %s row's verdict starts with one of %s", at, state, strings.Join(prefixes, ", "))
	}
	switch state {
	case "planned":
		a.plannedRow(at, id, ph)
	case "used":
		if len(a.callsByID[id]) == 0 {
			a.addf("%s: %s is used and no gapi.Call names it", at, id)
		}
	case "out":
		if len(verdict) < apiCoverageMinReason {
			a.addf("%s: %s is out with a reason of %d characters; say why in at least %d",
				at, id, len(verdict), apiCoverageMinReason)
		}
	}
	if state != "planned" && ph != "-" {
		a.addf("%s: only a planned row names a phase; write -", at)
	}
	if exists && state != "out" && !a.reachable(m) {
		a.addf("%s: %s accepts none of the scopes internal/scopes requests, so it cannot be %s", at, id, state)
	}
}

// plannedRow checks a planned row's phase against the CHANGELOG, and
// that nothing calls it yet.
func (a *apiCoverageAudit) plannedRow(at, id, ph string) {
	n, err := strconv.Atoi(ph)
	switch {
	case err != nil || n < 0 || n > 4:
		a.addf("%s: a planned row names its phase, 0 to 4; got %q", at, ph)
	case n < a.phase:
		a.addf("%s: %s was planned for phase %d and the CHANGELOG says "+
			"phase %d has begun; build it or argue the verdict again", at, id, n, a.phase)
	default:
		a.byPhase[ph]++
	}
	if len(a.callsByID[id]) > 0 {
		a.addf("%s: %s is planned and %s calls it; mark it used", at, id, a.callsByID[id][0].at)
	}
}

// unrecorded reports each published method with no row, and returns
// how many accept no scope this server requests.
func (a *apiCoverageAudit) unrecorded(methods []discoveryMethod) int {
	outOfReach := 0
	for _, m := range methods {
		if _, ok := a.seen[m.ID]; !ok {
			a.addf("%s (%s %s) has no row in %s", m.ID, m.Verb, m.Path, apiCoverageRecordPath)
		}
		if !a.reachable(m) {
			outOfReach++
		}
	}
	return outOfReach
}

// call checks one client call against its row and the snapshot.
func (a *apiCoverageAudit) call(c apiCoverageCall) {
	m, ok := a.methods[c.id]
	if !ok {
		a.addf("%s: gapi.Call names %q, which is not a published method", c.at, c.id)
		return
	}
	switch state := a.seen[c.id]; state {
	case "used", "planned": // planned is reported above
	default:
		a.addf("%s: %s is called and its row says %q; a call needs a used row",
			c.at, c.id, apiCoverageStateName(state))
	}
	if c.verb != m.Verb {
		a.addf("%s: %s is %s in the snapshot and the call sends %s", c.at, c.id, m.Verb, c.verb)
	}
	if want := discoveryClientPath(m.Path); c.path != want {
		a.addf("%s: %s's path is %q under users/me and the call writes %q", c.at, c.id, want, c.path)
	}
}

// floors holds what the run read against the floors.
func (a *apiCoverageAudit) floors(floor apiCoverageLimits, rows, calls int) {
	if rows < floor.methods {
		a.addf("%d rows, below the floor of %d", rows, floor.methods)
	}
	if in := a.count["used"] + a.count["planned"]; in < floor.inScope {
		a.addf("%d methods used or planned, below the floor of %d; "+
			"writing one off moves the floor in the same change", in, floor.inScope)
	}
	if calls < floor.calls {
		a.addf("%d client calls, below the floor of %d", calls, floor.calls)
	}
}

func apiCoverageStateName(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

// apiCoverageCalls reads every gapi.Call composite literal under
// root/internal from non-test files: Call{...} inside package gapi,
// gapi.Call{...} elsewhere. ID and Path must be string literals and Method a literal
// or an http.MethodX constant, so the gate can read them.
func apiCoverageCalls(root string) (calls []apiCoverageCall, files int, problems []string, err error) {
	dir := filepath.Join(root, "internal")
	fset := token.NewFileSet()
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == dir {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		files++
		inGapi := file.Name.Name == "gapi"
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !apiCoverageIsCall(lit.Type, inGapi) || len(lit.Elts) == 0 {
				return true
			}
			pos := fset.Position(lit.Pos())
			rel, _ := filepath.Rel(root, pos.Filename)
			at := fmt.Sprintf("%s:%d", filepath.ToSlash(rel), pos.Line)
			c := apiCoverageCall{at: at}
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					problems = append(problems, at+": a gapi.Call literal without field names cannot be read")
					return true
				}
				key, _ := kv.Key.(*ast.Ident)
				if key == nil {
					continue
				}
				switch key.Name {
				case "ID":
					c.id, ok = apiCoverageString(kv.Value)
					if !ok {
						problems = append(problems, at+": ID is not a string literal")
					}
				case "Path":
					c.path, ok = apiCoverageString(kv.Value)
					if !ok {
						problems = append(problems, at+": Path is not a string literal")
					}
				case "Method":
					c.verb = apiCoverageVerb(kv.Value)
					if c.verb == "" {
						problems = append(problems, at+": Method is neither a string literal nor http.MethodX")
					}
				}
			}
			if c.id == "" {
				problems = append(problems, at+": a gapi.Call literal with no ID")
				return true
			}
			calls = append(calls, c)
			return true
		})
		return nil
	})
	return calls, files, problems, err
}

func apiCoverageIsCall(t ast.Expr, inGapi bool) bool {
	switch t := t.(type) {
	case *ast.Ident:
		return inGapi && t.Name == "Call"
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		return ok && x.Name == "gapi" && t.Sel.Name == "Call"
	}
	return false
}

func apiCoverageString(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

func apiCoverageVerb(e ast.Expr) string {
	if s, ok := apiCoverageString(e); ok {
		return strings.ToUpper(s)
	}
	if sel, ok := e.(*ast.SelectorExpr); ok {
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "http" && strings.HasPrefix(sel.Sel.Name, "Method") {
			return strings.ToUpper(strings.TrimPrefix(sel.Sel.Name, "Method"))
		}
	}
	return ""
}

// apiCoverageRequestable is every scope URL internal/scopes declares as
// a string constant: the scopes this server requests or treats as
// implied. A method accepting none of them cannot be used.
func apiCoverageRequestable(dir string) (map[string]bool, error) {
	files, err := parseGoDir(token.NewFileSet(), dir)
	if err != nil {
		return nil, fmt.Errorf("read internal/scopes: %w", err)
	}
	out := map[string]bool{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			if spec, ok := n.(*ast.ValueSpec); ok {
				for _, v := range spec.Values {
					if s, ok := apiCoverageString(v); ok && strings.HasPrefix(s, "https://") {
						out[s] = true
					}
				}
			}
			return true
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("internal/scopes declares no scope URL; has the scope set moved?")
	}
	return out, nil
}

// apiCoverageHeading is a released version heading in the CHANGELOG.
var apiCoverageHeading = regexp.MustCompile(`^## \[(\d+)\.(\d+)\.\d+\]`)

// apiCoveragePhase is the §16 phase under way, read from the newest
// released CHANGELOG heading: phase N ships as v0.(N+1).0 and phase 4 as
// v1.0.0. No released heading is phase 0.
func apiCoveragePhase(changelog string) int {
	f, err := os.Open(changelog) //nolint:gosec // a path this repository owns
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		m := apiCoverageHeading.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		if major >= 1 {
			return 5
		}
		return minor
	}
	return 0
}
