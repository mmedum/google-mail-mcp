package main

import (
	"encoding/json"
	"errors"
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

// The configuration page's profile row says what a profile name is in
// the server's own words, and an unwired rule is refused rather than
// found everywhere.
func TestStalenessProfile(t *testing.T) {
	const rule = "up to 64 characters from a to z"
	row := "| `" + mcpbProfileVar + "` | `--profile` | `default` | Named profile. A name is "
	stalenessNone(t, stalenessProfile("# Configuration\n\n"+row+rule+". |\n", rule))
	stalenessNone(t, stalenessProfile("# Configuration\r\n\r\n"+row+rule+". |\r\n", rule))
	stalenessHas(t, stalenessProfile(row+"up to 64 characters. |\n\nA name is "+rule+".\n", rule),
		"docs/configuration.md's GMAIL_PROFILE row does not say a profile name is \"up to 64 characters from a to z\"")
	stalenessHas(t, stalenessProfile("A name is "+rule+".\n", rule),
		"docs/configuration.md has no GMAIL_PROFILE row")
	stalenessHas(t, stalenessProfile("anything", ""), "did not reach the staleness check")
}

func TestStalenessGates(t *testing.T) {
	_, p := stalenessGates("`leaks` and `pins`", []string{"leaks", "pins"})
	stalenessNone(t, p)
	_, p = stalenessGates("`leaks`", []string{"leaks", "pins"})
	stalenessHas(t, p, "does not name the pins gate")
	_, p = stalenessGates("", []string{"leaks"})
	stalenessHas(t, p, "CONTRIBUTING.md is missing")
}

func TestStalenessNoVersionInProse(t *testing.T) {
	docs := map[string]string{
		"README.md":            "[![release](https://img/v1.2.3)](x)\nInstall `v1.2.3` with go.\n```\ngo install x@v1.2.3\n```\n",
		"CONTRIBUTING.md":      "ok",
		"docs/security.md":     "ok",
		"docs/architecture.md": "A client from v2.1.199 asks first.",
		"CHANGELOG.md":         "- Fixed in v4.0.2.",
		"CLAUDE.md":            "Pin golangci-lint v2.5.0.",
	}
	_, p := stalenessNoVersionInProse(docs)
	stalenessNone(t, p)
	docs["docs/security.md"] = "Fixed in v0.3.1."
	_, p = stalenessNoVersionInProse(docs)
	stalenessHas(t, p, "docs/security.md:1 writes v0.3.1")
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
	if stalenessScopeModes == nil || stalenessConfigEnv == nil || stalenessProfileRule == "" {
		t.Fatal("staleness_code.go did not wire the scope, settings and profile rule sources")
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
	readme := "# x\n\n## Tools\n\n| Tool | What |\n|---|---|\n| `get_profile` | a |\n| `list_labels` | b |\n\n## Safety\n" +
		"\n```json\n{\"permissions\": {\"ask\": [\"mcp__google-mail__get_profile\"]}}\n```\n"
	if n, p := stalenessReadmeTools(readme, []string{"get_profile", "list_labels"}, []string{"get_profile"}, nil); n != 2 || len(p) != 0 {
		t.Errorf("an agreeing table: %d rows, %v", n, p)
	}
	_, p := stalenessReadmeTools(readme, []string{"get_profile", "get_thread"}, []string{"get_profile"}, nil)
	if len(p) != 2 || !strings.Contains(strings.Join(p, "\n"), "get_thread") || !strings.Contains(strings.Join(p, "\n"), "list_labels") {
		t.Errorf("a table missing one tool and listing another: %v", p)
	}
	if _, p := stalenessReadmeTools("# x\n", []string{"get_profile"}, nil, nil); len(p) != 1 {
		t.Errorf("no table at all: %v", p)
	}
	// A binary that could not be read holds the table to nothing, and says so.
	_, p = stalenessReadmeTools(readme, nil, nil, errors.New("no build"))
	if want := []string{"the built binary's tools could not be read: no build"}; !slices.Equal(p, want) {
		t.Errorf("an unreadable binary: %q, want %q", p, want)
	}
	_, p = stalenessReadmeTools("# x\n", nil, nil, errors.New("no build"))
	if len(p) != 2 {
		t.Errorf("no table and an unreadable binary: %q", p)
	}
}

// A Claude Code permission rule in the README names a tool the binary
// registers, or it asks nobody; the README gives at least one; and every
// tool the README must ask about has one, or a client runs it unasked.
func TestStalenessReadmePermissionRules(t *testing.T) {
	table := "# x\n\n## Tools\n\n| Tool | What |\n|---|---|\n| `get_profile` | a |\n| `send_draft` | b |\n"
	tools := []string{"get_profile", "send_draft"}
	_, p := stalenessReadmeTools(table+"\n{\"ask\": [\"mcp__google-mail__send_draft\", \"mcp__google-mail__send_drafts\"]}\n",
		tools, []string{"send_draft"}, nil)
	if len(p) != 1 || p[0] != "README.md gives a permission rule for send_drafts, which the binary does not register" {
		t.Errorf("a rule for no tool: %q", p)
	}
	_, p = stalenessReadmeTools(table+"\n{\"ask\": [\"mcp__google-mail__get_profile\"]}\n", tools, []string{"send_draft"}, nil)
	if len(p) != 1 || p[0] != "README.md's permissions.ask block has no rule for send_draft, which sends, changes a setting or deletes for good" {
		t.Errorf("a must-ask tool left out: %q", p)
	}
	_, p = stalenessReadmeTools(table+"\n{\"allow\": [\"mcp__google-mail__send_draft\"]}\n", tools, []string{"send_draft"}, nil)
	if len(p) != 1 || p[0] != "README.md's permissions.ask block has no rule for send_draft, which sends, changes a setting or deletes for good" {
		t.Errorf("a must-ask tool allowed rather than asked: %q", p)
	}
	_, p = stalenessReadmeTools(table+"\n{\"ask\": [\n  \"mcp__google-mail__get_profile\",\n  \"mcp__google-mail__send_draft\"\n]}\n",
		tools, []string{"send_draft"}, nil)
	if len(p) != 0 {
		t.Errorf("an ask list over lines: %q", p)
	}
	_, p = stalenessReadmeTools(table+"\n\"mcp__google-mail__send_draft\"\n", tools, nil, nil)
	stalenessHas(t, p, "registers no tool the README must ask about")
	_, p = stalenessReadmeTools(table, tools, []string{"send_draft"}, nil)
	stalenessHas(t, p, "README.md gives no Claude Code permission rule")
}

// A client reads a tool as destructive unless it is read-only or says it
// is not, as MCP's defaults have it.
func TestDumpDestructive(t *testing.T) {
	for annotations, want := range map[string]bool{
		``:                          true,
		`{"readOnlyHint":true}`:     false,
		`{"destructiveHint":false}`: false,
		`{"destructiveHint":true}`:  true,
		`{"openWorldHint":true}`:    true,
		`{"readOnlyHint":false,"destructiveHint":false}`: false,
	} {
		if got := dumpDestructive(dumpTool{Name: "x", Annotations: json.RawMessage(annotations)}); got != want {
			t.Errorf("%s: %v, want %v", annotations, got, want)
		}
	}
}

// The README must ask about every tool annotated destructive but the
// Write kind. A tool with no kind is held to the rule.
func TestStalenessMustAsk(t *testing.T) {
	destructive, notDestructive := json.RawMessage(`{"destructiveHint":true}`), json.RawMessage(`{"destructiveHint":false}`)
	d := schemaDump{
		Tools: []dumpTool{
			{Name: "trash", Annotations: destructive},
			{Name: "send_draft", Annotations: destructive},
			{Name: "update_signature", Annotations: destructive},
			{Name: "delete_permanently", Annotations: destructive},
			{Name: "delete_draft", Annotations: destructive},
			{Name: "set_vacation", Annotations: destructive},
			{Name: "create_label", Annotations: notDestructive},
			{Name: "get_profile", Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
			{Name: "unkinded", Annotations: destructive},
		},
		Kinds: map[string]string{"trash": "write", "send_draft": "send", "update_signature": "settings",
			"delete_permanently": "destructive", "delete_draft": "write-for-good", "set_vacation": "auto-reply",
			"create_label": "adding", "get_profile": "read"},
	}
	want := []string{"send_draft", "update_signature", "delete_permanently", "delete_draft", "set_vacation", "unkinded"}
	if got := stalenessMustAsk(&d); !slices.Equal(got, want) {
		t.Errorf("stalenessMustAsk = %v, want %v", got, want)
	}
}
