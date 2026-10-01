package redact

import (
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/v2/internal/model"
	coreredact "github.com/mmedum/google-mail-mcp/v2/internal/redact"
	"github.com/mmedum/google-mail-mcp/v2/internal/render"
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
		`To: "Åse Fiktivsen" <ase@example.org>`,
		"Message-ID: <CAF=abc123@mail.example.com>",
		"tokens " + plantedAccess + " " + plantedRefresh + " " + plantedSecret + " " + plantedClient,
	}, "\n")
	got := r.Do(in)

	for _, secret := range []string{
		plantedID, plantedDraft, "mail.google.com", "someone@example.com", "ase@example.org",
		"CAF=abc123", "A Person", "Åse", "Fiktivsen",
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

func TestHistoryIDsAreMasked(t *testing.T) {
	r := NewRedactor(false)
	for _, line := range []string{
		`=== list_changes {"history_id":"7399630","label":"x"} ===`,
		"since history 7399630, messages labeled x only",
		"history_id=7399679 continues from here next time.",
		"history 7399632 · message <ID_1> added",
		"history.list with startHistoryId=7399630 answered HTTP 404",
	} {
		got := r.Do(line)
		if strings.Contains(got, "73996") || !strings.Contains(got, "<HISTORY_") {
			t.Errorf("a history id survived: %s", got)
		}
	}
	if got := r.Do("history_id=7399630 and history 7399630"); strings.Count(got, "<HISTORY_1>") != 2 {
		t.Errorf("one id got two placeholders: %s", got)
	}
}

// The history-id positions follow list_changes's wording, so the test
// runs the renderer itself: rewording one of its lines without the
// redactor fails here, before a live transcript carries the account's
// ids again (architecture §18 row 38).
func TestHistoryIDsInTheRenderedChangesAreMasked(t *testing.T) {
	const start, current, record = "7390001", "7390099", "7390042"
	change := []model.Change{{HistoryID: record, Kind: model.ChangeAdded, MessageID: "0000000000000001",
		ThreadID: "0000000000000001"}}
	for name, l := range map[string]render.ChangeList{
		"complete": {Start: start, Changes: change, HistoryID: current},
		"paged":    {Start: start, Changes: change, HistoryID: current, NextPageToken: "page-1"},
		"labeled":  {Start: start, Changes: change, HistoryID: current, Label: &model.LabelRef{ID: "Label_1", Name: "L"}},
		"expired":  {Start: start, Expired: true, HistoryID: current},
	} {
		text := render.Changes(l, render.Options{}).Text
		got := NewRedactor(false).Do(text)
		for _, id := range []string{start, current, record} {
			if strings.Contains(got, id) {
				t.Errorf("%s: history id %s survived:\n%s", name, id, got)
			}
		}
	}
}

// The live driver's download directory sits under the system's
// temporary directory, whose path can carry an account name.
func TestTheDriversTemporaryDirectoryIsMasked(t *testing.T) {
	for _, line := range []string{
		"Path: /home/someone/tmp/livemail-20260926-203229-cce4b7-12345/livemail-synthetic.txt",
		"Path: /var/folders/ab/cdef/T/livemail-20260926-203229-cce4b7-12345/livemail-synthetic-1.txt",
		`Path: C:\Users\someone\AppData\Local\Temp\livemail-20260926-203229-cce4b7-12345\livemail-synthetic.txt`,
	} {
		got := NewRedactor(false).Do(line)
		if strings.Contains(got, "someone") || strings.Contains(got, "folders") || !strings.Contains(got, "<DIR_1>") ||
			!strings.Contains(got, "livemail-synthetic") {
			t.Errorf("the directory was not masked, or the file name went with it: %s", got)
		}
	}
}

// One filter id keeps one placeholder wherever it appears, and an id's
// last character is masked with the rest.
func TestAFilterIDIsMaskedOnceAndWhole(t *testing.T) {
	r := NewRedactor(false)
	got := r.Do(`created filter ANe1Bmj1; {"filter_id":"ANe1Bmj1"}; filter abc1- x`)
	if strings.Count(got, "<FILTER_1>") != 2 || !strings.Contains(got, "filter <FILTER_2> x") {
		t.Errorf("%s", got)
	}
}

// Label and filter ids say what the account has set up, and are masked
// wherever they appear; a word after "filter" that is not an id stays.
func TestLabelAndFilterIDsAreMasked(t *testing.T) {
	r := NewRedactor(false)
	got := r.Do(`created filter ANe1BmjAbCdEf_12-x; labels Label_12, Label_7; ` +
		`{"filter_id":"xyz_789"}; deleted filter Zq9_abcdef; the filter is gone; a filter without confirm`)
	for _, leak := range []string{"ANe1BmjAbCdEf", "Label_12", "Label_7", "xyz_789", "Zq9_abcdef"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked: %s", leak, got)
		}
	}
	if !strings.Contains(got, "the filter is gone") || !strings.Contains(got, "a filter without confirm") {
		t.Errorf("ordinary words were masked: %s", got)
	}
}
