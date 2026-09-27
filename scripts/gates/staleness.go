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
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// staleness holds the documents to what the code defines. Each rule
// derives its expected set from the code, the registry or a record file,
// and asserts a floor on how much it read.
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
	docs     map[string]string // repository path → text
	exists   func(rel string) bool
	topLevel map[string]bool // top-level names git would commit
	// packages are the module's Go package directories, slash-separated
	// and relative to the root; hasGo says which directories hold Go.
	packages []string
	hasGo    func(rel string) bool
	// codeEnv are the GMAIL_ variables the code reads.
	codeEnv []string
	// newestTag is the newest semver tag, "" for none.
	newestTag string
	// built is whether any non-test Go file exists under internal/.
	built bool
	// tools are the dumped tool names; nil when the binary was not
	// available, which is itself a problem.
	tools    []string
	toolsErr error
	// modes renders the scope block; nil when internal/scopes is not
	// wired.
	modes []stalenessMode
	// verdicts is testdata/api-coverage.tsv's last column; surface is
	// the method count of testdata/api-surface.json, -1 when absent.
	verdicts []string
	surface  int
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
	stalenessScopeModes func() []stalenessMode
	stalenessConfigEnv  func() []string
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
	arch := in.docs["docs/architecture.md"]

	n, p := stalenessPackageMap(in.docs["CLAUDE.md"], in.packages, in.exists, in.hasGo)
	r.add(fmt.Sprintf("package map: %d listed paths, %d packages", n, len(in.packages)), p)

	n, p = stalenessPaths(in.docs, in.exists, in.topLevel)
	r.add(fmt.Sprintf("paths: %d repository paths named", n), p)

	n, p = stalenessEnv(in.docs["docs/configuration.md"], in.codeEnv)
	r.add(fmt.Sprintf("settings: %d variables", n), p)

	n, p = stalenessGates(in.docs["docs/development.md"], in.commands)
	r.add(fmt.Sprintf("gates: %d commands", n), p)

	r.add("status line", stalenessStatus(arch, in.docs["CHANGELOG.md#versions"], in.newestTag, in.built))

	n, p = stalenessNoVersionInProse(in.docs)
	r.add(fmt.Sprintf("version in prose: %d documents", n), p)

	n, p = stalenessToolCounts(arch, in.tools, in.toolsErr)
	r.add(fmt.Sprintf("tool counts: %d table rows", n), p)

	n, p = stalenessReadmeTools(in.docs["README.md"], in.tools)
	r.add(fmt.Sprintf("README tool table: %d rows", n), p)

	n, p = stalenessVerdictCounts(in.docs, in.verdicts, in.surface)
	r.add(fmt.Sprintf("verdict counts: %d verdicts", n), p)

	n, p = stalenessScopeBlock(in.docs["docs/gcp-setup.md"], in.modes)
	r.add(fmt.Sprintf("scope block: %d modes", n), p)
	return r
}

// stalenessDocs are the documents read. CHANGELOG.md is reduced to its
// [Unreleased] section: older entries are history.
var stalenessDocs = []string{"README.md", "CLAUDE.md", "CONTRIBUTING.md", "SECURITY.md", "CHANGELOG.md"}

func stalenessGather(root string, args []string) (stalenessInputs, error) {
	in := stalenessInputs{surface: -1}
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
	if c, ok := in.docs["CHANGELOG.md"]; ok {
		in.docs["CHANGELOG.md"] = stalenessUnreleased(c)
		in.docs["CHANGELOG.md#versions"] = c
	}
	in.exists = func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	in.hasGo = func(rel string) bool {
		m, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(rel), "*.go"))
		return len(m) > 0
	}
	files, err := gitFiles(root)
	if err != nil {
		return in, err
	}
	in.topLevel = map[string]bool{}
	for _, f := range files {
		first, _, _ := strings.Cut(f, "/")
		in.topLevel[first] = true
	}
	in.packages, err = stalenessGoList(root)
	if err != nil {
		return in, err
	}
	in.codeEnv, in.built, err = stalenessCodeEnv(root)
	if err != nil {
		return in, err
	}
	if stalenessConfigEnv != nil {
		in.codeEnv = append(in.codeEnv, stalenessConfigEnv()...)
		slices.Sort(in.codeEnv)
		in.codeEnv = slices.Compact(in.codeEnv)
	}
	in.newestTag = stalenessNewestTag(root)
	if stalenessScopeModes != nil {
		in.modes = stalenessScopeModes()
	}
	rows, _ := readTSV(filepath.Join(root, "testdata", "api-coverage.tsv"), 4)
	for _, row := range rows {
		in.verdicts = append(in.verdicts, row.fields[3])
	}
	if b, err := os.ReadFile(filepath.Join(root, "testdata", "api-surface.json")); err == nil {
		var s struct {
			Methods []json.RawMessage `json:"methods"`
		}
		if json.Unmarshal(b, &s) == nil {
			in.surface = len(s.Methods)
		}
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
		}
	}
	return in, nil
}

