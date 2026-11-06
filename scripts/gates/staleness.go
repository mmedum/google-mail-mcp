package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// staleness holds the documents to what the code, the built binary and
// the gate registry define, and keeps version numbers out of prose. The
// links and paths the documents name are the docs gate's to check.
func staleness(out io.Writer, args []string) error {
	in, err := stalenessGather(".", args)
	if err != nil {
		return err
	}
	r := stalenessCheck(in)
	for _, line := range r.read {
		_, _ = fmt.Fprintln(out, "  read: "+line)
	}
	if err := problemsError(out, "the documents", r.problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "staleness ok: %d checks\n", len(r.read))
	return nil
}

// stalenessInputs is everything the rules read, gathered once so the
// rules are pure functions a test can feed.
type stalenessInputs struct {
	docs map[string]string // repository path → text
	// codeEnv are the GMAIL_ variables the code reads.
	codeEnv []string
	// profileRule is config.ProfileRule, "" when internal/config is not
	// wired.
	profileRule string
	// tools are the dumped tool names; nil when the binary was not
	// available, which toolsErr says. mustAsk are the ones the README's
	// permissions.ask example must name (stalenessMustAsk).
	tools, mustAsk []string
	toolsErr       error
	// modes renders the scope block; nil when internal/scopes is not
	// wired.
	modes    []stalenessMode
	commands []string
}

// stalenessMode is one row of the generated scope block.
type stalenessMode struct {
	Name   string
	Flags  []string
	Scopes []string
}

// Set by staleness_code.go, which is the one file importing the server's
// packages.
var (
	stalenessScopeModes  func() []stalenessMode
	stalenessConfigEnv   func() []string
	stalenessProfileRule string
)

type stalenessReport struct {
	read     []string
	problems []string
}

func (r *stalenessReport) add(read string, problems []string) {
	r.read = append(r.read, read)
	r.problems = append(r.problems, problems...)
}

func stalenessCheck(in stalenessInputs) stalenessReport {
	var r stalenessReport
	n, p := stalenessEnv(in.docs["docs/configuration.md"], in.codeEnv)
	r.add(fmt.Sprintf("settings: %d variables", n), p)
	r.add("profile rule", stalenessProfile(in.docs["docs/configuration.md"], in.profileRule))

	n, p = stalenessGates(in.docs[stalenessGatesDoc], in.commands)
	r.add(fmt.Sprintf("gates: %d commands", n), p)

	n, p = stalenessNoVersionInProse(in.docs)
	r.add(fmt.Sprintf("version in prose: %d documents", n), p)

	n, p = stalenessReadmeTools(in.docs["README.md"], in.tools, in.mustAsk, in.toolsErr)
	r.add(fmt.Sprintf("README tool table: %d rows", n), p)

	n, p = stalenessScopeBlock(in.docs["docs/gcp-setup.md"], in.modes)
	r.add(fmt.Sprintf("scope block: %d modes", n), p)
	return r
}

// stalenessDocs are the documents read. CHANGELOG.md is reduced to its
// [Unreleased] section: older entries are history.
var stalenessDocs = []string{"README.md", checklistDoc, stalenessGatesDoc, "SECURITY.md", changelogFile}

func stalenessGather(root string, args []string) (stalenessInputs, error) {
	var in stalenessInputs
	in.docs = map[string]string{}
	names := slices.Clone(stalenessDocs)
	mds, _ := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	for _, m := range mds {
		rel, _ := filepath.Rel(root, m)
		names = append(names, filepath.ToSlash(rel))
	}
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			continue // a missing document fails the rule that needs it
		}
		in.docs[name] = string(b)
	}
	if c, ok := in.docs[changelogFile]; ok {
		in.docs[changelogFile] = stalenessUnreleased(c)
	}
	var err error
	in.codeEnv, err = stalenessCodeEnv(root)
	if err != nil {
		return in, err
	}
	if stalenessConfigEnv != nil {
		in.codeEnv = append(in.codeEnv, stalenessConfigEnv()...)
		slices.Sort(in.codeEnv)
		in.codeEnv = slices.Compact(in.codeEnv)
	}
	in.profileRule = stalenessProfileRule
	if stalenessScopeModes != nil {
		in.modes = stalenessScopeModes()
	}
	in.commands = slices.Sorted(maps.Keys(commands))

	bin, cleanup, err := serverBinary(args)
	if err != nil {
		in.toolsErr = err
	} else {
		defer cleanup()
		d, _, err := dumpSchemas(bin)
		if err != nil {
			in.toolsErr = err
		} else {
			for _, t := range d.Tools {
				in.tools = append(in.tools, t.Name)
			}
			in.mustAsk = stalenessMustAsk(d)
		}
	}
	return in, nil
}

