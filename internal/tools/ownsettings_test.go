package tools_test

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/v3/internal/config"
	"github.com/mmedum/google-mail-mcp/v3/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v3/internal/server"
	"github.com/mmedum/google-mail-mcp/v3/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/v3/internal/tools"
	"github.com/mmedum/google-mail-mcp/v3/internal/userconfig"
)

// The model never widens its own reach: no tool turns a flag on, names
// another directory or profile, or changes what the server stored
// (standard §3). Its settings are the operator's, outside the
// conversation. So no tool takes an input named after a setting, and
// driving every tool, its guards set, leaves the environment, the
// profile directory and the home directory as they were.
func TestNoToolWritesTheServersOwnSettings(t *testing.T) {
	home, base := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, ".config"))
	dir := userconfig.Dir(base)
	for path, body := range map[string]string{
		dir.ConfigPath(userconfig.DefaultProfile):                        `{"scopes":[]}`,
		dir.DefaultClientSecretPath(userconfig.DefaultProfile):           `{"installed":{}}`,
		dir.TokenFilePath(userconfig.DefaultProfile):                     `{"refresh_token":"x"}`,
		filepath.Join(home, ".config", "google-mail-mcp", "config.json"): `{}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	trees := []string{home, base}
	before := snapshot(t, trees)
	if len(before) < 4 {
		t.Fatalf("read %d files before; this test would pass on an empty directory", len(before))
	}
	env := os.Environ()

	gmail, _ := canaryGmail(t)
	cfg := tools.FullSurface(config.Config{LocalDir: t.TempDir(), APIBase: gmail.URL, ConfigDir: dir,
		Profile: userconfig.DefaultProfile})
	client := gapi.New(gapi.Options{BaseURL: gmail.URL, MaxRetries: -1,
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: canaryToken})})
	h := testutil.ConnectServer(t, server.New(server.Deps{Deps: tools.Deps{Config: cfg, Client: client}, Version: "test"}))

	settings := map[string]bool{}
	for _, v := range config.EnvVars() {
		settings[strings.ToLower(strings.TrimPrefix(v, config.EnvPrefix))] = true
	}
	surface, inputs := h.Tools(t), 0
	if len(surface) < 30 || len(settings) < 13 {
		t.Fatalf("%d tools and %d settings, below the floors of 30 and 13", len(surface), len(settings))
	}
	for _, tool := range surface {
		args := argsFor(tool)
		inputs += len(args)
		for name := range args {
			if settings[name] {
				t.Errorf("%s takes %s, which is one of the server's own settings", tool.Name, name)
			}
		}
		guarded := maps.Clone(args)
		for _, guard := range []string{"confirm", "enable"} {
			if _, ok := guarded[guard]; ok {
				guarded[guard] = true
			}
		}
		for _, a := range []map[string]any{args, guarded} {
			if _, err := h.Client.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.Name, Arguments: a}); err != nil {
				t.Fatalf("call %s: %v", tool.Name, err)
			}
		}
	}
	if inputs < 100 {
		t.Fatalf("read %d inputs, below the floor of 100", inputs)
	}
	if after := snapshot(t, trees); !maps.Equal(before, after) {
		t.Errorf("the tools changed the server's files:\nbefore %v\nafter  %v", slices.Sorted(maps.Keys(before)),
			slices.Sorted(maps.Keys(after)))
	}
	if after := os.Environ(); !slices.Equal(env, after) {
		t.Error("the tools changed the process's environment")
	}
}

// snapshot is every file under roots, by path, as its mode and content.
func snapshot(t *testing.T, roots []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = info.Mode().String() + " " + string(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}
