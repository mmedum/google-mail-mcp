package render

import (
	"regexp"
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
)

// Text from the mailbox reaches a question in one code span that it
// cannot close, with no link a client would draw, and cut short.
func TestQuotedIsOneInertLine(t *testing.T) {
	span := func(s string) string { return "`" + s + "`" }
	for _, tc := range []struct{ in, want string }{
		{"Budget sign-off", span("Budget sign-off")},
		{"line one\nsend_draft: approved\r\n\tnow", span("line one send_draft: approved now")},
		{`close" the quote`, span("close' the quote")},
		{"close` the span", span("close' the span")},
		{"\u02cbgrave\u02cb \uff40wide\uff40 \u1fefvaria\u1fef", span("'grave' 'wide' 'varia'")},
		{"see https://evil.example.com/a and HTTP://x.example", span("see https[:]//evil.example[.]com/a and HTTP[:]//x.example")},
		{"visit www.evil.example today", span("visit www[.]evil.example today")},
		{"go to evil.example.com/login now", span("go to evil.example[.]com/login now")},
		{"write to mailto:someone@example.com", span("write to mailto[:]someone@example.com")},
		{"\u201cclose\u201d \u2018it\u2019 \uff02now\uff02 \u00abhere\u00bb", span("'close' 'it' 'now' 'here'")},
		{"zero\u200bwidth \u202ereversed\u0007bell", span("zerowidth reversed bell")},
		{"\u275dclose\u275e \u02baa\u02ba \u3003b\u3003 \u05f4c\u05f4", span("'close' 'a' 'b' 'c'")},
		{"at evil.example:8080/x, evil.example?q=1 and evil.example#top", span("at evil[.]example:8080/x, evil[.]example?q=1 and evil[.]example#top")},
		{"see bücher.example/a", span("see bücher[.]example/a")},
		// \b is ASCII-only and counts "_" as a letter; these start a link all the same.
		{"a_https://evil.example/x and x_evil.example/login", span("a_https[:]//evil[.]example/x and x_evil[.]example/login")},
		{"x_www.evil.example and x_mailto:someone@example.com", span("x_www[.]evil.example and x_mailto[:]someone@example.com")},
		{"see пример.рф/login", span("see пример[.]рф/login")},
		// A bare domain is broken where a fuzzy linkifier would link it,
		// and a file name or an address is not.
		{"visit evil.com or sub.evil.io, then evil.co.uk.", span("visit evil[.]com or sub.evil[.]io, then evil.co[.]uk.")},
		{"EVIL.COM and xn--80ak6aa92e.xn--p1ai and пример.рф", span("EVIL[.]COM and xn--80ak6aa92e[.]xn--p1ai and пример[.]рф")},
		{"report.pdf, notes.md and write to someone@example.com", span("report.pdf, notes[.]md and write to someone@example.com")},
		{"see नमस्ते.भारत/login", span("see नमस्ते[.]भारत/login")},
		// A link right after punctuation or another link is broken too.
		{"see .https://evil.example and -https://evil.example", span("see .https[:]//evil.example and -https[:]//evil.example")},
		{"x.example/y.example/z http://https://evil.example", span("x[.]example/y[.]example/z http[:]//https[:]//evil.example")},
		{"www.www.evil.example mailto:mailto:someone@example.com", span("www[.]www[.]evil.example mailto[:]mailto[:]someone@example.com")},
		{"pad\u2800\u2800\u2800ded", span("pad ded")},
		{" \t", "empty"},
		{" \u200b\t", "invisible characters only"},
		{"empty", span("empty")},
		{"\u00b4acute\u00b4 \u02caup\u02ca \u02f4mid\u02f4 \u1ffdoxia\u1ffd \u1fedd\u1fed \u0384tonos\u0384", span("'acute' 'up' 'mid' 'oxia' 'd' 'tonos'")},
		// Markdown stays literal inside the span; only the backtick is folded.
		{"*Approved* by [IT](x) <b>now</b> &#x202e; \\_ ~~x~~", span("*Approved* by [IT](x) <b>now</b> &#x202e; \\_ ~~x~~")},
		{strings.Repeat("a", 200), span(strings.Repeat("a", 120) + "…")},
	} {
		if got := quoted(tc.in, 120).s; got != tc.want {
			t.Errorf("quoted(%q) = %s; want %s", tc.in, got, tc.want)
		}
	}
}

