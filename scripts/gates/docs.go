package main

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// The docs gate checks references, not claims. Decision IDs, every
// citation of one, every link to a heading, the codemap, the paths the
// docs name and the live steps the code, docs and records name all have
// to resolve.
// Whether the cited text is true is for review and the live run.
//
// Evidence has no IDs. What was verified upstream is stated at the line
// that depends on it, with its source link, so an E- citation fails.

// docsGolangci is the file the docs gate reads besides the ones other
// gates name.
const docsGolangci = ".golangci.yml"

// docsStepTypes are the types the live driver's tables hold: a step is a
// tool call and a spike a question for Google. A comment cites either as
// a live step.
var docsStepTypes = []string{"step", "spike"}

// A decision ID is D- and two or more digits, zero-padded.
const docsDecisionPattern = "D-NN"

var (
	docsDecisionRef = regexp.MustCompile(`\bD-\d+\b`)
	// docsDecisionHeading finds an entry, with its title.
	docsDecisionHeading = regexp.MustCompile(`^### (D-\S*)(?:[ \t]+(.*))?$`)
	docsEvidenceRef     = regexp.MustCompile(`\bE-\d+\b`)
)

// docsDecisionNumber is the number in a canonical decision ID, or -1 when
// id is not one.
func docsDecisionNumber(id string) int {
	digits, ok := strings.CutPrefix(id, "D-")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || docsDecisionText(n) != id {
		return -1
	}
	return n
}

func docsDecisionText(n int) string { return fmt.Sprintf("D-%02d", n) }

var (
	// A section number names nothing a reader can follow unless it says
	// which document it numbers: an RFC named right before it, or a URL
	// it sits in.
	docsBareSection   = regexp.MustCompile(`§\s?\d+`)
	docsSectionSource = regexp.MustCompile(`RFC \d+ ?§|https?://[^\s)>\]]+`)
	// A pointer like client.go:238 or client.go:238:5 names the file.
	docsLineSuffix = regexp.MustCompile(`:\d+(?::\d+)?$`)
	// A comment cites live steps by their quoted names, listed with commas,
	// "and" or "or".
	docsLiveStepCite = regexp.MustCompile(`\blive steps? ((?:"[^"]*"(?:,? and |,? or |, )?)+)`)
	docsQuotedName   = regexp.MustCompile(`"([^"]*)"`)
	// YAML and TOML end a line with a comment after white space.
	docsTrailingComment = regexp.MustCompile(`\s#`)
	docsTOMLMultiline   = regexp.MustCompile(`'''|"""`)

	docsMDLink = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	// A footnote, [^label]:, is not a link definition, though its text may
	// hold links.
	docsMDLinkDef  = regexp.MustCompile(`^\s{0,3}\[[^\]^][^\]]*\]:\s*<?([^\s>]+)`)
	docsATXHeading = regexp.MustCompile(`^#{1,6}[ \t]+(.*?)(?:[ \t]+#+)?$`)
	// Go has no code spans or links for the path check to read, so a doc
	// reference in a comment or a string is found by its shape: a relative
	// path ending in .md, with an optional anchor. A match never starts
	// right after a slash or a dot; one inside a URL is docsInURL's to drop.
	docsGoDocRef = regexp.MustCompile(`(?:^|[^\w/.-])(?:\./)?((?:[\w.-]+/)*[\w.-]+\.md)(?:#([\p{L}\p{N}_-]+))?`)
	// The root documents are named in capitals. A lowercase name in a
	// comment is a file the code writes, which need not exist here.
	docsRootDocName = regexp.MustCompile(`^[A-Z][A-Z0-9_-]*\.md$`)
	// A root file or a source path is held even when its first segment is
	// gone. .txt and .json are left out: checksums.txt and manifest.json
	// name files in a release archive and a bundle.
	docsPathShape = regexp.MustCompile(`^(?:(?:cmd|internal|scripts|docs|testdata|packaging|\.github)/|[\w.-]+\.(?:md|ya?ml|toml|mod|sum)$|Makefile$|LICENSE$)`)
	// An entry runs to the next heading at its own level or above.
	docsEntryEnd = regexp.MustCompile(`^#{1,3} `)

	docsSeparatorRow = regexp.MustCompile(`^\|(\s*:?-+:?\s*\|)+$`)
	docsPackageName  = regexp.MustCompile(`^((?:internal|cmd)/[^/\s]+)/?$`)
	docsGatesCommand = regexp.MustCompile(`(?:^|\s|\./)scripts/gates\s+(\S+)`)
)

const docsSupersededBy = "superseded by "

var (
	docsIndexHeader = []string{"ID", "Decision", "Status", "Held by"}
	// The last status is a pattern; docsValidStatus reads it.
	docsStatuses    = []string{"accepted", "rejected", "deprecated", docsSupersededBy + docsDecisionPattern}
	docsEntryFields = []string{"Rejected", "Cost"}
)

// docsUnfollowable are the characters that mark a word as a glob or a
// placeholder.
const docsUnfollowable = "*?[]{}<>$…"

// docsFollowable reports whether a word can name something that exists:
// it holds none of docsUnfollowable and no "...", Go's package wildcard.
func docsFollowable(word string) bool {
	return !strings.ContainsAny(word, docsUnfollowable) && !strings.Contains(word, "...")
}

// docsFloors are the fewest live steps, links and code-span paths the
// gate may read.
type docsFloors struct{ liveSteps, links, spanPaths int }

// docsLimits are the repository's floors, each well under what it holds.
// A pass below its floor has stopped reading what it checks.
var docsLimits = docsFloors{liveSteps: 100, links: 15, spanPaths: 60}

func docs(out io.Writer, _ []string) error {
	return docsCheck(".", docsLimits, out)
}

// docsCheck runs every check over the checkout at root. A checkout git
// cannot list is a gate that could not run.
func docsCheck(root string, floors docsFloors, out io.Writer) error {
	files, err := gitFiles(root)
	if err != nil {
		return couldNotRun{err}
	}
	g := newDocsGate(root, files, floors)
	g.run()
	if err := problemsError(out, "the docs", g.problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "docs ok: %d decisions, %d live steps, %d links and %d code-span paths, in %d files\n",
		len(g.decisions), len(g.liveSteps), g.links, g.spanPaths, len(g.files))
	return nil
}

