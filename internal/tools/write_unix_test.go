//go:build unix

package tools_test

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
)

// A named pipe in GMAIL_LOCAL_DIR is refused before it is opened, which
// would wait for a writer that never comes.
func TestANamedPipeIsNotAttached(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Skip("no mkfifo here:", err)
	}
	h, _ := connectFake(t, config.Config{LocalDir: dir})
	done := make(chan struct{})
	go func() {
		defer close(done)
		refused(t, h, "create_draft", map[string]any{"body": "x", "attachments": []any{"pipe"}}, gapi.ClassInvalid)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("create_draft hung on a named pipe")
	}
}
