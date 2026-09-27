package tools_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/internal/mime"
	"github.com/mmedum/google-mail-mcp/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/internal/tools"
)

// refused calls a tool that must fail and returns its error text.
func refused(t *testing.T, h *testutil.Harness, name string, args map[string]any, class gapi.Class) string {
	t.Helper()
	res := h.Call(t, name, args)
	text := testutil.Text(res)
	if !res.IsError {
		t.Fatalf("%s succeeded: %s", name, text)
	}
	if !strings.HasPrefix(text, "["+string(class)+"]") {
		t.Fatalf("%s: %s; want [%s]", name, text, class)
	}
	return text
}

// writeCalls counts the requests that changed something.
func writeCalls(fake *gmailtest.Server) int {
	n := 0
	for _, c := range fake.Calls() {
		for _, w := range []string{".create", ".update", ".delete", ".modify", "trash", ".patch"} {
			if strings.HasSuffix(c.Method, w) || strings.Contains(c.Method, "trash") && w == "trash" {
				n++
				break
			}
		}
	}
	return n
}

func storedDraft(t *testing.T, fake *gmailtest.Server, messageID string) *mime.Message {
	t.Helper()
	raw, ok := fake.Raw(messageID)
	if !ok {
		t.Fatalf("no stored message %s", messageID)
	}
	return mime.ParseRaw(raw)
}

// outside is a rendering with its blocks cut out: the server's own lines.
func outside(text string) string {
	var b strings.Builder
	for {
		i := strings.Index(text, "<<<untrusted-mail")
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:i])
		j := strings.Index(text[i:], "<<<end-untrusted-mail")
		if j < 0 {
			return b.String()
		}
		text = text[i+j+len("<<<end-untrusted-mail"):]
	}
}

func labelIDs(ls []tools.LabelRef) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.ID
	}
	return out
}

func TestCreateDraft(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	var out tools.DraftWriteOut
	text := call(t, h, "create_draft", map[string]any{
		"to": []any{"Ada Quill <ada.quill@example.com>"}, "cc": []any{"bruno.fennick@example.org"},
		"bcc": []any{"Zoë Ångström <zoe@example.org>"}, "subject": "Plan für März", "body": "Line one\nLine two",
		"body_html": "<p>Line one</p>",
	}, &out)
	if out.DryRun || out.DraftID == "" || out.MessageID == "" || !slices.Contains(labelIDs(out.Labels), "DRAFT") || out.Units != 11 {
		t.Fatalf("out = %+v", out)
	}
	if string(out.UntrustedFrom) != gmailtest.Reader.Name+" <"+gmailtest.Account+">" || len(out.Recipients) != 3 ||
		out.Recipients[2].Field != "bcc" || out.Recipients[0].Origin != "caller" {
		t.Fatalf("from %q recipients %+v", out.UntrustedFrom, out.Recipients)
	}
	m := storedDraft(t, fake, out.MessageID)
	// Gmail replaces the Message-ID the server wrote, so the result names
	// none rather than one that was not kept.
	if m.Subject != "Plan für März" || len(m.Bcc) != 1 || m.Bcc[0].Name != "Zoë Ångström" || m.Body.Source != mime.SourcePlain ||
		out.UntrustedRFC822MessageID != "" || !strings.HasPrefix(m.MessageID, "<draft.") {
		t.Fatalf("stored %q %v %q %q", m.Subject, m.Bcc, m.Body.Source, m.MessageID)
	}
	if !strings.Contains(text, "update_draft needs draft_id "+out.DraftID+" and message_id "+out.MessageID) {
		t.Errorf("the witness is not named:\n%s", text)
	}
	if o := outside(text); strings.Contains(o, "März") || strings.Contains(o, "ada.quill") {
		t.Errorf("mail text outside the block:\n%s", text)
	}

	drafts := len(fake.DraftIDs())
	var dry tools.DraftWriteOut
	text = call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "body": "x", "dry_run": true}, &dry)
	if !dry.DryRun || dry.DraftID != "" || len(fake.DraftIDs()) != drafts || !strings.HasPrefix(text, "dry run") {
		t.Fatalf("a dry run saved: %+v\n%s", dry, text)
	}

	var from tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"from": gmailtest.AliasAddress, "body": "x"}, &from)
	if !strings.Contains(string(from.UntrustedFrom), gmailtest.AliasAddress) {
		t.Errorf("from alias: %q", from.UntrustedFrom)
	}
}