// run runs every check. Later checks need what earlier ones read: the
// test names and live steps from the Go pass, and the decisions the
// citations resolve to.
func (g *docsGate) run() {
	g.readGo()
	g.readVocabulary()
	g.readDecisions()
	g.checkCitations()
	g.checkCodemap()
}

// docsInTestdata reports whether a file is a fixture. Go ignores testdata
// directories, and the gate reads neither their code nor their prose.
func docsInTestdata(file string) bool {
	return slices.Contains(strings.Split(path.Dir(file), "/"), "testdata")
}

// docsGate is what the checks share: the checkout, what they read from
// it, and the problems they found.
type docsGate struct {
	root    string
	files   []string
	present map[string]bool
	dirs    map[string]bool

	problems []string
	reported map[string]bool

	proseOf  map[string][]string
	slugs    map[string]map[string]bool
	comments map[string][]docsSourceLine
	literals map[string][]docsSourceLine

	tests       map[string]bool
	makeTargets map[string]bool
	linters     map[string]bool
	// liveSteps is where the driver names each step.
	liveSteps map[string]token.Position

	// decisions stays nil when the architecture document cannot be read,
	// so a citation of one is not also reported as unknown.
	decisions map[string]*docsDecision

	// links and spanPaths count what the Markdown pass checked, each held
	// to its floor.
	links, spanPaths int
	floors           docsFloors
}

// docsSourceLine is one line of a Go comment or string literal.
type docsSourceLine struct {
	line int
	text string
}

func newDocsGate(root string, files []string, floors docsFloors) *docsGate {
	g := &docsGate{
		root:      root,
		files:     files,
		present:   map[string]bool{},
		dirs:      map[string]bool{},
		reported:  map[string]bool{},
		proseOf:   map[string][]string{},
		slugs:     map[string]map[string]bool{},
		comments:  map[string][]docsSourceLine{},
		literals:  map[string][]docsSourceLine{},
		tests:     map[string]bool{},
		liveSteps: map[string]token.Position{},
		floors:    floors,
	}
	for _, f := range files {
		g.present[f] = true
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			g.dirs[d] = true
		}
	}
	return g
}

// exists reports whether rel is a file or a directory of the checkout.
func (g *docsGate) exists(rel string) bool { return g.present[rel] || g.dirs[rel] }

// fail records a problem once. A line that cites one ID twice is one
// problem, not two.
func (g *docsGate) fail(format string, a ...any) {
	if p := fmt.Sprintf(format, a...); !g.reported[p] {
		g.reported[p] = true
		g.problems = append(g.problems, p)
	}
}

// at reports a problem at a line of a file, or at the file when line is 0.
func (g *docsGate) at(file string, line int, format string, a ...any) {
	where := file
	if line > 0 {
		where += ":" + strconv.Itoa(line)
	}
	g.fail("%s: %s", where, fmt.Sprintf(format, a...))
}

// malformed reports a decision ID not written with two or more digits,
// naming the ID it should be. A heading or index row is read twice, by its
// reader and the citation scan, and the one wording lets fail report it once.
func (g *docsGate) malformed(file string, line int, id string) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "D-"))
	if err != nil || n < 1 {
		n = 1
	}
	g.at(file, line, "%q is not written as a decision ID like %s", id, docsDecisionText(n))
}

// source is a checkout file's bytes as they are on disk.
func (g *docsGate) source(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(g.root, filepath.FromSlash(rel)))
}

func (g *docsGate) readFile(rel string) (string, bool) {
	if !g.present[rel] {
		g.fail("%s is missing", rel)
		return "", false
	}
	raw, err := g.source(rel)
	if err != nil {
		g.fail("gates cannot read %s: %v", rel, err)
		return "", false
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n"), true
}

// lines is a Markdown file's prose lines, read once. A file that cannot be
// read is tried again by the next caller, and fail drops the repeat.
func (g *docsGate) lines(rel string) ([]string, bool) {
	if l, ok := g.proseOf[rel]; ok {
		return l, true
	}
	raw, ok := g.readFile(rel)
	if !ok {
		return nil, false
	}
	l := docsProse(raw)
	g.proseOf[rel] = l
	return l, true
}

// docsHeadingLevel is an ATX heading's level, the run of #s before the
// blank that ends it, or 0 for a line that is not a heading
// (https://spec.commonmark.org/0.31.2/#atx-headings).
func docsHeadingLevel(line string) int {
	if !docsATXHeading.MatchString(line) {
		return 0
	}
	return len(line) - len(strings.TrimLeft(line, "#"))
}

// docsEndsSection reports whether a line is a heading that ends a section
// of the given level: one at that level or above it.
func docsEndsSection(line string, level int) bool {
	l := docsHeadingLevel(line)
	return l > 0 && l <= level
}

// docsProse is a Markdown document's lines with fenced code blanked, so
// an example in a fence is neither a heading nor a citation. Blanking
// rather than dropping keeps the line numbers.
func docsProse(doc string) []string {
	lines := strings.Split(doc, "\n")
	var fence docsFence
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
		if fence.code(lines[i]) {
			lines[i] = ""
		}
	}
	return lines
}

// docsFence follows a Markdown document's fenced code a line at a time.
// Its zero value is outside any fence.
type docsFence struct{ open string }

// code reports whether a line opens, sits inside or closes fenced code. A
// fence closes at a line of only its own character, at least as many
// (https://spec.commonmark.org/0.31.2/#fenced-code-blocks).
func (f *docsFence) code(line string) bool {
	unindented := strings.TrimLeft(strings.TrimRight(line, " \t\r"), " ")
	marker := docsFenceMarker(unindented)
	switch {
	case f.open == "" && marker != "":
		f.open = marker
	case f.open != "":
		if strings.HasPrefix(marker, f.open) && strings.TrimLeft(unindented, marker[:1]) == "" {
			f.open = ""
		}
	default:
		return false
	}
	return true
}

