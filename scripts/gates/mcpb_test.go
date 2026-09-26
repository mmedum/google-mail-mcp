package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// mcpbFixture decodes the good manifest fixture as a document.
func mcpbFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "mcpb", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// mcpbRoot writes a manifest document into a fresh repository root.
func mcpbRoot(t *testing.T, doc map[string]any) string {
	t.Helper()
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return writeTree(t, map[string]string{mcpbManifestPath: string(raw)})
}

// mcpbDig returns the object at a dotted path in a decoded document.
func mcpbDig(doc map[string]any, keys ...string) map[string]any {
	cur := doc
	for _, k := range keys {
		cur = cur[k].(map[string]any)
	}
	return cur
}

func TestMcpbCommittedManifestPasses(t *testing.T) {
	var out sink
	if err := mcpbCheck(&out, filepath.Join("..", "..")); err != nil {
		t.Fatalf("the committed manifest fails: %v\n%s", err, out.String())
	}
	out.mustSay(t, "5 staged files, 3 platforms")
}

func TestMcpbFixturePasses(t *testing.T) {
	var out sink
	if err := mcpbCheck(&out, mcpbRoot(t, mcpbFixture(t))); err != nil {
		t.Fatalf("the fixture fails: %v\n%s", err, out.String())
	}
}

