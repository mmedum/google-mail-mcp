package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
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
		"schema-ack":    {run: noop, runsIn: inCI},
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
	mk.WriteString("schema-ack:\n\t$(GATES) schema-ack $(BASE) $(HEAD)\n\n")
	mk.WriteString("api-diff:\n\t$(GATES) api-diff\n\n")
	mk.WriteString("check: " + strings.Join(prereqs, " ") + " vet cover\n")

	ci := "on: [push]\njobs:\n  gates:\n    steps:\n" + strings.Join(steps, "") +
		// Recipe-mapped rather than by make: the same command, verbatim.
		"      - name: vet\n        run: |\n          # the tagged code too\n          go vet ./...\n" +
		"          go vet -tags=live ./...\n          go vet -tags=evals ./...\n" +
		"      - name: cover\n        run: make cover\n" +
		"      - name: history\n        run: make leaks-history\n" +
		"      - name: changelog\n        run: make changelog BASE=origin/main\n" +
		"      - name: schema-ack\n        run: make schema-ack BASE=origin/main\n"
	return map[string]string{
		"Makefile":                      mk.String(),
		".github/workflows/ci.yml":      ci,
		".github/workflows/release.yml": "on: [push]\njobs:\n  r:\n    steps:\n      - run: go run ./scripts/gates release-notes v1\n",
		".goreleaser.yaml":              "post: go run ./scripts/gates mcpb-pack dist 1 out\n",
		".githooks/pre-commit":          "#!/bin/sh\nexec go run ./scripts/gates precommit\n",
		// The tags the tree needs, and files that need none: an OS is the
		// go command's to set, and testdata is not built.
		"scripts/livemail/main.go":          "//go:build live\n\npackage main\n",
		"scripts/evals/main.go":             "//go:build evals && unix\n\npackage main\n",
		"internal/keyring/keyring_other.go": "//go:build !windows\n\npackage keyring\n",
		"internal/gapi/testdata/x.go":       "//go:build fixture\n\npackage x\n",
	}, registry
}

// parityIn runs the gate over files in a git checkout of their own.
func parityIn(t *testing.T, files map[string]string, registry map[string]command) *parityReport {
	t.Helper()
	r, err := parityCheck(docsCheckout(t, files), registry)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParityPassesWhenTheyAgree(t *testing.T) {
	files, registry := parityFixture()
	r := parityIn(t, files, registry)
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
			want: "scripts/evals/main.go builds only with -tags=evals and ci.yml vets no such pass"},
		{name: "a vet tag only CI uses", file: "Makefile",
			old: "\t$(GO) vet -tags=live ./...\n", new: "",
			want: "scripts/livemail/main.go builds only with -tags=live and `make check` vets no such pass"},
		{name: "a vet commented out of its recipe", file: "Makefile",
			old: "\t$(GO) vet -tags=live ./...\n", new: "\t# $(GO) vet -tags=live ./...\n",
			want: "scripts/livemail/main.go builds only with -tags=live and `make check` vets no such pass"},
		{name: "a test that turns vet off is not a vet pass", file: ".github/workflows/ci.yml",
			old: "          go vet -tags=evals ./...\n", new: "          go test -vet=off -tags=evals ./...\n",
			want: "scripts/evals/main.go builds only with -tags=evals and ci.yml vets no such pass"},
		{name: "a test on the vet's line is not a vet pass", file: ".github/workflows/ci.yml",
			old: "          go vet -tags=evals ./...\n", new: "          go vet ./... && go test -tags=evals ./...\n",
			want: "scripts/evals/main.go builds only with -tags=evals and ci.yml vets no such pass"},
		{name: "the zero pipeline", registry: func(r map[string]command) {
			r["g01"] = command{}
		}, want: "the g01 gate declares no pipeline"},
		{name: "the hook", file: ".githooks/pre-commit", old: "precommit", new: "leaks",
			want: "does not run `go run ./scripts/gates precommit`"},
		{name: "a floor on prerequisites", file: "Makefile",
			old: "check: g00 g01 g02 g03", new: "check:", want: "check: has 18 prerequisites, want at least 20"},
		{name: "a floor on the tree's tags", file: "scripts/livemail/main.go",
			old: "//go:build live\n", new: "",
			want: "the tree's //go:build lines need 1 -tags lists, want at least 2 (live and evals)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, registry := parityFixture()
			if tc.file != "" {
				applyEdits(t, files, replace(tc.file, tc.old, tc.new))
			}
			if tc.registry != nil {
				tc.registry(registry)
			}
			r := parityIn(t, files, registry)
			if joined := strings.Join(r.problems, "\n"); !strings.Contains(joined, tc.want) {
				t.Errorf("want %q among the problems, got:\n%s", tc.want, joined)
			}
		})
	}
}

