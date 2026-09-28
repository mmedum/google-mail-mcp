//go:build live

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
)

// fakeMailbox records what the driver did to the mailbox.
type fakeMailbox struct {
	failInsertAt  int
	inserted      []string
	trashed       []string
	labels        []string
	deleted       []string
	drafts        []string
	deletedDrafts []string
}

func (f *fakeMailbox) CreateLabel(_ context.Context, name string) (string, error) {
	f.labels = append(f.labels, name)
	return "Label_1", nil
}

func (f *fakeMailbox) Insert(_ context.Context, _ string, raw []byte) (string, string, error) {
	if f.failInsertAt > 0 && len(f.inserted)+1 == f.failInsertAt {
		return "", "", errors.New("insert refused")
	}
	id := "000000000000000" + string(rune('1'+len(f.inserted)))
	f.inserted = append(f.inserted, string(raw))
	return id, id, nil
}

func (f *fakeMailbox) Trash(_ context.Context, id string) error {
	f.trashed = append(f.trashed, id)
	return nil
}

func (f *fakeMailbox) CreateDraft(_ context.Context, raw []byte) (string, error) {
	f.drafts = append(f.drafts, string(raw))
	return fmt.Sprintf("r%016x", len(f.drafts)), nil
}

func (f *fakeMailbox) DeleteDraft(_ context.Context, id string) error {
	f.deletedDrafts = append(f.deletedDrafts, id)
	return nil
}

func (f *fakeMailbox) Probe(context.Context, string, string, url.Values, any) probeResult {
	return probeResult{status: 403}
}

func (f *fakeMailbox) HistoryID(context.Context) (string, error) { return "1000", nil }

func (f *fakeMailbox) InternalDate(context.Context, string) (int64, error) { return 0, nil }

func (f *fakeMailbox) UpdateDraft(context.Context, string, []byte) (string, error) { return "", nil }

func (f *fakeMailbox) DraftMessageID(context.Context, string) (string, error) { return "", nil }

func (f *fakeMailbox) Send(context.Context, mime.Outgoing, string, string) (string, string, error) {
	return "", "", errors.New("the fake never sends")
}

func (f *fakeMailbox) Label(context.Context, string, string) error { return nil }

func (f *fakeMailbox) SentCopy(context.Context, string) (string, []byte, error) { return "", nil, nil }

func (f *fakeMailbox) Header(context.Context, string, string) (string, error) { return "", nil }

func (f *fakeMailbox) Account(context.Context) (string, error) { return "reader@example.com", nil }

func (f *fakeMailbox) FullScope() bool { return false }

func (f *fakeMailbox) SettingsScope() bool { return false }

func (f *fakeMailbox) SaveSettings(context.Context) (savedSettings, error) {
	return savedSettings{}, nil
}

func (f *fakeMailbox) RestoreSettings(context.Context, savedSettings) error { return nil }

func (f *fakeMailbox) FiltersFrom(context.Context, string) ([]string, error) { return nil, nil }

func (f *fakeMailbox) DeleteFiltersFrom(context.Context, string) (int, error) { return 0, nil }

func (f *fakeMailbox) DeleteLabel(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestRunLabelIsUniqueAndScopesQueries(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	a, b := newRunLabel(now), newRunLabel(now)
	if a.name == b.name {
		t.Error("two runs in the same second got the same label")
	}
	if !regexp.MustCompile(`^livemail-20260925-100000-[0-9a-f]{6}$`).MatchString(a.name) {
		t.Errorf("label %q", a.name)
	}
	if got := a.query("is:unread"); got != "label:"+a.name+" is:unread" {
		t.Errorf("query = %q", got)
	}
}

func TestSeedingAndCleanUp(t *testing.T) {
	box := &fakeMailbox{}
	r := newRunLabel(time.Now())
	s, err := seedMailbox(context.Background(), box, r, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.messages) != 3 || len(box.labels) != 1 || box.labels[0] != r.name || s.historyStart != "1000" {
		t.Fatalf("seeded %+v, labels %v", s, box.labels)
	}
	if len(s.drafts) != runDrafts || len(box.drafts) != runDrafts {
		t.Fatalf("drafts %v in the seed, %d made", s.drafts, len(box.drafts))
	}
	for _, raw := range append(append([]string(nil), box.inserted...), box.drafts...) {
		if !strings.Contains(raw, r.name) {
			t.Errorf("a synthetic message or draft does not carry the run's name, so no scoped read can find it")
		}
		for _, addr := range regexp.MustCompile(`[\w.\-]+@[\w.\-]+`).FindAllString(raw, -1) {
			if !strings.HasSuffix(addr, "@example.com") && !strings.HasSuffix(addr, "@example.org") &&
				!strings.HasSuffix(addr, ".invalid") {
				t.Errorf("a synthetic message carries %s, outside the reserved domains", addr)
			}
		}
	}
	if !strings.Contains(box.inserted[0], "filename=\""+syntheticAttachmentName+"\"") {
		t.Error("the first synthetic message carries no attachment for download_attachment")
	}
	s.extraLabels = []string{"Label_9"}
	if err := cleanUp(context.Background(), box, s); err != nil {
		t.Fatal(err)
	}
	if len(box.trashed) != 3 || len(box.deleted) != 2 || len(box.deletedDrafts) != runDrafts {
		t.Errorf("trashed %v, deleted %v, drafts deleted %v", box.trashed, box.deleted, box.deletedDrafts)
	}
}

// A failed insert still removes the label and what was inserted.
func TestAFailedSeedCleansUp(t *testing.T) {
	box := &fakeMailbox{failInsertAt: 2}
	if _, err := seedMailbox(context.Background(), box, newRunLabel(time.Now()), 3); err == nil {
		t.Fatal("a failed insert was not reported")
	}
	if len(box.trashed) != 1 || len(box.deleted) != 1 {
		t.Errorf("trashed %v, deleted %v; want the one insert and the label cleaned up", box.trashed, box.deleted)
	}
}

func TestTheGuardRefusesReadsOutsideTheRun(t *testing.T) {
	r := newRunLabel(time.Now())
	e := &env{seed: &seeded{label: r, messages: []string{"0000000000000001"}, threads: []string{"0000000000000002"}}}
	for _, tc := range []struct {
		tool string
		args map[string]any
		ok   bool
	}{
		{"search_messages", map[string]any{"q": r.query("")}, true},
		{"search_messages", map[string]any{"q": "is:unread"}, false},
		{"search_threads", map[string]any{}, false},
		{"get_message", map[string]any{"message_id": "0000000000000001"}, true},
		{"get_thread", map[string]any{"thread_id": "0000000000000002"}, true},
		{"get_message", map[string]any{"message_id": "0000000000000009"}, false},
		{"get_message", map[string]any{"message_id": "rfc822:<" + r.name + ".1@livemail.invalid>"}, true},
		{"get_message", map[string]any{"message_id": "rfc822:<someone@example.com>"}, false},
		{"list_drafts", map[string]any{"q": "subject:" + r.name}, true},
		{"list_drafts", map[string]any{}, false},
		{"get_draft", map[string]any{"draft_id": "r0000000000000009"}, false},
		{"get_profile", nil, true},
		{"list_changes", map[string]any{"history_id": "1", "label": r.name}, true},
		{"list_changes", map[string]any{"history_id": "1"}, false},
		{"list_changes", map[string]any{"history_id": "1", "label": "INBOX"}, false},
		{"download_attachment", map[string]any{"message_id": "0000000000000001", "part_id": "1"}, true},
		{"download_attachment", map[string]any{"message_id": "0000000000000009", "part_id": "1"}, false},
	} {
		err := e.guard(tc.tool, tc.args)
		if (err == nil) != tc.ok {
			t.Errorf("guard(%s, %v) = %v, want ok=%v", tc.tool, tc.args, err, tc.ok)
		}
		if err != nil && !errors.Is(err, errUnscoped) {
			t.Errorf("guard refused with %v, not errUnscoped", err)
		}
	}
}

// A seed that fails stops a run before the server starts: the binary
// does not exist, so reaching it would fail differently.
func TestAFailedSeedStopsBeforeTheServer(t *testing.T) {
	o, tr, err := parseOptions([]string{"-profile", "p", "-binary", "/nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	err = drive(context.Background(), o, tr, &fakeMailbox{failInsertAt: 1})
	if err == nil || !strings.Contains(err.Error(), "insert refused") {
		t.Errorf("drive = %v, want the insert's refusal", err)
	}
}

func TestProfileIsRequired(t *testing.T) {
	if _, _, err := parseOptions(nil); err == nil {
		t.Error("a run without -profile was accepted")
	}
}

func TestEveryReadToolHasAStep(t *testing.T) {
	want := []string{"get_profile", "search_threads", "search_messages", "get_thread", "get_message",
		"list_labels", "list_drafts", "get_draft", "list_changes", "get_settings", "list_filters", "download_attachment"}
	have := map[string]bool{}
	for _, s := range steps {
		have[s.tool] = true
	}
	for _, tool := range want {
		if !have[tool] {
			t.Errorf("no step for %s", tool)
		}
	}
}

func TestEveryWriteToolHasAStep(t *testing.T) {
	have := map[string]bool{}
	for _, s := range writeSteps {
		have[s.tool] = true
	}
	for _, tool := range []string{"create_draft", "update_draft", "delete_draft", "modify_labels", "trash", "restore",
		"create_label", "update_label"} {
		if !have[tool] {
			t.Errorf("no step for %s", tool)
		}
	}
}

func TestTheGuardHoldsWritesToTheRun(t *testing.T) {
	r := newRunLabel(time.Now())
	e := &env{account: "reader@example.com", seed: &seeded{label: r, messages: []string{"0000000000000001"},
		threads: []string{"0000000000000002"}, drafts: []string{"r1"}, draftMessages: []string{"0000000000000003"}}}
	for _, tc := range []struct {
		tool string
		args map[string]any
		ok   bool
	}{
		{"create_draft", map[string]any{"to": []any{rcptTo}, "cc": []any{rcptCc}, "bcc": []any{rcptBcc}}, true},
		{"create_draft", map[string]any{"to": []any{"someone@example.net"}}, false},
		{"create_draft", map[string]any{"bcc": []any{"Name <person@example.net>"}}, false},
		{"create_draft", map[string]any{"to": []any{"a@example.net, b@example.com"}}, false},
		{"create_draft", map[string]any{"to": []any{"<a@example.com> x@example.net"}}, false},
		{"create_draft", map[string]any{"new_field": "x"}, false},
		{"create_draft", map[string]any{"from": "reader@example.com"}, true},
		{"create_draft", map[string]any{"from": "other@example.com"}, false},
		{"create_draft", map[string]any{"reply_to": "0000000000000001"}, true},
		{"create_draft", map[string]any{"reply_to": "0000000000000009"}, false},
		{"create_draft", map[string]any{"reply_to_thread": "0000000000000009"}, false},
		{"create_draft", map[string]any{"attachments": []any{attachName(r)}}, true},
		{"create_draft", map[string]any{"attachments": []any{"notes.txt"}}, false},
		{"update_draft", map[string]any{"draft_id": "r1", "message_id": "0000000000000003", "add_attachments": []any{"x"}}, false},
		{"update_draft", map[string]any{"draft_id": "r1", "message_id": "0000000000000003"}, true},
		{"modify_labels", map[string]any{"message_ids": []any{"0000000000000001"}, "thread_ids": []any{"0000000000000002"}}, true},
		{"modify_labels", map[string]any{"message_ids": []any{"0000000000000001", "0000000000000009"}}, false},
		{"trash", map[string]any{"thread_ids": []any{"0000000000000009"}}, false},
		{"create_label", map[string]any{"name": r.name + "-made"}, true},
		{"create_label", map[string]any{"name": strings.ToUpper(r.name) + "-MADE"}, true},
		{"create_label", map[string]any{"name": "Receipts"}, false},
		{"update_label", map[string]any{"label": "Receipts", "name": r.name + "-x"}, false},
		{"delete_draft", map[string]any{"draft_id": "r9"}, false},
	} {
		err := e.guard(tc.tool, tc.args)
		if (err == nil) != tc.ok {
			t.Errorf("guard(%s, %v) = %v, want ok=%v", tc.tool, tc.args, err, tc.ok)
		}
		if err != nil && !errors.Is(err, errUnscoped) {
			t.Errorf("guard refused with %v, not errUnscoped", err)
		}
	}
}

// The sending spikes run only with a second address, and never to the
// account itself.
func TestSendingSpikesNeedASecondAddress(t *testing.T) {
	s := &seeded{label: newRunLabel(time.Now())}
	for _, x := range []spikeRun{
		{box: &fakeMailbox{}, s: s, account: "reader@example.com", spikesDE: true},
		{box: &fakeMailbox{}, s: s, account: "reader@example.com", sendTo: "Reader@example.com", spikesDE: true},
		{box: &fakeMailbox{}, s: s, sendTo: "second@example.org", spikesDE: true},
		{box: &fakeMailbox{}, s: s, account: "reader@example.com", sendTo: "second@example.org"},
	} {
		for _, ask := range []func(context.Context, spikeRun) string{spikeD, spikeE} {
			if got := ask(context.Background(), x); !strings.Contains(got, "not run") {
				t.Errorf("sendTo %q account %q: %s", x.sendTo, x.account, got)
			}
		}
	}
	if len(s.messages) != 0 {
		t.Error("a spike recorded a sent message without -send-to")
	}
	if _, _, err := parseOptions([]string{"-profile", "p", "-send-to", "Name <a@example.org>"}); err == nil {
		t.Error("a -send-to with a display name was accepted")
	}
	if o, _, err := parseOptions([]string{"-profile", "p", "-send-to", "second@example.org"}); err != nil || o.sendTo != "second@example.org" {
		t.Errorf("-send-to: %+v, %v", o, err)
	}
}

func TestEveryGatedToolHasAStep(t *testing.T) {
	have := map[string]bool{}
	for _, s := range gatedSteps {
		have[s.tool] = true
	}
	for _, tool := range []string{"send_draft", "delete_permanently", "delete_label"} {
		if !have[tool] {
			t.Errorf("no step for %s", tool)
		}
	}
}

// A send reaches -send-to and nothing else, and a step that sends or
// deletes for good is skipped when the run cannot do it.
func TestTheGuardHoldsSendsToSendTo(t *testing.T) {
	r := newRunLabel(time.Now())
	seed := &seeded{label: r, messages: []string{"0000000000000001"}, drafts: []string{"r1"},
		draftMessages: []string{"0000000000000003"}}
	without := &env{seed: seed}
	with := &env{seed: seed, sendTo: "second@example.net", full: true}
	send := map[string]any{"draft_id": "r1", "message_id": "0000000000000003", "confirm_recipients": []any{"second@example.net"}}
	for _, tc := range []struct {
		e    *env
		tool string
		args map[string]any
		ok   bool
	}{
		{with, "create_draft", map[string]any{"to": []any{"second@example.net"}}, true},
		{without, "create_draft", map[string]any{"to": []any{"second@example.net"}}, false},
		{with, "create_draft", map[string]any{"to": []any{"Second <second@example.net>"}}, false},
		{with, "create_draft", map[string]any{"cc": []any{"third@example.net"}}, false},
		{with, "send_draft", send, true},
		{without, "send_draft", send, false},
		{with, "send_draft", map[string]any{"draft_id": "r1", "message_id": "0000000000000003",
			"confirm_recipients": []any{"third@example.net"}}, false},
		{with, "send_draft", map[string]any{"draft_id": "r9", "message_id": "0000000000000003"}, false},
		{with, "delete_permanently", map[string]any{"message_ids": []any{"0000000000000009"}, "confirm": true}, false},
		{with, "delete_label", map[string]any{"label": r.name + "-renamed", "confirm": true}, true},
		{with, "delete_label", map[string]any{"label": "Receipts", "confirm": true}, false},
	} {
		err := tc.e.guard(tc.tool, tc.args)
		if (err == nil) != tc.ok {
			t.Errorf("guard(%s, %v) with send-to %q = %v, want ok=%v", tc.tool, tc.args, tc.e.sendTo, err, tc.ok)
		}
	}
	for _, s := range gatedSteps {
		if s.needs == nil {
			continue
		}
		if why := s.needs(without); why == "" {
			t.Errorf("%s runs without -send-to or the full scope", s.name)
		}
		if why := s.needs(with); why != "" {
			t.Errorf("%s is skipped with -send-to and the full scope: %s", s.name, why)
		}
	}
	for _, s := range gatedSteps {
		sends := s.tool == "send_draft" && s.refuses == "" && !strings.Contains(s.name, "dry run")
		if sends && s.needs == nil {
			t.Errorf("%s sends and has no needs", s.name)
		}
	}
}

// Spike G is not asked under a scope that deletes, and B, C, H and L do
// not run without what they need.
func TestPhaseThreeSpikesNeedTheirInputs(t *testing.T) {
	s := &seeded{label: newRunLabel(time.Now()), messages: []string{"0000000000000001"}}
	x := spikeRun{box: &fakeMailbox{}, s: s, full: true}
	for _, sp := range spikes {
		if strings.HasPrefix(sp.name, "G") {
			if got := sp.ask(context.Background(), x); !strings.HasPrefix(got, "not run") {
				t.Errorf("spike G under the full scope: %s", got)
			}
		}
	}
	for _, sp := range slices.Concat(sendSpikes, settingsSpikes) {
		if got := sp.ask(context.Background(), x); !strings.HasPrefix(got, "not run") {
			t.Errorf("spike %s: %s", sp.name, got)
		}
	}
	if len(s.messages) != 1 {
		t.Error("a spike changed the run's messages")
	}
}

// A message deleted for good is not trashed at cleanup, and stays in
// place for the spikes that read messages beside threads.
func TestCleanUpSkipsDeletedMessages(t *testing.T) {
	box := &fakeMailbox{}
	s := &seeded{messages: []string{"0000000000000001", "0000000000000002"}}
	s.deleted("0000000000000002")
	if err := cleanUp(context.Background(), box, s); err != nil {
		t.Fatal(err)
	}
	if len(box.trashed) != 1 || box.trashed[0] != "0000000000000001" || len(s.messages) != 2 {
		t.Errorf("trashed %v, messages %v", box.trashed, s.messages)
	}
}

// -clean names a run and nothing else, so its one search can only find
// the run's own mail.
func TestCleanTakesOnlyARunName(t *testing.T) {
	for _, name := range []string{"livemail-20260926-234553-62dca1"} {
		if _, _, err := parseOptions([]string{"-profile", "p", "-clean", name}); err != nil {
			t.Errorf("-clean %s: %v", name, err)
		}
	}
	for _, name := range []string{"invoice", "livemail-", "livemail-20260926-234553-62dca1 OR is:inbox", "*"} {
		if _, _, err := parseOptions([]string{"-profile", "p", "-clean", name}); err == nil {
			t.Errorf("-clean %q was accepted", name)
		}
	}
}

// The settings steps change only what the run owns: a signature it wrote
// on the account's own address, filters matching its own sender and
// deleted only when it made them, and a vacation reply months ahead.
// Every one of them runs only with the settings scope, and every step
// passes the guard with the arguments it builds.
func TestTheGuardHoldsTheSettingsSteps(t *testing.T) {
	r := newRunLabel(time.Now())
	e := &env{seed: &seeded{label: r}, account: "reader@example.com", signatureAddress: "alias@example.com",
		settings: true, filters: []string{"ANe1Bmg1"}}
	soon := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	ahead := time.Now().Add(250 * 24 * time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		tool string
		args map[string]any
		ok   bool
	}{
		{"update_signature", map[string]any{"signature": "Synthetic signature for " + r.name}, true},
		{"update_signature", map[string]any{"signature": "Somebody else's words"}, false},
		{"update_signature", map[string]any{"send_as": "boss@example.com", "signature": ""}, false},
		// The account's own address is not the one saved when it is not the default.
		{"update_signature", map[string]any{"send_as": "reader@example.com", "signature": ""}, false},
		{"create_filter", map[string]any{"from": r.name + "@filters.invalid", "trash": true, "confirm": true}, true},
		{"create_filter", map[string]any{"from": "bank@example.com", "trash": true, "confirm": true}, false},
		{"create_filter", map[string]any{"from": r.name + "@filters.invalid", "add_labels": []any{"Receipts"}}, false},
		{"create_filter", map[string]any{"from": r.name + "@filters.invalid", "forward": "x@example.com"}, false},
		{"delete_filter", map[string]any{"filter_id": "ANe1Bmg1", "confirm": true}, true},
		{"delete_filter", map[string]any{"filter_id": "someone-elses", "confirm": true}, false},
		{"set_vacation", map[string]any{"enable": true, "body": "Synthetic reply for " + r.name, "start": ahead}, true},
		{"set_vacation", map[string]any{"enable": true, "body": "Synthetic reply for " + r.name, "start": soon}, false},
		{"set_vacation", map[string]any{"enable": true, "body": "Synthetic reply for " + r.name}, false},
		{"set_vacation", map[string]any{"enable": false}, true},
	} {
		if err := e.guard(tc.tool, tc.args); (err == nil) != tc.ok {
			t.Errorf("guard(%s, %v) = %v, want ok=%v", tc.tool, tc.args, err, tc.ok)
		}
	}
	e.seed.messages = []string{"0000000000000001"}
	for _, s := range settingsSteps {
		if s.needs == nil || s.needs(&env{}) == "" {
			t.Errorf("%s runs without the settings scope", s.name)
		}
		if s.tool == "delete_filter" && s.needs(&env{settings: true}) == "" {
			t.Errorf("%s runs with no filter to delete", s.name)
		}
		args := s.args(e)
		if err := e.guard(s.tool, args); err != nil {
			t.Errorf("%s: its own arguments fail the guard: %v", s.name, err)
		}
	}
}

// Cleanup deletes the run's filters back to back, which Gmail can refuse
// as overlapping (§18 row 54); each refused delete is waited out and
// made again, so no synthetic filter is left on the account.
func TestCleanupRepeatsOverlappingFilterDeletes(t *testing.T) {
	saved := rateWait
	rateWait = time.Millisecond
	t.Cleanup(func() { rateWait = saved })
	var mu sync.Mutex
	refusals := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"filter":[{"id":"f1","criteria":{"from":"run@filters.invalid"}},` +
				`{"id":"f2","criteria":{"from":"run@filters.invalid"}},{"id":"f3","criteria":{"from":"other@example.org"}}]}`))
		case r.Method == http.MethodDelete && refusals[r.URL.Path] < 2:
			refusals[r.URL.Path]++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Precondition check failed.","errors":[{"reason":"failedPrecondition"}]}}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	m := &restMailbox{http: srv.Client(), base: srv.URL + "/"}
	n, err := m.DeleteFiltersFrom(context.Background(), "run@filters.invalid")
	if err != nil || n != 2 || len(refusals) != 2 {
		t.Fatalf("deleted %d (%v) after refusals %v; want 2 deleted, each refused twice first", n, err, refusals)
	}
}
