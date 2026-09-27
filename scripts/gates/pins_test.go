package main

import (
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
)

// pinsFixture is a repository every rule passes on. A test replaces one
// file to break one thing.
func pinsFixture() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.27.1\n\nrequire (\n\tgithub.com/modelcontextprotocol/go-sdk v1.8.0\n)\n",
		"Makefile": "GOLANGCI_LINT ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2\n" +
			"GOVULNCHECK ?= golang.org/x/vuln/cmd/govulncheck@v1.8.0\n" +
			"GOLICENSES ?= github.com/google/go-licenses@v1.6.0\n" +
			"GITLEAKS ?= github.com/zricethezav/gitleaks/v8@v8.30.1\n" +
			"ACTIONLINT ?= github.com/rhysd/actionlint/cmd/actionlint@v1.7.12\n" +
			"GORELEASER ?= github.com/goreleaser/goreleaser/v2@v2.18.2\n",
		"docs/architecture.md": "# x\n\n| Tool | Version |\n|---|---|\n| Go | 1.27.1 (`go.mod`) |\n" +
			"| MCP Go SDK | v1.8.0 |\n| golangci-lint | v2.13.2 |\n| goreleaser | v2.18.2 |\n" +
			"| cosign | v3.1.3 |\n| syft | v1.52.0 |\n| mcp-publisher | v1.8.1 |\n| gitleaks | v8.30.1 |\n" +
			"| govulncheck | v1.8.0 |\n| go-licenses | §17.3 |\n| actionlint | v1.7.12 |\n" +
			"| codeql-action | v4.38.1 |\n| mcpb manifest schema | v2.1.2 tag |\n\nAfter.\n",
		".github/workflows/ci.yml": "on: [push]\njobs:\n  test:\n    steps:\n" + pinsTestSteps +
			"      - name: test\n        run: make test\n",
		".github/workflows/codeql.yml": "on: [push]\njobs:\n  analyze:\n    steps:\n" + pinsTestSteps +
			"      - uses: github/codeql-action/init@" + pinsTestCQL + " # v4.38.1\n" +
			"      - uses: github/codeql-action/analyze@" + pinsTestCQL + " # v4.38.1\n",
		".github/workflows/release.yml": "on:\n  push:\n    tags: [\"v*\"]\nenv:\n  GORELEASER_VERSION: v2.18.2\n" +
			"jobs:\n  release:\n    steps:\n" + pinsTestSteps +
			"      - uses: sigstore/cosign-installer@" + pinsTestSHA + " # v4.1.2\n        with:\n          cosign-release: v3.1.3\n" +
			"      - uses: anchore/sbom-action/download-syft@" + pinsTestSHA + " # v0.24.2\n        with:\n          syft-version: v1.52.0\n" +
			"      - uses: goreleaser/goreleaser-action@" + pinsTestSHA + " # v7.2.3\n        with:\n" +
			"          version: ${{ env.GORELEASER_VERSION }}\n          args: release --clean\n",
		".github/workflows/publish-mcp.yml": "on: [workflow_dispatch]\njobs:\n  publish:\n    steps:\n" + pinsTestSteps +
			"      - uses: sigstore/cosign-installer@" + pinsTestSHA + " # v4.1.2\n        with:\n          cosign-release: v3.1.3\n" +
			"      - name: publish\n        env:\n          PUBLISHER_VERSION: v1.8.1\n        run: ./mcp-publisher publish\n",
	}
}

func TestPinsPassesOnAPinnedRepository(t *testing.T) {
	r, err := pinsCheck(writeTree(t, pinsFixture()))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.problems) > 0 {
		t.Fatalf("problems on a clean fixture:\n%s", strings.Join(r.problems, "\n"))
	}
	if r.installers != 4 || r.docRows != 13 || len(r.tools) != len(pinsTools) {
		t.Errorf("read %d installers, %d doc rows, %d tools; want 4, 13, %d",
			r.installers, r.docRows, len(r.tools), len(pinsTools))
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
		{"a pin that differs from §5a", "docs/architecture.md", "| cosign | v3.1.3 |", "| cosign | v3.1.4 |",
			"and §5a says v3.1.4"},
		{"a §5a row the gate does not know", "docs/architecture.md", "| Go |", "| Rust | 1.0.0 |\n| Go |",
			`names "Rust"`},
		{"a tool pinned nowhere", ".github/workflows/publish-mcp.yml", "PUBLISHER_VERSION: v1.8.1", "X: y",
			"mcp-publisher is pinned nowhere"},
		{"an unknown version env", ".github/workflows/ci.yml", "        run: make test\n",
			"        env:\n          THING_VERSION: v1.0.0\n        run: make test\n", "THING_VERSION"},
		{"a floor on the pin table", "docs/architecture.md", "| govulncheck | v1.8.0 |\n| go-licenses | §17.3 |\n" +
			"| actionlint | v1.7.12 |\n| codeql-action | v4.38.1 |\n", "", "read 9 rows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := pinsFixture()
			if !strings.Contains(files[tc.file], tc.old) {
				t.Fatalf("the fixture's %s has no %q to break", tc.file, tc.old)
			}
			files[tc.file] = strings.Replace(files[tc.file], tc.old, tc.new, 1)
			r, err := pinsCheck(writeTree(t, files))
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

func TestPinsNamesAMissingWorkflow(t *testing.T) {
	files := pinsFixture()
	delete(files, ".github/workflows/codeql.yml")
	r, err := pinsCheck(writeTree(t, files))
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
