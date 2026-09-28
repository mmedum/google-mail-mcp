package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
)

func TestDownloadAttachment(t *testing.T) {
	s, fake := newService(t)
	dir := t.TempDir()
	sc := fake.Scenario(gmailtest.ScenarioInternational)
	ctx := context.Background()

	d, err := s.DownloadAttachment(ctx, dir, sc.MessageIDs[0], "1")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("%PDF-1.4 generated fixture\n")
	got, err := os.ReadFile(filepath.Join(dir, "résumé.pdf"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file = %q, %v; want the attachment's bytes under its decoded name", got, err)
	}
	sum := sha256.Sum256(want)
	if filepath.Base(d.Path) != "résumé.pdf" || d.Bytes != int64(len(want)) || d.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("download = %+v", d)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(d.Path); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v; want 0600", fi.Mode().Perm())
		}
	}

	// The same attachment again is never written over the first.
	if err := os.WriteFile(filepath.Join(dir, "résumé-1.pdf"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := s.DownloadAttachment(ctx, dir, sc.MessageIDs[0], "1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(again.Path) != "résumé-2.pdf" || !again.Suffixed || d.Suffixed {
		t.Errorf("second download named %q; want résumé-2.pdf past two taken names", filepath.Base(again.Path))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "résumé-1.pdf")); string(b) != "mine" {
		t.Errorf("an existing file was overwritten: %q", b)
	}
}

// A declared name that climbs out of the directory and hides its
// extension behind a bidi override lands in the directory, renamed.
func TestDownloadAttachmentKeepsAnUnsafeNameInTheDirectory(t *testing.T) {
	s, fake := newService(t)
	dir := t.TempDir()
	sc := fake.Scenario(gmailtest.ScenarioInternational)
	d, err := s.DownloadAttachment(context.Background(), dir, sc.MessageIDs[2], "1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(d.Path) != dir || filepath.Base(d.Path) != "invoicetxt.exe" || !d.Attachment.Renamed {
		t.Fatalf("download = %+v; want invoicetxt.exe inside the directory, marked renamed", d)
	}
	entries, _ := os.ReadDir(filepath.Dir(dir))
	for _, e := range entries {
		if e.Name() != filepath.Base(dir) {
			t.Errorf("a file appeared beside the directory: %s", e.Name())
		}
	}
}

func TestDownloadAttachmentStreamsALargeFile(t *testing.T) {
	s, fake := newService(t)
	dir := t.TempDir()
	content := bytes.Repeat([]byte("large attachment "), 1<<16)
	id, part := fake.AddAttachmentMessage("large.bin", content)
	d, err := s.DownloadAttachment(context.Background(), dir, id, part)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(d.Path)
	if !bytes.Equal(got, content) {
		t.Fatalf("wrote %d bytes; want %d", len(got), len(content))
	}
	if n := len(calls(fake, "gmail.users.messages.attachments.get")); n != 1 {
		t.Errorf("%d attachment reads; want 1", n)
	}
}

func TestDownloadAttachmentRefusals(t *testing.T) {
	s, fake := newService(t)
	dir := t.TempDir()
	sc := fake.Scenario(gmailtest.ScenarioInternational)
	ctx := context.Background()

	_, err := s.DownloadAttachment(ctx, "", sc.MessageIDs[0], "1")
	wantClass(t, err, gapi.ClassBlocked)
	// Part 0 is the body, not an attachment.
	_, err = s.DownloadAttachment(ctx, dir, sc.MessageIDs[0], "0")
	wantClass(t, err, gapi.ClassNotFound)
	_, err = s.DownloadAttachment(ctx, dir, "0000000000fffff0", "1")
	wantClass(t, err, gapi.ClassNotFound)

	fake.Fail(gmailtest.Failure{Method: "gmail.users.messages.attachments.get", Status: 500, Reason: "backendError", Times: 4})
	_, err = s.DownloadAttachment(ctx, dir, sc.MessageIDs[0], "1")
	wantClass(t, err, gapi.ClassUnavailable)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a failed download left %d files; want none", len(entries))
	}
}

func TestDownloadAttachmentByRFC822ID(t *testing.T) {
	s, fake := newService(t)
	sc := fake.Scenario(gmailtest.ScenarioInternational)
	d, err := s.DownloadAttachment(context.Background(), t.TempDir(),
		"rfc822:<fixture."+sc.MessageIDs[1]+"@mail.example.com>", "1")
	if err != nil {
		t.Fatal(err)
	}
	if d.MessageID != sc.MessageIDs[1] || filepath.Base(d.Path) != "会議メモ.txt" {
		t.Errorf("download = %+v", d)
	}
}

// An attachment over the cap is refused from its declared size, before
// the read that would spend quota and fill the disk.
func TestDownloadAttachmentRefusesAnOversizeAttachmentBeforeReadingIt(t *testing.T) {
	s, fake := newService(t)
	id, part := fake.AddAttachmentMessage("huge.bin", make([]byte, gapi.MaxAttachmentBytes+1))
	dir := t.TempDir()
	_, err := s.DownloadAttachment(context.Background(), dir, id, part)
	wantClass(t, err, gapi.ClassInvalid)
	if n := len(calls(fake, "gmail.users.messages.attachments.get")); n != 0 {
		t.Errorf("%d attachment reads; want none", n)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused download left %d files", len(entries))
	}
}
