package main

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"math/bits"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The parity gate holds `make check` and CI to the same set, and every
// gate in the registry to the pipeline it declares.
//
// The two lists live in two files and a person only ever edits one of
// them. So targets are mapped to CI steps by what they run, not by name:
// a CI step runs a target when it calls `make <target>`, or when its
// command is the target's recipe. A step's `name:` is not a command and
// is not read. Makefile variables are expanded before anything is
// compared, so `$(GATES) pins` and `go run ./scripts/gates pins` are the
// same line. The vet passes are held to the build tags the tree's files
// need, read from their //go:build lines.

const (
	parityMakefile = "Makefile"
	parityCI       = "ci.yml"
	parityHook     = ".githooks/pre-commit"
)

// parityCIOnly are the targets CI runs that `make check` does not, each
// with the reason. An entry CI no longer runs fails, so the list cannot
// outlive its reasons.
var parityCIOnly = map[string]string{
	"leaks-history": "reads every commit in the clone, which needs fetch-depth 0 and is slow on a laptop",
	"changelog":     "measures a pull request against its base, which only a pull request has",
	"schema-ack":    "measures a pull request against its merge-base, which only a pull request has",
}

// parityReleaseOnly are the inRelease commands, each with where it runs.
// TestParityExcusesEveryReleaseCommand holds this against the registry.
var parityReleaseOnly = map[string]string{
	"mcpb-pack":     "the universal binary's post hook in .goreleaser.yaml, the one point every binary exists",
	"release-notes": "release.yml writes the release body from the CHANGELOG before goreleaser runs",
}

// parityReleaseFiles are where an inRelease command must be found.
var parityReleaseFiles = []string{".goreleaser.yaml", ".github/workflows/release.yml", ".github/workflows/publish-mcp.yml"}

var (
	// parityGateName is the gates command at the start of the word after
	// ./scripts/gates.
	parityGateName = regexp.MustCompile(`^[a-z][a-z0-9-]*`)
)

func parity(out io.Writer, _ []string) error {
	r, err := parityCheck(".", commands)
	if err != nil {
		return err
	}
	if err := problemsError(out, "make/CI parity", r.problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "parity ok: %d check prerequisites, %d CI command lines, %d gates in check, "+
		"%d build tag sets vetted on both sides, %d CI-only and %d release-only entries excused\n",
		r.prereqs, r.ciLines, r.checkGates, r.vetTags, len(parityCIOnly), len(parityReleaseOnly))
	return nil
}

// parityReport is what the gate read and found.
type parityReport struct {
	problems                              []string
	prereqs, ciLines, checkGates, vetTags int
}

// parityCheck compares the repository at root with a registry. A
// checkout git cannot list is a gate that could not run.
func parityCheck(root string, registry map[string]command) (*parityReport, error) {
	mk, err := readMakefile(filepath.Join(root, parityMakefile))
	if err != nil {
		return nil, err
	}
	check, ok := mk.targets["check"]
	if !ok {
		return nil, fmt.Errorf("%s has no check: target", parityMakefile)
	}
	flows, err := readWorkflows(root)
	if err != nil {
		return nil, err
	}
	ci, ok := workflowNamed(flows, parityCI)
	if !ok {
		return nil, fmt.Errorf("%s/%s does not exist", workflowDir, parityCI)
	}
	files, err := gitFiles(root)
	if err != nil {
		return nil, couldNotRun{err}
	}
	tags, err := parityTreeTags(root, files)
	if err != nil {
		return nil, err
	}

	r := &parityReport{prereqs: len(check.prereqs), problems: slices.Clone(mk.refused)}

	// What CI runs: make targets, and the other command lines verbatim.
	// What a push to main runs is what `make check` is held to; a job or
	// step a pull request alone runs, or whose failure passes, is not.
	ciTargets, pushTargets := map[string]bool{}, map[string]bool{}
	var ciCommands, pushCommands []string
	for _, s := range ci.steps() {
		onPush := ci.onPush(s)
		for _, line := range workflowRunLines(s.Run) {
			r.ciLines++
			r.checkCILine(line)
			ciCommands = append(ciCommands, line)
			if onPush {
				pushCommands = append(pushCommands, line)
			}
			for _, t := range parityMakeTargets(line) {
				ciTargets[t] = true
				if onPush {
					pushTargets[t] = true
				}
			}
		}
	}
	p := parityPipelines{
		mk:           mk,
		ciTargets:    ciTargets,
		ciCommands:   ciCommands,
		pushCommands: pushCommands,
		ciClosure:    mk.closure(slices.Sorted(maps.Keys(pushTargets))...),
		checkClosure: mk.closure("check"),
	}
	everyClosure := mk.closure(slices.Sorted(maps.Keys(ciTargets))...)
	p.checkRecipes = parityLines(mk, p.checkClosure, nil)
	p.ciAll = parityLines(mk, p.ciClosure, pushCommands)
	p.ciEvery = parityLines(mk, everyClosure, ciCommands)

	r.checkFailuresStop(mk, append(slices.Clone(p.checkClosure), everyClosure...))
	r.checkPrereqs(p, check.prereqs)
	r.checkCITargets(p)
	r.checkGatesRun(root, p, registry)
	r.checkVetTags(p, tags)
	r.checkGoRuns(p)
	r.checkFloors()
	return r, nil
}

