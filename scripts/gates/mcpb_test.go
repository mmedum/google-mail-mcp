package main

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/v3/internal/config"
)

// mcpbFixture decodes the committed manifest as a document, so the
// breaks below are of the bundle that ships.
func mcpbFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", mcpbManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// mcpbRoot writes a manifest document into a fresh repository root,
// with the files the packer copies from the repository.
func mcpbRoot(t *testing.T, doc map[string]any) string {
	t.Helper()
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return writeTree(t, map[string]string{
		mcpbManifestPath: string(raw),
		"LICENSE":        "                                 Apache License\n",
		"README.md":      "# google-mail-mcp\n",
	})
}

// mcpbCommitted decodes the committed manifest as the checks read it.
func mcpbCommitted(t *testing.T) mcpbManifest {
	t.Helper()
	_, m, err := mcpbReadManifest(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return m
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
	out.mustSay(t, "7 staged files, 3 platforms, 8 user_config keys")
}

// The breaks below start from the committed manifest in a fresh root,
// which passes.
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
		{"a command relative to the bundle", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config")["command"] = "server/google-mail-mcp-darwin"
		}, `mcp_config.command is "server/google-mail-mcp-darwin"; a command starts with ${__dirname}/`},
		{"a command from the root", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config", "platform_overrides", "win32")["command"] = "/server/google-mail-mcp.exe"
		}, `platform_overrides.win32.command is "/server/google-mail-mcp.exe"; a command starts with ${__dirname}/`},
		{"command not staged", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config")["command"] = "${__dirname}/server/typo"
		}, "mcp_config.command is"},
		{"entry point a repository file", func(d map[string]any) {
			mcpbDig(d, "server")["entry_point"] = "LICENSE"
		}, `entry_point "LICENSE" is not a program the packer stages`},
		{"command a repository file", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config")["command"] = "${__dirname}/README.md"
		}, `mcp_config.command is "${__dirname}/README.md", which is not a program the packer stages`},
		{"a platform running a binary the launcher chooses", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config", "platform_overrides", "linux")["command"] =
				"${__dirname}/server/google-mail-mcp-linux-amd64"
		}, `platform "linux" runs "server/google-mail-mcp-linux-amd64", which only the launcher runs`},
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
		{"a node server", func(d map[string]any) { mcpbDig(d, "server")["type"] = "node" },
			`server.type is "node"; the bundle carries a binary`},
		{"entry point another program than the command runs", func(d map[string]any) {
			mcpbDig(d, "server")["entry_point"] = "server/google-mail-mcp.exe"
		}, `entry_point is "server/google-mail-mcp.exe", and mcp_config.command runs "server/google-mail-mcp-darwin"`},
		// Every referential check passes this: no platform is left to
		// spawn the wrong file.
		{"a platform dropped with its override", func(d map[string]any) {
			mcpbDig(d, "compatibility")["platforms"] = []any{"darwin", "win32"}
			delete(mcpbDig(d, "server", "mcp_config", "platform_overrides"), "linux")
		}, "compatibility.platforms is [darwin win32]; the packer stages entry points for [darwin linux win32]"},
		{"a platform claimed twice", func(d map[string]any) {
			mcpbDig(d, "compatibility")["platforms"] = []any{"darwin", "win32", "linux", "linux"}
		}, "compatibility.platforms is [darwin linux linux win32]; the packer stages entry points for [darwin linux win32]"},
		{"no privacy policy", func(d map[string]any) { delete(d, "privacy_policies") }, "privacy_policies is empty"},
		{"a license LICENSE is not", func(d map[string]any) { d["license"] = "MIT" },
			`license is "MIT" and LICENSE is Apache-2.0`},
		{"a switch the bundle turns on", func(d map[string]any) {
			mcpbDig(d, "user_config", "enable_send")["default"] = true
		}, "the bundle's defaults configure the server otherwise than no environment does, in EnableSend;"},
		{"a required switch the bundle turns on", func(d map[string]any) {
			mcpbDig(d, "user_config", "enable_send")["required"] = true
			mcpbDig(d, "user_config", "enable_send")["default"] = true
		}, "the bundle's defaults configure the server otherwise than no environment does, in EnableSend;"},
		{"a switch asked as text", func(d map[string]any) {
			mcpbDig(d, "user_config", "enable_destructive")["type"] = "string"
			mcpbDig(d, "user_config", "enable_destructive")["default"] = "false"
		}, "user_config.enable_destructive is a string; GMAIL_ENABLE_DESTRUCTIVE is a switch, so ask for a boolean"},
		{"an optional key with no default", func(d map[string]any) {
			delete(mcpbDig(d, "user_config", "local_dir"), "default")
		}, "spends ${user_config.local_dir}, which is optional with no default"},
		{"a key with multiple values", func(d map[string]any) {
			mcpbDig(d, "user_config", "local_dir")["multiple"] = true
		}, "spends ${user_config.local_dir}, which takes multiple values"},
		{"a setting the bundle no longer asks for", func(d map[string]any) {
			delete(mcpbDig(d, "user_config"), "profile")
			delete(mcpbDig(d, "server", "mcp_config", "env"), "GMAIL_PROFILE")
		}, "user_config does not ask for profile, which sets GMAIL_PROFILE"},
		{"a setting that reaches another variable", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config", "env")["GMAIL_PROFILE"] = "${user_config.local_dir}"
		}, `mcp_config.env.GMAIL_PROFILE is "${user_config.local_dir}", want "${user_config.profile}"`},
		{"a key that is no setting's", func(d map[string]any) {
			mcpbDig(d, "user_config")["log_level"] = map[string]any{
				"type": "string", "title": "Log level", "description": "x", "default": "", "required": false}
		}, "user_config asks for log_level, which is no setting's key"},
		{"a variable the bundle leaves out", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config", "env")["GMAIL_LOG_LEVEL"] = "debug"
		}, "mcp_config.env sets GMAIL_LOG_LEVEL, which the bundle leaves out: diagnostics"},
		{"a variable the server does not read", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config", "env")["GMAIL_NOTHING"] = "x"
		}, "mcp_config.env sets GMAIL_NOTHING, which the server does not read"},
		{"base args missing", func(d map[string]any) { delete(mcpbDig(d, "server", "mcp_config"), "args") },
			"mcp_config.args is missing"},
		{"base args null", func(d map[string]any) { mcpbDig(d, "server", "mcp_config")["args"] = nil },
			"mcp_config.args is null"},
		{"base args with a flag", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config")["args"] = []any{"--enable-send=true"}
		}, "mcp_config.args is not []"},
		{"an empty override args", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config", "platform_overrides", "linux")["args"] = []any{}
		}, "platform_overrides.linux.args is set"},
		{"an empty override env", func(d map[string]any) {
			mcpbDig(d, "server", "mcp_config", "platform_overrides", "win32")["env"] = map[string]any{}
		}, "platform_overrides.win32.env is set"},
		{"a refused field with no title", func(d map[string]any) {
			mcpbDig(d, "user_config", "read_only")["title"] = ""
		}, "user_config.read_only has no title"},
		{"a refused field with a blank title", func(d map[string]any) {
			mcpbDig(d, "user_config", "read_only")["title"] = " "
		}, "user_config.read_only has no title"},
		{"a profile field that does not say what a name is", func(d map[string]any) {
			mcpbDig(d, "user_config", "profile")["description"] = "Which stored login to use."
		}, `user_config.profile does not say a name is "up to 64 characters from a to z`},
		{"a refused field that does not name its pair", func(d map[string]any) {
			mcpbDig(d, "user_config", "enable_send")["description"] = "Register the tool that sends a draft."
		}, `user_config.enable_send does not name "Read-only", which the server refuses it with`},
		{"defaults that configure the server", func(d map[string]any) {
			mcpbDig(d, "user_config", "profile")["default"] = "work"
		}, "the bundle's defaults configure the server otherwise than no environment does, in Profile;"},
		{"defaults the server refuses", func(d map[string]any) {
			mcpbDig(d, "user_config", "local_dir")["default"] = "attachments"
		}, `the environment the bundle's defaults give the server is refused: config: invalid: GMAIL_LOCAL_DIR "attachments"`},
		{"a required folder's default the server refuses", func(d map[string]any) {
			mcpbDig(d, "user_config", "local_dir")["required"] = true
			mcpbDig(d, "user_config", "local_dir")["default"] = "attachments"
		}, `the environment the bundle's defaults give the server is refused: config: invalid: GMAIL_LOCAL_DIR "attachments"`},
		{"a list default", func(d map[string]any) {
			mcpbDig(d, "user_config", "profile")["default"] = []any{"work"}
		}, "user_config.profile defaults to a list, which the defaults check cannot build an environment from"},
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

