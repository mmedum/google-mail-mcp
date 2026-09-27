// Package userconfig stores non-secret, per-profile state between runs:
// where the OAuth client JSON lives, which account signed in, where the
// refresh token went and which scopes Google granted.
//
// It lives under a base directory — os.UserConfigDir()/google-mail-mcp
// unless GMAIL_CONFIG_DIR names another inside the home directory — and
// a non-default profile lives under profiles/<name>/ below it. The
// refresh token is internal/credentials' business, not this package's.
package userconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mmedum/google-mail-mcp/internal/fileperm"
)

// AppDir is the directory name under the user's config directory.
const AppDir = "google-mail-mcp"

// DefaultProfile is the profile used when none is named.
const DefaultProfile = "default"

// ErrNotFound means the profile has no config file yet.
var ErrNotFound = errors.New("userconfig: no config file for this profile; run `google-mail-mcp login`")

// ErrOutsideHome means the config directory override resolves outside
// the home directory.
var ErrOutsideHome = errors.New("userconfig: the config directory must be inside your home directory")

// Config is the stored, non-secret profile state.
type Config struct {
	ClientSecretPath string `json:"client_secret_path,omitempty"`
	AccountEmail     string `json:"account_email,omitempty"`
	TokenStore       string `json:"token_store,omitempty"`
	// Scopes is what Google granted at the last login, not what was
	// requested. A scope requested and refused is the failure worth
	// seeing, and storing the request would hide it.
	Scopes    []string  `json:"scopes,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// userConfigDir and userHomeDir are the os functions, replaceable in
// tests.
var (
	userConfigDir = os.UserConfigDir
	userHomeDir   = os.UserHomeDir
)

// BaseDir resolves the application directory. An empty override means
// the platform default.
//
// An override is refused outside the home directory, compared by real
// path. This package creates the directory 0700 and writes 0600 files
// into it, so a mistyped value such as /etc or another user's directory
// would re-permission something that is not ours.
func BaseDir(override string) (string, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		d, err := userConfigDir()
		if err != nil {
			return "", fmt.Errorf("userconfig: locate the user config directory: %w", err)
		}
		return filepath.Join(d, AppDir), nil
	}
	abs, err := filepath.Abs(override)
	if err != nil {
		return "", fmt.Errorf("userconfig: resolve %q: %w", override, err)
	}
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: the home directory cannot be found (%w)", ErrOutsideHome, err)
	}
	if !withinDir(realPath(home), realPath(abs)) {
		return "", fmt.Errorf("%w: %q", ErrOutsideHome, override)
	}
	return abs, nil
}

// realPath resolves links as far as the file system can, so that two
// names for one directory compare equal: macOS temporary paths under
// /var are /private/var, and Windows hands out 8.3 short names. The
// directory usually does not exist yet, so the deepest existing
// ancestor is resolved and the rest appended.
func realPath(path string) string {
	rest := ""
	for cur := path; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// withinDir reports whether path is dir or sits under it.
func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Dir is one resolved base directory. Every path this package hands out
// is under it.
type Dir string

// Profile returns the directory holding one profile's files.
func (d Dir) Profile(profile string) string {
	if profile == "" || profile == DefaultProfile {
		return string(d)
	}
	return filepath.Join(string(d), "profiles", profile)
}

// ConfigPath is the profile's config file.
func (d Dir) ConfigPath(profile string) string {
	return filepath.Join(d.Profile(profile), "config.json")
}

// DefaultClientSecretPath is where login looks for the OAuth client
// JSON when none is named.
func (d Dir) DefaultClientSecretPath(profile string) string {
	return filepath.Join(d.Profile(profile), "client_secret.json")
}

// TokenFilePath is the plaintext fallback for the refresh token.
func (d Dir) TokenFilePath(profile string) string {
	return filepath.Join(d.Profile(profile), "token.json")
}

// ResolveClientSecretPath picks the OAuth client JSON: the override
// when given, then what the profile remembers, then the default. One
// answer in one place, because a chain written out at each call site
// drifts between the copies.
func (d Dir) ResolveClientSecretPath(profile, override string) (string, error) {
	if v := strings.TrimSpace(override); v != "" {
		return v, nil
	}
	c, err := d.Load(profile)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if v := strings.TrimSpace(c.ClientSecretPath); v != "" {
		return v, nil
	}
	return d.DefaultClientSecretPath(profile), nil
}

// Load reads the profile's config. ErrNotFound if absent.
func (d Dir) Load(profile string) (Config, error) {
	p := d.ConfigPath(profile)
	data, err := os.ReadFile(p) //nolint:gosec // a path composed here from the validated profile name
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNotFound
	}
	if err != nil {
		return Config{}, fmt.Errorf("userconfig: read %s: %w", p, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("userconfig: parse %s: %w", p, err)
	}
	return c, nil
}

// Save writes the profile's config, restricted to the current account.
func (d Dir) Save(profile string, c Config) error {
	p := d.ConfigPath(profile)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("userconfig: create %s: %w", filepath.Dir(p), err)
	}
	c.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("userconfig: encode: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("userconfig: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("userconfig: replace %s: %w", p, err)
	}
	// The file holds the account's address. After the rename, because on
	// Windows the access list belongs to the file at its final path.
	return fileperm.RestrictToOwner(p)
}

// Remove deletes the profile's config file. A missing file is fine.
func (d Dir) Remove(profile string) error {
	p := d.ConfigPath(profile)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("userconfig: remove %s: %w", p, err)
	}
	return nil
}

// Profiles lists every configured profile, the default included.
func (d Dir) Profiles() ([]string, error) {
	var out []string
	if _, err := os.Stat(d.ConfigPath(DefaultProfile)); err == nil {
		out = append(out, DefaultProfile)
	}
	entries, err := os.ReadDir(filepath.Join(string(d), "profiles"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("userconfig: list profiles: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(d.ConfigPath(e.Name())); err == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// SharingGrant returns the other profiles that may hold the same grant
// as this one: the same account through the same Cloud project. Google
// revokes the grant, not one token, and the grant spans every client in
// the project, so revoking here signs those out too; logout says so
// first. project names the Cloud project of a client secret path, or ""
// when it cannot tell. Only a difference both sides know rules a
// profile out, so an unknown account or project errs toward warning.
func (d Dir) SharingGrant(profile string, project func(clientSecretPath string) string) ([]string, error) {
	mine, err := d.Load(profile)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(mine.ClientSecretPath) == "" {
		return nil, nil
	}
	myProject := project(mine.ClientSecretPath)
	names, err := d.Profiles()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		if name == profile {
			continue
		}
		other, err := d.Load(name)
		if err != nil || strings.TrimSpace(other.ClientSecretPath) == "" {
			continue
		}
		if knownDifferent(mine.AccountEmail, other.AccountEmail, strings.EqualFold) {
			continue
		}
		if other.ClientSecretPath != mine.ClientSecretPath &&
			knownDifferent(myProject, project(other.ClientSecretPath), func(a, b string) bool { return a == b }) {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// knownDifferent reports whether a and b are both known and not equal.
func knownDifferent(a, b string, equal func(a, b string) bool) bool {
	return a != "" && b != "" && !equal(a, b)
}