// parityPipelines is what the Makefile and CI run, as parityCheck read it.
type parityPipelines struct {
	mk        *makefile
	ciTargets map[string]bool
	// ciCommands are every CI command line, and pushCommands those a push
	// to main runs.
	ciCommands, pushCommands []string
	// ciClosure is what the targets a push runs depend on.
	ciClosure, checkClosure []string
	// checkRecipes are the expanded recipe lines `make check` runs, ciAll
	// the command lines a push runs with the recipes of the targets they
	// run, and ciEvery the same for every CI line.
	checkRecipes, ciAll, ciEvery []string
}

func (r *parityReport) fail(format string, a ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, a...))
}

// checkPrereqs holds that every check prerequisite is run by CI, by name
// or by recipe.
func (r *parityReport) checkPrereqs(p parityPipelines, prereqs []string) {
	for _, t := range prereqs {
		if _, defined := p.mk.targets[t]; !defined {
			r.fail("check: names %s, which the Makefile does not define", t)
			continue
		}
		if !slices.Contains(p.ciClosure, t) && !parityRecipeInCI(p.mk.recipe(t), p.pushCommands) {
			r.fail("`make check` runs %s and %s never does", t, parityCI)
		}
	}
}

// checkCITargets holds that every target CI runs is in check, or excused
// with a reason, and that every excuse is still needed.
func (r *parityReport) checkCITargets(p parityPipelines) {
	for _, t := range slices.Sorted(maps.Keys(p.ciTargets)) {
		if _, defined := p.mk.targets[t]; !defined {
			r.fail("%s runs `make %s`, which the Makefile does not define", parityCI, t)
			continue
		}
		reason, excused := parityCIOnly[t]
		switch {
		case slices.Contains(p.checkClosure, t) && excused:
			r.fail("%s is excused as CI-only and `make check` runs it; drop the excuse", t)
		case slices.Contains(p.checkClosure, t):
		case !excused:
			r.fail("%s runs `make %s` and `make check` does not; add it to check or to parityCIOnly with the reason", parityCI, t)
		case strings.TrimSpace(reason) == "":
			r.fail("%s is excused as CI-only with no reason", t)
		}
	}
	for t := range parityCIOnly {
		if !p.ciTargets[t] {
			r.fail("%s is excused as CI-only and %s no longer runs it; drop the excuse", t, parityCI)
		}
	}
}

// parityGateSites are the gates each pipeline calls.
type parityGateSites struct {
	// ci is what a push runs, and ciEvery what any CI run does.
	check, ci, ciEvery, release map[string]bool
}

