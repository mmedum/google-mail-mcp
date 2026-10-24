package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	"github.com/mmedum/google-mail-mcp/v3/internal/config"
)

// The bundle gate. Its checks are referential — does every name in the
// manifest resolve to a file the packer stages, for the right platform —
// and the staged names are static, so they run on every commit against
// the committed manifest with no build. Only packing waits for a release.

const (
	// mcpbManifestPath is the committed manifest.
	mcpbManifestPath = "packaging/mcpb/manifest.json"
	// mcpbMinManifestVersion is the floor. Claims about $schema hold the
	// manifest against itself; a stale manifest is self-consistent.
	mcpbMinManifestVersion = "0.3"
	// mcpbNoLoginPhrase must appear in long_description: the bundle
	// cannot sign anybody in, and an install that omits this appears to
	// work and then fails on the first call.
	mcpbNoLoginPhrase = "does not log you in"
	// mcpbMinDesktop is the claude_desktop requirement the manifest
	// declares.
	mcpbMinDesktop = ">=0.10.0"
)

// mcpbStaged is one file the packer puts in the bundle. The table below
// is the single source of the staged names: the gate reads it without a
// build, the packer reads it to find the files, and the Linux launcher
// is generated from it.
type mcpbStaged struct {
	// path is where the file lands in the bundle.
	path string
	// glob finds it under the release's dist directory; empty for a
	// generated file or one copied from the repository.
	glob string
	// repo is the repository file it is copied from, for a file that is
	// read rather than run.
	repo string
	// launcher marks the generated Linux launcher, written at pack time
	// from this table.
	launcher bool
	// platform is the one this file is the entry point for; empty for a
	// binary the launcher chooses.
	platform string
	// goos and goarch say where the file runs, for reading back its
	// --version on the host. goarch "all" is a universal binary.
	goos, goarch string
	// uname lists the `uname -m` values the launcher maps to this file.
	uname []string
}

// mcpbFiles is what a bundle contains. goreleaser names directories
// <build id>_<goos>_<goarch>[_<variant>], and the universal binary's
// <id>_darwin_all.
var mcpbFiles = []mcpbStaged{
	{path: "server/" + binaryName + "-darwin", glob: "*_darwin_all/" + binaryName,
		platform: "darwin", goos: "darwin", goarch: "all"},
	{path: "server/" + binaryName + ".exe", glob: "*_windows_amd64*/" + binaryName + ".exe",
		platform: "win32", goos: "windows", goarch: "amd64"},
	{path: "server/launch-linux.sh", launcher: true,
		platform: "linux"},
	{path: "server/" + binaryName + "-linux-amd64", glob: "*_linux_amd64*/" + binaryName,
		goos: "linux", goarch: "amd64", uname: []string{"x86_64", "amd64"}},
	{path: "server/" + binaryName + "-linux-arm64", glob: "*_linux_arm64*/" + binaryName,
		goos: "linux", goarch: "arm64", uname: []string{"aarch64", "arm64"}},
	// The bundle redistributes the binaries as the archives do, with the
	// license and the README beside them.
	{path: "LICENSE", repo: "LICENSE"},
	{path: "README.md", repo: "README.md"},
}

// mcpbManifest is the part of the manifest the checks read. A key it
// does not name is still checked and still shipped: the vendored schema
// validates the document's bytes, and the packer stamps the version into
// them, not into this.
type mcpbManifest struct {
	Schema          string   `json:"$schema"`
	ManifestVersion string   `json:"manifest_version"`
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	Description     string   `json:"description"`
	LongDescription string   `json:"long_description"`
	Support         string   `json:"support"`
	License         string   `json:"license"`
	PrivacyPolicies []string `json:"privacy_policies"`
	Server          struct {
		Type       string `json:"type"`
		EntryPoint string `json:"entry_point"`
		MCPConfig  struct {
			Command string `json:"command"`
			// Args stays raw, so a missing key, null and a list are told
			// apart.
			Args              json.RawMessage   `json:"args"`
			Env               map[string]string `json:"env"`
			PlatformOverrides map[string]struct {
				Command string            `json:"command"`
				Args    []string          `json:"args"`
				Env     map[string]string `json:"env"`
			} `json:"platform_overrides"`
		} `json:"mcp_config"`
	} `json:"server"`
	UserConfig    map[string]mcpbOption `json:"user_config"`
	Compatibility struct {
		ClaudeDesktop string   `json:"claude_desktop"`
		Platforms     []string `json:"platforms"`
	} `json:"compatibility"`
}