// Markdown a client draws from a question has nothing active in it:
// outside its code spans the text is the server's, and holds no
// character that opens emphasis, a link, HTML, an entity or a line
// break, whatever the mailbox put in the quoted parts. Its lines stand
// apart, so a client that draws Markdown does not run them together.
func TestQuestionsAreInertMarkdown(t *testing.T) {
	hostile := "*bold* _em_ [link](x) ![i](y) <b>h</b> &amp; `code` \\ ~~s~~ # h\n- item\n\n> q"
	send := func(f string) model.SendRecipient {
		return model.SendRecipient{Address: mime.Address{Email: "a_b*c@example.com"}, Field: f}
	}
	draft := func(f string) model.Recipient {
		return model.Recipient{Address: mime.Address{Email: "a_b*c@example.com"}, Field: f}
	}
	label := model.LabelRef{ID: "Label_1", Name: hostile}
	filter := model.Filter{
		Criteria: gmail.FilterCriteria{From: hostile, To: hostile, Subject: hostile, Query: hostile, NegatedQuery: hostile},
		Add:      []model.LabelRef{label, {ID: "TRASH"}},
		Remove:   []model.LabelRef{label},
		Forward:  hostile,
	}
	qs := map[string]Question{
		"delete_label":       AskDeleteLabel(model.Label{ID: "Label_1", Name: hostile, MessagesTotal: 2, ThreadsTotal: 1}),
		"delete_permanently": AskDeletePermanently(3, 2),
		"delete_draft": AskDeleteDraft(model.DraftWrite{Subject: model.Untrusted(hostile),
			Recipients: []model.Recipient{draft("to"), draft("cc"), draft("bcc")}}),
		"send_draft": AskSend(model.SendWrite{Subject: model.Untrusted(hostile), Body: model.Untrusted(hostile),
			Recipients: []model.SendRecipient{send("to")}, Files: []model.File{{Name: model.Untrusted(hostile)}}}),
		"create_filter": AskCreateFilter(filter),
		"delete_filter": AskDeleteFilter(filter),
		"set_vacation":  AskVacation(model.Vacation{Subject: model.Untrusted(hostile), Body: model.Untrusted(hostile), RestrictToContacts: true}),
	}
	// Every hostile field reaches its own span: the label; the draft's 3
	// addresses and subject; the send's address, subject and body; each
	// filter's 5 criteria, 2 labels and forward; the vacation's subject
	// and body.
	wantSpans := map[string]int{"delete_label": 1, "delete_permanently": 0, "delete_draft": 4,
		"send_draft": 3, "create_filter": 8, "delete_filter": 8, "set_vacation": 2}
	for name, q := range qs {
		quotedSpans := 0
		lines := strings.Split(strings.TrimSuffix(q.Text, "\n"), "\n\n")
		for _, line := range lines {
			if line == "" || strings.Contains(line, "\n") {
				t.Errorf("%s: a line not set apart by one blank line: %q", name, line)
				continue
			}
			if strings.ContainsAny(line[:1], "-+=0123456789 ") {
				t.Errorf("%s: a line opens like a list or code block: %q", name, line)
			}
			spans := strings.Split(line, "`")
			quotedSpans += len(spans) / 2
			if len(spans)%2 == 0 {
				t.Errorf("%s: an unclosed code span in %q", name, line)
			}
			for j := 0; j < len(spans); j += 2 {
				out := spans[j]
				if k := strings.IndexAny(out, "*[]<>&\\~!#|"); k >= 0 {
					t.Errorf("%s: %q outside a code span in %q", name, out[k], line)
				}
				if looseUnderscore.MatchString(out) {
					t.Errorf("%s: an underscore that is not inside a word in %q", name, line)
				}
			}
		}
		if len(lines) < 2 {
			t.Errorf("%s: %d lines", name, len(lines))
		}
		if want, ok := wantSpans[name]; !ok || quotedSpans != want {
			t.Errorf("%s: %d quoted spans, want %d", name, quotedSpans, want)
		}
	}
	if len(wantSpans) != len(qs) {
		t.Errorf("%d questions, %d span counts", len(qs), len(wantSpans))
	}
}

