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
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The pins gate holds every third-party tool to one exact version, the
// same version everywhere it is named (D-95). The pins are the workflows,
// the Makefile and go.mod, compared with each other; no document copies
// one.
//
// The ways a version floats, each a rule:
//   - an action used by tag or branch rather than a 40-hex commit SHA,
//     or a SHA with no comment saying which version it is, and a
//     container image, a step's, a job's or a service's, without its
//     sha256 digest;
//   - an action that installs a tool without naming the tool's version:
//     a SHA pins the wrapper, not the tool;
//   - a version input, env value or `go run module@v` that is a range, a
//     major line or `latest`;
//   - a Makefile variable that pins a tool no recipe runs with `go run`,
//     so the tool runs from the PATH;
//   - one tool pinned to two versions in two files, or on two lines of
//     one, so a rehearsal or a pre-commit scan runs something other than
//     CI;
//   - a tool a recipe or the pre-commit hook runs from the PATH, or a
//     download of whatever release is newest;
//   - a shell other than bash, at any level of a workflow.
//
// Every action is classified as an installer or not, and an unknown one
// fails: being unclassified is how an unpinned tool slips through.

const (
	pinsMakefile = "Makefile"
	pinsGoMod    = "go.mod"
	// pinsScripts is the maintainer tooling, whose Go may copy a pin.
	pinsScripts = "scripts"
	// pinsMinPlaces is the fewest pins the readers may find in all.
	// pinsSources names a file read as empty; this catches a reader that
	// drops part of what a file pins.
	pinsMinPlaces = 16
)

// pinsRequiredWorkflows must exist; a gate that counted files would
// pass with the wrong two.
var pinsRequiredWorkflows = []string{"ci.yml", "codeql.yml", "release.yml", "publish-mcp.yml"}

// pinsSources are the files that pin a tool, each held to at least one
// pin read. A total alone passes when one file reads as empty and the
// others make up the count.
var pinsSources = []string{
	workflowDir + "/codeql.yml", workflowDir + "/release.yml", workflowDir + "/publish-mcp.yml",
	pinsMakefile, pinsGoMod,
}

// pinsTool is one tool this repository runs, and every way it can be
// named.
type pinsTool struct {
	name string
	// modules match a `module@version` in the Makefile or a run line.
	modules []string
	// envKeys are env names whose value is this tool's version.
	envKeys []string
	// actions install this tool, with the input that pins it.
	actions map[string]string
	// commentOf reads the version from the trailing comment on these
	// actions' SHA pins, for an action that is the tool itself.
	commentOf []string
	// fromGoMod reads the version from go.mod: "go" or a module path. The
	// go row's action input names go.mod rather than a version.
	fromGoMod string
}

var pinsTools = []pinsTool{
	{name: "go", fromGoMod: "go", actions: map[string]string{"actions/setup-go": "go-version-file"}},
	{name: "mcp-go-sdk", fromGoMod: "github.com/modelcontextprotocol/go-sdk"},
	{name: "golangci-lint", modules: []string{"golangci-lint"}},
	{name: "goreleaser", modules: []string{"github.com/goreleaser/goreleaser"}, envKeys: []string{"GORELEASER_VERSION"},
		actions: map[string]string{"goreleaser/goreleaser-action": "version"}},
	{name: "cosign", actions: map[string]string{"sigstore/cosign-installer": "cosign-release"}},
	{name: "syft", actions: map[string]string{
		"anchore/sbom-action/download-syft": "syft-version", "anchore/sbom-action": "syft-version"}},
	{name: "mcp-publisher", envKeys: []string{"PUBLISHER_VERSION"}},
	{name: "gitleaks", modules: []string{"github.com/zricethezav/gitleaks"}, envKeys: []string{"GITLEAKS_VERSION"}},
	{name: "govulncheck", modules: []string{"golang.org/x/vuln/cmd/govulncheck"}},
	{name: "go-licenses", modules: []string{"github.com/google/go-licenses"}},
	{name: "actionlint", modules: []string{"github.com/rhysd/actionlint"}},
	{name: "codeql-action", commentOf: []string{"github/codeql-action/"}},
}

