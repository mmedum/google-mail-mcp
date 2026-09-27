package gmailtest_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/mime"
)

func get(t *testing.T, s *gmailtest.Server, path string, q url.Values, out any) int {
	t.Helper()
	u := s.URL() + "/gmail/v1/users/me/" + path
	if q != nil {
		u += "?" + q.Encode()
	}
	resp, err := http.Get(u) //nolint:gosec,noctx // test URL
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("decode %s: %v\n%s", path, err, b)
		}
	}
	if resp.StatusCode != http.StatusOK && out != nil {
		if e, ok := out.(*apiError); ok {
			_ = json.Unmarshal(b, e)
			e.RetryAfter = resp.Header.Get("Retry-After")
		}
	}
	return resp.StatusCode
}

type apiError struct {
	Error struct {
		Code   int    `json:"code"`
		Status string `json:"status"`
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
	RetryAfter string `json:"-"`
}

var idShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestIDsAreGeneratedShape(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	for _, name := range s.Scenarios() {
		sc := s.Scenario(name)
		for _, id := range append([]string{sc.ThreadID}, sc.MessageIDs...) {
			if !idShape.MatchString(id) || !strings.Contains(id, "00000000") {
				t.Errorf("%s: id %q is not 16 hex digits with a run of zeros", name, id)
			}
		}
	}
}

func TestProfileAndLabels(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	var p gmail.Profile
	if get(t, s, "profile", nil, &p) != 200 || p.EmailAddress != gmailtest.Account || p.MessagesTotal == 0 || p.HistoryID == "" {
		t.Fatalf("profile %+v", p)
	}
	var ls gmail.ListLabelsResponse
	get(t, s, "labels", nil, &ls)
	if len(ls.Labels) < 15 {
		t.Fatalf("labels %d", len(ls.Labels))
	}
	for _, l := range ls.Labels {
		if l.MessagesTotal != 0 {
			t.Fatalf("labels.list carries counts: %+v", l)
		}
	}
	var inbox gmail.Label
	get(t, s, "labels/INBOX", nil, &inbox)
	if inbox.MessagesTotal == 0 || inbox.ThreadsTotal == 0 || inbox.MessagesUnread == 0 {
		t.Fatalf("inbox counts %+v", inbox)
	}
	var e apiError
	if code := get(t, s, "labels/Label_999", nil, &e); code != 404 || e.Error.Errors[0].Reason != "notFound" {
		t.Fatalf("missing label: %d %+v", code, e)
	}
}

func TestMessagesListSearch(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	count := func(q url.Values) int {
		var r gmail.ListMessagesResponse
		if code := get(t, s, "messages", q, &r); code != 200 {
			t.Fatalf("%v: %d", q, code)
		}
		return len(r.Messages)
	}
	plain := s.Scenario(gmailtest.ScenarioPlainThread)
	if n := count(url.Values{"q": {"subject:offsite"}}); n != 3 {
		t.Errorf("subject: %d", n)
	}
	if n := count(url.Values{"q": {"label:projects-offsite"}}); n != 3 {
		t.Errorf("label by name: %d", n)
	}
	if n := count(url.Values{"q": {"label:Label_2"}}); n != 3 {
		t.Errorf("label by id: %d", n)
	}
	if n := count(url.Values{"q": {"is:unread from:bruno subject:offsite"}}); n != 1 {
		t.Errorf("unread from: %d", n)
	}
	if n := count(url.Values{"q": {`"old mill" -from:bruno`}}); n != 2 {
		t.Errorf("phrase with negation: %d", n)
	}
	if n := count(url.Values{"q": {"prize"}}); n != 0 {
		t.Errorf("spam shown by default: %d", n)
	}
	for _, q := range []string{"in:spam", "in:anywhere prize", "label:spam"} {
		if n := count(url.Values{"q": {q}}); n != 1 {
			t.Errorf("%s: %d", q, n)
		}
	}
	if n := count(url.Values{"q": {"prize"}, "includeSpamTrash": {"true"}}); n != 1 {
		t.Errorf("includeSpamTrash: %d", n)
	}
	if n := count(url.Values{"labelIds": {"TRASH"}}); n != 1 {
		t.Errorf("labelIds TRASH: %d", n)
	}
	if n := count(url.Values{"q": {"has:attachment"}}); n != 4 {
		t.Errorf("has:attachment: %d", n)
	}
	if n := count(url.Values{"q": {"to:hanako"}}); n != 0 {
		t.Errorf("to: %d", n)
	}
	if n := count(url.Values{"q": {"cc:eleni"}}); n != 1 {
		t.Errorf("cc: %d", n)
	}
	m, _ := s.Message(plain.MessageIDs[0], "raw")
	raw, _ := base64.URLEncoding.DecodeString(m.Raw)
	pm := mime.ParseRaw(raw)
	if n := count(url.Values{"q": {"rfc822msgid:" + pm.MessageID}}); n != 1 {
		t.Errorf("rfc822msgid: %d", n)
	}
	if n := count(url.Values{"q": {"after:2026/03/02 before:1772500000"}}); n == 0 {
		t.Errorf("after/before: %d", n)
	}
	if n := count(url.Values{"q": {"before:1000"}}); n != 0 {
		t.Errorf("before epoch: %d", n)
	}
	if n := count(url.Values{"q": {"after:garbage"}}); n != 0 {
		t.Errorf("bad date: %d", n)
	}
	for _, q := range []string{"is:starred", "is:important", "is:read", "in:inbox", "in:drafts", "in:sent", "is:nonsense", "has:drive"} {
		_ = count(url.Values{"q": {q}})
	}
	if n := count(url.Values{"q": {"is:starred"}}); n != 1 {
		t.Errorf("starred: %d", n)
	}
	if n := count(url.Values{"q": {"in:drafts"}}); n != 1 {
		t.Errorf("drafts: %d", n)
	}
}

func TestPaging(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	var all []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 50 {
			t.Fatal("paging does not end")
		}
		q := url.Values{"maxResults": {"7"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var r gmail.ListMessagesResponse
		get(t, s, "messages", q, &r)
		for _, m := range r.Messages {
			all = append(all, m.ID)
		}
		if r.NextPageToken == "" {
			break
		}
		token = r.NextPageToken
	}
	var one gmail.ListMessagesResponse
	get(t, s, "messages", url.Values{"maxResults": {"500"}}, &one)
	if len(all) != len(one.Messages) || int(one.ResultSizeEstimate) != len(all) {
		t.Fatalf("paged %d, single %d (estimate %d)", len(all), len(one.Messages), one.ResultSizeEstimate)
	}

	// The empty first page with a token: stopping there is the bug.
	s.EmptyFirstPage = true
	var first gmail.ListThreadsResponse
	get(t, s, "threads", nil, &first)
	if len(first.Threads) != 0 || first.NextPageToken == "" {
		t.Fatalf("empty first page: %+v", first)
	}
	var second gmail.ListThreadsResponse
	get(t, s, "threads", url.Values{"pageToken": {first.NextPageToken}}, &second)
	if len(second.Threads) == 0 {
		t.Fatal("the page after the empty one is empty")
	}

	var e apiError
	if code := get(t, s, "messages", url.Values{"pageToken": {"bogus"}}, &e); code != 400 {
		t.Fatalf("bad token: %d", code)
	}
	if code := get(t, s, "messages", url.Values{"maxResults": {"0"}}, &e); code != 400 {
		t.Fatalf("bad maxResults: %d", code)
	}
}

func TestFormats(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	id := s.Scenario(gmailtest.ScenarioPlainThread).MessageIDs[0]
	var full, meta, minimal, raw gmail.Message
	get(t, s, "messages/"+id, nil, &full)
	get(t, s, "messages/"+id, url.Values{"format": {"metadata"}, "metadataHeaders": {"Subject", "From"}}, &meta)
	get(t, s, "messages/"+id, url.Values{"format": {"minimal"}}, &minimal)
	get(t, s, "messages/"+id, url.Values{"format": {"raw"}}, &raw)
	if full.Payload == nil || full.Payload.Body == nil || full.Raw != "" {
		t.Fatalf("full %+v", full)
	}
	if meta.Payload == nil || len(meta.Payload.Headers) != 2 || meta.Payload.Parts != nil {
		t.Fatalf("metadata %+v", meta.Payload)
	}
	if minimal.Payload != nil || minimal.Raw != "" || minimal.Snippet == "" || minimal.InternalDate == "" {
		t.Fatalf("minimal %+v", minimal)
	}
	if raw.Raw == "" || raw.Payload != nil {
		t.Fatalf("raw %+v", raw)
	}
	var e apiError
	if code := get(t, s, "messages/"+id, url.Values{"format": {"bogus"}}, &e); code != 400 {
		t.Fatalf("bad format %d", code)
	}
	if code := get(t, s, "messages/00000000ffffffff", nil, &e); code != 404 {
		t.Fatalf("missing message %d", code)
	}
	if code := get(t, s, "nothing/here", nil, &e); code != 404 {
		t.Fatalf("unknown route %d", code)
	}
	resp, err := http.Get(s.URL() + "/elsewhere") //nolint:noctx // test
	if err != nil || resp.StatusCode != 404 {
		t.Fatalf("outside prefix: %v", err)
	}
	_ = resp.Body.Close()
}

// TestRawAndFullAgree is the property the fake exists for: both views of
// every generated message parse to the same fields.
func TestRawAndFullAgree(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	for _, name := range s.Scenarios() {
		for _, id := range s.Scenario(name).MessageIDs {
			full, _ := s.Message(id, "full")
			raw, _ := s.Message(id, "raw")
			fetched := map[string][]byte{}
			fromFull, err := mime.ParsePayload(full.Payload, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, need := range fromFull.NeedsFetch {
				b, ok := s.Attachment(need.AttachmentID)
				if !ok {
					t.Fatalf("%s: attachment %s not stored", name, need.AttachmentID)
				}
				fetched[need.PartID] = b
			}
			fromFull, _ = mime.ParsePayload(full.Payload, fetched)
			fromRaw, err := mime.ParseRawBase64URL(raw.Raw)
			if err != nil {
				t.Fatal(err)
			}
			if fromFull.Subject != fromRaw.Subject || !reflect.DeepEqual(fromFull.From, fromRaw.From) ||
				!reflect.DeepEqual(fromFull.To, fromRaw.To) || !reflect.DeepEqual(fromFull.Cc, fromRaw.Cc) ||
				fromFull.MessageID != fromRaw.MessageID || !fromFull.Date.Equal(fromRaw.Date) {
				t.Errorf("%s %s headers differ:\nfull %+v\nraw  %+v", name, id, fromFull, fromRaw)
			}
			if fromFull.Body.Text != fromRaw.Body.Text || fromFull.Body.Source != fromRaw.Body.Source {
				t.Errorf("%s %s bodies differ:\nfull %q\nraw  %q", name, id, fromFull.Body.Text, fromRaw.Body.Text)
			}
			if len(fromFull.Attachments) != len(fromRaw.Attachments) {
				t.Fatalf("%s %s attachments: %d vs %d", name, id, len(fromFull.Attachments), len(fromRaw.Attachments))
			}
			for i := range fromFull.Attachments {
				a, b := fromFull.Attachments[i], fromRaw.Attachments[i]
				if a.Filename != b.Filename || a.MimeType != b.MimeType || a.CalendarMethod != b.CalendarMethod || a.PartID != b.PartID {
					t.Errorf("%s %s attachment %d: %+v vs %+v", name, id, i, a, b)
				}
			}
			if fromFull.Subject == "" || fromFull.Body.Text == "" {
				t.Errorf("%s %s parsed empty: %q %q", name, id, fromFull.Subject, fromFull.Body.Text)
			}
		}
	}
}

func TestScenarioContent(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	parse := func(id string) *mime.Message {
		raw, _ := s.Message(id, "raw")
		m, err := mime.ParseRawBase64URL(raw.Raw)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	intl := s.Scenario(gmailtest.ScenarioInternational)
	want := []string{"Résumé du projet", "Re: プロジェクト概要", "Re: Отчёт по проекту", "Re: 项目概要"}
	for i, id := range intl.MessageIDs {
		if got := parse(id).Subject; got != want[i] {
			t.Errorf("international %d subject %q, want %q", i, got, want[i])
		}
	}
	m1 := parse(intl.MessageIDs[0])
	if m1.From[0].Name != "Zoë Ångström" || m1.Attachments[0].Filename != "résumé.pdf" || m1.Attachments[1].Filename != "年度報告.xlsx" {
		t.Errorf("latin-1 message: %+v %+v", m1.From, m1.Attachments)
	}
	if m := parse(intl.MessageIDs[1]); m.From[0].Name != "山田 花子" || m.Attachments[0].Filename != "会議メモ.txt" || !strings.Contains(m.Body.Text, "資料を確認しました") {
		t.Errorf("japanese message: %+v %+v %q", m.From, m.Attachments, m.Body.Text)
	}
	if m := parse(intl.MessageIDs[2]); m.Attachments[0].Filename != "invoicetxt.exe" || !m.Attachments[0].Renamed || !strings.Contains(m.Body.Text, "Добрый день") {
		t.Errorf("cyrillic message: %+v %q", m.Attachments, m.Body.Text)
	}
	if m := parse(intl.MessageIDs[3]); m.Cc[0].Name != "Ελένη Δοκιμή" || !strings.Contains(m.Body.Text, "收到") {
		t.Errorf("chinese message: %+v %q", m.Cc, m.Body.Text)
	}

	news := parse(s.Scenario(gmailtest.ScenarioNewsletter).MessageIDs[0])
	if news.Body.HiddenChars() < 100 || len(news.Body.Mismatches()) != 1 || strings.Contains(news.Body.Text, "attacker") {
		t.Errorf("newsletter: hidden %v mismatches %+v\n%s", news.Body.Hidden, news.Body.Mismatches(), news.Body.Text)
	}
	ph := parse(s.Scenario(gmailtest.ScenarioPlaceholder).MessageIDs[0])
	if !ph.Body.PlaceholderSkipped || !strings.Contains(ph.Body.Text, "Garden suite") {
		t.Errorf("placeholder: %+v", ph.Body)
	}
	inv := parse(s.Scenario(gmailtest.ScenarioInvite).MessageIDs[0])
	methods := []string{}
	for _, a := range inv.Attachments {
		methods = append(methods, a.CalendarMethod)
	}
	if !slices.Equal(methods, []string{"REQUEST", "REQUEST"}) {
		t.Errorf("invite methods %v", methods)
	}
	long := s.Scenario(gmailtest.ScenarioLongThread)
	if len(long.MessageIDs) != 12 {
		t.Fatalf("long thread %d", len(long.MessageIDs))
	}
	if m := parse(long.MessageIDs[6]); len(m.Body.Spans) == 0 {
		t.Errorf("outlook reply has no spans:\n%s", m.Body.Text)
	}
	if m := parse(long.MessageIDs[11]); len(m.Body.Spans) < 2 {
		t.Errorf("last reply spans %+v", m.Body.Spans)
	}

	full, _ := s.Message(s.Scenario(gmailtest.ScenarioBackedBody).MessageIDs[0], "full")
	backed, _ := mime.ParsePayload(full.Payload, nil)
	if len(backed.NeedsFetch) != 2 || backed.Body.Text != "" {
		t.Errorf("backed body: %+v", backed.NeedsFetch)
	}
}

func TestThreadsAndDrafts(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	sc := s.Scenario(gmailtest.ScenarioDraftReply)
	var th gmail.Thread
	get(t, s, "threads/"+sc.ThreadID, url.Values{"format": {"metadata"}}, &th)
	if len(th.Messages) != 3 || th.HistoryID == "" || th.Messages[0].Payload.Parts != nil {
		t.Fatalf("thread %+v", th)
	}
	var e apiError
	if code := get(t, s, "threads/"+sc.ThreadID, url.Values{"format": {"raw"}}, &e); code != 400 {
		t.Fatalf("raw thread %d", code)
	}
	if code := get(t, s, "threads/"+sc.ThreadID, url.Values{"format": {"x"}}, &e); code != 400 {
		t.Fatalf("bad thread format %d", code)
	}
	if code := get(t, s, "threads/00000000ffffffff", nil, &e); code != 404 {
		t.Fatalf("missing thread %d", code)
	}
	var tl gmail.ListThreadsResponse
	get(t, s, "threads", url.Values{"q": {"subject:budget"}}, &tl)
	if len(tl.Threads) != 1 || tl.Threads[0].ID != sc.ThreadID || tl.Threads[0].Snippet == "" {
		t.Fatalf("threads.list %+v", tl)
	}
	get(t, s, "threads", url.Values{"maxResults": {"x"}}, &e)

	var dl gmail.ListDraftsResponse
	get(t, s, "drafts", nil, &dl)
	if len(dl.Drafts) != 1 || dl.Drafts[0].ID != sc.DraftID || dl.Drafts[0].Message.ThreadID != sc.ThreadID {
		t.Fatalf("drafts %+v", dl)
	}
	var none gmail.ListDraftsResponse
	get(t, s, "drafts", url.Values{"q": {"nothing-matches-this"}}, &none)
	if len(none.Drafts) != 0 {
		t.Fatalf("draft q %+v", none)
	}
	if code := get(t, s, "drafts", url.Values{"maxResults": {"-1"}}, &e); code != 400 {
		t.Fatalf("drafts bad max %d", code)
	}
	var d gmail.Draft
	get(t, s, "drafts/"+sc.DraftID, nil, &d)
	if d.Message == nil || d.Message.ID != sc.MessageIDs[2] || !slices.Contains(d.Message.LabelIDs, "DRAFT") {
		t.Fatalf("draft %+v", d)
	}
	if code := get(t, s, "drafts/r0", nil, &e); code != 404 {
		t.Fatalf("missing draft %d", code)
	}
	if code := get(t, s, "drafts/"+sc.DraftID, url.Values{"format": {"x"}}, &e); code != 400 {
		t.Fatalf("draft bad format %d", code)
	}
	if code := get(t, s, "threads", url.Values{"pageToken": {"page-x"}}, &e); code != 400 {
		t.Fatalf("threads bad token %d", code)
	}
	if code := get(t, s, "drafts", url.Values{"pageToken": {"page-99999"}}, &e); code != 400 {
		t.Fatalf("drafts bad token %d", code)
	}
}

func TestAttachmentsGet(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	id := s.Scenario(gmailtest.ScenarioBackedBody).MessageIDs[0]
	full, _ := s.Message(id, "full")
	att := full.Payload.Parts[0].Body.AttachmentID
	var body gmail.MessagePartBody
	if code := get(t, s, "messages/"+id+"/attachments/"+att, nil, &body); code != 200 || body.Size == 0 || body.Data == "" {
		t.Fatalf("attachment %d %+v", code, body)
	}
	var e apiError
	other := s.Scenario(gmailtest.ScenarioPlainThread).MessageIDs[0]
	if code := get(t, s, "messages/"+other+"/attachments/"+att, nil, &e); code != 400 {
		t.Fatalf("attachment of another message %d", code)
	}
	if code := get(t, s, "messages/00000000ffffffff/attachments/"+att, nil, &e); code != 404 {
		t.Fatalf("attachment of missing message %d", code)
	}
}

func TestHistory(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	var h gmail.ListHistoryResponse
	if code := get(t, s, "history", url.Values{"startHistoryId": {"1000"}}, &h); code != 200 {
		t.Fatalf("history %d", code)
	}
	var added, labelAdded, labelRemoved int
	for _, r := range h.History {
		added += len(r.MessagesAdded)
		labelAdded += len(r.LabelsAdded)
		labelRemoved += len(r.LabelsRemoved)
	}
	if added == 0 || labelAdded != 1 || labelRemoved != 1 {
		t.Fatalf("records: added %d labelAdded %d labelRemoved %d", added, labelAdded, labelRemoved)
	}
	var only gmail.ListHistoryResponse
	get(t, s, "history", url.Values{"startHistoryId": {"1000"}, "historyTypes": {"labelRemoved"}}, &only)
	if len(only.History) != 1 {
		t.Fatalf("historyTypes filter: %d", len(only.History))
	}
	get(t, s, "history", url.Values{"startHistoryId": {"1000"}, "labelId": {"STARRED"}}, &only)
	if len(only.History) == 0 {
		t.Fatal("labelId filter found nothing")
	}
	var now gmail.ListHistoryResponse
	get(t, s, "history", url.Values{"startHistoryId": {h.HistoryID}}, &now)
	if len(now.History) != 0 || now.HistoryID != h.HistoryID {
		t.Fatalf("from now: %+v", now)
	}
	var e apiError
	if code := get(t, s, "history", url.Values{"startHistoryId": {"1"}}, &e); code != 404 || e.Error.Errors[0].Reason != "notFound" {
		t.Fatalf("expired cursor: %d %+v", code, e)
	}
	if code := get(t, s, "history", nil, &e); code != 400 {
		t.Fatalf("missing start: %d", code)
	}
	if code := get(t, s, "history", url.Values{"startHistoryId": {"1000"}, "maxResults": {"x"}}, &e); code != 400 {
		t.Fatalf("bad max: %d", code)
	}
	if code := get(t, s, "history", url.Values{"startHistoryId": {"1000"}, "pageToken": {"x"}}, &e); code != 400 {
		t.Fatalf("bad token: %d", code)
	}
	if s.HistoryID() == 0 {
		t.Fatal("history id")
	}
}

func TestUnitsAndFailures(t *testing.T) {
	s := gmailtest.New()
	defer s.Close()
	get(t, s, "profile", nil, nil)
	get(t, s, "messages", nil, nil)
	get(t, s, "threads/"+s.Scenario(gmailtest.ScenarioPlainThread).ThreadID, nil, nil)
	if s.Units() != 1+5+40 || len(s.Calls()) != 3 || s.Calls()[2].Method != "gmail.users.threads.get" {
		t.Fatalf("units %d calls %+v", s.Units(), s.Calls())
	}
	s.ResetAccounting()
	if s.Units() != 0 || len(s.Calls()) != 0 {
		t.Fatal("reset")
	}

	s.Fail(gmailtest.Failure{Method: "gmail.users.messages.list", Status: 429, Reason: "rateLimitExceeded", RetryAfter: "3"})
	var e apiError
	if code := get(t, s, "profile", nil, nil); code != 200 {
		t.Fatalf("unmatched method failed: %d", code)
	}
	if code := get(t, s, "messages", nil, &e); code != 429 || e.RetryAfter != "3" || e.Error.Errors[0].Reason != "rateLimitExceeded" || e.Error.Status != "RESOURCE_EXHAUSTED" {
		t.Fatalf("429: %d %+v", code, e)
	}
	if code := get(t, s, "messages", nil, nil); code != 200 {
		t.Fatalf("failure did not clear: %d", code)
	}

	s.Fail(gmailtest.Failure{Status: 503, Reason: "backendError", Times: 2})
	for range 2 {
		if code := get(t, s, "labels", nil, &e); code != 503 {
			t.Fatalf("503: %d", code)
		}
	}
	if code := get(t, s, "labels", nil, nil); code != 200 {
		t.Fatalf("after 503s: %d", code)
	}

	// A fresh connection, so the transport cannot retry the GET on
	// another one and hide the reset.
	s.Fail(gmailtest.Failure{Reset: true})
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(s.URL() + "/gmail/v1/users/me/profile") //nolint:noctx // test
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("reset delivered a response")
	}
}