// stalenessCodeEnv reads every GMAIL_ string literal in the server's
// non-test Go.
func stalenessCodeEnv(root string) ([]string, error) {
	seen := map[string]bool{}
	envName := regexp.MustCompile(`^GMAIL_[A-Z0-9_]+$`)
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fs.SkipDir
				}
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return fs.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil && envName.MatchString(s) {
						seen[s] = true
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// stalenessUnreleased is the [Unreleased] section, the lines the next
// release note lifts.
func stalenessUnreleased(changelog string) string {
	lines, from, to := changelogSection(changelog, "Unreleased")
	return strings.Join(lines[from:to], "\n")
}

var stalenessEnvRef = regexp.MustCompile(`\bGMAIL_[A-Z0-9_]*[A-Z0-9]\b`)

// stalenessEnv holds docs/configuration.md to the variables the code
// reads, both ways.
func stalenessEnv(config string, codeEnv []string) (int, []string) {
	if config == "" {
		return 0, []string{"docs/configuration.md is missing"}
	}
	var problems []string
	if len(codeEnv) < 8 {
		problems = append(problems, fmt.Sprintf("the code reads %d GMAIL_ variables; the check is not reading it", len(codeEnv)))
	}
	documented := map[string]bool{}
	for _, v := range stalenessEnvRef.FindAllString(config, -1) {
		documented[v] = true
	}
	for _, v := range codeEnv {
		if !documented[v] {
			problems = append(problems, "docs/configuration.md does not document "+v)
		}
	}
	for _, v := range slices.Sorted(maps.Keys(documented)) {
		if !slices.Contains(codeEnv, v) {
			problems = append(problems, "docs/configuration.md documents "+v+", which the code does not read")
		}
	}
	return len(codeEnv), problems
}

// stalenessProfile requires the profile setting's row of
// docs/configuration.md to say what a profile name is in the words the
// server refuses one with. An empty rule is the wiring missing, and is
// refused rather than found in any text.
func stalenessProfile(config, rule string) []string {
	if rule == "" {
		return []string{"config.ProfileRule did not reach the staleness check"}
	}
	for line := range strings.Lines(config) {
		if strings.HasPrefix(line, "| `"+mcpbProfileVar+"` |") {
			if strings.Contains(line, rule) {
				return nil
			}
			return []string{fmt.Sprintf("docs/configuration.md's %s row does not say a profile name is %q",
				mcpbProfileVar, rule)}
		}
	}
	return []string{fmt.Sprintf("docs/configuration.md has no %s row to say a profile name is %q", mcpbProfileVar, rule)}
}

// stalenessGatesDoc is where each gate is documented.
const stalenessGatesDoc = "CONTRIBUTING.md"

// stalenessGates requires CONTRIBUTING.md to name every command the
// registry holds, in a code span.
func stalenessGates(contributing string, names []string) (int, []string) {
	if contributing == "" {
		return len(names), []string{stalenessGatesDoc + " is missing, and it is where each gate is documented"}
	}
	var problems []string
	for _, name := range names {
		if !strings.Contains(contributing, "`"+name+"`") && !strings.Contains(contributing, "gates "+name) {
			problems = append(problems, stalenessGatesDoc+" does not name the "+name+" gate")
		}
	}
	return len(names), problems
}

// stalenessProseDocs are held to carrying no version: a badge shows it.
// architecture.md and CLAUDE.md are left out, as a decision or a rule
// may name a version of the software it depends on.
func stalenessProseDocs(docs map[string]string) []string {
	var out []string
	for name := range docs {
		if name == architectureDoc || name == changelogFile || name == checklistDoc {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

var (
	stalenessProseVersion = regexp.MustCompile(`\bv\d+\.\d+\.\d+\b`)
	stalenessFence        = regexp.MustCompile("(?ms)^```.*?^```")
)

func stalenessNoVersionInProse(docs map[string]string) (int, []string) {
	names := stalenessProseDocs(docs)
	var problems []string
	for _, name := range names {
		text := stalenessFence.ReplaceAllString(docs[name], "")
		for i, line := range strings.Split(text, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "[!") || strings.HasPrefix(t, "|") || strings.Contains(t, "://") {
				continue
			}
			t = codeSpan.ReplaceAllString(t, "")
			if v := stalenessProseVersion.FindString(t); v != "" {
				problems = append(problems, fmt.Sprintf("%s:%d writes %s in prose; a badge shows the version and cannot go stale",
					name, i+1, v))
			}
		}
	}
	if len(names) < 3 {
		problems = append(problems, fmt.Sprintf("only %d documents checked for a version in prose", len(names)))
	}
	return len(names), problems
}

// stalenessReadmeTool is a row of the README's tool table: the tool name
// in backticks in the first column.
var stalenessReadmeTool = regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")

// stalenessReadmeTools holds the README's tool table to the tools the
// built binary registers, both ways: the README is where a person learns
// what the server does, so a tool it omits does not exist for them, and
// one it lists that the binary lacks is a promise nothing keeps. The
// README's permissions.ask rules name registered tools, and give a rule
// for every tool in mustAsk (stalenessMustAsk), which a new one must not
// join unasked. A binary whose tools could not be read holds the README
// to nothing, which toolsErr says.
func stalenessReadmeTools(readme string, tools, mustAsk []string, toolsErr error) (int, []string) {
	rows := stalenessReadmeTool.FindAllStringSubmatch(markdownSection(readme, "## Tools"), -1)
	listed := make([]string, 0, len(rows))
	for _, r := range rows {
		listed = append(listed, r[1])
	}
	var problems []string
	if len(listed) == 0 {
		problems = append(problems, "README.md has no tool table under \"## Tools\"; this rule holds nothing")
	}
	if toolsErr != nil {
		problems = append(problems, "the built binary's tools could not be read: "+toolsErr.Error())
	}
	if len(problems) > 0 {
		return len(listed), problems
	}
	for _, t := range tools {
		if !slices.Contains(listed, t) {
			problems = append(problems, "the binary registers "+t+", which README.md's tool table does not list")
		}
	}
	for _, t := range listed {
		if !slices.Contains(tools, t) {
			problems = append(problems, "README.md's tool table lists "+t+", which the binary does not register")
		}
	}
	// A Claude Code permission rule for a tool that does not exist matches
	// nothing, so the person it was meant to ask is never asked.
	rules := stalenessPermissionRule.FindAllStringSubmatch(readme, -1)
	if len(rules) == 0 {
		problems = append(problems, "README.md gives no Claude Code permission rule for a tool, in the form "+
			"\"mcp__google-mail__<tool>\"; the README's permissions.ask block is missing")
	}
	for _, m := range rules {
		if !slices.Contains(tools, m[1]) {
			problems = append(problems, "README.md gives a permission rule for "+m[1]+", which the binary does not register")
		}
	}
	// Only a rule inside an "ask" list asks: one under "allow" lets the
	// tool run unasked.
	asked := map[string]bool{}
	for _, block := range stalenessAskList.FindAllStringSubmatch(readme, -1) {
		for _, m := range stalenessPermissionRule.FindAllStringSubmatch(block[1], -1) {
			asked[m[1]] = true
		}
	}
	if len(mustAsk) == 0 {
		problems = append(problems, "the binary registers no tool the README must ask about; the permission rules are held to nothing")
	}
	for _, t := range mustAsk {
		if !asked[t] {
			problems = append(problems, "README.md's permissions.ask block has no rule for "+t+
				", which sends, changes a setting or deletes for good")
		}
	}
	return len(listed), problems
}

// stalenessMustAsk is every tool the README's permissions.ask example
// must name: annotated destructive and not of the Write kind, whose
// tools can be undone and run unasked under the example's allow rule. A
// tool with no kind is held to the rule.
func stalenessMustAsk(d *schemaDump) []string {
	var out []string
	for _, t := range d.Tools {
		if dumpDestructive(t) && d.Kinds[t.Name] != smokeKindWrite {
			out = append(out, t.Name)
		}
	}
	return out
}

// dumpDestructive reports whether a client reads a dumped tool as
// destructive: not read-only, with destructiveHint true or absent, the
// MCP default.
func dumpDestructive(t dumpTool) bool {
	var a struct {
		ReadOnly    *bool `json:"readOnlyHint"`
		Destructive *bool `json:"destructiveHint"`
	}
	if len(t.Annotations) > 0 && json.Unmarshal(t.Annotations, &a) != nil {
		return true // unreadable: held to a rule rather than let through
	}
	if a.ReadOnly != nil && *a.ReadOnly {
		return false
	}
	return a.Destructive == nil || *a.Destructive
}

// stalenessPermissionRule is a Claude Code permission rule for one of
// this server's tools, under the server name the README uses.
var stalenessPermissionRule = regexp.MustCompile(`"mcp__google-mail__([a-z0-9_]+)"`)

// stalenessAskList is a permissions "ask" list in a JSON block, its
// entries captured.
var stalenessAskList = regexp.MustCompile(`"ask"\s*:\s*\[([^\]]*)\]`)

const (
	stalenessScopesBegin = "<!-- scopes:begin"
	stalenessScopesEnd   = "<!-- scopes:end -->"
)

// stalenessScopeTable renders the scope block from the modes. It is the
// only rendering; docs/gcp-setup.md must carry it byte for byte.
func stalenessScopeTable(modes []stalenessMode) string {
	var b strings.Builder
	b.WriteString("| Mode | Set by | Scopes login requests |\n|---|---|---|\n")
	for _, m := range modes {
		flags := "nothing (the default)"
		if len(m.Flags) > 0 {
			flags = "`" + strings.Join(m.Flags, "`, `") + "`"
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` |\n", m.Name, flags, strings.Join(m.Scopes, "`, `"))
	}
	return b.String()
}

// stalenessScopeBlock generates the scope block from internal/scopes for
// every mode and compares it exactly with the marked block.
func stalenessScopeBlock(gcp string, modes []stalenessMode) (int, []string) {
	if modes == nil {
		return 0, []string{"scope source not wired: internal/scopes absent, so the docs/gcp-setup.md block cannot be generated"}
	}
	if len(modes) < 3 {
		return len(modes), []string{fmt.Sprintf("internal/scopes reports %d modes; the generator is not reading it", len(modes))}
	}
	want := stalenessScopeTable(modes)
	begin := strings.Index(gcp, stalenessScopesBegin)
	end := strings.Index(gcp, stalenessScopesEnd)
	if begin < 0 || end < begin {
		return len(modes), []string{"docs/gcp-setup.md has no " + stalenessScopesBegin + " ... " + stalenessScopesEnd + " block"}
	}
	nl := strings.Index(gcp[begin:], "\n")
	got := strings.Trim(gcp[begin+nl+1:end], "\n")
	if got != strings.TrimSuffix(want, "\n") {
		return len(modes), []string{"docs/gcp-setup.md's scope block differs from what internal/scopes generates; " +
			"put this between the markers:\n" + want}
	}
	return len(modes), nil
}
