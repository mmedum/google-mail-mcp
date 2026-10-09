package tools_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/v2/internal/config"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/v2/internal/tools"
)

func TestListChanges(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	start := strconv.FormatUint(fake.HistoryID()-2, 10)
	var out tools.ChangesOut
	text := call(t, h, "list_changes", map[string]any{"history_id": start, "max": 1}, &out)
	if out.Expired || len(out.Changes) != 1 || out.Complete || out.NextPageToken == "" {
		t.Fatalf("first page = %+v", out)
	}
	if !strings.Contains(text, "page_token="+out.NextPageToken) {
		t.Errorf("the text does not say how to continue:\n%s", text)
	}
	var next tools.ChangesOut
	call(t, h, "list_changes", map[string]any{"history_id": start, "max": 1, "page_token": out.NextPageToken}, &next)
	if len(next.Changes) != 1 || !next.Complete || next.HistoryID != strconv.FormatUint(fake.HistoryID(), 10) {
		t.Fatalf("second page = %+v", next)
	}
}

func TestListChangesSaysACursorExpired(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	var out tools.ChangesOut
	text := call(t, h, "list_changes", map[string]any{"history_id": "3"}, &out)
	if !out.Expired || out.HistoryID != strconv.FormatUint(fake.HistoryID(), 10) || len(out.Changes) != 0 {
		t.Fatalf("out = %+v", out)
	}
	if !strings.Contains(text, "cursor expired") {
		t.Errorf("the text does not say the cursor expired:\n%s", text)
	}
}

func TestGetSettingsLeadsWithForwarding(t *testing.T) {
	h, fake := connectFake(t, config.Config{ReadOnly: true})
	fake.UpdateSettings(func(s *gmailtest.Settings) {
		s.AutoForwarding.Enabled, s.AutoForwarding.EmailAddress = true, gmailtest.BackupAddress
	})
	var out tools.SettingsOut
	text := call(t, h, "get_settings", nil, &out)
	if !out.AutoForwarding.Enabled || out.AutoForwarding.Address != gmailtest.BackupAddress || out.Units != 7 {
		t.Fatalf("out = %+v", out)
	}
	if !strings.Contains(text, "budget: ") || !strings.Contains(strings.SplitN(text, "\n", 3)[1], "FORWARDING IS ON") {
		t.Errorf("forwarding is not the first thing said:\n%s", text)
	}
	if out.Vacation.UntrustedSubject == "" || !strings.Contains(text, out.Boundary) {
		t.Errorf("the vacation reply is not in a block:\n%s", text)
	}
}

func TestListFiltersFlagsForwarding(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var out tools.FiltersOut
	text := call(t, h, "list_filters", nil, &out)
	if len(out.Filters) != 3 || out.Forwarding != 1 || out.Units != 2 {
		t.Fatalf("out = %+v", out)
	}
	if !strings.Contains(text, "FORWARDS matching mail to "+gmailtest.BackupAddress) {
		t.Errorf("the forwarding filter is not flagged:\n%s", text)
	}
}

func TestDownloadsAreRegisteredOnlyWithALocalDir(t *testing.T) {
	for _, tc := range []struct {
		cfg  config.Config
		want bool
	}{
		{config.Config{}, false},
		{config.Config{ReadOnly: true}, false},
		{config.Config{LocalDir: t.TempDir()}, true},
		{config.Config{ReadOnly: true, LocalDir: t.TempDir()}, true},
	} {
		h, _ := connectFake(t, tc.cfg)
		got := map[string]bool{}
		for _, tool := range h.Tools(t) {
			if tool.Name == "download_attachment" || tool.Name == "download_attachments" {
				got[tool.Name] = true
				if tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
					t.Errorf("%s: annotations = %+v; want not read-only (it writes a file) and not destructive (it never overwrites)",
						tool.Name, tool.Annotations)
				}
			}
		}
		if got["download_attachment"] != tc.want || got["download_attachments"] != tc.want {
			t.Errorf("config %+v: registered %v; want both %v", tc.cfg, got, tc.want)
		}
	}
}

