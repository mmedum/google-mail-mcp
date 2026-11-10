package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// One reader for a GitHub workflow, shared by pins, parity and release.
// It parses YAML rather than splitting lines: a line splitter reads a
// `version:` under `env:` as a tool pin, and understands only one of the
// shapes a sequence can be written in, so a file written another way
// yields no steps and therefore no problems.

// workflowDir is where the workflows live, relative to the root.
const workflowDir = ".github/workflows"

// workflow is the part of a workflow file the gates read.
type workflow struct {
	// path is where it was read from, slash-separated, for messages.
	path string
	// text is the raw file, for the checks that are about literal lines,
	// and node its parse, for the comment after a SHA wherever YAML lets a
	// `uses:` value sit.
	text string
	node yaml.Node
	// On is left as a node: `on` is a union of shapes.
	On          yaml.Node         `yaml:"on"`
	Env         map[string]string `yaml:"env"`
	Defaults    workflowDefaults  `yaml:"defaults"`
	Permissions yaml.Node         `yaml:"permissions"`
	Jobs        map[string]struct {
		Env  map[string]string `yaml:"env"`
		Uses string            `yaml:"uses"`
		// With is a reusable workflow call's inputs.
		With        map[string]any   `yaml:"with"`
		Steps       []workflowStep   `yaml:"steps"`
		Defaults    workflowDefaults `yaml:"defaults"`
		Permissions yaml.Node        `yaml:"permissions"`
		If          string           `yaml:"if"`
		// ContinueOnError is a bool or an expression.
		ContinueOnError yaml.Node `yaml:"continue-on-error"`
		// Container and each service are an image, or a mapping with
		// an image: key.
		Container yaml.Node            `yaml:"container"`
		Services  map[string]yaml.Node `yaml:"services"`
	} `yaml:"jobs"`
}

// workflowDefaults is a workflow's or a job's `defaults:`.
type workflowDefaults struct {
	Run struct {
		Shell string `yaml:"shell"`
	} `yaml:"run"`
}

// workflowStep is one entry under a job's `steps:`.
type workflowStep struct {
	Name  string            `yaml:"name"`
	Uses  string            `yaml:"uses"`
	Run   string            `yaml:"run"`
	Shell string            `yaml:"shell"`
	With  map[string]any    `yaml:"with"`
	Env   map[string]string `yaml:"env"`
	If    string            `yaml:"if"`
	// ContinueOnError is a bool or an expression.
	ContinueOnError yaml.Node `yaml:"continue-on-error"`
	// job names the job the step belongs to.
	job string
}

// input reads one `with:` value as text, its name matched in any case as
// the runner reads input names (upper case, into INPUT_ variables). Values
// are not all strings (`fetch-depth: 0`), so they are rendered rather than
// asserted.
func (s workflowStep) input(key string) (string, bool) {
	return workflowInput(s.With, key)
}

// workflowInput reads one input of a with: block, its name in any case.
func workflowInput(with map[string]any, key string) (string, bool) {
	for name, v := range with {
		if strings.EqualFold(name, key) {
			return strings.TrimSpace(fmt.Sprint(v)), true
		}
	}
	return "", false
}

// action is the step's action without its ref: "actions/checkout".
func (s workflowStep) action() string {
	a, _, _ := strings.Cut(s.Uses, "@")
	return a
}

// steps is every step in the file, jobs in name order.
func (w workflow) steps() []workflowStep {
	var out []workflowStep
	for _, name := range slices.Sorted(maps.Keys(w.Jobs)) {
		for _, s := range w.Jobs[name].Steps {
			s.job = name
			out = append(out, s)
		}
	}
	return out
}

// resolveEnv turns `${{ env.NAME }}` into the value the workflow's env
// block gives it. Anything else comes back unchanged, and so does a
// reference to a name nobody set, so it fails as a version rather than
// passing on a value that does not exist.
func (w workflow) resolveEnv(value string) string {
	v := strings.TrimSpace(value)
	inner, ok := strings.CutPrefix(v, "${{")
	if !ok {
		return value
	}
	inner, ok = strings.CutSuffix(inner, "}}")
	if !ok {
		return value
	}
	name, ok := strings.CutPrefix(strings.TrimSpace(inner), "env.")
	if !ok {
		return value
	}
	if got, ok := w.Env[name]; ok {
		return got
	}
	return value
}

// triggersOnTag reports whether pushing a tag starting with prefix
// starts this workflow.
func (w workflow) triggersOnTag(prefix string) bool {
	push := workflowMappingValue(&w.On, "push")
	tags := workflowMappingValue(push, "tags")
	if tags == nil {
		return false
	}
	for _, pattern := range tags.Content {
		if strings.HasPrefix(pattern.Value, prefix) {
			return true
		}
	}
	return strings.HasPrefix(tags.Value, prefix)
}

// workflowImage is the image a job's container or service names, or empty
// when there is none.
func workflowImage(node *yaml.Node) string {
	node = workflowResolve(node)
	if node != nil && node.Kind == yaml.MappingNode {
		node = workflowResolve(workflowMappingValue(node, "image"))
	}
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(node.Value)
}

