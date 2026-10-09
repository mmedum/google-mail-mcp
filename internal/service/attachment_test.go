package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/service"
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

// A part Gmail inlined in the message is written from the message
// itself, with no attachment read.
func TestDownloadAttachmentWritesAnInlinedPart(t *testing.T) {
	s, fake := newService(t)
	sc := fake.Scenario(gmailtest.ScenarioInvite)
	d, err := s.DownloadAttachment(context.Background(), t.TempDir(), sc.MessageIDs[0], "0.2")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(d.Path)
	if d.Bytes != 271 || len(got) != 271 || !bytes.HasPrefix(got, []byte("BEGIN:VCALENDAR\r\n")) ||
		!bytes.Contains(got, []byte("SUMMARY:Design review\r\n")) {
		t.Fatalf("wrote %d bytes (reported %d): %q; want the 271-byte invitation", len(got), d.Bytes, got)
	}
	if n := len(calls(fake, "gmail.users.messages.attachments.get")); n != 0 {
		t.Errorf("%d attachment reads; want none", n)
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

// attachedMessage is an RFC 5322 message to attach as message/rfc822.
const attachedMessage = "From: Bruno Fennick <bruno.fennick@example.org>\r\nTo: Ada Quill <ada.quill@example.com>\r\n" +
	"Subject: The venue\r\nDate: Mon, 2 Mar 2026 09:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n\r\nThe lake house is booked.\r\n"

func names(ds []service.Download) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = filepath.Base(d.Path)
	}
	return out
}

// With no part named, an invitation's calendar version of the body is
// passed over: the same invitation is attached as invite.ics.
func TestDownloadAttachmentsPassesOverAnInvitationsCalendarBody(t *testing.T) {
	s, fake := newService(t)
	dir := t.TempDir()
	sc := fake.Scenario(gmailtest.ScenarioInvite)
	got, err := s.DownloadAttachments(context.Background(), dir, sc.MessageIDs[0], nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got.Files), []string{"invite.ics"}) || got.Files[0].Attachment.PartID != "1" ||
		!slices.Equal(got.Skipped, []service.Skipped{{PartID: "0.2", Reason: service.SkipInvitationCopy}}) || len(got.Failed) != 0 {
		t.Fatalf("got %+v; want invite.ics from part 1, and part 0.2 passed over as the invitation's copy", got)
	}
	if b, _ := os.ReadFile(got.Files[0].Path); !bytes.HasPrefix(b, []byte("BEGIN:VCALENDAR\r\n")) {
		t.Errorf("invite.ics holds %q", b)
	}
}

// An invitation carried only as a calendar version of the body is the
// only copy there is, so it is saved.
func TestDownloadAttachmentsSavesAnInvitationsOnlyCopy(t *testing.T) {
	s, fake := newService(t)
	cal := &gmailtest.Part{ContentType: "text/calendar; charset=utf-8; method=REQUEST", CTE: "7bit",
		Content: []byte("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nEND:VCALENDAR\r\n")}
	alt := &gmailtest.Part{ContentType: `multipart/alternative; boundary="alt-only"`, Boundary: "alt-only",
		Children: []*gmailtest.Part{{ContentType: "text/plain; charset=utf-8", Content: []byte("You are invited.\n")}, cal}}
	id := fake.AddPartsMessage(alt)
	got, err := s.DownloadAttachments(context.Background(), t.TempDir(), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got.Files), []string{"invite.ics"}) || len(got.Skipped) != 0 {
		t.Fatalf("got %+v; want the calendar part saved as invite.ics", got)
	}
}

