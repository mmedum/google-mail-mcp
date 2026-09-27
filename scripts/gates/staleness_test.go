package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func stalenessHas(t *testing.T, problems []string, want string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Errorf("no problem says %q; got %q", want, problems)
}

func stalenessNone(t *testing.T, problems []string) {
	t.Helper()
	if len(problems) > 0 {
		t.Errorf("unexpected problems: %q", problems)
	}
}

func stalenessSet(paths ...string) func(string) bool {
	return func(p string) bool { return slices.Contains(paths, p) }
}

const stalenessClaude = "# x\n\n## Where things go\n\nPlanned.\n\n" +
	"- `cmd/app/` wiring.\n- `internal/a/` a; `internal/b/` b; `internal/c/` c; `internal/d/` d; `internal/e/` e;\n" +
	"  `internal/f/` f; `internal/g/` g; `internal/h/` h; `internal/i/` i; `internal/j/` j.\n" +
	"- `internal/gapi/` client, with\n  `gmailtest/` the fake.\n" +
	"- `scripts/gates/` gates; `scripts/internal/` shared; `testdata/` data.\n\n## Next\n\n`internal/zzz/`\n"

func TestStalenessPackageMap(t *testing.T) {
	pkgs := []string{"cmd/app", "internal/a", "internal/b", "internal/c", "internal/d", "internal/e", "internal/f",
		"internal/g", "internal/h", "internal/i", "internal/j", "internal/gapi", "internal/gapi/gmailtest",
		"scripts/gates", "scripts/internal/redact"}
	dirs := append(slices.Clone(pkgs), "scripts/internal", "testdata")
	hasGo := stalenessSet(pkgs...)
	n, p := stalenessPackageMap(stalenessClaude, pkgs, stalenessSet(dirs...), hasGo)
	stalenessNone(t, p)
	if n != 16 {
		t.Errorf("read %d listed paths, want 16 (the next section's path is not the map)", n)
	}

	_, p = stalenessPackageMap(stalenessClaude, append(slices.Clone(pkgs), "internal/version"), stalenessSet(dirs...), hasGo)
	stalenessHas(t, p, "does not name the package internal/version/")

	_, p = stalenessPackageMap(stalenessClaude, pkgs[1:], stalenessSet(dirs[1:]...), hasGo)
	stalenessHas(t, p, "lists cmd/app/, which does not exist")

	// A subpackage of a package must be named itself.
	_, p = stalenessPackageMap(stalenessClaude, append(slices.Clone(pkgs), "internal/a/sub"), stalenessSet(dirs...), hasGo)
	stalenessHas(t, p, "internal/a/sub/")

	_, p = stalenessPackageMap("## Where things go\n\n- `a/`\n", pkgs, stalenessSet(dirs...), hasGo)
	stalenessHas(t, p, "the extractor is not reading it")
}

func TestStalenessPaths(t *testing.T) {
	var b strings.Builder
	for i := range 20 {
		fmt.Fprintf(&b, "See `internal/p%d/file.go`.\n", i)
	}
	exists := func(p string) bool {
		return strings.HasPrefix(p, "internal/p") || p == "docs/other.md" || p == ".github/workflows/ci.yml"
	}
	docs := map[string]string{
		"README.md": b.String(),
		"docs/x.md": "Read [other](other.md), `ci.yml`, `users/me`, `mail.google.com/mail/u/`, `https://x/y`, `~/a/b`.\n" +
			"```\ninternal/gone/in-a-fence.go\n```\n",
	}
	top := map[string]bool{"internal": true, "docs": true, ".github": true}
	n, p := stalenessPaths(docs, exists, top)
	stalenessNone(t, p)
	if n != 22 {
		t.Errorf("examined %d, want 22", n)
	}

	docs["docs/x.md"] += "`internal/gone.go` and [gone](../nowhere.md) and `release.md`\n"
	_, p = stalenessPaths(docs, exists, top)
	stalenessHas(t, p, "docs/x.md names internal/gone.go")
	stalenessHas(t, p, "links to ../nowhere.md")
	stalenessHas(t, p, "names release.md")

	_, p = stalenessPaths(map[string]string{"README.md": "`internal/x`"}, exists, top)
	stalenessHas(t, p, "the extractor has stopped reading them")
}

func TestStalenessEnv(t *testing.T) {
	code := []string{"GMAIL_A", "GMAIL_B", "GMAIL_C", "GMAIL_D", "GMAIL_E", "GMAIL_F", "GMAIL_G", "GMAIL_H"}
	doc := strings.Join(code, " ")
	_, p := stalenessEnv(doc, code)
	stalenessNone(t, p)
	_, p = stalenessEnv(doc+" GMAIL_GONE", code)
	stalenessHas(t, p, "documents GMAIL_GONE, which the code does not read")
	_, p = stalenessEnv(strings.Replace(doc, "GMAIL_H", "", 1), code)
	stalenessHas(t, p, "does not document GMAIL_H")
	// GMAIL_A must not be satisfied by GMAIL_AB.
	_, p = stalenessEnv(strings.Replace(doc, "GMAIL_A ", "GMAIL_AB ", 1), code)
	stalenessHas(t, p, "does not document GMAIL_A")
	_, p = stalenessEnv("", code)
	stalenessHas(t, p, "missing")
}

