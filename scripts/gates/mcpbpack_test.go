package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// mcpbPackDist lays out a goreleaser dist tree with a stand-in for every
// binary, each a script printing version.
func mcpbPackDist(t *testing.T, root, version string) string {
	t.Helper()
	dist := filepath.Join(root, "dist")
	for _, dir := range []string{
		"google-mail-mcp-universal_darwin_all",
		"google-mail-mcp_windows_amd64_v1",
		"google-mail-mcp_linux_amd64_v1",
		"google-mail-mcp_linux_arm64_v8.0",
		"google-mail-mcp_darwin_amd64_v1", // not packed: the universal binary is
	} {
		name := binaryName
		if strings.Contains(dir, "windows") {
			name += ".exe"
		}
		path := filepath.Join(dist, dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		body := "#!/bin/sh\necho " + binaryName + " " + version + "\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

func mcpbPackFixtureRoot(t *testing.T) string {
	t.Helper()
	return mcpbRoot(t, mcpbFixture(t))
}

func TestMcpbPackWritesAndReadsBack(t *testing.T) {
	root := mcpbPackFixtureRoot(t)
	dist := mcpbPackDist(t, root, "1.2.3")
	var out sink
	bundle, err := mcpbPackTo(&out, root, dist, "v1.2.3", filepath.Join(dist, "google-mail-mcp_1.2.3.mcpb"), false)
	if err != nil {
		t.Fatalf("pack: %v\n%s", err, out.String())
	}
	out.mustSay(t, "6 entries, version 1.2.3, read back")

	zr, err := zip.OpenReader(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	// Stated, not read from the packer.
	fixed := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, f := range zr.File {
		if !f.Modified.Equal(fixed) {
			t.Errorf("%s stamped %v, want %v", f.Name, f.Modified, fixed)
		}
		if f.Name != "manifest.json" && f.Mode().Perm() != 0o755 {
			t.Errorf("%s has mode %v, want 0755", f.Name, f.Mode().Perm())
		}
		if f.Name == "manifest.json" {
			rc, _ := f.Open()
			raw, _ := io.ReadAll(rc)
			_ = rc.Close()
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			if doc["version"] != "1.2.3" {
				t.Errorf("packed version %v", doc["version"])
			}
			// A key the typed decode does not declare survives.
			if doc["display_name"] != "Gmail" {
				t.Errorf("display_name lost: %v", doc["display_name"])
			}
			if !bytes.Contains(raw, []byte(`">=0.10.0"`)) {
				t.Errorf("claude_desktop is not written as >=0.10.0; the encoder escaped it: %s", raw)
			}
		}
		if f.Name == "server/launch-linux.sh" {
			rc, _ := f.Open()
			raw, _ := io.ReadAll(rc)
			_ = rc.Close()
			if string(raw) != launcherScript(mcpbFiles) {
				t.Error("the packed launcher is not the generated one")
			}
		}
	}

	// Same inputs, same bytes.
	first, _ := os.ReadFile(bundle)
	if _, err := mcpbPackTo(&out, root, dist, "1.2.3", bundle, false); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(bundle)
	if !bytes.Equal(first, second) {
		t.Error("two packs of the same inputs differ")
	}
}

func TestMcpbPackRefuses(t *testing.T) {
	cases := []struct {
		name    string
		version string
		out     string
		setup   func(t *testing.T, root, dist string)
		want    string
	}{
		{name: "placeholder version", version: placeholderVersion, want: "committed placeholder"},
		{name: "bundle misnamed", version: "1.2.3", out: "bundle.mcpb", want: "the release publishes"},
		{name: "committed manifest stamped", version: "1.2.3", setup: func(t *testing.T, root, _ string) {
			doc := mcpbFixture(t)
			doc["version"] = "1.0.0"
			raw, _ := json.Marshal(doc)
			if err := os.WriteFile(filepath.Join(root, mcpbManifestPath), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "must carry"},
		{name: "glob matches nothing", version: "1.2.3", setup: func(t *testing.T, _, dist string) {
			if err := os.RemoveAll(filepath.Join(dist, "google-mail-mcp_linux_arm64_v8.0")); err != nil {
				t.Fatal(err)
			}
		}, want: "nothing matches"},
		{name: "glob matches two", version: "1.2.3", setup: func(t *testing.T, _, dist string) {
			extra := filepath.Join(dist, "google-mail-mcp_linux_amd64_v3", binaryName)
			if err := os.MkdirAll(filepath.Dir(extra), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(extra, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "matches 2 files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := mcpbPackFixtureRoot(t)
			dist := mcpbPackDist(t, root, tc.version)
			if tc.setup != nil {
				tc.setup(t, root, dist)
			}
			out := filepath.Join(dist, tc.out)
			if tc.out == "" {
				out = filepath.Join(dist, mcpbPackName(tc.version))
			}
			var s sink
			_, err := mcpbPackTo(&s, root, dist, tc.version, out, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want %q", err, tc.want)
			}
		})
	}
}

// The staged binary for this host is run, and its --version must name
// the version being stamped.
func TestMcpbPackHostVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in binaries are shell scripts")
	}
	root := mcpbPackFixtureRoot(t)
	dist := mcpbPackDist(t, root, "1.2.2") // built as the previous version
	if err := filepath.Walk(dist, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			return os.Chmod(p, 0o700) //nolint:gosec // a test binary
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var out sink
	_, err := mcpbPackTo(&out, root, dist, "1.2.3", filepath.Join(dist, mcpbPackName("1.2.3")), true)
	hostStaged := (runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")) ||
		runtime.GOOS == "darwin"
	if !hostStaged {
		if err != nil {
			t.Fatal(err)
		}
		out.mustSay(t, "read-back is skipped")
		return
	}
	if err == nil || !strings.Contains(err.Error(), "--version says") {
		t.Errorf("a binary reporting 1.2.2 packed as 1.2.3: %v", err)
	}
	if _, err := mcpbPackTo(&out, root, dist, "1.2.3-SNAPSHOT-abc", filepath.Join(dist, mcpbPackName("1.2.3-SNAPSHOT-abc")), true); err != nil {
		t.Errorf("a snapshot is exempt: %v", err)
	}
}
