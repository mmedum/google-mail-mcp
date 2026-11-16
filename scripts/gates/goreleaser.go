package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// One reader for .goreleaser.yaml, shared by the release and pins gates:
// the part of the config the release gate holds, and every command the
// release runs, read off the same parse.

// goreleaserChecked is the goreleaser version whose source the command
// key paths and the template rule are checked against. The pins gate
// holds it to the version release.yml runs.
const goreleaserChecked = "v2.18.2"

// goreleaserFile is the parsed config: the fields the release gate reads,
// the commands the release runs, and the shapes the reader refuses.
type goreleaserFile struct {
	cfg releaseConfig
	// runs are the command lines the release runs: a hook, or a sign's, an
	// SBOM's or a publisher's command with its arguments.
	runs    []goreleaserRun
	refused []string
}

// goreleaserRun is one command line and the line of the file it is on.
type goreleaserRun struct {
	line int
	text string
}

// goreleaserLines are the key paths whose value is a command line, as
// goreleaser v2.18.2's config reads them
// (https://github.com/goreleaser/goreleaser/blob/v2.18.2/pkg/config/config.go):
// a hook as a string, a list of strings or a list of {cmd: …}.
var goreleaserLines = [][]string{
	{"before", "hooks"},
	{"builds", "hooks", "pre"}, {"builds", "hooks", "post"},
	{"universal_binaries", "hooks", "pre"}, {"universal_binaries", "hooks", "post"},
	{"dockers_v2", "hooks", "pre"}, {"dockers_v2", "hooks", "post"},
	{"publishers"},
}

// goreleaserWords are the blocks whose command is cmd: with args: after it.
var goreleaserWords = []string{"signs", "docker_signs", "binary_signs", "sboms"}

// readGoreleaser reads the goreleaser config under root, refusing a line
// break YAML parsers do not all read alike.
func readGoreleaser(root string) (goreleaserFile, error) {
	data, err := os.ReadFile(filepath.Join(root, releaseGoreleaser)) //nolint:gosec // a repository path
	if err != nil {
		return goreleaserFile{}, err
	}
	var g goreleaserFile
	refuse := func(n int, format string, a ...any) {
		g.refused = append(g.refused, fmt.Sprintf("%s:%d: %s", releaseGoreleaser, n, fmt.Sprintf(format, a...)))
	}
	yamlStrayBreaks(string(data), refuse)
	if err := yaml.Unmarshal(data, &g.cfg); err != nil {
		return goreleaserFile{}, fmt.Errorf("%s is not valid YAML: %w", releaseGoreleaser, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return goreleaserFile{}, fmt.Errorf("%s is not valid YAML: %w", releaseGoreleaser, err)
	}
	goreleaserMergeKeys(&doc, refuse)
	for _, path := range goreleaserLines {
		goreleaserAt(&doc, path, func(n *yaml.Node) { g.hook(n, refuse) })
	}
	for _, block := range goreleaserWords {
		goreleaserAt(&doc, []string{block}, func(n *yaml.Node) { g.command(n, refuse) })
	}
	return g, nil
}

// hook reads a hook's value: a string, a {cmd: …}, or a list of either.
func (g *goreleaserFile) hook(n *yaml.Node, refuse func(int, string, ...any)) {
	n = workflowResolve(n)
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			g.hook(item, refuse)
		}
	case yaml.MappingNode:
		if cmd := workflowResolve(workflowMappingValue(n, "cmd")); cmd != nil {
			g.hook(cmd, refuse)
		}
	case yaml.ScalarNode:
		g.line(n.Line, n.Value, refuse)
	}
}

// command reads a sign's, an SBOM's or a publisher's cmd: and args: as
// one line.
func (g *goreleaserFile) command(n *yaml.Node, refuse func(int, string, ...any)) {
	if n = workflowResolve(n); n.Kind != yaml.MappingNode {
		return
	}
	cmd := workflowResolve(workflowMappingValue(n, "cmd"))
	if cmd == nil || cmd.Kind != yaml.ScalarNode {
		return
	}
	words := []string{cmd.Value}
	if args := workflowResolve(workflowMappingValue(n, "args")); args != nil && args.Kind == yaml.SequenceNode {
		for _, a := range args.Content {
			words = append(words, workflowResolve(a).Value)
		}
	}
	g.line(cmd.Line, strings.Join(words, " "), refuse)
}