func TestStalenessGates(t *testing.T) {
	_, p := stalenessGates("`leaks` and `pins`", []string{"leaks", "pins"})
	stalenessNone(t, p)
	_, p = stalenessGates("`leaks`", []string{"leaks", "pins"})
	stalenessHas(t, p, "does not name the pins gate")
	_, p = stalenessGates("", []string{"leaks"})
	stalenessHas(t, p, "docs/development.md is missing")
}

func TestStalenessStatus(t *testing.T) {
	const designOnly = "**Status: design only. Nothing is built or tagged.** More."
	stalenessNone(t, stalenessStatus(designOnly, "", "", false))
	stalenessHas(t, stalenessStatus(designOnly, "", "", true), "says nothing is built")
	stalenessHas(t, stalenessStatus("**Status: phase 0 in progress.**", "", "", true), "does not say so")
	stalenessHas(t, stalenessStatus("**Status: v0.1.0 released.**", "", "", true), "nothing is tagged or released")
	stalenessNone(t, stalenessStatus("**Status: v0.1.0 released.**", "", "0.1.0", true))
	// The release commit writes the heading before the tag exists.
	stalenessNone(t, stalenessStatus("**Status: v0.2.0 released.**", "## [0.2.0] - x\n## [0.1.0]", "0.1.0", true))
	stalenessHas(t, stalenessStatus("**Status: v0.1.0 released.**", "", "0.2.0", true), "the newest release is 0.2.0")
	stalenessHas(t, stalenessStatus(designOnly, "", "0.1.0", false), "says nothing is tagged")
	stalenessHas(t, stalenessStatus("no status", "", "", false), "no **Status")
}

func TestStalenessNoVersionInProse(t *testing.T) {
	docs := map[string]string{
		"README.md":            "[![release](https://img/v1.2.3)](x)\nInstall `v1.2.3` with go.\n```\ngo install x@v1.2.3\n```\n",
		"CONTRIBUTING.md":      "ok",
		"docs/security.md":     "ok",
		"docs/architecture.md": "Phase 0 (v0.1.0)",
	}
	_, p := stalenessNoVersionInProse(docs)
	stalenessNone(t, p)
	docs["docs/security.md"] = "Fixed in v0.3.1."
	_, p = stalenessNoVersionInProse(docs)
	stalenessHas(t, p, "docs/security.md:1 writes v0.3.1")
}

const stalenessToolTable = "## 8. Tool surface\n\nFour tools: three by default, two in read-only mode, one\nfewer in each when `X` is unset.\n\n" +
	"| Tool | Kind | Registered | Scope |\n|---|---|---|---|\n" +
	"| `get_a` | Read | always | r |\n| `get_b` | Read | always | r |\n| `get_c` | Read | always | r |\n" +
	"| `get_d` | Read | always | r |\n| `get_e` | Read | always | r |\n| `get_f` | Read | always | r |\n" +
	"| `dl` | Read | `GMAIL_LOCAL_DIR` set | r |\n| `w` | Write | not read-only | m |\n| `s` | Send | `GMAIL_ENABLE_SEND` | m |\n" +
	"\n### 8a. Every\n\n| `x_y` | nope | nope |\n"

func TestStalenessToolCounts(t *testing.T) {
	good := strings.Replace(stalenessToolTable, "Four tools: three by default, two in read-only mode",
		"Nine tools: eight by default, seven in read-only mode", 1)
	n, p := stalenessToolCounts(good, []string{"get_a", "dl"}, nil)
	stalenessNone(t, p)
	if n != 9 {
		t.Errorf("read %d rows, want 9", n)
	}
	_, p = stalenessToolCounts(stalenessToolTable, nil, nil)
	stalenessHas(t, p, `§8 says "Four" tools; the table says 9`)
	stalenessHas(t, p, `"two" in read-only mode; the table says 7`)
	_, p = stalenessToolCounts(good, []string{"secret"}, nil)
	stalenessHas(t, p, "registers secret, which §8's table does not list")
	_, p = stalenessToolCounts(good, nil, fmt.Errorf("no build"))
	stalenessHas(t, p, "could not be read")
}

