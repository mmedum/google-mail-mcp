package main

import (
	"slices"
	"strings"
	"testing"
)

const (
	pinsTestSHA   = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	pinsTestCQL   = "1c5b675653bb5c22dbe9b12b556ec555138e09fd"
	pinsTestSteps = `      - uses: actions/checkout@` + pinsTestSHA + ` # v7.0.1
        with:
          persist-credentials: false
      - uses: actions/setup-go@` + pinsTestSHA + ` # v7.0.0
        with:
          go-version-file: go.mod
`
	pinsTestShell = "defaults:\n  run:\n    shell: bash\n"
)

// pinsFixture is a repository every rule passes on. A test replaces one
// file to break one thing.
func pinsFixture() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.27.1\n\nrequire (\n\tgithub.com/modelcontextprotocol/go-sdk v1.8.0\n)\n",
		"Makefile": "GO ?= go\n" +
			"GOLANGCI_LINT ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2\n" +
			"GOVULNCHECK ?= golang.org/x/vuln/cmd/govulncheck@v1.8.0\n" +
			"GOLICENSES ?= github.com/google/go-licenses@v1.6.0\n" +
			"GITLEAKS ?= github.com/zricethezav/gitleaks/v8@v8.30.1\n" +
			"ACTIONLINT ?= github.com/rhysd/actionlint/cmd/actionlint@v1.7.12\n" +
			"GORELEASER ?= github.com/goreleaser/goreleaser/v2@v2.18.2\n" +
			"\nlint:\n\t$(GO) run $(GOLANGCI_LINT) run\n" +
			"\nvuln:\n\t$(GO) run $(GOVULNCHECK) ./...\n" +
			"\nlicenses:\n\t$(GO) run $(GOLICENSES) check ./...\n" +
			"\nsecrets:\n\t$(GO) run $(GITLEAKS) dir .\n\t$(GO) run $(GITLEAKS) git .\n" +
			"\nactionlint:\n\t$(GO) run $(ACTIONLINT)\n" +
			"\ngoreleaser-check:\n\t$(GO) run $(GORELEASER) check\n",
		".github/workflows/ci.yml": "on: [push]\n" + pinsTestShell + "jobs:\n  test:\n    steps:\n" + pinsTestSteps +
			"      - name: test\n        run: make test\n",
		".github/workflows/codeql.yml": "on: [push]\n" + pinsTestShell + "jobs:\n  analyze:\n    steps:\n" + pinsTestSteps +
			"      - uses: github/codeql-action/init@" + pinsTestCQL + " # v4.38.1\n" +
			"      - uses: github/codeql-action/analyze@" + pinsTestCQL + " # v4.38.1\n",
		".github/workflows/release.yml": "on:\n  push:\n    tags: [\"v*\"]\n" + pinsTestShell + "env:\n  GORELEASER_VERSION: v2.18.2\n" +
			"jobs:\n  release:\n    steps:\n" + pinsTestSteps +
			"      - uses: sigstore/cosign-installer@" + pinsTestSHA + " # v4.1.2\n        with:\n          cosign-release: v3.1.3\n" +
			"      - uses: anchore/sbom-action/download-syft@" + pinsTestSHA + " # v0.24.2\n        with:\n          syft-version: v1.52.0\n" +
			"      - uses: goreleaser/goreleaser-action@" + pinsTestSHA + " # v7.2.3\n        with:\n" +
			"          version: ${{ env.GORELEASER_VERSION }}\n          args: release --clean\n",
		".github/workflows/publish-mcp.yml": "on: [workflow_dispatch]\n" + pinsTestShell + "jobs:\n  publish:\n    steps:\n" + pinsTestSteps +
			"      - uses: sigstore/cosign-installer@" + pinsTestSHA + " # v4.1.2\n        with:\n          cosign-release: v3.1.3\n" +
			"      - name: publish\n        env:\n          PUBLISHER_VERSION: v1.8.1\n        run: ./mcp-publisher publish\n",
		".goreleaser.yaml":      "version: 2\nbefore:\n  hooks:\n    - go mod download\n",
		".githooks/pre-commit":  "#!/bin/sh\nexec go run ./scripts/gates precommit\n",
		"scripts/gates/main.go": "package main\n",
	}
}

