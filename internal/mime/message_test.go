package mime

import (
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
)

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

func TestParseRawPlain(t *testing.T) {
	m := ParseRaw(crlf(`From: =?UTF-8?Q?Zo=C3=AB?= <zoe@example.org>
To: Ada Quill <ada@example.com>, bruno@example.org
Cc: "Holm, Freya" <freya@example.com>
Reply-To: replies@example.invalid
Subject: =?UTF-8?Q?Caf=C3=A9?= plans
Date: Mon, 2 Mar 2026 09:30:00 +0000
Message-ID: <m1@mail.example.com>
In-Reply-To: <m0@mail.example.com>
References: <r1@mail.example.com>
 <m0@mail.example.com>
List-Unsubscribe: <mailto:leave@example.com>
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Hello Ada,=20
this line is soft=
 broken and caf=C3=A9 is decoded.
`))
	if m.Subject != "Café plans" {
		t.Errorf("subject %q", m.Subject)
	}
	if len(m.From) != 1 || m.From[0].Name != "Zoë" || m.From[0].Email != "zoe@example.org" {
		t.Errorf("from %+v", m.From)
	}
	if len(m.To) != 2 || len(m.Cc) != 1 || m.Cc[0].Name != "Holm, Freya" || m.ReplyTo[0].Email != "replies@example.invalid" {
		t.Errorf("to %+v cc %+v reply-to %+v", m.To, m.Cc, m.ReplyTo)
	}
	if m.MessageID != "<m1@mail.example.com>" || len(m.References) != 2 || m.InReplyTo[0] != "<m0@mail.example.com>" {
		t.Errorf("ids %q %v %v", m.MessageID, m.References, m.InReplyTo)
	}
	if m.Date.IsZero() || m.ListUnsubscribe != "<mailto:leave@example.com>" {
		t.Errorf("date %v unsub %q", m.Date, m.ListUnsubscribe)
	}
	want := "Hello Ada,\nthis line is soft broken and café is decoded."
	if m.Body.Text != want || m.Body.Source != SourcePlain {
		t.Errorf("body %q (%s), want %q", m.Body.Text, m.Body.Source, want)
	}
	if got := m.Header("subject"); got != "Café plans" {
		t.Errorf("Header = %q", got)
	}
}

func TestParseRawCharsetsAndEncodings(t *testing.T) {
	cyr, _ := charmap.Windows1251.NewEncoder().Bytes([]byte("Привет, как дела?"))
	b64 := base64.StdEncoding.EncodeToString(cyr)
	m := ParseRaw([]byte("Subject: x\nContent-Type: text/plain; charset=windows-1251\nContent-Transfer-Encoding: base64\n\n" +
		b64[:10] + "\n" + b64[10:] + "\n"))
	if m.Body.Text != "Привет, как дела?" {
		t.Errorf("windows-1251 base64: %q", m.Body.Text)
	}

	m = ParseRaw([]byte("Content-Type: text/plain; charset=x-nobody-knows\n\nplain ascii"))
	if m.Body.Text != "plain ascii" || len(m.Body.UnknownCharsets) != 1 {
		t.Errorf("unknown charset: %q %v", m.Body.Text, m.Body.UnknownCharsets)
	}

	m = ParseRaw([]byte("Content-Type: text/plain\nContent-Transfer-Encoding: base64\n\nSGVsbG8gd29y!!!bGQ=\n"))
	if !strings.HasPrefix(m.Body.Text, "Hello wor") {
		t.Errorf("corrupt base64 keeps prefix: %q", m.Body.Text)
	}

	m = ParseRaw([]byte("Content-Type: text/plain; charset=utf-8; format=flowed; delsp=no\n\nA flowed \nline.\n"))
	if m.Body.Text != "A flowed line." {
		t.Errorf("flowed: %q", m.Body.Text)
	}

	m = ParseRaw([]byte("Content-Type: text/html\n\n<html><head><meta charset=\"iso-8859-1\"></head><body>caf\xe9</body></html>"))
	if m.Body.Text != "café" || m.Body.Source != SourceHTML {
		t.Errorf("html meta charset: %q", m.Body.Text)
	}

	m = ParseRaw([]byte("Subject: no type\n\nbody\u200B text"))
	if m.Body.Text != "body text" || m.Body.HiddenChars() != 1 || m.ContentType != "text/plain" {
		t.Errorf("default type: %q hidden %d type %q", m.Body.Text, m.Body.HiddenChars(), m.ContentType)
	}

	m = ParseRaw([]byte("Subject: =?UTF-8?Q?a=E2=80=AEb?=\nFrom: \"Ev\u202Eil\" <e@example.com>\n\nx"))
	if m.Subject != "ab" || m.From[0].Name != "Evil" || m.HeaderHidden != 2 {
		t.Errorf("header invisibles: %q %q %d", m.Subject, m.From[0].Name, m.HeaderHidden)
	}

	m = ParseRaw([]byte("To: Ada <ada@example.com, bruno@example.org\n\nx"))
	if len(m.LenientHeaders) != 1 || m.LenientHeaders[0] != "To" || len(m.To) != 2 {
		t.Errorf("lenient: %v %+v", m.LenientHeaders, m.To)
	}
}

