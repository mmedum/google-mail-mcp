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
		{"a link with no heading", "## [Unreleased]\n[Unreleased]: https://h/compare/x...HEAD\n[0.9.0]: https://h\n",
			"[0.9.0]: is a link reference with no heading"},
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
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-qm", "one")

	write("internal/a/a.go", "package a\n\nvar X = 1\n")
	git("commit", "-qam", "two")
	var out sink
	if err := changelogCheck(&out, root, "HEAD~1", "HEAD", map[string]bool{"internal/a": true}); err == nil || !strings.Contains(err.Error(), "did not") {
		t.Errorf("a source change with no entry passed: %v", err)
	}

	write("CHANGELOG.md", "# Changelog\n\n## [Unreleased]\n\n- X exists\n\n[Unreleased]: https://h\n")
	git("commit", "-qam", "three")
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