// pinsFixturePlaces is how many pins the fixture writes: go.mod two, the
// Makefile six, ci.yml one, codeql.yml three, release.yml five and
// publish-mcp.yml three, a setup-go step's go.mod among each workflow's.
const pinsFixturePlaces = 20

// pinsFixtureInstallers is how many installing steps the fixture has: a
// setup-go in each of the four workflows, cosign in release.yml and
// publish-mcp.yml, and syft and goreleaser in release.yml.
const pinsFixtureInstallers = 8

func TestPinsPassesOnAPinnedRepository(t *testing.T) {
	r, err := pinsCheck(writeTree(t, pinsFixture()), pinsFixturePlaces)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.problems) > 0 {
		t.Fatalf("problems on a clean fixture:\n%s", strings.Join(r.problems, "\n"))
	}
	if r.installers != pinsFixtureInstallers || r.places != pinsFixturePlaces || len(r.tools) != len(pinsTools) {
		t.Errorf("read %d installers, %d places, %d tools; want %d, %d, %d",
			r.installers, r.places, len(r.tools), pinsFixtureInstallers, pinsFixturePlaces, len(pinsTools))
	}
}

// Every pin a reader finds counts toward the floor, so one pin less is
// one under it: a reader that stops seeing its file cannot pass.
func TestPinsHoldsTheReadersToAFloor(t *testing.T) {
	r, err := pinsCheck(writeTree(t, pinsFixture()), pinsFixturePlaces+1)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"read 20 pins across the workflows, Makefile and go.mod, want at least 21; a reader is not seeing its file"}
	if !slices.Equal(r.problems, want) {
		t.Errorf("problems %q, want %q", r.problems, want)
	}
}

