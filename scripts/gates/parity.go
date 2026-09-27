package main

import (
	"fmt"
	"io"
	"maps"
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
// same line.

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
	parityGateCall = regexp.MustCompile(`go run \./scripts/gates\s+([a-z][a-z0-9-]*)`)
	parityVetTags  = regexp.MustCompile(`-tags[= ]([A-Za-z0-9_,]+)`)
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
		"%d vet tag sets, %d CI-only and %d release-only entries excused\n",
		r.prereqs, r.ciLines, r.checkGates, r.vetTags, len(parityCIOnly), len(parityReleaseOnly))
	return nil
}

// parityReport is what the gate read and found.
type parityReport struct {
	problems                              []string
	prereqs, ciLines, checkGates, vetTags int
}

// parityCheck compares the repository at root with a registry.
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

	r := &parityReport{prereqs: len(check.prereqs)}

	// What CI runs: make targets, and the other command lines verbatim.
	ciTargets := map[string]bool{}
	var ciCommands []string
	for _, s := range ci.steps() {
		for _, line := range workflowRunLines(s.Run) {
			r.ciLines++
			ciCommands = append(ciCommands, line)
			for _, t := range parityMakeTargets(line) {
				ciTargets[t] = true
			}
		}
	}
	p := parityPipelines{
		mk:           mk,
		ciTargets:    ciTargets,
		ciCommands:   ciCommands,
		ciClosure:    mk.closure(slices.Sorted(maps.Keys(ciTargets))...),
		checkClosure: mk.closure("check"),
	}

	r.checkPrereqs(p, check.prereqs)
	r.checkCITargets(p)
	r.checkGatesRun(root, p, registry)
	r.checkVetTags(p)
	r.checkFloors()
	return r, nil
}

// parityPipelines is what the Makefile and CI run, as parityCheck read it.
type parityPipelines struct {
	mk                      *makefile
	ciTargets               map[string]bool
	ciCommands              []string
	ciClosure, checkClosure []string
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
		if !slices.Contains(p.ciClosure, t) && !parityRecipeInCI(p.mk.recipe(t), p.ciCommands) {
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
	check, ci, release map[string]bool
}

// checkGatesRun holds that every gate runs where the registry says it
// does, and that the pre-commit hook runs the precommit gate.
func (r *parityReport) checkGatesRun(root string, p parityPipelines, registry map[string]command) {
	sites := parityGateSites{
		check:   parityGatesIn(p.mk, p.checkClosure, nil),
		ci:      parityGatesIn(p.mk, p.ciClosure, p.ciCommands),
		release: map[string]bool{},
	}
	for _, m := range parityGateCall.FindAllStringSubmatch(parityReadAll(root, parityReleaseFiles), -1) {
		sites.release[m[1]] = true
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
			r.fail("the %s gate runs in check by the registry, and %s never calls it", name, parityCI)
		}
	case inCI:
		if !sites.ci[name] {
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

// checkVetTags holds every vet build tag both ways: a tag vetted only
// locally leaves tagged code compiling only on a maintainer's laptop.
func (r *parityReport) checkVetTags(p parityPipelines) {
	checkTags := parityVetTagsIn(p.mk, p.checkClosure, nil)
	ciTags := parityVetTagsIn(p.mk, p.ciClosure, p.ciCommands)
	r.vetTags = len(checkTags)
	for tag := range checkTags {
		if !ciTags[tag] {
			r.fail("`make check` vets with -tags=%s and %s does not", tag, parityCI)
		}
	}
	for tag := range ciTags {
		if !checkTags[tag] {
			r.fail("%s vets with -tags=%s and `make check` does not", parityCI, tag)
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
		r.fail("`make check` vets %d build tags, want at least 2 (live and evals)", r.vetTags)
	}
}

// parityMakeTargets are the targets a `make` command line names.
func parityMakeTargets(line string) []string {
	fields := strings.Fields(line)
	if len(fields) == 0 || (fields[0] != "make" && fields[0] != "$(MAKE)") {
		return nil
	}
	var out []string
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") || strings.Contains(f, "=") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// parityRecipeInCI reports whether every line of a recipe is a command
// CI runs verbatim.
func parityRecipeInCI(recipe, ciCommands []string) bool {
	if len(recipe) == 0 {
		return false
	}
	for _, line := range recipe {
		line = strings.TrimLeft(line, "@-")
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

// parityGatesIn are the gates the targets' recipes and the extra
// command lines call.
func parityGatesIn(mk *makefile, targets, extra []string) map[string]bool {
	found := map[string]bool{}
	for _, line := range parityLines(mk, targets, extra) {
		for _, m := range parityGateCall.FindAllStringSubmatch(line, -1) {
			found[m[1]] = true
		}
	}
	return found
}

// parityVetTagsIn are the build tags `go vet` runs with.
func parityVetTagsIn(mk *makefile, targets, extra []string) map[string]bool {
	found := map[string]bool{}
	for _, line := range parityLines(mk, targets, extra) {
		if !strings.Contains(line, " vet") {
			continue
		}
		for _, m := range parityVetTags.FindAllStringSubmatch(line, -1) {
			found[m[1]] = true
		}
	}
	return found
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
