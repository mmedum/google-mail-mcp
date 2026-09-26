package transcript

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/scripts/internal/redact"
)

// An address arrives three ways: a literal line, a format argument, and
// the run's own failure. All three are redacted, to the same placeholder.
func TestEveryLinePrintedIsRedacted(t *testing.T) {
	var out, errOut bytes.Buffer
	tr := NewTo(redact.NewRedactor(false), &out, &errOut)

	tr.Say("From: someone@example.com")
	tr.Sayf("=== create_draft %s ===", `{"to":"someone@example.com"}`)
	tr.Fail("livemail: inserting for someone@example.com failed")

	both := out.String() + errOut.String()
	if strings.Contains(both, "someone@example.com") {
		t.Errorf("an address reached the terminal:\n%s", both)
	}
	if strings.Count(both, "<EMAIL_1>") != 3 {
		t.Errorf("the same address should be the same placeholder every time:\n%s", both)
	}
	if got := strings.Count(errOut.String(), "\n"); got != 1 {
		t.Errorf("the failure went to stderr as %d lines, want 1", got)
	}
}

func TestALineIsALine(t *testing.T) {
	var out bytes.Buffer
	tr := NewTo(redact.NewRedactor(false), &out, &out)
	tr.Say("a result that ends in a newline\n")
	tr.Sayf("and one with %s\n\n", "two")
	if got := out.String(); got != "a result that ends in a newline\nand one with two\n" {
		t.Errorf("got %q", got)
	}
}

func TestRawLeavesEverythingAlone(t *testing.T) {
	var out bytes.Buffer
	tr := NewTo(redact.NewRedactor(true), &out, &out)
	tr.Say("From: someone@example.com")
	if !strings.Contains(out.String(), "someone@example.com") {
		t.Errorf("-raw redacted anyway: %q", out.String())
	}
	if !strings.Contains(tr.Summary(), "redaction off") {
		t.Errorf("the summary does not say redaction was off: %q", tr.Summary())
	}
}
