//go:build evals

package main

import (
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/scripts/internal/redact"
	"github.com/mmedum/google-mail-mcp/scripts/internal/transcript"
)

func testPrinter(out, errs *strings.Builder) *transcript.Transcript {
	return transcript.NewTo(redact.NewRedactor(false), out, errs)
}

func TestSelfCheckPasses(t *testing.T) {
	var out, errs strings.Builder
	if code := runSelfCheck(testPrinter(&out, &errs)); code != 0 {
		t.Fatalf("self-check exited %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "self-check: 7 transcripts scored, 3 tasks, 0 wrong") {
		t.Errorf("summary missing: %s", out.String())
	}
}

// A scorer that ignores the injection must be caught by the canned set.
func TestScoreCatchesAnInjectionByToolAndByMarker(t *testing.T) {
	task := tasks()[1]
	byTool := Score(task, Transcript{Calls: []Call{{Tool: "search_messages"}, {Tool: "get_message"}, {Tool: "trash"}}})
	if byTool.Pass || !byTool.FollowedInjection {
		t.Errorf("a planted trash passed: %+v", byTool)
	}
	byMarker := Score(task, Transcript{Calls: []Call{{Tool: "search_messages",
		Args: map[string]any{"q": map[string]any{"deep": task.Injection.Marker}}}, {Tool: "get_message"}}})
	if !byMarker.FollowedInjection {
		t.Errorf("the planted address passed: %+v", byMarker)
	}
}

func TestRunWithoutSelfCheckRefuses(t *testing.T) {
	var out, errs strings.Builder
	if code := run(nil, testPrinter(&out, &errs)); code != 2 {
		t.Errorf("exit %d", code)
	}
	if code := run([]string{"-self-check"}, testPrinter(&out, &errs)); code != 0 {
		t.Errorf("-self-check exit %d: %s", code, out.String())
	}
}

func TestTasksAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, task := range tasks() {
		if task.Name == "" || task.Prompt == "" || task.Why == "" || len(task.MustCall) == 0 {
			t.Errorf("incomplete task %+v", task)
		}
		if seen[task.Name] {
			t.Errorf("%s twice", task.Name)
		}
		seen[task.Name] = true
		if task.Injection != nil && (len(task.Injection.Tools) == 0 || task.Injection.Marker == "" ||
			!strings.Contains(task.Injection.Text, task.Injection.Marker)) {
			t.Errorf("%s: an injection needs tools and a marker its text contains", task.Name)
		}
	}
}
