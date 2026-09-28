package render_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
	"github.com/mmedum/google-mail-mcp/v2/internal/render"
)

// update rewrites the golden files instead of comparing against them:
//
//	go test ./internal/render -update
//
// A golden file makes a change to how mail reads show up as a diff
// somebody has to look at (§13). Regenerating is one flag; reading the
// diff is the part that cannot be automated.
var update = flag.Bool("update", false, "rewrite the golden files")

// goldenDir is the repository's testdata, where the leak scan looks.
const goldenDir = "../../testdata/golden"

// token is fixed so goldens are stable.
const token = "TESTTOKEN"

func opts() render.Options {
	return render.Options{Tokens: func() string { return token }}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".txt")
	if *update {
		if err := os.MkdirAll(goldenDir, 0o750); err != nil {
			t.Fatalf("create %s: %v", goldenDir, err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // a fixture path built from a literal
	if err != nil {
		t.Fatalf("read %s: %v (run `go test ./internal/render -update` to create it)", path, err)
	}
	if got != string(want) {
		t.Fatalf("%s does not match the golden file.\n--- got ---\n%s\n--- want ---\n%s\n"+
			"If the change is deliberate, run `go test ./internal/render -update` and read the diff.", path, got, want)
	}
}

type box struct {
	t      *testing.T
	s      *gmailtest.Server
	labels model.LabelIndex
}

func newBox(t *testing.T) box {
	t.Helper()
	s := gmailtest.New()
	t.Cleanup(s.Close)
	return box{t: t, s: s, labels: model.NewLabelIndex(s.Labels())}
}

// message reads a message as the service will: once, then again with
// the parts it asked for.
func (b box) message(id, format string) model.Message {
	b.t.Helper()
	g, ok := b.s.Message(id, format)
	if !ok {
		b.t.Fatalf("no message %s", id)
	}
	m, err := model.NewMessage(&g, b.labels, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	if len(m.NeedsFetch) > 0 {
		fetched := map[string][]byte{}
		for _, n := range m.NeedsFetch {
			fetched[n.PartID], _ = b.s.Attachment(n.AttachmentID)
		}
		m, _ = model.NewMessage(&g, b.labels, fetched)
	}
	return m
}

func (b box) thread(name, format string) model.Thread {
	sc := b.s.Scenario(name)
	t := model.Thread{ID: sc.ThreadID}
	for _, id := range sc.MessageIDs {
		t.Messages = append(t.Messages, b.message(id, format))
	}
	return t
}

func TestGoldenProfileAndLabels(t *testing.T) {
	b := newBox(t)
	golden(t, "profile", render.Profile(model.NewProfile(gmail.Profile{
		EmailAddress: gmailtest.Account, MessagesTotal: 29, ThreadsTotal: 11, HistoryID: "1031",
	})))
	golden(t, "labels", render.Labels(model.NewLabels(b.s.Labels(), false)))
	counted := []gmail.Label{
		{ID: "INBOX", Name: "INBOX", Type: "system", MessagesTotal: 21, MessagesUnread: 5, ThreadsTotal: 8, ThreadsUnread: 4},
		{ID: "Label_3", Name: "Receipts", Type: "user", MessagesTotal: 1, ThreadsTotal: 1,
			LabelListVisibility: "labelShowIfUnread", Color: &gmail.LabelColor{TextColor: "#ffffff", BackgroundColor: "#16a766"}},
	}
	golden(t, "labels_counted", render.Labels(model.NewLabels(counted, true)))
}

func TestGoldenListings(t *testing.T) {
	b := newBox(t)
	var threads []model.Thread
	for _, name := range []string{gmailtest.ScenarioInjection, gmailtest.ScenarioDraftReply, gmailtest.ScenarioInternational, gmailtest.ScenarioNewsletter} {
		th := b.thread(name, "metadata")
		th.Snippet = th.Latest().Snippet
		threads = append(threads, th)
	}
	golden(t, "threads", render.Threads(render.ThreadList{Threads: threads, NextPageToken: "page-4", ResultSizeEstimate: 11}, opts()).Text)
	golden(t, "threads_empty_page", render.Threads(render.ThreadList{NextPageToken: "page-0", ResultSizeEstimate: 11}, opts()).Text)
	golden(t, "threads_complete", render.Threads(render.ThreadList{Threads: threads[:1], ResultSizeEstimate: 1}, opts()).Text)

	var msgs []model.Message
	for _, id := range b.s.Scenario(gmailtest.ScenarioInternational).MessageIDs {
		msgs = append(msgs, b.message(id, "metadata"))
	}
	golden(t, "messages", render.Messages(render.MessageList{Messages: msgs, ResultSizeEstimate: 4}, opts()).Text)

	sc := b.s.Scenario(gmailtest.ScenarioDraftReply)
	d := model.Draft{ID: sc.DraftID, Message: b.message(sc.MessageIDs[2], "metadata")}
	golden(t, "drafts", render.Drafts(render.DraftList{Drafts: []model.Draft{d}, ResultSizeEstimate: 1}, opts()).Text)
}

func TestGoldenThreads(t *testing.T) {
	b := newBox(t)
	golden(t, "thread_plain", render.Thread(b.thread(gmailtest.ScenarioPlainThread, "full"), opts()).Text)

	o := opts()
	o.ShowQuoted = true
	golden(t, "thread_plain_show_quoted", render.Thread(b.thread(gmailtest.ScenarioPlainThread, "full"), o).Text)

	long := b.thread(gmailtest.ScenarioLongThread, "full")
	o = opts()
	o.Budget = 6000
	res := render.Thread(long, o)
	if !res.Truncated || res.NextCursor == 0 || len(res.Omitted) == 0 {
		t.Fatalf("long thread under a small budget: %+v", res)
	}
	golden(t, "thread_long_budget", res.Text)

	o.Cursor = res.NextCursor
	golden(t, "thread_long_cursor", render.Thread(long, o).Text)

	golden(t, "thread_international", render.Thread(b.thread(gmailtest.ScenarioInternational, "full"), opts()).Text)
	golden(t, "thread_draft_reply", render.Thread(b.thread(gmailtest.ScenarioDraftReply, "full"), opts()).Text)
}

// A thread's drafts follow the conversation; over the budget they are
// listed by id, and a continued read does not repeat them (§17.2).
func TestThreadDraftsFollowTheConversation(t *testing.T) {
	b := newBox(t)
	th := b.thread(gmailtest.ScenarioDraftReply, "full")
	sc := b.s.Scenario(gmailtest.ScenarioDraftReply)
	draft := sc.MessageIDs[2]

	o := opts()
	o.Budget = render.MinBudget
	res := render.Thread(th, o)
	if n := utf8.RuneCountInString(res.Text); n > res.Budget || slices.Contains(res.Omitted, draft) {
		t.Fatalf("over budget or a draft offered to the cursor: %d characters, omitted %v", n, res.Omitted)
	}
	if !strings.Contains(res.Text, "drafts in this thread, not sent: 1 draft\n  message "+draft) ||
		strings.Contains(res.Text, "I will confirm") || !strings.Contains(res.Text, "get_message reads each draft") {
		t.Errorf("drafts over the budget are not listed by id:\n%s", res.Text)
	}

	// Forty drafts: the list is cut, and the read stays in its budget.
	many := th
	many.Messages = slices.Clone(th.Messages)
	for i := range 40 {
		d := th.Messages[2]
		d.ID = fmt.Sprintf("%016x", 0xd0+i)
		many.Messages = append(many.Messages, d)
	}
	o = opts()
	o.Budget = render.MinBudget
	res = render.Thread(many, o)
	if n := utf8.RuneCountInString(res.Text); n > res.Budget || !strings.Contains(res.Text, "and 36 more") ||
		!strings.Contains(res.Text, "── 2 of 2") {
		t.Errorf("forty drafts: %d characters of %d, or the list or the newest message is missing:\n%s", n, res.Budget, res.Text)
	}

	o = opts()
	o.Cursor = 1
	if text := render.Thread(th, o).Text; strings.Contains(text, "drafts in this thread") {
		t.Errorf("a continued read repeats the drafts:\n%s", text)
	}
}

func TestGoldenMessages(t *testing.T) {
	b := newBox(t)
	one := func(name string) model.Message { return b.message(b.s.Scenario(name).MessageIDs[0], "full") }

	golden(t, "message_newsletter", render.Message(one(gmailtest.ScenarioNewsletter), opts()).Text)
	golden(t, "message_placeholder", render.Message(one(gmailtest.ScenarioPlaceholder), opts()).Text)
	golden(t, "message_invite", render.Message(one(gmailtest.ScenarioInvite), opts()).Text)
	golden(t, "message_backed_body", render.Message(one(gmailtest.ScenarioBackedBody), opts()).Text)
	golden(t, "message_injection", render.Message(one(gmailtest.ScenarioInjection), opts()).Text)
	golden(t, "message_spam", render.Message(one(gmailtest.ScenarioSpam), opts()).Text)

	o := opts()
	o.AllHeaders = true
	golden(t, "message_all_headers", render.Message(one(gmailtest.ScenarioPlainThread), o).Text)

	golden(t, "message_metadata_only", render.Message(b.message(b.s.Scenario(gmailtest.ScenarioPlainThread).MessageIDs[0], "metadata"), opts()).Text)

	long := b.message(b.s.Scenario(gmailtest.ScenarioLongThread).MessageIDs[9], "full")
	o = opts()
	o.Budget = 3000
	res := render.Message(long, o)
	if res.NextOffset == 0 {
		t.Fatal("long body was not cut")
	}
	golden(t, "message_long_cut", res.Text)
	o.Offset = res.NextOffset
	golden(t, "message_long_continued", render.Message(long, o).Text)

	sc := b.s.Scenario(gmailtest.ScenarioDraftReply)
	d := model.Draft{ID: sc.DraftID, Message: b.message(sc.MessageIDs[2], "full")}
	golden(t, "draft", render.Draft(d, opts()).Text)
}
