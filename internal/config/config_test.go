package config

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mmedum/google-mail-mcp/v2/internal/credentials"
	"github.com/mmedum/google-mail-mcp/v2/internal/scopes"
)

// isolate points the home directory at a temporary one, so the config
// directory check has a home to compare against and nothing real is
// touched.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	// os.UserConfigDir reads %AppData% on Windows, not the home.
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	return home
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func build(t *testing.T, env map[string]string, args ...string) (Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s := Define(fs, envOf(env))
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return s.Build()
}

func TestDefaults(t *testing.T) {
	home := isolate(t)
	c, err := build(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile != "default" || c.ReadOnly || c.EnableSend || c.EnableDestructive || c.LocalDir != "" {
		t.Errorf("defaults = %+v", c)
	}
	if c.LogLevel != LogInfo || c.LogFormat != LogText {
		t.Errorf("log defaults = %s/%s", c.LogLevel, c.LogFormat)
	}
	if c.HTTPTimeout != 60*time.Second {
		t.Errorf("timeout = %s", c.HTTPTimeout)
	}
	if c.APIBase != DefaultAPIBase {
		t.Errorf("api base = %s", c.APIBase)
	}
	if !strings.HasPrefix(string(c.ConfigDir), home) {
		t.Errorf("config dir %s is not under home %s", c.ConfigDir, home)
	}
	if !slices.Equal(c.Scopes(), []string{scopes.Modify}) {
		t.Errorf("default scopes = %v", c.Scopes())
	}
}

func TestFlagBeatsEnvironment(t *testing.T) {
	isolate(t)
	c, err := build(t, map[string]string{"GMAIL_PROFILE": "work"}, "-profile", "home")
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile != "home" {
		t.Errorf("profile = %q", c.Profile)
	}
}

func TestEnvironmentSettings(t *testing.T) {
	home := isolate(t)
	dir := t.TempDir()
	c, err := build(t, map[string]string{
		"GMAIL_ENABLE_SEND":        "true",
		"GMAIL_ENABLE_DESTRUCTIVE": "yes",
		"GMAIL_LOCAL_DIR":          dir + string(filepath.Separator),
		"GMAIL_LOG_LEVEL":          "DEBUG",
		"GMAIL_LOG_FORMAT":         "json",
		"GMAIL_HTTP_TIMEOUT":       "5s",
		"GMAIL_API_BASE":           "http://127.0.0.1:8080/",
		"GMAIL_CONFIG_DIR":         filepath.Join(home, "cfg"),
		"GMAIL_CLIENT_SECRET":      " /x/client.json ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c.EnableSend || !c.EnableDestructive || c.LocalDir != filepath.Clean(dir) {
		t.Errorf("flags = %+v", c)
	}
	if c.LogLevel != LogDebug || c.LogFormat != LogJSON || c.HTTPTimeout != 5*time.Second {
		t.Errorf("logging/timeout = %+v", c)
	}
	if c.APIBase != "http://127.0.0.1:8080" || string(c.ConfigDir) != filepath.Join(home, "cfg") {
		t.Errorf("base/dir = %s %s", c.APIBase, c.ConfigDir)
	}
	if c.ClientSecretPath != "/x/client.json" {
		t.Errorf("client secret = %q", c.ClientSecretPath)
	}
	if !slices.Equal(c.Scopes(), []string{scopes.Full}) {
		t.Errorf("destructive scopes = %v", c.Scopes())
	}
}

// §9.4: read-only with an enable flag is refused, naming both.
func TestReadOnlyWithAnEnableFlagIsRefused(t *testing.T) {
	isolate(t)
	for _, flagName := range []string{"GMAIL_ENABLE_SEND", "GMAIL_ENABLE_DESTRUCTIVE"} {
		_, err := build(t, map[string]string{"GMAIL_READ_ONLY": "true", flagName: "true"})
		if err == nil {
			t.Fatalf("READ_ONLY with %s was accepted", flagName)
		}
		if !strings.Contains(err.Error(), "GMAIL_READ_ONLY") || !strings.Contains(err.Error(), flagName) {
			t.Errorf("error does not name both: %v", err)
		}
	}
	c, err := build(t, map[string]string{"GMAIL_READ_ONLY": "true"})
	if err != nil || !c.ReadOnly || !slices.Equal(c.Scopes(), []string{scopes.Readonly}) {
		t.Errorf("read-only alone: %+v, %v", c, err)
	}
}

func TestEveryProblemIsReported(t *testing.T) {
	home := isolate(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string]string{
		"profile":        {"GMAIL_PROFILE": "Bad Name!"},
		"bool":           {"GMAIL_READ_ONLY": "maybe"},
		"relative dir":   {"GMAIL_LOCAL_DIR": "attachments"},
		"missing dir":    {"GMAIL_LOCAL_DIR": filepath.Join(home, "nope")},
		"file as dir":    {"GMAIL_LOCAL_DIR": file},
		"level":          {"GMAIL_LOG_LEVEL": "loud"},
		"format":         {"GMAIL_LOG_FORMAT": "xml"},
		"timeout":        {"GMAIL_HTTP_TIMEOUT": "soon"},
		"timeout range":  {"GMAIL_HTTP_TIMEOUT": "1h"},
		"base http":      {"GMAIL_API_BASE": "http://example.com"},
		"base scheme":    {"GMAIL_API_BASE": "ftp://example.com"},
		"base path":      {"GMAIL_API_BASE": "https://example.com/gmail"},
		"base relative":  {"GMAIL_API_BASE": "example"},
		"config outside": {"GMAIL_CONFIG_DIR": t.TempDir()},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := build(t, env); !errors.Is(err, ErrInvalid) {
				t.Errorf("accepted: %v", err)
			}
		})
	}
	// Two problems at once are both named.
	_, err := build(t, map[string]string{"GMAIL_LOG_LEVEL": "loud", "GMAIL_LOG_FORMAT": "xml"})
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") || !strings.Contains(err.Error(), "LOG_FORMAT") {
		t.Errorf("joined errors: %v", err)
	}
}

