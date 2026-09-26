package gapi_test

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/internal/gmail"
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