// Inline parts are passed over unless include_inline, and an emoji
// reaction is passed over either way.
func TestDownloadAttachmentsInlineAndReactions(t *testing.T) {
	s, fake := newService(t)
	image := &gmailtest.Part{ContentType: "image/png", Disposition: "inline", CTE: "base64", ContentID: "logo@example.com",
		Content: []byte("PNG generated fixture"), Filename: "logo.png"}
	reaction := &gmailtest.Part{ContentType: "text/vnd.google.email-reaction+json; charset=utf-8", CTE: "7bit",
		Content: []byte(`{"version":1,"emoji":"+"}`)}
	id := fake.AddPartsMessage(gmailtest.File("text/plain", "notes.txt", []byte("Notes.\n")), image, reaction)

	got, err := s.DownloadAttachments(context.Background(), t.TempDir(), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	wantSkipped := []service.Skipped{{PartID: "2", Reason: service.SkipInline}, {PartID: "3", Reason: service.SkipReaction}}
	if !slices.Equal(names(got.Files), []string{"notes.txt"}) || !slices.Equal(got.Skipped, wantSkipped) {
		t.Fatalf("got %+v; want notes.txt, the image passed over as inline, the reaction passed over", got)
	}

	got, err = s.DownloadAttachments(context.Background(), t.TempDir(), id, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got.Files), []string{"notes.txt", "logo.png"}) ||
		!slices.Equal(got.Skipped, []service.Skipped{{PartID: "3", Reason: service.SkipReaction}}) {
		t.Fatalf("include_inline: got %+v; want notes.txt and logo.png, the reaction passed over", got)
	}
}

