package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const releaseTestConfig = `version: 2
project_name: google-mail-mcp
before:
  hooks:
    - go mod download
builds:
  - id: google-mail-mcp
    binary: google-mail-mcp
    goos: [linux, darwin, windows]
    goarch: [amd64, arm64]
    flags: [-trimpath]
    mod_timestamp: "{{ .CommitTimestamp }}"
    ldflags:
      - -s -w -X example.com/internal/version.Version={{ .Version }}
universal_binaries:
  - id: google-mail-mcp-universal
    ids: [google-mail-mcp]
    replace: false
    hooks:
      post: go run ./scripts/gates mcpb-pack dist {{ .Version }} dist/google-mail-mcp_{{ .Version }}.mcpb
archives:
  - id: archive
    ids: [google-mail-mcp]
checksum:
  name_template: checksums.txt
  extra_files:
    - glob: ./dist/*.mcpb
sboms:
  - artifacts: archive
signs:
  - cmd: cosign
    signature: "${artifact}.bundle"
    args: [sign-blob, "--bundle=${signature}", "--yes", "${artifact}"]
    artifacts: checksum
release:
  draft: false
  extra_files:
    - glob: ./dist/*.mcpb
`

const releaseTestWorkflow = `on:
  push:
    tags: ["v*.*.*"]
jobs:
  goreleaser:
    steps:
      - name: Test
        run: go test ./...
      - name: notes
        run: go run ./scripts/gates release-notes "${REF}" > "${RUNNER_TEMP}/notes.md"
      - uses: sigstore/cosign-installer@x
        with:
          cosign-release: v3.1.3
      - uses: anchore/sbom-action/download-syft@x
        with:
          syft-version: v1.52.0
      - uses: goreleaser/goreleaser-action@x
        with:
          args: build --single-target --snapshot --clean --output a
      - uses: goreleaser/goreleaser-action@x
        with:
          args: build --single-target --snapshot --clean --output b
      - uses: goreleaser/goreleaser-action@x
        with:
          args: release --clean --release-notes=notes.md
      - uses: actions/attest-build-provenance@x
        with:
          subject-path: "dist/*.tar.gz,dist/*.zip,dist/*.mcpb,dist/checksums.txt"
`

const releaseTestOut = "dist/google-mail-mcp_$(VERSION).mcpb"

func releaseTestStaged() []mcpbStaged {
	return []mcpbStaged{
		{path: "server/m-darwin", glob: "*_darwin_all/google-mail-mcp", platform: "darwin", goos: "darwin", goarch: "all"},
		{path: "server/m.exe", glob: "*_windows_amd64*/google-mail-mcp.exe", platform: "win32", goos: "windows", goarch: "amd64"},
		{path: "server/launch-linux.sh", platform: "linux"},
		{path: "server/m-linux-amd64", glob: "*_linux_amd64*/google-mail-mcp", goos: "linux", goarch: "amd64"},
		{path: "server/m-linux-arm64", glob: "*_linux_arm64*/google-mail-mcp", goos: "linux", goarch: "arm64"},
	}
}

func releaseTestRun(t *testing.T, config, flowText string, staged []mcpbStaged, out string) []string {
	t.Helper()
	var cfg releaseConfig
	if err := yaml.Unmarshal([]byte(config), &cfg); err != nil {
		t.Fatal(err)
	}
	var flow workflow
	if err := yaml.Unmarshal([]byte(flowText), &flow); err != nil {
		t.Fatal(err)
	}
	flow.path = "release.yml"
	problems, _ := releaseValidate(cfg, staged, out, flow)
	return problems
}

func TestReleasePassesOnAWiredRelease(t *testing.T) {
	if p := releaseTestRun(t, releaseTestConfig, releaseTestWorkflow, releaseTestStaged(), releaseTestOut); len(p) > 0 {
		t.Fatalf("problems on a wired fixture:\n%s", strings.Join(p, "\n"))
	}
}