// pinsNotInstallers are actions that install no tool, each with the
// reason, so adding one is a decision rather than an omission.
var pinsNotInstallers = map[string]string{
	"actions/checkout":                "checks out the repository",
	"actions/upload-artifact":         "uploads, installs nothing",
	"actions/download-artifact":       "downloads, installs nothing",
	"actions/attest-build-provenance": "calls the attestation API",
	"github/codeql-action/init":       "CodeQL's bundle is GitHub's to manage; the action's own version is held by comment",
	"github/codeql-action/autobuild":  "builds with the Go already installed",
	"github/codeql-action/analyze":    "uploads results",
}

var (
	pinsSHA      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pinsExact    = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)
	pinsModuleAt = regexp.MustCompile(`([A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+)@(\S+)`)
	pinsSemverIn = regexp.MustCompile(`v?\d+\.\d+\.\d+`)
	// pinsVersionEn is an env name holding a version, in any case: the
	// runner does not fold env names, and a reader that did not either
	// would pass `Tool_Version: latest`.
	pinsVersionEn = regexp.MustCompile(`(?i)_VERSION$`)
	// pinsContextRef is one context's value, such as ${{ inputs.tag }}: a
	// reusable workflow of this repository holds it where it uses it.
	pinsContextRef = regexp.MustCompile(`^\$\{\{\s*[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_-]*)+\s*\}\}$`)
	// floatingDownload is a tool fetched at whatever version is newest: a
	// GitHub "latest release" download, or a Go module at @latest.
	// `runs-on: ubuntu-latest` is an image label, not a tool.
	floatingDownload = regexp.MustCompile(`releases/latest/download|@latest\b`)
	// pinsImageDigest is a container image pinned by its content digest.
	pinsImageDigest = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
	// pinsGoModToolchain and pinsGoModGo are setup-go's own patterns for
	// the toolchain directive and the go line.
	pinsGoModToolchain = regexp.MustCompile(`(?m)^toolchain go(1\.\d+(?:\.\d+|rc\d+)?)`)
	pinsGoModGo        = regexp.MustCompile(`(?m)^go (\d+(?:\.\d+)*)`)
)

// pinsFound is one place a tool's version is written.
type pinsFound struct {
	version, at string
}

func pins(out io.Writer, _ []string) error {
	report, err := pinsCheck(".", pinsMinPlaces)
	if err != nil {
		return err
	}
	if err := problemsError(out, "the pins", report.problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "pins ok: %d workflows, %d actions by SHA, %d installers name their tool, "+
		"%d tools pinned in %d places\n",
		report.workflows, report.actions, report.installers, len(report.tools), report.places)
	return nil
}

// pinsReport is what the gate read and found.
type pinsReport struct {
	problems                               []string
	workflows, actions, installers, places int
	tools                                  map[string][]pinsFound
	// placesIn counts the pins read from each file.
	placesIn map[string]int
	// installed names each tool a workflow step installs.
	installed map[string]bool
}