// Each case breaks one thing and names the sentence that must say so.
func TestPinsRefuses(t *testing.T) {
	cases := []struct {
		name, file, old, new, want string
	}{
		{"a tag instead of a SHA", ".github/workflows/ci.yml", "checkout@" + pinsTestSHA, "checkout@v7",
			`actions/checkout is pinned to "v7"`},
		{"a SHA with no comment", ".github/workflows/ci.yml", pinsTestSHA + " # v7.0.1", pinsTestSHA,
			"with no comment saying"},
		{"an installer that names no tool version", ".github/workflows/release.yml", "          cosign-release: v3.1.3\n", "",
			"does not set cosign-release"},
		{"an unknown action", ".github/workflows/ci.yml", "      - name: test\n",
			"      - uses: someone/thing@" + pinsTestSHA + " # v1.0.0\n      - name: test\n", "someone/thing is not classified"},
		{"a range", ".github/workflows/release.yml", "syft-version: v1.52.0", "syft-version: ~v1.52",
			"not one exact version"},
		{"latest", "Makefile", "actionlint@v1.7.12", "actionlint@latest", "pinned to @latest"},
		{"an env reference nobody set", ".github/workflows/release.yml", "  GORELEASER_VERSION: v2.18.2\n", "  OTHER: x\n",
			"${{ env.GORELEASER_VERSION }}"},
		{"the rehearsal and the release disagree", "Makefile", "goreleaser/v2@v2.18.2", "goreleaser/v2@v2.18.1",
			"goreleaser is"},
		{"gitleaks differs between the hook's source and CI", ".github/workflows/ci.yml", "        run: make test\n",
			"        run: go run github.com/zricethezav/gitleaks/v8@v8.29.0 dir .\n", "gitleaks is"},
		{"cosign differs between the release and the publish", ".github/workflows/publish-mcp.yml",
			"cosign-release: v3.1.3", "cosign-release: v3.1.4", "cosign is v3.1.4 at"},
		{"a Go version that is not one exact version", "go.mod", "go 1.27.1", "go 1.27",
			`go is pinned to "1.27", which is not one exact version`},
		{"a tool pinned nowhere", ".github/workflows/publish-mcp.yml", "PUBLISHER_VERSION: v1.8.1", "X: y",
			"mcp-publisher is pinned nowhere"},
		{"an unknown version env", ".github/workflows/ci.yml", "        run: make test\n",
			"        env:\n          THING_VERSION: v1.0.0\n        run: make test\n", "THING_VERSION"},
		{"a uses: on the next line, by tag", ".github/workflows/ci.yml", "      - name: test\n",
			"      - name: cache\n        uses:\n          actions/checkout@v7\n      - name: test\n", `actions/checkout is pinned to "v7"`},
		{"a step in a flow mapping, by tag", ".github/workflows/ci.yml", "      - name: test\n",
			"      - {uses: actions/checkout@v7}\n      - name: test\n", `actions/checkout is pinned to "v7"`},
		{"a flow mapping's SHA with no comment", ".github/workflows/ci.yml", "      - name: test\n",
			"      - {uses: actions/checkout@" + pinsTestSHA + "}\n      - name: test\n", "with no comment saying"},
		{"an env version name in another case", ".github/workflows/ci.yml", "        run: make test\n",
			"        env:\n          Gitleaks_Version: latest\n        run: make test\n", `gitleaks is pinned to "latest"`},
		{"a version input in another case", ".github/workflows/release.yml", "syft-version: v1.52.0", "Syft-Version: latest",
			`Syft-Version is "latest", which is not one exact version`},
		{"one input written twice, in two cases", ".github/workflows/release.yml", "          syft-version: v1.52.0\n",
			"          syft-version: v1.52.0\n          SYFT-VERSION: v1.52.0\n", "are one input to the runner"},
		{"a local reusable call given latest", ".github/workflows/ci.yml", "jobs:\n  test:\n",
			"jobs:\n  call:\n    uses: ./.github/workflows/publish-mcp.yml\n    with:\n      tool-version: latest\n  test:\n",
			`with tool-version "latest", which is not one exact version`},
		{"a remote reusable call given latest", ".github/workflows/ci.yml", "jobs:\n  test:\n",
			"jobs:\n  call:\n    uses: some-org/some-repo/.github/workflows/x.yml@" + pinsTestSHA + " # v1.0.0\n" +
				"    with:\n      tool-version: latest\n  test:\n",
			`with tool-version "latest", which is not one exact version`},
		{"go-version beside go-version-file", ".github/workflows/ci.yml", "          go-version-file: go.mod\n",
			"          go-version-file: go.mod\n          go-version: 1.20.1\n", "sets go-version, which setup-go reads before"},
		{"a workflow with no shell", ".github/workflows/ci.yml", pinsTestShell, "",
			"no workflow-level `defaults: run: shell: bash`"},
		{"a step in another shell", ".github/workflows/ci.yml", "        run: make test\n",
			"        shell: pwsh\n        run: make test\n", "has a step in pwsh"},
		{"an env value naming a tool at another version", ".github/workflows/ci.yml", "        run: make test\n",
			"        env:\n          GOVULNCHECK: golang.org/x/vuln/cmd/govulncheck@v1.1.0\n        run: make test\n",
			"govulncheck is v1.1.0 at .github/workflows/ci.yml"},
		{"a recipe that runs a known tool from the PATH", "Makefile", "\t$(GO) run $(GOVULNCHECK) ./...\n",
			"\t$(GO) run $(GOVULNCHECK) ./...\n\tgolangci-lint run\n", "names golangci-lint as a word of its own"},
		{"a recipe that downloads the latest release", "Makefile", "\t$(GO) run $(GOVULNCHECK) ./...\n",
			"\t$(GO) run $(GOVULNCHECK) ./...\n\tcurl -fsSLO https://github.com/x/y/releases/latest/download/z\n",
			"releases/latest/download installs whatever is newest"},
		{"a substitution that runs a tool at another version", "Makefile", "\t$(GO) run $(GOVULNCHECK) ./...\n",
			"\t$(GO) run $(GOVULNCHECK) ./...\n\techo $$(go run golang.org/x/vuln/cmd/govulncheck@v1.1.0 -version)\n",
			"govulncheck is"},
		{"a Makefile line the reader does not read", "Makefile", "GO ?= go\n", "GO ?= go\ninclude other.mk\n",
			"is a directive or an assignment"},
		{"a goreleaser hook that runs an unknown tool", ".goreleaser.yaml", "    - go mod download\n",
			"    - go mod download\n    - go run example.org/tool@v1.0.0\n", "names a tool pinsTools does not know"},
		{"a goreleaser hook whose else branch runs another tool", ".goreleaser.yaml", "    - go mod download\n",
			"    - go mod download\n    - '{{ if .IsSnapshot }}true{{ else }}go run example.org/tool@main{{ end }}'\n",
			"a template control action"},
		{"a goreleaser hook that runs a tool at another version through bash -c", ".goreleaser.yaml",
			"    - go mod download\n", "    - go mod download\n    - bash -c \"go run golang.org/x/vuln/cmd/govulncheck@v1.1.0 ./...\"\n",
			"govulncheck is"},
		{"a goreleaser merge key", ".goreleaser.yaml", "before:\n", "before:\n  <<: {skip: true}\n", "a merge key"},
		{"goreleaser at a version the reader was not checked against", ".github/workflows/release.yml",
			"GORELEASER_VERSION: v2.18.2", "GORELEASER_VERSION: v2.18.3", "reader is checked against v2.18.2's source"},
		{"a Go literal under scripts at another version", "scripts/gates/main.go", "package main\n",
			"package main\n\nconst x = \"golang.org/x/vuln/cmd/govulncheck@v1.1.0\"\n", "govulncheck is"},
		{"the hook runs a known tool from the PATH", ".githooks/pre-commit", "exec go run",
			"gitleaks git --staged\nexec go run", "names gitleaks as a word of its own"},
		// A shape the reader does not parse hides the file's steps, and
		// with them every pin in it.
		{"a workflow whose pins the reader stops seeing", ".github/workflows/publish-mcp.yml",
			"  publish:\n    steps:\n", "  publish:\n    stages:\n",
			".github/workflows/publish-mcp.yml: no pins read, though it pins a tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := pinsFixture()
			applyEdits(t, files, replace(tc.file, tc.old, tc.new))
			r, err := pinsCheck(writeTree(t, files), pinsFixturePlaces)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(r.problems, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("want %q among the problems, got:\n%s", tc.want, joined)
			}
		})
	}
}