// The bundle asks for every variable the server reads, less those left
// out with a reason. A new one fails until the manifest asks for it,
// and a reason for one the server does not read fails too.
func TestMcpbAsksForEverySettingNotLeftOut(t *testing.T) {
	asked, problems := mcpbAskedFrom(config.Vars(), mcpbLeftOut)
	var names []string
	for _, v := range asked {
		names = append(names, v.Name)
	}
	want := []string{"GMAIL_PROFILE", "GMAIL_CLIENT_SECRET", "GMAIL_READ_ONLY", "GMAIL_ENABLE_SEND",
		"GMAIL_ENABLE_DESTRUCTIVE", "GMAIL_ENABLE_SETTINGS", "GMAIL_DRAFT_SIGNATURE", "GMAIL_LOCAL_DIR"}
	if !slices.Equal(names, want) || len(problems) != 0 {
		t.Fatalf("asked %v with problems %v, want %v and none", names, problems, want)
	}

	m := mcpbCommitted(t)
	if p := mcpbSettingProblems(m, config.Vars(), mcpbLeftOut); len(p) != 0 {
		t.Fatalf("the committed manifest fails: %v", p)
	}
	newVar := append(config.Vars(), config.Var{Name: "GMAIL_NEW"})
	got := strings.Join(mcpbSettingProblems(m, newVar, mcpbLeftOut), "\n")
	if !strings.Contains(got, "user_config does not ask for new, which sets GMAIL_NEW") {
		t.Errorf("a setting neither asked for nor left out passed: %s", got)
	}
	leftOut := maps.Clone(mcpbLeftOut)
	leftOut["GMAIL_GONE"] = "removed"
	got = strings.Join(mcpbSettingProblems(m, config.Vars(), leftOut), "\n")
	if !strings.Contains(got, "mcpbLeftOut names GMAIL_GONE, which the server does not read") {
		t.Errorf("a reason for a setting the server does not read passed: %s", got)
	}
	// With the profile left out, nothing holds a field to ProfileRule,
	// and that is refused rather than passed in silence.
	// A bundle that drops the profile field is told so once.
	dropped := mcpbCommitted(t)
	delete(dropped.UserConfig, "profile")
	delete(dropped.Server.MCPConfig.Env, "GMAIL_PROFILE")
	if p := slices.DeleteFunc(mcpbSettingProblems(dropped, config.Vars(), mcpbLeftOut), func(p string) bool {
		return !strings.Contains(p, "GMAIL_PROFILE")
	}); !slices.Equal(p, []string{"user_config does not ask for profile, which sets GMAIL_PROFILE"}) {
		t.Errorf("a dropped profile field gives %q, want the one problem", p)
	}
	noProfile := maps.Clone(mcpbLeftOut)
	noProfile["GMAIL_PROFILE"] = "left out"
	got = strings.Join(mcpbSettingProblems(m, config.Vars(), noProfile), "\n")
	if !strings.Contains(got, "the bundle asks for no GMAIL_PROFILE, so nothing was held to config.ProfileRule") {
		t.Errorf("a bundle with no profile setting passed the ProfileRule check: %s", got)
	}
}