// pinsCheck runs every rule over the repository at root, held to finding
// at least minPlaces pins.
func pinsCheck(root string, minPlaces int) (*pinsReport, error) {
	flows, err := readWorkflows(root)
	if err != nil {
		return nil, err
	}
	mk, err := readMakefile(filepath.Join(root, pinsMakefile))
	if err != nil {
		return nil, err
	}
	gomod, err := os.ReadFile(filepath.Join(root, pinsGoMod))
	if err != nil {
		return nil, err
	}
	g, err := readGoreleaser(root)
	if err != nil {
		return nil, err
	}
	hook, err := os.ReadFile(filepath.Join(root, parityHook)) //nolint:gosec // a repository path
	if err != nil {
		return nil, err
	}

	r := &pinsReport{tools: map[string][]pinsFound{}, placesIn: map[string]int{}, installed: map[string]bool{},
		workflows: len(flows), problems: slices.Clone(mk.refused)}
	// addIn records the pins read from one file, and textIn those the text
	// rule finds there, but a version a rule already recorded: the same
	// pin is one fact, reported once.
	seen := map[string]bool{}
	addIn := func(file string) func(tool, version, at string) {
		return func(tool, version, at string) {
			seen[tool+"@"+version] = true
			r.tools[tool] = append(r.tools[tool], pinsFound{version, at})
			r.places++
			r.placesIn[file]++
			if !pinsExact.MatchString(version) {
				r.problems = append(r.problems, fmt.Sprintf("%s: %s is pinned to %q, which is not one exact version",
					at, tool, version))
			}
		}
	}

	textIn := func(file string) func(tool, version, at string) {
		add := addIn(file)
		return func(tool, version, at string) {
			if !seen[tool+"@"+version] {
				add(tool, version, at)
			}
		}
	}

	for _, base := range pinsRequiredWorkflows {
		if _, ok := workflowNamed(flows, base); !ok {
			r.problems = append(r.problems, fmt.Sprintf("%s/%s does not exist", workflowDir, base))
		}
	}
	for _, w := range flows {
		pinsWorkflow(string(gomod), w, r, addIn(w.path))
		pinsTextVersions(w.path, w.text, textIn(w.path))
	}
	pinnedBy := pinsMakefileVars(mk, r, addIn(pinsMakefile))
	pinsMakefileRecipes(mk, pinnedBy, r, addIn(pinsMakefile))
	pinsTextVersions(pinsMakefile, pinsExpanded(mk), textIn(pinsMakefile))
	pinsGoreleaser(g, r, addIn(releaseGoreleaser), textIn(releaseGoreleaser))
	pinsHook(string(hook), r, textIn(parityHook))
	if err := pinsGoSource(root, textIn(pinsScripts)); err != nil {
		return nil, err
	}
	pinsGoModVersions(string(gomod), addIn(pinsGoMod))

	for _, t := range pinsTools {
		found := r.tools[t.name]
		if len(found) == 0 {
			r.problems = append(r.problems, fmt.Sprintf("%s is pinned nowhere: a tool the table names "+
				"that no file pins is either gone or unread", t.name))
			continue
		}
		for _, f := range found[1:] {
			if pinsNormal(f.version) != pinsNormal(found[0].version) {
				r.problems = append(r.problems, fmt.Sprintf("%s is %s at %s and %s at %s: one tool, one version",
					t.name, found[0].version, found[0].at, f.version, f.at))
			}
		}
	}
	// The goreleaser reader's key paths and template rule are goreleaser's
	// at one version, so a release that runs another is read on belief.
	if found := r.tools["goreleaser"]; len(found) > 0 && pinsNormal(found[0].version) != pinsNormal(goreleaserChecked) {
		r.problems = append(r.problems, fmt.Sprintf("%s runs goreleaser %s, and the %s reader is checked against "+
			"%s's source; check its key paths and template rule against %s's, then set goreleaserChecked",
			found[0].at, found[0].version, releaseGoreleaser, goreleaserChecked, found[0].version))
	}
	// Floors: zero findings and zero inputs look the same otherwise.
	if r.actions < 10 {
		r.problems = append(r.problems, fmt.Sprintf("read only %d action references; the reader is not "+
			"seeing the workflows", r.actions))
	}
	// A floor per installer, not a count of steps: one installer used in
	// every job would meet a count alone.
	for _, t := range pinsTools {
		if len(t.actions) > 0 && !r.installed[t.name] {
			r.problems = append(r.problems, fmt.Sprintf("no workflow step installs %s through %s; the reader "+
				"is not seeing the step, or the table names an installer nothing uses",
				t.name, strings.Join(slices.Sorted(maps.Keys(t.actions)), " or ")))
		}
	}
	for _, f := range pinsSources {
		if r.placesIn[f] == 0 {
			r.problems = append(r.problems, fmt.Sprintf("%s: no pins read, though it pins a tool; "+
				"its reader is not seeing it", f))
		}
	}
	if r.places < minPlaces {
		r.problems = append(r.problems, fmt.Sprintf("read %d pins across the workflows, %s and %s, want at least %d; "+
			"a reader is not seeing its file", r.places, pinsMakefile, pinsGoMod, minPlaces))
	}
	return r, nil
}

// pinsWorkflow applies the workflow rules to one file, with gomod the
// repository's go.mod.
func pinsWorkflow(gomod string, w workflow, r *pinsReport, add func(tool, version, at string)) {
	pinsFloatingLines(w, r)
	pinsUses(w, r, add)
	pinsShells(w, r)
	pinsCalls(w, r)

	// The structure, for which input belongs to which step.
	envs := []map[string]string{w.Env}
	for _, j := range w.Jobs {
		envs = append(envs, j.Env)
	}
	for _, s := range w.steps() {
		envs = append(envs, s.Env)
		// An image is held here, on the parse, which sees a `uses:` in
		// every shape YAML allows; the line scan does not.
		if strings.HasPrefix(s.Uses, "docker://") && !pinsImageDigest.MatchString(s.Uses) {
			r.problems = append(r.problems, fmt.Sprintf("%s: job %s: %s is a container image with no sha256 digest, "+
				"so the image it runs can change", w.path, s.job, s.Uses))
		}
		if s.Uses == "" || strings.HasPrefix(s.Uses, "./") || strings.HasPrefix(s.Uses, "docker://") {
			pinsRunLines(s.Run, w.path, r, add)
			continue
		}
		pinsStep(gomod, w, s, r, add)
	}
	for _, env := range envs {
		pinsEnv(w.path, env, r, add)
	}
	pinsJobImages(w, r)
}