// line records one command line, rendered as goreleaserCommand reads it,
// or refuses one a control action splits.
func (g *goreleaserFile) line(n int, text string, refuse func(int, string, ...any)) {
	text = strings.ReplaceAll(text, "\n", " ")
	if !goreleaserWrapped(text) {
		refuse(n, "a template control action; goreleaser runs one branch, and the gates read only "+
			"a command wrapped whole in one {{ if }}...{{ end }}")
		return
	}
	if rendered := goreleaserCommand(text); rendered != "" {
		g.runs = append(g.runs, goreleaserRun{line: n, text: rendered})
	}
}

// goreleaserAt calls fn on each node at path under the document root,
// through every sequence on the way.
func goreleaserAt(n *yaml.Node, path []string, fn func(*yaml.Node)) {
	n = workflowResolve(n)
	switch {
	case n == nil:
	case n.Kind == yaml.DocumentNode || n.Kind == yaml.SequenceNode:
		for _, item := range n.Content {
			goreleaserAt(item, path, fn)
		}
	case len(path) == 0:
		fn(n)
	case n.Kind == yaml.MappingNode:
		if v := workflowMappingValue(n, path[0]); v != nil {
			goreleaserAt(v, path[1:], fn)
		}
	}
}

// goreleaserMergeKeys refuses a merge key anywhere in the config: what it
// merges sits under another key than the one the release reads it at.
func goreleaserMergeKeys(n *yaml.Node, refuse func(int, string, ...any)) {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "<<" {
				refuse(n.Content[i].Line, "a merge key; write the keys out where the release reads them")
			}
		}
	}
	for _, c := range n.Content {
		goreleaserMergeKeys(c, refuse)
	}
}

// goreleaserTemplate is one template action.
var goreleaserTemplate = regexp.MustCompile(`\{\{.*?\}\}`)

// goreleaserControl is an action that renders no text of its own: a
// wrapper's if, else, range, with or end, a break, a continue or a
// comment (https://pkg.go.dev/text/template#hdr-Actions).
var goreleaserControl = regexp.MustCompile(`^\{\{-?\s*(?:(if|else|end|range|with|break|continue)\b|/\*)`)

// goreleaserWrapped reports whether a command line's control actions are
// none, or one if or with that opens the line and the end that closes it.
// goreleaserCommand reads every branch as one line, and goreleaser runs
// only the branch it renders, so any other control action hides what runs.
func goreleaserWrapped(line string) bool {
	line = strings.TrimSpace(line)
	opened := false
	for _, at := range goreleaserTemplate.FindAllStringIndex(line, -1) {
		m := goreleaserControl.FindStringSubmatch(line[at[0]:at[1]])
		switch {
		case m == nil:
		case !opened && at[0] == 0 && (m[1] == "if" || m[1] == "with"):
			opened = true
		case opened && at[1] == len(line) && m[1] == "end":
			return true
		default:
			return false
		}
	}
	return !opened
}

// goreleaserPlaceholder is the word an action that is a word of its own
// reads as.
const goreleaserPlaceholder = "{{}}"

// goreleaserCommand is a command line as goreleaser runs it, for the
// gates: goreleaser renders the templates and then splits the line into
// words (https://github.com/goreleaser/goreleaser/blob/v2.18.2/internal/pipe/universalbinary/universalbinary.go#L108-L114;
// a before hook at internal/pipe/before/before.go#L28-L32, a build's at
// internal/pipe/build/build.go#L198-L204 and a publisher's at
// internal/exec/exec.go#L217-L225 there). A control action renders no
// text, and a run of actions that stands as a word of its own reads as
// one placeholder word; inside a word they read as nothing, so the word
// stays whole.
func goreleaserCommand(text string) string {
	text = goreleaserTemplate.ReplaceAllStringFunc(text, func(action string) string {
		if goreleaserControl.MatchString(action) {
			return ""
		}
		return action
	})
	var b strings.Builder
	last := 0
	actions := goreleaserTemplate.FindAllStringIndex(text, -1)
	for i := 0; i < len(actions); i++ {
		start, end := actions[i][0], actions[i][1]
		for i+1 < len(actions) && actions[i+1][0] == end {
			i++
			end = actions[i][1]
		}
		b.WriteString(text[last:start])
		if blankAt(text, start-1) && blankAt(text, end) {
			b.WriteString(goreleaserPlaceholder)
		}
		last = end
	}
	b.WriteString(text[last:])
	return strings.TrimSpace(b.String())
}

// blankAt reports whether t holds a blank at i, or i is past either end.
func blankAt(t string, i int) bool {
	return i < 0 || i >= len(t) || t[i] == ' ' || t[i] == '\t'
}
