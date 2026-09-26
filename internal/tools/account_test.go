package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/internal/tools"
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

func TestDownloadAttachmentIsRegisteredOnlyWithALocalDir(t *testing.T) {
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
		got := false
		for _, tool := range h.Tools(t) {
			if tool.Name == "download_attachment" {
				got = true
				if tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
					t.Errorf("annotations = %+v; want not read-only (it writes a file) and not destructive (it never overwrites)",
						tool.Annotations)
				}
			}
		}
		if got != tc.want {
			t.Errorf("config %+v: registered %v; want %v", tc.cfg, got, tc.want)
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

	for _, uri := range []string{"gmail://threads/0000000000fffff0", "gmail://threads/a/b", "gmail://elsewhere/x"} {
		if _, err := h.Client.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri}); err == nil {
			t.Errorf("read %s succeeded; want resource not found", uri)
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