// stalenessGoList is every package in the module, tagged ones included,
// as slash-separated directories relative to root.
func stalenessGoList(root string) ([]string, error) {
	cmd := exec.Command("go", "list", "-tags", "live,evals", "-e", "-f", "{{.Dir}}", "./...")
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.Lines(string(b)) {
		dir := strings.TrimSpace(line)
		if dir == "" {
			continue
		}
		rel, err := filepath.Rel(abs, dir)
		if err != nil {
			return nil, err
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}

// stalenessCodeEnv reads every GMAIL_ string literal in the server's
// non-test Go, and whether any server code exists at all.
func stalenessCodeEnv(root string) ([]string, bool, error) {
	seen := map[string]bool{}
	built := false
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
			if top == "internal" {
				built = true
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
			return nil, false, err
		}
	}
	return slices.Sorted(maps.Keys(seen)), built, nil
}

var stalenessSemver = regexp.MustCompile(`^v(\d+\.\d+\.\d+)$`)

func stalenessNewestTag(root string) string {
	tags, err := gitOutput(root, "tag", "--sort=-v:refname")
	if err != nil {
		return ""
	}
	for line := range strings.Lines(tags) {
		if m := stalenessSemver.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return m[1]
		}
	}
	return ""
}

func stalenessUnreleased(changelog string) string {
	s := markdownSection(changelog, "## [Unreleased]")
	// The link references sit under no heading.
	if i := strings.Index(s, "\n["); i >= 0 {
		s = s[:i]
	}
	return s
}

var stalenessBacktickDir = regexp.MustCompile("`([A-Za-z0-9_./-]+/)`")

// stalenessPackageMap holds CLAUDE.md "Where things go" against the
// module's packages, both ways. A bare `name/` in a bullet is relative
// to the path before it in the same bullet. A listed directory holding
// no Go of its own covers the packages beneath it.
func stalenessPackageMap(claude string, packages []string, exists, hasGo func(string) bool) (int, []string) {
	section := markdownSection(claude, "## Where things go")
	if section == "" {
		return 0, []string{`CLAUDE.md has no "## Where things go" section`}
	}
	var listed []string
	for bullet := range strings.SplitSeq(section, "\n- ") {
		prev := ""
		for _, m := range stalenessBacktickDir.FindAllStringSubmatch(bullet, -1) {
			p := m[1]
			if !strings.Contains(strings.TrimSuffix(p, "/"), "/") && prev != "" && !exists(strings.TrimSuffix(p, "/")) {
				p = prev + p
			}
			listed = append(listed, strings.TrimSuffix(p, "/"))
			prev = p
		}
	}
	var problems []string
	if len(listed) < 15 {
		problems = append(problems, fmt.Sprintf("CLAUDE.md lists %d paths; the extractor is not reading it", len(listed)))
	}
	if len(packages) < 5 {
		problems = append(problems, fmt.Sprintf("go list found %d packages; the check is not reading the module", len(packages)))
	}
	for _, l := range listed {
		if !exists(l) {
			problems = append(problems, "CLAUDE.md \"Where things go\" lists "+l+"/, which does not exist")
		}
	}
	for _, pkg := range packages {
		covered := slices.Contains(listed, pkg)
		for _, l := range listed {
			if !covered && strings.HasPrefix(pkg, l+"/") && !hasGo(l) {
				covered = true
			}
		}
		if !covered {
			problems = append(problems, "CLAUDE.md \"Where things go\" does not name the package "+pkg+"/")
		}
	}
	return len(listed), problems
}

var (
	stalenessCodeSpan = regexp.MustCompile("`([^`\n]+)`")
	stalenessMDLink   = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	stalenessFence    = regexp.MustCompile("(?ms)^```.*?^```")
)