func TestDownloadAttachmentWritesIntoTheLocalDir(t *testing.T) {
	dir := t.TempDir()
	h, fake := connectFake(t, config.Config{LocalDir: dir})
	sc := fake.Scenario(gmailtest.ScenarioInternational)

	var msg tools.MessageOut
	call(t, h, "get_message", map[string]any{"message_id": sc.MessageIDs[2]}, &msg)
	if len(msg.Message.Attachments) != 1 || msg.Message.Attachments[0].PartID != "1" {
		t.Fatalf("attachments = %+v; want one, with its part_id", msg.Message.Attachments)
	}

	var out tools.DownloadOut
	text := call(t, h, "download_attachment", map[string]any{"message_id": sc.MessageIDs[2], "part_id": "1"}, &out)
	if filepath.Dir(string(out.UntrustedPath)) != dir || out.UntrustedDeclaredName == "" || out.Suffixed || out.Units != 40 {
		t.Fatalf("out = %+v", out)
	}
	if b, err := os.ReadFile(string(out.UntrustedPath)); err != nil || string(b) != "MZ generated fixture" || out.Bytes != int64(len(b)) {
		t.Fatalf("file = %q, %v", b, err)
	}
	// The name is the sender's: the server says what it did outside the
	// block, and the name only inside it.
	before, _, _ := strings.Cut(text, "<<<untrusted-mail")
	outside := before + text[strings.LastIndex(text, ">>>")+len(">>>"):]
	if strings.Contains(outside, "invoice") {
		t.Errorf("the file name reached the server's own lines:\n%s", text)
	}

	var second tools.DownloadOut
	call(t, h, "download_attachment", map[string]any{"message_id": sc.MessageIDs[2], "part_id": "1"}, &second)
	if !second.Suffixed || second.UntrustedPath == out.UntrustedPath {
		t.Errorf("second download = %+v; want a new, numbered name", second)
	}
}

// download_attachments lists each file as download_attachment does,
// says why a part was passed over or failed, and keeps the sender's file
// names inside a block.
func TestDownloadAttachmentsReportsEachPart(t *testing.T) {
	dir := t.TempDir()
	h, fake := connectFake(t, config.Config{LocalDir: dir})
	sc := fake.Scenario(gmailtest.ScenarioInvite)

	var out tools.DownloadsOut
	text := call(t, h, "download_attachments", map[string]any{"message_id": sc.MessageIDs[0]}, &out)
	if out.MessageID != sc.MessageIDs[0] || len(out.Files) != 1 || out.Files[0].PartID != "1" ||
		filepath.Base(string(out.Files[0].UntrustedPath)) != "invite.ics" || out.Files[0].Bytes != 271 || out.Units != 40 {
		t.Fatalf("out = %+v; want invite.ics from part 1, 271 bytes, for 40 units", out)
	}
	if len(out.Skipped) != 1 || out.Skipped[0] != (tools.PassedPart{PartID: "0.2", Reason: "invitation_copy"}) ||
		len(out.Failed) != 0 {
		t.Fatalf("skipped %+v, failed %+v; want part 0.2 passed over as invitation_copy", out.Skipped, out.Failed)
	}
	if !strings.Contains(text, "passed over part 0.2: it is an invitation's calendar version of the body") {
		t.Errorf("the text does not say why part 0.2 was passed over:\n%s", text)
	}
	if strings.Contains(outside(text), "invite.ics") {
		t.Errorf("the file name reached the server's own lines:\n%s", text)
	}

	call(t, h, "download_attachments", map[string]any{"message_id": sc.MessageIDs[0], "part_ids": []any{"1", "7"}}, &out)
	if len(out.Files) != 1 || !out.Files[0].Suffixed || len(out.Failed) != 1 || out.Failed[0].PartID != "7" ||
		!strings.HasPrefix(out.Failed[0].Error, "[not_found] ") {
		t.Fatalf("out = %+v; want part 1 saved under a new name and part 7 failed as [not_found]", out)
	}
}