func TestCreateDraftRefusals(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	cases := []struct {
		name  string
		args  map[string]any
		class gapi.Class
	}{
		{"two addresses in one entry", map[string]any{"to": []any{"a@example.com, b@example.com"}}, gapi.ClassInvalid},
		{"not an address", map[string]any{"cc": []any{"nobody"}}, gapi.ClassInvalid},
		{"non-ASCII address", map[string]any{"to": []any{"zoë@example.org"}}, gapi.ClassInvalid},
		{"line break in subject", map[string]any{"subject": "a\nBcc: x@example.com"}, gapi.ClassInvalid},
		{"html without text", map[string]any{"body_html": "<p>x</p>"}, gapi.ClassInvalid},
		{"from not a send-as", map[string]any{"from": "someone@example.com"}, gapi.ClassInvalid},
		{"reply_all alone", map[string]any{"reply_all": true}, gapi.ClassInvalid},
		{"both reply fields", map[string]any{"reply_to": plain.MessageIDs[0], "reply_to_thread": plain.ThreadID}, gapi.ClassInvalid},
		{"a subject on a reply", map[string]any{"reply_to": plain.MessageIDs[0], "subject": "New"}, gapi.ClassInvalid},
		{"reply to a draft", map[string]any{"reply_to": fake.Scenario(gmailtest.ScenarioDraftReply).MessageIDs[2]}, gapi.ClassInvalid},
		{"reply to the trash", map[string]any{"reply_to": fake.Scenario(gmailtest.ScenarioTrash).MessageIDs[0]}, gapi.ClassInvalid},
		{"reply to nothing", map[string]any{"reply_to": "00000000000fffff"}, gapi.ClassNotFound},
		{"attachments without a directory", map[string]any{"attachments": []any{"a.txt"}}, gapi.ClassBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.ResetAccounting()
			refused(t, h, "create_draft", tc.args, tc.class)
			if n := writeCalls(fake); n != 0 {
				t.Errorf("%d writes reached Gmail", n)
			}
		})
	}
}

func TestCreateDraftReplies(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	parentID := plain.MessageIDs[2] // from Bruno to the reader and Ada
	parentRaw, _ := fake.Raw(parentID)
	parent := mime.ParseRaw(parentRaw)

	var out tools.DraftWriteOut
	text := call(t, h, "create_draft", map[string]any{"reply_to": parentID, "body": "Thanks.", "cc": []any{"chiara@example.com"}}, &out)
	if out.Reply == nil || out.Reply.ParentID != parentID || !out.Reply.Joined || out.ThreadID != plain.ThreadID || out.Units != 31 {
		t.Fatalf("reply %+v thread %s units %d", out.Reply, out.ThreadID, out.Units)
	}
	if len(out.Recipients) != 2 || !strings.Contains(string(out.Recipients[0].UntrustedAddress), gmailtest.Bruno.Email) ||
		out.Recipients[0].Origin != "parent" || out.Recipients[1].Origin != "caller" {
		t.Fatalf("recipients %+v", out.Recipients)
	}
	m := storedDraft(t, fake, out.MessageID)
	if m.Subject != "Re: Offsite venue" || strings.Join(m.InReplyTo, "") != parent.MessageID ||
		m.References[len(m.References)-1] != parent.MessageID || len(m.References) != len(parent.References)+1 {
		t.Fatalf("threading: subject %q in-reply-to %v references %v", m.Subject, m.InReplyTo, m.References)
	}
	if !strings.Contains(text, "Gmail filed the draft in the parent's thread.") {
		t.Errorf("text:\n%s", text)
	}

	var all tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"reply_to": parentID, "reply_all": true, "body": "All."}, &all)
	var origins []string
	for _, r := range all.Recipients {
		origins = append(origins, r.Field+":"+r.Origin)
		if strings.Contains(string(r.UntrustedAddress), gmailtest.Account) {
			t.Errorf("the account's own address is a recipient: %v", all.Recipients)
		}
	}
	if strings.Join(origins, ",") != "to:parent,to:reply_all" || all.Reply.DroppedOwn != 1 {
		t.Fatalf("reply-all recipients %v dropped %d", origins, all.Reply.DroppedOwn)
	}

	// The thread's newest message is a draft; the one before it was sent
	// by the account, so the reply goes to that message's recipients.
	dr := fake.Scenario(gmailtest.ScenarioDraftReply)
	var th tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"reply_to_thread": dr.ThreadID, "body": "Following up."}, &th)
	if !th.Reply.FromThread || th.Reply.ParentID != dr.MessageIDs[1] || !th.Reply.Joined || th.Units != 51 ||
		len(th.Recipients) != 1 || !strings.Contains(string(th.Recipients[0].UntrustedAddress), gmailtest.Freya.Email) {
		t.Fatalf("thread reply %+v %+v units %d", th.Reply, th.Recipients, th.Units)
	}
	if string(th.UntrustedSubject) != "Re: Budget sign-off" {
		t.Errorf("subject %q", th.UntrustedSubject)
	}
}

