package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sink is an io.Writer a test can make assertions about. Every gate says
// how much it read, and that sentence is the only difference between
// "found nothing" and "looked at nothing".
type sink struct{ strings.Builder }

func (s *sink) mustSay(t *testing.T, want string) {
	t.Helper()
	if !strings.Contains(s.String(), want) {
		t.Errorf("the gate printed %q, which does not say %q", s.String(), want)
	}
}

// writeTree writes files (slash-separated path → content) under a fresh
// temporary directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, files)
	return root
}

// writeFiles writes files (slash-separated path → content) under root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// gitQuiet is the command-scope config every git a test runs carries,
// which every config file yields to
// (https://git-scm.com/docs/git-config#Documentation/git-config.txt-GITCONFIGCOUNT):
// no command starts `git maintenance run --auto` after a write
// (https://git-scm.com/docs/git-maintenance#Documentation/git-maintenance.txt-maintenanceauto),
// and a command that runs `git gc --auto` itself finds no work
// (https://git-scm.com/docs/git-gc#Documentation/git-gc.txt-gcauto).
// Automatic maintenance runs detached and holds .git/objects/maintenance.lock
// after git returns, so a copy of the repository races it
// (https://github.com/git/git/blob/v2.51.0/builtin/gc.c#L1663).
var gitQuiet = []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=maintenance.auto", "GIT_CONFIG_VALUE_0=false",
	"GIT_CONFIG_KEY_1=gc.auto", "GIT_CONFIG_VALUE_1=0"}

// gitIn runs git in dir as a fixture identity, reading none of the
// machine's configuration: no signing key, no hooks, no default branch
// of its own.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, gitQuiet...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// isolateGit keeps the machine's git configuration from every git the
// test runs, the gate's own included, so a global ignore file cannot hide
// a fixture file, and turns automatic maintenance off as gitQuiet does.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, kv := range gitQuiet {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
}

// gitRepo is writeTree's files committed to a fresh repository.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := writeTree(t, files)
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "first")
	return root
}
