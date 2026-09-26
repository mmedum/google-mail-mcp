//go:build live

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
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
