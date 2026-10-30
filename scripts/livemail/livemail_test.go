//go:build live

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmedum/google-mail-mcp/v3/internal/mime"
	"github.com/mmedum/google-mail-mcp/v3/scripts/internal/redact"
	"github.com/mmedum/google-mail-mcp/v3/scripts/internal/transcript"
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

func (f *fakeMailbox) ReadSettings(context.Context) (savedSettings, error) {
	return savedSettings{}, nil
}

func (f *fakeMailbox) WriteSignature(_ context.Context, _, signature string) (string, error) {
	return signature, nil
}

func (f *fakeMailbox) WriteVacation(context.Context, json.RawMessage) error { return nil }

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
	if attached := mime.ParseRaw([]byte(box.inserted[2])).Attachments; len(attached) != 1 ||
		attached[0].PartID != "1" || attached[0].MimeType != "message/rfc822" ||
		!strings.Contains(mime.ParseRaw([]byte(box.inserted[2])).Body.Text, "Synthetic body 3") {
		t.Errorf("the third synthetic message does not carry one attached message, part 1, beside its body: %+v", attached)
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
		err := e.guard(step{tool: tc.tool}, tc.args)
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
		"list_labels", "list_drafts", "get_draft", "list_changes", "get_settings", "list_filters", "download_attachment",
		"download_attachments", "read_attachment"}
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
		{"create_draft", map[string]any{"forward": "0000000000000001"}, true},
		{"create_draft", map[string]any{"forward": "0000000000000009"}, false},
		{"create_draft", map[string]any{"forward": "rfc822:<someone@example.net>"}, false},
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
		err := e.guard(step{tool: tc.tool}, tc.args)
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
	send := map[string]any{"draft_id": "r1", "message_id": "0000000000000003", "subject": r.name + " draft 1",
		"first_line": "Synthetic.", "confirm_recipients": []any{"second@example.net"}}
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
		err := tc.e.guard(step{tool: tc.tool}, tc.args)
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