func TestStalenessVerdictCounts(t *testing.T) {
	var verdicts []string
	for i := range 60 {
		verdicts = append(verdicts, []string{"Used: x", "Gated: y", "Deferred (§17): z", "Written off: w"}[i%4])
	}
	docs := map[string]string{
		"docs/architecture.md": "### 8a. Every published method, with a verdict\n\nAll 60 methods of the discovery document.\n\n" +
			"Fifteen used, fifteen gated, fifteen deferred to §17, fifteen written off.\n",
		"CLAUDE.md": "All sixty are in §8a.",
	}
	n, p := stalenessVerdictCounts(docs, verdicts, 60)
	stalenessNone(t, p)
	if n != 60 {
		t.Errorf("read %d", n)
	}
	_, p = stalenessVerdictCounts(docs, verdicts, 61)
	stalenessHas(t, p, "testdata/api-surface.json has 61 methods")
	verdicts[0] = "Maybe: x"
	_, p = stalenessVerdictCounts(docs, verdicts, 60)
	stalenessHas(t, p, "starting with none of")
	stalenessHas(t, p, `"Fifteen" used`)
	// The table, while it exists, wins over the record.
	docs["docs/architecture.md"] += "| `a.b` | GET | Used: x |\n"
	_, p = stalenessVerdictCounts(docs, verdicts, 60)
	stalenessHas(t, p, "§8a's table holds 1 verdicts")
}

func TestStalenessNumber(t *testing.T) {
	for s, want := range map[string]int{"79": 79, "Twenty-three": 23, "twelve": 12, "thirty": 30, "Seventy-nine": 79} {
		if got, ok := stalenessNumber(s); !ok || got != want {
			t.Errorf("%s = %d, %v", s, got, ok)
		}
	}
	for _, s := range []string{"the", "ten-one", "twenty-twelve"} {
		if _, ok := stalenessNumber(s); ok {
			t.Errorf("%s read as a number", s)
		}
	}
}

func TestStalenessScopeBlock(t *testing.T) {
	modes := []stalenessMode{
		{Name: "read-only", Flags: []string{"GMAIL_READ_ONLY=true"}, Scopes: []string{"r"}},
		{Name: "default", Scopes: []string{"m"}},
		{Name: "destructive", Flags: []string{"GMAIL_ENABLE_DESTRUCTIVE=true"}, Scopes: []string{"f"}},
	}
	block := stalenessScopeTable(modes)
	doc := "x\n<!-- scopes:begin (generated) -->\n" + block + "<!-- scopes:end -->\ny"
	_, p := stalenessScopeBlock(doc, modes)
	stalenessNone(t, p)
	if !strings.Contains(block, "| default | nothing (the default) | `m` |") {
		t.Errorf("rendering changed: %s", block)
	}
	_, p = stalenessScopeBlock(strings.Replace(doc, "`f`", "`g`", 1), modes)
	stalenessHas(t, p, "differs from what internal/scopes generates")
	_, p = stalenessScopeBlock("x\n<!-- scopes:begin -->\n<!-- scopes:end -->\n", modes)
	stalenessHas(t, p, "differs")
	_, p = stalenessScopeBlock("nothing", modes)
	stalenessHas(t, p, "has no <!-- scopes:begin")
	_, p = stalenessScopeBlock(doc, nil)
	stalenessHas(t, p, "scope source not wired")
}

func TestStalenessSectionStopsAtSameLevel(t *testing.T) {
	text := "# T\n## A\na\n### A1\nsub\n## B\nb\n"
	if got := markdownSection(text, "## A"); got != "\na\n### A1\nsub\n" {
		t.Errorf("section %q", got)
	}
	if got := stalenessUnreleased("## [Unreleased]\n- x\n\n[Unreleased]: http://x\n"); strings.Contains(got, "http") {
		t.Errorf("link references kept: %q", got)
	}
}

// The wiring to internal/scopes is live, and the modes carry scopes.
func TestStalenessScopeSourceIsWired(t *testing.T) {
	if stalenessScopeModes == nil || stalenessConfigEnv == nil {
		t.Fatal("staleness_code.go did not wire the scope and settings sources")
	}
	modes := stalenessScopeModes()
	if len(modes) < 3 {
		t.Fatalf("%d modes", len(modes))
	}
	for _, m := range modes {
		if len(m.Scopes) == 0 {
			t.Errorf("mode %s has no scopes", m.Name)
		}
	}
	if len(stalenessConfigEnv()) < 8 {
		t.Errorf("config reports %d variables", len(stalenessConfigEnv()))
	}
}

func TestStalenessReadmeToolsBothWays(t *testing.T) {
	readme := "# x\n\n## Tools\n\n| Tool | What |\n|---|---|\n| `get_profile` | a |\n| `list_labels` | b |\n\n## Safety\n"
	if n, p := stalenessReadmeTools(readme, []string{"get_profile", "list_labels"}); n != 2 || len(p) != 0 {
		t.Errorf("an agreeing table: %d rows, %v", n, p)
	}
	_, p := stalenessReadmeTools(readme, []string{"get_profile", "get_thread"})
	if len(p) != 2 || !strings.Contains(strings.Join(p, "\n"), "get_thread") || !strings.Contains(strings.Join(p, "\n"), "list_labels") {
		t.Errorf("a table missing one tool and listing another: %v", p)
	}
	if _, p := stalenessReadmeTools("# x\n", []string{"get_profile"}); len(p) != 1 {
		t.Errorf("no table at all: %v", p)
	}
}
