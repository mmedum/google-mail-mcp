package mime

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fixedBoundaries makes Build's output byte-stable for the test.
func fixedBoundaries(t *testing.T) {
	t.Helper()
	n := 0
	old := newBoundary
	newBoundary = func() string {
		n++
		return "=_test" + strings.Repeat("x", n)
	}
	t.Cleanup(func() { newBoundary = old })
}

var testDate = time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)

// leaves returns the parsed leaves of raw bytes, in order.
func leaves(raw []byte) []*node {
	count := 0
	var out []*node
	var walk func(n *node)
	walk = func(n *node) {
		if len(n.children) == 0 {
			out = append(out, n)
		}
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(fromRaw(raw, "", 0, &count))
	return out
}

func TestBuildPlainASCIIIsExact(t *testing.T) {
	fixedBoundaries(t)
	raw, err := Build(Outgoing{
		From:      &Address{Name: "Rae Reader", Email: "reader@example.com"},
		To:        []Address{{Name: "Ada Quill", Email: "ada.quill@example.com"}},
		Subject:   "Lunch on Friday",
		Text:      "Hi Ada,\nFriday at noon?\n",
		MessageID: "<abc@example.com>",
		Date:      testDate,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "From: Rae Reader <reader@example.com>\r\n" +
		"To: Ada Quill <ada.quill@example.com>\r\n" +
		"Subject: Lunch on Friday\r\n" +
		"Date: Mon, 02 Mar 2026 09:30:00 +0000\r\n" +
		"Message-ID: <abc@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"Hi Ada,\r\nFriday at noon?\r\n"
	if string(raw) != want {
		t.Fatalf("got\n%q\nwant\n%q", raw, want)
	}
}

func TestBuildRoundTripsFourScripts(t *testing.T) {
	fixedBoundaries(t)
	o := Outgoing{
		From: &Address{Name: "Zoë Ångström", Email: "zoe@example.org"},
		To: []Address{
			{Name: "山田 花子", Email: "hanako@example.com"},
			{Name: "Иван Петров", Email: "ivan@example.org"},
		},
		Cc:        []Address{{Name: "Ελένη Δοκιμή", Email: "eleni@example.org"}},
		Bcc:       []Address{{Email: "bruno.fennick@example.org"}},
		Subject:   "Réunion — 会議の案内 · Отчёт · Καλημέρα, and a subject long enough that it has to fold across lines",
		Text:      "Grüße,\nこんにちは\nПривет\nΓεια σας\n\n-- \nZoë",
		HTML:      "<p>Grüße <b>こんにちは</b></p>",
		MessageID: "<id.1@example.org>",
		InReplyTo: "<parent@example.com>",
		References: []string{
			"<root@example.com>", "<parent@example.com>",
		},
		Date: testDate,
		Attachments: []OutAttachment{
			{Filename: "Отчёт за квартал — 会議資料 (финальная версия).pdf", MediaType: "application/pdf", Content: []byte("%PDF-1.7 fake")},
			{Filename: "notes.txt", MediaType: "text/plain", Content: []byte("plain notes\n")},
		},
	}
	raw, err := Build(o)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(raw), "\r\n") {
		// A word longer than the line's room is written whole; checkLines
		// holds 998. Past a few characters over, the fold failed.
		if len(line) > foldAt+2 {
			t.Errorf("line %d is %d characters: %q", i, len(line), line)
		}
	}
	if bytes.ContainsAny(raw, "\x00") || !isASCII(raw) {
		t.Fatal("the raw message is not ASCII")
	}
	m := ParseRaw(raw)
	if m.Subject != o.Subject {
		t.Errorf("subject %q", m.Subject)
	}
	checkAddrs(t, "from", m.From, []Address{*o.From})
	checkAddrs(t, "to", m.To, o.To)
	checkAddrs(t, "cc", m.Cc, o.Cc)
	checkAddrs(t, "bcc", m.Bcc, o.Bcc)
	if len(m.LenientHeaders) > 0 {
		t.Errorf("headers read leniently: %v", m.LenientHeaders)
	}
	if m.MessageID != o.MessageID || strings.Join(m.InReplyTo, " ") != o.InReplyTo ||
		strings.Join(m.References, " ") != strings.Join(o.References, " ") {
		t.Errorf("ids: %q %q %q", m.MessageID, m.InReplyTo, m.References)
	}
	if m.Body.Source != SourcePlain || !strings.Contains(m.Body.Text, "こんにちは") {
		t.Errorf("body %q from %q", m.Body.Text, m.Body.Source)
	}
	if len(m.Attachments) != 2 {
		t.Fatalf("%d attachments", len(m.Attachments))
	}
	if a := m.Attachments[0]; a.DeclaredName != o.Attachments[0].Filename || a.MimeType != "application/pdf" || a.PartID != "1" {
		t.Errorf("attachment %+v", a)
	}
	ls := leaves(raw)
	if got := string(ls[0].data); got != normalizeNewlines(o.Text) {
		t.Errorf("plain part %q", got)
	}
	if got := string(ls[1].data); got != o.HTML {
		t.Errorf("html part %q", got)
	}
	if got := string(ls[2].data); got != "%PDF-1.7 fake" {
		t.Errorf("attachment content %q", got)
	}
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c > 0x7e || (c < 0x20 && c != '\r' && c != '\n' && c != '\t') {
			return false
		}
	}
	return true
}

func checkAddrs(t *testing.T, what string, got, want []Address) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d addresses, want %d: %+v", what, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %+v, want %+v", what, i, got[i], want[i])
		}
	}
}

