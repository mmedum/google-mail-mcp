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

// The pins gate holds every third-party tool to one exact version, the
// same version everywhere it is named, and the version docs/architecture.md
// §5a says was checked against upstream.
//
// Five ways a version floats, each a rule:
//   - an action used by tag or branch rather than a 40-hex commit SHA,
//     or a SHA with no comment saying which version it is;
//   - an action that installs a tool without naming the tool's version:
//     a SHA pins the wrapper, not the tool;
//   - a version input, env value or `go run module@v` that is a range, a
//     major line or `latest`;
//   - one tool pinned to two versions in two files, so a rehearsal or a
//     pre-commit scan runs something other than CI;
//   - a pin that differs from the §5a table, which then misdescribes
//     what ships.
//
// Every action is classified as an installer or not, and an unknown one
// fails: being unclassified is how an unpinned tool slips through.

const (
	pinsMakefile     = "Makefile"
	pinsGoMod        = "go.mod"
	pinsArchitecture = "docs/architecture.md"
)

// pinsRequiredWorkflows must exist; a gate that counted files would
// pass with the wrong two.
var pinsRequiredWorkflows = []string{"ci.yml", "codeql.yml", "release.yml", "publish-mcp.yml"}

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
	// fromGoMod reads the version from go.mod: "go" or a module path.
	fromGoMod string
}

var pinsTools = []pinsTool{
	{name: "go", fromGoMod: "go"},
	{name: "mcp-go-sdk", fromGoMod: "github.com/modelcontextprotocol/go-sdk"},
	{name: "golangci-lint", modules: []string{"golangci-lint"},
		actions: map[string]string{"golangci/golangci-lint-action": "version"}},
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
	"actions/setup-go":                "installs Go from go-version-file, which is go.mod: the pin is the file",
	"github/codeql-action/init":       "CodeQL's bundle is GitHub's to manage; the action's own version is held by comment",
	"github/codeql-action/autobuild":  "builds with the Go already installed",
	"github/codeql-action/analyze":    "uploads results",
}

// pinsDocOnly are §5a pin-table rows no workflow or Makefile names, each
// with the gate that holds them instead.
var pinsDocOnly = map[string]string{
	"mcpb manifest schema": "the mcpb gate holds the manifest's $schema URL to its tag",
}

var (
	pinsUsesLine  = regexp.MustCompile(`^\s*-?\s*uses:\s*([^\s#]+)\s*(#.*)?$`)
	pinsSHA       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pinsExact     = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)
	pinsModuleAt  = regexp.MustCompile(`([A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+)@(\S+)`)
	pinsSemverIn  = regexp.MustCompile(`v?\d+\.\d+\.\d+`)
	pinsVersionEn = regexp.MustCompile(`_VERSION$`)
)

// pinsFound is one place a tool's version is written.
type pinsFound struct {
	version, at string
}

func pins(out io.Writer, _ []string) error {
	report, err := pinsCheck(".")
	if err != nil {
		return err
	}
	if err := problemsError(out, "the pins", report.problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "pins ok: %d workflows, %d actions by SHA, %d installers name their tool, "+
		"%d tools pinned in %d places, %d §5a rows held\n",
		report.workflows, report.actions, report.installers, len(report.tools), report.places, report.docRows)
	return nil
}

// pinsReport is what the gate read and found.
type pinsReport struct {
	problems                               []string
	workflows, actions, installers, places int
	docRows                                int
	tools                                  map[string][]pinsFound
}

// pinsCheck runs every rule over the repository at root.
func pinsCheck(root string) (*pinsReport, error) {
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
	doc, err := os.ReadFile(filepath.Join(root, pinsArchitecture))
	if err != nil {
		return nil, err
	}

	r := &pinsReport{tools: map[string][]pinsFound{}, workflows: len(flows)}
	add := func(tool, version, at string) {
		r.tools[tool] = append(r.tools[tool], pinsFound{version, at})
		r.places++
		if !pinsExact.MatchString(version) {
			r.problems = append(r.problems, fmt.Sprintf("%s: %s is pinned to %q, which is not one exact version",
				at, tool, version))
		}
	}

	for _, base := range pinsRequiredWorkflows {
		if _, ok := workflowNamed(flows, base); !ok {
			r.problems = append(r.problems, fmt.Sprintf("%s/%s does not exist", workflowDir, base))
		}
	}
	for _, w := range flows {
		pinsWorkflow(w, r, add)
	}
	pinsMakefileVars(mk, r, add)
	pinsGoModVersions(string(gomod), add)

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
	r.problems = append(r.problems, pinsAgainstDoc(string(doc), r)...)

	// Floors: zero findings and zero inputs look the same otherwise.
	if r.actions < 10 {
		r.problems = append(r.problems, fmt.Sprintf("read only %d action references; the reader is not "+
			"seeing the workflows", r.actions))
	}
	if r.installers < 4 {
		r.problems = append(r.problems, fmt.Sprintf("found %d tool-installing steps, want at least 4 "+
			"(goreleaser, cosign, syft and cosign again for publishing)", r.installers))
	}
	return r, nil
}

