package service_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/service"
)

func newService(t *testing.T) (*service.Service, *gmailtest.Server) {
	t.Helper()
	fake := gmailtest.New()
	t.Cleanup(fake.Close)
	c := gapi.New(gapi.Options{
		BaseURL:     fake.URL(),
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}),
		Sleep:       func(context.Context, time.Duration) error { return nil },
	})
	return service.New(c), fake
}

func wantClass(t *testing.T, err error, c gapi.Class) {
	t.Helper()
	got, ok := gapi.ClassOf(err)
	if !ok || got != c {
		t.Fatalf("err = %v; want class %s", err, c)
	}
}

func calls(fake *gmailtest.Server, method string) []gmailtest.Call {
	var out []gmailtest.Call
	for _, c := range fake.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func TestProfile(t *testing.T) {
	s, _ := newService(t)
	p, err := s.Profile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Email != gmailtest.Account || p.HistoryID == "" {
		t.Errorf("profile = %+v", p)
	}
}

func TestSearchThreadsDefaultsToTwentyAndReadsHeadersOnly(t *testing.T) {
	s, fake := newService(t)
	got, searched, err := s.SearchThreads(context.Background(), service.Search{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Threads) == 0 {
		t.Fatal("no threads")
	}
	if searched.Q != "" {
		t.Errorf("q = %q; want empty", searched.Q)
	}
	list := calls(fake, "gmail.users.threads.list")
	if len(list) != 1 || !strings.Contains(list[0].Query, "maxResults=20") {
		t.Errorf("threads.list calls = %+v; want one with maxResults=20", list)
	}
	for _, c := range calls(fake, "gmail.users.threads.get") {
		if !strings.Contains(c.Query, "format=metadata") {
			t.Errorf("threads.get %q is not a metadata read", c.Query)
		}
	}
}

func TestSearchResolvesLabelNamesAndAppendsEpochBounds(t *testing.T) {
	s, fake := newService(t)
	loc, err := service.ParseZone("Europe/Copenhagen")
	if err != nil {
		t.Fatal(err)
	}
	_, searched, err := s.SearchMessages(context.Background(), service.Search{
		Q: "from:ada", After: "2026-03-01", Location: loc,
		Before: "2026-04-01T00:00:00Z", Labels: []string{"projects", "inbox"},
	})
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 3, 1, 0, 0, 0, 0, time.FixedZone("CET", 3600)).Unix()
	want := "from:ada after:" + strconv.FormatInt(after, 10) + " before:1775001600"
	if searched.Q != want {
		t.Errorf("q = %q; want %q", searched.Q, want)
	}
	list := calls(fake, "gmail.users.messages.list")
	if len(list) != 1 || !strings.Contains(list[0].Query, "labelIds=Label_1") || !strings.Contains(list[0].Query, "labelIds=INBOX") {
		t.Errorf("messages.list = %+v; want both labels resolved to ids", list)
	}
}

func TestSearchRefusals(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		in   service.Search
		want gapi.Class
	}{
		{"unknown label", service.Search{Labels: []string{"no such label"}}, gapi.ClassNotFound},
		{"bad date", service.Search{After: "yesterday"}, gapi.ClassInvalid},
		{"inverted bounds", service.Search{After: "2026-02-01", Before: "2026-01-01"}, gapi.ClassInvalid},
		{"max too large", service.Search{Max: service.MaxMax + 1}, gapi.ClassInvalid},
		{"negative max", service.Search{Max: -1}, gapi.ClassInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := s.SearchThreads(ctx, tc.in)
			wantClass(t, err, tc.want)
		})
	}
}

func TestParseZone(t *testing.T) {
	if loc, err := service.ParseZone(""); loc != nil || err != nil {
		t.Errorf("ParseZone(\"\") = %v, %v; want nil, nil", loc, err)
	}
	if loc, err := service.ParseZone("Europe/Copenhagen"); err != nil || loc.String() != "Europe/Copenhagen" {
		t.Errorf("ParseZone(Europe/Copenhagen) = %v, %v", loc, err)
	}
	_, err := service.ParseZone("Mars/Olympus")
	wantClass(t, err, gapi.ClassInvalid)
	if !strings.Contains(err.Error(), "not an IANA zone name") {
		t.Errorf("err = %v", err)
	}
}

// A search that is refused on its own terms sends nothing.
func TestInvalidSearchSendsNothing(t *testing.T) {
	s, fake := newService(t)
	_, _, err := s.SearchMessages(context.Background(), service.Search{Max: -1})
	wantClass(t, err, gapi.ClassInvalid)
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("%d calls for a refused search; want 0", n)
	}
}

func TestThreadFetchesBodyPartsBehindAttachmentIDs(t *testing.T) {
	s, fake := newService(t)
	sc := fake.Scenario(gmailtest.ScenarioBackedBody)
	th, err := s.Thread(context.Background(), sc.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls(fake, "gmail.users.messages.attachments.get")) == 0 {
		t.Fatal("no attachments.get: the backed body part was never fetched")
	}
	for _, m := range th.Messages {
		if len(m.NeedsFetch) != 0 {
			t.Errorf("message %s still needs %d parts", m.ID, len(m.NeedsFetch))
		}
		if strings.TrimSpace(m.Body.Text) == "" {
			t.Errorf("message %s has an empty body", m.ID)
		}
	}
}

func TestMessageByRFC822ID(t *testing.T) {
	s, fake := newService(t)
	sc := fake.Scenario(gmailtest.ScenarioPlainThread)
	id := sc.MessageIDs[0]
	m, err := s.Message(context.Background(), "rfc822:<fixture."+id+"@mail.example.com>")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != id || !m.Complete {
		t.Errorf("got %s complete=%v; want %s complete", m.ID, m.Complete, id)
	}

	_, err = s.Message(context.Background(), "rfc822:<nobody@example.invalid>")
	wantClass(t, err, gapi.ClassNotFound)
	_, err = s.Message(context.Background(), "rfc822:  ")
	wantClass(t, err, gapi.ClassInvalid)
}

