package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func withBuildInfo(t *testing.T, info *debug.BuildInfo, ok bool) {
	t.Helper()
	prev := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return info, ok }
	t.Cleanup(func() { readBuildInfo = prev })
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := Version
	Version = v
	t.Cleanup(func() { Version = prev })
}

func TestString(t *testing.T) {
	tests := []struct {
		name    string
		ldflags string
		info    *debug.BuildInfo
		ok      bool
		want    string
	}{
		{"ldflags without v", "1.2.3", nil, false, "v1.2.3"},
		{"ldflags with v", "v1.2.3", nil, false, "v1.2.3"},
		{"go install", "dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.4.0"}}, true, "v0.4.0"},
		{"local build", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "dev"},
		{"no build info", "dev", nil, false, "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withVersion(t, tt.ldflags)
			withBuildInfo(t, tt.info, tt.ok)
			if got := String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInfoNamesTheBinary(t *testing.T) {
	if got := Info(); !strings.HasPrefix(got, "google-mail-mcp ") {
		t.Errorf("Info() = %q", got)
	}
}

func TestModule(t *testing.T) {
	withBuildInfo(t, &debug.BuildInfo{Deps: []*debug.Module{
		{Path: "example.test/a", Version: "v1.0.0"},
		{Path: "example.test/b", Version: "v1.0.0", Replace: &debug.Module{Version: "v1.1.0"}},
	}}, true)
	if got := Module("example.test/a"); got != "v1.0.0" {
		t.Errorf("a = %q", got)
	}
	if got := Module("example.test/b"); got != "v1.1.0" {
		t.Errorf("replaced b = %q", got)
	}
	if got := Module("example.test/c"); got != "unknown" {
		t.Errorf("missing c = %q", got)
	}
	withBuildInfo(t, nil, false)
	if got := Module("example.test/a"); got != "unknown" {
		t.Errorf("no build info = %q", got)
	}
}