// stalenessPaths stats every repository path the documents name, in a
// code span or a relative link. A span's first segment must be a name
// git would commit, which is what keeps `users/me` and `mail.google.com/`
// from reading as paths. Bare file names are checked for .md and
// workflow names only.
func stalenessPaths(docs map[string]string, exists func(string) bool, topLevel map[string]bool) (int, []string) {
	var problems []string
	examined := 0
	for _, doc := range slices.Sorted(maps.Keys(docs)) {
		if strings.Contains(doc, "#") {
			continue
		}
		text := stalenessFence.ReplaceAllString(docs[doc], "")
		dir := path.Dir(doc)
		for _, m := range stalenessMDLink.FindAllStringSubmatch(text, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			examined++
			if !exists(path.Clean(path.Join(dir, target))) {
				problems = append(problems, fmt.Sprintf("%s links to %s, which does not exist", doc, target))
			}
		}
		for _, m := range stalenessCodeSpan.FindAllStringSubmatch(text, -1) {
			p, ok := stalenessPathLike(m[1])
			if !ok {
				continue
			}
			var candidates []string
			if strings.Contains(p, "/") {
				first, _, _ := strings.Cut(p, "/")
				if !topLevel[first] {
					continue
				}
				candidates = []string{p, path.Join(dir, p)}
			} else {
				ext := path.Ext(p)
				if ext != ".md" && ext != ".yml" && ext != ".yaml" {
					continue
				}
				candidates = []string{p, path.Join(dir, p), ".github/workflows/" + p, ".github/" + p}
			}
			examined++
			if !slices.ContainsFunc(candidates, exists) {
				problems = append(problems, fmt.Sprintf("%s names %s, which does not exist", doc, p))
			}
		}
	}
	if examined < 20 {
		problems = append(problems, fmt.Sprintf("only %d repository paths found across %d documents; the extractor has stopped reading them",
			examined, len(docs)))
	}
	return examined, problems
}

// stalenessPathLike cleans a code span into a candidate path.
func stalenessPathLike(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " ~$*{}<>@=:…,()[]'\"") || strings.Contains(s, "...") ||
		strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") && !strings.HasPrefix(s, ".g") {
		return "", false
	}
	s, _, _ = strings.Cut(s, "#")
	s = strings.TrimSuffix(s, "/")
	return s, s != ""
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

// stalenessGates requires docs/development.md to name every command the
// registry holds, in a code span.
func stalenessGates(development string, names []string) (int, []string) {
	if development == "" {
		return len(names), []string{"docs/development.md is missing, and it is where each gate is documented"}
	}
	var problems []string
	for _, name := range names {
		if !strings.Contains(development, "`"+name+"`") && !strings.Contains(development, "gates "+name) {
			problems = append(problems, "docs/development.md does not name the "+name+" gate")
		}
	}
	return len(names), problems
}

