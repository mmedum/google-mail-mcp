package main

import (
	"os"
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
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