// docsFenceMarker is the run of three or more backticks or tildes a line
// opens a fence with, or empty. A backtick fence's info string holds no
// backtick, so ```x``` is a code span
// (https://spec.commonmark.org/0.31.2/#fenced-code-blocks).
func docsFenceMarker(line string) string {
	for _, c := range []string{"`", "~"} {
		n := len(line) - len(strings.TrimLeft(line, c))
		if n >= 3 && (c == "~" || !strings.Contains(line[n:], "`")) {
			return line[:n]
		}
	}
	return ""
}

// docsCodeSpans is the text of each code span on a line.
func docsCodeSpans(line string) []string {
	spans := codeSpan.FindAllStringSubmatch(line, -1)
	out := make([]string, 0, len(spans))
	for _, m := range spans {
		out = append(out, m[1])
	}
	return out
}

// headingSlugs is every anchor GitHub gives a Markdown file's headings.
func (g *docsGate) headingSlugs(rel string) map[string]bool {
	if s, ok := g.slugs[rel]; ok {
		return s
	}
	lines, _ := g.lines(rel)
	s := map[string]bool{}
	for _, line := range lines {
		m := docsATXHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// A repeated heading gets -1, -2 and so on, skipping any suffix a
		// real heading already took, the way GitHub numbers them.
		base := docsGitHubSlug(m[1])
		slug := base
		for n := 1; s[slug]; n++ {
			slug = base + "-" + strconv.Itoa(n)
		}
		s[slug] = true
	}
	g.slugs[rel] = s
	return s
}

// docsGitHubSlug is the anchor GitHub gives a heading: lowercase, with
// everything but letters, digits, spaces, hyphens and underscores dropped,
// and spaces turned into hyphens.
func docsGitHubSlug(heading string) string {
	heading = docsMDLink.ReplaceAllString(heading, "")
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// readGo collects every comment line, every test and fuzz function name,
// and the string literals outside tests. A string is read for doc paths only, as
// a message can send a user to a doc; a test's strings are fixtures. It
// also reads the live steps from the driver's own files.
func (g *docsGate) readGo() {
	fset := token.NewFileSet()
	read := 0
	var driver []*ast.File
	for _, file := range g.files {
		if !strings.HasSuffix(file, ".go") || docsInTestdata(file) {
			continue
		}
		src, err := g.source(file)
		if err != nil {
			g.fail("gates cannot read %s: %v", file, err)
			continue
		}
		f, err := parser.ParseFile(fset, file, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			g.fail("gates cannot parse %s: %v", file, err)
			continue
		}
		read++
		for _, group := range f.Comments {
			for _, c := range group.List {
				start := fset.Position(c.Slash).Line
				for i, text := range strings.Split(c.Text, "\n") {
					g.comments[file] = append(g.comments[file], docsSourceLine{start + i, text})
				}
			}
		}
		if !strings.HasSuffix(file, "_test.go") {
			// A step a test builds is a fake, not one the live run takes.
			if path.Dir(file) == liveCoverDriver {
				driver = append(driver, f)
			}
			g.literals[file] = docsStringLines(fset, f)
			continue
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && docsRunsUnderGoTest(fn.Name.Name) {
				g.tests[fn.Name.Name] = true
			}
		}
	}
	if read == 0 {
		g.fail("no .go files read: this gate would be looking at nothing")
	}
	g.readLiveSteps(fset, driver)
	if n := len(g.liveSteps); n < g.floors.liveSteps {
		g.at(liveCoverDriver, 0, "%d live steps read, fewer than %d: the step tables are not being read", n, g.floors.liveSteps)
	}
}

// readLiveSteps collects the name of each entry in the driver's step and
// spike tables. The compiler makes every entry a step or a spike, so an
// entry is a literal with its name keyed, a call to a top-level function
// that takes a name, or a method called on either. Two entries of one
// name would make a citation of it ambiguous, so the second is refused.
func (g *docsGate) readLiveSteps(fset *token.FileSet, files []*ast.File) {
	makers := map[string]int{}
	for _, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				if i := docsNamePos(fn.Type.Params); i >= 0 {
					makers[fn.Name.Name] = i
				}
			}
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			table, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			array, ok := table.Type.(*ast.ArrayType)
			if !ok || !slices.ContainsFunc(docsStepTypes, func(t string) bool { return docsIsIdent(array.Elt, t) }) {
				return true
			}
			for _, elt := range table.Elts {
				name, ok := docsStepName(elt, makers)
				if !ok {
					continue
				}
				at := fset.Position(elt.Pos())
				if first, seen := g.liveSteps[name]; seen {
					g.at(at.Filename, at.Line, "names the live step %q, which %s:%d names too; a step's name is its own",
						name, first.Filename, first.Line)
					continue
				}
				g.liveSteps[name] = at
			}
			return false
		})
	}
}

// docsNamePos is the position of the string parameter called name, or -1.
// An unnamed parameter holds a position too.
func docsNamePos(list *ast.FieldList) int {
	i := 0
	for _, field := range list.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, name := range field.Names {
			if name.Name == "name" && docsIsIdent(field.Type, "string") {
				return i
			}
			i++
		}
	}
	return -1
}

// docsStepName is a table entry's name: the field keyed name, the name
// argument of a call to a maker, or the name of the entry a method is
// called on.
func docsStepName(elt ast.Expr, makers map[string]int) (string, bool) {
	var value ast.Expr
	switch e := elt.(type) {
	case *ast.CompositeLit:
		for _, field := range e.Elts {
			if kv, ok := field.(*ast.KeyValueExpr); ok && docsIsIdent(kv.Key, "name") {
				value = kv.Value
			}
		}
	case *ast.CallExpr:
		switch fn := e.Fun.(type) {
		case *ast.Ident:
			if i, ok := makers[fn.Name]; ok && i < len(e.Args) {
				value = e.Args[i]
			}
		case *ast.SelectorExpr:
			return docsStepName(fn.X, makers)
		}
	}
	return apiCoverageString(value)
}