// mcpbOption is one user_config entry. Its default stays JSON, so a
// switch's is compared as the host reads it.
type mcpbOption struct {
	Type        string          `json:"type"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Required    bool            `json:"required"`
	Multiple    bool            `json:"multiple"`
	Default     json.RawMessage `json:"default"`
}

// mcpb is the gate: the committed manifest, in the working directory.
func mcpb(out io.Writer, _ []string) error {
	return mcpbCheck(out, ".")
}

func mcpbCheck(out io.Writer, root string) error {
	_, m, problems, err := mcpbCommittedManifest(root)
	if err != nil {
		return err
	}
	if err := problemsError(out, mcpbManifestPath, problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "mcpb: %s: %d staged files, %d platforms, %d user_config keys; every name resolves\n",
		mcpbManifestPath, len(mcpbFiles), len(m.Compatibility.Platforms), len(m.UserConfig))
	return nil
}

// mcpbCommittedManifest is where the gate and the packer both start: the
// committed manifest under root, read once and decoded once, and every
// problem mcpbManifestProblems finds in it, sorted. The packer ships this
// document with the real version stamped in, not these bytes.
//
// A key repeated in one object is refused before anything else is
// checked, and so is a key that differs from another only in case. The
// decode into mcpbManifest merges a repeated object into the first, and
// matches a key to a field whatever its case, while the schema
// validator, the packer's stamp and the host each keep the last and
// read a key only as spelled, so every other check would read a
// document that is not the one shipped (§18 row 96).
func mcpbCommittedManifest(root string) (raw []byte, m mcpbManifest, problems []string, err error) {
	raw, m, err = mcpbReadManifest(root)
	if err != nil {
		return nil, mcpbManifest{}, nil, err
	}
	repeated, err := mcpbRepeatedKeys(raw)
	if err != nil {
		return nil, mcpbManifest{}, nil, fmt.Errorf("%s: reading its keys: %w", mcpbManifestPath, err)
	}
	if len(repeated) > 0 {
		for _, at := range repeated {
			problems = append(problems, fmt.Sprintf("the manifest has %s more than once, counting a spelling "+
				"that differs only in case; the gate's decode merges such keys, and the schema, the packer and "+
				"the host do not, so they would read different documents", at))
		}
		slices.Sort(problems)
		return raw, m, problems, nil
	}
	problems = mcpbManifestProblems(root, m, raw)
	slices.Sort(problems)
	return raw, m, problems, nil
}

// mcpbReadManifest reads the committed manifest under root and decodes
// it, once each.
func mcpbReadManifest(root string) ([]byte, mcpbManifest, error) {
	path := filepath.Join(root, mcpbManifestPath)
	raw, err := os.ReadFile(path) //nolint:gosec // a repository path
	if err != nil {
		return nil, mcpbManifest{}, err
	}
	var m mcpbManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, mcpbManifest{}, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return raw, m, nil
}

// mcpbRepeatedKeys is the path of every key an object in raw has more
// than once, a key equal to another under Unicode case folding counting
// as the same key, each once, in document order: "server" at the top
// level, "server.mcp_config.env.GMAIL_PROFILE" inside, [i] for an
// element of a list, and ["a.b"] for a key that is not plainly a name.
// A document whose tokens cannot be read is an error, never a document
// with no repeats.
func mcpbRepeatedKeys(raw []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var repeated []string
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			// The first spelling of each key, and whether it has been
			// reported.
			var seen []string
			reported := map[int]bool{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := keyTok.(string)
				at := mcpbKeyPath(path, key)
				switch i := slices.IndexFunc(seen, func(k string) bool { return strings.EqualFold(k, key) }); {
				case i < 0:
					seen = append(seen, key)
				case !reported[i]:
					reported[i] = true
					repeated = append(repeated, at)
				}
				if err := walk(at); err != nil {
					return err
				}
			}
		case json.Delim('['):
			for i := 0; dec.More(); i++ {
				if err := walk(fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		default:
			return nil
		}
		_, err = dec.Token() // the closing brace or bracket
		return err
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return repeated, nil
}

// mcpbPlainKey is a key a path names bare.
var mcpbPlainKey = regexp.MustCompile(`^[A-Za-z0-9_$-]+$`)

// mcpbKeyPath is the path of key in the object at path. A key that is
// not plainly a name, such as one holding a dot or a bracket, is quoted,
// so no two paths read the same.
func mcpbKeyPath(path, key string) string {
	switch {
	case !mcpbPlainKey.MatchString(key):
		return path + "[" + strconv.Quote(key) + "]"
	case path == "":
		return key
	}
	return path + "." + key
}

// mcpbManifestProblems is every check the gate and the packer both run:
// the manifest's own, and the files it copies from the repository at
// root.
func mcpbManifestProblems(root string, m mcpbManifest, raw []byte) []string {
	problems := mcpbValidate(m, mcpbFiles, launcherNamesIn(launcherScript(mcpbFiles)))
	if m.Version != placeholderVersion {
		problems = append(problems, fmt.Sprintf("version is %q; the committed manifest carries %q and the "+
			"packer writes the real one", m.Version, placeholderVersion))
	}
	problems = append(problems, mcpbDocumentProblems(m, raw)...)
	problems = append(problems, mcpbIdentityProblems(m, mcpbFiles)...)
	problems = append(problems, mcpbSettingProblems(m, config.Vars(), mcpbLeftOut)...)
	return append(problems, mcpbRepoProblems(root, m)...)
}

// mcpbRepoProblems reads each file the packer copies from the repository
// once: that it is there, and for LICENSE, which license it is.
func mcpbRepoProblems(root string, m mcpbManifest) []string {
	var problems []string
	for _, f := range mcpbFiles {
		if f.repo == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, f.repo)) //nolint:gosec // a repository path
		if err != nil {
			problems = append(problems, fmt.Sprintf("the packer copies %s from the repository and it is not there", f.repo))
			continue
		}
		if f.repo != "LICENSE" {
			continue
		}
		if want := mcpbLicense(data); want != "" && m.License != want {
			problems = append(problems, fmt.Sprintf("license is %q and LICENSE is %s", m.License, want))
		}
	}
	return problems
}

// mcpbDocumentProblems holds what the manifest says about itself: its
// schema reference, its version floor, the vendored schema, the no-login
// sentence and support.
func mcpbDocumentProblems(m mcpbManifest, raw []byte) []string {
	problems := mcpbShapeProblems(m.Schema, m.ManifestVersion, m.Support)
	if name, err := vendorManifestSchemaFile(m.ManifestVersion); err != nil {
		problems = append(problems, err.Error())
	} else if err := vendorValidate(name, "the manifest", raw); err != nil {
		problems = append(problems, err.Error())
	}
	if !strings.Contains(strings.ToLower(m.LongDescription), mcpbNoLoginPhrase) {
		problems = append(problems, fmt.Sprintf("long_description does not say %q: the bundle cannot sign "+
			"anybody in, and that is the first thing a person who installs it meets", mcpbNoLoginPhrase))
	}
	return problems
}

// mcpbIdentityProblems holds what the manifest says it is: the server's
// own name, a binary, the family's Claude Desktop floor, a privacy
// policy, and exactly the platforms the staged table has an entry point
// for, so dropping one platform and its override together is refused.
// The manifest specification requires privacy_policies of a server that
// connects to a service processing user data, and neither the schema
// nor `mcpb validate` enforces it. mcpbValidate holds each platform to
// the file staged for it.
func mcpbIdentityProblems(m mcpbManifest, files []mcpbStaged) []string {
	var problems []string
	if m.Name != binaryName {
		problems = append(problems, fmt.Sprintf("name is %q, want %q", m.Name, binaryName))
	}
	if m.Server.Type != "binary" {
		problems = append(problems, fmt.Sprintf("server.type is %q; the bundle carries a binary", m.Server.Type))
	}
	if m.Compatibility.ClaudeDesktop != mcpbMinDesktop {
		problems = append(problems, fmt.Sprintf("compatibility.claude_desktop is %q, want %q",
			m.Compatibility.ClaudeDesktop, mcpbMinDesktop))
	}
	var entryPoints []string
	for _, f := range files {
		if f.platform != "" {
			entryPoints = append(entryPoints, f.platform)
		}
	}
	slices.Sort(entryPoints)
	if claimed := slices.Sorted(slices.Values(m.Compatibility.Platforms)); !slices.Equal(claimed, entryPoints) {
		problems = append(problems, fmt.Sprintf("compatibility.platforms is %v; the packer stages entry points for %v",
			claimed, entryPoints))
	}
	if len(m.PrivacyPolicies) == 0 {
		problems = append(problems, "privacy_policies is empty; the manifest specification requires one for a "+
			"server that connects to a service processing user data, as this one does to Gmail")
	}
	return problems
}

// mcpbUserConfigRef matches every ${user_config.x} in a value, including
// one composed into a longer string.
var mcpbUserConfigRef = regexp.MustCompile(`\$\{user_config\.([A-Za-z0-9_]+)\}`)

// mcpbValidate is the referential checks. It takes the staged table and
// the launcher's names as arguments so a test can break each one.
func mcpbValidate(m mcpbManifest, files []mcpbStaged, launcher []string) []string {
	var problems []string
	// Only a program can be spawned; the files copied from the repository
	// are there to be read.
	staged := map[string]mcpbStaged{}
	for _, f := range files {
		if f.repo == "" {
			staged[f.path] = f
		}
	}
	resolve := func(cmd string) string {
		return strings.TrimPrefix(cmd, "${__dirname}/")
	}

	// entry_point names a staged program, and the one the default
	// command runs, which follows the commands.
	_, entryStaged := staged[m.Server.EntryPoint]
	if !entryStaged {
		problems = append(problems, fmt.Sprintf("entry_point %q is not a program the packer stages", m.Server.EntryPoint))
	}

	// Every command names one too.
	commands := map[string]string{"": m.Server.MCPConfig.Command}
	for platform, over := range m.Server.MCPConfig.PlatformOverrides {
		if over.Command != "" {
			commands[platform] = over.Command
		}
	}
	for _, platform := range slices.Sorted(maps.Keys(commands)) {
		where := "mcp_config.command"
		if platform != "" {
			where = "platform_overrides." + platform + ".command"
		}
		// ${__dirname} is the one path the specification fixes to the
		// bundle; what a relative command is resolved against, no source
		// says (§18 row 95).
		if !strings.HasPrefix(commands[platform], "${__dirname}/") {
			problems = append(problems, fmt.Sprintf("%s is %q; a command starts with ${__dirname}/, the one "+
				"path the specification fixes to the bundle", where, commands[platform]))
			continue
		}
		if _, ok := staged[resolve(commands[platform])]; !ok {
			problems = append(problems, fmt.Sprintf("%s is %q, which is not a program the packer stages", where, commands[platform]))
		}
	}
	// So the bundle does not name one file as the server and spawn
	// another. An entry point or a command that names no program is
	// reported above.
	if rel, ok := strings.CutPrefix(commands[""], "${__dirname}/"); ok && entryStaged && rel != m.Server.EntryPoint {
		if _, ok := staged[rel]; ok {
			problems = append(problems, fmt.Sprintf("entry_point is %q, and mcp_config.command runs %q",
				m.Server.EntryPoint, rel))
		}
	}

	problems = append(problems, mcpbReferenceProblems(m)...)

	// Every override is a claimed platform.
	claimed := map[string]bool{}
	for _, p := range m.Compatibility.Platforms {
		claimed[p] = true
	}
	for platform := range m.Server.MCPConfig.PlatformOverrides {
		if !claimed[platform] {
			problems = append(problems, fmt.Sprintf("platform_overrides has %q, which compatibility.platforms "+
				"does not claim", platform))
		}
	}

	// Each claimed platform spawns the file staged for it, not merely a
	// staged file: without its override, a platform runs the default
	// command, which is another platform's binary.
	for _, platform := range m.Compatibility.Platforms {
		cmd, ok := commands[platform]
		if !ok {
			cmd = commands[""]
		}
		f, found := staged[resolve(cmd)]
		switch {
		case !found || f.platform == platform:
		case f.platform == "":
			problems = append(problems, fmt.Sprintf("platform %q runs %q, which only the launcher runs", platform, f.path))
		default:
			problems = append(problems, fmt.Sprintf("platform %q runs %q, which is staged for %q", platform, f.path, f.platform))
		}
	}

	// The launcher chooses between the packer's names.
	var launchable []string
	for _, f := range files {
		if len(f.uname) > 0 {
			launchable = append(launchable, filepath.Base(f.path))
		}
	}
	chosen := slices.Clone(launcher)
	slices.Sort(launchable)
	slices.Sort(chosen)
	if !slices.Equal(launchable, chosen) {
		problems = append(problems, fmt.Sprintf("the launcher runs [%s]; the packer stages [%s]",
			strings.Join(chosen, " "), strings.Join(launchable, " ")))
	}
	return problems
}

// mcpbReferenceProblems holds every ${user_config.x} mcp_config.env
// spends to a key user_config declares, and that a host substitutes:
// the reference host leaves a reference as written when its key is
// optional with no default, or takes multiple values (§18 row 91). An
// override's env is refused outright, so it is not read here.
func mcpbReferenceProblems(m mcpbManifest) []string {
	var problems []string
	for key, value := range m.Server.MCPConfig.Env {
		for _, ref := range mcpbUserConfigRef.FindAllStringSubmatch(value, -1) {
			opt, ok := m.UserConfig[ref[1]]
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("mcp_config.env.%s spends ${user_config.%s}, which user_config "+
					"does not declare", key, ref[1]))
			case !opt.Required && opt.Default == nil:
				problems = append(problems, fmt.Sprintf("mcp_config.env.%s spends ${user_config.%s}, which is optional "+
					"with no default; a host leaves it as written when the person sets nothing", key, ref[1]))
			case opt.Multiple:
				problems = append(problems, fmt.Sprintf("mcp_config.env.%s spends ${user_config.%s}, which takes multiple "+
					"values; a host does not substitute an array in a string", key, ref[1]))
			}
		}
	}
	return problems
}

var (
	// mcpbPinnedSchema captures the format version a schema URL names.
	mcpbPinnedSchema = regexp.MustCompile(`/mcpb-manifest-v(\d+\.\d+)\.schema\.json$`)
	// mcpbUpstreamSchema is the whole upstream URL, capturing its ref.
	// An allow-list on the whole URL: a blacklist of branch names passes
	// any other branch, a partial tag and somebody else's host.
	mcpbUpstreamSchema = regexp.MustCompile(
		`^https://raw\.githubusercontent\.com/anthropics/mcpb/([^/]+)/schemas/mcpb-manifest-v\d+\.\d+\.schema\.json$`)
	// mcpbImmutableRef is a full release tag or a commit SHA.
	mcpbImmutableRef = regexp.MustCompile(`^(v[0-9]+\.[0-9]+\.[0-9]+|[0-9a-f]{40})$`)
)