// workflowResolve is the node an alias such as *img stands for, or node
// itself when it is not an alias.
func workflowResolve(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

// workflowMappingValue reads one key out of a YAML mapping node.
func workflowMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// readWorkflow parses one workflow file. name is the path to report.
func readWorkflow(path, name string) (workflow, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a path under .github/workflows
	if err != nil {
		return workflow{}, err
	}
	// yaml.v3 and GitHub's parser need not agree on every line break, so a
	// file holding one either may read apart is refused rather than read.
	var stray []string
	yamlStrayBreaks(string(data), func(n int, format string, a ...any) {
		stray = append(stray, fmt.Sprintf("%s:%d: %s", name, n, fmt.Sprintf(format, a...)))
	})
	if len(stray) > 0 {
		return workflow{}, errors.New(strings.Join(stray, "\n"))
	}
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		return workflow{}, fmt.Errorf("%s is not valid YAML: %w", name, err)
	}
	if err := yaml.Unmarshal(data, &w.node); err != nil {
		return workflow{}, fmt.Errorf("%s is not valid YAML: %w", name, err)
	}
	w.path = name
	w.text = string(data)
	return w, nil
}

// readWorkflows reads every workflow under root, sorted by path.
func readWorkflows(root string) ([]workflow, error) {
	var paths []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		found, err := filepath.Glob(filepath.Join(root, workflowDir, pattern))
		if err != nil {
			return nil, err
		}
		paths = append(paths, found...)
	}
	slices.Sort(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no workflows under %s", workflowDir)
	}
	var out []workflow
	for _, p := range paths {
		w, err := readWorkflow(p, workflowDir+"/"+filepath.Base(p))
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

// workflowMatrixFilter is a condition that picks legs of a matrix, such as
// `matrix.os == 'ubuntu-latest'`, with or without the expression syntax
// around it: some leg runs on every push.
var workflowMatrixFilter = regexp.MustCompile(`^(?:\$\{\{\s*)?matrix\.[A-Za-z0-9_.-]+\s*[!=]=\s*'[^']*'(?:\s*\}\})?$`)

// onPush reports whether a step runs on every push to main: it and its
// job carry no condition but a matrix filter, and neither lets its failure
// pass (https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idcontinue-on-error).
// Any other condition, a pull request's or `false`, can keep it from
// running, so it does not count.
func (w workflow) onPush(s workflowStep) bool {
	job := w.Jobs[s.job]
	for _, cond := range []string{job.If, s.If} {
		if c := strings.TrimSpace(cond); c != "" && !workflowMatrixFilter.MatchString(c) {
			return false
		}
	}
	for _, c := range []*yaml.Node{&job.ContinueOnError, &s.ContinueOnError} {
		if c.Kind != 0 && c.Value != "false" {
			return false
		}
	}
	return true
}

// workflowNamed finds one workflow by its base name.
func workflowNamed(flows []workflow, base string) (workflow, bool) {
	for _, w := range flows {
		if strings.HasSuffix(w.path, "/"+base) {
			return w, true
		}
	}
	return workflow{}, false
}

// workflowRunLines is the commands of a `run:` block: comments, blank
// lines and continuation breaks removed, so a word in a comment is not
// read as a command.
func workflowRunLines(run string) []string {
	var out []string
	var pending string
	for line := range strings.SplitSeq(run, "\n") {
		t := strings.TrimSpace(line)
		if pending == "" && (t == "" || strings.HasPrefix(t, "#")) {
			continue
		}
		if cont, ok := strings.CutSuffix(t, "\\"); ok {
			pending += strings.TrimRight(cont, " \t") + " "
			continue
		}
		out = append(out, strings.TrimSpace(pending+t))
		pending = ""
	}
	if strings.TrimSpace(pending) != "" {
		out = append(out, strings.TrimSpace(pending))
	}
	return out
}

// loneCR names a CR that is not half of a CRLF pair.
const loneCR = "a CR with no LF after it"

// yamlBreaks are the line breaks YAML parsers do not all agree on, each by
// its name; LF and CRLF are read alike everywhere. YAML 1.2 breaks a line
// at a lone CR too (https://yaml.org/spec/1.2.2/#54-line-break-characters),
// and YAML 1.1 at NEL, LS and PS as well
// (https://yaml.org/spec/1.1/#id871136), so a line after one may be read
// by one parser as a line and by another as the rest of the line before.
var yamlBreaks = map[rune]string{
	'\r':     loneCR,
	'\u0085': "U+0085 (NEXT LINE)",
	'\u2028': "U+2028 (LINE SEPARATOR)",
	'\u2029': "U+2029 (PARAGRAPH SEPARATOR)",
}

// strayBreak is a line break the readers do not agree on: its name, the
// line it sits on, counted at LF, and the byte of the line it starts at,
// from 1.
type strayBreak struct {
	name     string
	line, at int
}

// strayBreaks is each rune of text that breaks names, but a CR followed by
// an LF.
func strayBreaks(text string, breaks map[rune]string) []strayBreak {
	var out []strayBreak
	line, start := 1, 0
	for i, r := range text {
		name, isBreak := breaks[r]
		switch {
		case r == '\n':
			line, start = line+1, i+1
		case !isBreak, r == '\r' && strings.HasPrefix(text[i+1:], "\n"):
		default:
			out = append(out, strayBreak{name, line, i - start + 1})
		}
	}
	return out
}

// yamlStrayBreaks refuses, through refuse, each line break of text that
// YAML parsers do not all read alike, by its line and its byte.
func yamlStrayBreaks(text string, refuse func(n int, format string, a ...any)) {
	for _, b := range strayBreaks(text, yamlBreaks) {
		refuse(b.line, "%s at byte %d of the line, which one YAML parser may read as a line break and another not; "+
			"break lines with LF or CRLF alone", b.name, b.at)
	}
}
