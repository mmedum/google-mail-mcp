package gapi_test

import (
	"bytes"
	"context"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
)

func fakeClient(t *testing.T) (*gapi.Client, *gmailtest.Server) {
	t.Helper()
	fake := gmailtest.New()
	t.Cleanup(fake.Close)
	return gapi.New(gapi.Options{
		BaseURL:     fake.URL(),
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}),
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}), fake
}

func lastQuery(t *testing.T, fake *gmailtest.Server, method string) url.Values {
	t.Helper()
	calls := fake.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == method {
			q, err := url.ParseQuery(calls[i].Query)
			if err != nil {
				t.Fatal(err)
			}
			return q
		}
	}
	t.Fatalf("no call to %s", method)
	return nil
}

func TestListOptionsReachTheQuery(t *testing.T) {
	c, fake := fakeClient(t)
	ctx := context.Background()
	o := gapi.ListOptions{Q: "from:ada", LabelIDs: []string{"INBOX", "Label_1"}, IncludeSpamTrash: true, Max: 7}
	if _, err := c.ListMessages(ctx, o); err != nil {
		t.Fatal(err)
	}
	q := lastQuery(t, fake, "gmail.users.messages.list")
	if q.Get("q") != "from:ada" || len(q["labelIds"]) != 2 || q.Get("includeSpamTrash") != "true" ||
		q.Get("maxResults") != "7" || q.Has("pageToken") {
		t.Errorf("messages.list query = %v", q)
	}

	first, err := c.ListMessages(ctx, gapi.ListOptions{Max: 1})
	if err != nil || first.NextPageToken == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	if _, err := c.ListMessages(ctx, gapi.ListOptions{Max: 1, PageToken: first.NextPageToken}); err != nil {
		t.Fatal(err)
	}
	if q := lastQuery(t, fake, "gmail.users.messages.list"); q.Get("pageToken") != first.NextPageToken {
		t.Errorf("the page token did not reach the query: %v", q)
	}

	if _, err := c.ListDrafts(ctx, o); err != nil {
		t.Fatal(err)
	}
	if q := lastQuery(t, fake, "gmail.users.drafts.list"); len(q["labelIds"]) != 0 {
		t.Errorf("drafts.list sent labelIds, which it does not take: %v", q)
	}

	if _, err := c.ListThreads(ctx, gapi.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	q = lastQuery(t, fake, "gmail.users.threads.list")
	q.Del("prettyPrint") // the client asks for compact JSON on every call
	if len(q) != 0 {
		t.Errorf("an empty listing sent %v", q)
	}
}

func TestFormatsAndHeaders(t *testing.T) {
	c, fake := fakeClient(t)
	ctx := context.Background()
	sc := fake.Scenario(gmailtest.ScenarioPlainThread)

	if _, err := c.GetMessage(ctx, sc.MessageIDs[0], gapi.FormatMetadata, "From", "Subject"); err != nil {
		t.Fatal(err)
	}
	q := lastQuery(t, fake, "gmail.users.messages.get")
	if q.Get("format") != "metadata" || len(q["metadataHeaders"]) != 2 {
		t.Errorf("metadata read = %v", q)
	}
	if _, err := c.GetThread(ctx, sc.ThreadID, gapi.FormatFull, "From"); err != nil {
		t.Fatal(err)
	}
	if q := lastQuery(t, fake, "gmail.users.threads.get"); q.Get("format") != "full" || len(q["metadataHeaders"]) != 0 {
		t.Errorf("a full read sent headers: %v", q)
	}
}

func TestEveryReadMethod(t *testing.T) {
	c, fake := fakeClient(t)
	ctx := context.Background()
	p, err := c.Profile(ctx)
	if err != nil || p.EmailAddress != gmailtest.Account {
		t.Fatalf("profile = %+v, %v", p, err)
	}
	labels, err := c.ListLabels(ctx)
	if err != nil || len(labels.Labels) == 0 {
		t.Fatalf("labels = %v, %v", labels, err)
	}
	if l, err := c.GetLabel(ctx, labels.Labels[0].ID); err != nil || l.ID != labels.Labels[0].ID {
		t.Errorf("label = %+v, %v", l, err)
	}
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	if d, err := c.GetDraft(ctx, sc.DraftID, gapi.FormatFull); err != nil || d.ID != sc.DraftID {
		t.Errorf("draft = %+v, %v", d, err)
	}
	backed := fake.Scenario(gmailtest.ScenarioBackedBody)
	m, err := c.GetMessage(ctx, backed.MessageIDs[0], gapi.FormatFull)
	if err != nil {
		t.Fatal(err)
	}
	var attID string
	for _, part := range flatten(m.Payload) {
		if part.Body != nil && part.Body.AttachmentID != "" {
			attID = part.Body.AttachmentID
		}
	}
	if attID == "" {
		t.Fatal("the backed-body scenario has no part behind an attachment id")
	}
	body, err := c.GetAttachment(ctx, backed.MessageIDs[0], attID)
	if err != nil || body.Data == "" {
		t.Errorf("attachment = %+v, %v", body, err)
	}
}

// flatten lists a MIME tree's parts, depth first.
func flatten(p *gmail.MessagePart) []gmail.MessagePart {
	if p == nil {
		return nil
	}
	out := []gmail.MessagePart{*p}
	for i := range p.Parts {
		out = append(out, flatten(&p.Parts[i])...)
	}
	return out
}

func TestListHistoryQueryAndExpiry(t *testing.T) {
	c, fake := fakeClient(t)
	ctx := context.Background()
	start := strconv.FormatUint(fake.HistoryID()-3, 10)
	res, err := c.ListHistory(ctx, gapi.HistoryOptions{StartHistoryID: start, LabelID: "INBOX",
		Types: []string{"messageAdded", "labelAdded"}, Max: 2})
	if err != nil {
		t.Fatal(err)
	}
	q := lastQuery(t, fake, "gmail.users.history.list")
	if q.Get("startHistoryId") != start || q.Get("labelId") != "INBOX" || len(q["historyTypes"]) != 2 || q.Get("maxResults") != "2" {
		t.Errorf("history.list query = %v", q)
	}
	if res.HistoryID != strconv.FormatUint(fake.HistoryID(), 10) {
		t.Errorf("historyId = %s; want the mailbox's current %d", res.HistoryID, fake.HistoryID())
	}

	_, err = c.ListHistory(ctx, gapi.HistoryOptions{StartHistoryID: "1"})
	if cl, _ := gapi.ClassOf(err); cl != gapi.ClassNotFound {
		t.Fatalf("an expired start = %v; want not_found for the service to read as expired", err)
	}
}

func TestSettingsReads(t *testing.T) {
	c, fake := fakeClient(t)
	ctx := context.Background()
	v, err := c.Vacation(ctx)
	if err != nil || v.ResponseSubject == "" {
		t.Fatalf("vacation = %+v, %v", v, err)
	}
	af, err := c.AutoForwarding(ctx)
	if err != nil || af.Enabled {
		t.Fatalf("auto-forwarding = %+v, %v", af, err)
	}
	fwd, err := c.ForwardingAddresses(ctx)
	if err != nil || len(fwd.ForwardingAddresses) != 2 {
		t.Fatalf("forwarding addresses = %+v, %v", fwd, err)
	}
	imap, err := c.Imap(ctx)
	if err != nil || !imap.Enabled {
		t.Fatalf("imap = %+v, %v", imap, err)
	}
	pop, err := c.Pop(ctx)
	if err != nil || pop.AccessWindow != "disabled" {
		t.Fatalf("pop = %+v, %v", pop, err)
	}
	lang, err := c.Language(ctx)
	if err != nil || lang.DisplayLanguage == "" {
		t.Fatalf("language = %+v, %v", lang, err)
	}
	as, err := c.SendAs(ctx)
	if err != nil || len(as.SendAs) != 2 {
		t.Fatalf("send-as = %+v, %v", as, err)
	}
	fs, err := c.Filters(ctx)
	if err != nil || len(fs.Filter) != 3 {
		t.Fatalf("filters = %+v, %v", fs, err)
	}
	// Every settings read is one unit, on the fake's own price list.
	if got := fake.Units(); got != 8 {
		t.Errorf("eight settings reads spent %d units; want 8", got)
	}
}

// rawReply builds a reply to a parent message with the server's MIME
// builder, dropping whichever of §2.5's conditions the test names.
func rawReply(t *testing.T, fake *gmailtest.Server, parentID, drop string, size int) (threadID string, raw []byte) {
	t.Helper()
	parent, ok := fake.Message(parentID, "full")
	if !ok {
		t.Fatalf("no message %s", parentID)
	}
	pm, err := mime.ParsePayload(parent.Payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := mime.Outgoing{
		To: pm.From, Subject: "Re: " + pm.Subject, Text: "Reply.\n", MessageID: mime.NewMessageID("a@example.com"),
		InReplyTo: pm.MessageID, References: append(append([]string(nil), pm.References...), pm.MessageID),
	}
	threadID = parent.ThreadID
	switch drop {
	case "threadId":
		threadID = ""
	case "headers":
		o.InReplyTo, o.References = "", nil
	case "subject":
		o.Subject = "Something else"
	}
	if size > 0 {
		o.Attachments = []mime.OutAttachment{{Filename: "big.bin", Content: make([]byte, size)}}
	}
	raw, err = mime.Build(o)
	if err != nil {
		t.Fatal(err)
	}
	return threadID, raw
}

func TestDraftsThreadOnlyWithAllThreeConditions(t *testing.T) {
	c, fake := fakeClient(t)
	ctx := context.Background()
	parent := fake.Scenario(gmailtest.ScenarioPlainThread)
	last := parent.MessageIDs[len(parent.MessageIDs)-1]
	for _, drop := range []string{"", "threadId", "headers", "subject"} {
		threadID, raw := rawReply(t, fake, last, drop, 0)
		d, err := c.CreateDraft(ctx, threadID, raw)
		if err != nil {
			t.Fatalf("drop %q: %v", drop, err)
		}
		joined := d.Message.ThreadID == parent.ThreadID
		if joined != (drop == "") {
			t.Errorf("drop %q: joined the thread = %v", drop, joined)
		}
		if !slices.Equal(d.Message.LabelIDs, []string{"DRAFT"}) || !strings.HasPrefix(d.ID, "r") {
			t.Errorf("draft %+v", d)
		}
	}
}

func TestADraftUpdateReplacesItsMessage(t *testing.T) {
	c, fake := fakeClient(t)
	ctx := context.Background()
	sc := fake.Scenario(gmailtest.ScenarioPlainThread)
	threadID, raw := rawReply(t, fake, sc.MessageIDs[len(sc.MessageIDs)-1], "", 0)
	d, err := c.CreateDraft(ctx, threadID, raw)
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.UpdateDraft(ctx, d.ID, threadID, raw)
	if err != nil || u.ID != d.ID || u.Message.ID == d.Message.ID || u.Message.ThreadID != sc.ThreadID {
		t.Fatalf("update %+v, %v", u, err)
	}
	if _, ok := fake.Message(d.Message.ID, "minimal"); ok {
		t.Error("the replaced message is still there")
	}
	got, err := c.GetDraft(ctx, d.ID, gapi.FormatRaw)
	if err != nil || got.Message.Raw == "" {
		t.Fatalf("read back %v", err)
	}
	if err := c.DeleteDraft(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetDraft(ctx, d.ID, gapi.FormatMinimal); !isClass(err, gapi.ClassNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if err := c.DeleteDraft(ctx, d.ID); !isClass(err, gapi.ClassNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if _, err := c.UpdateDraft(ctx, d.ID, "", raw); !isClass(err, gapi.ClassNotFound) {
		t.Errorf("update of a deleted draft: %v", err)
	}
}

func TestALargeDraftGoesThroughTheUploadPath(t *testing.T) {
	c, fake := fakeClient(t)
	ctx := context.Background()
	sc := fake.Scenario(gmailtest.ScenarioPlainThread)
	threadID, raw := rawReply(t, fake, sc.MessageIDs[len(sc.MessageIDs)-1], "", gapi.UploadThreshold)
	d, err := c.CreateDraft(ctx, threadID, raw)
	if err != nil || d.Message.ThreadID != sc.ThreadID {
		t.Fatalf("create %+v, %v", d, err)
	}
	if u, err := c.UpdateDraft(ctx, d.ID, threadID, raw); err != nil || u.Message.ThreadID != sc.ThreadID {
		t.Fatalf("update %+v, %v", u, err)
	}
	var uploads int
	for _, call := range fake.Calls() {
		if strings.HasPrefix(call.Path, "/upload/") {
			uploads++
		}
	}
	if uploads != 2 {
		t.Errorf("%d uploads", uploads)
	}
	stored, _ := fake.Message(mustDraftMessage(t, c, d.ID), "raw")
	back, err := mime.DecodeBase64URL(stored.Raw)
	if err != nil || !bytes.Equal(back, raw) {
		t.Error("the uploaded bytes were not stored as sent")
	}
}

func mustDraftMessage(t *testing.T, c *gapi.Client, id string) string {
	t.Helper()
	d, err := c.GetDraft(context.Background(), id, gapi.FormatMinimal)
	if err != nil {
		t.Fatal(err)
	}
	return d.Message.ID
}

func isClass(err error, want gapi.Class) bool {
	c, ok := gapi.ClassOf(err)
	return ok && c == want
}

func TestLabelWritesOnMessagesAndThreads(t *testing.T) {
	c, fake := fakeClient(t)
	ctx := context.Background()
	sc := fake.Scenario(gmailtest.ScenarioPlainThread)
	m, err := c.ModifyMessage(ctx, sc.MessageIDs[0], gmail.ModifyMessageRequest{AddLabelIDs: []string{"STARRED"}, RemoveLabelIDs: []string{"INBOX"}})
	if err != nil || !slices.Contains(m.LabelIDs, "STARRED") || slices.Contains(m.LabelIDs, "INBOX") {
		t.Fatalf("modify %+v, %v", m, err)
	}
	th, err := c.ModifyThread(ctx, sc.ThreadID, gmail.ModifyThreadRequest{AddLabelIDs: []string{"Label_1"}})
	if err != nil || len(th.Messages) != len(sc.MessageIDs) || !slices.Contains(th.Messages[0].LabelIDs, "Label_1") {
		t.Fatalf("thread modify %+v, %v", th, err)
	}
	for _, bad := range []string{"SENT", "DRAFT", "Label_999"} {
		_, err := c.ModifyMessage(ctx, sc.MessageIDs[0], gmail.ModifyMessageRequest{AddLabelIDs: []string{bad}})
		if !isClass(err, gapi.ClassInvalid) {
			t.Errorf("add %s: %v", bad, err)
		}
	}
	draft := fake.Scenario(gmailtest.ScenarioDraftReply)
	if _, err := c.ModifyMessage(ctx, draft.MessageIDs[2], gmail.ModifyMessageRequest{AddLabelIDs: []string{"STARRED"}}); !isClass(err, gapi.ClassInvalid) {
		t.Errorf("labeling a draft: %v", err)
	}

	tm, err := c.TrashMessage(ctx, sc.MessageIDs[0])
	if err != nil || !slices.Contains(tm.LabelIDs, "TRASH") {
		t.Fatalf("trash %+v, %v", tm, err)
	}
	if um, err := c.UntrashMessage(ctx, sc.MessageIDs[0]); err != nil || slices.Contains(um.LabelIDs, "TRASH") {
		t.Fatalf("untrash %+v, %v", um, err)
	}
	tt, err := c.TrashThread(ctx, sc.ThreadID)
	if err != nil || !slices.Contains(tt.Messages[len(tt.Messages)-1].LabelIDs, "TRASH") {
		t.Fatalf("trash thread %+v, %v", tt, err)
	}
	if ut, err := c.UntrashThread(ctx, sc.ThreadID); err != nil || slices.Contains(ut.Messages[0].LabelIDs, "TRASH") {
		t.Fatalf("untrash thread %+v, %v", ut, err)
	}
	if _, err := c.TrashMessage(ctx, "00000000000fffff"); !isClass(err, gapi.ClassNotFound) {
		t.Errorf("trash of nothing: %v", err)
	}
}

func TestLabelCreateAndPatch(t *testing.T) {
	c, _ := fakeClient(t)
	ctx := context.Background()
	l, err := c.CreateLabel(ctx, gmail.Label{Name: "Travel", LabelListVisibility: "labelShowIfUnread",
		Color: &gmail.LabelColor{TextColor: "#ffffff", BackgroundColor: "#16a766"}})
	if err != nil || l.ID == "" || l.Type != gmail.LabelTypeUser || l.MessageListVisibility != "show" || l.Color == nil {
		t.Fatalf("create %+v, %v", l, err)
	}
	for name, want := range map[string]gapi.Class{"travel": gapi.ClassConflict, "Inbox": gapi.ClassInvalid, " ": gapi.ClassInvalid} {
		if _, err := c.CreateLabel(ctx, gmail.Label{Name: name}); !isClass(err, want) {
			t.Errorf("create %q: %v, want %s", name, err, want)
		}
	}
	p, err := c.PatchLabel(ctx, l.ID, gmail.Label{Name: "Trips", MessageListVisibility: "hide"})
	if err != nil || p.Name != "Trips" || p.MessageListVisibility != "hide" || p.LabelListVisibility != "labelShowIfUnread" || p.Color == nil {
		t.Fatalf("patch %+v, %v", p, err)
	}
	if _, err := c.PatchLabel(ctx, l.ID, gmail.Label{Name: "TRIPS"}); err != nil {
		t.Errorf("a label renamed in its own case: %v", err)
	}
	if _, err := c.PatchLabel(ctx, "Label_1", gmail.Label{Name: "Receipts"}); !isClass(err, gapi.ClassConflict) {
		t.Errorf("rename onto a taken name: %v", err)
	}
	if _, err := c.PatchLabel(ctx, "INBOX", gmail.Label{Name: "x"}); !isClass(err, gapi.ClassInvalid) {
		t.Errorf("patch a system label: %v", err)
	}
	if _, err := c.PatchLabel(ctx, l.ID, gmail.Label{Color: &gmail.LabelColor{TextColor: "red"}}); !isClass(err, gapi.ClassInvalid) {
		t.Errorf("a bad color: %v", err)
	}
	if _, err := c.PatchLabel(ctx, "Label_999", gmail.Label{Name: "x"}); !isClass(err, gapi.ClassNotFound) {
		t.Errorf("patch nothing: %v", err)
	}
}

// The client paces the writes the fake refuses when they overlap, and
// no others: the two lists are kept apart, as the unit costs are, and
// held equal here.
func TestFakeAndClientAgreeOnFilterWrites(t *testing.T) {
	got := slices.Sorted(maps.Keys(gapi.FilterWriteIDs))
	if want := gmailtest.FilterWriteMethods(); !slices.Equal(got, want) {
		t.Errorf("the client paces %v; the fake refuses overlaps of %v", got, want)
	}
}
