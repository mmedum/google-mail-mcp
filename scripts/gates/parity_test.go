package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// parityTestGates is how many gates the fixture's check runs: enough to
// clear every floor.
const parityTestGates = 20

// parityFixture is a repository and registry that agree. A test edits
// one of them to break one thing.
func parityFixture() (map[string]string, map[string]command) {
	noop := func(io.Writer, []string) error { return nil }
	registry := map[string]command{
		"changelog":     {run: noop, runsIn: inCI},
		"release-notes": {run: noop, runsIn: inRelease},
		"mcpb-pack":     {run: noop, runsIn: inRelease},
		"api-diff":      {run: noop, runsIn: manual},
	}
	var mk strings.Builder
	mk.WriteString("GO ?= go\nGATES ?= $(GO) run ./scripts/gates\n\n")
	var prereqs, steps []string
	for i := range parityTestGates {
		name := fmt.Sprintf("g%02d", i)
		registry[name] = command{run: noop, runsIn: inCheck}
		prereqs = append(prereqs, name)
		fmt.Fprintf(&mk, ".PHONY: %s\n%s: ## a gate\n\t$(GATES) %s\n\n", name, name, name)
		steps = append(steps, fmt.Sprintf("      - name: %s\n        run: make %s\n", name, name))
	}
	mk.WriteString("vet:\n\t$(GO) vet ./...\n\t$(GO) vet -tags=live ./...\n\t$(GO) vet -tags=evals ./...\n\n")
	mk.WriteString("test:\n\t$(GO) test ./...\n\ncover: test\n\t@echo floors\n\n")
	mk.WriteString("leaks-history:\n\t$(GATES) leaks history\n\n")
	mk.WriteString("changelog:\n\t$(GATES) changelog $(BASE) $(HEAD)\n\n")
	mk.WriteString("api-diff:\n\t$(GATES) api-diff\n\n")
	mk.WriteString("check: " + strings.Join(prereqs, " ") + " vet cover\n")

	ci := "on: [push]\njobs:\n  gates:\n    steps:\n" + strings.Join(steps, "") +
		// Recipe-mapped rather than by make: the same command, verbatim.
		"      - name: vet\n        run: |\n          # the tagged code too\n          go vet ./...\n" +
		"          go vet -tags=live ./...\n          go vet -tags=evals ./...\n" +
		"      - name: cover\n        run: make cover\n" +
		"      - name: history\n        run: make leaks-history\n" +
		"      - name: changelog\n        run: make changelog BASE=origin/main\n"
	return map[string]string{
		"Makefile":                      mk.String(),
		".github/workflows/ci.yml":      ci,
		".github/workflows/release.yml": "on: [push]\njobs:\n  r:\n    steps:\n      - run: go run ./scripts/gates release-notes v1\n",
		".goreleaser.yaml":              "post: go run ./scripts/gates mcpb-pack dist 1 out\n",
		".githooks/pre-commit":          "#!/bin/sh\nexec go run ./scripts/gates precommit\n",
	}, registry
}

func TestParityPassesWhenTheyAgree(t *testing.T) {
	files, registry := parityFixture()
	r, err := parityCheck(writeTree(t, files), registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.problems) > 0 {
		t.Fatalf("problems on an agreeing fixture:\n%s", strings.Join(r.problems, "\n"))
	}
	if r.prereqs != parityTestGates+2 || r.checkGates != parityTestGates || r.vetTags != 2 {
		t.Errorf("read %d prerequisites, %d gates, %d vet tags", r.prereqs, r.checkGates, r.vetTags)
	}
}