// checkGatesRun holds that every gate runs where the registry says it
// does, and that the pre-commit hook runs the precommit gate.
func (r *parityReport) checkGatesRun(root string, p parityPipelines, registry map[string]command) {
	sites := parityGateSites{
		check:   parityGatesIn(p.checkRecipes),
		ci:      parityGatesIn(p.ciAll),
		ciEvery: parityGatesIn(p.ciEvery),
		release: parityGatesIn(strings.Split(parityReadAll(root, parityReleaseFiles), "\n")),
	}
	hook, _ := os.ReadFile(filepath.Join(root, parityHook)) //nolint:gosec // a repository path
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		r.checkGate(name, registry[name], sites)
	}
	for name := range parityReleaseOnly {
		if c, ok := registry[name]; !ok || c.runsIn != inRelease {
			r.fail("parityReleaseOnly excuses %s, which is not a release command in the registry", name)
		}
	}
	if !strings.Contains(string(hook), "scripts/gates precommit") {
		r.fail("%s does not run `go run ./scripts/gates precommit`", parityHook)
	}
}

// checkGate holds one gate to the pipeline its registry entry declares.
func (r *parityReport) checkGate(name string, c command, sites parityGateSites) {
	switch c.runsIn {
	case inCheck:
		r.checkGates++
		if !sites.check[name] {
			r.fail("the %s gate runs in check by the registry, and no target `make check` runs calls it", name)
		}
		if !sites.ci[name] {
			r.fail("the %s gate runs in check by the registry, and no job of %s a push to main runs calls it", name, parityCI)
		}
	case inCI:
		if !sites.ciEvery[name] {
			r.fail("the %s gate runs in CI by the registry, and %s never calls it", name, parityCI)
		}
		if sites.check[name] {
			r.fail("the %s gate is CI-only by the registry, and `make check` runs it; declare it inCheck", name)
		}
	case inRelease:
		reason := parityReleaseOnly[name]
		if strings.TrimSpace(reason) == "" {
			r.fail("the %s gate runs in release and parityReleaseOnly gives no reason", name)
		}
		if !sites.release[name] {
			r.fail("the %s gate runs in release by the registry, and none of %s calls it",
				name, strings.Join(parityReleaseFiles, ", "))
		}
		if sites.check[name] {
			r.fail("the %s gate is release-only by the registry, and `make check` runs it", name)
		}
	case manual:
		if sites.check[name] {
			r.fail("the %s gate is manual by the registry, and `make check` runs it; declare it inCheck", name)
		}
	default:
		r.fail("the %s gate declares no pipeline", name)
	}
}

// checkVetTags holds what each tagged file needs to a vet pass on both
// sides that compiles it: a file behind build tags compiles only in a pass
// with them, and one vetted only locally compiles only on a maintainer's
// laptop.
func (r *parityReport) checkVetTags(p parityPipelines, needs []parityTagNeed) {
	checkPasses := parityVetPasses(p.checkRecipes)
	ciPasses := parityVetPasses(p.ciAll)
	r.vetTags = len(needs)
	for _, need := range needs {
		if !need.vetted(checkPasses) {
			r.fail("%s builds only with %s and `make check` vets no such pass; a contributor cannot see it break",
				need.file, need)
		}
		if !need.vetted(ciPasses) {
			r.fail("%s builds only with %s and %s vets no such pass; it compiles only on a maintainer's machine",
				need.file, need, parityCI)
		}
	}
}

// checkGoRuns is the other direction for a tool CI runs itself rather
// than through make: `make check` runs the same module at the same
// version, or a contributor cannot reproduce what CI ran.
func (r *parityReport) checkGoRuns(p parityPipelines) {
	ran := map[string]bool{}
	for _, line := range p.checkRecipes {
		for _, run := range goRunsIn(line) {
			ran[run.pkg] = true
		}
	}
	for _, line := range p.ciCommands {
		for _, run := range goRunsIn(line) {
			if strings.Contains(run.pkg, "@") && !ran[run.pkg] {
				r.fail("%s runs `go run %s` and no target `make check` runs does; a contributor cannot reproduce it",
					parityCI, run.pkg)
			}
		}
	}
}

// checkFloors fails an empty comparison, which would otherwise be equal.
func (r *parityReport) checkFloors() {
	if r.prereqs < 20 {
		r.fail("check: has %d prerequisites, want at least 20", r.prereqs)
	}
	if r.ciLines < 15 {
		r.fail("%s has %d command lines, want at least 15; the reader is not seeing the steps", parityCI, r.ciLines)
	}
	if r.checkGates < 15 {
		r.fail("the registry has %d gates in check, want at least 15", r.checkGates)
	}
	if r.vetTags < 2 {
		r.fail("the tree's //go:build lines need %d -tags lists, want at least 2 (live and evals); "+
			"the tree is not being read", r.vetTags)
	}
}

