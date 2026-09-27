package userconfig

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	prevHome, prevCfg := userHomeDir, userConfigDir
	userHomeDir = func() (string, error) { return home, nil }
	userConfigDir = func() (string, error) { return filepath.Join(home, ".config"), nil }
	t.Cleanup(func() { userHomeDir, userConfigDir = prevHome, prevCfg })
	return home
}

func TestBaseDirDefault(t *testing.T) {
	home := fakeHome(t)
	got, err := BaseDir("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", AppDir); got != want {
		t.Errorf("BaseDir = %q, want %q", got, want)
	}
}

func TestBaseDirOverrideInsideHome(t *testing.T) {
	home := fakeHome(t)
	want := filepath.Join(home, "not", "yet", "created")
	got, err := BaseDir(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("BaseDir = %q, want %q", got, want)
	}
}

func TestBaseDirRefusesOutsideHome(t *testing.T) {
	home := fakeHome(t)
	for _, v := range []string{t.TempDir(), filepath.Join(home, "..", "elsewhere")} {
		if _, err := BaseDir(v); !errors.Is(err, ErrOutsideHome) {
			t.Errorf("BaseDir(%q) = %v, want ErrOutsideHome", v, err)
		}
	}
}

// A link inside home pointing outside it is outside home.
func TestBaseDirRefusesALinkOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home := fakeHome(t)
	link := filepath.Join(home, "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := BaseDir(filepath.Join(link, "cfg")); !errors.Is(err, ErrOutsideHome) {
		t.Errorf("a link out of home was accepted: %v", err)
	}
}

func TestBaseDirErrors(t *testing.T) {
	prevHome, prevCfg := userHomeDir, userConfigDir
	t.Cleanup(func() { userHomeDir, userConfigDir = prevHome, prevCfg })
	userHomeDir = func() (string, error) { return "", errors.New("no home") }
	userConfigDir = func() (string, error) { return "", errors.New("no config dir") }
	if _, err := BaseDir(""); err == nil {
		t.Error("no config dir was not reported")
	}
	if _, err := BaseDir("/x"); !errors.Is(err, ErrOutsideHome) {
		t.Errorf("no home: %v", err)
	}
}

func TestPaths(t *testing.T) {
	d := Dir("/base")
	if got := d.Profile(""); got != "/base" {
		t.Errorf("default profile dir = %q", got)
	}
	if got := d.Profile("work"); got != filepath.Join("/base", "profiles", "work") {
		t.Errorf("work profile dir = %q", got)
	}
	if got := d.TokenFilePath("default"); got != filepath.Join("/base", "token.json") {
		t.Errorf("token path = %q", got)
	}
}

func TestSaveLoadRemove(t *testing.T) {
	d := Dir(t.TempDir())
	if _, err := d.Load("work"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load before Save = %v", err)
	}
	in := Config{ClientSecretPath: "/x/client.json", AccountEmail: "a@example.com", TokenStore: "keyring", Scopes: []string{"s"}}
	if err := d.Save("work", in); err != nil {
		t.Fatal(err)
	}
	got, err := d.Load("work")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountEmail != in.AccountEmail || got.UpdatedAt.IsZero() || !slices.Equal(got.Scopes, in.Scopes) {
		t.Errorf("round trip = %+v", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(d.ConfigPath("work"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("config mode %o is readable by others", fi.Mode().Perm())
		}
	}
	if err := d.Remove("work"); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove("work"); err != nil {
		t.Errorf("removing twice: %v", err)
	}
}

func TestLoadRejectsBadJSON(t *testing.T) {
	d := Dir(t.TempDir())
	if err := os.WriteFile(d.ConfigPath(""), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Load(""); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("bad JSON: %v", err)
	}
}

func TestResolveClientSecretPath(t *testing.T) {
	d := Dir(t.TempDir())
	got, err := d.ResolveClientSecretPath("default", "")
	if err != nil || got != d.DefaultClientSecretPath("default") {
		t.Errorf("default = %q, %v", got, err)
	}
	if err := d.Save("default", Config{ClientSecretPath: "/stored.json"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.ResolveClientSecretPath("default", ""); got != "/stored.json" {
		t.Errorf("stored = %q", got)
	}
	if got, _ := d.ResolveClientSecretPath("default", " /flag.json "); got != "/flag.json" {
		t.Errorf("override = %q", got)
	}
}

func TestProfilesAndSharingClient(t *testing.T) {
	d := Dir(t.TempDir())
	if got, err := d.Profiles(); err != nil || len(got) != 0 {
		t.Fatalf("empty dir: %v, %v", got, err)
	}
	for name, secret := range map[string]string{"default": "/a.json", "work": "/a.json", "home": "/b.json"} {
		if err := d.Save(name, Config{ClientSecretPath: secret}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(string(d), "profiles", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(string(d), "profiles", "stray"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := d.Profiles()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"default", "home", "work"}) {
		t.Errorf("Profiles = %v", names)
	}
	others, err := d.SharingClient("default")
	if err != nil || !slices.Equal(others, []string{"work"}) {
		t.Errorf("SharingClient = %v, %v", others, err)
	}
	if err := d.Save("bare", Config{}); err != nil {
		t.Fatal(err)
	}
	if others, err := d.SharingClient("bare"); err != nil || others != nil {
		t.Errorf("no client path: %v, %v", others, err)
	}
	if _, err := d.SharingClient("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown profile: %v", err)
	}
}