// Every set of the switches is started, and the refused ones are held to
// the declared list both ways. A declared key that is not a switch is
// refused. When the defaults alone do not start, the check says nothing.
func TestMcpbRefusalsAreDeclared(t *testing.T) {
	m := mcpbCommitted(t)
	asked, _ := mcpbAskedFrom(config.Vars(), mcpbLeftOut)
	if p := mcpbRefusalProblems(m, asked, mcpbRefused, mcpbStartDefaults(m).err); len(p) != 0 {
		t.Fatalf("the committed manifest fails: %v", p)
	}
	// read_only is in every declared set, and a missing title is one
	// problem, not one per set.
	untitled := mcpbCommitted(t)
	opt := untitled.UserConfig["read_only"]
	opt.Title = ""
	untitled.UserConfig["read_only"] = opt
	got := mcpbRefusalProblems(untitled, asked, mcpbRefused, nil)
	if n := len(slices.DeleteFunc(got, func(p string) bool {
		return !strings.HasPrefix(p, "user_config.read_only has no title")
	})); n != 1 {
		t.Errorf("an untitled read_only is reported %d times, want 1", n)
	}
	// It is reported when the defaults do not start, too: the title check
	// starts nothing.
	if got := mcpbRefusalProblems(untitled, asked, mcpbRefused, errors.New("the defaults do not start")); !slices.ContainsFunc(got,
		func(p string) bool { return strings.HasPrefix(p, "user_config.read_only has no title") }) {
		t.Errorf("an untitled read_only passed beside defaults that do not start: %q", got)
	}
	// A title with a space around it is named without the space.
	padded := mcpbCommitted(t)
	opt = padded.UserConfig["read_only"]
	opt.Title += " "
	padded.UserConfig["read_only"] = opt
	if got := mcpbRefusalProblems(padded, asked, mcpbRefused, nil); len(got) != 0 {
		t.Errorf("a padded title is not found in descriptions that name it: %q", got)
	}
	cases := []struct {
		name     string
		declared [][]string
		want     string
	}{
		{"a refusal not declared", mcpbRefused[:2],
			"the server refuses enable_settings, read_only together, and mcpbRefused does not declare it"},
		{"a declared refusal the server starts with", append(slices.Clone(mcpbRefused), []string{"enable_send", "enable_settings"}),
			"mcpbRefused declares a refusal among enable_send, enable_settings, and the server starts with them"},
		{"a declared key that is not a switch", append(slices.Clone(mcpbRefused), []string{"profile", "read_only"}),
			"mcpbRefused names profile, which is not a switch the bundle asks for"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpbRefusalProblems(m, asked, tc.declared, mcpbStartDefaults(m).err)
			if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, tc.want) }) {
				t.Errorf("got %q, want one starting %q", got, tc.want)
			}
		})
	}

	m.UserConfig["local_dir"] = mcpbOption{Type: "directory", Default: json.RawMessage(`"attachments"`)}
	if got := mcpbRefusalProblems(m, asked, nil, mcpbStartDefaults(m).err); len(got) != 0 {
		t.Errorf("defaults that do not start: got %q, want nothing, since the defaults check reports it", got)
	}
}