func TestBuildRefuses(t *testing.T) {
	base := Outgoing{To: []Address{{Email: "a@example.com"}}, MessageID: "<x@example.com>"}
	cases := []struct {
		name string
		edit func(o *Outgoing)
	}{
		{"line break in subject", func(o *Outgoing) { o.Subject = "hi\r\nBcc: evil@example.com" }},
		{"control in name", func(o *Outgoing) { o.To[0].Name = "a\x00b" }},
		{"non-ASCII address", func(o *Outgoing) { o.To[0].Email = "zoë@example.org" }},
		{"not an address", func(o *Outgoing) { o.To[0].Email = "a@b@c" }},
		{"address with a name", func(o *Outgoing) { o.To[0].Email = "A <a@example.com>" }},
		{"no message id", func(o *Outgoing) { o.MessageID = "" }},
		{"bad in-reply-to", func(o *Outgoing) { o.InReplyTo = "parent" }},
		{"bad reference", func(o *Outgoing) { o.References = []string{"<a@b> <c@d>"} }},
		{"path as attachment name", func(o *Outgoing) { o.Attachments = []OutAttachment{{Filename: "../x"}} }},
		{"bad media type", func(o *Outgoing) { o.Attachments = []OutAttachment{{Filename: "x", MediaType: "text"}} }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			o := base
			o.To = append([]Address(nil), base.To...)
			tt.edit(&o)
			if _, err := Build(o); err == nil {
				t.Fatal("built")
			}
		})
	}
	if _, err := Build(Outgoing{To: []Address{{Name: "a\nb", Email: "a@example.com"}}, MessageID: "<x@y>"}); !errors.Is(err, ErrControl) {
		t.Errorf("want ErrControl, got %v", err)
	}
}