// Parts named are saved in the order named, inline or not; a name the
// message does not list fails alone.
func TestDownloadAttachmentsNamedParts(t *testing.T) {
	s, fake := newService(t)
	sc := fake.Scenario(gmailtest.ScenarioInternational)
	got, err := s.DownloadAttachments(context.Background(), t.TempDir(), sc.MessageIDs[0], []string{"2", "9", "1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got.Files), []string{"年度報告.xlsx", "résumé.pdf"}) || len(got.Skipped) != 0 ||
		len(got.Failed) != 1 || got.Failed[0].PartID != "9" || got.Failed[0].Class != string(gapi.ClassNotFound) {
		t.Fatalf("got %+v; want parts 2 and 1 saved, and part 9 failed as not_found", got)
	}
	if n := len(calls(fake, "gmail.users.messages.get")); n != 1 {
		t.Errorf("%d message reads; want 1 for every part", n)
	}
}

// A part that fails leaves no file, and the file written before it
// stays.
func TestDownloadAttachmentsKeepsWhatWasWrittenBeforeAFailure(t *testing.T) {
	s, fake := newService(t)
	dir := t.TempDir()
	// The first is kept in the message, so only the second is read apart.
	first := &gmailtest.Part{ContentType: "text/plain; charset=utf-8", Disposition: `attachment; filename="first.txt"`,
		CTE: "base64", Content: []byte("First.\n")}
	id := fake.AddPartsMessage(first, gmailtest.File("text/plain", "second.txt", []byte("Second.\n")))
	fake.Fail(gmailtest.Failure{Method: "gmail.users.messages.attachments.get", Status: 500, Reason: "backendError", Times: 4})

	got, err := s.DownloadAttachments(context.Background(), dir, id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got.Files), []string{"first.txt"}) || len(got.Failed) != 1 || got.Failed[0].PartID != "2" ||
		got.Failed[0].Class != string(gapi.ClassUnavailable) {
		t.Fatalf("got %+v; want first.txt saved and part 2 failed", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "first.txt" {
		t.Errorf("the directory holds %v; want first.txt alone", entries)
	}
}

// An attached message Gmail serves as its own parts has no content to
// write, and is refused rather than written as an empty file. An
// attached message stored apart is written whole, and an empty
// attachment is written empty.
func TestDownloadAttachmentsAttachedMessages(t *testing.T) {
	s, fake := newService(t)
	dir := t.TempDir()
	stored := &gmailtest.Part{ContentType: "message/rfc822", Disposition: `attachment; filename="stored.eml"`,
		Content: []byte(attachedMessage), Filename: "stored.eml"}
	expanded := &gmailtest.Part{ContentType: "message/rfc822", Disposition: `attachment; filename="expanded.eml"`,
		Content: []byte(attachedMessage), Expanded: true}
	empty := &gmailtest.Part{ContentType: "text/plain; charset=utf-8", Disposition: `attachment; filename="empty.txt"`}
	id := fake.AddPartsMessage(stored, expanded, empty)

	got, err := s.DownloadAttachments(context.Background(), dir, id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got.Files), []string{"stored.eml", "empty.txt"}) || len(got.Failed) != 1 ||
		got.Failed[0].PartID != "2" || got.Failed[0].Class != string(gapi.ClassUnsupported) {
		t.Fatalf("got %+v; want stored.eml and empty.txt saved, and part 2 refused as unsupported", got)
	}
	if b, _ := os.ReadFile(got.Files[0].Path); !bytes.Contains(b, []byte("The lake house is booked.")) {
		t.Errorf("stored.eml holds %q; want the attached message", b)
	}
	if got.Files[1].Bytes != 0 {
		t.Errorf("empty.txt is %d bytes", got.Files[1].Bytes)
	}

	_, err = s.DownloadAttachment(context.Background(), dir, id, "2")
	wantClass(t, err, gapi.ClassUnsupported)
	if _, err := os.Stat(filepath.Join(dir, "expanded.eml")); !os.IsNotExist(err) {
		t.Errorf("a refused attached message left a file: %v", err)
	}
}

// One call saves at most 100 parts; with none named, the rest are
// passed over and named.
func TestDownloadAttachmentsStopsAtOneHundred(t *testing.T) {
	fake := gmailtest.New()
	t.Cleanup(fake.Close)
	// A quota this test cannot reach, so 102 reads do not wait for it.
	s := service.New(gapi.New(gapi.Options{BaseURL: fake.URL(), UnitsPerMinute: 1 << 20,
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"})}))
	parts := make([]*gmailtest.Part, service.MaxDownloads+2)
	for i := range parts {
		parts[i] = gmailtest.File("text/plain", fmt.Sprintf("f%03d.txt", i), []byte("x"))
	}
	id := fake.AddPartsMessage(parts...)
	got, err := s.DownloadAttachments(context.Background(), t.TempDir(), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []service.Skipped{{PartID: "101", Reason: service.SkipLimit}, {PartID: "102", Reason: service.SkipLimit}}
	if len(got.Files) != service.MaxDownloads || !slices.Equal(got.Skipped, want) {
		t.Fatalf("saved %d, skipped %+v; want 100 saved and parts 101 and 102 passed over", len(got.Files), got.Skipped)
	}
}

func TestDownloadAttachmentsRefusals(t *testing.T) {
	s, fake := newService(t)
	sc := fake.Scenario(gmailtest.ScenarioInternational)
	ctx := context.Background()

	_, err := s.DownloadAttachments(ctx, "", sc.MessageIDs[0], nil, false)
	wantClass(t, err, gapi.ClassBlocked)
	_, err = s.DownloadAttachments(ctx, t.TempDir(), sc.MessageIDs[0], []string{"1", "2", "1"}, false)
	wantClass(t, err, gapi.ClassInvalid)
	many := make([]string, service.MaxDownloads+1)
	for i := range many {
		many[i] = strconv.Itoa(i)
	}
	_, err = s.DownloadAttachments(ctx, t.TempDir(), sc.MessageIDs[0], many, false)
	wantClass(t, err, gapi.ClassInvalid)
	if n := len(calls(fake, "gmail.users.messages.get")); n != 0 {
		t.Errorf("%d message reads; a refused call reads nothing", n)
	}
	_, err = s.DownloadAttachments(ctx, t.TempDir(), "0000000000fffff0", nil, false)
	wantClass(t, err, gapi.ClassNotFound)
}