func TestCreateDraftAttachments(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "Отчёт.pdf"), []byte("%PDF fake"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, gapi.UploadThreshold+10), 0o600))
	must(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	must(t, os.WriteFile(outsideFile, []byte("secret"), 0o600))
	if err := os.Symlink(outsideFile, filepath.Join(dir, "link.txt")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	h, fake := connectFake(t, config.Config{LocalDir: dir})

	var out tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "body": "see attached",
		"attachments": []any{"Отчёт.pdf"}}, &out)
	if len(out.Attachments) != 1 || out.Attachments[0].UntrustedName != "Отчёт.pdf" ||
		out.Attachments[0].UntrustedMimeType != "application/pdf" || out.Upload {
		t.Fatalf("attachments %+v upload %v", out.Attachments, out.Upload)
	}
	if m := storedDraft(t, fake, out.MessageID); len(m.Attachments) != 1 || m.Attachments[0].DeclaredName != "Отчёт.pdf" {
		t.Fatalf("stored attachments %+v", m.Attachments)
	}

	var big tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "big", "attachments": []any{"big.bin"}}, &big)
	if !big.Upload || big.Bytes <= gapi.UploadThreshold {
		t.Fatalf("a %d-byte draft was not uploaded", big.Bytes)
	}

	for name, class := range map[string]gapi.Class{
		"../secret.txt": gapi.ClassInvalid, "sub/x": gapi.ClassInvalid, "missing.txt": gapi.ClassNotFound,
		"link.txt": gapi.ClassInvalid, "sub": gapi.ClassInvalid, "..": gapi.ClassInvalid,
	} {
		refused(t, h, "create_draft", map[string]any{"body": "x", "attachments": []any{name}}, class)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpdateDraft(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("first"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("second"), 0o600))
	h, fake := connectFake(t, config.Config{LocalDir: dir})

	var made tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "cc": []any{"c@example.com"},
		"subject": "Draft", "body": "one", "attachments": []any{"a.txt", "b.txt"}}, &made)
	beforeRaw, _ := fake.Raw(made.MessageID)

	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID}, gapi.ClassInvalid)
	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": "0000000000000abc",
		"subject": "x"}, gapi.ClassStale)

	var up tools.DraftWriteOut
	text := call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"subject": "Draft, revised", "cc": []any{}, "remove_attachments": []any{"1"}}, &up)
	if up.MessageID == made.MessageID || up.PreviousMessageID != made.MessageID || up.DraftID != made.DraftID || up.Units != 35 {
		t.Fatalf("update %+v", up)
	}
	if strings.Join(up.Changed, ",") != "cc,subject,attachments" || len(up.Removed) != 1 || up.Removed[0].UntrustedName != "a.txt" {
		t.Fatalf("changed %v removed %+v", up.Changed, up.Removed)
	}
	m := storedDraft(t, fake, up.MessageID)
	if m.Subject != "Draft, revised" || len(m.Cc) != 0 || len(m.To) != 1 || m.Body.Text != "one" ||
		len(m.Attachments) != 1 || m.Attachments[0].DeclaredName != "b.txt" {
		t.Fatalf("stored: %q cc %v to %v body %q attachments %+v", m.Subject, m.Cc, m.To, m.Body.Text, m.Attachments)
	}
	if before := mime.ParseRaw(beforeRaw); m.MessageID != before.MessageID {
		t.Errorf("the Message-ID changed from %q to %q", before.MessageID, m.MessageID)
	}
	afterRaw, _ := fake.Raw(up.MessageID)
	if !strings.Contains(string(afterRaw), "c2Vjb25k") { // b.txt, base64, as it was
		t.Error("the kept attachment is not in the saved draft")
	}
	if !strings.Contains(text, "the next update_draft needs message_id "+up.MessageID) {
		t.Errorf("text:\n%s", text)
	}

	// The old witness is stale now.
	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"body": "two"}, gapi.ClassStale)

	var dry tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": up.MessageID,
		"body": "three", "add_attachments": []any{"a.txt"}, "dry_run": true}, &dry)
	if !dry.DryRun || storedDraft(t, fake, up.MessageID).Body.Text != "one" || len(dry.Added) != 1 {
		t.Fatalf("a dry run saved: %+v", dry)
	}
	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": up.MessageID,
		"remove_attachments": []any{"7"}}, gapi.ClassInvalid)
	text = refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": up.MessageID,
		"remove_attachments": []any{""}}, gapi.ClassInvalid)
	if !strings.Contains(text, "whole draft") {
		t.Errorf("text: %s", text)
	}
}