// pinsWorkflow applies the workflow rules to one file.
func pinsWorkflow(w workflow, r *pinsReport, add func(tool, version, at string)) {
	pinsUsesLines(w, r, add)

	// The structure, for which input belongs to which step.
	envs := []map[string]string{w.Env}
	for _, j := range w.Jobs {
		envs = append(envs, j.Env)
	}
	for _, s := range w.steps() {
		envs = append(envs, s.Env)
		if s.Uses == "" || strings.HasPrefix(s.Uses, "./") {
			pinsRunLines(s.Run, w.path, r, add)
			continue
		}
		pinsStep(w, s, r, add)
	}
	for _, env := range envs {
		pinsEnv(w.path, env, r, add)
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
func pinsStep(w workflow, s workflowStep, r *pinsReport, add func(tool, version, at string)) {
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
		v, set := s.input(key)
		if !set {
			r.problems = append(r.problems, fmt.Sprintf("%s is pinned by SHA and does not set %s, so the "+
				"tool it installs is whatever is current that morning", at, key))
			continue
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
		if !strings.Contains(line, "go run") {
			continue
		}
		for _, m := range pinsModuleAt.FindAllStringSubmatch(line, -1) {
			if tool := pinsToolByModule(m[1]); tool != "" {
				add(tool, m[2], path+": go run "+m[1])
			} else {
				r.problems = append(r.problems, fmt.Sprintf("%s: go run %s@%s names a tool pinsTools does not know",
					path, m[1], m[2]))
			}
		}
	}
}

// pinsMakefileVars reads every `module@version` variable.
func pinsMakefileVars(mk *makefile, r *pinsReport, add func(tool, version, at string)) {
	for _, name := range slices.Sorted(maps.Keys(mk.vars)) {
		value := mk.vars[name]
		if strings.Contains(value, "@latest") {
			r.problems = append(r.problems, fmt.Sprintf("%s: %s is pinned to @latest", pinsMakefile, name))
			continue
		}
		m := pinsModuleAt.FindStringSubmatch(value)
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
}

// pinsGoModVersions reads the Go directive and required modules.
func pinsGoModVersions(gomod string, add func(tool, version, at string)) {
	for line := range strings.SplitSeq(gomod, "\n") {
		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "require")))
		for _, t := range pinsTools {
			if t.fromGoMod == "" || len(fields) < 2 {
				continue
			}
			if fields[0] == t.fromGoMod {
				add(t.name, fields[1], pinsGoMod+": "+t.fromGoMod)
			}
		}
	}
}

// pinsAgainstDoc holds the §5a pin table to what the files pin.
func pinsAgainstDoc(doc string, r *pinsReport) []string {
	rows := pinsDocTable(doc)
	if len(rows) < 10 {
		return []string{fmt.Sprintf("%s: read %d rows of the §5a pin table, want at least 10", pinsArchitecture, len(rows))}
	}
	byDoc := map[string]string{
		"go": "go", "mcp go sdk": "mcp-go-sdk", "golangci-lint": "golangci-lint", "goreleaser": "goreleaser",
		"cosign": "cosign", "syft": "syft", "mcp-publisher": "mcp-publisher", "gitleaks": "gitleaks",
		"govulncheck": "govulncheck", "go-licenses": "go-licenses", "actionlint": "actionlint",
		"codeql-action": "codeql-action",
	}
	var problems []string
	for _, row := range rows {
		if _, ok := pinsDocOnly[row.tool]; ok {
			r.docRows++
			continue
		}
		tool, ok := byDoc[strings.ToLower(row.tool)]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: the §5a pin table names %q, which the pins gate "+
				"does not know", pinsArchitecture, row.tool))
			continue
		}
		r.docRows++
		want := pinsSemverIn.FindString(row.version)
		if want == "" {
			continue // a row that defers its version, such as go-licenses to §17
		}
		for _, f := range r.tools[tool] {
			if pinsNormal(f.version) != pinsNormal(want) {
				problems = append(problems, fmt.Sprintf("%s pins %s %s and §5a says %s", f.at, tool, f.version, want))
			}
		}
	}
	return problems
}

// pinsDocRow is one row of the §5a pin table.
type pinsDocRow struct{ tool, version string }

// pinsDocTable reads the table whose header is `| Tool | Version |`.
func pinsDocTable(doc string) []pinsDocRow {
	var rows []pinsDocRow
	inside := false
	for line := range strings.SplitSeq(doc, "\n") {
		t := strings.TrimSpace(line)
		if t == "| Tool | Version |" {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if !strings.HasPrefix(t, "|") {
			break
		}
		cells := strings.Split(strings.Trim(t, "|"), "|")
		if len(cells) != 2 || strings.HasPrefix(strings.TrimSpace(cells[0]), "---") {
			continue
		}
		rows = append(rows, pinsDocRow{strings.TrimSpace(cells[0]), strings.TrimSpace(cells[1])})
	}
	return rows
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