// pinsJobImages holds each job's container and service images to a
// sha256 digest, as a step's docker:// image is held
// (https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idcontainer).
func pinsJobImages(w workflow, r *pinsReport) {
	for _, id := range slices.Sorted(maps.Keys(w.Jobs)) {
		job := w.Jobs[id]
		images := map[string]string{"the container": workflowImage(&job.Container)}
		for name, service := range job.Services {
			images["the service "+name] = workflowImage(&service)
		}
		for _, what := range slices.Sorted(maps.Keys(images)) {
			if image := images[what]; image != "" && !pinsImageDigest.MatchString(image) {
				r.problems = append(r.problems, fmt.Sprintf("%s: job %s runs %s from %s, which has no sha256 digest, "+
					"so the image it runs can change", w.path, id, what, image))
			}
		}
	}
}

// pinsFloatingLines refuses a line of the file, but a comment, that
// fetches whatever version is newest.
func pinsFloatingLines(w workflow, r *pinsReport) {
	for i, line := range strings.Split(w.text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			pinsFloating(fmt.Sprintf("%s:%d", w.path, i+1), line, r)
		}
	}
}

// pinsUses holds every `uses:` the parse finds, a step's or a reusable
// workflow call's, wherever YAML lets one sit: on the next line, in a
// flow mapping or behind an alias, which a line scan does not see.
func pinsUses(w workflow, r *pinsReport, add func(tool, version, at string)) {
	lines := strings.Split(w.text, "\n")
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if v := workflowResolve(n.Content[i+1]); n.Content[i].Value == "uses" && v.Kind == yaml.ScalarNode {
					pinsUse(w.path, v, lines, r, add)
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&w.node)
}

// pinsUse holds one action reference: its 40-character commit SHA, and a
// comment naming the version the SHA is.
func pinsUse(file string, v *yaml.Node, lines []string, r *pinsReport, add func(tool, version, at string)) {
	ref := strings.TrimSpace(v.Value)
	if strings.HasPrefix(ref, "./") {
		return
	}
	r.actions++
	if strings.HasPrefix(ref, "docker://") {
		return
	}
	at := fmt.Sprintf("%s:%d", file, v.Line)
	action, sha, ok := strings.Cut(ref, "@")
	comment := pinsUseComment(v, lines)
	switch {
	case !ok:
		r.problems = append(r.problems, fmt.Sprintf("%s: %s has no ref at all", at, action))
	case !pinsSHA.MatchString(sha):
		r.problems = append(r.problems, fmt.Sprintf("%s: %s is pinned to %q, a tag or branch that "+
			"can be moved; pin the 40-character commit SHA", at, action, sha))
	case comment == "":
		r.problems = append(r.problems, fmt.Sprintf("%s: %s is pinned by SHA with no comment saying "+
			"which version it is", at, action))
	}
	for _, t := range pinsTools {
		for _, prefix := range t.commentOf {
			if strings.HasPrefix(action, prefix) {
				add(t.name, pinsSemverIn.FindString(comment), at)
			}
		}
	}
}

// pinsUseComment is the comment after a `uses:` value: the parse's, or
// the text after a # on the value's line, where YAML hangs a comment after
// a flow mapping on the mapping rather than on the value.
func pinsUseComment(v *yaml.Node, lines []string) string {
	if c := strings.TrimSpace(strings.TrimPrefix(v.LineComment, "#")); c != "" {
		return c
	}
	if v.Line < 1 || v.Line > len(lines) {
		return ""
	}
	line := lines[v.Line-1]
	if i := strings.Index(line, v.Value); i >= 0 {
		line = line[i+len(v.Value):]
	}
	if _, c, ok := strings.Cut(line, " #"); ok {
		return strings.TrimSpace(c)
	}
	return ""
}