// Each break, one at a time, refused with its own message.
func TestMcpbRefusesEachBreak(t *testing.T) {
	const pinned = "https://raw.githubusercontent.com/anthropics/mcpb/%s/schemas/mcpb-manifest-v%s.schema.json"
	cases := []struct {
		name   string
		mutate func(doc map[string]any)
		want   string
	}{
		{"entry point not staged", func(d map[string]any) {
			mcpbDig(d, "server")["entry_point"] = "server/nothing"
		}, `entry_point "server/nothing"`},
		{"command not staged", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config")["command"] = "${__dirname}/server/typo"
		}, "mcp_config.command is"},
		{"win32 override typo", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config", "platform_overrides", "win32")["command"] = "${__dirname}/server/gmail.exe"
		}, "platform_overrides.win32.command"},
		{"win32 override deleted", func(d map[string]any) {
			delete(mcpbDig(d, "server", "mcp_config", "platform_overrides"), "win32")
		}, `platform "win32" runs "server/google-mail-mcp-darwin"`},
		{"override for an unclaimed platform", func(d map[string]any) {
			mcpbDig(d, "compatibility")["platforms"] = []any{"darwin", "win32"}
		}, `platform_overrides has "linux"`},
		{"composed user_config not declared", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config", "env")["GMAIL_CONFIG_DIR"] = "${user_config.home}/gmail"
		}, "spends ${user_config.home}"},
		{"no $schema", func(d map[string]any) { delete(d, "$schema") }, "no $schema"},
		{"unpinned schema path", func(d map[string]any) {
			d["$schema"] = "https://raw.githubusercontent.com/anthropics/mcpb/main/dist/mcpb-manifest.schema.json"
		}, "not the versioned"},
		{"branch ref", func(d map[string]any) {
			d["$schema"] = strings.ReplaceAll(strings.Replace(pinned, "%s", "main", 1), "%s", "0.3")
		}, `ref "main"`},
		{"partial tag", func(d map[string]any) {
			d["$schema"] = strings.ReplaceAll(strings.Replace(pinned, "%s", "v2.1", 1), "%s", "0.3")
		}, `ref "v2.1"`},
		{"somebody else's host", func(d map[string]any) {
			d["$schema"] = "https://example.com/schemas/mcpb-manifest-v0.3.schema.json"
		}, "not upstream's published path"},
		{"version disagrees with the URL", func(d map[string]any) { d["manifest_version"] = "0.4" },
			`manifest_version is "0.4" and $schema pins v0.3`},
		{"below the floor, self-consistent", func(d map[string]any) {
			d["manifest_version"] = "0.2"
			d["$schema"] = strings.ReplaceAll(strings.Replace(pinned, "%s", "v2.1.2", 1), "%s", "0.2")
		}, "below the floor of 0.3"},
		{"no support", func(d map[string]any) { delete(d, "support") }, "support is"},
		{"no no-login sentence", func(d map[string]any) { d["long_description"] = "Read Gmail." },
			`does not say "does not log you in"`},
		{"old desktop", func(d map[string]any) { mcpbDig(d, "compatibility")["claude_desktop"] = ">=0.9.0" },
			"claude_desktop"},
		{"real version committed", func(d map[string]any) { d["version"] = "1.2.3" }, `version is "1.2.3"`},
		{"unknown key the schema refuses", func(d map[string]any) { d["entrypoint"] = "x" },
			"does not satisfy mcpb-manifest-v0.3.schema.json"},
		{"wrong name", func(d map[string]any) { d["name"] = "gmail" }, `name is "gmail"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := mcpbFixture(t)
			tc.mutate(doc)
			var out sink
			err := mcpbCheck(&out, mcpbRoot(t, doc))
			if err == nil {
				t.Fatalf("accepted: %s", out.String())
			}
			out.mustSay(t, tc.want)
		})
	}
}

// The launcher's names are the packer's; renaming a staged binary in
// one place and not the other fails.
func TestMcpbLauncherNamesMustMatch(t *testing.T) {
	var m mcpbManifest
	raw, _ := json.Marshal(mcpbFixture(t))
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if p := mcpbValidate(m, mcpbFiles, launcherNamesIn(launcherScript(mcpbFiles))); len(p) != 0 {
		t.Fatalf("the generated launcher disagrees: %v", p)
	}
	got := mcpbValidate(m, mcpbFiles, []string{binaryName + "-linux-x64", binaryName + "-linux-arm64"})
	if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "the launcher runs") }) {
		t.Errorf("a launcher naming another binary passed: %v", got)
	}
}

func TestMcpbBehindIsNumeric(t *testing.T) {
	for v, want := range map[string]bool{"0.2": true, "0.3": false, "0.10": false, "1.0": false, "x": true, "": true} {
		if got := mcpbBehind(v, "0.3"); got != want {
			t.Errorf("mcpbBehind(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestLauncherScriptNamesTheStagedBinaries(t *testing.T) {
	script := launcherScript(mcpbFiles)
	got := launcherNamesIn(script)
	want := []string{binaryName + "-linux-amd64", binaryName + "-linux-arm64"}
	if !slices.Equal(got, want) {
		t.Errorf("launcher names %v, want %v", got, want)
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "echo ") && !strings.Contains(line, ">&2") {
			t.Errorf("a launcher line writes to stdout, which is the JSON-RPC stream: %q", line)
		}
	}
	if !strings.Contains(script, `exec "$bin" "$@"`) {
		t.Error("the launcher does not exec the binary")
	}
}

// The launcher, run: an unknown architecture and a missing binary each
// go to stderr and exit non-zero; a present binary is exec'd.
func TestLauncherScriptRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a POSIX shell script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	launcher := filepath.Join(dir, "launch-linux.sh")
	if err := os.WriteFile(launcher, []byte(launcherScript(mcpbFiles)), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	fake := t.TempDir()
	runAs := func(arch string) (string, string, error) {
		t.Helper()
		uname := "#!/bin/sh\necho " + arch + "\n"
		if err := os.WriteFile(filepath.Join(fake, "uname"), []byte(uname), 0o700); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
		cmd := exec.Command(sh, launcher, "--flag")
		cmd.Env = append(os.Environ(), "PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"))
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	stdout, stderr, err := runAs("sparc64")
	if err == nil || stdout != "" || !strings.Contains(stderr, "no binary in this bundle for sparc64") {
		t.Errorf("unknown arch: err %v, stdout %q, stderr %q", err, stdout, stderr)
	}
	stdout, stderr, err = runAs("x86_64")
	if err == nil || stdout != "" || !strings.Contains(stderr, binaryName+"-linux-amd64 is missing") {
		t.Errorf("missing binary: err %v, stdout %q, stderr %q", err, stdout, stderr)
	}
	bin := filepath.Join(dir, binaryName+"-linux-arm64")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ran \"$@\"\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	stdout, stderr, err = runAs("aarch64")
	if err != nil || stdout != "ran --flag\n" {
		t.Errorf("present binary: err %v, stdout %q, stderr %q", err, stdout, stderr)
	}
}