func TestResources(t *testing.T) {
	h, fake := connectFake(t, config.Config{ReadOnly: true})
	sc := fake.Scenario(gmailtest.ScenarioPlainThread)
	ctx := context.Background()

	var templates []string
	for rt, err := range h.Client.ResourceTemplates(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		templates = append(templates, rt.URITemplate)
	}
	if strings.Join(templates, " ") != tools.MessageResource+" "+tools.ThreadResource &&
		strings.Join(templates, " ") != tools.ThreadResource+" "+tools.MessageResource {
		t.Fatalf("templates = %v", templates)
	}

	for uri, want := range map[string]string{
		"gmail://threads/" + sc.ThreadID:       "thread " + sc.ThreadID,
		"gmail://messages/" + sc.MessageIDs[0]: "message " + sc.MessageIDs[0],
		tools.LabelsResource:                   "Projects/Offsite · id Label_2",
	} {
		res, err := h.Client.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("read %s: %v", uri, err)
		}
		if len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, want) || res.Contents[0].MIMEType != "text/plain" {
			t.Errorf("read %s = %+v; want text containing %q", uri, res.Contents, want)
		}
	}
	// The thread resource is get_thread's text: same budget, same blocks.
	res, _ := h.Client.ReadResource(ctx, &mcp.ReadResourceParams{URI: "gmail://threads/" + sc.ThreadID})
	tool := h.Call(t, "get_thread", map[string]any{"thread_id": sc.ThreadID})
	var th tools.ThreadOut
	testutil.DecodeStructured(t, tool.StructuredContent, &th)
	if strings.ReplaceAll(res.Contents[0].Text, boundaryIn(res.Contents[0].Text), "T") !=
		strings.ReplaceAll(testutil.Text(tool), th.Boundary, "T") {
		t.Errorf("the thread resource differs from get_thread:\n%s\n---\n%s", res.Contents[0].Text, testutil.Text(tool))
	}

	// A thread or message Gmail does not have is the protocol's
	// resource-not-found, invalid params since SEP-2164, not a server
	// error.
	for _, uri := range []string{"gmail://threads/0000000000fffff0", "gmail://messages/0000000000fffff0", "gmail://threads/a/b",
		"gmail://elsewhere/x"} {
		_, err := h.Client.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		var werr *jsonrpc.Error
		if !errors.As(err, &werr) || werr.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("read %s: err = %v; want resource not found (code -32602)", uri, err)
		}
	}
}

// boundaryIn reads the token off a rendering's first block.
func boundaryIn(text string) string {
	const open = "<<<untrusted-mail "
	i := strings.Index(text, open)
	if i < 0 {
		return "\x00"
	}
	tok, _, _ := strings.Cut(text[i+len(open):], ":")
	return tok
}

// readAttachment reads one attachment and returns the result and its text.
func readAttachment(t *testing.T, h *testutil.Harness, args map[string]any) (tools.AttachmentOut, string) {
	t.Helper()
	var out tools.AttachmentOut
	text := call(t, h, "read_attachment", args, &out)
	return out, text
}

// A text attachment's content arrives inside a block, decoded from its
// charset, for the message read and the attachment read.
func TestReadAttachmentReadsTextTypes(t *testing.T) {
	h, fake := connectFake(t, config.Config{ReadOnly: true})
	id := fake.AddPartsMessage(
		gmailtest.File("text/csv; charset=iso-8859-1", "prices.csv", []byte("item;price\nCaf\xe9;3\n")),
		gmailtest.File("text/markdown; charset=utf-8", "notes.md", []byte("# Notes\u200b\n\n> a quote in Markdown\n")),
		gmailtest.File("application/json", "data.json", []byte(`{"room": "Garden suite"}`)),
		gmailtest.File("text/calendar; charset=utf-8", "event.ics", []byte("BEGIN:VCALENDAR\r\nSUMMARY:Review\r\nEND:VCALENDAR\r\n")),
	)
	for _, tc := range []struct {
		part, as, want string
		hidden         int
	}{
		{"1", "text", "item;price\nCafé;3", 0},
		// The zero-width space is removed and counted; the quote stays, as
		// only plain text and HTML collapse one.
		{"2", "text", "# Notes\n\n> a quote in Markdown", 1},
		{"3", "text", `{"room": "Garden suite"}`, 0},
		{"4", "calendar", "BEGIN:VCALENDAR\nSUMMARY:Review\nEND:VCALENDAR", 0},
	} {
		out, text := readAttachment(t, h, map[string]any{"message_id": id, "part_id": tc.part})
		if out.ReadAs != tc.as || out.PartID != tc.part || out.MessageID != id || out.Message != nil || out.Units != 40 ||
			out.HiddenCharsRemoved != tc.hidden {
			t.Errorf("part %s: out = %+v; want read as %s, %d hidden, for 40 units", tc.part, out, tc.as, tc.hidden)
		}
		if !strings.Contains(text, tc.want) || strings.Contains(outside(text), tc.want) {
			t.Errorf("part %s: the content %q is not inside a block:\n%s", tc.part, tc.want, text)
		}
	}
}