// pinsShells holds every level of a workflow to bash. The runner's default
// is bash on Linux and macOS and PowerShell on Windows, so a step without
// a shell reads its line one way or the other by the runner it lands on
// (https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#defaultsrunshell).
func pinsShells(w workflow, r *pinsReport) {
	if w.Defaults.Run.Shell != "bash" {
		r.problems = append(r.problems, fmt.Sprintf("%s: no workflow-level `defaults: run: shell: bash`. Every run "+
			"step goes to a shell, so a job-level block only fixes the jobs that exist today", w.path))
	}
	for _, id := range slices.Sorted(maps.Keys(w.Jobs)) {
		if sh := w.Jobs[id].Defaults.Run.Shell; sh != "" && sh != "bash" {
			r.problems = append(r.problems, fmt.Sprintf("%s: job %s runs its steps in %s; every level is bash", w.path, id, sh))
		}
	}
	for _, s := range w.steps() {
		if s.Shell != "" && s.Shell != "bash" {
			r.problems = append(r.problems, fmt.Sprintf("%s: job %s has a step in %s; every level is bash", w.path, s.job, s.Shell))
		}
	}
}

// pinsCalls holds the versions a job passes a reusable workflow. A remote
// one's are held as a step's inputs are, since its code is not read here.
// One of this repository's may be one context's value, which the workflow
// holds where it uses it; a literal is still exact.
func pinsCalls(w workflow, r *pinsReport) {
	for _, id := range slices.Sorted(maps.Keys(w.Jobs)) {
		job := w.Jobs[id]
		if job.Uses == "" {
			continue
		}
		at := fmt.Sprintf("%s: job %s calls %s", w.path, id, job.Uses)
		pinsInputCase(at, job.With, r)
		local := strings.HasPrefix(job.Uses, "./")
		for _, key := range slices.Sorted(maps.Keys(job.With)) {
			if !pinsVersionInput(key) {
				continue
			}
			v, _ := workflowInput(job.With, key)
			if !pinsExact.MatchString(v) && (!local || !pinsContextRef.MatchString(v)) {
				r.problems = append(r.problems, fmt.Sprintf("%s with %s %q, which is not one exact version", at, key, v))
			}
		}
	}
}

// pinsVersionInput reports whether an input name holds a version, in any
// case: the runner reads input names upper-cased.
func pinsVersionInput(key string) bool {
	k := strings.ToLower(key)
	return k != "go-version-file" && (strings.HasSuffix(k, "version") || strings.HasSuffix(k, "release"))
}

// pinsInputCase refuses two inputs whose names differ only in case: the
// runner reads both into one INPUT_ variable, so which one wins is not
// written down.
func pinsInputCase(at string, with map[string]any, r *pinsReport) {
	seen := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(with)) {
		upper := strings.ToUpper(key)
		if other, twice := seen[upper]; twice {
			r.problems = append(r.problems, fmt.Sprintf("%s: inputs %s and %s are one input to the runner, "+
				"which reads names in upper case", at, other, key))
		}
		seen[upper] = key
	}
}

// pinsStep checks one step that uses an action: an installer sets the
// input that pins its tool, any other action is classified, and every
// version input names one exact version.
func pinsStep(gomod string, w workflow, s workflowStep, r *pinsReport, add func(tool, version, at string)) {
	action := s.action()
	at := w.path + ": " + action
	installer := false
	for _, t := range pinsTools {
		key, ok := t.actions[action]
		if !ok {
			continue
		}
		installer = true
		r.installers++
		r.installed[t.name] = true
		v, set := s.input(key)
		if !set {
			r.problems = append(r.problems, fmt.Sprintf("%s is pinned by SHA and does not set %s, so the "+
				"tool it installs is whatever is current that morning", at, key))
			continue
		}
		if t.fromGoMod == "go" {
			version, problem := pinsGoVersionFile(v, gomod)
			if problem != "" {
				r.problems = append(r.problems, fmt.Sprintf("%s sets %s to %q, %s", at, key, v, problem))
				continue
			}
			v = version
		}
		add(t.name, w.resolveEnv(v), at)
		// setup-go reads go-version before go-version-file
		// (https://github.com/actions/setup-go/blob/b7ad1dad31e06c5925ef5d2fc7ad053ef454303e/README.md#L44),
		// so one beside go.mod is the Go the step installs.
		if _, set := s.input("go-version"); t.fromGoMod == "go" && set {
			r.problems = append(r.problems, fmt.Sprintf("%s sets go-version, which setup-go reads before %s; "+
				"pin Go by go.mod alone", at, key))
		}
	}
	pinsInputCase(at, s.With, r)
	if _, known := pinsNotInstallers[action]; !installer && !known {
		r.problems = append(r.problems, fmt.Sprintf("%s is not classified: add it to pinsTools with the "+
			"input that pins its tool, or to pinsNotInstallers with the reason", at))
	}
	for key := range s.With {
		if !pinsVersionInput(key) {
			continue
		}
		if v, _ := s.input(key); !pinsExact.MatchString(w.resolveEnv(v)) {
			r.problems = append(r.problems, fmt.Sprintf("%s: %s is %q, which is not one exact version", at, key, v))
		}
	}
}

