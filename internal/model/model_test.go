package model_test

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
)

func fixture(t *testing.T) (*gmailtest.Server, model.LabelIndex) {
	t.Helper()
	s := gmailtest.New()
	t.Cleanup(s.Close)
	return s, model.NewLabelIndex(s.Labels())
}

func TestLabelIndex(t *testing.T) {
	x := model.NewLabelIndex([]gmail.Label{{ID: "INBOX", Name: "INBOX"}, {ID: "Label_7", Name: "Projects/Alpha"}, {ID: "Label_8"}})
	got := x.Refs([]string{"INBOX", "Label_7", "Label_8", "Label_404"})
	want := []model.LabelRef{{"INBOX", "INBOX"}, {"Label_7", "Projects/Alpha"}, {"Label_8", "Label_8"}, {"Label_404", "Label_404"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestLabels(t *testing.T) {
	ls := model.NewLabels([]gmail.Label{
		{ID: "Label_2", Name: "zeta", Type: "user", Color: &gmail.LabelColor{TextColor: "#000000", BackgroundColor: "#ffffff"}},
		{ID: "INBOX", Name: "INBOX", Type: "system", MessagesTotal: 4},
		{ID: "Label_1", Name: "Alpha", Type: "user"},
		{ID: "Label_3", Type: "user"},
	}, true)
	var names []string
	for _, l := range ls {
		names = append(names, l.Name)
	}
	if !slices.Equal(names, []string{"INBOX", "Alpha", "Label_3", "zeta"}) {
		t.Fatalf("order %v", names)
	}
	if !ls[0].HasCounts || ls[0].MessagesTotal != 4 || ls[3].BackgroundColor != "#ffffff" {
		t.Fatalf("fields %+v", ls)
	}
}

func TestProfile(t *testing.T) {
	p := model.NewProfile(gmail.Profile{EmailAddress: "reader@example.com", MessagesTotal: 3, ThreadsTotal: 2, HistoryID: "10"})
	if p.Email != "reader@example.com" || p.MessagesTotal != 3 || p.ThreadsTotal != 2 || p.HistoryID != "10" {
		t.Fatalf("%+v", p)
	}
}

func TestMessageFormats(t *testing.T) {
	s, x := fixture(t)
	id := s.Scenario(gmailtest.ScenarioPlainThread).MessageIDs[0]

	full, _ := s.Message(id, "full")
	m, err := model.NewMessage(&full, x, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Complete || m.Subject != "Offsite venue" || m.Sender().Email != "ada.quill@example.com" || m.Body.Text == "" || m.RFC822MessageID == "" {
		t.Fatalf("full %+v", m)
	}
	if !m.HasLabel("STARRED") || !slices.Contains(m.Labels, model.LabelRef{ID: "Label_2", Name: "Projects/Offsite"}) {
		t.Fatalf("labels %v", m.Labels)
	}
	if m.Date.IsZero() || m.DateHeader == "" || m.SizeEstimate == 0 || m.HistoryID == "" {
		t.Fatalf("dates %+v", m)
	}

	meta, _ := s.Message(id, "metadata")
	mm, _ := model.NewMessage(&meta, x, nil)
	if mm.Complete || mm.Subject != "Offsite venue" || mm.Body.Text != "" {
		t.Fatalf("metadata %+v", mm)
	}

	minimal, _ := s.Message(id, "minimal")
	mn, _ := model.NewMessage(&minimal, x, nil)
	if mn.Complete || mn.Subject != "" || mn.Snippet == "" || mn.Date.IsZero() {
		t.Fatalf("minimal %+v", mn)
	}

	raw, _ := s.Message(id, "raw")
	mr, _ := model.NewMessage(&raw, x, nil)
	if !mr.Complete || mr.Body.Text != m.Body.Text {
		t.Fatalf("raw %+v", mr)
	}

	if _, err := model.NewMessage(nil, x, nil); err == nil {
		t.Fatal("nil message accepted")
	}
	if _, err := model.NewMessage(&gmail.Message{Raw: " "}, x, nil); err == nil {
		t.Fatal("blank raw accepted")
	}
	noDate := gmail.Message{Payload: &gmail.MessagePart{Headers: []gmail.MessagePartHeader{{Name: "Date", Value: "Mon, 2 Mar 2026 09:00:00 +0000"}}}}
	if nd, _ := model.NewMessage(&noDate, x, nil); nd.Date.IsZero() {
		t.Fatal("header date not used as a fallback")
	}
	if _, err := model.NewMessage(&gmail.Message{Payload: nil}, x, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSnippetUnescaped(t *testing.T) {
	x := model.NewLabelIndex(nil)
	m, _ := model.NewMessage(&gmail.Message{Snippet: "Tom &amp; Jerry&#39;s \u200Bplan"}, x, nil)
	if m.Snippet != "Tom & Jerry's plan" || m.SnippetHidden != 1 {
		t.Fatalf("%q %d", m.Snippet, m.SnippetHidden)
	}
}

func TestAttachmentsAndFetch(t *testing.T) {
	s, x := fixture(t)
	intl := s.Scenario(gmailtest.ScenarioInternational)
	g, _ := s.Message(intl.MessageIDs[0], "full")
	m, _ := model.NewMessage(&g, x, nil)
	if !m.HasAttachments || len(m.Attachments) != 2 {
		t.Fatalf("attachments %+v", m.Attachments)
	}
	meta, _ := s.Message(intl.MessageIDs[0], "metadata")
	if mm, _ := model.NewMessage(&meta, x, nil); !mm.HasAttachments {
		t.Fatal("multipart/mixed metadata not flagged")
	}

	backed := s.Scenario(gmailtest.ScenarioBackedBody).MessageIDs[0]
	g, _ = s.Message(backed, "full")
	m, _ = model.NewMessage(&g, x, nil)
	if len(m.NeedsFetch) != 2 || m.Body.Text != "" {
		t.Fatalf("needs fetch %+v", m.NeedsFetch)
	}
	fetched := map[string][]byte{}
	for _, n := range m.NeedsFetch {
		fetched[n.PartID], _ = s.Attachment(n.AttachmentID)
	}
	m, _ = model.NewMessage(&g, x, fetched)
	if len(m.NeedsFetch) != 0 || !strings.Contains(m.Body.Text, "Paragraph 40.") {
		t.Fatalf("after fetch: %d %q", len(m.NeedsFetch), m.Body.Text[:min(80, len(m.Body.Text))])
	}
}

func TestThread(t *testing.T) {
	s, x := fixture(t)
	sc := s.Scenario(gmailtest.ScenarioPlainThread)
	var g gmail.Thread
	g.ID = sc.ThreadID
	g.Snippet = "a &amp; b"
	for _, id := range sc.MessageIDs {
		m, _ := s.Message(id, "full")
		g.Messages = append(g.Messages, m)
	}
	th, err := model.NewThread(&g, x, nil)
	if err != nil {
		t.Fatal(err)
	}
	if th.Subject() != "Offsite venue" || th.Latest().ID != sc.MessageIDs[2] || th.Unread() != 1 || th.Snippet != "a & b" {
		t.Fatalf("thread %+v", th)
	}
	var emails []string
	for _, p := range th.Participants() {
		emails = append(emails, p.Email)
	}
	if !slices.Equal(emails, []string{"ada.quill@example.com", "reader@example.com", "bruno.fennick@example.org"}) {
		t.Fatalf("participants %v", emails)
	}
	if len(th.Labels()) < 4 || th.HasAttachments() {
		t.Fatalf("labels %v attachments %v", th.Labels(), th.HasAttachments())
	}
	if (model.Thread{}).Subject() != "" || (model.Thread{}).Latest().ID != "" {
		t.Fatal("empty thread")
	}
	if _, err := model.NewThread(nil, x, nil); err == nil {
		t.Fatal("nil thread accepted")
	}
	bad := gmail.Thread{Messages: []gmail.Message{{Raw: " "}}}
	if _, err := model.NewThread(&bad, x, nil); err == nil {
		t.Fatal("bad message accepted")
	}
	nameOnly := model.Thread{Messages: []model.Message{{}}}
	nameOnly.Messages[0].From = append(nameOnly.Messages[0].From, th.Messages[0].From[0])
	nameOnly.Messages[0].From[0].Email = ""
	if len(nameOnly.Participants()) != 1 {
		t.Fatal("name-only participant")
	}
	if (model.Message{}).Sender().Email != "" {
		t.Fatal("empty sender")
	}
}

// A thread's latest is its newest message that was sent or received:
// drafts, messages in the trash and emoji reactions after it are passed
// over. A thread of nothing else gives its newest message.
func TestThreadLatestPassesOverWhatWasNotSaid(t *testing.T) {
	b64 := base64.URLEncoding.EncodeToString
	from := []gmail.MessagePartHeader{{Name: "From", Value: "Ada Quill <ada.quill@example.com>"}}
	headersOnly := func(id, millis string, labels ...string) gmail.Message {
		return gmail.Message{ID: id, LabelIDs: labels, InternalDate: millis,
			Payload: &gmail.MessagePart{MimeType: "text/plain", Headers: from, Body: &gmail.MessagePartBody{}}}
	}
	// What Gmail sends for an emoji reaction, read with its parts.
	reaction := gmail.Message{ID: "m5", LabelIDs: []string{"INBOX"}, InternalDate: "5000",
		Payload: &gmail.MessagePart{MimeType: "multipart/alternative", Headers: from, Body: &gmail.MessagePartBody{},
			Parts: []gmail.MessagePart{
				{PartID: "0", MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: b64([]byte("Ada reacted")), Size: 11}},
				{PartID: "1", MimeType: model.ReactionType,
					Body: &gmail.MessagePartBody{Data: b64([]byte(`{"version":1,"emoji":"x"}`)), Size: 25}},
			}}}
	wire := gmail.Thread{ID: "m1", Messages: []gmail.Message{
		headersOnly("m1", "1000", "INBOX"),
		headersOnly("m2", "2000", "SENT"),
		headersOnly("m3", "3000", "DRAFT"),
		headersOnly("m4", "4000", "TRASH", "INBOX"),
		reaction,
		headersOnly("m6", "6000", "DRAFT"),
	}}
	th, err := model.NewThread(&wire, model.NewLabelIndex(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !th.Messages[4].IsReaction() || th.Messages[0].IsReaction() {
		t.Fatalf("reactions: m5 %v, m1 %v", th.Messages[4].IsReaction(), th.Messages[0].IsReaction())
	}
	if got := th.Latest().ID; got != "m2" {
		t.Errorf("latest = %s, want m2", got)
	}
	if m, ok := th.LatestAnswerable(); !ok || m.ID != "m2" {
		t.Errorf("latest answerable = %s, %v; want m2", m.ID, ok)
	}
	if th.Drafts() != 2 || th.Unread() != 0 {
		t.Errorf("drafts %d, unread %d; want 2 and 0", th.Drafts(), th.Unread())
	}

	th.Messages = th.Messages[2:]
	if got := th.Latest().ID; got != "m6" {
		t.Errorf("with nothing sent or received, latest = %s, want the newest, m6", got)
	}
	if m, ok := th.LatestAnswerable(); ok {
		t.Errorf("with nothing sent or received, latest answerable = %s", m.ID)
	}
}

// A participant is one address, whatever name or case it comes with;
// two addresses under one name are two participants.
func TestParticipantsAreDistinctAddresses(t *testing.T) {
	th := model.Thread{Messages: []model.Message{
		{From: []mime.Address{{Name: "Ada", Email: "ada@example.com"}}, To: []mime.Address{{Name: "Team", Email: "team@example.com"}}},
		{From: []mime.Address{{Name: "Ada Quill", Email: "Ada@Example.com"}}, Cc: []mime.Address{{Name: "Team", Email: "team@example.org"}}},
	}}
	var got []string
	for _, p := range th.Participants() {
		got = append(got, p.Name+" <"+p.Email+">")
	}
	want := []string{"Ada <ada@example.com>", "Team <team@example.com>", "Team <team@example.org>"}
	if !slices.Equal(got, want) {
		t.Errorf("Participants() = %q, want %q", got, want)
	}
}

// A reply's In-Reply-To and References reach the model; a message
// without them has none.
func TestAReplyKeepsItsThreadingHeaders(t *testing.T) {
	x := model.NewLabelIndex(nil)
	raw := "From: ada@example.com\r\nTo: reader@example.com\r\nSubject: Re: Plan\r\n" +
		"In-Reply-To: <second.1@example.com>\r\nReferences: <first.1@example.com> <second.1@example.com>\r\n\r\nYes.\r\n"
	m, err := model.NewMessage(&gmail.Message{Raw: base64.URLEncoding.EncodeToString([]byte(raw))}, x, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []model.Untrusted{"<second.1@example.com>"}; !slices.Equal(m.InReplyTo, want) {
		t.Errorf("InReplyTo = %q, want %q", m.InReplyTo, want)
	}
	if want := []model.Untrusted{"<first.1@example.com>", "<second.1@example.com>"}; !slices.Equal(m.References, want) {
		t.Errorf("References = %q, want %q", m.References, want)
	}

	plain := "From: ada@example.com\r\nTo: reader@example.com\r\nSubject: Plan\r\n\r\nHello.\r\n"
	m, err = model.NewMessage(&gmail.Message{Raw: base64.URLEncoding.EncodeToString([]byte(plain))}, x, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.InReplyTo != nil || m.References != nil {
		t.Errorf("a message that answers nothing: InReplyTo %q, References %q; want none", m.InReplyTo, m.References)
	}
}

func TestDraft(t *testing.T) {
	s, x := fixture(t)
	sc := s.Scenario(gmailtest.ScenarioDraftReply)
	g, _ := s.Message(sc.MessageIDs[2], "full")
	d, err := model.NewDraft(&gmail.Draft{ID: sc.DraftID, Message: &g}, x, nil)
	if err != nil || d.ID != sc.DraftID || d.Message.ID != sc.MessageIDs[2] || !d.Message.HasLabel("DRAFT") {
		t.Fatalf("draft %+v %v", d, err)
	}
	if _, err := model.NewDraft(&gmail.Draft{ID: "r1"}, x, nil); err == nil {
		t.Fatal("draft without message accepted")
	}
}

func TestChanges(t *testing.T) {
	x := model.NewLabelIndex([]gmail.Label{{ID: "Label_1", Name: "Projects"}})
	msg := &gmail.Message{ID: "0000000000000001", ThreadID: "0000000000000001", LabelIDs: []string{"INBOX"}}
	hs := []gmail.History{
		{ID: "11", MessagesAdded: []gmail.HistoryMessageAdded{{Message: msg}, {}}},
		{ID: "12", LabelsAdded: []gmail.HistoryLabelAdded{{Message: msg, LabelIDs: []string{"Label_1"}}}},
		{ID: "13", LabelsRemoved: []gmail.HistoryLabelRemoved{{Message: msg, LabelIDs: []string{"UNREAD"}}}},
		{ID: "14", MessagesDeleted: []gmail.HistoryMessageDeleted{{Message: msg}}},
	}
	cs := model.NewChanges(hs, x)
	var kinds []model.ChangeKind
	for _, c := range cs {
		kinds = append(kinds, c.Kind)
	}
	if !slices.Equal(kinds, []model.ChangeKind{model.ChangeAdded, model.ChangeLabelsAdded, model.ChangeLabelsRemoved, model.ChangeDeleted}) {
		t.Fatalf("kinds %v", kinds)
	}
	if cs[1].Labels[0].Name != "Projects" || cs[0].HistoryID != "11" || cs[3].MessageID != msg.ID {
		t.Fatalf("changes %+v", cs)
	}
}