// mcpbShapeProblems holds the manifest's declaration of its own format.
func mcpbShapeProblems(schema, manifestVersion, support string) []string {
	var problems []string
	switch {
	case schema == "":
		problems = append(problems, "the manifest has no $schema, so nothing says which format it is")
	case !mcpbPinnedSchema.MatchString(schema):
		problems = append(problems, fmt.Sprintf("$schema %q is not the versioned mcpb-manifest-v<x.y>.schema.json "+
			"file; the unpinned path serves whatever upstream publishes today", schema))
	case !mcpbUpstreamSchema.MatchString(schema):
		problems = append(problems, fmt.Sprintf("$schema %q is not upstream's published path", schema))
	case !mcpbImmutableRef.MatchString(mcpbUpstreamSchema.FindStringSubmatch(schema)[1]):
		problems = append(problems, fmt.Sprintf("$schema is served from ref %q, which can move; name a full tag "+
			"or a commit SHA", mcpbUpstreamSchema.FindStringSubmatch(schema)[1]))
	default:
		if declared := mcpbPinnedSchema.FindStringSubmatch(schema)[1]; declared != manifestVersion {
			problems = append(problems, fmt.Sprintf("manifest_version is %q and $schema pins v%s", manifestVersion, declared))
		}
	}
	if mcpbBehind(manifestVersion, mcpbMinManifestVersion) {
		problems = append(problems, fmt.Sprintf("manifest_version is %q, below the floor of %s",
			manifestVersion, mcpbMinManifestVersion))
	}
	if !strings.HasPrefix(support, "https://") {
		problems = append(problems, fmt.Sprintf("support is %q; a bundle that fails on somebody's desktop "+
			"must say where to report it, as an https URL", support))
	}
	return problems
}