// pinsEnv reads the tool versions an env block names.
func pinsEnv(path string, env map[string]string, r *pinsReport, add func(tool, version, at string)) {
	for _, key := range slices.Sorted(maps.Keys(env)) {
		if !pinsVersionEn.MatchString(key) {
			continue
		}
		tool := pinsToolByEnv(key)
		if tool == "" {
			r.problems = append(r.problems, fmt.Sprintf("%s: the env value %s names a version of a tool "+
				"pinsTools does not know", path, key))
			continue
		}
		add(tool, env[key], path+": "+key)
	}
}

// pinsRunLines reads `go run module@version` out of a run block.
func pinsRunLines(run, path string, r *pinsReport, add func(tool, version, at string)) {
	for _, line := range workflowRunLines(run) {
		for _, gr := range goRunsIn(line) {
			module, version, ok := strings.Cut(gr.pkg, "@")
			switch tool := pinsToolByModule(module); {
			case !ok:
			case tool != "":
				add(tool, version, path+": go run "+module)
			default:
				r.problems = append(r.problems, fmt.Sprintf("%s: go run %s names a tool pinsTools does not know",
					path, gr.pkg))
			}
		}
	}
}

// pinsGoVersionFile is the Go version a go-version-file gives, read as
// setup-go reads a go.mod, or why it gives none. gomod is the
// repository's go.mod, the one file the Go version is held to
// (https://github.com/actions/setup-go/blob/b7ad1dad31e06c5925ef5d2fc7ad053ef454303e/src/installer.ts#L645,
// setup-go v7.0.0).
func pinsGoVersionFile(file, gomod string) (version, problem string) {
	if path.Clean(file) != pinsGoMod {
		return "", "which is not the repository's " + pinsGoMod
	}
	if v := pinsGoVersion(gomod); v != "" {
		return v, ""
	}
	return "", "which names no Go version"
}

// pinsGoVersion is the Go a go.mod runs: its toolchain directive, or
// else its go line. setup-go installs that, and the go
// command switches to it.
func pinsGoVersion(gomod string) string {
	if m := pinsGoModToolchain.FindStringSubmatch(gomod); m != nil {
		return m[1]
	}
	if m := pinsGoModGo.FindStringSubmatch(gomod); m != nil {
		return m[1]
	}
	return ""
}

// pinsMakefileVars reads every `module@version` variable, and returns the
// variable that names each module@version.
func pinsMakefileVars(mk *makefile, r *pinsReport, add func(tool, version, at string)) map[string]string {
	pinnedBy := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(mk.vars)) {
		// Recipes are read expanded, so a version kept in another
		// variable is compared as the recipe runs it.
		value := mk.expand(mk.vars[name])
		m := pinsModuleAt.FindStringSubmatch(value)
		if m != nil {
			pinnedBy[m[0]] = name
		}
		if strings.Contains(value, "@latest") {
			r.problems = append(r.problems, fmt.Sprintf("%s: %s is pinned to @latest", pinsMakefile, name))
			continue
		}
		if m == nil {
			continue
		}
		tool := pinsToolByModule(m[1])
		if tool == "" {
			r.problems = append(r.problems, fmt.Sprintf("%s: %s names %s, which pinsTools does not know",
				pinsMakefile, name, m[1]))
			continue
		}
		add(tool, m[2], pinsMakefile+": "+name)
	}
	return pinnedBy
}