func TestUpdateDraftKeepsTheTwoBodiesInStep(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var made tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "plain", "body_html": "<p>html</p>"}, &made)
	text := refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"body": "new"}, gapi.ClassInvalid)
	if !strings.Contains(text, "body_html") {
		t.Errorf("text: %s", text)
	}
	var up tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"body": "new", "body_html": "<p>new</p>"}, &up)
	if strings.Join(up.Changed, ",") != "body,body_html" {
		t.Errorf("changed %v", up.Changed)
	}
}

func TestUpdateDraftWarnsWhenAReplyLosesItsSubject(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	var made tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"reply_to": plain.MessageIDs[2], "body": "x"}, &made)
	var up tools.DraftWriteOut
	text := call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"subject": "Unrelated"}, &up)
	if !up.ThreadingAtRisk || !strings.Contains(text, "may leave its thread") {
		t.Fatalf("no warning:\n%s", text)
	}
}

func TestDeleteDraft(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)

	refused(t, h, "delete_draft", map[string]any{"draft_id": sc.DraftID}, gapi.ClassBlocked)
	var dry tools.DraftWriteOut
	call(t, h, "delete_draft", map[string]any{"draft_id": sc.DraftID, "dry_run": true}, &dry)
	if !dry.DryRun || dry.Deleted || !slices.Contains(fake.DraftIDs(), sc.DraftID) || dry.MessageID != sc.MessageIDs[2] {
		t.Fatalf("dry run %+v", dry)
	}
	var out tools.DraftWriteOut
	text := call(t, h, "delete_draft", map[string]any{"draft_id": sc.DraftID, "confirm": true}, &out)
	if !out.Deleted || out.Gone || slices.Contains(fake.DraftIDs(), sc.DraftID) || out.Units != 30 {
		t.Fatalf("delete %+v", out)
	}
	if !strings.Contains(text, "permanently") || string(out.UntrustedSubject) != "Re: Budget sign-off" {
		t.Errorf("text:\n%s", text)
	}
	refused(t, h, "delete_draft", map[string]any{"draft_id": sc.DraftID, "confirm": true}, gapi.ClassNotFound)
}

func TestDeleteDraftGoneBetweenReadAndDelete(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.drafts.delete", Status: 404, Reason: "notFound"})
	var out tools.DraftWriteOut
	text := call(t, h, "delete_draft", map[string]any{"draft_id": sc.DraftID, "confirm": true}, &out)
	if !out.Gone || !out.Deleted || !strings.Contains(text, "is gone") {
		t.Fatalf("out %+v\n%s", out, text)
	}
}