var (
	stalenessStatusLine   = regexp.MustCompile(`(?m)^\*\*Status[^*]*\*\*`)
	stalenessVersion      = regexp.MustCompile(`\bv?(\d+\.\d+\.\d+)\b`)
	stalenessHeading      = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`)
	stalenessNothingTag   = regexp.MustCompile(`(?i)nothing\b[^.]*\btagged|not (?:yet )?tagged|untagged`)
	stalenessNothingBuilt = regexp.MustCompile(`(?i)design only|nothing\b[^.]*\bbuilt`)
)

// stalenessStatus holds the design document's status line against the
// newest tag or CHANGELOG heading, and against whether code exists.
func stalenessStatus(arch, changelog, tag string, built bool) []string {
	line := stalenessStatusLine.FindString(arch)
	if line == "" {
		return []string{"docs/architecture.md has no **Status: ...** line; the check is reading the wrong file"}
	}
	var known []string
	if tag != "" {
		known = append(known, tag)
	}
	if m := stalenessHeading.FindStringSubmatch(changelog); m != nil {
		known = append(known, m[1])
	}
	var claims []string
	for _, m := range stalenessVersion.FindAllStringSubmatch(line, -1) {
		claims = append(claims, m[1])
	}
	var problems []string
	switch {
	case len(known) == 0:
		if len(claims) > 0 {
			problems = append(problems, fmt.Sprintf("the status line claims %v and nothing is tagged or released", claims))
		}
		if !stalenessNothingTag.MatchString(line) {
			problems = append(problems, "nothing is tagged, and the status line does not say so")
		}
	default:
		if stalenessNothingTag.MatchString(line) {
			problems = append(problems, fmt.Sprintf("the status line says nothing is tagged; %v is", known))
		}
		ok := false
		for _, c := range claims {
			ok = ok || slices.Contains(known, c)
		}
		if !ok {
			problems = append(problems, fmt.Sprintf("the status line names %v; the newest release is %s",
				claims, strings.Join(known, " or ")))
		}
	}
	if built && stalenessNothingBuilt.MatchString(line) {
		problems = append(problems, "the status line says nothing is built, and internal/ holds code")
	}
	return problems
}

// stalenessProseDocs are held to carrying no version: a badge shows it.
// architecture.md is excluded because its plan names the versions phases
// will become.
func stalenessProseDocs(docs map[string]string) []string {
	var out []string
	for name := range docs {
		if name == "docs/architecture.md" || strings.HasPrefix(name, "CHANGELOG.md") || name == "CLAUDE.md" {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

var stalenessProseVersion = regexp.MustCompile(`\bv\d+\.\d+\.\d+\b`)

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
			t = stalenessCodeSpan.ReplaceAllString(t, "")
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

var (
	stalenessToolSentence = regexp.MustCompile(`(?i)\b([a-z0-9-]+)\s+tools:\s+([a-z0-9-]+)\s+by\s+default,\s+([a-z0-9-]+)\s+in\s+read-only\s+mode,\s+([a-z0-9-]+)\s+fewer\s+in\s+each\s+when`)
	stalenessToolRow      = regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\| ([^|]+) \\| ([^|]+) \\|")
)

// stalenessToolCounts holds §8's sentence against §8's table, and the
// built binary's tools against the table's names.
// stalenessReadmeTool is a row of the README's tool table: the tool name
// in backticks in the first column.
var stalenessReadmeTool = regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")

// stalenessReadmeTools holds the README's tool table to the tools the
// built binary registers, both ways: the README is where a person learns
// what the server does, so a tool it omits does not exist for them, and
// one it lists that the binary lacks is a promise nothing keeps.
func stalenessReadmeTools(readme string, tools []string) (int, []string) {
	rows := stalenessReadmeTool.FindAllStringSubmatch(markdownSection(readme, "## Tools"), -1)
	listed := make([]string, 0, len(rows))
	for _, r := range rows {
		listed = append(listed, r[1])
	}
	if len(listed) == 0 {
		return 0, []string{"README.md has no tool table under \"## Tools\"; this rule holds nothing"}
	}
	if tools == nil {
		return len(listed), nil // the binary's absence is reported by the tool-count rule
	}
	var problems []string
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
	return len(listed), problems
}

func stalenessToolCounts(arch string, tools []string, toolsErr error) (int, []string) {
	section := markdownSection(arch, "## 8. Tool surface")
	if i := strings.Index(section, "\n### 8a"); i >= 0 {
		section = section[:i]
	}
	rows := stalenessToolRow.FindAllStringSubmatch(section, -1)
	if len(rows) < 8 {
		return len(rows), []string{fmt.Sprintf("§8's tool table has %d rows; the check is reading the wrong section", len(rows))}
	}
	var problems []string
	total, readOnly, def, local := len(rows), 0, 0, 0
	var names []string
	for _, r := range rows {
		names = append(names, r[1])
		reg := strings.TrimSpace(r[3])
		switch {
		case reg == "always":
			readOnly++
			def++
		case strings.Contains(reg, "GMAIL_LOCAL_DIR"):
			readOnly++
			def++
			local++
		case reg == "not read-only":
			def++
		}
	}
	m := stalenessToolSentence.FindStringSubmatch(section)
	if m == nil {
		problems = append(problems, "§8 states no tool counts in the form \"N tools: N by default, N in read-only mode, "+
			"N fewer in each when\"; this rule holds nothing")
	} else {
		for i, want := range []int{total, def, readOnly, local} {
			got, ok := stalenessNumber(m[i+1])
			label := []string{"tools", "by default", "in read-only mode", "fewer when GMAIL_LOCAL_DIR is unset"}[i]
			if !ok || got != want {
				problems = append(problems, fmt.Sprintf("§8 says %q %s; the table says %d", m[i+1], label, want))
			}
		}
	}
	if toolsErr != nil {
		problems = append(problems, "the built binary's tools could not be read: "+toolsErr.Error())
	}
	for _, t := range tools {
		if !slices.Contains(names, t) {
			problems = append(problems, "the binary registers "+t+", which §8's table does not list")
		}
	}
	return len(rows), problems
}

var (
	stalenessVerdictSentence = regexp.MustCompile(`(?i)\b([a-z0-9-]+)\s+used,\s+([a-z0-9-]+)\s+gated,\s+([a-z0-9-]+)\s+deferred[^,]*,\s+([a-z0-9-]+)\s+written\s+off`)
	stalenessAllMethods      = regexp.MustCompile(`(?i)\ball\s+([a-z0-9-]+)\s+(?:published\s+API\s+methods|methods\s+of\s+the\s+discovery\s+document|are\s+in\s+§8a)`)
	stalenessVerdictRow      = regexp.MustCompile("(?m)^\\| `[A-Za-z.]+` \\| [A-Z]+ \\| (.+) \\|$")
)

// stalenessVerdictCounts holds §8a's counts against its table, or the
// record it moved to, and every "all N methods" against the record and
// the fetched surface.
func stalenessVerdictCounts(docs map[string]string, verdicts []string, surface int) (int, []string) {
	arch := docs["docs/architecture.md"]
	section := markdownSection(arch, "### 8a. Every published method, with a verdict")
	source := "testdata/api-coverage.tsv"
	if rows := stalenessVerdictRow.FindAllStringSubmatch(section, -1); len(rows) > 0 {
		verdicts = nil
		for _, r := range rows {
			verdicts = append(verdicts, r[1])
		}
		source = "§8a's table"
	}
	if len(verdicts) < 50 {
		return len(verdicts), []string{fmt.Sprintf("%s holds %d verdicts; the check is not reading it", source, len(verdicts))}
	}
	counts := map[string]int{}
	var problems []string
	for _, v := range verdicts {
		kind := ""
		for _, k := range []string{"Used", "Gated", "Deferred", "Written off"} {
			if strings.HasPrefix(v, k) {
				kind = k
			}
		}
		if kind == "" {
			problems = append(problems, fmt.Sprintf("%s has a verdict starting with none of Used, Gated, Deferred, Written off: %q",
				source, stalenessClipWords(v)))
		}
		counts[kind]++
	}
	if m := stalenessVerdictSentence.FindStringSubmatch(section); m == nil {
		problems = append(problems, "§8a states no verdict counts; this rule holds nothing")
	} else {
		for i, k := range []string{"Used", "Gated", "Deferred", "Written off"} {
			if got, ok := stalenessNumber(m[i+1]); !ok || got != counts[k] {
				problems = append(problems, fmt.Sprintf("§8a says %q %s; %s has %d", m[i+1], strings.ToLower(k), source, counts[k]))
			}
		}
	}
	claims := 0
	for _, name := range []string{"CLAUDE.md", "docs/architecture.md"} {
		for _, m := range stalenessAllMethods.FindAllStringSubmatch(docs[name], -1) {
			claims++
			got, ok := stalenessNumber(m[1])
			if !ok || got != len(verdicts) {
				problems = append(problems, fmt.Sprintf("%s says %q; %s has %d verdicts", name, m[0], source, len(verdicts)))
			}
			if surface >= 0 && got != surface {
				problems = append(problems, fmt.Sprintf("%s says %q; testdata/api-surface.json has %d methods", name, m[0], surface))
			}
		}
	}
	if claims == 0 {
		problems = append(problems, "no document states how many methods were judged; the method-count rule holds nothing")
	}
	return len(verdicts), problems
}

func stalenessClipWords(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// stalenessNumber reads digits or an English number up to ninety-nine.
func stalenessNumber(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	units := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
		"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}
	tens := []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
	s = strings.ToLower(s)
	if i := slices.Index(units, s); i >= 0 {
		return i, true
	}
	t, u, _ := strings.Cut(s, "-")
	ti := slices.Index(tens, t)
	if ti < 2 {
		return 0, false
	}
	if u == "" {
		return ti * 10, true
	}
	ui := slices.Index(units, u)
	if ui < 1 || ui > 9 {
		return 0, false
	}
	return ti*10 + ui, true
}

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