// pinsMakefileRecipes holds what the recipes run with `go run`. A module
// written into a recipe is a pin of its own, and a variable in pinnedBy
// that no recipe runs pins nothing: the tool then runs from the PATH.
func pinsMakefileRecipes(mk *makefile, pinnedBy map[string]string, r *pinsReport, add func(tool, version, at string)) {
	ran := map[string]bool{}
	for _, target := range slices.Sorted(maps.Keys(mk.targets)) {
		for i, line := range mk.recipe(target) {
			at := fmt.Sprintf("%s:%d", pinsMakefile, mk.targets[target].lines[i])
			// A go run at @latest is the go run rule's to report.
			if !slices.ContainsFunc(goRunsIn(line), func(gr goRun) bool { return strings.HasSuffix(gr.pkg, "@latest") }) {
				pinsFloating(at, line, r)
			}
			pinsFromPath(at, line, "run it with $(GO) run and the variable that pins it", r)
			for _, gr := range goRunsIn(line) {
				module, version, ok := strings.Cut(gr.pkg, "@")
				if !ok {
					continue
				}
				ran[gr.pkg] = true
				if _, named := pinnedBy[gr.pkg]; named {
					continue
				}
				if tool := pinsToolByModule(module); tool != "" {
					add(tool, version, pinsMakefile+": "+target)
				} else {
					r.problems = append(r.problems, fmt.Sprintf("%s: %s runs go run %s, a tool pinsTools does not know",
						pinsMakefile, target, gr.pkg))
				}
			}
		}
	}
	for _, module := range slices.Sorted(maps.Keys(pinnedBy)) {
		if !ran[module] {
			r.problems = append(r.problems, fmt.Sprintf("%s: %s pins %s and no recipe runs it with go run, "+
				"so the tool runs from the PATH", pinsMakefile, pinnedBy[module], module))
		}
	}
}

// pinsGoModVersions reads the Go a go.mod runs and its required modules.
func pinsGoModVersions(gomod string, add func(tool, version, at string)) {
	for _, t := range pinsTools {
		if t.fromGoMod == "go" {
			add(t.name, pinsGoVersion(gomod), pinsGoMod+": go")
		}
	}
	for line := range strings.SplitSeq(gomod, "\n") {
		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "require")))
		for _, t := range pinsTools {
			if t.fromGoMod == "" || t.fromGoMod == "go" || len(fields) < 2 {
				continue
			}
			if fields[0] == t.fromGoMod {
				add(t.name, fields[1], pinsGoMod+": "+t.fromGoMod)
			}
		}
	}
}

func pinsToolByModule(module string) string {
	for _, t := range pinsTools {
		for _, m := range t.modules {
			if strings.Contains(module, m) {
				return t.name
			}
		}
	}
	return ""
}

func pinsToolByEnv(key string) string {
	for _, t := range pinsTools {
		if slices.ContainsFunc(t.envKeys, func(k string) bool { return strings.EqualFold(k, key) }) {
			return t.name
		}
	}
	return ""
}

func pinsNormal(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// pinsFloating refuses a line that fetches a tool at whatever version is
// newest.
func pinsFloating(at, line string, r *pinsReport) {
	if floating := floatingDownload.FindString(line); floating != "" {
		r.problems = append(r.problems, fmt.Sprintf("%s: %s installs whatever is newest. Name the version: "+
			"what builds a release must not float", at, floating))
	}
}

// pinsFromPath refuses each word of a command line that names a tool
// pinsTools knows, by its name or a path ending in it, wherever it sits:
// a pipeline, a substitution and `env -i tool` run it from the PATH as a
// first word does. The go command is go.mod's, and the module a go run
// names is the run that pins the tool. remedy says how to run it instead.
func pinsFromPath(at, line, remedy string, r *pinsReport) {
	words := shellSplit(line)
	module := map[string]bool{}
	for _, run := range goRunsOf(words) {
		module[run.pkg] = true
	}
	named := map[string]bool{}
	for _, w := range words {
		tool := path.Base(w.text)
		if tool == "go" || module[w.text] || named[tool] ||
			!slices.ContainsFunc(pinsTools, func(t pinsTool) bool { return t.name == tool }) {
			continue
		}
		named[tool] = true
		r.problems = append(r.problems, fmt.Sprintf("%s: names %s as a word of its own, which runs it from the "+
			"PATH at whatever version is installed there; %s", at, tool, remedy))
	}
}

// pinsTextVersions records a known tool's module@version on any line of
// text but a comment line, whatever the other rules made of the line, so
// a copy they do not read, such as an env value or a bash -c, is held to
// the tool's one version. An action is owner/repo and a Go module starts
// with a domain, so only the second is read.
func pinsTextVersions(file, text string, add func(tool, version, at string)) {
	for i, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			pinsModules(fmt.Sprintf("%s:%d", file, i+1), line, add)
		}
	}
}