// A switch in a set is on, whatever its default: a required one with
// none, or one with a list, is not left out, which would make a
// declared refusal look accepted and hide one not declared.
func TestMcpbRefusalsTurnASwitchOnWhateverItsDefault(t *testing.T) {
	asked, _ := mcpbAskedFrom(config.Vars(), mcpbLeftOut)

	m := mcpbCommitted(t)
	readOnly := m.UserConfig["read_only"]
	readOnly.Required, readOnly.Default = true, nil
	m.UserConfig["read_only"] = readOnly
	if got := mcpbRefusalProblems(m, asked, mcpbRefused, mcpbStartDefaults(m).err); len(got) != 0 {
		t.Errorf("a required switch with no default, refused as declared: got %q, want nothing", got)
	}

	m = mcpbCommitted(t)
	readOnly = m.UserConfig["read_only"]
	readOnly.Default = json.RawMessage(`["true"]`)
	m.UserConfig["read_only"] = readOnly
	got := mcpbRefusalProblems(m, asked, mcpbRefused[:2], mcpbStartDefaults(m).err)
	const want = "the server refuses enable_settings, read_only together, and mcpbRefused does not declare it"
	if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, want) }) {
		t.Errorf("a switch with a list default, refused and not declared: got %q, want one starting %q", got, want)
	}
}

// Every list or object default is reported, in key order, and the
// comparison still runs over the rest.
func TestMcpbDefaultsReportsEveryUnreadableDefault(t *testing.T) {
	m := mcpbCommitted(t)
	m.UserConfig["profile"] = mcpbOption{Type: "string", Default: json.RawMessage(`["work"]`)}
	m.UserConfig["local_dir"] = mcpbOption{Type: "directory", Default: json.RawMessage(`{"path": "x"}`)}
	send := m.UserConfig["enable_send"]
	send.Default = json.RawMessage(`true`)
	m.UserConfig["enable_send"] = send
	got := mcpbDefaultsProblems(mcpbStartDefaults(m))
	want := []string{
		"user_config.local_dir defaults to an object, which",
		"user_config.profile defaults to a list, which",
		"the bundle's defaults configure the server otherwise than no environment does",
	}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %d problems starting %q", got, len(want), want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("problem %d is %q, want one starting %q", i, got[i], want[i])
		}
	}
}