// HTML is converted as a body is: hidden text removed and counted, a
// link whose text names another site flagged, nothing fetched.
func TestReadAttachmentConvertsHTML(t *testing.T) {
	h, fake := connectFake(t, config.Config{ReadOnly: true})
	page := `<p>The menu is attached.</p><span style="display:none">secret words</span>` +
		`<a href="https://tracker.example/x">www.harbor.example</a>`
	id := fake.AddPartsMessage(gmailtest.File("text/html; charset=utf-8", "menu.html", []byte(page)))
	out, text := readAttachment(t, h, map[string]any{"message_id": id, "part_id": "1"})
	if out.ReadAs != "html" || out.HiddenCharsRemoved != len("secret words") || out.LinkMismatches != 1 {
		t.Fatalf("out = %+v; want html, 12 hidden characters and one link flagged", out)
	}
	if !strings.Contains(text, "The menu is attached.") || strings.Contains(text, "secret words") {
		t.Errorf("the text shows the wrong content:\n%s", text)
	}
	if !strings.Contains(text, "note: 1 link whose text names a different site") {
		t.Errorf("the mismatched link is not flagged:\n%s", text)
	}
}

// A long attachment is cut to the budget and continued with offset.
func TestReadAttachmentContinuesWithOffset(t *testing.T) {
	h, fake := connectFake(t, config.Config{ReadOnly: true})
	var long strings.Builder
	for i := range 60 {
		long.WriteString("Paragraph " + strconv.Itoa(i+1) + " of the minutes, written out at length so the file runs past one budget.\n\n")
	}
	id := fake.AddPartsMessage(gmailtest.File("text/plain; charset=utf-8", "minutes.txt", []byte(long.String())))

	first, text := readAttachment(t, h, map[string]any{"message_id": id, "part_id": "1", "budget_chars": 2000})
	if !first.Truncated || first.NextOffset == 0 || !strings.Contains(text, "offset="+strconv.Itoa(first.NextOffset)) {
		t.Fatalf("first read: %+v; want it cut, with the offset to continue from", first)
	}
	if strings.Contains(text, "Paragraph 60 ") {
		t.Errorf("the first read already holds the last paragraph")
	}
	_, text = readAttachment(t, h, map[string]any{"message_id": id, "part_id": "1", "offset": first.NextOffset})
	if !strings.Contains(text, "(content from character "+strconv.Itoa(first.NextOffset)+" of ") ||
		!strings.Contains(text, "Paragraph 60 ") || strings.Contains(text, "Paragraph 1 ") {
		t.Errorf("the second read does not continue where the first stopped:\n%s", text)
	}
}

// A body or an attachment with thousands of links whose text names
// another site still reads within its budget: the note counts every
// link, the block pairs the first that fit an eighth of the budget, and
// the rest are counted after it (§4.8).
func TestManyMisleadingLinksStayWithinTheBudget(t *testing.T) {
	h, fake := connectFake(t, config.Config{ReadOnly: true})
	var page strings.Builder
	for range 3000 {
		page.WriteString(`<a href="https://lure.example/">www.bank.example</a> `)
	}
	body := fake.AddPartsMessage(&gmailtest.Part{ContentType: "text/html; charset=utf-8", CTE: "base64", Content: []byte(page.String())})
	file := fake.AddPartsMessage(gmailtest.File("text/html; charset=utf-8", "page.html", []byte(page.String())))
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"get_message", map[string]any{"message_id": body, "budget_chars": 2000}},
		{"read_attachment", map[string]any{"message_id": file, "part_id": "1", "budget_chars": 2000}},
	} {
		var out struct {
			Message struct {
				LinkMismatches int `json:"link_mismatches"`
			} `json:"message"`
			LinkMismatches int `json:"link_mismatches"`
			Budget         int `json:"budget_chars"`
		}
		text := call(t, h, tc.tool, tc.args, &out)
		if n := utf8.RuneCountInString(text); n > 2000 || out.Budget != 2000 {
			t.Errorf("%s: %d characters for a budget of %d", tc.tool, n, out.Budget)
		}
		if out.LinkMismatches+out.Message.LinkMismatches != 3000 {
			t.Errorf("%s: link_mismatches %d, %d; want 3000", tc.tool, out.LinkMismatches, out.Message.LinkMismatches)
		}
		for _, want := range []string{
			"note: 3000 links whose text names a different site from the one it points to; the block below pairs the first 4.\n",
			"\n… and 2996 more\n",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: no %q in\n%s", tc.tool, want, text)
			}
		}
		if n := strings.Count(text, "text names www.bank.example, link points to lure.example\n"); n != 4 {
			t.Errorf("%s: %d pairs listed; want 4", tc.tool, n)
		}
	}
}

