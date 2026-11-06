package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

// A reader for the parts of the Makefile the gates ask about: variables,
// targets, their prerequisites and their recipes. It is not make. It
// knows `NAME = v` with `?=`, `:=` or `+=`, after `export` or `override`,
// `target: prereqs`, and recipe lines starting with a TAB, read as make
// reads them.

// makefile is a parsed Makefile.
type makefile struct {
	vars    map[string]string
	targets map[string]*makefileTarget
}

// makefileTarget is one rule.
type makefileTarget struct {
	name    string
	prereqs []string
	recipe  []string
}

var (
	makefileVarLine    = regexp.MustCompile(`^(?:(?:export|override)[ \t]+)*([A-Za-z_][A-Za-z0-9_]*)[ \t]*([:?+]?=)[ \t]*(.*)$`)
	makefileTargetLine = regexp.MustCompile(`^([A-Za-z0-9_.\-]+)\s*:([^=].*)?$`)
	makefileVarRef     = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_]*)\)`)
)

// readMakefile parses a Makefile.
func readMakefile(path string) (*makefile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a repository path
	if err != nil {
		return nil, err
	}
	return parseMakefile(string(data)), nil
}

// parseMakefile reads a Makefile as make does: a # starts a comment in an
// assignment, a later `=` or `:=` replaces a value, `?=` sets only a
// variable not yet set, and `+=` appends after a space
// (https://www.gnu.org/software/make/manual/html_node/Setting.html,
// https://www.gnu.org/software/make/manual/html_node/Appending.html). A
// target's rules merge their prerequisites, and the last recipe is the
// one that runs
// (https://www.gnu.org/software/make/manual/html_node/Multiple-Rules.html).
func parseMakefile(text string) *makefile {
	m := &makefile{vars: map[string]string{}, targets: map[string]*makefileTarget{}}
	var current *makefileTarget
	// fresh is whether the current rule has yet to give its first recipe
	// line, which replaces any recipe an earlier rule gave.
	fresh := false
	var pending string
	for raw := range strings.SplitSeq(text, "\n") {
		raw = strings.TrimRight(raw, "\r")
		if pending != "" {
			raw = pending + strings.TrimLeft(raw, " \t")
			pending = ""
		}
		if cont, ok := strings.CutSuffix(raw, "\\"); ok {
			pending = strings.TrimRight(cont, " \t") + " "
			continue
		}
		if strings.HasPrefix(raw, "\t") {
			if current != nil {
				// @, - and + tell make how to run a command and are not
				// part of it (https://www.gnu.org/software/make/manual/html_node/Recipe-Syntax.html).
				line := strings.TrimLeft(strings.TrimSpace(raw), "@-+ \t")
				if line != "" && !strings.HasPrefix(line, "#") {
					if fresh {
						current.recipe, fresh = nil, false
					}
					current.recipe = append(current.recipe, line)
				}
			}
			continue
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if v := makefileVarLine.FindStringSubmatch(line); v != nil {
			current = nil
			value, _, _ := strings.Cut(v[3], "#")
			value = strings.TrimSpace(value)
			old, set := m.vars[v[1]]
			switch {
			case v[2] == "?=" && set:
				continue
			case v[2] == "+=" && old != "":
				value = old + " " + value
			}
			m.vars[v[1]] = value
			continue
		}
		if t := makefileTargetLine.FindStringSubmatch(line); t != nil {
			name := t[1]
			if name == ".PHONY" {
				current = nil
				continue
			}
			prereqs, _, _ := strings.Cut(t[2], "##")
			prereqs, _, _ = strings.Cut(prereqs, "#")
			if m.targets[name] == nil {
				m.targets[name] = &makefileTarget{name: name}
			}
			current, fresh = m.targets[name], true
			for _, p := range strings.Fields(prereqs) {
				if !slices.Contains(current.prereqs, p) {
					current.prereqs = append(current.prereqs, p)
				}
			}
			continue
		}
		current = nil
	}
	return m
}

// expand replaces $(NAME) with the variable's value, recursively, and
// leaves a name it does not know in place.
func (m *makefile) expand(s string) string {
	for range 10 {
		next := makefileVarRef.ReplaceAllStringFunc(s, func(ref string) string {
			name := makefileVarRef.FindStringSubmatch(ref)[1]
			if v, ok := m.vars[name]; ok {
				return v
			}
			return ref
		})
		if next == s {
			break
		}
		s = next
	}
	return s
}

// closure is target and everything it depends on, transitively, sorted.
func (m *makefile) closure(names ...string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		if t, ok := m.targets[n]; ok {
			for _, p := range t.prereqs {
				walk(p)
			}
		}
	}
	for _, n := range names {
		walk(n)
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// recipe is a target's own recipe lines, expanded.
func (m *makefile) recipe(name string) []string {
	t, ok := m.targets[name]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(t.recipe))
	for _, line := range t.recipe {
		out = append(out, m.expand(line))
	}
	return out
}

// variable is one variable's raw value, or an error naming it.
func (m *makefile) variable(name string) (string, error) {
	v, ok := m.vars[name]
	if !ok || v == "" {
		return "", fmt.Errorf("the Makefile defines no %s", name)
	}
	return v, nil
}