const placeholderAlt = `Subject: Newsletter
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="alt"

preamble is ignored
--alt
Content-Type: text/plain; charset=utf-8

View this email in your browser.
--alt
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: quoted-printable

<p>Real <b>content</b> here.</p><div style=3D"display:none">hidden bait</div>
--alt--
epilogue is ignored
`

func TestBodySelection(t *testing.T) {
	m := ParseRaw(crlf(placeholderAlt))
	if m.Body.Text != "Real content here." || m.Body.Source != SourceHTML || !m.Body.PlaceholderSkipped {
		t.Fatalf("placeholder: %+v", m.Body)
	}
	if m.Body.HiddenChars() != 11 {
		t.Fatalf("hidden %+v", m.Body.Hidden)
	}
	if len(m.Body.PartIDs) != 1 || m.Body.PartIDs[0] != "1" {
		t.Fatalf("part ids %v", m.Body.PartIDs)
	}

	real := strings.Replace(placeholderAlt, "View this email in your browser.", "The plain version is the real one.", 1)
	m = ParseRaw(crlf(real))
	if m.Body.Text != "The plain version is the real one." || m.Body.Source != SourcePlain || m.Body.PlaceholderSkipped {
		t.Fatalf("plain preferred: %+v", m.Body)
	}

	htmlOnly := "Content-Type: text/html; charset=utf-8\n\n<p>Only <a href=\"https://track.example.invalid/c\">example.com</a></p>"
	m = ParseRaw([]byte(htmlOnly))
	if m.Body.Text != "Only example.com <track.example.invalid>" || len(m.Body.Mismatches()) != 1 {
		t.Fatalf("html only: %q %+v", m.Body.Text, m.Body.Links)
	}
}

const nested = `From: ada@example.com
Subject: Nested
Content-Type: multipart/mixed; boundary=outer

--outer
Content-Type: multipart/related; boundary="rel"

--rel
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

R=E9sum=E9 attached.
--alt
Content-Type: text/html; charset=iso-8859-1

<p>R&eacute;sum&eacute; attached.</p>
--alt--
--rel
Content-Type: image/png
Content-ID: <logo@example.com>
Content-Transfer-Encoding: base64

iVBORw0KGgo=
--rel--
--outer
Content-Type: application/pdf; name="fallback.pdf"
Content-Disposition: attachment; filename*=iso-8859-1'fr'r%E9sum%E9.pdf
Content-Transfer-Encoding: base64

JVBERi0=
--outer
Content-Type: text/calendar; charset=utf-8; method=REQUEST

BEGIN:VCALENDAR
METHOD:REQUEST
END:VCALENDAR
--outer
Content-Type: application/ics; name="invite.ics"
Content-Disposition: attachment; filename="invite.ics"

BEGIN:VCALENDAR
METHOD:CANCEL
END:VCALENDAR
--outer
Content-Type: message/rfc822

Subject: inner
From: inner@example.org

Inner body is not the outer body.
--outer
Content-Type: text/plain; name="notes.txt"
Content-Disposition: attachment

notes
--outer
Content-Type: application/octet-stream
Content-Disposition: attachment; filename="..\..\invoice` + "\u202E" + `fdp.exe"

MZ
--outer
Content-Type: text/plain

Footer added by a list.
--outer--
`