// docsRunsUnderGoTest reports whether a _test.go function is one `go
// test` runs, and so can hold a decision: a test, or a fuzz target, whose
// seed corpus runs as a test
// (https://go.dev/doc/security/fuzz/#running-fuzz-tests). A benchmark
// runs only under -bench, and an example with no output comment is
// compiled but not executed (`go help testfunc`), so neither holds
// anything by its name.
func docsRunsUnderGoTest(name string) bool {
	return strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Fuzz")
}

func docsIsIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// docsStringLines is the value of every string literal in a file, by
// source line. Only a raw string spans lines; an escaped newline stays on
// one.
func docsStringLines(fset *token.FileSet, f *ast.File) []docsSourceLine {
	var out []docsSourceLine
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		parts := []string{value}
		if lit.Value[0] == '`' {
			parts = strings.Split(value, "\n")
		}
		start := fset.Position(lit.Pos()).Line
		for i, text := range parts {
			out = append(out, docsSourceLine{start + i, text})
		}
		return true
	})
	return out
}

// readVocabulary reads the Makefile targets and enabled linters a
// "Held by" item may name.
func (g *docsGate) readVocabulary() {
	g.makeTargets = map[string]bool{}
	if mk, ok := g.readFile(parityMakefile); ok {
		for target := range parseMakefile(mk).targets {
			g.makeTargets[target] = true
		}
		if len(g.makeTargets) == 0 {
			g.at(parityMakefile, 0, "no targets read: a Held by naming one would never match")
		}
	}
	if cfg, ok := g.readFile(docsGolangci); ok {
		var parsed struct {
			Linters struct {
				Enable []string `yaml:"enable"`
			} `yaml:"linters"`
		}
		if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
			g.fail("gates cannot parse %s: %v", docsGolangci, err)
			return
		}
		g.linters = map[string]bool{}
		for _, name := range parsed.Linters.Enable {
			g.linters[name] = true
		}
		if len(g.linters) == 0 {
			g.at(docsGolangci, 0, "no enabled linters read: a Held by naming one would never match")
		}
	}
}

// holds reports whether a "Held by" item names something that exists.
// live is the Makefile's target, so it holds only while `make live` does.
func (g *docsGate) holds(item string) bool {
	_, gate := commands[item]
	return item == "review" || gate || g.makeTargets[item] || g.linters[item] || g.tests[item]
}