// pinsModules records each known tool's module@version in text, at at. A
// version at latest is the floating rule's.
func pinsModules(at, text string, add func(tool, version, at string)) {
	for _, m := range pinsModuleAt.FindAllStringSubmatch(text, -1) {
		first, _, _ := strings.Cut(m[1], "/")
		version := strings.TrimRight(m[2], "\"'`),;")
		// A version still written as a reference is one the structured
		// rules resolve, or refuse.
		unresolved := strings.ContainsAny(version, "$({")
		if tool := pinsToolByModule(m[1]); tool != "" && strings.Contains(first, ".") && version != "latest" && !unresolved {
			add(tool, version, at+": "+m[1])
		}
	}
}

// pinsGoRun is one module@version the go command runs or installs.
type pinsGoRun struct {
	verb, pkg string
}

// pinsGoRuns is each module@version a command line runs or installs with
// the go command, read as the shell splits it, its flags skipped. A
// package of this module has no version and is not one.
func pinsGoRuns(line string) []pinsGoRun {
	var out []pinsGoRun
	for _, run := range goRunsIn(line) {
		if strings.Contains(run.pkg, "@") {
			out = append(out, pinsGoRun{"run", run.pkg})
		}
	}
	for _, c := range goCommands(line) {
		if c.sub != "install" {
			continue
		}
		for _, pkg := range goAfterFlags(c.args) {
			if strings.Contains(pkg, "@") {
				out = append(out, pinsGoRun{"install", pkg})
			}
		}
	}
	return out
}

// pinsGoreleaser holds each command the goreleaser config runs in the
// release as a workflow's run line is held: nothing fetched at whatever
// version is newest, each module@version the go command runs or installs a
// pin of a tool pinsTools knows, and a known tool's module@version
// anywhere in the line, such as in a bash -c, a pin as well. A sign's
// cosign and an SBOM's syft run from the PATH the release job's installers
// set, so the PATH rule is not applied here.
func pinsGoreleaser(g goreleaserFile, r *pinsReport, add, text func(tool, version, at string)) {
	r.problems = append(r.problems, g.refused...)
	for _, run := range g.runs {
		at := fmt.Sprintf("%s:%d", releaseGoreleaser, run.line)
		pinsFloating(at, run.text, r)
		for _, gr := range pinsGoRuns(run.text) {
			module, version, _ := strings.Cut(gr.pkg, "@")
			switch tool := pinsToolByModule(module); {
			case tool == "":
				r.problems = append(r.problems, fmt.Sprintf("%s: go %s %s names a tool pinsTools does not know",
					at, gr.verb, gr.pkg))
			case version != "latest":
				add(tool, version, at+": go "+gr.verb+" "+module)
			}
		}
		pinsModules(at, run.text, text)
	}
}

// pinsHook holds the pre-commit hook's lines as a recipe's are held: no
// known tool from the PATH, nothing at whatever version is newest, and a
// known tool's module@version held to its one version.
func pinsHook(text string, r *pinsReport, add func(tool, version, at string)) {
	for i, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		at := fmt.Sprintf("%s:%d", parityHook, i+1)
		pinsFloating(at, line, r)
		pinsFromPath(at, line, "run it through a make target that pins it", r)
		pinsModules(at, line, add)
	}
}

// pinsGoSource records each known tool's module@version in a string
// literal of the maintainer tooling's Go source, so a copy of a pin in a
// gate is held to the Makefile's. A comment, a test and testdata pin
// nothing.
func pinsGoSource(root string, add func(tool, version, at string)) error {
	return filepath.WalkDir(filepath.Join(root, pinsScripts), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		// A slash name, so a position reads the same on every OS.
		name := filepath.ToSlash(rel)
		src, err := os.ReadFile(p) //nolint:gosec // a file under scripts/
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil {
				pinsModules(fmt.Sprintf("%s:%d", name, fset.Position(lit.Pos()).Line), v, add)
			}
			return true
		})
		return nil
	})
}

// pinsExpanded is the Makefile's text with each line expanded as make
// expands it, line for line, so a version kept in a variable is read as
// the line runs it.
func pinsExpanded(mk *makefile) string {
	lines := strings.Split(mk.text, "\n")
	for i, line := range lines {
		lines[i] = mk.expand(line)
	}
	return strings.Join(lines, "\n")
}