func TestFormatAddressesQuotesAndEncodes(t *testing.T) {
	cases := []struct {
		in   Address
		want string
	}{
		{Address{Email: "a@example.com"}, "a@example.com"},
		{Address{Name: "Ada Quill", Email: "a@example.com"}, "Ada Quill <a@example.com>"},
		{Address{Name: "Quill, Ada", Email: "a@example.com"}, `"Quill, Ada" <a@example.com>`},
		{Address{Name: `Ada "the" Quill`, Email: "a@example.com"}, `"Ada \"the\" Quill" <a@example.com>`},
		{Address{Name: "Ada  Quill", Email: "a@example.com"}, `"Ada  Quill" <a@example.com>`},
		{Address{Name: "=?utf-8?q?x?=", Email: "a@example.com"}, "=?utf-8?b?PT91dGYtOD9xP3g/PQ==?= <a@example.com>"},
		{Address{Name: "Zoë", Email: "z@example.org"}, "=?utf-8?b?Wm/Dqw==?= <z@example.org>"},
	}
	for _, tt := range cases {
		got, err := FormatAddresses([]Address{tt.in})
		if err != nil || got != tt.want {
			t.Errorf("FormatAddresses(%+v) = %q, %v; want %q", tt.in, got, err, tt.want)
			continue
		}
		if strings.Contains(tt.in.Name, "=?") {
			// The parser decodes a word net/mail left encoded, because
			// senders quote encoded-words; a name that is literally one
			// reads back decoded. Written safely, not round-tripped.
			continue
		}
		back, strict := ParseAddressList(got)
		if !strict || len(back) != 1 || back[0] != tt.in {
			t.Errorf("%q parsed back as %+v (strict %v)", got, back, strict)
		}
	}
}

func TestEncodeWordsStayWithinSeventyFive(t *testing.T) {
	s := strings.Repeat("日本語のテキスト😀", 20)
	for w := range strings.SplitSeq(encodeWords(s), " ") {
		if len(w) > maxWordChars {
			t.Fatalf("word of %d characters", len(w))
		}
	}
	if got := DecodeHeader(encodeWords(s)); got != s {
		t.Fatalf("decoded %q", got)
	}
}

func TestFilenameContinuationsRoundTrip(t *testing.T) {
	name := strings.Repeat("Ω%", 60) + ".txt"
	param := filenameParam(name)
	if !strings.Contains(param, "filename*1*=") {
		t.Fatalf("no continuation in %q", param)
	}
	_, params := ParseMediaType("attachment; " + param)
	if params["filename"] != name {
		t.Fatalf("read back %q", params["filename"])
	}
	for seg := range strings.SplitSeq(param, "; ") {
		if strings.Contains(seg[len(seg)-min(2, len(seg)):], "%") {
			t.Fatalf("segment %q splits an escape", seg)
		}
	}
}

func TestNewMessageID(t *testing.T) {
	id := NewMessageID("reader@Example.COM")
	if !ValidMessageID(id) || !strings.HasSuffix(id, "@example.com>") {
		t.Fatalf("id %q", id)
	}
	if a, b := NewMessageID("a@example.com"), NewMessageID("a@example.com"); a == b {
		t.Fatal("two ids are equal")
	}
	if id := NewMessageID("a@not a domain"); !strings.HasSuffix(id, "@google-mail-mcp.invalid>") {
		t.Fatalf("id %q", id)
	}
}

func TestMediaTypeFor(t *testing.T) {
	for name, want := range map[string]string{
		"a.PDF": "application/pdf", "b.tar.gz": "application/octet-stream", "c": "application/octet-stream",
		"d.jpeg": "image/jpeg",
	} {
		if got := MediaTypeFor(name); got != want {
			t.Errorf("MediaTypeFor(%q) = %q", name, got)
		}
	}
}

func TestFoldKeepsTheValue(t *testing.T) {
	line := "Subject: " + strings.Repeat("word ", 40) + strings.Repeat("x", 120) + " end"
	folded := fold(line)
	if strings.ReplaceAll(folded, "\r\n", "") != line {
		t.Fatalf("unfolded differs:\n%q", folded)
	}
	for _, l := range strings.Split(folded, "\r\n") {
		if len(l) > foldAt && strings.Contains(strings.TrimPrefix(l, " "), " ") {
			t.Errorf("line of %d with a space in it: %q", len(l), l)
		}
	}
}