// The invitation step prints the units its read spent: the result's text
// leaves them out, and they say where Gmail kept the calendar part.
func TestTheInvitationStepPrintsItsUnits(t *testing.T) {
	var out strings.Builder
	e := &env{tr: transcript.NewTo(redact.NewRedactor(false), &out, &out), structured: map[string]any{
		"units": float64(41),
		"message": map[string]any{"attachments": []any{
			map[string]any{"part_id": "2", "invitation": map[string]any{"method": "PUBLISH"}},
		}},
	}}
	if err := e.invitationIs("2", map[string]any{"method": "PUBLISH"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "report: the read spent 41 units;") {
		t.Errorf("the transcript does not give the units: %q", out.String())
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
	e := &env{seed: &seeded{label: r, drafts: []string{"r0000000000000031"}, draftMessages: []string{"0000000000000032"}},
		account: "reader@example.com", signatureAddress: "alias@example.com", settings: true, filters: []string{"ANe1Bmg1"},
		signedDraft: "r0000000000000031", signedMessage: "0000000000000032", signatureSet: true}
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
		{"create_trash_filter", map[string]any{"from": r.name + "@filters.invalid", "confirm": true}, true},
		{"create_trash_filter", map[string]any{"from": "bank@example.com", "confirm": true}, false},
		{"create_filter", map[string]any{"from": r.name + "@filters.invalid", "trash": true}, false},
		{"create_filter", map[string]any{"from": r.name + "@filters.invalid", "add_labels": []any{"Receipts"}}, false},
		{"create_filter", map[string]any{"from": r.name + "@filters.invalid", "forward": "x@example.com"}, false},
		{"delete_filter", map[string]any{"filter_id": "ANe1Bmg1", "confirm": true}, true},
		{"delete_filter", map[string]any{"filter_id": "someone-elses", "confirm": true}, false},
		{"set_vacation", map[string]any{"enable": true, "body": "Synthetic reply for " + r.name, "start": ahead}, true},
		{"set_vacation", map[string]any{"enable": true, "body": "Synthetic reply for " + r.name, "start": soon}, false},
		{"set_vacation", map[string]any{"enable": true, "body": "Synthetic reply for " + r.name}, false},
		{"set_vacation", map[string]any{"enable": false}, true},
	} {
		if err := e.guard(step{tool: tc.tool}, tc.args); (err == nil) != tc.ok {
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
		// A draft step after a failed set would carry the account's own
		// signature into the transcript (CLAUDE.md rule 1).
		if strings.Contains(s.tool, "draft") && s.needs(&env{settings: true}) == "" {
			t.Errorf("%s runs without the run's own signature in place", s.name)
		}
		args := s.args(e)
		if err := e.guard(s, args); err != nil {
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

// settingsBox is a mailbox whose settings a test sets: each ReadSettings
// answers the next of reads, and a signature write answers sigErr, or
// sigStored when it is set. It records every write, and how many copies
// of the settings were in dir at each signature write.
type settingsBox struct {
	fakeMailbox
	dir           string
	reads         []savedSettings
	sigErr        error
	sigStored     string
	signatures    []string
	vacations     []json.RawMessage
	copiesAtWrite []int
}

func (b *settingsBox) ReadSettings(context.Context) (savedSettings, error) {
	s := b.reads[0]
	if len(b.reads) > 1 {
		b.reads = b.reads[1:]
	}
	return s, nil
}

func (b *settingsBox) WriteSignature(_ context.Context, _, signature string) (string, error) {
	copies, _ := filepath.Glob(filepath.Join(b.dir, "*-settings.json"))
	b.copiesAtWrite = append(b.copiesAtWrite, len(copies))
	b.signatures = append(b.signatures, signature)
	switch {
	case b.sigErr != nil:
		return "", b.sigErr
	case b.sigStored != "":
		return b.sigStored, nil
	}
	return signature, nil
}

func (b *settingsBox) WriteVacation(_ context.Context, v json.RawMessage) error {
	b.vacations = append(b.vacations, v)
	return nil
}

// syntheticSettings are an account's settings as a run might find them.
func syntheticSettings() savedSettings {
	return savedSettings{address: "reader@example.com",
		signature: `<div dir="ltr">Synthetic <b>signature</b> &amp; a second line</div>`,
		vacation:  json.RawMessage(`{"enableAutoReply":false,"responseSubject":"Synthetic away","restrictToContacts":true}`)}
}

// Before anything changes, the settings are in a file named for the run
// that only the account can read, and the transcript names the file but
// never what it holds. A copy that cannot be written stops the run
// before anything changes.
func TestTheSettingsAreKeptBeforeAnythingChanges(t *testing.T) {
	dir, r, saved := t.TempDir(), newRunLabel(time.Now()), syntheticSettings()
	box := &settingsBox{dir: dir, reads: []savedSettings{saved}}
	var out strings.Builder
	k, err := keepSettings(context.Background(), box, transcript.NewTo(redact.NewRedactor(false), &out, &out), dir, r, true)
	if err != nil {
		t.Fatal(err)
	}
	if k.path != filepath.Join(dir, r.name+"-settings.json") {
		t.Errorf("the copy is %s, not named for the run in %s", k.path, dir)
	}
	if info, err := os.Stat(k.path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the copy: %v, %v; want mode 0600", info, err)
	}
	data, err := os.ReadFile(k.path)
	if err != nil {
		t.Fatal(err)
	}
	var kept settingsCopy
	if err := json.Unmarshal(data, &kept); err != nil {
		t.Fatal(err)
	}
	if kept.Address != saved.address || kept.Signature != saved.signature || !sameJSON(kept.Vacation, saved.vacation) {
		t.Errorf("the copy holds %+v, not the settings read", kept)
	}
	if !slices.Equal(box.copiesAtWrite, []int{1}) {
		t.Errorf("copies in the directory at each signature write: %v; want the one copy before the first", box.copiesAtWrite)
	}
	if !strings.Contains(out.String(), filepath.Base(k.path)) {
		t.Errorf("the transcript does not name the copy: %q", out.String())
	}
	for _, s := range []string{"Synthetic <b>signature", "Synthetic away"} {
		if strings.Contains(out.String(), s) {
			t.Errorf("the transcript prints the settings themselves: %q", out.String())
		}
	}

	// The same run's copy is already there: nothing is written.
	again := &settingsBox{dir: dir, reads: []savedSettings{saved}}
	if _, err := keepSettings(context.Background(), again, transcript.NewTo(redact.NewRedactor(false), &out, &out),
		dir, r, true); err == nil || len(again.signatures) != 0 {
		t.Errorf("keepSettings over an existing copy = %v, and wrote signatures %v; want an error and no write",
			err, again.signatures)
	}
}

// A signature Gmail refuses, or stores changed, when it is written back
// as it was is one the restore could not put back, so no signature step
// runs and the restore leaves the signature alone. One Gmail takes back
// unchanged lets them run, and is written back at the end.
func TestASignatureGmailWillNotTakeBackIsNeverChanged(t *testing.T) {
	saved := syntheticSettings()
	for _, tc := range []struct {
		name      string
		box       *settingsBox
		skip      string
		restores  int
		stepsSkip bool
	}{
		{"refused", &settingsBox{sigErr: errors.New(`PATCH settings/sendAs/x: HTTP 400, reason "invalidArgument"`)},
			`reason "invalidArgument"`, 1, true},
		{"stored changed", &settingsBox{sigStored: "<div>Synthetic</div>"}, "stored the account's own signature changed", 1, true},
		{"taken back", &settingsBox{}, "", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.box.dir, tc.box.reads = t.TempDir(), []savedSettings{saved}
			var out strings.Builder
			k, err := keepSettings(context.Background(), tc.box, transcript.NewTo(redact.NewRedactor(false), &out, &out),
				tc.box.dir, newRunLabel(time.Now()), true)
			if err != nil {
				t.Fatal(err)
			}
			if tc.skip == "" && k.signatureSkip != "" || !strings.Contains(k.signatureSkip, tc.skip) {
				t.Errorf("signatureSkip = %q, want %q in it", k.signatureSkip, tc.skip)
			}
			e := &env{settings: true, signatureSkip: k.signatureSkip}
			for _, s := range settingsSteps {
				if s.tool != "update_signature" {
					continue
				}
				if skipped := s.needs(e) != ""; skipped != tc.stepsSkip {
					t.Errorf("%s: skipped %v, want %v", s.name, skipped, tc.stepsSkip)
				}
			}
			if err := k.restore(context.Background(), tc.box); err != nil {
				t.Fatal(err)
			}
			if len(tc.box.signatures) != tc.restores {
				t.Errorf("the signature was written %d times, want %d: the write back, and the restore only when the "+
					"steps ran", len(tc.box.signatures), tc.restores)
			}
		})
	}

	// A run that drives no signature step does not write the signature.
	box := &settingsBox{dir: t.TempDir(), reads: []savedSettings{saved}}
	var out strings.Builder
	k, err := keepSettings(context.Background(), box, transcript.NewTo(redact.NewRedactor(false), &out, &out), box.dir,
		newRunLabel(time.Now()), drivesSignature(regexp.MustCompile("filter")))
	if err != nil || k.signature || len(box.signatures) != 0 {
		t.Errorf("a run without signature steps: %v, restore writes it %v, written %v", err, k.signature, box.signatures)
	}
	if !drivesSignature(regexp.MustCompile("")) || !drivesSignature(regexp.MustCompile("clear the signature")) {
		t.Error("a run that drives a signature step is not seen to")
	}
}

// The restore reads the settings back, and deletes the copy only when
// they are as the run found them. Otherwise it fails and names the copy,
// and the copy stays.
func TestTheRestoreIsReadBack(t *testing.T) {
	saved := syntheticSettings()
	otherSignature, otherVacation, reordered := saved, saved, saved
	otherSignature.signature = "<div>Synthetic signature for a run</div>"
	otherVacation.vacation = json.RawMessage(`{"enableAutoReply":false,"responseSubject":"Synthetic run","restrictToContacts":true}`)
	reordered.vacation = json.RawMessage(`{ "restrictToContacts": true, "responseSubject": "Synthetic away", "enableAutoReply": false }`)
	for _, tc := range []struct {
		name string
		now  savedSettings
		want string
	}{
		{"as found", saved, ""},
		{"as found, keys in another order", reordered, ""},
		{"another signature", otherSignature, "the signature reads back other than the run found it"},
		{"another vacation reply", otherVacation, "the vacation reply reads back other than the run found it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, r := t.TempDir(), newRunLabel(time.Now())
			path, err := writeSettingsCopy(dir, r, saved)
			if err != nil {
				t.Fatal(err)
			}
			box := &settingsBox{dir: dir, reads: []savedSettings{tc.now}}
			err = keptSettings{saved: saved, path: path, signature: true}.restore(context.Background(), box)
			_, statErr := os.Stat(path)
			if tc.want == "" {
				if err != nil || !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("restore = %v, copy %v; want nil and the copy deleted", err, statErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Errorf("restore = %v; want %q and the copy's path", err, tc.want)
			}
			if statErr != nil {
				t.Errorf("the copy is gone after a restore that did not read back: %v", statErr)
			}
			if strings.Contains(err.Error(), "Synthetic") {
				t.Errorf("the failure prints the settings: %v", err)
			}
			if len(box.vacations) != 1 || !sameJSON(box.vacations[0], saved.vacation) || !slices.Equal(box.signatures, []string{saved.signature}) {
				t.Errorf("the restore wrote signatures %v and vacations %s, not the saved ones", box.signatures, box.vacations)
			}
		})
	}
}

// A restore Gmail refuses is reported with its status and Google's
// reason, and never the signature, which the refusal's message may repeat.
func TestARefusedRestoreSaysWhyNotWhat(t *testing.T) {
	saved := syntheticSettings()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 400,
				"message": "Bad signature: " + saved.signature, "status": "INVALID_ARGUMENT",
				"errors": []any{map[string]any{"message": "Bad signature: " + saved.signature, "reason": "invalidArgument"}}}})
		case r.Method == http.MethodPut:
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/settings/sendAs":
			_ = json.NewEncoder(w).Encode(map[string]any{"sendAs": []any{map[string]any{"sendAsEmail": saved.address,
				"signature": "", "isDefault": true}}})
		default:
			_, _ = w.Write(saved.vacation)
		}
	}))
	t.Cleanup(srv.Close)
	m := &restMailbox{http: srv.Client(), base: srv.URL + "/"}
	path, err := writeSettingsCopy(t.TempDir(), newRunLabel(time.Now()), saved)
	if err != nil {
		t.Fatal(err)
	}
	err = keptSettings{saved: saved, path: path, signature: true}.restore(context.Background(), m)
	if err == nil {
		t.Fatal("a refused restore was not reported")
	}
	for _, s := range []string{"put the signature back", "HTTP 400", `reason "invalidArgument"`, path} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the failure does not say %q: %v", s, err)
		}
	}
	if strings.Contains(err.Error(), "Synthetic") {
		t.Errorf("the failure prints the signature: %v", err)
	}
}

