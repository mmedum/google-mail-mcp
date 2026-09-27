package service_test

import (
	"bytes"
	"context"
	"strconv"
	"testing"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/internal/model"
	"github.com/mmedum/google-mail-mcp/internal/service"
)

func TestChangesSinceAHistoryID(t *testing.T) {
	s, fake := newService(t)
	ctx := context.Background()
	start := strconv.FormatUint(fake.HistoryID()-4, 10)
	c, err := s.Changes(ctx, service.ChangesQuery{HistoryID: start})
	if err != nil {
		t.Fatal(err)
	}
	if c.Expired || len(c.Changes) != 4 || c.NextPageToken != "" {
		t.Fatalf("changes = %+v; want the last four, complete", c)
	}
	if c.HistoryID != strconv.FormatUint(fake.HistoryID(), 10) {
		t.Errorf("history_id = %s; want the current %d", c.HistoryID, fake.HistoryID())
	}
	for _, ch := range c.Changes {
		if ch.MessageID == "" || ch.HistoryID == "" {
			t.Errorf("a change without ids: %+v", ch)
		}
	}
}

func TestChangesNameLabelsAndFilterByOne(t *testing.T) {
	s, fake := newService(t)
	ctx := context.Background()
	c, err := s.Changes(ctx, service.ChangesQuery{HistoryID: "1000", Label: "projects",
		Kinds: []string{"added", "added"}, Max: 50})
	if err != nil {
		t.Fatal(err)
	}
	if c.Label == nil || c.Label.Name != "Projects" {
		t.Fatalf("label = %+v; want Projects resolved from its name", c.Label)
	}
	want := len(fake.Scenario(gmailtest.ScenarioInternational).MessageIDs)
	if len(c.Changes) != want {
		t.Fatalf("%d changes; want the %d added messages labeled Projects", len(c.Changes), want)
	}
	for _, ch := range c.Changes {
		if ch.Kind != model.ChangeAdded {
			t.Errorf("kind %s; only added was asked for", ch.Kind)
		}
		named := false
		for _, l := range ch.Labels {
			named = named || l.Name == "Projects"
		}
		if !named {
			t.Errorf("change %+v does not name the Projects label", ch)
		}
	}
	q := calls(fake, "gmail.users.history.list")
	if len(q) != 1 || !bytes.Contains([]byte(q[0].Query), []byte("historyTypes=messageAdded")) ||
		bytes.Count([]byte(q[0].Query), []byte("historyTypes")) != 1 {
		t.Errorf("history.list query = %v; want one historyTypes=messageAdded", q)
	}
}

// §7.6: an expired cursor is said, with the id to restart from, and is
// never an empty list.
func TestAnExpiredCursorIsReportedNotEmpty(t *testing.T) {
	s, fake := newService(t)
	c, err := s.Changes(context.Background(), service.ChangesQuery{HistoryID: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Expired || c.HistoryID != strconv.FormatUint(fake.HistoryID(), 10) {
		t.Fatalf("changes = %+v; want expired with the current history id", c)
	}
	if len(calls(fake, "gmail.users.getProfile")) != 1 {
		t.Errorf("the restart point was not read from the profile")
	}
}

func TestChangesRefusals(t *testing.T) {
	s, fake := newService(t)
	ctx := context.Background()
	for name, q := range map[string]service.ChangesQuery{
		"no history id":    {},
		"not a number":     {HistoryID: "abc"},
		"negative":         {HistoryID: "-4"},
		"max over the cap": {HistoryID: "1000", Max: 501},
		"unknown kind":     {HistoryID: "1000", Kinds: []string{"moved"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Changes(ctx, q)
			wantClass(t, err, gapi.ClassInvalid)
		})
	}
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("refused queries made %d requests", n)
	}
	_, err := s.Changes(ctx, service.ChangesQuery{HistoryID: "1000", Label: "No such label"})
	wantClass(t, err, gapi.ClassNotFound)
}

func TestAHistoryFailureOtherThanExpiryKeepsItsClass(t *testing.T) {
	s, fake := newService(t)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.history.list", Status: 403, Reason: "forbidden", Message: "no"})
	_, err := s.Changes(context.Background(), service.ChangesQuery{HistoryID: "1000"})
	wantClass(t, err, gapi.ClassForbidden)
}

func TestSettings(t *testing.T) {
	s, fake := newService(t)
	fake.UpdateSettings(func(st *gmailtest.Settings) {
		st.AutoForwarding.Enabled, st.AutoForwarding.EmailAddress = true, gmailtest.BackupAddress
		st.Vacation.ResponseBodyPlainText = ""
		st.Vacation.ResponseBodyHTML = `<p>Away.</p><p style="display:none">hidden</p>`
	})
	st, err := s.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.AutoForwarding.Enabled || st.AutoForwarding.Address != gmailtest.BackupAddress {
		t.Errorf("auto-forwarding = %+v", st.AutoForwarding)
	}
	if !st.Vacation.BodyFromHTML || st.Vacation.Body != "Away." || st.Vacation.Start.IsZero() {
		t.Errorf("vacation = %+v; want the HTML body as text, hidden text dropped, and dates", st.Vacation)
	}
	if len(st.SendAs) != 2 || st.SendAs[0].Signature != "Rae Reader\nExample Org" {
		t.Errorf("send-as = %+v; want the signature as text", st.SendAs)
	}
	if got := fake.Units(); got != 7 {
		t.Errorf("settings spent %d units; want 7", got)
	}
}

func TestFiltersNameTheirLabels(t *testing.T) {
	s, _ := newService(t)
	fs, err := s.Filters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 3 {
		t.Fatalf("%d filters; want 3", len(fs))
	}
	byID := map[string]model.Filter{}
	for _, f := range fs {
		byID[f.ID] = f
	}
	if f := byID[gmailtest.FilterNewsletters]; len(f.Add) != 1 || f.Add[0].Name != "Newsletters" || f.Remove[0].Name != "INBOX" {
		t.Errorf("newsletters filter = %+v", f)
	}
	if f := byID[gmailtest.FilterForward]; f.Forward != gmailtest.BackupAddress {
		t.Errorf("forwarding filter = %+v", f)
	}
}

// A label deleted between its resolution and history.list is a 404 too.
// It is reported as the label's, never as an expired cursor that would
// move the caller past changes it can still read.
func TestALabelDeletedMidCallIsNotAnExpiredCursor(t *testing.T) {
	s, fake := newService(t)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.history.list", Status: 404, Reason: "notFound",
		Then: func(f *gmailtest.Server) { f.RemoveLabel("Label_1") }})
	_, err := s.Changes(context.Background(), service.ChangesQuery{HistoryID: "1000", Label: "Projects"})
	wantClass(t, err, gapi.ClassNotFound)
	if n := len(calls(fake, "gmail.users.getProfile")); n != 0 {
		t.Errorf("the profile was read for a restart point %d times; the cursor did not expire", n)
	}

	// With the label still there, the same 404 is the cursor's.
	s, fake = newService(t)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.history.list", Status: 404, Reason: "notFound"})
	c, err := s.Changes(context.Background(), service.ChangesQuery{HistoryID: " 1000 ", Label: "Projects"})
	if err != nil || !c.Expired || c.Start != "1000" {
		t.Fatalf("changes = %+v, %v; want expired, from the trimmed start", c, err)
	}
}