func TestParityRefuses(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		old, new string
		registry func(map[string]command)
		want     string
	}{
		{name: "a check target CI does not run", file: ".github/workflows/ci.yml",
			old: "run: make g03\n", new: "run: echo g03\n", want: "`make check` runs g03 and ci.yml never does"},
		{name: "a step's name is not a command", file: ".github/workflows/ci.yml",
			old: "      - name: g04\n        run: make g04\n", new: "      - name: make g04\n        run: echo hi\n",
			want: "`make check` runs g04 and ci.yml never does"},
		{name: "a comment is not a command", file: ".github/workflows/ci.yml",
			old: "run: make g05\n", new: "run: |\n          # make g05\n          true\n",
			want: "`make check` runs g05 and ci.yml never does"},
		{name: "a CI target check does not run", file: ".github/workflows/ci.yml",
			old: "run: make cover\n", new: "run: make cover api-diff\n",
			want: "ci.yml runs `make api-diff` and `make check` does not"},
		{name: "a stale CI-only excuse", file: ".github/workflows/ci.yml",
			old: "run: make leaks-history\n", new: "run: echo\n", want: "leaks-history is excused as CI-only"},
		{name: "an inCheck gate no target calls", registry: func(r map[string]command) {
			r["ghost"] = command{runsIn: inCheck}
		}, want: "the ghost gate runs in check by the registry, and no target"},
		{name: "a manual gate that check runs", registry: func(r map[string]command) {
			r["g07"] = command{runsIn: manual}
		}, want: "the g07 gate is manual by the registry, and `make check` runs it"},
		{name: "a CI gate CI does not run", file: ".github/workflows/ci.yml",
			old: "run: make changelog BASE=origin/main\n", new: "run: echo\n",
			want: "the changelog gate runs in CI by the registry, and ci.yml never calls it"},
		{name: "a release gate nothing calls", file: ".goreleaser.yaml",
			old: "mcpb-pack", new: "mcpb-packer", want: "the mcpb-pack gate runs in release"},
		{name: "a release gate with no reason", registry: func(r map[string]command) {
			r["sign"] = command{runsIn: inRelease}
		}, want: "the sign gate runs in release and parityReleaseOnly gives no reason"},
		{name: "a vet tag only check uses", file: ".github/workflows/ci.yml",
			old: "          go vet -tags=evals ./...\n", new: "",
			want: "`make check` vets with -tags=evals and ci.yml does not"},
		{name: "the zero pipeline", registry: func(r map[string]command) {
			r["g01"] = command{}
		}, want: "the g01 gate declares no pipeline"},
		{name: "the hook", file: ".githooks/pre-commit", old: "precommit", new: "leaks",
			want: "does not run `go run ./scripts/gates precommit`"},
		{name: "a floor on prerequisites", file: "Makefile",
			old: "check: g00 g01 g02 g03", new: "check:", want: "check: has 18 prerequisites, want at least 20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, registry := parityFixture()
			if tc.file != "" {
				if !strings.Contains(files[tc.file], tc.old) {
					t.Fatalf("%s has no %q to break", tc.file, tc.old)
				}
				files[tc.file] = strings.Replace(files[tc.file], tc.old, tc.new, 1)
			}
			if tc.registry != nil {
				tc.registry(registry)
			}
			r, err := parityCheck(writeTree(t, files), registry)
			if err != nil {
				t.Fatal(err)
			}
			if joined := strings.Join(r.problems, "\n"); !strings.Contains(joined, tc.want) {
				t.Errorf("want %q among the problems, got:\n%s", tc.want, joined)
			}
		})
	}
}

// Every release command in the real registry names where it runs.
func TestParityExcusesEveryReleaseCommand(t *testing.T) {
	release := commandsRunningIn(inRelease)
	if len(release) == 0 {
		t.Fatal("the registry has no release commands; this test would hold nothing")
	}
	for _, name := range release {
		if strings.TrimSpace(parityReleaseOnly[name]) == "" {
			t.Errorf("%s runs in release and parityReleaseOnly gives no reason", name)
		}
	}
	for name := range parityReleaseOnly {
		if !slices.Contains(release, name) {
			t.Errorf("parityReleaseOnly excuses %s, which is not a release command", name)
		}
	}
	for name, reason := range parityCIOnly {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is excused as CI-only with no reason", name)
		}
	}
}

func TestParityMakeTargets(t *testing.T) {
	got := parityMakeTargets(`make changelog CHANGELOG_BASE="origin/${BASE_REF}" -j2 cover`)
	if !slices.Equal(got, []string{"changelog", "cover"}) {
		t.Errorf("parityMakeTargets = %v", got)
	}
	if parityMakeTargets("go test ./...") != nil {
		t.Error("a go command was read as make")
	}
}

func TestMakefileParse(t *testing.T) {
	mk := parseMakefile("A ?= x\nB = $(A)/y # note\nC := $(B) \\\n  z\n.PHONY: t\nt: u v ## help\n\t$(C) run\n\t# comment\nu:\n\techo\n")
	if got := mk.expand("$(B)"); got != "x/y" {
		t.Errorf("expand = %q", got)
	}
	if got := mk.recipe("t"); !slices.Equal(got, []string{"x/y z run"}) {
		t.Errorf("recipe = %q", got)
	}
	if got := mk.closure("t"); !slices.Equal(got, []string{"t", "u", "v"}) {
		t.Errorf("closure = %v", got)
	}
	if _, err := mk.variable("NONE"); err == nil {
		t.Error("a missing variable was found")
	}
}