// parityCase edits the agreeing fixture and wants exactly these problems,
// or none when want is nil.
type parityCase struct {
	name  string
	edits []docsEdit
	want  []string
}

func checkParityCases(t *testing.T, cases []parityCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, registry := parityFixture()
			applyEdits(t, files, tc.edits...)
			if got := parityIn(t, files, registry).problems; !slices.Equal(got, tc.want) {
				t.Errorf("problems\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Each -tags list the tree's files need has a vet pass on both sides that
// sets all of it.
func TestParityHoldsTheTreesTagsToAVetPass(t *testing.T) {
	const ci = ".github/workflows/ci.yml"
	checkParityCases(t, []parityCase{
		{"a file an OS tag and a custom tag build, which no pass vets", []docsEdit{
			add("internal/e2e/e2e_test.go", "//go:build e2e && linux\n\npackage e2e\n"),
		}, []string{
			"internal/e2e/e2e_test.go builds only with -tags=e2e and `make check` vets no such pass; a contributor cannot see it break",
			"internal/e2e/e2e_test.go builds only with -tags=e2e and ci.yml vets no such pass; it compiles only on a maintainer's machine",
		}},
		{"two tags, set by one pass on each side, the flag anywhere among vet's arguments", []docsEdit{
			add("internal/soak/soak_test.go", "//go:build integration && slow\n\npackage soak\n"),
			replace("Makefile", "\t$(GO) vet -tags=evals ./...\n", "\t$(GO) vet -tags=evals ./...\n\t$(GO) vet -v -tags=integration,slow ./...\n"),
			replace(ci, "          go vet -tags=evals ./...\n", "          go vet -tags=evals ./...\n          go vet -v -tags=integration,slow ./...\n"),
		}, nil},
		{"two tags set by two passes", []docsEdit{
			add("internal/soak/soak_test.go", "//go:build integration && slow\n\npackage soak\n"),
			replace("Makefile", "\t$(GO) vet -tags=evals ./...\n", "\t$(GO) vet -tags=evals ./...\n\t$(GO) vet -tags=integration ./...\n\t$(GO) vet -tags=slow ./...\n"),
			replace(ci, "          go vet -tags=evals ./...\n", "          go vet -tags=evals ./...\n          go vet -tags=integration ./...\n"+
				"          go vet -tags=slow ./...\n          go vet -tags=integration,slow ./...\n"),
		}, []string{
			"internal/soak/soak_test.go builds only with -tags=integration,slow and `make check` vets no such pass; a contributor cannot see it break",
		}},
		{"a file either of two tags builds, with one of them vetted", []docsEdit{
			replace("scripts/evals/main.go", "//go:build evals && unix\n", "//go:build live || evals\n"),
			replace("Makefile", "\t$(GO) vet -tags=evals ./...\n", ""),
			replace(ci, "          go vet -tags=evals ./...\n", ""),
		}, nil},
		{"a file either of two tags builds, with neither vetted", []docsEdit{
			replace("scripts/evals/main.go", "//go:build evals && unix\n", "//go:build live || evals\n"),
			replace("Makefile", "\t$(GO) vet -tags=live ./...\n\t$(GO) vet -tags=evals ./...\n", ""),
			replace(ci, "          go vet -tags=live ./...\n          go vet -tags=evals ./...\n", ""),
		}, []string{
			"scripts/evals/main.go builds only with -tags=evals or -tags=live and `make check` vets no such pass; a contributor cannot see it break",
			"scripts/evals/main.go builds only with -tags=evals or -tags=live and ci.yml vets no such pass; it compiles only on a maintainer's machine",
			"scripts/livemail/main.go builds only with -tags=live and `make check` vets no such pass; a contributor cannot see it break",
			"scripts/livemail/main.go builds only with -tags=live and ci.yml vets no such pass; it compiles only on a maintainer's machine",
		}},
		{"a vet in check's own recipe, which CI does not run", []docsEdit{
			add("internal/soak/soak_test.go", "//go:build soak\n\npackage soak\n"),
			replace("Makefile", " vet cover\n", " vet cover\n\t$(GO) vet -tags=soak ./...\n"),
		}, []string{
			"internal/soak/soak_test.go builds only with -tags=soak and ci.yml vets no such pass; it compiles only on a maintainer's machine",
		}},
	})
}

// The Makefile is read as make reads it: a target's rules merge their
// prerequisites, and a recipe line's @, - and + are not part of the
// command CI runs.
func TestParityReadsEveryRuleOfATarget(t *testing.T) {
	checkParityCases(t, []parityCase{
		{"a prerequisite a second check rule adds", []docsEdit{
			replace("Makefile", "api-diff:\n", "check: api-diff\n\napi-diff:\n"),
		}, []string{
			"`make check` runs api-diff and ci.yml never does",
			"the api-diff gate is manual by the registry, and `make check` runs it; declare it inCheck",
		}},
		{"a recipe line's prefixes", []docsEdit{
			replace("Makefile", "\t$(GO) vet ./...\n", "\t@+$(GO) vet ./...\n"),
		}, nil},
		{"a recipe line make goes on past", []docsEdit{
			replace("Makefile", "\t$(GO) vet ./...\n", "\t-$(GO) vet ./...\n"),
		}, []string{"Makefile:85: vet's recipe line starts with -, so make goes on when it fails"}},
		{".IGNORE", []docsEdit{
			replace("Makefile", "vet:\n", ".IGNORE: g00\nvet:\n"),
		}, []string{"Makefile: .IGNORE makes make ignore a failing command; drop it"}},
		{"a line the Makefile reader does not read", []docsEdit{
			replace("Makefile", "vet:\n", "include other.mk\nvet:\n"),
		}, []string{`Makefile:84: "include other.mk" is a directive or an assignment, which this reader does not read`}},
		{"a recipe that passes whatever it ran", []docsEdit{
			replace("Makefile", "\t$(GATES) g00\n", "\t$(GATES) g00 || true\n"),
		}, []string{"Makefile:6: g00's recipe line is one where `|| true` passes whatever came before, so a failing check passes"}},
	})
}

// What a push to main runs is what `make check` is held to, and CI runs a
// check through make, failing when it fails.
func TestParityHoldsCheckToWhatAPushRuns(t *testing.T) {
	const ci = ".github/workflows/ci.yml"
	notRun := []string{
		"`make check` runs g00 and ci.yml never does",
		"the g00 gate runs in check by the registry, and no job of ci.yml a push to main runs calls it",
	}
	checkParityCases(t, []parityCase{
		{"a step a pull request alone runs", []docsEdit{
			replace(ci, "      - name: g00\n", "      - name: g00\n        if: github.event_name == 'pull_request'\n"),
		}, notRun},
		{"a step whose failure passes", []docsEdit{
			replace(ci, "      - name: g00\n", "      - name: g00\n        continue-on-error: true\n"),
		}, notRun},
		{"a step a matrix filter runs", []docsEdit{
			replace(ci, "      - name: g00\n", "      - name: g00\n        if: matrix.os == 'ubuntu-latest'\n"),
		}, nil},
		{"a step that never runs", []docsEdit{
			replace(ci, "      - name: g00\n", "      - name: g00\n        if: false\n"),
		}, notRun},
		{"a step under a condition other than a matrix filter", []docsEdit{
			replace(ci, "      - name: g00\n", "      - name: g00\n        if: github.event_name != 'push'\n"),
		}, notRun},
		{"a CI line that passes whatever it ran", []docsEdit{
			replace(ci, "        run: make g00\n", "        run: make g00 || true\n"),
		}, []string{"ci.yml runs `make g00 || true`, where `|| true` passes whatever came before, so a failing check passes"}},
		{"a gate CI calls outside make", []docsEdit{
			replace(ci, "        run: make g00\n", "        run: go run ./scripts/gates g00\n"),
		}, []string{"ci.yml runs `go run ./scripts/gates g00`, a gate called outside make; call its make target, " +
			"so CI and `make check` run one recipe"}},
	})
}

func TestShellSwallowed(t *testing.T) {
	for line, want := range map[string]string{
		"make leaks":                "",
		"make leaks && make pins":   "",
		"make leaks || true":        "`|| true` passes whatever came before",
		"make leaks || :":           "`|| :` passes whatever came before",
		"make leaks || exit 0":      "`|| exit 0` passes whatever came before",
		"make leaks || exit 1":      "",
		"make leaks; true":          "`; true` ends it with success whatever came before",
		"make leaks; make pins":     "",
		"make leaks &":              "& runs a command in the background, where its failure is never seen",
		"! make leaks":              "! turns a failure into success",
		"set +e; make leaks":        "set +e lets the commands after it fail",
		"make leaks 2>&1 | tee log": "",
		"out=$(gofmt -l .); exit 1": "",
		"echo 'make x || true'":     "",
	} {
		if got := shellSwallowed(shellSplit(line)); got != want {
			t.Errorf("shellSwallowed(%q) = %q, want %q", line, got, want)
		}
	}
}

// A tool CI runs with go run is one `make check` runs at the same version.
func TestParityHoldsCIsOwnGoRunsToMakeCheck(t *testing.T) {
	const (
		ci   = ".github/workflows/ci.yml"
		tool = "honnef.co/go/tools/cmd/staticcheck"
	)
	ciRuns := func(version string) docsEdit {
		return replace(ci, "run: make cover\n", "run: |\n          make cover\n          # go run "+tool+"@v0.1.0\n"+
			"          go run "+tool+"@"+version+" ./...\n")
	}
	checkRuns := replace("Makefile", "cover: test\n\t@echo floors\n", "cover: test\n\t@echo floors\n\t$(GO) run "+tool+"@v0.6.1 ./...\n")
	checkParityCases(t, []parityCase{
		{"one make check runs, at its version", []docsEdit{ciRuns("v0.6.1"), checkRuns}, nil},
		{"one make check does not run, beside a commented one", []docsEdit{ciRuns("v0.6.1")}, []string{
			"ci.yml runs `go run " + tool + "@v0.6.1` and no target `make check` runs does; a contributor cannot reproduce it",
		}},
		{"one make check runs at another version", []docsEdit{ciRuns("v0.6.0"), checkRuns}, []string{
			"ci.yml runs `go run " + tool + "@v0.6.0` and no target `make check` runs does; a contributor cannot reproduce it",
		}},
		{"a package of this module, which has no version to hold", []docsEdit{
			replace(ci, "run: make cover\n", "run: |\n          make cover\n          go run ./cmd/tool\n"),
		}, nil},
	})
}

// A `go run` names its package after its build flags, on every side.
func TestParityReadsAGoRunPastItsFlags(t *testing.T) {
	const (
		ci   = ".github/workflows/ci.yml"
		tool = "honnef.co/go/tools/cmd/staticcheck@v0.6.1"
	)
	ciRuns := replace(ci, "run: make cover\n", "run: |\n          make cover\n          go run -mod=mod "+tool+" ./...\n")
	checkParityCases(t, []parityCase{
		{"the gates, run after build flags in make, CI and the release", []docsEdit{
			replace("Makefile", "GATES ?= $(GO) run ./scripts/gates", "GATES ?= $(GO) run -tags gates -trimpath ./scripts/gates"),
			replace(".goreleaser.yaml", "go run ./scripts/gates", "go run -trimpath ./scripts/gates"),
		}, nil},
		{"a flagged tool CI runs and make check does not", []docsEdit{ciRuns}, []string{
			"ci.yml runs `go run " + tool + "` and no target `make check` runs does; a contributor cannot reproduce it",
		}},
		{"a flagged tool both run", []docsEdit{
			ciRuns,
			replace("Makefile", "cover: test\n\t@echo floors\n", "cover: test\n\t@echo floors\n\t$(GO) run -modfile tools.mod "+tool+" ./...\n"),
		}, nil},
	})
}

// The tags each file needs: none where the go command sets them, and the
// smallest custom set otherwise.
func TestParityTreeTags(t *testing.T) {
	files := map[string]string{
		"a/live.go":         "//go:build live && unix\n\npackage a\n",
		"a/other.go":        "//go:build !windows\n\npackage a\n",
		"a/windows.go":      "//go:build windows\n\npackage a\n",
		"a/e2e.go":          "//go:build e2e && !experimental\n\npackage a\n",
		"a/race.go":         "//go:build evals || !race\n\npackage a\n",
		"a/arch.go":         "//go:build amd64.v2 && go1.22\n\npackage a\n",
		"a/exp.go":          "//go:build goexperiment.synctest\n\npackage a\n",
		"a/instrumented.go": "//go:build msan && asan\n\npackage a\n",
		"a/racy.go":         "//go:build race\n\npackage a\n",
		"a/mixed.go":        "//go:build (a && b) || c\n\npackage a\n",
		"a/live2.go":        "//go:build live\n\npackage a\n",
		"a/ignored.go":      "//go:build ignore\n\npackage a\n",
		"a/both.go":         "//go:build integration && slow\n\npackage a\n",
		"a/either.go":       "//go:build alpha || beta\n\npackage a\n",
		"a/never.go":        "//go:build stress && linux && windows\n\npackage a\n",
		"a/plain.go":        "package a\n",
		"a/notes.txt":       "//go:build prose\n",
		"a/testdata/x.go":   "//go:build fixture\n\npackage x\n",
	}
	needs, err := parityTreeTags(writeTree(t, files), slices.Sorted(maps.Keys(files)))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range needs {
		got = append(got, n.file+": "+n.String())
	}
	want := []string{
		"a/both.go: -tags=integration,slow",
		"a/e2e.go: -tags=e2e",
		"a/either.go: -tags=alpha or -tags=beta",
		"a/live.go: -tags=live",
		"a/mixed.go: -tags=c or -tags=a,b",
		"a/never.go: -tags=stress",
	}
	if !slices.Equal(got, want) {
		t.Errorf("parityTreeTags = %q, want %q", got, want)
	}
}

func TestParityVetPassesReadTheTagsFlagAnywhere(t *testing.T) {
	got := parityVetPasses([]string{
		"go vet -tags=live ./...",
		"go vet -v -tags a,b ./... && go test -tags=evals ./...",
		`go vet --tags="x" ./...; go vet ./...`,
		"go vet --tags y ./...",
		"make vet",
		"go test -vet=off -tags=live ./...",
	})
	want := [][]string{{"live"}, {"a", "b"}, {"x"}, {"y"}}
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("parityVetPasses = %q, want %q", got, want)
	}
}

func TestGoRunsInSkipsTheFlags(t *testing.T) {
	for line, want := range map[string][]goRun{
		"go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...": {
			{pkg: "golang.org/x/vuln/cmd/govulncheck@v1.8.0", args: []string{"./..."}}},
		"go run -mod=mod -modfile tools.mod -trimpath ./scripts/gates pins": {
			{pkg: "./scripts/gates", args: []string{"pins"}}},
		`CGO_ENABLED=0 go run "x.org/y@v1" && go run -C dir ./z a`: {
			{pkg: "x.org/y@v1", args: []string{}},
			{pkg: "./z", args: []string{"a"}}},
		`go run -ldflags "-s -w" ./scripts/gates docs`: {
			{pkg: "./scripts/gates", args: []string{"docs"}}},
		`go run -ldflags="-X a=b -s" tool.org/x@v1 x`: {
			{pkg: "tool.org/x@v1", args: []string{"x"}}},
		`go run -ldflags=-X\ a=b ./scripts/gates pins`: {
			{pkg: "./scripts/gates", args: []string{"pins"}}},
		`go run -ldflags '-X a=b c' ./scripts/gates pins`: {
			{pkg: "./scripts/gates", args: []string{"pins"}}},
		`go run -gcflags 'all=-N -l' ./cmd/x; echo done`: {
			{pkg: "./cmd/x", args: []string{}}},
		`$GOROOT/bin/go run x.org/y@main`: {
			{pkg: "x.org/y@main", args: []string{}}},
		"cargo run x":              nil,
		`echo "go run x.org/y@v1"`: nil,
		"echo go run":              nil,
		"go vet ./...":             nil,
	} {
		got := goRunsIn(line)
		if !slices.EqualFunc(got, want, func(a, b goRun) bool { return a.pkg == b.pkg && slices.Equal(a.args, b.args) }) {
			t.Errorf("goRunsIn(%q) = %q, want %q", line, got, want)
		}
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

func TestMakefileReadsAsMakeDoes(t *testing.T) {
	mk := parseMakefile(textLines(
		"A ?= one",
		"A ?= two",
		"B = x#comment",
		"B += y",
		"C += z",
		"export D := d",
		"override E = e",
		"F := first",
		"F = second",
		"check: a",
		"\techo one",
		"check: b a",
		"check:",
		"lint:",
		"\techo old",
		"lint:",
		"\techo new",
		"quiet:",
		"\t@-echo one",
		"\t+ echo two",
	))
	wantVars := map[string]string{"A": "one", "B": "x y", "C": "z", "D": "d", "E": "e", "F": "second"}
	if !maps.Equal(mk.vars, wantVars) {
		t.Errorf("vars = %q, want %q", mk.vars, wantVars)
	}
	if got := mk.targets["check"].prereqs; !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("check's prerequisites = %q, want [a b]: a target's rules merge them, each once", got)
	}
	if got := mk.recipe("check"); !slices.Equal(got, []string{"echo one"}) {
		t.Errorf("check's recipe = %q, want [echo one]: a rule with no recipe leaves the one before", got)
	}
	if got := mk.recipe("lint"); !slices.Equal(got, []string{"echo new"}) {
		t.Errorf("lint's recipe = %q, want [echo new]: the last recipe is the one that runs", got)
	}
	if got := mk.recipe("quiet"); !slices.Equal(got, []string{"echo one", "echo two"}) {
		t.Errorf("quiet's recipe = %q, want [echo one echo two]: @, - and + are not part of a command", got)
	}
}

// Outside a git checkout the tree's files cannot be listed, so the gate
// could not run, which exits 2 rather than a finding's 1.
func TestParityNeedsAGitCheckout(t *testing.T) {
	files, registry := parityFixture()
	root := writeTree(t, files)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	_, err := parityCheck(root, registry)
	if !errors.As(err, new(couldNotRun)) || !strings.HasPrefix(err.Error(), "could not run: git ls-files") {
		t.Errorf("parityCheck outside a checkout = %v, want a git ls-files failure it could not run past", err)
	}
}
