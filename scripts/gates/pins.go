package main

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
//   - one tool pinned to two versions in two files, so a rehearsal or a
//     pre-commit scan runs something other than CI.
//
// Every action is classified as an installer or not, and an unknown one
// fails: being unclassified is how an unpinned tool slips through.

const (
	pinsMakefile = "Makefile"
	pinsGoMod    = "go.mod"
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
	// pinsUsesLine is a `uses:` line, its value read without the quotes
	// YAML allows around it.
	pinsUsesLine  = regexp.MustCompile(`^\s*-?\s*uses:\s*["']?([^\s#"']+)["']?\s*(#.*)?$`)
	pinsSHA       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pinsExact     = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)
	pinsModuleAt  = regexp.MustCompile(`([A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+)@(\S+)`)
	pinsSemverIn  = regexp.MustCompile(`v?\d+\.\d+\.\d+`)
	pinsVersionEn = regexp.MustCompile(`_VERSION$`)
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

	r := &pinsReport{tools: map[string][]pinsFound{}, placesIn: map[string]int{}, installed: map[string]bool{},
		workflows: len(flows)}
	// addIn records the pins read from one file.
	addIn := func(file string) func(tool, version, at string) {
		return func(tool, version, at string) {
			r.tools[tool] = append(r.tools[tool], pinsFound{version, at})
			r.places++
			r.placesIn[file]++
			if !pinsExact.MatchString(version) {
				r.problems = append(r.problems, fmt.Sprintf("%s: %s is pinned to %q, which is not one exact version",
					at, tool, version))
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
	}
	pinnedBy := pinsMakefileVars(mk, r, addIn(pinsMakefile))
	pinsMakefileRecipes(mk, pinnedBy, r, addIn(pinsMakefile))
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
	pinsUsesLines(w, r, add)

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

// pinsUsesLines reads the literal `uses:` lines, for the SHA and its
// comment.
func pinsUsesLines(w workflow, r *pinsReport, add func(tool, version, at string)) {
	for i, line := range strings.Split(w.text, "\n") {
		at := fmt.Sprintf("%s:%d", w.path, i+1)
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "@latest") {
			r.problems = append(r.problems, at+": something is pinned to @latest")
		}
		m := pinsUsesLine.FindStringSubmatch(line)
		if m == nil || strings.HasPrefix(m[1], "./") {
			continue
		}
		r.actions++
		if strings.HasPrefix(m[1], "docker://") {
			continue
		}
		action, ref, ok := strings.Cut(m[1], "@")
		comment := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m[2]), "#"))
		switch {
		case !ok:
			r.problems = append(r.problems, fmt.Sprintf("%s: %s has no ref at all", at, action))
		case !pinsSHA.MatchString(ref):
			r.problems = append(r.problems, fmt.Sprintf("%s: %s is pinned to %q, a tag or branch that "+
				"can be moved; pin the 40-character commit SHA", at, action, ref))
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
	}
	if _, known := pinsNotInstallers[action]; !installer && !known {
		r.problems = append(r.problems, fmt.Sprintf("%s is not classified: add it to pinsTools with the "+
			"input that pins its tool, or to pinsNotInstallers with the reason", at))
	}
	for key := range s.With {
		if key == "go-version-file" || (!strings.HasSuffix(key, "version") && !strings.HasSuffix(key, "release")) {
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
		for _, line := range mk.recipe(target) {
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
		if slices.Contains(t.envKeys, key) {
			return t.name
		}
	}
	return ""
}

func pinsNormal(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }
