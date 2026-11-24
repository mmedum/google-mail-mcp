package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestChangelogLinks(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"before the first release", "# Changelog\n\n## [Unreleased]\n\n- x\n\n" +
			"[Unreleased]: https://github.com/o/r/compare/abc...HEAD\n", ""},
		{"released and linked", "## [Unreleased]\n\n## [0.2.0] - 2026-01-02\n\n## [0.1.0] - 2026-01-01\n\n" +
			"[Unreleased]: https://h/compare/v0.2.0...HEAD\n[0.2.0]: https://h/compare/v0.1.0...v0.2.0\n" +
			"[0.1.0]: https://h/releases/tag/v0.1.0\n", ""},
		{"no Unreleased heading", "## [0.1.0]\n\n[0.1.0]: https://h\n[Unreleased]: https://h/compare/v0.1.0...HEAD\n",
			"no ## [Unreleased] heading"},
		{"no Unreleased link", "## [Unreleased]\n", "[Unreleased] has no link reference"},
		{"a version with no link", "## [Unreleased]\n## [0.1.0]\n[Unreleased]: https://h/compare/v0.1.0...HEAD\n",
			"## [0.1.0] has no [0.1.0]: link reference"},
		{"Unreleased compares from an old tag", "## [Unreleased]\n## [0.2.0]\n## [0.1.0]\n" +
			"[Unreleased]: https://h/compare/v0.1.0...HEAD\n[0.2.0]: https://h\n[0.1.0]: https://h\n",
			"it should compare from v0.2.0..."},
		{"an empty definition, which does not take the next line's URL", "## [Unreleased]\n## [0.2.0]\n## [0.1.0]\n" +
			"[Unreleased]: https://h/compare/v0.2.0...HEAD\n[0.2.0]:\n[0.1.0]: https://h\n",
			"## [0.2.0] has no [0.2.0]: link reference"},
		{"a link with no heading", "## [Unreleased]\n[Unreleased]: https://h/compare/x...HEAD\n[0.9.0]: https://h\n",
			"[0.9.0]: is a link reference with no heading"},
		{"a heading in fenced code, which is an example", "## [Unreleased]\n\n```\n## [9.9.9]\n```\n\n" +
			"[Unreleased]: https://h/compare/abc...HEAD\n", ""},
		{"a version semver does not allow", "## [Unreleased]\n## [1.2.3-rc..1]\n[Unreleased]: https://h/compare/abc...HEAD\n",
			"## [1.2.3-rc..1] is not a semver version"},
		{"a version with a leading zero", "## [Unreleased]\n## [1.02.3]\n[Unreleased]: https://h/compare/abc...HEAD\n",
			"## [1.02.3] is not a semver version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := changelogLinkProblems(tc.text)
			joined := strings.Join(problems, "\n")
			if tc.want == "" && joined != "" {
				t.Errorf("unexpected problems: %s", joined)
			}
			if tc.want != "" && !strings.Contains(joined, tc.want) {
				t.Errorf("want %q, got %q", tc.want, joined)
			}
		})
	}
}

func TestChangelogAddedUnderUnreleased(t *testing.T) {
	under := "@@ -1,5 +1,6 @@\n # Changelog\n ## [Unreleased]\n+- a new tool\n ## [0.1.0]\n - old\n"
	if n := changelogAddedUnderUnreleased(under); n != 1 {
		t.Errorf("an entry under [Unreleased] counted %d", n)
	}
	released := "@@ -1,5 +1,6 @@\n ## [Unreleased]\n ## [0.1.0]\n+- slipped in\n"
	if n := changelogAddedUnderUnreleased(released); n != 0 {
		t.Errorf("an entry under a released heading counted %d", n)
	}
	cut := "@@ -1,4 +1,6 @@\n-## [Unreleased]\n+## [Unreleased]\n+\n+## [0.2.0] - 2026-01-01\n - entry\n+- one more\n"
	if n := changelogAddedUnderUnreleased(cut); n != 1 {
		t.Errorf("a release cut counted %d", n)
	}
	// The first release: the file is new, so no [Unreleased] is removed.
	first := "@@ -0,0 +1,6 @@\n+# Changelog\n+## [Unreleased]\n+\n+## [1.0.0] - 2026-01-01\n+- a tool\n+- another\n"
	if n := changelogAddedUnderUnreleased(first); n != 2 {
		t.Errorf("the first release counted %d", n)
	}
}

// Only a bullet at the margin is an entry, and nothing in fenced code is
// a heading or an entry.
func TestChangelogCountsOnlyEntries(t *testing.T) {
	for name, tc := range map[string]struct {
		diff string
		want int
	}{
		"a subheading alone":   {"@@ -1,3 +1,4 @@\n ## [Unreleased]\n+### Fixed\n ## [0.1.0]\n", 0},
		"a line of prose":      {"@@ -1,3 +1,4 @@\n ## [Unreleased]\n+Fixed the thing.\n ## [0.1.0]\n", 0},
		"an indented bullet":   {"@@ -1,3 +1,4 @@\n ## [Unreleased]\n+  - nested\n ## [0.1.0]\n", 0},
		"a bullet with a star": {"@@ -1,3 +1,4 @@\n ## [Unreleased]\n+* a fix\n ## [0.1.0]\n", 1},
		"a bullet in a fence":  {"@@ -1,3 +1,6 @@\n ## [Unreleased]\n+```\n+- not an entry\n+```\n ## [0.1.0]\n", 0},
		"a heading in a fence": {"@@ -1,3 +1,6 @@\n ## [Unreleased]\n ## [0.1.0]\n+```\n+## [Unreleased]\n+```\n" +
			"+- slipped in\n", 0},
	} {
		if got := changelogAddedUnderUnreleased(tc.diff); got != tc.want {
			t.Errorf("%s: counted %d, want %d", name, got, tc.want)
		}
	}
}

func TestChangelogTouched(t *testing.T) {
	shipped := map[string]bool{"internal/x": true, "cmd/m": true}
	got := changelogTouched([]string{"internal/x/a.go", "internal/x/a_test.go", "internal/x/testdata/f",
		"docs/a.md", "cmd/m/main.go", "scripts/gates/x.go", "internal/x/fake/fake.go", "packaging/m.json", "go.mod"},
		shipped)
	if strings.Join(got, ",") != "internal/x/a.go,cmd/m/main.go,packaging/m.json,go.mod" {
		t.Errorf("changelogTouched = %v", got)
	}
}

// The binary links the server and not the in-memory fake.
func TestShippedPackageDirsIsTheBinarys(t *testing.T) {
	dirs, err := shippedPackageDirs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if !dirs["internal/service"] || !dirs["cmd/google-mail-mcp"] {
		t.Errorf("the binary's packages miss internal/service or cmd/google-mail-mcp: %v", dirs)
	}
	if dirs["internal/gapi/gmailtest"] || dirs["scripts/gates"] {
		t.Errorf("the binary's packages include gmailtest or the gates: %v", dirs)
	}
}

// The gate end to end, over a real repository with three commits.
func TestChangelogCheckOverGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := writeTree(t, map[string]string{
		"CHANGELOG.md":    "# Changelog\n\n## [Unreleased]\n\n[Unreleased]: https://h\n",
		"internal/a/a.go": "package a\n",
	})
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-qm", "one")

	write("internal/a/a.go", "package a\n\nvar X = 1\n")
	gitIn(t, root, "commit", "-qam", "two")
	var out sink
	if err := changelogCheck(&out, root, "HEAD~1", "HEAD", map[string]bool{"internal/a": true}); err == nil || !strings.Contains(err.Error(), "did not") {
		t.Errorf("a source change with no entry passed: %v", err)
	}

	write("CHANGELOG.md", "# Changelog\n\n## [Unreleased]\n\n- X exists\n\n[Unreleased]: https://h\n")
	gitIn(t, root, "commit", "-qam", "three")
	if err := changelogCheck(&out, root, "HEAD~2", "HEAD", map[string]bool{"internal/a": true}); err != nil {
		t.Errorf("an entry under [Unreleased] failed: %v", err)
	}
	out.mustSay(t, "1 line(s) added under [Unreleased]")
}

func TestReleaseNotes(t *testing.T) {
	text := "# Changelog\n\n## [Unreleased]\n\n## [1.1.0] - 2026-02-01\n\n### Added\n\n- a thing\n\n" +
		"```\n## not a heading\n```\n\n## [1.0.0] - 2026-01-01\n\n### Fixed\n\n- b\n\n" +
		"[1.1.0]: https://h\n[1.0.0]: https://h\n"
	root := writeTree(t, map[string]string{"CHANGELOG.md": text})
	path := filepath.Join(root, "CHANGELOG.md")

	var out sink
	if err := releaseNotes(&out, []string{"v1.1.0", path}); err != nil {
		t.Fatal(err)
	}
	want := "### Added\n\n- a thing\n\n```\n## not a heading\n```\n"
	if out.String() != want {
		t.Errorf("notes = %q, want %q (verbatim, the fence kept whole)", out.String(), want)
	}
	out.Reset()
	if err := releaseNotes(&out, []string{"1.0.0", path}); err != nil || strings.Contains(out.String(), "https://") {
		t.Errorf("the oldest section carried the link footer or failed: %q, %v", out.String(), err)
	}
	for _, v := range []string{"Unreleased", "9.9.9"} {
		if err := releaseNotes(&out, []string{v, path}); err == nil {
			t.Errorf("an empty or missing section %s was accepted", v)
		}
	}
}

// A footer line with no URL yet still ends the section above it.
func TestReleaseNotesStopAtAnEmptyFooterLine(t *testing.T) {
	text := "## [Unreleased]\n\n## [1.0.0]\n\n- b\n\n[Unreleased]:\n[1.0.0]: https://h\n"
	path := filepath.Join(writeTree(t, map[string]string{"CHANGELOG.md": text}), "CHANGELOG.md")
	var out sink
	if err := releaseNotes(&out, []string{"1.0.0", path}); err != nil || out.String() != "- b\n" {
		t.Errorf("notes = %q, %v; want %q", out.String(), err, "- b\n")
	}
}

func TestChecklist(t *testing.T) {
	prereqs := "fmt vet tidy lint cover vuln licenses secrets leaks pins classes api-coverage api-fields " +
		"schema-diff smoke staleness checklist changelog-links transcript live-cover"
	files := func(doc string) map[string]string {
		return map[string]string{
			"Makefile":  "check: " + prereqs + "\n",
			"CLAUDE.md": "# x\n\n## Definition of done\n\n`make check`:\n\n```\n" + doc + "\n```\n\n## Next\n\n```\nother\n```\n",
		}
	}
	n, problems, err := checklistCheck(writeTree(t, files(prereqs)))
	if err != nil || len(problems) > 0 || n != 20 {
		t.Fatalf("an agreeing pair: %d, %v, %v", n, problems, err)
	}
	for name, tc := range map[string]struct{ doc, want string }{
		"missing": {strings.Replace(prereqs, " pins", "", 1), "check: runs pins and CLAUDE.md does not name it"},
		"extra":   {prereqs + " parity", "CLAUDE.md names parity and check: does not run it"},
		"twice":   {prereqs + " vet", "CLAUDE.md names vet twice"},
		"order":   {strings.Replace(prereqs, "fmt vet", "vet fmt", 1), "another order"},
	} {
		t.Run(name, func(t *testing.T) {
			_, problems, err := checklistCheck(writeTree(t, files(tc.doc)))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Errorf("want %q, got %v", tc.want, problems)
			}
		})
	}
	if _, _, err := checklistCheck(writeTree(t, map[string]string{"Makefile": "check: a\n", "CLAUDE.md": "# x\n"})); err == nil {
		t.Error("a CLAUDE.md with no definition of done was accepted")
	}
}