func TestLoad(t *testing.T) {
	isolate(t)
	c, err := Load([]string{"-read-only"}, envOf(nil))
	if err != nil || !c.ReadOnly {
		t.Errorf("Load = %+v, %v", c, err)
	}
	if _, err := Load([]string{"-unknown"}, envOf(nil)); err == nil {
		t.Error("unknown flag accepted")
	}
}

func TestEnvVars(t *testing.T) {
	got := EnvVars()
	want := []string{
		"GMAIL_API_BASE", "GMAIL_CLIENT_SECRET", "GMAIL_CONFIG_DIR", "GMAIL_ENABLE_DESTRUCTIVE",
		"GMAIL_ENABLE_SEND", "GMAIL_ENABLE_SETTINGS", "GMAIL_HTTP_TIMEOUT", "GMAIL_LOCAL_DIR", "GMAIL_LOG_FORMAT",
		"GMAIL_LOG_LEVEL", "GMAIL_PROFILE", "GMAIL_READ_ONLY", "GMAIL_REFRESH_TOKEN", "GMAIL_REQUIRE_PROMPT",
	}
	if !slices.Equal(got, want) {
		t.Errorf("EnvVars =\n%v\nwant\n%v", got, want)
	}
	// The credential store reads its variable itself; the two names must
	// be one.
	if !slices.Contains(got, credentials.EnvVar) {
		t.Errorf("EnvVars misses %s", credentials.EnvVar)
	}
}

func TestLogLevelSlog(t *testing.T) {
	for _, l := range []LogLevel{LogDebug, LogInfo, LogWarn, LogError} {
		if l.Slog().String() != strings.ToUpper(string(l)) {
			t.Errorf("%s -> %s", l, l.Slog())
		}
	}
}

func TestNewLoggerWritesWhereItIsTold(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(Config{LogLevel: LogInfo, LogFormat: LogJSON}, &buf).Info("hello")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Errorf("json logger wrote %q", buf.String())
	}
	buf.Reset()
	NewLogger(Config{LogLevel: LogInfo, LogFormat: LogText}, &buf).Info("hello")
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("text logger wrote %q", buf.String())
	}
}

// The Claude Desktop bundle passes an unset option as an empty variable
// and a switch as the text "true" or "false". Empty means unset.
func TestEmptyValuesMeanUnset(t *testing.T) {
	isolate(t)
	c, err := build(t, map[string]string{
		"GMAIL_LOCAL_DIR": "", "GMAIL_CLIENT_SECRET": "", "GMAIL_CONFIG_DIR": "",
		"GMAIL_READ_ONLY": "false", "GMAIL_ENABLE_SEND": "", "GMAIL_ENABLE_DESTRUCTIVE": "false",
		"GMAIL_PROFILE": "", "GMAIL_LOG_LEVEL": "", "GMAIL_HTTP_TIMEOUT": "", "GMAIL_API_BASE": "",
	})
	if err != nil {
		t.Fatalf("empty values refused: %v", err)
	}
	if c.LocalDir != "" || c.ClientSecretPath != "" || c.ReadOnly || c.EnableSend || c.Profile != "default" {
		t.Errorf("empty values = %+v", c)
	}
	// Whitespace alone is also unset.
	if c, err := build(t, map[string]string{"GMAIL_LOCAL_DIR": "  "}); err != nil || c.LocalDir != "" {
		t.Errorf("blank local dir: %+v, %v", c, err)
	}
}