// Each file the packer copies from the repository must be there.
func TestMcpbRefusesAMissingRepositoryFile(t *testing.T) {
	copied := 0
	for _, f := range mcpbFiles {
		if f.repo == "" {
			continue
		}
		copied++
		root := mcpbRoot(t, mcpbFixture(t))
		if err := os.Remove(filepath.Join(root, f.repo)); err != nil {
			t.Fatal(err)
		}
		var out sink
		if err := mcpbCheck(&out, root); err == nil {
			t.Fatalf("a missing %s passed: %s", f.repo, out.String())
		}
		out.mustSay(t, "copies "+f.repo+" from the repository")
	}
	if copied < 2 {
		t.Fatalf("the packer copies %d files from the repository, below the floor of 2", copied)
	}
}

// The launcher's names are the packer's; renaming a staged binary in
// one place and not the other fails.
func TestMcpbLauncherNamesMustMatch(t *testing.T) {
	m := mcpbCommitted(t)
	if p := mcpbValidate(m, mcpbFiles, launcherNamesIn(launcherScript(mcpbFiles))); len(p) != 0 {
		t.Fatalf("the generated launcher disagrees: %v", p)
	}
	got := mcpbValidate(m, mcpbFiles, []string{binaryName + "-linux-x64", binaryName + "-linux-arm64"})
	if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "the launcher runs") }) {
		t.Errorf("a launcher naming another binary passed: %v", got)
	}
}

// mcpbRawRoot is a fresh repository root whose manifest is the committed
// one's text with old replaced by new, for a break no decoded document
// can carry, such as a key repeated.
func mcpbRawRoot(t *testing.T, old, new string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", mcpbManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), old) {
		t.Fatalf("the committed manifest does not say %q", old)
	}
	return writeTree(t, map[string]string{
		mcpbManifestPath: strings.Replace(string(raw), old, new, 1),
		"LICENSE":        "                                 Apache License\n",
		"README.md":      "# google-mail-mcp\n",
	})
}

// A key repeated in one object is refused, alone: the decode the checks
// read merges it, and the schema, the stamp and the host keep the last.
func TestMcpbRefusesARepeatedKey(t *testing.T) {
	cases := []struct{ name, old, new, want string }{
		// The decode merges the two, so every check sees the env and the
		// overrides; the stamp and the host keep the second, which has
		// neither.
		{"a second server object", `  "user_config": {`,
			`  "server": {"type": "binary", "entry_point": "server/google-mail-mcp-darwin", ` +
				`"mcp_config": {"command": "${__dirname}/server/google-mail-mcp-darwin", "args": []}},` +
				"\n" + `  "user_config": {`,
			"the manifest has server more than once"},
		{"a variable mapped twice", `"GMAIL_PROFILE": "${user_config.profile}",`,
			`"GMAIL_PROFILE": "${user_config.profile}", "GMAIL_PROFILE": "${user_config.profile}",`,
			"the manifest has server.mcp_config.env.GMAIL_PROFILE more than once"},
		// The decode matches Command to the field and keeps it, so every
		// check reads the staged program; the host runs command.
		{"a command spelled with a capital", `"command": "${__dirname}/server/google-mail-mcp-darwin",`,
			`"command": "/bin/sh", "Command": "${__dirname}/server/google-mail-mcp-darwin",`,
			"the manifest has server.mcp_config.Command more than once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, problems, err := mcpbCommittedManifest(mcpbRawRoot(t, tc.old, tc.new))
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 1 || !strings.HasPrefix(problems[0], tc.want) {
				t.Errorf("problems:\n%s\nwant the one starting %q", strings.Join(problems, "\n"), tc.want)
			}
		})
	}
}

