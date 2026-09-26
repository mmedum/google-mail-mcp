package redact

import (
	"strings"
	"testing"

	coreredact "github.com/mmedum/google-mail-mcp/internal/redact"
)

// Planted values are built by concatenation, so the repository's own leak
// gate never reads one whole in this file.
var (
	plantedID      = "18c3f2a4" + "b5d6e7f8"
	plantedDraft   = "r-8123456" + "789012345678"
	plantedAccess  = "ya29." + "a0AfB_byC1234567890abcdef"
	plantedRefresh = "1//0" + "9abcdefGhijklmnop-qrstu"
	plantedSecret  = "GOCSPX-" + "abcdefghijklmnop1234"
	plantedClient  = "123456789012-" + "abcdefghijklmnopqrstuvwxyz012345" + ".apps.googleusercontent.com"
)

func TestRedactsIdsLinksAddressesAndTokens(t *testing.T) {
	r := NewRedactor(false)
	in := strings.Join([]string{
		"Subject: quarterly numbers",
		"message " + plantedID + " in thread " + plantedID,
		"draft " + plantedDraft,
		"link: https://mail.google.com/mail/u/" + "0/#inbox/" + plantedID,
		"From: A Person <someone@example.com>",
		`To: "Åse Fiktivsen" <kim@example.org>`,
		"Message-ID: <CAF=abc123@mail.example.com>",
		"tokens " + plantedAccess + " " + plantedRefresh + " " + plantedSecret + " " + plantedClient,
	}, "\n")
	got := r.Do(in)

	for _, secret := range []string{
		plantedID, plantedDraft, "mail.google.com", "someone@example.com", "kim@example.org",
		"CAF=abc123", "A Person", "Kim", "Fiktivsen",
		plantedAccess, plantedRefresh, plantedSecret, plantedClient,
	} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived redaction:\n%s", secret, got)
		}
	}
	// Prose cannot be told from a subject, so it stays, and Summary says so.
	for _, kept := range []string{"Subject: quarterly numbers", "message <ID_1> in thread <ID_1>", "From: "} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q is missing:\n%s", kept, got)
		}
	}
}

func TestOrdinaryWordsSurvive(t *testing.T) {
	r := NewRedactor(false)
	in := "search_messages returned 3 results; include_spam_trash false; 2026-09-25T10:00:00Z"
	if got := r.Do(in); got != in {
		t.Errorf("ordinary text was changed:\n%s\n%s", in, got)
	}
}

func TestSummaryAlwaysWarnsAboutContent(t *testing.T) {
	if s := NewRedactor(false).Summary(); !strings.Contains(s, "never redacted") {
		t.Errorf("summary with nothing redacted = %q", s)
	}
	busy := NewRedactor(false)
	busy.Do("a@example.com")
	if s := busy.Summary(); !strings.Contains(s, "1 email") || !strings.Contains(s, "never redacted") {
		t.Errorf("summary after redacting = %q", s)
	}
	if s := NewRedactor(true).Summary(); !strings.Contains(s, "redaction off") {
		t.Errorf("summary with redaction off = %q", s)
	}
}

func TestOffLeavesEverything(t *testing.T) {
	r := NewRedactor(true)
	if got := r.Do("someone@example.com"); got != "someone@example.com" {
		t.Errorf("off still redacted: %q", got)
	}
}

// ID is the server's own truncation, so a maintainer's report and a log
// line show the same key for the same id. A short id is masked whole
// rather than printed: the old copy printed it, which is what it is for.
func TestIDIsTheServers(t *testing.T) {
	for _, id := range []string{plantedID, plantedDraft, "abc", "abcdef", "", "ünïcödé-id"} {
		if got, want := ID(id), coreredact.ID(id); got != want {
			t.Errorf("ID(%q) = %q, the server says %q", id, got, want)
		}
	}
}

// A draft id and a refresh token are the shapes the server masks, so a
// value the server would hide in its own output is hidden here too.
func TestDraftIDsAndRefreshTokensMatchTheServer(t *testing.T) {
	shortDraft := "r-1234" + "56789"
	bareRefresh := "1//" + "abcdefghijk"
	for _, v := range []string{shortDraft, bareRefresh} {
		if coreredact.Line(v) == v {
			t.Fatalf("the server does not mask %q; the fixture is wrong", v)
		}
		if got := NewRedactor(false).Do("value " + v); strings.Contains(got, v) {
			t.Errorf("%q survived: %q", v, got)
		}
	}
}

func TestTheCloudProjectNumberInAReceivedHeaderIsMasked(t *testing.T) {
	r := NewRedactor(false)
	got := r.Do("Received: from 123456789012 named unknown by gmailapi.google.com with HTTPREST")
	if strings.Contains(got, "123456789012") {
		t.Errorf("the project number survived: %s", got)
	}
}