// mcpbBehind reports whether a major.minor version is below the floor,
// numerically: "0.10" is not older than "0.3". An unreadable version is
// behind.
func mcpbBehind(version, floor string) bool {
	parse := func(v string) (int, int, bool) {
		a, b, ok := strings.Cut(v, ".")
		if !ok {
			return 0, 0, false
		}
		x, err1 := strconv.Atoi(a)
		y, err2 := strconv.Atoi(b)
		return x, y, err1 == nil && err2 == nil
	}
	major, minor, ok := parse(version)
	fMajor, fMinor, _ := parse(floor)
	if !ok {
		return true
	}
	if major != fMajor {
		return major < fMajor
	}
	return minor < fMinor
}

// mcpbLicense is the SPDX id of a LICENSE's text, or "" when it is not
// recognized.
func mcpbLicense(data []byte) string {
	switch text := string(data); {
	case strings.Contains(text, "Apache License"):
		return "Apache-2.0"
	case strings.Contains(text, "MIT License"):
		return "MIT"
	}
	return ""
}

// mcpbLeftOut is every variable the server reads that the bundle does
// not ask for, with why. The bundle asks for every other one.
var mcpbLeftOut = map[string]string{
	config.EnvPrefix + "LOG_LEVEL":      "diagnostics, set by hand while chasing a problem",
	config.EnvPrefix + "LOG_FORMAT":     "diagnostics, set by hand while chasing a problem",
	config.EnvPrefix + "HTTP_TIMEOUT":   "tuning, whose default suits an install",
	config.EnvPrefix + "API_BASE":       "a development override, for tests against a local fake",
	config.EnvConfigDir:                 "moves every profile, which tests do; a second account is a profile",
	config.EnvRefreshToken:              "a credential for automation; a bundle install uses the login's keyring entry",
	config.EnvPrefix + "REQUIRE_PROMPT": "removed, and read only to refuse it set",
}

