// Package config loads and validates the runtime configuration.
//
// Environment variables (GMAIL_*) are the source of truth, because an
// MCP client passes only command, args and env to a stdio server. Most
// settings also have a flag bound to the same name; a flag given on the
// command line wins over the environment, and the environment over the
// default. Build validates once and reports every problem together, so
// a misconfigured server fails before it announces itself.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mmedum/google-mail-mcp/internal/scopes"
	"github.com/mmedum/google-mail-mcp/internal/userconfig"
)

// EnvPrefix is prepended to every environment variable name.
const EnvPrefix = "GMAIL_"

// Variables read from the environment only. CONFIG_DIR has no flag
// because it is read the same way by every subcommand before anything
// else; REFRESH_TOKEN because a secret on a command line is visible to
// every process on the machine. internal/credentials reads the second.
const (
	EnvConfigDir    = EnvPrefix + "CONFIG_DIR"
	EnvRefreshToken = EnvPrefix + "REFRESH_TOKEN"
)

// envOnly is every variable read outside Define, for EnvVars.
var envOnly = []string{EnvConfigDir, EnvRefreshToken}

// DefaultAPIBase is the Gmail API origin. Paths go under
// /gmail/v1/users/me/.
const DefaultAPIBase = "https://gmail.googleapis.com"

// DefaultHTTPTimeout bounds one attempt at an API call.
const DefaultHTTPTimeout = 60 * time.Second

// LogLevel is a typed enum constrained at load time.
type LogLevel string

