package mime

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func ptrTo(s string) *string { return &s }

func build(t *testing.T, o Outgoing) []byte {
	t.Helper()
	if o.MessageID == "" {
		o.MessageID = "<e@example.com>"
	}
	o.Date = testDate
	raw, err := Build(o)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var (
	attA = OutAttachment{Filename: "a.txt", MediaType: "text/plain", Content: []byte("first file")}
	attB = OutAttachment{Filename: "b.pdf", MediaType: "application/pdf", Content: []byte("%PDF second")}
)

// entityBytes is how one attachment appears in a built message, so a
// test can find it byte for byte after an edit.
func entityBytes(t *testing.T, a OutAttachment) []byte {
	t.Helper()
	e, err := attachmentEntity(a)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	e.write(&b)
	return b.Bytes()
}

func TestEditRawWithNothingIsTheSameBytes(t *testing.T) {
	raw := build(t, Outgoing{To: []Address{{Email: "a@example.com"}}, Subject: "s", Text: "t", HTML: "<p>t</p>",
		Attachments: []OutAttachment{attA, attB}})
	out, _, err := EditRaw(raw, Edit{})
	if err != nil || !bytes.Equal(out, raw) {
		t.Fatalf("changed: %v\n%s", err, out)
	}
}

func TestEditRawHeaders(t *testing.T) {
	raw := build(t, Outgoing{
		To: []Address{{Email: "a@example.com"}}, Cc: []Address{{Email: "c@example.com"}},
		Subject: "old", Text: "t", InReplyTo: "<p@example.com>", References: []string{"<p@example.com>"},
	})
	subject, _ := FormatText("new — Neu")
	out, _, err := EditRaw(raw, Edit{Headers: []SetHeader{
		{Name: "Subject", Value: subject}, {Name: "Cc", Value: ""}, {Name: "Bcc", Value: "b@example.com"},
		{Name: "To", Value: "d@example.com"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	m := ParseRaw(out)
	if m.Subject != "new — Neu" || len(m.Cc) != 0 || len(m.Bcc) != 1 || len(m.To) != 1 || m.To[0].Email != "d@example.com" {
		t.Fatalf("subject %q cc %v bcc %v to %v", m.Subject, m.Cc, m.Bcc, m.To)
	}
	if strings.Join(m.InReplyTo, "") != "<p@example.com>" || m.MessageID != "<e@example.com>" {
		t.Fatalf("threading headers lost: %q %q", m.InReplyTo, m.MessageID)
	}
	hs, _ := splitRawHeaders(out)
	var names []string
	for _, h := range hs {
		names = append(names, h.name)
	}
	if got := strings.Join(names, " "); got != "To Subject Date Message-ID In-Reply-To References Bcc MIME-Version Content-Type Content-Transfer-Encoding" {
		t.Fatalf("header order %s", got)
	}
	for _, bad := range []SetHeader{{Name: "Content-Type", Value: "text/html"}, {Name: "Subject", Value: "a\r\nBcc: x"}} {
		if _, _, err := EditRaw(raw, Edit{Headers: []SetHeader{bad}}); err == nil {
			t.Errorf("set %s", bad.Name)
		}
	}
}

func TestEditRawBodies(t *testing.T) {
	plain := build(t, Outgoing{To: []Address{{Email: "a@example.com"}}, Text: "old text", Attachments: []OutAttachment{attA}})
	both := build(t, Outgoing{To: []Address{{Email: "a@example.com"}}, Text: "old", HTML: "<p><b>old</b></p>", Attachments: []OutAttachment{attA}})
	bare := build(t, Outgoing{To: []Address{{Email: "a@example.com"}}, Text: "old bare"})

	cases := []struct {
		name       string
		raw        []byte
		text, html *string
		err        error
		plain      string
		htm        string
	}{
		{"new text on a plain draft", plain, ptrTo("new\ntext"), nil, nil, "new\ntext", ""},
		{"text alone on a draft with HTML", both, ptrTo("x"), nil, ErrBothBodies, "", ""},
		{"html alone on a draft with both", both, nil, ptrTo("<b>x</b>"), ErrBothBodies, "", ""},
		{"both on a draft with both", both, ptrTo("new"), ptrTo("<p>new</p>"), nil, "new", "<p>new</p>"},
		{"both on a plain draft", plain, ptrTo("new"), ptrTo("<p>new</p>"), nil, "new", "<p>new</p>"},
		{"html beside a kept plain part", plain, nil, ptrTo("<i>added</i>"), nil, "old text", "<i>added</i>"},
		{"html beside the whole message", bare, nil, ptrTo("<i>added</i>"), nil, "old bare", "<i>added</i>"},
		{"text on a single-part message", bare, ptrTo("replaced"), nil, nil, "replaced", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := EditRaw(tt.raw, Edit{Text: tt.text, HTML: tt.html})
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var gotPlain, gotHTML string
			for _, l := range leaves(out) {
				switch {
				case l.mediaType == "text/plain" && l.declaredName() == "":
					gotPlain = string(l.data)
				case l.mediaType == "text/html":
					gotHTML = string(l.data)
				}
			}
			if gotPlain != tt.plain || gotHTML != tt.htm {
				t.Fatalf("plain %q html %q", gotPlain, gotHTML)
			}
			if bytes.Contains(tt.raw, entityBytes(t, attA)) && !bytes.Contains(out, entityBytes(t, attA)) {
				t.Fatal("the attachment was not carried over byte for byte")
			}
			m := ParseRaw(out)
			if m.MessageID != "<e@example.com>" || len(m.To) != 1 {
				t.Fatalf("message headers lost: %q %v", m.MessageID, m.To)
			}
			if n := strings.Count(string(out), "MIME-Version"); n != 1 {
				t.Fatalf("%d MIME-Version headers", n)
			}
		})
	}
}

func TestEditRawAttachments(t *testing.T) {
	raw := build(t, Outgoing{To: []Address{{Email: "a@example.com"}}, Text: "t", HTML: "<p>t</p>",
		Attachments: []OutAttachment{attA, attB}})
	m := ParseRaw(raw)
	if len(m.Attachments) != 2 || m.Attachments[0].PartID != "1" || m.Attachments[1].PartID != "2" {
		t.Fatalf("attachments %+v", m.Attachments)
	}
	added := OutAttachment{Filename: "Отчёт.pdf", MediaType: "application/pdf", Content: []byte("third")}
	out, _, err := EditRaw(raw, Edit{Remove: []string{"1"}, Add: []OutAttachment{added}})
	if err != nil {
		t.Fatal(err)
	}
	got := ParseRaw(out)
	var names []string
	for _, a := range got.Attachments {
		names = append(names, a.DeclaredName)
	}
	if strings.Join(names, ",") != "b.pdf,Отчёт.pdf" {
		t.Fatalf("attachments %v", names)
	}
	if !bytes.Contains(out, entityBytes(t, attB)) {
		t.Fatal("the kept attachment changed")
	}
	if got.Body.Source != SourcePlain || got.Body.Text != "t" {
		t.Fatalf("body %q", got.Body.Text)
	}

	for _, id := range []string{"0", "0.0", "", "9"} {
		if _, _, err := EditRaw(raw, Edit{Remove: []string{id}}); !errors.Is(err, ErrNotAttachment) {
			t.Errorf("remove %q: %v", id, err)
		}
	}
}

func TestEditRawAddsToASinglePart(t *testing.T) {
	raw := build(t, Outgoing{To: []Address{{Email: "a@example.com"}}, Subject: "s", Text: "only text"})
	out, _, err := EditRaw(raw, Edit{Add: []OutAttachment{attA}})
	if err != nil {
		t.Fatal(err)
	}
	m := ParseRaw(out)
	if m.ContentType != "multipart/mixed" || m.Subject != "s" || m.Body.Text != "only text" || len(m.Attachments) != 1 {
		t.Fatalf("%s %q %q %d", m.ContentType, m.Subject, m.Body.Text, len(m.Attachments))
	}
	hs, _ := splitRawHeaders(out)
	if rawGet(hs, "Content-Transfer-Encoding") != "" {
		t.Fatal("the old part's content headers stayed on the message")
	}
}

func TestEditRawGivesABodyToAMessageWithout(t *testing.T) {
	raw := []byte("To: a@example.com\r\nMIME-Version: 1.0\r\nContent-Type: application/pdf; name=x.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERg==\r\n")
	out, _, err := EditRaw(raw, Edit{Text: ptrTo("see attached")})
	if err != nil {
		t.Fatal(err)
	}
	// A message with no text has no shape to keep, so it takes the HTML
	// version beside its new text.
	m, ls := ParseRaw(out), leaves(out)
	if m.Body.Text != "see attached" || len(m.Attachments) != 1 || len(ls) != 3 ||
		string(ls[1].data) != "<p>see attached</p>\n" || string(ls[2].data) != "%PDF" {
		t.Fatalf("%q %+v, %d parts", m.Body.Text, m.Attachments, len(ls))
	}
	if _, _, err := EditRaw(raw, Edit{HTML: ptrTo("<p>x</p>")}); !errors.Is(err, ErrNoPlainBody) {
		t.Fatalf("html alone: %v", err)
	}
	html := []byte("To: a@example.com\r\nContent-Type: text/html\r\n\r\n<p>x</p>")
	if _, _, err := EditRaw(html, Edit{Text: ptrTo("x")}); !errors.Is(err, ErrHTMLOnly) {
		t.Fatalf("text on HTML only: %v", err)
	}
}

func TestEditRawRefusesEmptyingAMultipart(t *testing.T) {
	raw := []byte("To: a@example.com\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n" +
		"Content-Type: application/pdf; name=x.pdf\r\n\r\nx\r\n--b--\r\n")
	if _, _, err := EditRaw(raw, Edit{Remove: []string{"0"}}); err == nil || errors.Is(err, ErrNotAttachment) {
		t.Fatalf("err %v", err)
	}
}

func FuzzEditRaw(f *testing.F) {
	f.Add([]byte(placeholderAlt), "new", "<p>new</p>", "1")
	f.Add([]byte(nested), "x", "", "0.1")
	f.Add([]byte("Content-Type: multipart/mixed; boundary=b\n\n--b\n\n--b--"), "", "h", "0")
	f.Fuzz(func(t *testing.T, raw []byte, text, html, remove string) {
		e := Edit{Remove: []string{remove}, Add: []OutAttachment{attA}}
		if text != "" {
			e.Text = &text
		}
		if html != "" {
			e.HTML = &html
		}
		out, _, err := EditRaw(raw, e)
		if err != nil {
			return
		}
		checkMessage(t, ParseRaw(out))
	})
}

// An edit finds the body where ParseRaw reads it: a multipart at the
// nesting limit is text to both.
func TestEditRawAgreesWithParseRawAtTheNestingLimit(t *testing.T) {
	out, _, err := EditRaw([]byte(nestedToLimit(unsplitPart)), Edit{Text: ptrTo("new")})
	if err != nil {
		t.Fatal(err)
	}
	if got := ParseRaw(out).Body.Text; got != "new" {
		t.Fatalf("body after the edit %q, want %q", got, "new")
	}
}

// A message with more parts than readEntity reads is refused, since
// rewriting a multipart from the parts read would drop the rest.
func TestEditRawRefusesATruncatedTree(t *testing.T) {
	var b strings.Builder
	b.WriteString("To: a@example.com\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n")
	for range maxParts + 5 {
		b.WriteString("--b\r\nContent-Type: application/pdf; name=x.pdf\r\n\r\nx\r\n")
	}
	b.WriteString("--b--\r\n")
	if _, _, err := EditRaw([]byte(b.String()), Edit{Text: ptrTo("x")}); !errors.Is(err, ErrTooManyParts) {
		t.Fatalf("err %v", err)
	}
}

// Text alone keeps the HTML version made from the text in step: made
// again, dropped, or added as HTMLMode says, and HTML written another way
// is never replaced or lost.
func TestEditRawKeepsTheHTMLMadeFromTheText(t *testing.T) {
	plain := build(t, Outgoing{Text: "Old text.\n"})
	made := build(t, Outgoing{Text: "Old text.\n", HTML: "<p>Old text.</p>\n"})
	madeB64 := []byte("Content-Type: multipart/alternative; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Old text.\r\n--b\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("<p>Old text.</p>\r\n")) + "\r\n--b--\r\n")
	madeSpaces := build(t, Outgoing{Text: "Old text.   \n", HTML: "<p>Old text.</p>\n"})
	hand := build(t, Outgoing{Text: "Old text.\n", HTML: "<p>Old <b>text</b>.</p>"})
	unlinked := build(t, Outgoing{Text: "See https://example.com/x.\n", HTML: "<p>See https://example.com/x.</p>\n"})
	rawTag := build(t, Outgoing{Text: "Use a <b> tag.\n", HTML: "<p>Use a <b> tag.</p>\n"})
	innerBreak := build(t, Outgoing{Text: "ab\n", HTML: "<p>a\nb</p>\n"})
	empty := build(t, Outgoing{Text: "\n"})
	spoofed := build(t, Outgoing{Text: "See https://example.com/x.\n",
		HTML: `<p>See <a href="https://evil.example/">https://example.com/x</a>.</p>`})
	htmlOnly := []byte("Content-Type: text/html; charset=utf-8\r\n\r\n<p>Old text.</p>\r\n")
	mixedPair := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Old text.\r\n--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Old text.</p>\r\n--b--\r\n")
	attached := build(t, Outgoing{Text: "Old text.\n", HTML: "<p>Old text.</p>\n",
		Attachments: []OutAttachment{{Filename: "a.txt", Content: []byte("file")}}})
	withInvite := []byte("Content-Type: multipart/alternative; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Old text.\r\n--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Old text.</p>\r\n--b\r\n" +
		"Content-Type: text/calendar; method=REQUEST\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n--b--\r\n")

	cases := []struct {
		name            string
		raw             []byte
		mode            HTMLMode
		text            string
		err             error
		plain, html     string
		changed, attach bool
	}{
		{"a plain draft stays plain", plain, KeepShape, "New.", nil, "New.", "(none)", false, false},
		{"the made version is made again", made, KeepShape, "New.", nil, "New.", "<p>New.</p>\n", true, false},
		{"made, as Gmail re-encodes it", madeB64, KeepShape, "New.", nil, "New.", "<p>New.</p>\n", true, false},
		{"made from text with trailing spaces", madeSpaces, KeepShape, "New.", nil, "New.", "<p>New.</p>\n", true, false},
		{"emptied text drops the made version", made, KeepShape, "", nil, "", "(none)", true, false},
		{"HTML written by hand is not replaced", hand, KeepShape, "New.", ErrBothBodies, "", "", false, false},
		{"made under other link rules", unlinked, KeepShape, "New.", nil, "New.", "<p>New.</p>\n", true, false},
		{"a link whose text is not its target", spoofed, KeepShape, "New.", ErrBothBodies, "", "", false, false},
		{"a raw tag that reads as the text", rawTag, KeepShape, "New.", ErrBothBodies, "", "", false, false},
		{"a line break inside a line", innerBreak, KeepShape, "New.", ErrBothBodies, "", "", false, false},
		{"a draft with no text takes the made version", empty, KeepShape, "New.", nil, "New.", "<p>New.</p>\n", true, false},
		{"HTML alone is not replaced", htmlOnly, KeepShape, "New.", ErrHTMLOnly, "", "", false, false},
		{"plain only drops the made version", made, PlainOnly, "New.", nil, "New.", "(none)", true, false},
		{"plain only on a plain draft", plain, PlainOnly, "New.", nil, "New.", "(none)", false, false},
		{"plain only keeps the attachments", attached, PlainOnly, "New.", nil, "New.", "(none)", true, true},
		{"plain only will not lose a third alternative", withInvite, PlainOnly, "New.", ErrTextLayout, "", "", false, false},
		{"plain only will not lose HTML written by hand", hand, PlainOnly, "New.", ErrDropsHTML, "", "", false, false},
		{"plain only needs the pair in one alternative", mixedPair, PlainOnly, "New.", ErrTextLayout, "", "", false, false},
		{"with HTML adds the made version", plain, WithHTML, "New.", nil, "New.", "<p>New.</p>\n", true, false},
		{"with HTML keeps HTML written by hand", hand, WithHTML, "New.", ErrBothBodies, "", "", false, false},
	}
	for _, c := range cases {
		out, changed, err := EditRaw(c.raw, Edit{Text: ptrTo(c.text), HTMLMode: c.mode})
		if c.err != nil || err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("%s: err %v, want %v", c.name, err, c.err)
			}
			continue
		}
		gotPlain, gotHTML := "", "(none)"
		attach := false
		for _, l := range leaves(out) {
			switch {
			case l.declaredName() != "":
				attach = true
			case l.mediaType == "text/plain":
				gotPlain = string(l.data)
			case l.mediaType == "text/html":
				gotHTML = string(l.data)
			}
		}
		if gotPlain != c.plain || gotHTML != c.html || changed != c.changed || attach != c.attach {
			t.Errorf("%s: plain %q html %q changed %v attachment %v; want %q %q %v %v",
				c.name, gotPlain, gotHTML, changed, attach, c.plain, c.html, c.changed, c.attach)
		}
	}
}

// FuzzMadeHTMLIsRecognized holds that whatever text HTMLFromText is
// given, the HTML it makes reads back as made from that text, so a body
// update never takes the server's own HTML for a person's.
func FuzzMadeHTMLIsRecognized(f *testing.F) {
	for _, s := range []string{"Hi Ada,\n\nSee https://example.com/(x).\n\tQ1  100\n", "a\u00a0b", "  \n\n<b>&amp;</b>"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if !utf8.ValidString(text) {
			t.Skip()
		}
		got, ok := madeText(HTMLFromText(text))
		if !ok || got != madeNormal(text) {
			t.Fatalf("%q made %q, read back as %q (ok %v), want %q", text, HTMLFromText(text), got, ok, madeNormal(text))
		}
	})
}