// Each row edits the fixture and wants exactly these problems, or none.
// The floor on places is off: a row that drops a pin is about something
// else.
func TestPinsNamesEachProblem(t *testing.T) {
	const (
		ci      = ".github/workflows/ci.yml"
		release = ".github/workflows/release.yml"
		vuln    = "\t$(GO) run $(GOVULNCHECK) ./...\n"
		goStep  = "          go-version-file: go.mod\n"
		digest  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		pathRun = "Makefile: GOVULNCHECK pins golang.org/x/vuln/cmd/govulncheck@v1.8.0 and no recipe runs it with go run, " +
			"so the tool runs from the PATH"
	)
	gorelease := "      - uses: goreleaser/goreleaser-action@" + pinsTestSHA + " # v7.2.3\n        with:\n" +
		"          version: ${{ env.GORELEASER_VERSION }}\n          args: release --clean\n"
	for _, tc := range []struct {
		name  string
		edits []docsEdit
		want  []string
	}{
		{name: "a container image by its digest, quoted", edits: []docsEdit{
			replace(ci, "      - name: test\n", "      - uses: \"docker://alpine@sha256:"+digest+"\"\n      - name: test\n"),
		}},
		{name: "a container image by tag, quoted, and by name alone", edits: []docsEdit{
			replace(ci, "      - name: test\n", "      - uses: \"docker://alpine:3.20\"\n      - uses: docker://alpine\n      - name: test\n"),
		}, want: []string{
			".github/workflows/ci.yml: job test: docker://alpine:3.20 is a container image with no sha256 digest, so the image it runs can change",
			".github/workflows/ci.yml: job test: docker://alpine is a container image with no sha256 digest, so the image it runs can change",
		}},
		{name: "a container image a line scan cannot see: on the next line, and in a flow mapping", edits: []docsEdit{
			replace(ci, "      - name: test\n", "      - name: unpinned image\n        uses:\n          docker://ghcr.io/example/img:main\n"+
				"      - {uses: \"docker://alpine:3.20\"}\n      - name: test\n"),
		}, want: []string{
			".github/workflows/ci.yml: job test: docker://ghcr.io/example/img:main is a container image with no sha256 digest, " +
				"so the image it runs can change",
			".github/workflows/ci.yml: job test: docker://alpine:3.20 is a container image with no sha256 digest, so the image it runs can change",
		}},
		{name: "a container image on the next line, by its digest", edits: []docsEdit{
			replace(ci, "      - name: test\n", "      - uses:\n          docker://alpine@sha256:"+digest+"\n      - name: test\n"),
		}},
		{name: "an action by SHA in single quotes", edits: []docsEdit{
			replace(ci, "      - uses: actions/checkout@"+pinsTestSHA+" # v7.0.1\n", "      - uses: 'actions/checkout@"+pinsTestSHA+"' # v7.0.1\n"),
		}},
		{name: "a job's container and service by digest", edits: []docsEdit{
			replace(ci, "  test:\n    steps:\n", "  test:\n    container: alpine@sha256:"+digest+"\n    services:\n      db:\n"+
				"        image: postgres@sha256:"+digest+"\n    steps:\n"),
		}},
		{name: "a job's container by tag, as a string and under image:, and a service by tag", edits: []docsEdit{
			replace(ci, "  test:\n    steps:\n", "  test:\n    container: node:18\n    services:\n      db:\n        image: postgres:16\n    steps:\n"),
			replace(".github/workflows/codeql.yml", "  analyze:\n    steps:\n", "  analyze:\n    container:\n      image: alpine:3.20\n    steps:\n"),
		}, want: []string{
			".github/workflows/ci.yml: job test runs the container from node:18, which has no sha256 digest, so the image it runs can change",
			".github/workflows/ci.yml: job test runs the service db from postgres:16, which has no sha256 digest, so the image it runs can change",
			".github/workflows/codeql.yml: job analyze runs the container from alpine:3.20, which has no sha256 digest, " +
				"so the image it runs can change",
		}},
		{name: "a job's container and its image by an alias", edits: []docsEdit{
			replace(ci, "jobs:\n  test:\n    steps:\n", "env:\n  IMAGE: &img node:18\njobs:\n  test:\n    container: *img\n    steps:\n"),
			replace(".github/workflows/codeql.yml", "  analyze:\n    steps:\n", "  analyze:\n    container:\n      image: &tag alpine:3.20\n"+
				"    services:\n      db:\n        image: *tag\n    steps:\n"),
		}, want: []string{
			".github/workflows/ci.yml: job test runs the container from node:18, which has no sha256 digest, so the image it runs can change",
			".github/workflows/codeql.yml: job analyze runs the container from alpine:3.20, which has no sha256 digest, " +
				"so the image it runs can change",
			".github/workflows/codeql.yml: job analyze runs the service db from alpine:3.20, which has no sha256 digest, " +
				"so the image it runs can change",
		}},
		{name: "a Makefile pin whose version is another variable", edits: []docsEdit{
			replace("Makefile", "GORELEASER ?= github.com/goreleaser/goreleaser/v2@v2.18.2\n",
				"GORELEASER_VERSION ?= v2.18.2\nGORELEASER ?= github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)\n"),
		}},
		{name: "a Makefile pin whose version is another variable, at another version", edits: []docsEdit{
			replace("Makefile", "GORELEASER ?= github.com/goreleaser/goreleaser/v2@v2.18.2\n",
				"GORELEASER_VERSION ?= v2.18.1\nGORELEASER ?= github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)\n"),
		}, want: []string{"goreleaser is v2.18.2 at .github/workflows/release.yml: goreleaser/goreleaser-action and " +
			"v2.18.1 at Makefile: GORELEASER: one tool, one version"}},
		{name: "setup-go with no go-version-file", edits: []docsEdit{replace(ci, goStep, "")}, want: []string{
			".github/workflows/ci.yml: actions/setup-go is pinned by SHA and does not set go-version-file, " +
				"so the tool it installs is whatever is current that morning",
		}},
		{name: "an exact toolchain over a go line that floats", edits: []docsEdit{
			replace("go.mod", "go 1.27.1\n", "go 1.27\n\ntoolchain go1.27.1\n"),
		}},
		{name: "a toolchain that floats over an exact go line", edits: []docsEdit{
			replace("go.mod", "go 1.27.1\n", "go 1.27.1\n\ntoolchain go1.27\n"),
		}, want: []string{
			`.github/workflows/ci.yml: actions/setup-go: go is pinned to "1.27", which is not one exact version`,
			`.github/workflows/codeql.yml: actions/setup-go: go is pinned to "1.27", which is not one exact version`,
			`.github/workflows/publish-mcp.yml: actions/setup-go: go is pinned to "1.27", which is not one exact version`,
			`.github/workflows/release.yml: actions/setup-go: go is pinned to "1.27", which is not one exact version`,
			`go.mod: go: go is pinned to "1.27", which is not one exact version`,
		}},
		{name: "a go.mod that names no Go version", edits: []docsEdit{replace("go.mod", "go 1.27.1\n", "")}, want: []string{
			`.github/workflows/ci.yml: actions/setup-go sets go-version-file to "go.mod", which names no Go version`,
			`.github/workflows/codeql.yml: actions/setup-go sets go-version-file to "go.mod", which names no Go version`,
			`.github/workflows/publish-mcp.yml: actions/setup-go sets go-version-file to "go.mod", which names no Go version`,
			`.github/workflows/release.yml: actions/setup-go sets go-version-file to "go.mod", which names no Go version`,
			`go.mod: go: go is pinned to "", which is not one exact version`,
		}},
		{name: "setup-go reading a file that is not a go.mod", edits: []docsEdit{
			replace(ci, goStep, "          go-version-file: .go-version\n"),
		}, want: []string{
			`.github/workflows/ci.yml: actions/setup-go sets go-version-file to ".go-version", which is not the repository's go.mod`,
		}},
		{name: "setup-go reading another go.mod", edits: []docsEdit{
			replace(ci, goStep, "          go-version-file: tools/go.mod\n"),
			add("tools/go.mod", "module example.com/tools\n\ngo 1.26.0\n"),
		}, want: []string{
			`.github/workflows/ci.yml: actions/setup-go sets go-version-file to "tools/go.mod", which is not the repository's go.mod`,
		}},
		{name: "a tool the Makefile runs from the PATH", edits: []docsEdit{
			replace("Makefile", vuln, "\tgovulncheck ./...\n"),
		}, want: []string{
			"Makefile:13: names govulncheck as a word of its own, which runs it from the PATH at whatever " +
				"version is installed there; run it with $(GO) run and the variable that pins it",
			pathRun,
		}},
		{name: "a tool the Makefile runs after build flags", edits: []docsEdit{
			replace("Makefile", vuln, "\t$(GO) run -modfile=tools.mod -mod mod $(GOVULNCHECK) ./...\n"),
		}},
		{name: "a package of this module, run by the Makefile and by CI", edits: []docsEdit{
			replace("Makefile", vuln, vuln+"\ndocs:\n\t$(GO) run ./scripts/gates docs\n"),
			replace(ci, "        run: make test\n", "        run: go run ./scripts/gates docs\n"),
		}},
		{name: "a tool CI runs through the go command's full path", edits: []docsEdit{
			replace(ci, "        run: make test\n", "        run: $GOROOT/bin/go run golang.org/x/vuln/cmd/govulncheck@main ./...\n"),
		}, want: []string{
			`.github/workflows/ci.yml: go run golang.org/x/vuln/cmd/govulncheck: govulncheck is pinned to "main", which is not one exact version`,
			"govulncheck is main at .github/workflows/ci.yml: go run golang.org/x/vuln/cmd/govulncheck and v1.8.0 at " +
				"Makefile: GOVULNCHECK: one tool, one version",
		}},
		{name: "a tool CI runs after a build flag, at another version", edits: []docsEdit{
			replace(ci, "        run: make test\n", "        run: go run -mod=mod github.com/zricethezav/gitleaks/v8@v8.29.0 dir .\n"),
		}, want: []string{
			"gitleaks is v8.29.0 at .github/workflows/ci.yml: go run github.com/zricethezav/gitleaks/v8 and v8.30.1 at " +
				"Makefile: GITLEAKS: one tool, one version",
		}},
		{name: "a recipe that runs a tool at latest", edits: []docsEdit{
			replace("Makefile", vuln, "\t$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...\n"),
		}, want: []string{
			`Makefile: vuln: govulncheck is pinned to "latest", which is not one exact version`,
			pathRun,
			"govulncheck is v1.8.0 at Makefile: GOVULNCHECK and latest at Makefile: vuln: one tool, one version",
		}},
		{name: "a variable at latest that a recipe runs, reported once", edits: []docsEdit{
			replace("Makefile", "govulncheck@v1.8.0", "govulncheck@latest"),
		}, want: []string{
			"Makefile: GOVULNCHECK is pinned to @latest",
			"govulncheck is pinned nowhere: a tool the table names that no file pins is either gone or unread",
		}},
		{name: "a recipe line continued onto the next, at latest", edits: []docsEdit{
			replace("Makefile", vuln, "\t$(GO) run \\\n\t\tgolang.org/x/vuln/cmd/govulncheck@latest ./...\n"),
		}, want: []string{
			`Makefile: vuln: govulncheck is pinned to "latest", which is not one exact version`,
			pathRun,
			"govulncheck is v1.8.0 at Makefile: GOVULNCHECK and latest at Makefile: vuln: one tool, one version",
		}},
		{name: "a recipe that runs a tool the table does not know", edits: []docsEdit{
			replace("Makefile", vuln, vuln+"\t$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.6.1 ./...\n"),
		}, want: []string{
			"Makefile: vuln runs go run honnef.co/go/tools/cmd/staticcheck@v0.6.1, a tool pinsTools does not know",
		}},
		{name: "a Makefile pin that is not one exact version", edits: []docsEdit{
			replace("Makefile", "govulncheck@v1.8.0", "govulncheck@v1.8"),
		}, want: []string{`Makefile: GOVULNCHECK: govulncheck is pinned to "v1.8", which is not one exact version`}},
		// One installer used in every job meets a count of steps.
		{name: "an installer no step uses, with another used twice", edits: []docsEdit{
			replace(release, gorelease, ""),
			replace(ci, "      - name: test\n", "      - uses: sigstore/cosign-installer@"+pinsTestSHA+" # v4.1.2\n"+
				"        with:\n          cosign-release: v3.1.3\n      - name: test\n"),
		}, want: []string{
			"no workflow step installs goreleaser through goreleaser/goreleaser-action; the reader is not seeing " +
				"the step, or the table names an installer nothing uses",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := pinsFixture()
			applyEdits(t, files, tc.edits...)
			r, err := pinsCheck(writeTree(t, files), 1)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(r.problems, tc.want) {
				t.Errorf("problems\n got %q\nwant %q", r.problems, tc.want)
			}
		})
	}
}