func TestCheckLinesRefusesOverlongLines(t *testing.T) {
	if err := checkLines([]byte("a\r\n" + strings.Repeat("b", 999) + "\r\n")); err == nil {
		t.Fatal("accepted a 999-character line")
	}
	if err := checkLines([]byte(strings.Repeat("b", 998))); err != nil {
		t.Fatal(err)
	}
}

// FuzzBuildRoundTrip holds §4.10's property: what Build writes, the
// parser reads back to the same fields, whatever the text and names.
func FuzzBuildRoundTrip(f *testing.F) {
	f.Add("Réunion 会議", "Zoë Ångström", "body\r\nline two  \n\n-- \nsig", "<p>x</p>", "Отчёт.pdf", []byte{0, 1, 2})
	f.Add("=?utf-8?q?x?=", `Quill, "Ada"`, strings.Repeat("long line ", 30), "", "a b", []byte("x"))
	f.Add("  spaced   subject  ", "山田 花子", "", "", strings.Repeat("名", 90), []byte{})
	f.Fuzz(func(t *testing.T, subject, name, text, html, filename string, content []byte) {
		for _, s := range []string{subject, name, text, html, filename} {
			if !utf8.ValidString(s) {
				t.Skip() // a JSON string is valid UTF-8
			}
		}
		if HasControl(subject) || HasControl(name) || HasControl(filename) || strings.ContainsAny(filename, `/\`) ||
			filename == "" || filename == "." || filename == ".." || filename != strings.TrimSpace(filename) ||
			text == "" && html != "" {
			t.Skip() // refused by Build or by the service before it
		}
		if strings.Contains(name, "=?") {
			t.Skip() // see TestFormatAddressesQuotesAndEncodes
		}
		name = strings.Join(strings.Fields(name), " ") // as net/mail reads a name the caller wrote
		o := Outgoing{
			From: &Address{Name: name, Email: "zoe@example.org"}, To: []Address{{Name: name, Email: "a@example.com"}},
			Subject: subject, Text: text, HTML: html, MessageID: "<f@example.com>", Date: testDate,
			Attachments: []OutAttachment{{Filename: filename, Content: content}},
		}
		raw, err := Build(o)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		m := ParseRaw(raw)
		wantSubject, _ := StripInvisible(subject)
		if m.Subject != strings.TrimSpace(wantSubject) {
			t.Fatalf("subject %q, want %q", m.Subject, subject)
		}
		wantName, _ := StripInvisible(name)
		if len(m.To) != 1 || m.To[0].Name != strings.TrimSpace(wantName) || m.To[0].Email != "a@example.com" {
			t.Fatalf("to %+v, want name %q", m.To, name)
		}
		if len(m.LenientHeaders) > 0 {
			t.Fatalf("read leniently: %v in\n%s", m.LenientHeaders, raw)
		}
		ls := leaves(raw)
		if got := string(ls[0].data); got != normalizeNewlines(text) {
			t.Fatalf("text %q, want %q", got, text)
		}
		i := 1
		if html != "" {
			if got := string(ls[1].data); got != normalizeNewlines(html) {
				t.Fatalf("html %q", got)
			}
			i = 2
		}
		if !bytes.Equal(ls[i].data, content) || ls[i].declaredName() != filename {
			t.Fatalf("attachment %q named %q", ls[i].data, ls[i].declaredName())
		}
	})
}

// A subject with no space to fold at is encoded, so its lines stay short
// and it reads back whole.
func TestBuildEncodesAnUnbreakableSubject(t *testing.T) {
	subject := strings.Repeat("x", 1200)
	raw, err := Build(Outgoing{Subject: subject, Text: "t", MessageID: "<s@example.com>", Date: testDate})
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(raw), "\r\n") {
		if len(line) > foldAt+2 {
			t.Fatalf("a line of %d characters", len(line))
		}
	}
	if got := ParseRaw(raw).Subject; got != subject {
		t.Fatalf("subject read back as %d characters", len(got))
	}
}
