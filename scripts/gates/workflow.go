package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
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
	// text is the raw file, for the checks that are about literal lines:
	// a SHA and the comment after it.
	text string
	// On is left as a node: `on` is a union of shapes.
	On   yaml.Node         `yaml:"on"`
	Env  map[string]string `yaml:"env"`
	Jobs map[string]struct {
		Env   map[string]string `yaml:"env"`
		Uses  string            `yaml:"uses"`
		Steps []workflowStep    `yaml:"steps"`
	} `yaml:"jobs"`
}

// workflowStep is one entry under a job's `steps:`.
type workflowStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
	// job names the job the step belongs to.
	job string
}

// input reads one `with:` value as text. Values are not all strings
// (`fetch-depth: 0`), so they are rendered rather than asserted.
func (s workflowStep) input(key string) (string, bool) {
	v, ok := s.With[key]
	if !ok {
		return "", false
	}
	return strings.TrimSpace(fmt.Sprint(v)), true
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
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
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