func TestPinsNamesAMissingWorkflow(t *testing.T) {
	files := pinsFixture()
	delete(files, ".github/workflows/codeql.yml")
	r, err := pinsCheck(writeTree(t, files), pinsFixturePlaces)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.problems, "\n"), "codeql.yml does not exist") {
		t.Errorf("a missing workflow was not named: %v", r.problems)
	}
}

func TestWorkflowResolveEnv(t *testing.T) {
	w := workflow{Env: map[string]string{"V": "v1.2.3"}}
	for in, want := range map[string]string{
		"${{ env.V }}": "v1.2.3", "${{env.V}}": "v1.2.3", "${{ env.W }}": "${{ env.W }}", "v9.9.9": "v9.9.9",
	} {
		if got := w.resolveEnv(in); got != want {
			t.Errorf("resolveEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

// The runner reads input names in upper case, so an installer's version
// input written in another case is the input.
func TestPinsReadsAnInputInAnyCase(t *testing.T) {
	files := pinsFixture()
	applyEdits(t, files, replace(".github/workflows/release.yml", "cosign-release: v3.1.3", "Cosign-Release: v3.1.3"))
	r, err := pinsCheck(writeTree(t, files), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.problems) > 0 {
		t.Errorf("problems:\n%s", strings.Join(r.problems, "\n"))
	}
}

// A reusable workflow of this repository may be given one context's value,
// which it holds where it uses it.
func TestPinsPassesALocalCallsContext(t *testing.T) {
	files := pinsFixture()
	applyEdits(t, files, replace(".github/workflows/ci.yml", "jobs:\n  test:\n",
		"jobs:\n  call:\n    uses: ./.github/workflows/publish-mcp.yml\n    with:\n      tool-version: ${{ inputs.v }}\n  test:\n"))
	r, err := pinsCheck(writeTree(t, files), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.problems) > 0 {
		t.Errorf("problems:\n%s", strings.Join(r.problems, "\n"))
	}
}

// A line break YAML parsers do not all read alike is refused before the
// file is read, wherever it sits.
func TestReadWorkflowRefusesAStrayLineBreak(t *testing.T) {
	for name, brk := range map[string]string{"a lone CR": "\r", "NEL": "\u0085", "LS": "\u2028", "PS": "\u2029"} {
		t.Run(name, func(t *testing.T) {
			files := pinsFixture()
			applyEdits(t, files, replace(".github/workflows/ci.yml", "      - name: test\n",
				"      - name: test"+brk+"      - uses: evil-org/x@main\n"))
			_, err := pinsCheck(writeTree(t, files), 1)
			if err == nil || !strings.Contains(err.Error(), ".github/workflows/ci.yml:") {
				t.Errorf("err = %v, want the line named", err)
			}
		})
	}
}