func TestNestedStructure(t *testing.T) {
	m := ParseRaw(crlf(nested))
	if m.Body.Text != "Résumé attached.\n\nFooter added by a list." || m.Body.Source != SourcePlain {
		t.Fatalf("body %q (%s)", m.Body.Text, m.Body.Source)
	}
	want := []struct {
		part, name, mt, method string
		inline, renamed        bool
	}{
		{"0.1", "attachment.png", "image/png", "", true, false},
		{"1", "résumé.pdf", "application/pdf", "", false, false},
		{"2", "invite.ics", "text/calendar", "REQUEST", false, false},
		{"3", "invite.ics", "application/ics", "CANCEL", false, false},
		{"4", "attachment.eml", "message/rfc822", "", false, false},
		{"5", "notes.txt", "text/plain", "", false, false},
		{"6", "invoicefdp.exe", "application/octet-stream", "", false, true},
	}
	if len(m.Attachments) != len(want) {
		t.Fatalf("attachments %+v", m.Attachments)
	}
	for i, w := range want {
		a := m.Attachments[i]
		if a.PartID != w.part || a.Filename != w.name || a.MimeType != w.mt || a.CalendarMethod != w.method || a.Inline != w.inline || a.Renamed != w.renamed {
			t.Errorf("[%d] = %+v, want %+v", i, a, w)
		}
	}
	if m.Attachments[0].ContentID != "logo@example.com" || !m.Attachments[0].NameMissing || m.Attachments[0].Size != 8 {
		t.Errorf("inline image %+v", m.Attachments[0])
	}
	if !strings.Contains(m.Attachments[6].DeclaredName, `\u{202E}`) {
		t.Errorf("declared name should show the override: %q", m.Attachments[6].DeclaredName)
	}
}

func TestMalformedStructure(t *testing.T) {
	// No boundary: read as text.
	m := ParseRaw([]byte("Content-Type: multipart/mixed\n\njust text"))
	if m.Body.Text != "just text" {
		t.Errorf("no boundary: %q", m.Body.Text)
	}
	// No close delimiter: the last part runs to the end.
	m = ParseRaw([]byte("Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nunterminated"))
	if m.Body.Text != "unterminated" {
		t.Errorf("no close: %q", m.Body.Text)
	}
	// Boundary never appears.
	m = ParseRaw([]byte("Content-Type: multipart/mixed; boundary=b\n\nnothing here"))
	if m.Body.Source != "" || m.Body.Text != "" {
		t.Errorf("missing parts: %+v", m.Body)
	}
	// Empty input.
	m = ParseRaw(nil)
	if m.Body.Text != "" || m.Subject != "" {
		t.Errorf("empty: %+v", m)
	}
	// Deep nesting stops.
	var b strings.Builder
	for i := range 100 {
		b.WriteString("Content-Type: multipart/mixed; boundary=b" + strings.Repeat("x", i) + "\n\n--b" + strings.Repeat("x", i) + "\n")
	}
	_ = ParseRaw([]byte(b.String()))
	// Many parts stop.
	b.Reset()
	b.WriteString("Content-Type: multipart/mixed; boundary=b\n\n")
	for range maxParts + 50 {
		b.WriteString("--b\nContent-Type: image/png\n\nx\n")
	}
	if n := len(ParseRaw([]byte(b.String())).Attachments); n > maxParts {
		t.Errorf("parts = %d", n)
	}
}