// parityMakeTargets are the targets the `make` commands of a command line
// name.
func parityMakeTargets(line string) []string {
	var out []string
	for _, command := range shellLists(shellSplit(line)) {
		words := shellTexts(slices.DeleteFunc(slices.Clone(command), func(w shellWord) bool { return w.redir }))
		// ! inverts a command's status and is not the command.
		if len(words) > 0 && words[0] == "!" {
			words = words[1:]
		}
		if len(words) == 0 || (words[0] != "make" && words[0] != "$(MAKE)") {
			continue
		}
		for _, f := range words[1:] {
			if strings.HasPrefix(f, "-") || strings.Contains(f, "=") {
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

// checkCILine holds one ci.yml command line: a gate is called through
// make, so CI and `make check` run one recipe and cannot differ in its
// arguments, and a line that runs make or a gate lets no failure pass.
func (r *parityReport) checkCILine(line string) {
	words := shellSplit(line)
	runsMake := len(parityMakeTargets(line)) > 0
	for _, run := range goRunsOf(words) {
		if run.pkg == "./scripts/gates" {
			r.fail("%s runs `%s`, a gate called outside make; call its make target, so CI and `make check` "+
				"run one recipe", parityCI, line)
			runsMake = true
		}
	}
	if how := shellSwallowed(words); runsMake && how != "" {
		r.fail("%s runs `%s`, where %s, so a failing check passes", parityCI, line, how)
	}
}

// checkFailuresStop holds that make stops on a failing command in every
// target check or CI runs: a - before a recipe line, a .IGNORE target and
// a command that swallows its own failure each let one pass
// (https://www.gnu.org/software/make/manual/html_node/Errors.html).
func (r *parityReport) checkFailuresStop(mk *makefile, targets []string) {
	if _, ok := mk.targets[".IGNORE"]; ok {
		r.fail("%s: .IGNORE makes make ignore a failing command; drop it", parityMakefile)
	}
	seen := map[string]bool{}
	for _, t := range targets {
		target, ok := mk.targets[t]
		if !ok || seen[t] {
			continue
		}
		seen[t] = true
		for i, prefix := range target.prefixes {
			if strings.Contains(prefix, "-") {
				r.fail("%s:%d: %s's recipe line starts with -, so make goes on when it fails",
					parityMakefile, target.lines[i], t)
			}
			if how := shellSwallowed(shellSplit(mk.expand(target.recipe[i]))); how != "" {
				r.fail("%s:%d: %s's recipe line is one where %s, so a failing check passes",
					parityMakefile, target.lines[i], t, how)
			}
		}
	}
}

// shellSwallowed names how a command line lets a failure pass, or is "":
// || with true, : or exit 0 after it, a last command of true, : or exit 0
// after ;, a command run in the background with &, ! before a command, and
// set +e (https://pubs.opengroup.org/onlinepubs/9799919799/utilities/V3_chap02.html#tag_19_09).
func shellSwallowed(words []shellWord) string {
	words = slices.DeleteFunc(slices.Clone(words), func(w shellWord) bool { return w.redir })
	op := ""
	for i := 0; i < len(words); {
		if words[i].op {
			if words[i].text == "&" {
				return "& runs a command in the background, where its failure is never seen"
			}
			op = words[i].text
			i++
			continue
		}
		end := i
		for end < len(words) && !words[end].op {
			end++
		}
		command := shellTexts(words[i:end])
		last := end == len(words)
		passes := command[0] == "true" || command[0] == ":" || command[0] == "exit" && len(command) == 2 && command[1] == "0"
		switch {
		case op == "||" && passes:
			return "`|| " + strings.Join(command, " ") + "` passes whatever came before"
		case op == ";" && last && passes:
			return "`; " + strings.Join(command, " ") + "` ends it with success whatever came before"
		case command[0] == "!":
			return "! turns a failure into success"
		case command[0] == "set" && slices.ContainsFunc(command[1:], func(a string) bool {
			return strings.HasPrefix(a, "+") && strings.Contains(a, "e")
		}):
			return "set +e lets the commands after it fail"
		}
		i = end
	}
	return ""
}

// parityRecipeInCI reports whether every line of a recipe is a command
// CI runs verbatim.
func parityRecipeInCI(recipe, ciCommands []string) bool {
	if len(recipe) == 0 {
		return false
	}
	for _, line := range recipe {
		if !slices.Contains(ciCommands, line) {
			return false
		}
	}
	return true
}

// parityLines are the targets' recipe lines followed by the extra
// command lines.
func parityLines(mk *makefile, targets, extra []string) []string {
	recipes := make([][]string, 0, len(targets))
	for _, t := range targets {
		recipes = append(recipes, mk.recipe(t))
	}
	return append(slices.Concat(recipes...), extra...)
}

// parityGatesIn are the gates command lines call.
func parityGatesIn(lines []string) map[string]bool {
	found := map[string]bool{}
	for _, line := range lines {
		for _, run := range goRunsIn(line) {
			if run.pkg != "./scripts/gates" || len(run.args) == 0 {
				continue
			}
			if name := parityGateName.FindString(run.args[0]); name != "" {
				found[name] = true
			}
		}
	}
	return found
}

// parityVetPasses is the tags of each `go vet` in lines that sets any,
// from its -tags flag wherever it sits among the arguments: -tags=a,b or
// -tags a,b, with one dash or two (https://pkg.go.dev/flag#hdr-Command_line_flag_syntax).
func parityVetPasses(lines []string) [][]string {
	var passes [][]string
	for _, line := range lines {
		for _, c := range goCommands(line) {
			if c.sub != "vet" {
				continue
			}
			args := c.args
			for i, arg := range args {
				name, value, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
				if !strings.HasPrefix(arg, "-") || name != "tags" {
					continue
				}
				if !inline && i+1 < len(args) {
					value = args[i+1]
				}
				if value != "" {
					passes = append(passes, strings.Split(value, ","))
				}
			}
		}
	}
	return passes
}

// parityTagNeed is what a tagged file needs from a vet pass: one that sets
// every tag of any one of its lists. file is the first file that needs it.
type parityTagNeed struct {
	file  string
	lists []string
}

func (n parityTagNeed) String() string { return "-tags=" + strings.Join(n.lists, " or -tags=") }

// vetted reports whether one of the passes compiles the file.
func (n parityTagNeed) vetted(passes [][]string) bool {
	return slices.ContainsFunc(n.lists, func(list string) bool {
		return slices.ContainsFunc(passes, func(pass []string) bool {
			return !slices.ContainsFunc(strings.Split(list, ","), func(tag string) bool { return !slices.Contains(pass, tag) })
		})
	})
}

// parityTreeTags is what each //go:build line under root needs, once per
// need and in file order: a file that builds with tags and not without
// them, which an untagged vet never compiles. files are slash-separated
// and relative to root.
func parityTreeTags(root string, files []string) ([]parityTagNeed, error) {
	fset := token.NewFileSet()
	var needs []parityTagNeed
	for _, rel := range files {
		if !strings.HasSuffix(rel, ".go") || docsInTestdata(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // a path git lists
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, rel, src, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			return nil, err
		}
		lists := parityRequiredTags(f)
		if lists != nil && !slices.ContainsFunc(needs, func(n parityTagNeed) bool { return slices.Equal(n.lists, lists) }) {
			needs = append(needs, parityTagNeed{file: rel, lists: lists})
		}
	}
	return needs, nil
}

// parityGoBuildExpr is a file's //go:build expression, or nil when it has
// none or the line does not parse
// (https://pkg.go.dev/cmd/go#hdr-Build_constraints).
func parityGoBuildExpr(f *ast.File) constraint.Expr {
	for _, group := range f.Comments {
		if group.Pos() >= f.Package {
			break
		}
		for _, c := range group.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				return nil
			}
			return expr
		}
	}
	return nil
}

// parityRequiredTags is the -tags lists that compile a file's //go:build
// line: none when it builds with no custom tag, and otherwise each minimal
// set of custom tags it builds with, joined by commas, any one of which
// is enough. A tag the go command sets for a target or a toolchain is free
// to be either value, so `e2e && linux` needs e2e, `a && b` needs one pass
// that sets both, and `a || b` a pass that sets either.
func parityRequiredTags(f *ast.File) []string {
	expr := parityGoBuildExpr(f)
	if expr == nil {
		return nil
	}
	var free, custom []string
	// Eval calls its function for every tag, even past a decided result.
	expr.Eval(func(tag string) bool {
		switch {
		// ignore keeps a file out of every build, so no -tags can need it.
		case tag == "ignore" || slices.Contains(free, tag) || slices.Contains(custom, tag):
		case parityToolchainTag(tag):
			free = append(free, tag)
		default:
			custom = append(custom, tag)
		}
		return false
	})
	slices.Sort(custom)
	if parityBuildsWith(expr, free, nil) {
		return nil
	}
	var found []int
	var lists []string
	for size := 1; size <= len(custom); size++ {
		for set := range 1 << len(custom) {
			// A set holding one that builds is not minimal.
			if bits.OnesCount(uint(set)) != size || slices.ContainsFunc(found, func(f int) bool { return set&f == f }) {
				continue
			}
			var with []string
			for i, tag := range custom {
				if set&(1<<i) != 0 {
					with = append(with, tag)
				}
			}
			if parityBuildsWith(expr, free, with) {
				found = append(found, set)
				lists = append(lists, strings.Join(with, ","))
			}
		}
	}
	return lists
}

// parityBuildsWith reports whether a file builds with the custom tags in
// with, and no other, for some value of each free tag.
func parityBuildsWith(expr constraint.Expr, free, with []string) bool {
	for set := range 1 << len(free) {
		if expr.Eval(func(tag string) bool {
			if i := slices.Index(free, tag); i >= 0 {
				return set&(1<<i) != 0
			}
			return slices.Contains(with, tag)
		}) {
			return true
		}
	}
	return false
}

// parityKnownOS and parityKnownArch are the GOOS and GOARCH values
// go/build knows, as https://go.dev/src/internal/syslist/syslist.go lists
// them.
var (
	parityKnownOS = map[string]bool{
		"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
		"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "nacl": true,
		"netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true,
		"windows": true, "zos": true,
	}
	parityKnownArch = map[string]bool{
		"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true,
		"arm64be": true, "loong64": true, "mips": true, "mipsle": true, "mips64": true,
		"mips64le": true, "mips64p32": true, "mips64p32le": true, "ppc": true, "ppc64": true,
		"ppc64le": true, "riscv": true, "riscv64": true, "s390": true, "s390x": true,
		"sparc": true, "sparc64": true, "wasm": true,
	}
	// parityOtherTags are set by the go command for a target and a
	// toolchain, and race, msan and asan by -race, -msan and -asan
	// (https://github.com/golang/go/blob/go1.27.3/src/cmd/go/internal/work/init.go#L212).
	parityOtherTags = map[string]bool{
		"unix": true, "cgo": true, "gc": true, "gccgo": true, "race": true, "msan": true, "asan": true,
	}
	// parityReleaseTag is a release tag, such as go1.22, set for each
	// release up to the toolchain's own.
	parityReleaseTag = regexp.MustCompile(`^go1\.\d+$`)
)

// parityToolchainTag reports whether a build tag is one no -tags flag
// needs to name: an OS, an architecture or one of its features, such as
// amd64.v2, or a tag the go command sets
// (https://pkg.go.dev/cmd/go#hdr-Build_constraints). A goexperiment tag
// is GOEXPERIMENT's to set (https://pkg.go.dev/internal/goexperiment).
func parityToolchainTag(tag string) bool {
	arch, _, _ := strings.Cut(tag, ".")
	return parityKnownOS[tag] || parityKnownArch[arch] || parityOtherTags[tag] || parityReleaseTag.MatchString(tag) ||
		strings.HasPrefix(tag, "goexperiment.")
}

// parityReadAll joins the files that exist; a missing one is the
// release pipeline's own gate to report.
func parityReadAll(root string, paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p))) //nolint:gosec // repository paths
		if err == nil {
			b.Write(data)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