// mcpbAskedFrom is what the bundle asks for at install: every variable
// the server reads, less each one leftOut gives a reason for. A variable
// added later is asked for until the manifest asks for it or a reason is
// written. It also refuses a reason for a variable the server does not
// read, so a removed one cannot linger.
func mcpbAskedFrom(vars []config.Var, leftOut map[string]string) (asked []config.Var, problems []string) {
	read := map[string]bool{}
	for _, v := range vars {
		read[v.Name] = true
		if leftOut[v.Name] == "" {
			asked = append(asked, v)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(leftOut)) {
		if !read[name] {
			problems = append(problems, fmt.Sprintf("mcpbLeftOut names %s, which the server does not read", name))
		}
	}
	return asked, problems
}

// mcpbProfileVar is the setting whose field states config.ProfileRule.
const mcpbProfileVar = config.EnvPrefix + "PROFILE"

// mcpbKey is the user_config key a setting is asked under: its name
// without the prefix, lowercased, which gives the family's bundles the
// same keys.
func mcpbKey(name string) string {
	return strings.ToLower(strings.TrimPrefix(name, config.EnvPrefix))
}

// mcpbSettingProblems holds what the bundle asks for to the server's own
// settings. vars and leftOut are config.Vars and mcpbLeftOut, arguments
// so a test can add to either.
//
// Every variable mcpbAskedFrom derives is asked for and reaches its
// variable, and nothing else is set. A switch is asked as a boolean, so
// Claude Desktop shows it as one. Settings pass by mcp_config.env alone:
// the base args stays [], and a platform override carries neither args
// nor env, an empty one included, because the host takes an override's
// in place of the base's (§18 row 94). An argument such as
// --enable-send=true, or an override env, would configure the server
// past every check here, which reads mcp_config.env alone.
func mcpbSettingProblems(m mcpbManifest, vars []config.Var, leftOut map[string]string) []string {
	asked, problems := mcpbAskedFrom(vars, leftOut)

	const keepArgs = "keep it as [], the shape every released bundle has had, since no Claude Desktop launch " +
		"has shown the key optional"
	var args []json.RawMessage
	switch raw := m.Server.MCPConfig.Args; {
	case raw == nil:
		problems = append(problems, "mcp_config.args is missing; "+keepArgs)
	case string(raw) == "null":
		problems = append(problems, "mcp_config.args is null; "+keepArgs)
	case json.Unmarshal(raw, &args) != nil || len(args) > 0:
		problems = append(problems, "mcp_config.args is not []; settings pass by env only, and an argument "+
			"there would configure the server past every check on env")
	}
	for _, platform := range slices.Sorted(maps.Keys(m.Server.MCPConfig.PlatformOverrides)) {
		over := m.Server.MCPConfig.PlatformOverrides[platform]
		if over.Args != nil {
			problems = append(problems, fmt.Sprintf("platform_overrides.%s.args is set; a host takes it in place of "+
				"mcp_config.args, an empty one too, and settings pass by env only", platform))
		}
		if over.Env != nil {
			problems = append(problems, fmt.Sprintf("platform_overrides.%s.env is set; a host takes it in place of "+
				"mcp_config.env, an empty one too, so put every variable in mcp_config.env", platform))
		}
	}

	keys, profileHeld := map[string]bool{}, false
	for _, v := range asked {
		key := mcpbKey(v.Name)
		keys[key] = true
		profileHeld = profileHeld || v.Name == mcpbProfileVar
		opt, ok := m.UserConfig[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("user_config does not ask for %s, which sets %s", key, v.Name))
			continue
		}
		if got, want := m.Server.MCPConfig.Env[v.Name], "${user_config."+key+"}"; got != want {
			problems = append(problems, fmt.Sprintf("mcp_config.env.%s is %q, want %q", v.Name, got, want))
		}
		if v.Switch && opt.Type != "boolean" {
			problems = append(problems, fmt.Sprintf("user_config.%s is a %s; %s is a switch, so ask for a boolean",
				key, cmp.Or(opt.Type, "field with no type"), v.Name))
		}
		// The server refuses any other name at start.
		if v.Name == mcpbProfileVar && !strings.Contains(opt.Description, config.ProfileRule) {
			problems = append(problems, fmt.Sprintf("user_config.%s does not say a name is %q", key, config.ProfileRule))
		}
	}
	if !profileHeld {
		problems = append(problems, fmt.Sprintf("the bundle asks for no %s, so nothing was held to "+
			"config.ProfileRule", mcpbProfileVar))
	}
	for _, key := range slices.Sorted(maps.Keys(m.UserConfig)) {
		if !keys[key] {
			problems = append(problems, fmt.Sprintf("user_config asks for %s, which is no setting's key", key))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(m.Server.MCPConfig.Env)) {
		switch {
		case slices.ContainsFunc(asked, func(v config.Var) bool { return v.Name == name }):
		case leftOut[name] != "":
			problems = append(problems, fmt.Sprintf("mcp_config.env sets %s, which the bundle leaves out: %s",
				name, leftOut[name]))
		default:
			problems = append(problems, fmt.Sprintf("mcp_config.env sets %s, which the server does not read", name))
		}
	}
	defaults := mcpbStartDefaults(m)
	problems = append(problems, mcpbDefaultsProblems(defaults)...)
	return append(problems, mcpbRefusalProblems(m, asked, mcpbRefused, defaults.err)...)
}

// mcpbHostEnv is the environment the reference host builds from the
// bundle's defaults, with each key in on set to true whatever its
// default, as the person setting it would. A boolean becomes
// "true" or "false", a string stays as it is, a number is written out,
// and a reference to a key with no default is left as written.
//
// A variable spending a required key with no default is left out: the
// person supplies it, and it is theirs either way. One with a default
// is not. The host checks a required key against the person's values
// alone, but Claude Desktop's form may fill it in from the default, and
// a person may accept it unread (§18 row 94). A default that is a list
// or an object is not guessed at: its variable is left out, and its key
// is in unreadable with what it is.
func mcpbHostEnv(m mcpbManifest, on ...string) (env, unreadable map[string]string) {
	env, unreadable = map[string]string{}, map[string]string{}
	for name, value := range m.Server.MCPConfig.Env {
		var b strings.Builder
		skip, last := false, 0
		for _, at := range mcpbUserConfigRef.FindAllStringSubmatchIndex(value, -1) {
			key, sub := value[at[2]:at[3]], value[at[0]:at[1]]
			opt := m.UserConfig[key]
			// A missing default leaves def nil. One present decodes, being
			// part of a manifest that did.
			var def any
			_ = json.Unmarshal(opt.Default, &def)
			if slices.Contains(on, key) {
				def = true
			}
			switch d := def.(type) {
			case bool:
				sub = strconv.FormatBool(d)
			case string:
				sub = d
			case float64:
				sub = strconv.FormatFloat(d, 'f', -1, 64)
			case nil:
				skip = skip || opt.Required
			case []any:
				unreadable[key], skip = "a list", true
			case map[string]any:
				unreadable[key], skip = "an object", true
			}
			b.WriteString(value[last:at[0]] + sub)
			last = at[1]
		}
		if !skip {
			env[name] = b.String() + value[last:]
		}
	}
	return env, unreadable
}

// mcpbLoad starts the server's configuration from an environment alone.
func mcpbLoad(env map[string]string) (config.Config, error) {
	return config.Load(nil, func(name string) string { return env[name] })
}

// mcpbDefaults is the environment a host builds from the bundle's
// defaults, and what the server starts with it, built once for the
// defaults and refusal checks.
type mcpbDefaults struct {
	env, unreadable map[string]string
	config          config.Config
	err             error
}

func mcpbStartDefaults(m mcpbManifest) mcpbDefaults {
	var d mcpbDefaults
	d.env, d.unreadable = mcpbHostEnv(m)
	d.config, d.err = mcpbLoad(d.env)
	return d
}

// mcpbDefaultsProblems reports a bundle whose defaults configure the
// server otherwise than no environment does, naming the fields that
// differ: a bundle install may turn on nothing an unconfigured server
// leaves off, because in Claude Desktop the only other guard is its
// approval of each tool, which a person can set to always allow (§18
// row 92). A list or object default is reported, every one in key
// order, and the comparison still runs over the rest.
func mcpbDefaultsProblems(d mcpbDefaults) []string {
	var problems []string
	for _, key := range slices.Sorted(maps.Keys(d.unreadable)) {
		problems = append(problems, fmt.Sprintf("user_config.%s defaults to %s, which the defaults check cannot "+
			"build an environment from: give it a boolean, a string or a number", key, d.unreadable[key]))
	}
	if d.err != nil {
		return append(problems, fmt.Sprintf("the environment the bundle's defaults give the server is refused: %v", d.err))
	}
	want, err := mcpbLoad(nil)
	if err != nil {
		return append(problems, fmt.Sprintf("an unconfigured server is refused: %v", err))
	}
	if diff := mcpbFieldDiff(d.config, want); len(diff) > 0 {
		problems = append(problems, fmt.Sprintf("the bundle's defaults configure the server otherwise than no "+
			"environment does, in %s; a bundle install may turn on nothing an unconfigured server leaves off",
			strings.Join(diff, ", ")))
	}
	return problems
}

// mcpbFieldDiff names the fields of two values of one struct type that
// differ, each compared as reflect.DeepEqual compares it: a pointer by
// what it points to, never by its address, and a slice, a map or an
// interface by its contents, without a panic. An unexported field is
// compared and named too.
func mcpbFieldDiff(a, b any) []string {
	va, vb := mcpbAddressable(a), mcpbAddressable(b)
	var fields []string
	for i := range va.NumField() {
		if !reflect.DeepEqual(mcpbFieldValue(va, i), mcpbFieldValue(vb, i)) {
			fields = append(fields, va.Type().Field(i).Name)
		}
	}
	return fields
}

// mcpbAddressable is a copy of v with an address, so that its fields
// have one too.
func mcpbAddressable(v any) reflect.Value {
	c := reflect.New(reflect.TypeOf(v)).Elem()
	c.Set(reflect.ValueOf(v))
	return c
}

// mcpbFieldValue is field i of the addressable struct v. Interface
// refuses an unexported field, so the field is read through its address
// in v, which is a copy no one else holds.
func mcpbFieldValue(v reflect.Value, i int) any {
	f := v.Field(i)
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface()
}

// mcpbRefused are the sets of the bundle's switches the server refuses
// at startup, each sorted. Each field in one names the others' titles in
// its description. The gate starts every set of the switches and holds
// the refused ones to this list, both ways.
var mcpbRefused = [][]string{
	{"enable_destructive", "read_only"},
	{"enable_send", "read_only"},
	{"enable_settings", "read_only"},
}

// mcpbRefusalProblems starts every set of the bundle's switches, each
// on over the defaults, and holds the ones the server refuses to
// declared: a refusal it does not declare, which no description then
// warns of, and a declared one the server starts with. A declared key
// that is not a switch the bundle asks for is refused too, and so is a
// field in a declared set with no title, which every description would
// name, each once. When the defaults alone do not start, which
// defaultsErr says, it starts no set: the defaults check reports that,
// and every set would fail with it.
func mcpbRefusalProblems(m mcpbManifest, asked []config.Var, declared [][]string, defaultsErr error) []string {
	var switches []string
	for _, v := range asked {
		if _, ok := m.UserConfig[mcpbKey(v.Name)]; ok && v.Switch {
			switches = append(switches, mcpbKey(v.Name))
		}
	}
	slices.Sort(switches)
	var problems []string
	inSets := map[string]bool{}
	for _, set := range declared {
		for _, key := range set {
			inSets[key] = true
		}
	}
	titled := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(inSets)) {
		switch {
		case !slices.Contains(switches, key):
			problems = append(problems, fmt.Sprintf("mcpbRefused names %s, which is not a switch the bundle asks for", key))
		case strings.TrimSpace(m.UserConfig[key].Title) == "":
			problems = append(problems, fmt.Sprintf("user_config.%s has no title, which the fields the server "+
				"refuses it with must name", key))
		default:
			titled[key] = true
		}
	}
	for _, set := range declared {
		for _, key := range set {
			if !slices.Contains(switches, key) {
				continue
			}
			for _, other := range set {
				title := strings.TrimSpace(m.UserConfig[other].Title)
				if other != key && titled[other] && !strings.Contains(m.UserConfig[key].Description, title) {
					problems = append(problems, fmt.Sprintf("user_config.%s does not name %q, which the server "+
						"refuses it with", key, title))
				}
			}
		}
	}
	if defaultsErr != nil {
		return problems
	}
	for bits := 1; bits < 1<<len(switches); bits++ {
		var on []string
		for i, key := range switches {
			if bits&(1<<i) != 0 {
				on = append(on, key)
			}
		}
		env, _ := mcpbHostEnv(m, on...)
		_, err := mcpbLoad(env)
		expected := slices.ContainsFunc(declared, func(set []string) bool {
			return !slices.ContainsFunc(set, func(key string) bool { return !slices.Contains(on, key) })
		})
		switch {
		case err != nil && !expected:
			problems = append(problems, fmt.Sprintf("the server refuses %s together, and mcpbRefused does not "+
				"declare it, so no description warns of it: %v", strings.Join(on, ", "), err))
		case err == nil && expected:
			problems = append(problems, fmt.Sprintf("mcpbRefused declares a refusal among %s, and the server "+
				"starts with them", strings.Join(on, ", ")))
		}
	}
	return problems
}