// The first-line step confirms every recipient the draft has, so the
// server's recipient guard, which comes first, lets it reach the first
// line; and the driver's guard lets it, since the step expects a refusal.
func TestTheFirstLineStepGetsPastTheRecipientGuard(t *testing.T) {
	r := newRunLabel(time.Now())
	e := &env{seed: &seeded{label: r, drafts: []string{"r1"}, draftMessages: []string{"0000000000000003"}},
		draftID: "r1", draftMessage: "0000000000000003"}
	addresses := func(args map[string]any, keys ...string) []string {
		var out []string
		for _, key := range keys {
			list, _ := args[key].([]any)
			for _, v := range list {
				as, _ := mime.ParseAddressList(v.(string))
				for _, a := range as {
					out = append(out, strings.ToLower(a.Email))
				}
			}
		}
		slices.Sort(out)
		return out
	}
	var draft, confirmed []string
	var send step
	for _, s := range writeSteps {
		if s.name == "update the draft" {
			draft = addresses(s.args(e), "to", "cc", "bcc")
		}
	}
	for _, s := range gatedSteps {
		if s.name == "a send with another first line is refused" {
			send, confirmed = s, addresses(s.args(e), "confirm_recipients")
		}
	}
	if len(draft) == 0 || !slices.Equal(draft, confirmed) {
		t.Errorf("the draft goes to %v, and the step confirms %v", draft, confirmed)
	}
	if err := e.guard(send, send.args(e)); err != nil {
		t.Errorf("the step's own arguments fail the guard: %v", err)
	}
	reserved := map[string]any{"draft_id": "r1", "message_id": "0000000000000003", "confirm_recipients": []any{rcptCc}}
	if err := e.guard(step{tool: "send_draft"}, reserved); !errors.Is(err, errUnscoped) {
		t.Errorf("a send the step expects to go confirms a reserved address: %v", err)
	}
	other := map[string]any{"draft_id": "r1", "message_id": "0000000000000003", "confirm_recipients": []any{"third@example.net"}}
	if err := e.guard(step{tool: "send_draft", refuses: "stale"}, other); !errors.Is(err, errUnscoped) {
		t.Errorf("a send the step expects refused confirms an address outside the reserved domains: %v", err)
	}
}

// A step that runs without -send-to names nothing the -send-to steps
// make: those were skipped, so it would name an id the run never had.
// The steps that can are the ones after them.
func TestStepsWithoutSendToNameNothingItsStepsMake(t *testing.T) {
	r := newRunLabel(time.Now())
	e := &env{full: true, settings: true, filters: []string{"ANe1Bmg1"},
		seed: &seeded{label: r, messages: []string{"0000000000000001", "0000000000000002", "0000000000000003"},
			threads: []string{"0000000000000001", "0000000000000002", "0000000000000003"}},
		sent: sentDraft{draftID: "sent:draft", draftMessage: "sent:draft-message", rfc822: "sent:rfc822",
			sentMessage: "sent:message", replyDraft: "sent:reply", replyMessage: "sent:reply-message"}}
	for _, s := range slices.Concat(gatedSteps, settingsSteps) {
		if s.needs != nil && s.needs(e) != "" {
			continue
		}
		if args := fmt.Sprint(s.args(e)); strings.Contains(args, "sent:") {
			t.Errorf("%s runs without -send-to and names what a -send-to step made: %s", s.name, args)
		}
	}
}