func TestModifyLabels(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	draft := fake.Scenario(gmailtest.ScenarioDraftReply)

	var out tools.ItemsOut
	text := call(t, h, "modify_labels", map[string]any{
		"message_ids": []any{plain.MessageIDs[0], plain.MessageIDs[2], draft.MessageIDs[2], "00000000000fffff"},
		"add":         []any{"starred"}, "remove": []any{"INBOX"},
	}, &out)
	if strings.Join(out.Verbs, ",") != "archive,star" || len(out.Items) != 4 {
		t.Fatalf("verbs %v items %+v", out.Verbs, out.Items)
	}
	want := []string{"changed", "changed", "failed", "failed"}
	for i, it := range out.Items {
		if it.Outcome != want[i] {
			t.Errorf("item %d: %s (%s); want %s", i, it.Outcome, it.Error, want[i])
		}
	}
	if !strings.HasPrefix(out.Items[2].Error, "[unsupported]") || !strings.HasPrefix(out.Items[3].Error, "[not_found]") {
		t.Errorf("errors %q %q", out.Items[2].Error, out.Items[3].Error)
	}
	if after := labelIDs(out.Items[1].LabelsAfter); !slices.Contains(after, "STARRED") || slices.Contains(after, "INBOX") {
		t.Errorf("after %v", after)
	}
	if out.Changed != 2 || out.Failed != 2 || !strings.Contains(text, "2 changed · 0 unchanged · 2 failed") {
		t.Errorf("counts:\n%s", text)
	}

	// Again: both are already as asked, and nothing is written.
	fake.ResetAccounting()
	var again tools.ItemsOut
	call(t, h, "modify_labels", map[string]any{"message_ids": []any{plain.MessageIDs[0], plain.MessageIDs[2]},
		"add": []any{"STARRED"}, "remove": []any{"INBOX"}}, &again)
	if again.Unchanged != 2 || writeCalls(fake) != 0 || again.Units != 41 {
		t.Fatalf("again %+v, %d writes", again, writeCalls(fake))
	}

	var th tools.ItemsOut
	call(t, h, "modify_labels", map[string]any{"thread_ids": []any{plain.ThreadID}, "add": []any{"Projects"}}, &th)
	if th.Items[0].Kind != "thread" || th.Items[0].Outcome != "changed" || !slices.Contains(labelIDs(th.Items[0].LabelsAfter), "Label_1") {
		t.Fatalf("thread %+v", th.Items)
	}

	var dry tools.ItemsOut
	fake.ResetAccounting()
	call(t, h, "modify_labels", map[string]any{"message_ids": []any{plain.MessageIDs[1]}, "add": []any{"UNREAD"}, "dry_run": true}, &dry)
	if !dry.DryRun || dry.Items[0].Outcome != "would_change" || writeCalls(fake) != 0 {
		t.Fatalf("dry %+v", dry)
	}

	many := make([]any, 101)
	for i := range many {
		many[i] = strings.Repeat("0", 15) + string(rune('a'+i%6)) + strings.Repeat("x", i/6)
	}
	for _, args := range []map[string]any{
		{"message_ids": []any{plain.MessageIDs[0]}, "add": []any{"SENT"}},
		{"message_ids": []any{plain.MessageIDs[0]}, "remove": []any{"DRAFT"}},
	} {
		refused(t, h, "modify_labels", args, gapi.ClassConflict)
	}
	for _, args := range []map[string]any{
		{"message_ids": []any{plain.MessageIDs[0]}, "add": []any{"TRASH"}},
		{"message_ids": []any{plain.MessageIDs[0]}},
		{"add": []any{"STARRED"}},
		{"message_ids": many, "add": []any{"STARRED"}},
		{"message_ids": []any{plain.MessageIDs[0]}, "add": []any{"STARRED"}, "remove": []any{"starred"}},
	} {
		refused(t, h, "modify_labels", args, gapi.ClassInvalid)
	}
	refused(t, h, "modify_labels", map[string]any{"message_ids": []any{plain.MessageIDs[0]}, "add": []any{"No such label"}}, gapi.ClassNotFound)
}

