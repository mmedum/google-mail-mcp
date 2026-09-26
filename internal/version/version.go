// Package version reports the binary's version. Releases set it with
// -ldflags "-X github.com/mmedum/google-mail-mcp/internal/version.Version=1.2.3".
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is set through ldflags. A local build leaves it "dev".
var Version = "dev"

// readBuildInfo is debug.ReadBuildInfo, replaceable in tests.
var readBuildInfo = debug.ReadBuildInfo

// String is the version to report.
//
// `go install module@v1.2.3` applies no ldflags, so a binary installed
// that way would call itself "dev" forever. Go records the module
// version it was built from, and that is the answer in that case.
func String() string {
	if Version != "dev" {
		return canonical(Version)
	}
	if info, ok := readBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return canonical(v)
		}
	}
	return Version
}

// canonical gives a release one spelling however it was built:
// goreleaser stamps the version without its leading v, and the build
// info keeps it.
func canonical(v string) string {
	if v == "" || v == "dev" {
		return v
	}
	if v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

// Info is the line --version prints.
func Info() string {
	return fmt.Sprintf("google-mail-mcp %s (%s %s/%s)", String(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Module returns the version of a dependency this binary was built
// with, or "unknown". The schema dump uses it for the MCP SDK, because
// a constant naming that version goes stale on the next upgrade.
func Module(path string) string {
	info, ok := readBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, d := range info.Deps {
		if d.Path == path {
			if d.Replace != nil && d.Replace.Version != "" {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return "unknown"
}
