package livecover

import (
	"strings"
	"testing"
)

// get_profile takes no options, so a run that called it records an empty
// set; it must still count as called.
func TestARunSaysWhatItActuallySent(t *testing.T) {
	rec := NewRecorder()
	rec.Sent("get_profile", map[string]any{})
	rec.Sent("get_message", map[string]any{"id": "x"})

	published := map[string][]string{
		"get_profile": {},
		"get_message": {"format", "id"},
		"get_thread":  {"id", "max_messages"},
	}
	report := rec.Report(published, nil)
	for _, want := range []string{"sent 1 of 4 options", "in 2 calls", "get_thread"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not say %q: %s", want, report)
		}
	}
	if strings.Contains(report, "get_profile") {
		t.Errorf("a tool with no options was reported as never called: %s", report)
	}
}

// A step that exists in the source and did not run is named: that is the
// whole reason there is a recorder beside the static gate.
func TestAStepThatExistsAndDoesNotRunIsNamed(t *testing.T) {
	rec := NewRecorder()
	rec.Sent("search_messages", map[string]any{"q": "x"})

	published := map[string][]string{"search_messages": {"q", "max_results", "include_spam_trash"}}
	believed := map[string]map[string]bool{"search_messages": {"q": true, "max_results": true}}
	report := rec.Report(published, believed)
	if !strings.Contains(report, "search_messages.max_results") {
		t.Errorf("an option the source claims and the run did not send is not named: %s", report)
	}
	if strings.Contains(report, "include_spam_trash") {
		t.Errorf("an option the source does not claim is the gate's business: %s", report)
	}
}

func TestAToolNeverCalledIsReportedOnce(t *testing.T) {
	rec := NewRecorder()
	rec.Sent("get_message", map[string]any{"id": "x"})

	published := map[string][]string{"get_message": {"id"}, "get_draft": {"id", "format"}}
	believed := map[string]map[string]bool{"get_draft": {"id": true, "format": true}}
	report := rec.Report(published, believed)
	if n := strings.Count(report, "get_draft"); n != 1 {
		t.Errorf("a tool never called is named %d times, want 1:\n%s", n, report)
	}
}