// The repeated-key problems are sorted, as every other list of
// problems is.
func TestMcpbRepeatedKeysAreReportedSorted(t *testing.T) {
	_, _, problems, err := mcpbCommittedManifest(mcpbRawRoot(t, `"GMAIL_PROFILE": "${user_config.profile}",`,
		`"GMAIL_PROFILE": "${user_config.profile}", "Z": "", "Z": "", "A": "", "A": "",`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range problems {
		at, _, _ := strings.Cut(strings.TrimPrefix(p, "the manifest has "), " ")
		got = append(got, at)
	}
	if want := []string{"server.mcp_config.env.A", "server.mcp_config.env.Z"}; !slices.Equal(got, want) {
		t.Errorf("the repeated keys are reported as %q, want %q", got, want)
	}
}

// A key is repeated only within one object, a key equal to another but
// for case counting as the same, and each repeated key is named once,
// by a path no other key has, a list's elements and a repeated object's
// own keys included.
func TestMcpbRepeatedKeysNameEachPathOnce(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want []string
	}{
		{`{"a": [{"b": 1, "b": 2, "b": 3}], "c": {"d": 1}, "d": 1}`, []string{"a[0].b"}},
		{`{"x": {"y": 1}, "x": {"y": 1, "y": 2}}`, []string{"x", "x.y"}},
		{`{"server": {"type": "binary"}, "user_config": {"type": {"type": "boolean"}}}`, nil},
		{`{"command": 1, "Command": 2, "COMMAND": 3}`, []string{"Command"}},
		{`{"kind": 1, "\u212aind": 2}`, []string{"[\"\u212aind\"]"}},
		{`{"env": {"a.b": 1, "a.b": 2}, "a": {"b": 1, "b": 2}}`, []string{`env["a.b"]`, "a.b"}},
		{`{"env": {"a[0]": 1, "a[0]": 2}}`, []string{`env["a[0]"]`}},
	} {
		got, err := mcpbRepeatedKeys([]byte(tc.doc))
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("mcpbRepeatedKeys(%s) = %q, %v; want %q", tc.doc, got, err, tc.want)
		}
	}
	// A document the walk cannot read is refused, not passed as one with
	// no repeats.
	if got, err := mcpbRepeatedKeys([]byte(`{"a": 1,`)); err == nil {
		t.Errorf("an unfinished document gives %q and no error", got)
	}
}

// The platforms come from what the table stages an entry point for, not
// from a list of their own.
func TestMcpbPlatformsComeFromTheStagedTable(t *testing.T) {
	m := mcpbCommitted(t)
	if got := mcpbIdentityProblems(m, mcpbFiles); len(got) != 0 {
		t.Fatalf("the committed manifest: %q", got)
	}
	files := slices.DeleteFunc(slices.Clone(mcpbFiles), func(f mcpbStaged) bool { return f.platform == "linux" })
	want := []string{"compatibility.platforms is [darwin linux win32]; the packer stages entry points for [darwin win32]"}
	if got := mcpbIdentityProblems(m, files); !slices.Equal(got, want) {
		t.Errorf("nothing staged for Linux gives %q, want %q", got, want)
	}
}

// Every field that differs is named, an unexported one too, whatever
// else differs: a slice or an interface by its contents, without a
// panic, and a pointer by what it points to, never by its address.
func TestMcpbFieldDiffNamesTheFieldsThatDiffer(t *testing.T) {
	type cfg struct {
		Shown  int
		hidden int
		rest   []string
		held   any
		at     *int
		Shared *int
	}
	one, two, otherOne := 1, 2, 1
	base := cfg{1, 1, nil, []string{"x"}, &one, &one}
	for _, tc := range []struct {
		b    cfg
		want []string
	}{
		{cfg{2, 1, nil, []string{"x"}, &one, &one}, []string{"Shown"}},
		{cfg{1, 2, nil, []string{"x"}, &one, &one}, []string{"hidden"}},
		{cfg{1, 1, []string{"x"}, []string{"x"}, &one, &one}, []string{"rest"}},
		{cfg{2, 1, []string{"x"}, []string{"x"}, &one, &one}, []string{"Shown", "rest"}},
		{cfg{1, 1, nil, []string{"y"}, &one, &one}, []string{"held"}},
		{cfg{1, 1, nil, []string{"x"}, &otherOne, &otherOne}, nil},
		{cfg{1, 1, nil, []string{"x"}, &two, &two}, []string{"at", "Shared"}},
		{cfg{1, 1, nil, []string{"x"}, &one, &one}, nil},
	} {
		if got := mcpbFieldDiff(base, tc.b); !slices.Equal(got, tc.want) {
			t.Errorf("mcpbFieldDiff against %+v = %q, want %q", tc.b, got, tc.want)
		}
	}
	if got := mcpbFieldDiff(config.Config{Profile: "a", EnableSend: true}, config.Config{Profile: "a"}); !slices.Equal(got,
		[]string{"EnableSend"}) {
		t.Errorf("a configuration differing in EnableSend gives %q, want [EnableSend]", got)
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
