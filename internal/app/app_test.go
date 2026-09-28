package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/mmedum/google-mail-mcp/v2/internal/config"
	"github.com/mmedum/google-mail-mcp/v2/internal/credentials"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/scopes"
	"github.com/mmedum/google-mail-mcp/v2/internal/userconfig"
)

// memKeyring keeps every test in this package off the OS keyring.
type memKeyring map[string]string

func (m memKeyring) Get(s, a string) (string, error) {
	if v, ok := m[s+"/"+a]; ok {
		return v, nil
	}
	return "", keyring.ErrNotFound
}
func (m memKeyring) Set(s, a, v string) error { m[s+"/"+a] = v; return nil }
func (m memKeyring) Delete(s, a string) error { delete(m, s+"/"+a); return nil }

func setup(t *testing.T, env map[string]string) (config.Config, func(string) string) {
	t.Helper()
	dir := t.TempDir()
	lookup := func(k string) string { return env[k] }
	cfg := config.Config{Profile: "default", ConfigDir: userconfig.Dir(dir), APIBase: "http://127.0.0.1:1", HTTPTimeout: 1e9}
	return cfg, lookup
}

func writeClient(t *testing.T, cfg config.Config) {
	t.Helper()
	p := cfg.ConfigDir.DefaultClientSecretPath(cfg.Profile)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"installed":{"client_id":"id","client_secret":"s"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAssembleWithoutCredentialsStillServes(t *testing.T) {
	cfg, env := setup(t, nil)
	r, err := Assemble(context.Background(), cfg, Options{Env: env, Keyring: memKeyring{}})
	if err != nil {
		t.Fatal(err)
	}
	if r.CredentialsErr == nil || r.TokenSource != nil {
		t.Fatalf("no client JSON, yet credentials resolved: %+v", r)
	}
	err = r.Client.Do(context.Background(), gapi.Call{ID: "gmail.users.getProfile", Method: http.MethodGet, Path: "profile"}, nil)
	if c, _ := gapi.ClassOf(err); c != gapi.ClassAuth {
		t.Errorf("a call before login: %v", err)
	}
	if r.Server("test") == nil {
		t.Fatal("no server")
	}
}

func TestAssembleWithAnEnvironmentToken(t *testing.T) {
	cfg, env := setup(t, map[string]string{credentials.EnvVar: "1//refresh-token-from-env"})
	writeClient(t, cfg)
	r, err := Assemble(context.Background(), cfg, Options{Env: env, Keyring: memKeyring{}})
	if err != nil {
		t.Fatal(err)
	}
	if r.CredentialsErr != nil || r.TokenSource == nil || r.Source != credentials.SourceEnv {
		t.Fatalf("runtime = %v, %v", r, r.CredentialsErr)
	}
	for _, s := range []string{r.String(), fmt.Sprintf("%v", r), fmt.Sprintf("%+v", r)} {
		if strings.Contains(s, "refresh-token-from-env") {
			t.Errorf("the token is printable: %s", s)
		}
	}
	if !strings.Contains(r.String(), "token_source=env") {
		t.Errorf("String = %s", r.String())
	}
}

func TestAssembleFromTheKeyring(t *testing.T) {
	cfg, env := setup(t, nil)
	writeClient(t, cfg)
	kr := memKeyring{credentials.ServiceName + "/default": "1//from-keyring"}
	r, err := Assemble(context.Background(), cfg, Options{Env: env, Keyring: kr})
	if err != nil || r.Source != credentials.SourceKeyring {
		t.Fatalf("%v, %v", r, err)
	}
}

func TestAssembleRefusesAnUnreadableProfile(t *testing.T) {
	cfg, env := setup(t, nil)
	if err := os.WriteFile(cfg.ConfigDir.ConfigPath(cfg.Profile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Assemble(context.Background(), cfg, Options{Env: env, Keyring: memKeyring{}}); err == nil {
		t.Fatal("a broken profile file was accepted")
	}
}

func TestScopeWarning(t *testing.T) {
	cfg, env := setup(t, nil)
	cfg.EnableDestructive = true
	if err := cfg.ConfigDir.Save(cfg.Profile, userconfig.Config{Scopes: []string{scopes.Modify}}); err != nil {
		t.Fatal(err)
	}
	r, err := Assemble(context.Background(), cfg, Options{Env: env, Keyring: memKeyring{}})
	if err != nil {
		t.Fatal(err)
	}
	if w := r.ScopeWarning(); !strings.Contains(w, scopes.Full) || !strings.Contains(w, "login") {
		t.Errorf("warning = %q", w)
	}
	cfg.EnableDestructive = false
	r, _ = Assemble(context.Background(), cfg, Options{Env: env, Keyring: memKeyring{}})
	if w := r.ScopeWarning(); w != "" {
		t.Errorf("covered scopes warned: %q", w)
	}
	// No login stored: not a scope problem.
	p := &Profile{Config: cfg}
	if p.MissingScopes() != nil {
		t.Error("no login reported missing scopes")
	}
}

func TestOpenProfileExpectsTheKeyringItUsed(t *testing.T) {
	cfg, env := setup(t, nil)
	if err := cfg.ConfigDir.Save(cfg.Profile, userconfig.Config{TokenStore: "keyring", ClientSecretPath: "/stored.json"}); err != nil {
		t.Fatal(err)
	}
	p, err := OpenProfile(cfg, memKeyring{}, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Store.ExpectKeyring || p.ClientSecretPath != "/stored.json" || !p.HasUser {
		t.Errorf("profile = %+v", p)
	}
	if _, _, err := p.Store.Resolve(); !errors.Is(err, credentials.ErrKeyringSilent) {
		t.Errorf("a silent keyring: %v", err)
	}
}