func b64u(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestParsePayload(t *testing.T) {
	p := &gmail.MessagePart{
		PartID:   "",
		MimeType: "multipart/mixed",
		Headers: []gmail.MessagePartHeader{
			{Name: "Subject", Value: "=?ISO-8859-1?Q?F=FCr_dich?="},
			{Name: "From", Value: "Ada <ada@example.com>"},
			{Name: "Content-Type", Value: `multipart/mixed; boundary="x"`},
		},
		Body: &gmail.MessagePartBody{},
		Parts: []gmail.MessagePart{
			{PartID: "0", MimeType: "multipart/alternative", Parts: []gmail.MessagePart{
				{PartID: "0.0", MimeType: "text/plain", Headers: []gmail.MessagePartHeader{{Name: "Content-Type", Value: "text/plain; charset=iso-8859-1"}},
					Body: &gmail.MessagePartBody{Size: 6, Data: base64.URLEncoding.EncodeToString([]byte("Gr\xfc\xdfe"))}},
				{PartID: "0.1", MimeType: "text/html", Headers: []gmail.MessagePartHeader{{Name: "Content-Type", Value: "text/html; charset=utf-8"}},
					Body: &gmail.MessagePartBody{Size: 20, Data: b64u("<p>Grüße</p>")}},
			}},
			{PartID: "1", MimeType: "application/pdf", Filename: "Bericht.pdf",
				Headers: []gmail.MessagePartHeader{{Name: "Content-Type", Value: "application/pdf"}},
				Body:    &gmail.MessagePartBody{Size: 12345, AttachmentID: "ATT-1"}},
		},
	}
	m, err := ParsePayload(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Für dich" || m.Body.Text != "Grüße" || m.Body.Source != SourcePlain {
		t.Fatalf("got %q %q", m.Subject, m.Body.Text)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "Bericht.pdf" || m.Attachments[0].AttachmentID != "ATT-1" || m.Attachments[0].Size != 12345 {
		t.Fatalf("attachments %+v", m.Attachments)
	}
	if len(m.NeedsFetch) != 0 {
		t.Fatalf("needs fetch %+v", m.NeedsFetch)
	}

	if _, err := ParsePayload(nil, nil); err == nil {
		t.Fatal("nil payload accepted")
	}
}

func TestParsePayloadNeedsFetch(t *testing.T) {
	p := &gmail.MessagePart{
		MimeType: "text/html",
		Headers:  []gmail.MessagePartHeader{{Name: "Content-Type", Value: "text/html; charset=utf-8"}},
		Body:     &gmail.MessagePartBody{Size: 90000, AttachmentID: "BIG"},
	}
	m, err := ParsePayload(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.NeedsFetch) != 1 || m.NeedsFetch[0].AttachmentID != "BIG" || m.NeedsFetch[0].PartID != "" || len(m.Body.Missing) != 1 {
		t.Fatalf("needs fetch %+v missing %v", m.NeedsFetch, m.Body.Missing)
	}
	m, _ = ParsePayload(p, map[string][]byte{"": []byte("<p>fetched</p>")})
	if m.Body.Text != "fetched" || len(m.NeedsFetch) != 0 || len(m.Body.Missing) != 0 {
		t.Fatalf("after fetch %+v", m.Body)
	}
}

func TestParsePayloadMetadata(t *testing.T) {
	p := &gmail.MessagePart{MimeType: "multipart/alternative", Headers: []gmail.MessagePartHeader{{Name: "Subject", Value: "Hi"}}}
	m, _ := ParsePayload(p, nil)
	if m.Subject != "Hi" || m.Body.Source != "" || len(m.NeedsFetch) != 0 || len(m.Body.Missing) != 0 {
		t.Fatalf("metadata %+v", m)
	}
	p = &gmail.MessagePart{MimeType: "text/plain", Headers: []gmail.MessagePartHeader{{Name: "Subject", Value: "Hi"}}}
	m, _ = ParsePayload(p, nil)
	if len(m.Body.Missing) != 0 {
		t.Fatalf("metadata text/plain %+v", m.Body)
	}
}

func TestParseRawBase64URL(t *testing.T) {
	m, err := ParseRawBase64URL(b64u("Subject: raw\r\n\r\nbody"))
	if err != nil || m.Subject != "raw" || m.Body.Text != "body" {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := ParseRawBase64URL("  "); err == nil {
		t.Fatal("empty raw accepted")
	}
	padded := base64.URLEncoding.EncodeToString([]byte("Subject: p\n\nx"))
	if m, _ := ParseRawBase64URL(padded); m.Subject != "p" {
		t.Fatalf("padded: %+v", m)
	}
	std := base64.StdEncoding.EncodeToString([]byte("Subject: ??>>\n\n\xff\xfe"))
	if m, _ := ParseRawBase64URL(std); m.Subject != "??>>" {
		t.Fatalf("std alphabet: %q", m.Subject)
	}
}

func TestDecodeQP(t *testing.T) {
	cases := map[string]string{
		"a=3Db":          "a=b",
		"soft=\r\nbreak": "softbreak",
		"trail   \nx":    "trail\nx",
		"bad=ZZ":         "bad=ZZ",
		"end=":           "end",
		"lower=c3=a9":    "loweré",
	}
	for in, want := range cases {
		if got := string(decodeQP([]byte(in))); got != want {
			t.Errorf("decodeQP(%q) = %q, want %q", in, got, want)
		}
	}
	if got := string(decodeTransfer([]byte("x"), "8bit")); got != "x" {
		t.Error("8bit")
	}
}

func TestSpansInBody(t *testing.T) {
	m := ParseRaw([]byte("Content-Type: text/plain\n\nYes.\n\nOn Mon, Ada wrote:\n> Well?\n"))
	if len(m.Body.Spans) != 1 || m.Body.Spans[0].Kind != SpanQuote {
		t.Fatalf("spans %+v", m.Body.Spans)
	}
}