func TestMessageNotFound(t *testing.T) {
	s, _ := newService(t)
	_, err := s.Message(context.Background(), "00000000000fffff")
	wantClass(t, err, gapi.ClassNotFound)
}

func TestLabelsWithCountsReadsEachLabel(t *testing.T) {
	s, fake := newService(t)
	ls, err := s.Labels(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(calls(fake, "gmail.users.labels.get")); n != len(ls) || n == 0 {
		t.Errorf("labels.get calls = %d for %d labels", n, len(ls))
	}
	for _, l := range ls {
		if !l.HasCounts {
			t.Errorf("label %s has no counts", l.ID)
		}
	}
	fake.ResetAccounting()
	if _, err := s.Labels(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := len(calls(fake, "gmail.users.labels.get")); n != 0 {
		t.Errorf("labels.get calls without counts = %d; want 0", n)
	}
}

func TestDrafts(t *testing.T) {
	s, fake := newService(t)
	list, err := s.Drafts(context.Background(), "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Drafts) == 0 {
		t.Fatal("no drafts")
	}
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	d, err := s.Draft(context.Background(), sc.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != sc.DraftID || d.Message.ID == "" || !d.Message.Complete {
		t.Errorf("draft = %s message %q complete=%v", d.ID, d.Message.ID, d.Message.Complete)
	}
	_, err = s.Drafts(context.Background(), "", service.MaxMax+1, "")
	wantClass(t, err, gapi.ClassInvalid)
}

func TestUpstreamFailuresKeepTheirClass(t *testing.T) {
	s, fake := newService(t)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.labels.list", Status: 404, Reason: "notFound", Times: 100})
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"profile": func() error {
			fake.Fail(gmailtest.Failure{Method: "gmail.users.getProfile", Status: 404})
			_, err := s.Profile(ctx)
			return err
		},
		"threads":  func() error { _, _, err := s.SearchThreads(ctx, service.Search{}); return err },
		"messages": func() error { _, _, err := s.SearchMessages(ctx, service.Search{}); return err },
		"thread":   func() error { _, err := s.Thread(ctx, "x"); return err },
		"message":  func() error { _, err := s.Message(ctx, "x"); return err },
		"labels":   func() error { _, err := s.Labels(ctx, false); return err },
		"drafts":   func() error { _, err := s.Drafts(ctx, "", 0, ""); return err },
		"draft":    func() error { _, err := s.Draft(ctx, "x"); return err },
	} {
		err := call()
		var e *gapi.Error
		if !errors.As(err, &e) || e.Class != gapi.ClassNotFound {
			t.Errorf("%s: err = %v; want [not_found]", name, err)
		}
	}
}

// Only the message whose body was behind an attachment id is rebuilt;
// a part that fails to fetch fails the read with its class.
func TestMessageFetchesItsBackedParts(t *testing.T) {
	s, fake := newService(t)
	sc := fake.Scenario(gmailtest.ScenarioBackedBody)
	var backed string
	for _, id := range sc.MessageIDs {
		fake.ResetAccounting()
		m, err := s.Message(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.NeedsFetch) != 0 || strings.TrimSpace(m.Body.Text) == "" {
			t.Errorf("message %s: %d parts unfetched, body %q", id, len(m.NeedsFetch), m.Body.Text)
		}
		if len(calls(fake, "gmail.users.messages.attachments.get")) > 0 {
			backed = id
		}
	}
	if backed == "" {
		t.Fatal("no message in the scenario fetched a part")
	}
	fake.Fail(gmailtest.Failure{Method: "gmail.users.messages.attachments.get", Status: 404, Reason: "notFound"})
	_, err := s.Message(context.Background(), backed)
	wantClass(t, err, gapi.ClassNotFound)
}

// A thread's every message, and a draft, is read with the body parts
// Gmail kept behind attachment ids, not only the first one.
func TestBackedBodiesAreFetchedForEachMessage(t *testing.T) {
	s, fake := newService(t)
	threadID, draftID := fake.AddBackedThread()
	ctx := context.Background()
	th, err := s.Thread(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, m := range th.Messages {
		bodies = append(bodies, strings.TrimSpace(m.Body.Text))
	}
	if want := []string{"Backed body 1.", "Backed body 2.", "Backed draft body."}; !slices.Equal(bodies, want) {
		t.Errorf("thread bodies %q; want %q", bodies, want)
	}
	d, err := s.Draft(ctx, draftID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(d.Message.Body.Text); got != "Backed draft body." || len(d.Message.NeedsFetch) != 0 {
		t.Errorf("draft body %q, %d parts unfetched; want the backed body, all fetched", got, len(d.Message.NeedsFetch))
	}
}

// The label list is read alongside the read it serves, and its failure
// is the one reported even when the read itself succeeded.
func TestALabelListFailureIsReportedFirst(t *testing.T) {
	s, fake := newService(t)
	sc := fake.Scenario(gmailtest.ScenarioPlainThread)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.labels.list", Status: 403, Reason: "forbidden", Times: 100})
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"thread":  func() error { _, err := s.Thread(ctx, sc.ThreadID); return err },
		"message": func() error { _, err := s.Message(ctx, sc.MessageIDs[0]); return err },
		"search":  func() error { _, _, err := s.SearchMessages(ctx, service.Search{}); return err },
		"labeled": func() error {
			_, _, err := s.SearchMessages(ctx, service.Search{Labels: []string{"INBOX"}})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { wantClass(t, call(), gapi.ClassForbidden) })
	}
}
