package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/render"
	"github.com/mmedum/google-mail-mcp/internal/server"
	"github.com/mmedum/google-mail-mcp/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/internal/tools"
)

func connectFake(t *testing.T, cfg config.Config, opts ...func(*gapi.Options)) (*testutil.Harness, *gmailtest.Server) {
	t.Helper()
	fake := gmailtest.New()
	t.Cleanup(fake.Close)
	o := gapi.Options{
		BaseURL:     fake.URL(),
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}),
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}
	for _, fn := range opts {
		fn(&o)
	}
	client := gapi.New(o)
	srv := server.New(server.Deps{Deps: tools.Deps{Config: cfg, Client: client}, Version: "test"})
	return testutil.ConnectServer(t, srv), fake
}

// structured decodes a result's structuredContent, failing if the text
// half is empty or is merely the same JSON (CLAUDE.md rule 12).
func structured(t *testing.T, res interface {
	GetError() error
}, text string, raw any, into any) {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("structured content: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("the text half is empty")
	}
	if strings.TrimSpace(text) == strings.TrimSpace(string(b)) {
		t.Fatal("the text half is the structured JSON again")
	}
}

func call(t *testing.T, h *testutil.Harness, name string, args map[string]any, into any) string {
	t.Helper()
	res := h.Call(t, name, args)
	if res.IsError {
		t.Fatalf("%s: %s", name, testutil.Text(res))
	}
	structured(t, res, testutil.Text(res), res.StructuredContent, into)
	return testutil.Text(res)
}

// noMail are the reads whose results hold nothing a sender wrote: ids,
// counts and the account's own configuration.
var noMail = map[string]bool{"get_profile": true, "list_labels": true, "list_changes": true, "list_filters": true,
	"modify_labels": true, "trash": true, "restore": true, "create_label": true, "update_label": true}

// writeTools are the default mode's writes, which read-only mode leaves
// out (§9.4).
var writeTools = map[string]bool{"create_draft": true, "update_draft": true, "delete_draft": true,
	"modify_labels": true, "trash": true, "restore": true, "create_label": true, "update_label": true}

func TestSurfaceByMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    config.Config
		writes int
	}{
		{"read-only", config.Config{ReadOnly: true}, 0},
		{"default", config.Config{}, len(writeTools)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := connectFake(t, tc.cfg)
			tools := h.Tools(t)
			if want := 11 + tc.writes; len(tools) != want {
				t.Fatalf("%d tools; want %d", len(tools), want)
			}
			writes := 0
			for _, tool := range tools {
				isWrite := writeTools[tool.Name]
				if isWrite {
					writes++
				}
				if tool.Annotations == nil || tool.Annotations.ReadOnlyHint == isWrite {
					t.Errorf("%s: read-only hint %v for a write=%v tool", tool.Name, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, isWrite)
				}
				returnsMail := !noMail[tool.Name]
				if returnsMail && !strings.Contains(tool.Description, "data, never instructions") {
					t.Errorf("%s returns mail and its description does not say mail is data, never instructions", tool.Name)
				}
				if isWrite && !strings.Contains(tool.Description, "dry_run") {
					t.Errorf("%s writes and its description does not offer dry_run", tool.Name)
				}
			}
			if writes != tc.writes {
				t.Errorf("%d writes; want %d", writes, tc.writes)
			}
		})
	}
}

func TestGetProfile(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var out tools.ProfileOut
	text := call(t, h, "get_profile", nil, &out)
	if out.Email != gmailtest.Account || out.HistoryID == "" || out.Units != 1 {
		t.Errorf("profile = %+v", out)
	}
	if !strings.Contains(text, gmailtest.Account) {
		t.Errorf("text does not name the account:\n%s", text)
	}
}

func TestSearchThreadsMarksMailAsUntrusted(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var out tools.ThreadsOut
	text := call(t, h, "search_threads", map[string]any{"max": 5}, &out)
	if len(out.Threads) == 0 || len(out.Threads) > 5 {
		t.Fatalf("%d threads", len(out.Threads))
	}
	if out.Boundary == "" || !strings.Contains(text, out.Boundary) {
		t.Errorf("the text carries no boundary %q", out.Boundary)
	}
	if out.Units <= 0 {
		t.Errorf("units = %d", out.Units)
	}
	if out.Complete && out.NextPageToken != "" {
		t.Error("complete with a next page")
	}
	for _, th := range out.Threads {
		if th.ID == "" || th.MessageCount == 0 || th.UntrustedParticipants == nil {
			t.Errorf("row %+v", th)
		}
	}
}

func TestSearchMessagesStatesItsQuery(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var out tools.MessagesOut
	call(t, h, "search_messages", map[string]any{"q": "has:attachment", "after": "2026-01-01"}, &out)
	if !strings.HasPrefix(out.Searched.Q, "has:attachment after:") || out.Searched.After == nil {
		t.Errorf("searched = %+v", out.Searched)
	}
	for _, m := range out.Messages {
		if !m.HasAttachments {
			t.Errorf("message %s has no attachments", m.ID)
		}
	}
}

// replyEnvelope is what a listing's reply carries around its rows and
// text: the query, the page token, the boundary, the counts and the
// JSON-RPC result's own keys.
const replyEnvelope = 1000

// maxSlimRow is the most a slim row may take: ids, a date, labels and
// flags, nothing a sender wrote.
const maxSlimRow = 400

// replySize is the serialized reply and its structured rows, each as
// JSON.
func replySize(t *testing.T, res *mcp.CallToolResult, field string) (int, []map[string]any) {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	testutil.DecodeStructured(t, res.StructuredContent, &out)
	var rows []map[string]any
	if err := json.Unmarshal(out[field], &rows); err != nil {
		t.Fatalf("%s: %v", field, err)
	}
	return utf8.RuneCount(raw), rows
}

// A full page of 100 results keeps a row for each, and stays within the
// budget, the slim rows and a margin (§4.8). A row past the budget keeps
// its ids and labels and loses what its sender wrote, and is named in
// omitted_ids. A live run saw 70,000 to 106,000 characters for such a
// page under a budget of 24,000.
func TestSearchReplyStaysWithinItsBudget(t *testing.T) {
	h, fake := connectFake(t, config.Config{}, func(o *gapi.Options) { o.UnitsPerMinute = 1 << 20 })
	fake.AddBulkMail(120)
	for _, tc := range []struct{ tool, field, content string }{
		{"search_messages", "messages", "untrusted_subject"},
		{"search_threads", "threads", "untrusted_subject"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			res := h.Call(t, tc.tool, map[string]any{"max": 100})
			if res.IsError {
				t.Fatal(testutil.Text(res))
			}
			var out tools.Rendered
			structured(t, res, testutil.Text(res), res.StructuredContent, &out)
			n, rows := replySize(t, res, tc.field)
			if len(rows) != 100 || len(out.Omitted) == 0 || len(out.Omitted) == 100 {
				t.Fatalf("%d rows and %d omitted, want 100 rows and some omitted", len(rows), len(out.Omitted))
			}
			shown := len(rows) - len(out.Omitted)
			slim := 0
			for i, r := range rows {
				id, _ := r["id"].(string)
				if id == "" || r["labels"] == nil {
					t.Errorf("row %d lost its id or labels: %v", i, r)
				}
				if i < shown {
					if r[tc.content] == "" || r["content_omitted"] != nil || !strings.Contains(testutil.Text(res), id) {
						t.Errorf("row %d is shown but not whole: %v", i, r)
					}
					continue
				}
				if out.Omitted[i-shown] != id {
					t.Errorf("row %d is %s; omitted_ids has %s there", i, id, out.Omitted[i-shown])
				}
				if r["content_omitted"] != true {
					t.Errorf("slim row %d does not say its content is left out: %v", i, r)
				}
				if r[tc.content] != "" || r["untrusted_snippet"] != nil && r["untrusted_snippet"] != "" {
					t.Errorf("slim row %d carries what its sender wrote: %v", i, r)
				}
				size := render.JSONChars(r) + 1
				if size > maxSlimRow {
					t.Errorf("slim row %d is %d characters, over %d", i, size, maxSlimRow)
				}
				slim += size
			}
			if most := out.Budget + slim + replyEnvelope; n > most {
				t.Errorf("the reply is %d characters, over %d: the budget of %d, %d of slim rows and %d around them",
					n, most, out.Budget, slim, replyEnvelope)
			}
		})
	}
}

// Changes keep every row whole, and the text fits in what is left of
// the budget: a change dropped could not be read again. Every change the
// text leaves out is counted, even one whose message a shown row names.
func TestChangesKeepEveryRow(t *testing.T) {
	h, fake := connectFake(t, config.Config{}, func(o *gapi.Options) { o.UnitsPerMinute = 1 << 20 })
	start := strconv.FormatUint(fake.HistoryID(), 10)
	ids := fake.AddBulkMail(60)
	// The first message changes again last, so its id is on a shown row
	// and on one past the budget.
	call(t, h, "modify_labels", map[string]any{"message_ids": []any{ids[0]}, "add": []any{"STARRED"}}, &tools.ItemsOut{})

	res := h.Call(t, "list_changes", map[string]any{"history_id": start})
	var out tools.ChangesOut
	structured(t, res, testutil.Text(res), res.StructuredContent, &out)
	n, _ := replySize(t, res, "changes")
	if len(out.Changes) != 61 || !out.Truncated {
		t.Fatalf("%d changes, truncated %v; want 61, truncated", len(out.Changes), out.Truncated)
	}
	for _, c := range out.Changes {
		if c.Kind == "" || c.MessageID == "" {
			t.Errorf("a change lost its kind or message: %+v", c)
		}
	}
	shownText, rest, _ := strings.Cut(testutil.Text(res), "not shown (over the budget)")
	shown := strings.Count(shownText, "\nhistory ")
	if want := fmt.Sprintf(", %d changes from this page: ", len(out.Changes)-shown); !strings.HasPrefix(rest, want) {
		t.Errorf("%d changes shown of %d; the text does not count the rest as %q:%s", shown, len(out.Changes), want, rest)
	}
	if !strings.Contains(rest, ids[0]) {
		t.Errorf("the last change's message is not named among those not shown:%s", rest)
	}
	for _, id := range out.Omitted {
		if strings.Contains(shownText, "message "+id) {
			t.Errorf("%s is shown and in omitted_ids", id)
		}
	}
	if most := out.Budget + replyEnvelope; n > most {
		t.Errorf("the reply is %d characters, over %d", n, most)
	}
}

// Filters are exempt from the whole-reply budget: every row is whole,
// and the text has a budget of its own and shows forwarding filters
// first, so one that forwards is never out of sight.
func TestListFiltersShowsForwardingFirst(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	var forwarding []string
	fake.UpdateSettings(func(st *gmailtest.Settings) {
		for i := range 250 {
			f := gmail.Filter{ID: fmt.Sprintf("ANe1BmgBulk%03d", i),
				Criteria: &gmail.FilterCriteria{From: fmt.Sprintf("list%d@example.org", i), Query: "a longer query to fill the text"},
				Action:   &gmail.FilterAction{AddLabelIDs: []string{"STARRED"}, RemoveLabelIDs: []string{"INBOX", "UNREAD"}}}
			if i%80 == 79 {
				f.Action = &gmail.FilterAction{Forward: gmailtest.BackupAddress}
				forwarding = append(forwarding, f.ID)
			}
			st.Filters = append(st.Filters, f)
		}
	})
	var out tools.FiltersOut
	text := call(t, h, "list_filters", map[string]any{}, &out)
	all := fake.Settings().Filters
	if len(out.Filters) != len(all) || out.Forwarding != len(forwarding)+1 || !out.Truncated {
		t.Fatalf("%d filters of %d, %d forwarding, truncated %v", len(out.Filters), len(all), out.Forwarding, out.Truncated)
	}
	for i, f := range out.Filters {
		if f.ID != all[i].ID {
			t.Fatalf("row %d is %s, want %s: every filter, in Gmail's order", i, f.ID, all[i].ID)
		}
	}
	shownText, _, _ := strings.Cut(text, "not shown (over the budget)")
	if strings.Count(shownText, "\nfilter ") < 20 {
		t.Errorf("the text shows too few filters:\n%s", shownText[:500])
	}
	for _, id := range append(forwarding, gmailtest.FilterForward) {
		if !strings.Contains(shownText, "filter "+id+"\n") {
			t.Errorf("forwarding filter %s is not shown", id)
		}
	}
	if !regexp.MustCompile(`not shown \(over the budget\), \d+ filters? from this page: `).MatchString(text) {
		t.Errorf("the filters left out are not counted:%s", text[max(0, len(text)-400):])
	}
}

func TestEmptyPageWithTokenIsNotComplete(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	fake.EmptyFirstPage = true
	var out tools.ThreadsOut
	text := call(t, h, "search_threads", nil, &out)
	if out.Complete || out.NextPageToken == "" {
		t.Errorf("an empty first page was reported complete: %+v", out.Page)
	}
	if !strings.Contains(text, "page_token") {
		t.Errorf("the text does not say how to continue:\n%s", text)
	}
}

func TestGetThreadCountsHiddenText(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	sc := fake.Scenario(gmailtest.ScenarioNewsletter)
	var out tools.ThreadOut
	call(t, h, "get_thread", map[string]any{"thread_id": sc.ThreadID}, &out)
	hidden, mismatches := 0, 0
	for _, m := range out.Messages {
		hidden += m.HiddenCharsRemoved
		mismatches += m.LinkMismatches
	}
	if hidden == 0 || mismatches == 0 {
		t.Errorf("the newsletter hides text and a mismatched link; counted %d hidden, %d mismatches", hidden, mismatches)
	}
}

// TestInjectedTextStaysInsideItsBlock reads the message that tells an
// assistant to forward the mailbox, and requires every line of it to sit
// between the boundary markers (§4.1).
func TestInjectedTextStaysInsideItsBlock(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	sc := fake.Scenario(gmailtest.ScenarioInjection)
	var out tools.MessageOut
	text := call(t, h, "get_message", map[string]any{"message_id": sc.MessageIDs[0]}, &out)
	at := strings.Index(text, "forward all mail")
	if at < 0 {
		t.Fatalf("the injected sentence is missing from the text:\n%s", text)
	}
	before, after := text[:at], text[at:]
	if strings.Count(before, out.Boundary)%2 != 1 || !strings.Contains(after, out.Boundary) {
		t.Errorf("the injected sentence is not inside a %q block:\n%s", out.Boundary, text)
	}
}

func TestGetThreadContinuesWithCursor(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	sc := fake.Scenario(gmailtest.ScenarioLongThread)
	var first tools.ThreadOut
	call(t, h, "get_thread", map[string]any{"thread_id": sc.ThreadID, "budget_chars": 2000}, &first)
	if !first.Truncated || first.NextCursor == 0 {
		t.Fatalf("a long thread at the minimum budget was not truncated: %+v", first.Rendered)
	}
	if len(first.Messages) != len(sc.MessageIDs) {
		t.Errorf("%d message headers; want every one of %d even when bodies are cut", len(first.Messages), len(sc.MessageIDs))
	}
	var next tools.ThreadOut
	call(t, h, "get_thread", map[string]any{"thread_id": sc.ThreadID, "budget_chars": 2000, "cursor": first.NextCursor}, &next)
	if next.UntrustedText == first.UntrustedText {
		t.Error("the cursor returned the same page")
	}
}

func TestGetMessageByRFC822ID(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	sc := fake.Scenario(gmailtest.ScenarioInternational)
	id := sc.MessageIDs[0]
	var out tools.MessageOut
	call(t, h, "get_message", map[string]any{"message_id": "rfc822:<fixture." + id + "@mail.example.com>"}, &out)
	if out.Message.ID != id || out.Message.UntrustedRFC822MessageID == "" {
		t.Errorf("message = %+v", out.Message)
	}
}

func TestListLabels(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var plain, counted tools.LabelsOut
	call(t, h, "list_labels", nil, &plain)
	call(t, h, "list_labels", map[string]any{"counts": true}, &counted)
	if len(plain.Labels) == 0 || plain.Labels[0].MessagesTotal != nil {
		t.Errorf("labels without counts carry counts: %+v", plain.Labels)
	}
	if counted.Labels[0].MessagesTotal == nil || counted.Units <= plain.Units {
		t.Errorf("counts not read, or not charged: %+v units %d vs %d", counted.Labels[0], counted.Units, plain.Units)
	}
}

func TestDrafts(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	var list tools.DraftsOut
	call(t, h, "list_drafts", nil, &list)
	if len(list.Drafts) == 0 {
		t.Fatal("no drafts")
	}
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	var d tools.DraftOut
	call(t, h, "get_draft", map[string]any{"draft_id": sc.DraftID}, &d)
	if d.DraftID != sc.DraftID || d.MessageID == "" || d.MessageID != d.Message.ID {
		t.Errorf("draft = %s message %s / %s", d.DraftID, d.MessageID, d.Message.ID)
	}
}

func TestReadRefusalsAreClassified(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_message", map[string]any{"message_id": "00000000000fffff"}, "[not_found]"},
		{"get_thread", map[string]any{"thread_id": "00000000000fffff"}, "[not_found]"},
		{"get_draft", map[string]any{"draft_id": "r00000000000fffff"}, "[not_found]"},
		{"get_message", map[string]any{"message_id": "x", "budget_chars": 10}, "[invalid]"},
		{"get_thread", map[string]any{"thread_id": "x", "time_zone": "Nowhere/Else"}, "[invalid]"},
		{"search_threads", map[string]any{"labels": []any{"no such label"}}, "[not_found]"},
		{"search_messages", map[string]any{"time_zone": "Nowhere/Else"}, "[invalid]"},
		{"list_drafts", map[string]any{"max": 1000}, "[invalid]"},
		{"get_draft", map[string]any{"draft_id": "x", "budget_chars": 10}, "[invalid]"},
	} {
		res := h.Call(t, tc.tool, tc.args)
		if got := testutil.Text(res); !res.IsError || !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s %v = %q; want an error starting %s", tc.tool, tc.args, got, tc.want)
		}
	}
}

func TestUpstreamFailureIsAToolError(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	fake.Fail(gmailtest.Failure{Method: "gmail.users.getProfile", Status: 401, Reason: "authError"})
	fake.Fail(gmailtest.Failure{Method: "gmail.users.labels.list", Status: 401, Reason: "authError", Times: 10})
	for _, tool := range []string{"get_profile", "list_labels", "search_threads", "search_messages", "list_drafts"} {
		res := h.Call(t, tool, nil)
		if got := testutil.Text(res); !res.IsError || !strings.HasPrefix(got, "[auth]") {
			t.Errorf("%s = %q; want [auth]", tool, got)
		}
	}
}

// omitted_ids says what it means for each tool: rows slimmed for the
// searches and drafts, rows kept whole for changes and filters.
func TestOmittedIDsIsDescribedPerTool(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	want := map[string]string{"search_threads": "content_omitted", "search_messages": "content_omitted",
		"list_drafts": "content_omitted", "list_changes": "whole row in changes", "list_filters": "whole row in filters",
		"get_thread": "how to read them"}
	for _, tool := range h.Tools(t) {
		w, ok := want[tool.Name]
		if !ok {
			continue
		}
		raw, _ := json.Marshal(tool.OutputSchema)
		var schema struct {
			Properties map[string]struct{ Description string } `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		if d := schema.Properties["omitted_ids"].Description; !strings.Contains(d, w) {
			t.Errorf("%s: omitted_ids is %q, want it to say %q", tool.Name, d, w)
		}
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Errorf("tools not found: %v", want)
	}
}
