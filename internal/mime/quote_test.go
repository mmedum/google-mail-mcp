package mime

import (
	"strings"
	"testing"
)

type wantSpan struct {
	kind     SpanKind
	text     string // the span's text, exactly
	trailing bool
}

func TestFindSpans(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []wantSpan
	}{
		{"none", "Just a note.\nSecond line.", nil},
		{"gmail style quote",
			"Sounds good.\n\nOn Mon, 2 Mar 2026 at 09:00, Ada Quill <ada@example.com> wrote:\n> Shall we meet?\n> Tuesday works.\n",
			[]wantSpan{{SpanQuote, "On Mon, 2 Mar 2026 at 09:00, Ada Quill <ada@example.com> wrote:\n> Shall we meet?\n> Tuesday works.", true}}},
		{"wrapped attribution",
			"Yes.\n\nOn Mon, 2 Mar 2026 at 09:00, Ada Quill\n<ada@example.com> wrote:\n\n> Shall we?\n",
			[]wantSpan{{SpanQuote, "On Mon, 2 Mar 2026 at 09:00, Ada Quill\n<ada@example.com> wrote:\n\n> Shall we?", true}}},
		{"french attribution",
			"Oui.\n\nLe lun. 2 mars 2026, Ada a écrit :\n> Question ?\n",
			[]wantSpan{{SpanQuote, "Le lun. 2 mars 2026, Ada a écrit :\n> Question ?", true}}},
		{"german attribution",
			"Ja.\nAm 02.03.2026 um 09:00 schrieb Ada <ada@example.com>:\n> Frage?",
			[]wantSpan{{SpanQuote, "Am 02.03.2026 um 09:00 schrieb Ada <ada@example.com>:\n> Frage?", true}}},
		{"blank lines inside a quote run",
			"Top.\n> a\n>\n\n> b\nBottom.",
			[]wantSpan{{SpanQuote, "> a\n>\n\n> b", false}}},
		{"inline reply",
			"> first question\nanswer one\n> second question\nanswer two",
			[]wantSpan{{SpanQuote, "> first question", false}, {SpanQuote, "> second question", false}}},
		{"signature",
			"Thanks,\nAda\n-- \nAda Quill\nExample Org\n",
			[]wantSpan{{SpanSignature, "-- \nAda Quill\nExample Org", true}}},
		{"signature then quote",
			"Fine.\n-- \nAda\n\nOn Tue, Bruno wrote:\n> ok?",
			[]wantSpan{{SpanSignature, "-- \nAda", true}, {SpanQuote, "On Tue, Bruno wrote:\n> ok?", true}}},
		{"dash dash without space",
			"Body\n--\nSig",
			[]wantSpan{{SpanSignature, "--\nSig", true}}},
		{"outlook header",
			"Approved.\n\n________________________________\nFrom: Ada Quill <ada@example.com>\nSent: Monday, March 2, 2026 9:00 AM\nTo: Bruno <bruno@example.org>\nSubject: Budget\n\nPlease approve.",
			[]wantSpan{{SpanQuote, "________________________________\nFrom: Ada Quill <ada@example.com>\nSent: Monday, March 2, 2026 9:00 AM\nTo: Bruno <bruno@example.org>\nSubject: Budget\n\nPlease approve.", true}}},
		{"outlook header without rule",
			"Ok.\nFrom: Ada\nDate: today\nSubject: x\nold",
			[]wantSpan{{SpanQuote, "From: Ada\nDate: today\nSubject: x\nold", true}}},
		{"original message",
			"See below.\n-----Original Message-----\nFrom: x\nold text",
			[]wantSpan{{SpanQuote, "-----Original Message-----\nFrom: x\nold text", true}}},
		{"all quote is not collapsed", "> only quoted\n> text", nil},
		{"forward header at top is the message",
			"From: Ada\nSent: today\nTo: Bruno\nforwarded content", nil},
		{"long signature is not a signature",
			"Body\n-- \n" + strings.Repeat("line\n", 40), nil},
		{"crlf", "Yes.\r\n> old\r\n", []wantSpan{{SpanQuote, "> old\r", true}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FindSpans(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("spans = %+v, want %d", got, len(c.want))
			}
			for i, s := range got {
				text := c.in[s.Start:s.End]
				if s.Kind != c.want[i].kind || text != c.want[i].text || s.Trailing != c.want[i].trailing {
					t.Errorf("[%d] = %s %q trailing=%v, want %s %q trailing=%v", i, s.Kind, text, s.Trailing,
						c.want[i].kind, c.want[i].text, c.want[i].trailing)
				}
				if s.Lines != strings.Count(text, "\n")+1 {
					t.Errorf("[%d] lines = %d for %q", i, s.Lines, text)
				}
			}
		})
	}
}

func TestUnflow(t *testing.T) {
	in := "This is a long \nparagraph that \nwas wrapped.\n\n> quoted \n> flowed\n>> deeper\n \nstuffed\n-- \nsig"
	want := "This is a long paragraph that was wrapped.\n\n> quoted flowed\n>> deeper\n\nstuffed\n-- \nsig"
	if got := unflow(in, false); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := unflow("Zusammen \ngeschrieben", true); got != "Zusammengeschrieben" {
		t.Fatalf("delsp: %q", got)
	}
	if got := unflow("dangling \n", false); got != "dangling " {
		t.Fatalf("dangling: %q", got)
	}
}

func TestIsPlaceholder(t *testing.T) {
	yes := []string{
		"",
		"   ",
		"To view this email, open it in a browser.",
		"View this email in your browser: https://example.com/v",
		"This is an HTML message. Please use an HTML-capable mail program to read it.",
		"Your email client does not support HTML messages.",
		"This is a multi-part message in MIME format.",
		"Please view the HTML version of this message.",
	}
	for _, s := range yes {
		if !isPlaceholder(s) {
			t.Errorf("not a placeholder: %q", s)
		}
	}
	no := []string{
		"Hi Ada, the meeting is at 10.",
		strings.Repeat("A long real body that mentions the web version once. ", 20),
	}
	for _, s := range no {
		if isPlaceholder(s) {
			t.Errorf("placeholder: %q", s)
		}
	}
}