// changelogGoMod is go.mod as the go command writes it, with both forms
// of require.
const changelogGoMod = "module m\n\ngo 1.27.4\n\nrequire (\n\texample.com/a v1.0.0\n\tgithub.com/modelcontextprotocol/go-sdk v1.8.0\n" +
	"\texample.com/b v0.1.0 // indirect\n)\n\nrequire example.com/c v1.2.0\n"

// changelogGoModCommented carries the comments the go command keeps after
// `require (` and after `)`.
var changelogGoModCommented = strings.NewReplacer("require (\n", "require ( // direct\n", ")\n\n", ") // direct\n\n").
	Replace(changelogGoMod)

// A go.mod that changed only in its require entries needs no entry; any
// other change to it, or a bump of the MCP SDK, still does.
func TestChangelogCheckDependencyBump(t *testing.T) {
	isolateGit(t)
	bumpA := strings.NewReplacer("a v1.0.0", "a v1.1.0")
	cases := []struct {
		name  string
		goMod string // the base's go.mod; empty: changelogGoMod
		files map[string]string
		want  string // empty: passes
	}{
		{"a dependency bump", "", map[string]string{
			"go.mod": strings.NewReplacer("a v1.0.0", "a v1.1.0", "c v1.2.0", "c v1.3.0",
				"// indirect\n)", "// indirect\n\texample.com/d v0.2.0 // indirect\n)").Replace(changelogGoMod),
		}, ""},
		{"a dependency bump between commented parens", changelogGoModCommented, map[string]string{
			"go.mod": bumpA.Replace(changelogGoModCommented),
		}, ""},
		{"a dependency bump with code", "", map[string]string{
			"go.mod":          bumpA.Replace(changelogGoMod),
			"internal/a/a.go": "package a // changed\n",
		}, "1 shipped file(s) changed (internal/a/a.go)"},
		{"a go directive change", "", map[string]string{
			"go.mod": strings.Replace(changelogGoMod, "go 1.27.4", "go 1.27.5", 1),
		}, "1 shipped file(s) changed (go.mod)"},
		{"a toolchain directive", "", map[string]string{
			"go.mod": strings.Replace(changelogGoMod, "go 1.27.4\n", "go 1.27.4\n\ntoolchain go1.27.5\n", 1),
		}, "1 shipped file(s) changed (go.mod)"},
		{"a replace after a commented paren", changelogGoModCommented, map[string]string{
			"go.mod": changelogGoModCommented + "\nreplace example.com/a => example.com/fork v1.0.0\n",
		}, "1 shipped file(s) changed (go.mod)"},
		{"a bump of the MCP SDK", "", map[string]string{
			"go.mod": strings.Replace(changelogGoMod, "go-sdk v1.8.0", "go-sdk v1.9.0", 1),
		}, "1 shipped file(s) changed (go.mod)"},
		{"a require block left open", "", map[string]string{
			"go.mod": bumpA.Replace(strings.Replace(changelogGoMod, ")\n", "", 1)),
		}, "1 shipped file(s) changed (go.mod)"},
		{"a bump in a require the go command would not spell", strings.Replace(changelogGoMod, "require (", "require(", 1),
			map[string]string{"go.mod": bumpA.Replace(strings.Replace(changelogGoMod, "require (", "require(", 1))},
			"1 shipped file(s) changed (go.mod)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			goMod := tc.goMod
			if goMod == "" {
				goMod = changelogGoMod
			}
			root := gitRepo(t, map[string]string{
				"CHANGELOG.md":    "# Changelog\n\n## [Unreleased]\n\n[Unreleased]: https://h\n",
				"internal/a/a.go": "package a\n",
				"go.mod":          goMod,
			})
			writeFiles(t, root, tc.files)
			gitIn(t, root, "commit", "-qam", "topic")
			var out sink
			err := changelogCheck(&out, root, "HEAD~1", "HEAD", map[string]bool{"internal/a": true})
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("want a pass, got %v", err)
			case tc.want == "":
				out.mustSay(t, "a dependency bump needs no entry")
			case err == nil || !strings.Contains(err.Error(), tc.want):
				t.Errorf("want an error with %q, got %v", tc.want, err)
			}
		})
	}
}

