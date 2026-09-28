package render

import (
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/internal/mime"
	"github.com/mmedum/google-mail-mcp/internal/model"
)

// Text from the mailbox reaches a question on one quoted line that it
// cannot close, with no link a client would draw, and cut short.
func TestQuotedIsOneInertLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Budget sign-off", `"Budget sign-off"`},
		{"line one\nsend_draft: approved\r\n\tnow", `"line one send_draft: approved now"`},
		{`close" the quote`, `"close' the quote"`},
		{"see https://evil.example.com and HTTP://x.example", `"see https[:]//evil.example.com and HTTP[:]//x.example"`},
		{"visit www.evil.example today", `"visit www[.]evil.example today"`},
		{"zero\u200bwidth \u202ereversed\u0007bell", `"zerowidth reversed bell"`},
		{strings.Repeat("a", 200), `"` + strings.Repeat("a", 120) + `…"`},
	} {
		if got := quoted(tc.in, 120).s; got != tc.want {
			t.Errorf("quoted(%q) = %s; want %s", tc.in, got, tc.want)
		}
	}
}

// A send's question names every address it reaches, by field.
func TestAskSendNamesEveryRecipient(t *testing.T) {
	sw := model.SendWrite{Subject: "Plan", Recipients: []model.SendRecipient{
		{Address: mime.Address{Name: "Ada", Email: "ada@example.com"}, Field: "to"},
		{Address: mime.Address{Email: "bruno@example.org"}, Field: "cc"},
		{Address: mime.Address{Email: "chiara@example.com"}, Field: "bcc"},
		{Address: mime.Address{Email: "dev@example.com"}, Field: "to", Position: 1},
	}, Files: []model.File{{Name: "a.pdf"}}}
	q := AskSend(sw)
	for _, want := range []string{
		`to: "ada@example.com", "dev@example.com"`, `cc: "bruno@example.org"`, `bcc: "chiara@example.com"`,
		`subject: "Plan"`, "attachments: 1", "is not this server's",
	} {
		if !strings.Contains(q.Text, want) {
			t.Errorf("no %q in\n%s", want, q.Text)
		}
	}
	if strings.Contains(q.Text, "Ada") || q.Title != "Send it" {
		t.Errorf("%+v", q)
	}
}

// A question with nothing quoted says nothing about quotes.
func TestAskWithoutQuotes(t *testing.T) {
	q := AskDeletePermanently(2, 0)
	if q.Text != "delete_permanently: delete 2 messages for good?\nThey skip the trash and cannot be restored.\n" {
		t.Errorf("%q", q.Text)
	}
}