// docsListItems splits a comma-separated cell. A closing period and code
// quotes around an item are not part of it.
func docsListItems(s string) []string {
	parts := strings.Split(strings.TrimSuffix(strings.TrimSpace(s), "."), ",")
	out := make([]string, 0, len(parts))
	for _, item := range parts {
		if item = strings.Trim(strings.TrimSpace(item), "`"); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// docsOrList is items the way a sentence lists them: "a, b or c".
func docsOrList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// docsHasField reports whether the entry headed by lines[heading] has a
// "name:" line.
func docsHasField(lines []string, heading int, name string) bool {
	for j := heading + 1; j < len(lines) && !docsEntryEnd.MatchString(lines[j]); j++ {
		if strings.HasPrefix(strings.TrimSpace(lines[j]), name+":") {
			return true
		}
	}
	return false
}

// docsDecision is one D-ID as its index row and its entry give it. A zero
// line means that half is missing: line is the row's, entryLine the
// entry's heading.
type docsDecision struct {
	id                    string
	line                  int
	title, status, heldBy string
	entryLine             int
	entryTitle            string
}

// lapsed reports whether a decision no longer holds. Only the architecture
// document may cite it.
func (d *docsDecision) lapsed() bool {
	return d.status == "deprecated" || strings.HasPrefix(d.status, docsSupersededBy)
}

// docsValidStatus reports whether s is one of docsStatuses. Whether a
// successor exists is the citation check's, which reads the index too.
func docsValidStatus(s string) bool {
	if successor, ok := strings.CutPrefix(s, docsSupersededBy); ok {
		return docsDecisionNumber(successor) > 0
	}
	return slices.Contains(docsStatuses, s)
}

func (g *docsGate) decision(id string) *docsDecision {
	if g.decisions[id] == nil {
		g.decisions[id] = &docsDecision{id: id}
	}
	return g.decisions[id]
}

func (g *docsGate) readDecisions() {
	lines, ok := g.lines(architectureDoc)
	if !ok {
		return
	}
	g.decisions = map[string]*docsDecision{}
	g.readDecisionIndex(lines)
	g.readDecisionEntries(lines)
	rows := 0
	for _, d := range g.decisions {
		if d.line != 0 {
			rows++
		}
	}
	if rows == 0 {
		g.at(architectureDoc, 0, "no decisions in the index: this gate would be looking at nothing")
	}

	last := len(lines) - 1
	for last >= 0 && lines[last] == "" {
		last--
	}
	next := 0
	if last >= 0 {
		if id, ok := strings.CutPrefix(lines[last], "Next ID: "); ok {
			next = max(docsDecisionNumber(id), 0)
		}
	}
	if next == 0 {
		g.at(architectureDoc, last+1, `the last line is not "Next ID: %s"`, docsDecisionPattern)
	}
	ids := slices.Collect(maps.Keys(g.decisions))
	slices.SortFunc(ids, func(a, b string) int { return cmp.Compare(docsDecisionNumber(a), docsDecisionNumber(b)) })
	for _, id := range ids {
		g.checkDecision(g.decisions[id])
	}
	if next > 0 {
		g.checkNextID(ids, next, last+1)
	}
}

// checkNextID holds the decisions to the Next ID line: each ID is below
// it, and each number below it has a decision, so no ID returns.
func (g *docsGate) checkNextID(ids []string, next, nextLine int) {
	for _, id := range ids {
		if docsDecisionNumber(id) >= next {
			d := g.decisions[id]
			g.at(architectureDoc, cmp.Or(d.line, d.entryLine), "%s is not below Next ID %s", id, docsDecisionText(next))
		}
	}
	for n := 1; n < next; n++ {
		if id := docsDecisionText(n); g.decisions[id] == nil {
			g.at(architectureDoc, nextLine, "%s is below Next ID %s and is missing; an ID is never deleted", id, docsDecisionText(next))
		}
	}
}

func (g *docsGate) readDecisionIndex(lines []string) {
	start := slices.Index(lines, "## Decisions")
	if start < 0 {
		g.at(architectureDoc, 0, `no "## Decisions" heading`)
		return
	}
	i := start + 1
	for i < len(lines) && lines[i] == "" {
		i++
	}
	if i+1 >= len(lines) || !strings.HasPrefix(lines[i], "|") || !docsSeparatorRow.MatchString(lines[i+1]) {
		g.at(architectureDoc, start+1, `the decision index is not a table directly under "## Decisions"`)
		return
	}
	if header := docsTableCells(lines[i]); !slices.Equal(header, docsIndexHeader) {
		g.at(architectureDoc, i+1, "the decision index has the columns %q, want %q", header, docsIndexHeader)
		return
	}
	for i += 2; i < len(lines) && strings.HasPrefix(lines[i], "|"); i++ {
		g.readIndexRow(i+1, docsTableCells(lines[i]))
	}
}

// docsTableCells splits a table row at each pipe a backslash does not
// escape, even inside a code span, and unescapes the rest
// (https://github.github.com/gfm/#tables-extension-).
func docsTableCells(row string) []string {
	row = strings.TrimPrefix(strings.TrimSpace(row), "|")
	if !strings.HasSuffix(row, `\|`) {
		row = strings.TrimSuffix(row, "|")
	}
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(row); i++ {
		switch {
		case strings.HasPrefix(row[i:], `\|`):
			cell.WriteByte('|')
			i++
		case row[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(row[i])
		}
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

func (g *docsGate) readIndexRow(line int, row []string) {
	if len(row) != len(docsIndexHeader) {
		g.at(architectureDoc, line, "an index row has %d cells, want %d", len(row), len(docsIndexHeader))
		return
	}
	id := row[0]
	if docsDecisionNumber(id) < 0 {
		g.malformed(architectureDoc, line, id)
		return
	}
	d := g.decision(id)
	if d.line != 0 {
		g.at(architectureDoc, line, "%s is in the index twice", id)
		return
	}
	d.line, d.title, d.status, d.heldBy = line, row[1], row[2], row[3]
}

func (g *docsGate) readDecisionEntries(lines []string) {
	for i, line := range lines {
		m := docsDecisionHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id := m[1]
		if docsDecisionNumber(id) < 0 {
			g.malformed(architectureDoc, i+1, id)
			continue
		}
		d := g.decision(id)
		if d.entryLine != 0 {
			g.at(architectureDoc, i+1, "%s has two entries", id)
			continue
		}
		d.entryLine, d.entryTitle = i+1, m[2]
		for _, name := range docsEntryFields {
			if !docsHasField(lines, i, name) {
				g.at(architectureDoc, i+1, "%s has no %q line", id, name+":")
			}
		}
	}
}

func (g *docsGate) checkDecision(d *docsDecision) {
	switch {
	case d.line == 0:
		g.at(architectureDoc, d.entryLine, "%s has no index row", d.id)
		return
	case d.entryLine == 0:
		g.at(architectureDoc, d.line, "%s has no entry", d.id)
	case d.entryTitle != d.title:
		g.at(architectureDoc, d.entryLine, "%s's entry is titled %q and its index row %q", d.id, d.entryTitle, d.title)
	}
	if !docsValidStatus(d.status) {
		g.at(architectureDoc, d.line, "%s has status %q, want %s", d.id, d.status, docsOrList(docsStatuses))
	}
	switch holders := docsListItems(d.heldBy); {
	case slices.Equal(holders, []string{"—"}):
		if d.status == "accepted" {
			g.at(architectureDoc, d.line, "%s is accepted and held by —; an accepted decision names what holds it", d.id)
		}
	case len(holders) == 0:
		g.at(architectureDoc, d.line, "%s is held by nothing", d.id)
	default:
		for _, h := range holders {
			if !g.holds(h) {
				g.at(architectureDoc, d.line, "%s is held by %q, which is not review, a make target, "+
					"a gates command, an enabled linter or a test or fuzz function", d.id, h)
			}
		}
	}
}

// docsIsTrailer reports whether a line is the decisions' Next ID line,
// which names an ID that does not exist.
func docsIsTrailer(file, line string) bool {
	return file == architectureDoc && strings.HasPrefix(line, "Next ID:")
}

// scanned is the range of a Markdown file's lines the citation scan
// reads. A changelog's released sections were true when written; its
// [Unreleased] section becomes a release note, and the scan reads the
// lines release-notes lifts, from the file as it is.
func (g *docsGate) scanned(file string, lines []string) (from, to int) {
	if file != changelogFile {
		return 0, len(lines)
	}
	raw, _ := g.readFile(file)
	_, from, to = changelogSection(raw, "Unreleased")
	return from, to
}

func (g *docsGate) checkCitations() {
	// The agent file is one of the documents whose paths this holds.
	if !g.present[checklistDoc] {
		g.fail("%s is missing", checklistDoc)
	}
	for _, file := range g.files {
		switch {
		case docsIsRecord(file):
			// A record's reasons cite decisions and live steps, as a
			// comment does.
			if raw, ok := g.readFile(file); ok {
				var rows []docsSourceLine
				for i, line := range strings.Split(raw, "\n") {
					rows = append(rows, docsSourceLine{i + 1, line})
				}
				g.checkComments(file, rows)
			}
		case docsInTestdata(file):
		case strings.HasSuffix(file, ".go"):
			g.checkComments(file, g.comments[file])
			for _, s := range g.literals[file] {
				g.checkGoDocRefs(file, s.line, s.text, false)
			}
		case strings.HasSuffix(file, ".md"):
			g.checkMarkdown(file)
		case docsIsConfigFile(file):
			if raw, ok := g.readFile(file); ok {
				g.checkComments(file, docsConfigComments(file, raw))
			}
		}
	}
	if g.links < g.floors.links {
		g.fail("%d Markdown links checked, fewer than %d: the links are not being read", g.links, g.floors.links)
	}
	if g.spanPaths < g.floors.spanPaths {
		g.fail("%d code-span paths checked, fewer than %d: the code spans are not being read", g.spanPaths, g.floors.spanPaths)
	}
}

// checkComments checks each comment line, then the live steps the lines
// name, since a name can wrap from one line to the next.
func (g *docsGate) checkComments(file string, comments []docsSourceLine) {
	for _, c := range comments {
		g.checkComment(file, c.line, c.text)
	}
	g.checkLiveSteps(file, comments)
}

// checkComment resolves what a comment line cites: decision IDs, section
// numbers and docs.
func (g *docsGate) checkComment(file string, n int, text string) {
	g.checkLine(file, n, text)
	g.checkGoDocRefs(file, n, text, true)
}

// docsIsRecord reports whether a file is a hand-written TSV record: the
// API coverage and field verdicts, and the live driver's waivers. Each is
// read whole although it sits in testdata, since nothing generates it;
// any other table there is a fixture.
func docsIsRecord(file string) bool {
	return file == apiCoverageRecordPath || file == apiFieldsRecordPath || file == liveCoverWaivers
}

// docsIsConfigFile reports whether a file holds # comments the citation
// scan reads as it reads a Go comment.
func docsIsConfigFile(file string) bool {
	base := path.Base(file)
	return base == "Makefile" || base == ".gitignore" || docsHasTrailingComments(file)
}

// docsHasTrailingComments reports whether a file's format ends a line
// with a comment, as YAML and TOML do.
func docsHasTrailingComments(file string) bool {
	return strings.HasSuffix(file, ".yml") || strings.HasSuffix(file, ".yaml") || strings.HasSuffix(file, ".toml")
}

// docsConfigComments is each # comment in a config file, by line: a line
// that starts with #, and in YAML or TOML the end of a line after white
// space and #. A " #" in a one-line quoted string is read too, which can
// only add a check; a TOML multi-line string is left out, as its # is
// text.
func docsConfigComments(file, raw string) []docsSourceLine {
	var out []docsSourceLine
	trailing := docsHasTrailingComments(file)
	toml, open := strings.HasSuffix(file, ".toml"), ""
	for i, line := range strings.Split(raw, "\n") {
		if toml {
			line, open = docsOutsideTOMLStrings(line, open)
		}
		if text := strings.TrimLeft(line, " \t"); strings.HasPrefix(text, "#") {
			out = append(out, docsSourceLine{i + 1, text})
		} else if at := docsTrailingComment.FindStringIndex(line); trailing && at != nil {
			out = append(out, docsSourceLine{i + 1, line[at[1]-1:]})
		}
	}
	return out
}

// docsOutsideTOMLStrings is a TOML line less its multi-line strings, each
// between three single or three double quotes
// (https://toml.io/en/v1.0.0#string). open is the delimiter of the string
// the line starts in, and the result's that of the one it leaves open. A
// delimiter after a # is in a comment and opens nothing.
func docsOutsideTOMLStrings(line, open string) (string, string) {
	var out strings.Builder
	for {
		if open != "" {
			end := strings.Index(line, open)
			if end < 0 {
				return out.String(), open
			}
			line = line[end+len(open):]
		}
		start := docsTOMLMultiline.FindStringIndex(line)
		if start == nil || strings.Contains(line[:start[0]], "#") {
			out.WriteString(line)
			return out.String(), ""
		}
		out.WriteString(line[:start[0]])
		line, open = line[start[1]:], line[start[0]:start[1]]
	}
}

// checkLiveSteps holds each live step a file's comments, prose or record
// rows name to one the live driver defines. A name wraps across lines, so
// adjacent lines are read as one text. A driver read below its floor holds
// nothing to it.
func (g *docsGate) checkLiveSteps(file string, lines []docsSourceLine) {
	if len(g.liveSteps) < g.floors.liveSteps {
		return
	}
	for start := 0; start < len(lines); {
		end := start + 1
		for end < len(lines) && lines[end].line == lines[end-1].line+1 {
			end++
		}
		g.checkLiveStepRun(file, lines[start:end])
		start = end
	}
}

// checkLiveStepRun checks one run of adjacent lines, reporting an unknown
// name at the line its opening quote is on.
func (g *docsGate) checkLiveStepRun(file string, run []docsSourceLine) {
	var text strings.Builder
	starts := make([]int, len(run))
	for i, c := range run {
		starts[i] = text.Len()
		// A comment's marker, // or #, is not text a name wraps across.
		text.WriteString(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(c.text, "//"), "#")))
		text.WriteByte(' ')
	}
	joined := text.String()
	if !strings.Contains(joined, "live step") {
		return
	}
	for _, cite := range docsLiveStepCite.FindAllStringSubmatchIndex(joined, -1) {
		for _, q := range docsQuotedName.FindAllStringSubmatchIndex(joined[cite[2]:cite[3]], -1) {
			at := cite[2] + q[0]
			name := strings.Join(strings.Fields(joined[cite[2]+q[2]:cite[2]+q[3]]), " ")
			// A placeholder such as "<name>" shows the form; it names no step.
			if _, ok := g.liveSteps[name]; !ok && docsFollowable(name) {
				i, _ := slices.BinarySearch(starts, at+1)
				g.at(file, run[i-1].line, "names the live step %q, which %s does not define", name, liveCoverDriver)
			}
		}
	}
}

func (g *docsGate) checkMarkdown(file string) {
	lines, ok := g.lines(file)
	if !ok {
		return
	}
	from, to := g.scanned(file, lines)
	// A paragraph can cite a live step across a line break, as a comment
	// can, so the prose is checked in runs of adjacent lines.
	var text []docsSourceLine
	removed := false
	for i := from; i < to; i++ {
		line := lines[i]
		// A code span or a live step under [Unreleased]'s Removed heading
		// names what the release takes away, so it need not exist. A deeper
		// heading sits inside that section and leaves it as it is.
		if file == changelogFile {
			if m := docsATXHeading.FindStringSubmatch(line); m != nil {
				switch docsHeadingLevel(line) {
				case 2:
					removed = false
				case 3:
					removed = strings.TrimSpace(m[1]) == "Removed"
				}
			}
		}
		if line != "" && !removed {
			text = append(text, docsSourceLine{i + 1, line})
		}
		if docsIsTrailer(file, line) {
			continue
		}
		g.checkLine(file, i+1, line)
		g.checkLinks(file, i+1, line)
		if !removed && strings.Contains(line, "`") {
			for _, span := range docsCodeSpans(line) {
				g.checkSpan(file, i+1, span)
			}
		}
	}
	g.checkLiveSteps(file, text)
}

// checkLine resolves the IDs and section numbers a line cites. Each regex
// runs only on a line holding the literal it needs.
func (g *docsGate) checkLine(file string, n int, text string) {
	if strings.Contains(text, "D-") {
		for _, id := range docsDecisionRef.FindAllString(text, -1) {
			g.citeDecision(file, n, id)
		}
	}
	if strings.Contains(text, "E-") {
		for _, id := range docsEvidenceRef.FindAllString(text, -1) {
			g.at(file, n, "cites %s; evidence has no IDs: state the behavior at the line that depends on it, with its source link", id)
		}
	}
	if !strings.Contains(text, "§") {
		return
	}
	sources := docsSectionSource.FindAllStringIndex(text, -1)
	for _, at := range docsBareSection.FindAllStringIndex(text, -1) {
		if !slices.ContainsFunc(sources, func(s []int) bool { return s[0] <= at[0] && at[0] < s[1] }) {
			g.at(file, n, "cites a section by number (%s); cite a D-ID, docs/<file>.md#<slug> or a source link", text[at[0]:at[1]])
			return
		}
	}
}

func (g *docsGate) citeDecision(file string, n int, id string) {
	d := g.decisions[id]
	switch {
	case docsDecisionNumber(id) < 0:
		g.malformed(file, n, id)
	case g.decisions == nil:
	case d == nil:
		g.at(file, n, "cites %s, which is not a decision", id)
	case file != architectureDoc && d.lapsed():
		g.at(file, n, "cites %s, which is %s", id, d.status)
	}
}

// checkGoDocRefs resolves the docs a line of Go names. A string names one
// only by docs/ path or anchor, from the root: a bare file name in a
// string is data. A path inside a URL names nothing here.
func (g *docsGate) checkGoDocRefs(file string, n int, text string, inComment bool) {
	if !strings.Contains(text, ".md") {
		return
	}
	for _, m := range docsGoDocRef.FindAllStringSubmatchIndex(text, -1) {
		if docsInURL(text, m[2]) {
			continue
		}
		target, slug := text[m[2]:m[3]], ""
		if m[4] >= 0 {
			slug = text[m[4]:m[5]]
		}
		switch {
		case inComment:
			if resolved, ok := g.commentDoc(file, target, slug); ok {
				g.checkTarget(file, n, "cites", resolved, slug)
			}
		case strings.HasPrefix(target, "docs/") || slug != "":
			g.checkTarget(file, n, "names", target, slug)
		}
	}
}

// docsInURL reports whether text[at:] sits inside a URL or a query
// string: the word it is in has :// or ? before it.
func docsInURL(text string, at int) bool {
	word := text[strings.LastIndexFunc(text[:at], unicode.IsSpace)+1 : at]
	return strings.Contains(word, "://") || strings.Contains(word, "?")
}

// commentDoc is the file a .md path in a comment names, read from the
// comment's directory first and then from the root, as a link is. A
// missing file is still a reference when the path has an anchor, starts
// at something in either place or has a path's shape, or is a root
// document's name, in capitals. A path out of the checkout never is.
func (g *docsGate) commentDoc(file, target, slug string) (string, bool) {
	dir := path.Dir(file)
	local := path.Join(dir, target)
	first, _, nested := strings.Cut(target, "/")
	switch {
	case local == ".." || strings.HasPrefix(local, "../"):
		return "", false
	case g.exists(local):
		return local, true
	case g.exists(target):
		return target, true
	case nested && g.exists(path.Join(dir, first)):
		return local, true
	case slug != "":
		return target, true
	case !nested:
		return target, docsRootDocName.MatchString(target)
	}
	return target, g.exists(first) || docsPathShape.MatchString(target)
}

// checkLinks resolves a Markdown link relative to the file it is in. A
// link written in a code span is an example, not a link.
func (g *docsGate) checkLinks(file string, n int, text string) {
	if !strings.Contains(text, "](") && !strings.Contains(text, "]:") {
		return
	}
	text = codeSpan.ReplaceAllStringFunc(text, func(span string) string { return strings.Repeat(" ", len(span)) })
	if m := docsMDLinkDef.FindStringSubmatch(text); m != nil {
		g.checkLinkTarget(file, n, m[1])
		return
	}
	for _, m := range docsMDLink.FindAllStringSubmatch(text, -1) {
		g.checkLinkTarget(file, n, m[1])
	}
}

func (g *docsGate) checkLinkTarget(file string, n int, target string) {
	p, slug, _ := strings.Cut(target, "#")
	if strings.Contains(p, ":") {
		return // A URL, which is not this gate's to fetch.
	}
	resolved := path.Join(path.Dir(file), p)
	switch {
	case p == "":
		resolved = file
	case strings.HasPrefix(p, "/"):
		resolved = strings.TrimPrefix(path.Clean(p), "/")
	case resolved == ".." || strings.HasPrefix(resolved, "../"):
		return // Out of the checkout, as GitHub's ../../issues/51 is: nothing here to check.
	}
	g.links++
	g.checkTarget(file, n, "links to", resolved, slug)
}

// checkTarget holds a reference to a file or directory of the checkout,
// and its slug to a heading when it names a Markdown file.
func (g *docsGate) checkTarget(file string, n int, verb, target, slug string) {
	if !g.exists(target) {
		g.at(file, n, "%s %s, which does not exist", verb, target)
		return
	}
	g.checkSlug(file, n, verb, target, slug)
}

func (g *docsGate) checkSlug(file string, n int, verb, target, slug string) {
	if slug != "" && strings.HasSuffix(target, ".md") && !g.headingSlugs(target)[slug] {
		g.at(file, n, "%s %s#%s, and %s has no heading with that slug", verb, target, slug, target)
	}
}

// checkSpan holds the make targets, gates commands and repository paths a
// code span names to what exists. A placeholder such as <name> names
// none.
func (g *docsGate) checkSpan(file string, n int, span string) {
	fields := strings.Fields(span)
	if len(fields) > 1 && fields[0] == "make" {
		for _, target := range fields[1:] {
			if !strings.HasPrefix(target, "-") && !strings.Contains(target, "=") &&
				docsFollowable(target) && !g.makeTargets[target] {
				g.at(file, n, "names make %s, which is not a Makefile target", target)
			}
		}
	}
	if m := docsGatesCommand.FindStringSubmatch(span); m != nil && docsFollowable(m[1]) {
		if _, ok := commands[m[1]]; !ok {
			g.at(file, n, "names scripts/gates %s, which is not a gates command", m[1])
		}
	}
	for _, f := range fields {
		g.checkPath(file, n, f)
	}
}

// checkPath holds a word to the tree when it starts at a top-level entry
// of the checkout, or has a path's shape. Anything else is a name, not a
// path: a package, a function, a resource id. A glob or a placeholder is
// not followable. Sentence punctuation after a word and a :line pointer
// are not part of the path.
func (g *docsGate) checkPath(file string, n int, word string) {
	if !docsFollowable(word) {
		return
	}
	word = strings.TrimRight(word, ".,;)")
	p, slug, _ := strings.Cut(strings.TrimPrefix(word, "./"), "#")
	if p = docsLineSuffix.ReplaceAllString(p, ""); p == "" {
		return
	}
	if first, _, _ := strings.Cut(p, "/"); !g.exists(first) && !docsPathShape.MatchString(p) {
		return
	}
	g.spanPaths++
	clean := strings.TrimSuffix(p, "/")
	target, ok := g.spanTarget(file, clean)
	switch {
	case !ok && !g.dirs[docsGoPackage(clean)]:
		g.at(file, n, "names %s, which does not exist", word)
	case !ok:
		// A Go name in a package that exists.
	case strings.HasSuffix(p, "/") && g.present[target]:
		g.at(file, n, "names %s, which is not a directory", word)
	default:
		g.checkSlug(file, n, "names", target, slug)
	}
}

// spanTarget is the file or directory a code span's path names. A bare
// name is read as a link is, from the naming file's directory, then from
// the root, then as a workflow named the way a person says it: ci.yml.
func (g *docsGate) spanTarget(file, p string) (string, bool) {
	tries := []string{p}
	if !strings.Contains(p, "/") {
		tries = []string{path.Join(path.Dir(file), p), p, path.Join(workflowDir, p)}
	}
	for _, t := range tries {
		if g.exists(t) {
			return t, true
		}
	}
	return "", false
}

// docsGoPackage is the package directory of a Go name written
// pkg/path.Ident or pkg/path.Type.Method: everything before the first dot
// after the last slash, or empty. The segment after that dot has to be a
// Go identifier and not a file extension, so a missing
// internal/evals/approver.go is not excused by its directory.
func docsGoPackage(p string) string {
	slash := strings.LastIndex(p, "/")
	dot := strings.Index(p[slash+1:], ".")
	if slash < 0 || dot < 0 {
		return ""
	}
	name, _, _ := strings.Cut(p[slash+1+dot+1:], ".")
	if !token.IsIdentifier(name) || docsFileExtensions[name] {
		return ""
	}
	return p[:slash+1+dot]
}

// docsFileExtensions are what docsGoPackage reads as a file's extension
// rather than a Go name.
var docsFileExtensions = map[string]bool{
	"go": true, "md": true, "yml": true, "yaml": true, "json": true, "toml": true,
	"tsv": true, "txt": true, "mod": true, "sum": true, "sh": true,
}

// checkCodemap holds the codemap to the tree: every package directly
// under internal/ and cmd/ is named, and every directory named is one. A
// name that does not exist is the path check's to report.
func (g *docsGate) checkCodemap() {
	lines, ok := g.lines(architectureDoc)
	if !ok {
		return
	}
	start := slices.Index(lines, "## Codemap")
	if start < 0 {
		g.at(architectureDoc, 0, `no "## Codemap" heading`)
		return
	}
	named := map[string]int{}
	for i := start + 1; i < len(lines) && !docsEndsSection(lines[i], 2); i++ {
		for _, span := range docsCodeSpans(lines[i]) {
			if m := docsPackageName.FindStringSubmatch(span); m != nil && named[m[1]] == 0 {
				named[m[1]] = i + 1
			}
		}
	}
	tree := g.packageDirs()
	if len(tree) == 0 {
		g.fail("no package directories under internal/ or cmd/: the codemap check would be looking at nothing")
	}
	for _, pkg := range tree {
		if named[pkg] == 0 {
			g.at(architectureDoc, start+1, `"## Codemap" does not name %s`, pkg)
		}
	}
	for _, pkg := range slices.Sorted(maps.Keys(named)) {
		if g.dirs[pkg] && !slices.Contains(tree, pkg) {
			g.at(architectureDoc, named[pkg], `"## Codemap" names %s, which holds no .go file`, pkg)
		}
	}
}

// packageDirs is every directory directly under internal/ or cmd/ that
// holds a .go file of its own, sorted.
func (g *docsGate) packageDirs() []string {
	var out []string
	for _, f := range g.files {
		parts := strings.Split(f, "/")
		if len(parts) == 3 && (parts[0] == "internal" || parts[0] == "cmd") && strings.HasSuffix(f, ".go") && !docsInTestdata(f) {
			out = append(out, parts[0]+"/"+parts[1])
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