// Invisible characters in a part's Content-Type, in its type or its
// invitation method, are removed and counted: in a message read, an
// attachment read, and the parts of an attached message.
func TestInvisibleCharactersInAPartsTypeAreRemoved(t *testing.T) {
	h, fake := connectFake(t, config.Config{ReadOnly: true})
	const tags = "\U000E0049\U000E0047\U000E004E" // three tag characters
	const removed = "invisible characters were removed from the subject, names, addresses and attachment types."
	inner := "From: Bruno Fennick <bruno.fennick@example.org>\r\nSubject: The venue\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nThe lake house.\r\n" +
		"--inner\r\nContent-Type: application/pdf" + tags + "\r\nContent-Disposition: attachment; filename=\"plan.pdf\"\r\n\r\n%PDF-1.4\r\n" +
		"--inner\r\nContent-Type: text/calendar; method=REQUEST" + tags + "\r\nContent-Disposition: attachment; filename=\"i.ics\"\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n--inner--\r\n"
	eml := fake.AddPartsMessage(&gmailtest.Part{ContentType: "message/rfc822", Disposition: `attachment; filename="venue.eml"`,
		Content: []byte(inner), Filename: "venue.eml"})
	var read tools.AttachmentOut
	text := call(t, h, "read_attachment", map[string]any{"message_id": eml, "part_id": "1"}, &read)
	if read.Message == nil || read.Message.HiddenCharsRemoved != 6 || len(read.Message.Attachments) != 2 ||
		read.Message.Attachments[0].UntrustedMimeType != "application/pdf" ||
		read.Message.Attachments[1].UntrustedCalendarMethod != "REQUEST" {
		t.Fatalf("attached message %+v", read.Message)
	}
	if strings.ContainsRune(text, 0xE0049) || !strings.Contains(text, "note: 6 "+removed) {
		t.Errorf("the text keeps a tag character, or does not count them:\n%s", text)
	}

	ics := fake.AddPartsMessage(gmailtest.File(`text/calendar; method="REQUEST`+tags+`"`, "i.ics", []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")))
	var msg tools.MessageOut
	text = call(t, h, "get_message", map[string]any{"message_id": ics}, &msg)
	if msg.Message.HiddenCharsRemoved != 3 || len(msg.Message.Attachments) != 1 ||
		msg.Message.Attachments[0].UntrustedCalendarMethod != "REQUEST" {
		t.Fatalf("message %+v", msg.Message)
	}
	if strings.ContainsRune(text, 0xE0049) || !strings.Contains(text, "note: 3 "+removed) {
		t.Errorf("the text keeps a tag character, or does not count them:\n%s", text)
	}
	text = call(t, h, "read_attachment", map[string]any{"message_id": ics, "part_id": "1"}, &read)
	if read.HiddenCharsRemoved != 3 || strings.ContainsRune(text, 0xE0049) ||
		!strings.Contains(text, "note: 3 invisible characters were removed from the attachment's type.") {
		t.Errorf("hidden_chars_removed %d; text:\n%s", read.HiddenCharsRemoved, text)
	}
}

// An attached message reads as get_message reads one: its headers and
// body, its quotes collapsed unless show_quoted, and its own attachments
// listed with no part id, since none names them in the mailbox.
func TestReadAttachmentReadsAnAttachedMessage(t *testing.T) {
	h, fake := connectFake(t, config.Config{ReadOnly: true})
	inner := "From: Bruno Fennick <bruno.fennick@example.org>\r\nTo: Ada Quill <ada.quill@example.com>\r\n" +
		"Subject: The venue\r\nDate: Mon, 2 Mar 2026 09:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nThe lake house is booked.\r\n\r\n" +
		"On Monday Ada wrote:\r\n> Is it booked?\r\n" +
		"--inner\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"plan.pdf\"\r\n\r\n%PDF-1.4\r\n" +
		"--inner--\r\n"
	id := fake.AddPartsMessage(&gmailtest.Part{ContentType: "message/rfc822",
		Disposition: `attachment; filename="venue.eml"`, Content: []byte(inner), Filename: "venue.eml"})

	out, text := readAttachment(t, h, map[string]any{"message_id": id, "part_id": "1"})
	m := out.Message
	if out.ReadAs != "message" || m == nil || m.ID != "" || m.ThreadID != "" || m.UntrustedSubject != "The venue" ||
		len(m.UntrustedFrom) != 1 || m.UntrustedFrom[0] != "Bruno Fennick <bruno.fennick@example.org>" {
		t.Fatalf("out = %+v, message %+v; want the attached message, with no id or thread", out, m)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].PartID != "" || m.Attachments[0].UntrustedFilename != "plan.pdf" {
		t.Errorf("attachments = %+v; want plan.pdf with no part_id", m.Attachments)
	}
	if !strings.Contains(text, "The lake house is booked.") || strings.Contains(text, "Is it booked?") ||
		!strings.Contains(text, "collapsed: 2 lines of quoted text") {
		t.Errorf("the body is not shown with its quote collapsed:\n%s", text)
	}
	if !strings.Contains(text, "Attachment: plan.pdf (application/pdf, 8 B)\n") {
		t.Errorf("the attached message's own attachment is not listed without a part id:\n%s", text)
	}
	if strings.Contains(outside(text), "The venue") || strings.Contains(outside(text), "bruno") {
		t.Errorf("the attached message's words reached the server's own lines:\n%s", text)
	}

	_, text = readAttachment(t, h, map[string]any{"message_id": id, "part_id": "1", "show_quoted": true})
	if !strings.Contains(text, "> Is it booked?") {
		t.Errorf("show_quoted does not show the quote:\n%s", text)
	}
}

// What cannot be read as text is refused before it is fetched.
func TestReadAttachmentRefusals(t *testing.T) {
	h, fake := connectFake(t, config.Config{ReadOnly: true})
	id := fake.AddPartsMessage(
		gmailtest.File("application/pdf", "plan.pdf", []byte("%PDF-1.4 generated fixture\n")),
		gmailtest.File("text/plain", "big.txt", []byte(strings.Repeat("x", 5<<20+1))),
		&gmailtest.Part{ContentType: "message/rfc822", Disposition: `attachment; filename="expanded.eml"`, Expanded: true,
			Content: []byte("From: Bruno Fennick <bruno.fennick@example.org>\r\nSubject: s\r\n\r\nBody.\r\n")},
	)
	for _, tc := range []struct {
		part  string
		class gapi.Class
		say   string
	}{
		{"1", gapi.ClassUnsupported, "download_attachment saves it"},
		{"2", gapi.ClassInvalid, "more than the 5 MB"},
		{"3", gapi.ClassUnsupported, "as parts of its own"},
		{"9", gapi.ClassNotFound, "get_message lists"},
	} {
		text := refused(t, h, "read_attachment", map[string]any{"message_id": id, "part_id": tc.part}, tc.class)
		if !strings.Contains(text, tc.say) {
			t.Errorf("part %s: %s; want it to say %q", tc.part, text, tc.say)
		}
	}
	if n := len(fake.CallsOf("gmail.users.messages.attachments.get")); n != 0 {
		t.Errorf("%d attachment reads; a refused read fetches nothing", n)
	}
	refused(t, h, "read_attachment", map[string]any{"message_id": id, "part_id": "1", "budget_chars": 10}, gapi.ClassInvalid)
}