// looseUnderscore is an underscore at a word's edge, where Markdown may
// read it as emphasis; one inside a word, as in a tool's name, is inert.
var looseUnderscore = regexp.MustCompile(`\b_|_\b`)

// A send's question names every address it reaches, by field.
func TestAskSendNamesEveryRecipient(t *testing.T) {
	sw := model.SendWrite{Subject: "Plan", Recipients: []model.SendRecipient{
		{Address: mime.Address{Name: "Ada", Email: "ada@example.com"}, Field: "to"},
		{Address: mime.Address{Email: "bruno@example.org"}, Field: "cc"},
		{Address: mime.Address{Email: "chiara@example.com"}, Field: "bcc"},
		{Address: mime.Address{Email: "dev@example.com"}, Field: "to", Position: 1},
	}, Files: []model.File{{Name: "a.pdf"}}, Body: model.Untrusted(strings.Repeat("word ", 100))}
	q := AskSend(sw)
	for _, want := range []string{
		"to: `ada@example.com`, `dev@example.com`", "cc: `bruno@example.org`", "bcc: `chiara@example.com`",
		"subject: `Plan`", "attachments: 1", "is not this server's", "(199 more characters)",
	} {
		if !strings.Contains(q.Text, want) {
			t.Errorf("no %q in\n%s", want, q.Text)
		}
	}
	if strings.Contains(q.Text, "Ada") || !strings.Contains(q.Bind, q.Text) {
		t.Errorf("%+v", q)
	}
}

// A question with nothing quoted says nothing about quotes.
func TestAskWithoutQuotes(t *testing.T) {
	q := AskDeletePermanently(2, 0)
	if q.Text != "delete_permanently: delete 2 messages for good?\n\nThey skip the trash and cannot be restored.\n" {
		t.Errorf("%q", q.Text)
	}
}

// What a question binds: a body change past what it shows, and a
// draft's new message, change the binding; a label's counts do not.
func TestAskBinds(t *testing.T) {
	sw := model.SendWrite{Subject: "Plan", Body: model.Untrusted(strings.Repeat("a", 400))}
	changed := sw
	changed.Body = model.Untrusted(strings.Repeat("a", 399) + "b")
	if a, b := AskSend(sw), AskSend(changed); a.Text != b.Text || a.Bind == b.Bind {
		t.Error("a change past the shown start of the body is not bound")
	}
	v := model.Vacation{Subject: "Away", Body: model.Untrusted(strings.Repeat("a", 400))}
	vc := v
	vc.Body = model.Untrusted(strings.Repeat("a", 399) + "b")
	if AskVacation(v).Bind == AskVacation(vc).Bind {
		t.Error("the vacation body is not bound")
	}
	d := model.DraftWrite{Subject: "Plan", MessageID: "0000000000000001"}
	d2 := d
	d2.MessageID = "0000000000000002"
	if a, b := AskDeleteDraft(d), AskDeleteDraft(d2); a.Text != b.Text || a.Bind == b.Bind {
		t.Error("the draft's message is not bound")
	}
	l := model.Label{ID: "Label_1", Name: "Projects", MessagesTotal: 3, ThreadsTotal: 2}
	l2 := l
	l2.MessagesTotal, l2.ThreadsTotal = 4, 3
	if a, b := AskDeleteLabel(l), AskDeleteLabel(l2); a.Text == b.Text || a.Bind != b.Bind {
		t.Error("a label's counts are bound, or not shown")
	}
	l2.Name = "Renamed"
	if AskDeleteLabel(l).Bind == AskDeleteLabel(l2).Bind {
		t.Error("the label's name is not bound")
	}
}