func TestTrashAndRestore(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	trashed := fake.Scenario(gmailtest.ScenarioTrash)
	draft := fake.Scenario(gmailtest.ScenarioDraftReply)

	var out tools.ItemsOut
	text := call(t, h, "trash", map[string]any{"message_ids": []any{plain.MessageIDs[0], trashed.MessageIDs[0], draft.MessageIDs[2]}}, &out)
	outcomes := []string{out.Items[0].Outcome, out.Items[1].Outcome, out.Items[2].Outcome}
	if strings.Join(outcomes, ",") != "changed,unchanged,failed" || !slices.Contains(labelIDs(out.Items[0].LabelsAfter), "TRASH") {
		t.Fatalf("items %+v", out.Items)
	}
	if !strings.Contains(text, "unchanged, already in the trash") || !strings.Contains(out.Items[2].Error, "delete_draft") {
		t.Errorf("text:\n%s", text)
	}
	if out.Units != 1+40+20+20 {
		t.Errorf("units %d", out.Units)
	}

	var back tools.ItemsOut
	text = call(t, h, "restore", map[string]any{"message_ids": []any{plain.MessageIDs[0], plain.MessageIDs[1]}}, &back)
	if back.Items[0].Outcome != "changed" || back.Items[1].Outcome != "unchanged" || !strings.Contains(text, "not in the trash") {
		t.Fatalf("restore %+v\n%s", back.Items, text)
	}

	var th tools.ItemsOut
	call(t, h, "trash", map[string]any{"thread_ids": []any{plain.ThreadID}}, &th)
	if th.Items[0].Outcome != "changed" {
		t.Fatalf("thread trash %+v", th.Items)
	}
	var thBack tools.ItemsOut
	call(t, h, "restore", map[string]any{"thread_ids": []any{plain.ThreadID}, "dry_run": true}, &thBack)
	if thBack.Items[0].Outcome != "would_change" {
		t.Fatalf("thread restore dry run %+v", thBack.Items)
	}
}

func TestCreateAndUpdateLabel(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var made tools.LabelWriteOut
	text := call(t, h, "create_label", map[string]any{"name": "Travel", "in_label_list": "show_if_unread",
		"text_color": "#FFFFFF", "background_color": "#16a766"}, &made)
	if made.Label.ID == "" || made.Label.InLabelList != "show_if_unread" || made.Label.TextColor != "#ffffff" || made.Units != 6 {
		t.Fatalf("created %+v", made)
	}
	if !strings.Contains(text, "created label Travel · id "+made.Label.ID) {
		t.Errorf("text:\n%s", text)
	}
	refused(t, h, "create_label", map[string]any{"name": "travel"}, gapi.ClassConflict)
	refused(t, h, "create_label", map[string]any{"name": "inbox"}, gapi.ClassInvalid)
	refused(t, h, "create_label", map[string]any{"name": "x", "in_label_list": "sometimes"}, gapi.ClassInvalid)
	refused(t, h, "create_label", map[string]any{"name": "x", "text_color": "#fff"}, gapi.ClassInvalid)

	var dry tools.LabelWriteOut
	call(t, h, "create_label", map[string]any{"name": "Later", "dry_run": true}, &dry)
	if !dry.DryRun || dry.Label.ID != "" {
		t.Fatalf("dry %+v", dry)
	}

	var up tools.LabelWriteOut
	text = call(t, h, "update_label", map[string]any{"label": "Travel", "name": "Trips", "in_message_list": "hide"}, &up)
	if up.Before == nil || up.Before.Name != "Travel" || up.Label.Name != "Trips" || up.Label.InMessageList != "hide" ||
		up.Label.InLabelList != "show_if_unread" || strings.Join(up.Changed, ",") != "name,in_message_list" {
		t.Fatalf("updated %+v", up)
	}
	if !strings.Contains(text, "before: Travel") || !strings.Contains(text, "after:  Trips") {
		t.Errorf("text:\n%s", text)
	}
	refused(t, h, "update_label", map[string]any{"label": "INBOX", "name": "Mine"}, gapi.ClassInvalid)
	refused(t, h, "update_label", map[string]any{"label": "Trips"}, gapi.ClassInvalid)
	refused(t, h, "update_label", map[string]any{"label": "Trips", "name": "Receipts"}, gapi.ClassConflict)
	refused(t, h, "update_label", map[string]any{"label": "Nothing"}, gapi.ClassInvalid)
	refused(t, h, "update_label", map[string]any{"label": "Nothing", "name": "x"}, gapi.ClassNotFound)
}