// Allowed LogLevel values.
const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// Slog returns the slog.Level for this level.
func (l LogLevel) Slog() slog.Level {
	switch l {
	case LogDebug:
		return slog.LevelDebug
	case LogWarn:
		return slog.LevelWarn
	case LogError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LogFormat is a typed enum constrained at load time.
type LogFormat string

// Allowed LogFormat values.
const (
	LogText LogFormat = "text"
	LogJSON LogFormat = "json"
)

// Config is the validated runtime configuration.
type Config struct {
	Profile string
	// ClientSecretPath overrides the profile's stored client JSON path.
	ClientSecretPath string
	// ReadOnly registers the Read kinds only and requests gmail.readonly.
	ReadOnly bool
	// EnableSend registers send_draft. It changes no scope (§9.4).
	EnableSend bool
	// EnableDestructive registers the permanent deletes and requests
	// https://mail.google.com/.
	EnableDestructive bool
	// EnableSettings registers the settings writes — signatures and
	// filters, and the vacation reply with EnableSend too — and requests
	// gmail.settings.basic, which no other scope covers (§9.4).
	EnableSettings bool
	// RequirePrompt refuses the writes that take confirm when the client
	// cannot put the question to the person (§4.13).
	RequirePrompt bool
	// LocalDir is the one directory attachments are written to. Empty
	// means no file transfer, and download_attachment is not registered.
	LocalDir    string
	LogLevel    LogLevel
	LogFormat   LogFormat
	HTTPTimeout time.Duration
	// ConfigDir is the resolved base directory for profiles.
	ConfigDir userconfig.Dir
	// APIBase is the Gmail API origin, overridden only by tests.
	APIBase string
}

// Scopes is what login requests under this configuration.
func (c Config) Scopes() []string {
	return scopes.ForMode(c.ReadOnly, c.EnableDestructive, c.EnableSettings)
}

// Settings holds the raw values before validation.
type Settings struct {
	Profile           string
	ClientSecretPath  string
	ReadOnly          string
	EnableSend        string
	EnableDestructive string
	EnableSettings    string
	RequirePrompt     string
	LocalDir          string
	LogLevel          string
	LogFormat         string
	HTTPTimeout       string
	APIBase           string
	ConfigDir         string
}

// Define registers one flag per setting on fs, each defaulting to its
// GMAIL_* variable read through env.
//
// EnvVars runs this against a recording lookup, so a setting added here
// is documented or the staleness gate fails.
func Define(fs *flag.FlagSet, env func(string) string) *Settings {
	s := &Settings{}
	def := func(p *string, name, key, fallback, usage string) {
		v := env(EnvPrefix + key)
		if v == "" {
			v = fallback
		}
		fs.StringVar(p, name, v, usage+" [env "+EnvPrefix+key+"]")
	}
	// A switch takes a bare --read-only as well as --read-only=true.
	// Build parses the text either way, so a bad value from the
	// environment is reported with the others rather than by the flag
	// package.
	defBool := func(p *string, name, key, usage string) {
		*p = env(EnvPrefix + key)
		if *p == "" {
			*p = "false"
		}
		fs.Var((*switchText)(p), name, usage+" [env "+EnvPrefix+key+"]")
	}
	def(&s.Profile, "profile", "PROFILE", userconfig.DefaultProfile, "named configuration profile")
	def(&s.ClientSecretPath, "client-secret", "CLIENT_SECRET", "", "path to the OAuth Desktop client JSON (overrides the stored profile setting)")
	defBool(&s.ReadOnly, "read-only", "READ_ONLY", "register only the read tools and request gmail.readonly")
	defBool(&s.EnableSend, "enable-send", "ENABLE_SEND", "register send_draft")
	defBool(&s.EnableDestructive, "enable-destructive", "ENABLE_DESTRUCTIVE",
		"register the permanent deletes and request https://mail.google.com/")
	defBool(&s.EnableSettings, "enable-settings", "ENABLE_SETTINGS",
		"register the signature, filter and vacation writes and request gmail.settings.basic")
	defBool(&s.RequirePrompt, "require-prompt", "REQUIRE_PROMPT",
		"refuse the writes that take confirm when the client cannot ask the person")
	def(&s.LocalDir, "local-dir", "LOCAL_DIR", "", "the one directory attachments are written to (unset turns file transfer off)")
	def(&s.LogLevel, "log-level", "LOG_LEVEL", string(LogInfo), "log level: debug, info, warn, error")
	def(&s.LogFormat, "log-format", "LOG_FORMAT", string(LogText), "log format: text, json")
	def(&s.HTTPTimeout, "http-timeout", "HTTP_TIMEOUT", DefaultHTTPTimeout.String(), "deadline for one attempt at a Google API call")
	def(&s.APIBase, "api-base", "API_BASE", DefaultAPIBase, "Gmail API origin; for tests")
	s.ConfigDir = env(EnvConfigDir)
	return s
}

// switchText is a flag.Value that holds its text for Build to parse and
// tells the flag package it may be given bare.
type switchText string

func (s *switchText) String() string {
	if s == nil {
		return ""
	}
	return string(*s)
}

func (s *switchText) Set(v string) error { *s = switchText(v); return nil }

// IsBoolFlag lets the flag be given without a value.
func (s *switchText) IsBoolFlag() bool { return true }

// EnvVars is every GMAIL_ variable this server reads, sorted. The
// Define part is recorded rather than listed, so it cannot fall behind.
func EnvVars() []string {
	var seen []string
	fs := flag.NewFlagSet("envvars", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	Define(fs, func(name string) string {
		seen = append(seen, name)
		return ""
	})
	seen = append(seen, envOnly...)
	slices.Sort(seen)
	return slices.Compact(seen)
}

var (
	profilePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	logLevels      = map[LogLevel]bool{LogDebug: true, LogInfo: true, LogWarn: true, LogError: true}
	logFormats     = map[LogFormat]bool{LogText: true, LogJSON: true}
)

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("config: invalid")

// Build validates the settings and returns a Config.
func (s *Settings) Build() (Config, error) {
	var c Config
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	c.Profile = strings.ToLower(strings.TrimSpace(s.Profile))
	if !profilePattern.MatchString(c.Profile) {
		add(fmt.Errorf("%w: profile %q must match %s", ErrInvalid, s.Profile, profilePattern))
	}
	c.ClientSecretPath = strings.TrimSpace(s.ClientSecretPath)

	var err error
	c.ReadOnly, err = parseBool("READ_ONLY", s.ReadOnly)
	add(err)
	c.EnableSend, err = parseBool("ENABLE_SEND", s.EnableSend)
	add(err)
	c.EnableDestructive, err = parseBool("ENABLE_DESTRUCTIVE", s.EnableDestructive)
	add(err)
	c.EnableSettings, err = parseBool("ENABLE_SETTINGS", s.EnableSettings)
	add(err)
	c.RequirePrompt, err = parseBool("REQUIRE_PROMPT", s.RequirePrompt)
	add(err)
	// Read-only with a write flag has no coherent meaning, and guessing
	// which one was meant would either drop a guard or a tool.
	for _, on := range []struct {
		set  bool
		name string
	}{{c.EnableSend, "ENABLE_SEND"}, {c.EnableDestructive, "ENABLE_DESTRUCTIVE"}, {c.EnableSettings, "ENABLE_SETTINGS"}} {
		if c.ReadOnly && on.set {
			add(fmt.Errorf("%w: %sREAD_ONLY and %s%s are both set; choose one", ErrInvalid, EnvPrefix, EnvPrefix, on.name))
		}
	}

	c.LocalDir, err = parseLocalDir(s.LocalDir)
	add(err)

	c.LogLevel = LogLevel(strings.ToLower(strings.TrimSpace(s.LogLevel)))
	if !logLevels[c.LogLevel] {
		add(fmt.Errorf("%w: %sLOG_LEVEL %q (want debug, info, warn, error)", ErrInvalid, EnvPrefix, s.LogLevel))
	}
	c.LogFormat = LogFormat(strings.ToLower(strings.TrimSpace(s.LogFormat)))
	if !logFormats[c.LogFormat] {
		add(fmt.Errorf("%w: %sLOG_FORMAT %q (want text, json)", ErrInvalid, EnvPrefix, s.LogFormat))
	}

	c.HTTPTimeout, err = parseTimeout(s.HTTPTimeout)
	add(err)
	c.APIBase, err = parseBase(s.APIBase)
	add(err)

	dir, err := userconfig.BaseDir(s.ConfigDir)
	if err != nil {
		add(fmt.Errorf("%w: %s: %w", ErrInvalid, EnvConfigDir, err))
	}
	c.ConfigDir = userconfig.Dir(dir)

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

// Load is Define, Parse and Build for a caller with no flags of its own.
func Load(args []string, env func(string) string) (Config, error) {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s := Define(fs, env)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return s.Build()
}

func parseBool(key, v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	}
	return false, fmt.Errorf("%w: %s%s %q (want true or false)", ErrInvalid, EnvPrefix, key, v)
}

// parseLocalDir refuses a relative path, because the working directory
// is wherever the client launched the server from, and a path that does
// not exist, because a typo accepted at start surfaces much later as a
// download failing for no visible reason.
func parseLocalDir(v string) (string, error) {
	dir := strings.TrimSpace(v)
	if dir == "" {
		return "", nil
	}
	name := EnvPrefix + "LOCAL_DIR"
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%w: %s %q must be an absolute path", ErrInvalid, name, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s %q does not exist or cannot be read: %w", ErrInvalid, name, dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s %q is not a directory", ErrInvalid, name, dir)
	}
	return filepath.Clean(dir), nil
}

func parseTimeout(v string) (time.Duration, error) {
	name := EnvPrefix + "HTTP_TIMEOUT"
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%w: %s %q: %w", ErrInvalid, name, v, err)
	}
	if d < time.Second || d > 10*time.Minute {
		return 0, fmt.Errorf("%w: %s %s must be between 1s and 10m", ErrInvalid, name, d)
	}
	return d, nil
}

// parseBase accepts an https origin, or http to a loopback address for
// a test server. Plain http anywhere else would send the access token
// in the clear.
func parseBase(v string) (string, error) {
	name := EnvPrefix + "API_BASE"
	raw := strings.TrimRight(strings.TrimSpace(v), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: %s %q is not an absolute URL", ErrInvalid, name, v)
	}
	if u.Path != "" || u.RawQuery != "" || u.User != nil {
		return "", fmt.Errorf("%w: %s %q must be an origin, with no path, query or user", ErrInvalid, name, v)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("%w: %s %q: plain http is allowed only to a loopback address", ErrInvalid, name, v)
		}
	default:
		return "", fmt.Errorf("%w: %s %q must be https", ErrInvalid, name, v)
	}
	return raw, nil
}

// NewLogger builds the process logger. w must be stderr on the server
// path: stdout carries only JSON-RPC frames.
func NewLogger(c Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel.Slog()}
	if c.LogFormat == LogJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