func TestReleaseRefuses(t *testing.T) {
	cases := []struct {
		name, inConfig, old, new, want string
	}{
		{"no universal binary ids in the archives", "config", "  - id: archive\n    ids: [google-mail-mcp]\n",
			"  - id: archive\n", "archives[0] names no ids"},
		{"the bundle not checksummed", "config", "checksum:\n  name_template: checksums.txt\n  extra_files:\n    - glob: ./dist/*.mcpb\n",
			"checksum:\n  name_template: checksums.txt\n", "checksum.extra_files does not cover"},
		{"the bundle not uploaded", "config", "  draft: false\n  extra_files:\n    - glob: ./dist/*.mcpb\n",
			"  draft: false\n", "release.extra_files does not cover"},
		{"a hook packing elsewhere", "config", "dist/google-mail-mcp_{{ .Version }}.mcpb", "out/x.mcpb",
			"the post hook packs to a path"},
		{"no trimpath", "config", "flags: [-trimpath]", "flags: []", "does not pass -trimpath"},
		{"a tidy hook", "config", "go mod download", "go mod tidy", "dirties the tree"},
		{"cosign without --bundle", "config", `"--bundle=${signature}", `, "", "does not pass --bundle"},
		{"a changelog block", "config", "release:\n", "changelog:\n  disable: true\nrelease:\n", "there is a changelog block"},
		{"a short matrix", "config", "goarch: [amd64, arm64]", "goarch: [amd64]", "builds 3 platform targets"},
		{"a space-separated subject path", "flow", `"dist/*.tar.gz,dist/*.zip,dist/*.mcpb,dist/checksums.txt"`,
			`"dist/*.tar.gz dist/*.zip dist/*.mcpb dist/checksums.txt"`, "separates with a space"},
		{"the bundle not attested", "flow", ",dist/*.mcpb", "", "does not name .mcpb"},
		{"one reproducible build", "flow", "build --single-target --snapshot --clean --output b",
			"build --snapshot --clean --output b", "builds one target 1 time(s)"},
		{"no release notes passed", "flow", "--release-notes=notes.md", "", "does not pass --release-notes"},
		{"no syft pin", "flow", "          syft-version: v1.52.0\n", "", "installs no syft"},
		{"no go test", "flow", "run: go test ./...", "run: echo", "runs no `go test`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config, flow := releaseTestConfig, releaseTestWorkflow
			target := &config
			if tc.inConfig == "flow" {
				target = &flow
			}
			if !strings.Contains(*target, tc.old) {
				t.Fatalf("no %q to break", tc.old)
			}
			*target = strings.Replace(*target, tc.old, tc.new, 1)
			joined := strings.Join(releaseTestRun(t, config, flow, releaseTestStaged(), releaseTestOut), "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("want %q, got:\n%s", tc.want, joined)
			}
		})
	}
}

func TestReleaseRefusesABadGlob(t *testing.T) {
	cases := map[string]struct {
		edit func([]mcpbStaged)
		want string
	}{
		"matches nothing": {func(s []mcpbStaged) { s[0].glob = "*darwin*universal*/google-mail-mcp" },
			"matches 0 build directories"},
		"matches two": {func(s []mcpbStaged) { s[3].glob = "*_linux_*/google-mail-mcp" }, "matches 2 build directories"},
		"hardcodes a variant": {func(s []mcpbStaged) { s[3].glob = "*_linux_amd64_v1/google-mail-mcp" },
			"matches 0 build directories"},
		"wrong binary name": {func(s []mcpbStaged) { s[1].glob = "*_windows_amd64*/google-mail-mcp" },
			`writes "google-mail-mcp.exe"`},
		"wrong platform":     {func(s []mcpbStaged) { s[1].platform = "linux" }, `entry point for "linux"`},
		"wrong architecture": {func(s []mcpbStaged) { s[4].goarch = "amd64" }, "says it runs on linux/amd64"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			staged := releaseTestStaged()
			tc.edit(staged)
			joined := strings.Join(releaseTestRun(t, releaseTestConfig, releaseTestWorkflow, staged, releaseTestOut), "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("want %q, got:\n%s", tc.want, joined)
			}
		})
	}
}