// A base branch that moved its go directive after the fork leaves a
// topic's dependency bump a bump: the gate measures from the merge-base.
func TestChangelogCheckFromTheMergeBase(t *testing.T) {
	isolateGit(t)
	root := gitRepo(t, map[string]string{
		"CHANGELOG.md":    "# Changelog\n\n## [Unreleased]\n\n[Unreleased]: https://h\n",
		"internal/a/a.go": "package a\n",
		"go.mod":          changelogGoMod,
	})
	gitIn(t, root, "branch", "base")
	gitIn(t, root, "checkout", "-q", "-b", "topic")
	writeFiles(t, root, map[string]string{"go.mod": strings.Replace(changelogGoMod, "a v1.0.0", "a v1.1.0", 1)})
	gitIn(t, root, "commit", "-qam", "bump")
	gitIn(t, root, "checkout", "-q", "base")
	writeFiles(t, root, map[string]string{"go.mod": strings.Replace(changelogGoMod, "go 1.27.4", "go 1.27.5", 1)})
	gitIn(t, root, "commit", "-qam", "base moved")
	var out sink
	if err := changelogCheck(&out, root, "base", "topic", map[string]bool{"internal/a": true}); err != nil {
		t.Fatalf("a bump on a branch whose base moved failed: %v", err)
	}
	out.mustSay(t, "a dependency bump needs no entry")
}

// A change nobody using the server can see passes with a `Changelog: none`
// footer that says why, in any case; a footer with no reason, or a word
// that only starts with "none", waives nothing. A shipped file git quotes
// and the release build are watched.
func TestChangelogCheckWaiverAndWatchedFiles(t *testing.T) {
	isolateGit(t)
	shipped := map[string]bool{"internal/a": true}
	for _, tc := range []struct {
		name    string
		files   map[string]string
		message string
		refused bool
	}{
		{"a refactor with a Changelog: none footer", map[string]string{"internal/a/a.go": "package a // renamed\n"},
			"refactor\n\nChangelog: none, the text is unchanged line for line", false},
		{"a Changelog: None footer in another case", map[string]string{"internal/a/a.go": "package a // renamed\n"},
			"refactor\n\nChangelog: None, a rename", false},
		{"a refactor with no footer", map[string]string{"internal/a/a.go": "package a // renamed\n"}, "refactor", true},
		{"a Changelog: none footer with no reason", map[string]string{"internal/a/a.go": "package a // renamed\n"},
			"refactor\n\nChangelog: none", true},
		{"a Changelog footer whose word only starts with none", map[string]string{"internal/a/a.go": "package a // renamed\n"},
			"change\n\nChangelog: nonetheless user-visible", true},
		{"a shipped file whose name git quotes", map[string]string{"internal/a/naïve notes.md": "x\n"}, "change", true},
		{"the release build", map[string]string{".goreleaser.yaml": "version: 2\n"}, "change", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := gitRepo(t, map[string]string{
				"CHANGELOG.md":    "# Changelog\n\n## [Unreleased]\n\n[Unreleased]: https://h\n",
				"internal/a/a.go": "package a\n",
				"go.mod":          changelogGoMod,
			})
			writeFiles(t, root, tc.files)
			gitIn(t, root, "add", "-A")
			gitIn(t, root, "commit", "-qm", tc.message)
			var out sink
			err := changelogCheck(&out, root, "HEAD~1", "HEAD", shipped)
			switch {
			case tc.refused && err == nil:
				t.Errorf("changelogCheck passed: %s; want a refusal", out.String())
			case !tc.refused && err != nil:
				t.Errorf("changelogCheck = %v; want a pass", err)
			}
		})
	}
}
