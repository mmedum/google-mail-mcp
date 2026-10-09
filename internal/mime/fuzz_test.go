package mime

import (
	netmail "net/mail"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
)

// checkMessage holds the invariants every parse keeps, whatever the
// input: text is valid UTF-8, spans lie inside the text, and attachment
// names are safe base names.
func checkMessage(t *testing.T, m *Message) {
	t.Helper()
	for _, s := range []string{m.Subject, m.Body.Text} {
		if !utf8.ValidString(s) {
			t.Fatalf("invalid UTF-8: %q", s)
		}
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		t.Fatalf("subject carries a line break: %q", m.Subject)
	}
	for _, s := range m.Body.Spans {
		if s.Start < 0 || s.End > len(m.Body.Text) || s.Start > s.End {
			t.Fatalf("span %+v outside %d bytes", s, len(m.Body.Text))
		}
	}
	for _, a := range m.Attachments {
		if a.Filename == "" || strings.ContainsAny(a.Filename, "/\\\x00") || strings.HasPrefix(a.Filename, ".") {
			t.Fatalf("unsafe filename %q", a.Filename)
		}
	}
}

func FuzzParseRaw(f *testing.F) {
	for _, s := range []string{
		placeholderAlt, nested,
		"Subject: =?UTF-8?B?w6k=?=\n\nbody",
		"Content-Type: multipart/mixed; boundary=b\n\n--b\n\n--b--",
		"Content-Type: text/html\n\n<a href=x>y</a><div style=display:none>z",
		"Content-Type: text/plain; format=flowed\n\n> a \n> b\n-- \nsig",
		"From: <<<@>>>, \"\n\n",
		"Content-Disposition: attachment; filename*0*=x'y'%; filename*1=\"",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		checkMessage(t, ParseRaw(b))
	})
}

func FuzzParsePayload(f *testing.F) {
	f.Add("text/html", "text/html; charset=shift_jis", "PGI-eDwvYj4", "attachment; filename*=utf-8''%", "")
	f.Add("multipart/alternative", "", "", "", "ATT")
	f.Fuzz(func(t *testing.T, mt, ct, data, disp, attID string) {
		p := &gmail.MessagePart{
			MimeType: "multipart/mixed",
			Parts: []gmail.MessagePart{{
				PartID:   "0",
				MimeType: mt,
				Headers:  []gmail.MessagePartHeader{{Name: "Content-Type", Value: ct}, {Name: "Content-Disposition", Value: disp}},
				Body:     &gmail.MessagePartBody{Data: data, AttachmentID: attID},
			}},
		}
		m, err := ParsePayload(p, map[string][]byte{"0": []byte(data)})
		if err != nil {
			t.Fatal(err)
		}
		checkMessage(t, m)
	})
}

// A rendered mailbox reads back with net/mail as the same mailbox, and
// with this package's own parser, which reads a draft's recipients, as
// the same address. It holds for any name, and for every address
// net/mail returns, a quoted local part included.
func FuzzAddressStringReadsBack(f *testing.F) {
	for _, c := range [][2]string{
		{"Quill, Ada", "ada@example.com"},
		{"Boss <boss@example.org>", "Ada <ada@example.com>"},
		{`a"b\c`, `"john doe"@example.com`},
		{"=?utf-8?q?Boss?=", `"a\"b"@example.com`},
		{"Zoë\tÅngström\x00", "x@[192.0.2.1]"},
		{" .Ada. ", "=?utf-8?q?Ada?= <ada@example.com>, b@example.org"},
		{"\xff", ""},
	} {
		f.Add(c[0], c[1])
	}
	f.Fuzz(func(t *testing.T, name, header string) {
		mailboxes := []Address{{Name: name, Email: "ada@example.com"}}
		if list, err := netmail.ParseAddressList(header); err == nil {
			for _, a := range list {
				mailboxes = append(mailboxes, Address{Name: name, Email: a.Address}, Address{Email: a.Address})
			}
		}
		for _, a := range mailboxes {
			s := a.String()
			got, err := netmail.ParseAddress(s)
			if err != nil {
				t.Fatalf("%+v renders as %q, which net/mail refuses: %v", a, s, err)
			}
			if got.Address != a.Email {
				t.Fatalf("%+v renders as %q, which net/mail reads as %q", a, s, got.Address)
			}
			if utf8.ValidString(a.Name) && !HasControl(a.Name) && got.Name != a.Name {
				t.Fatalf("%+v renders as %q, which net/mail reads with the name %q", a, s, got.Name)
			}
			if own, strict := ParseAddressList(s); !strict || len(own) != 1 || own[0].Email != a.Email {
				t.Fatalf("%+v renders as %q, which this package reads as %+v (strict %v)", a, s, own, strict)
			}
		}
	})
}

// FuzzLenientAddressIsOutsideQuotes holds that an address read from a
// list the standard parser refused was written outside every quoted
// string and comment, where a sender would write one to pass it off as
// the sender. Space is ignored, since net/mail reads "a @ b" as a@b, and
// a local part net/mail unquotes, "a"@b, is held by its domain.
func FuzzLenientAddressIsOutsideQuotes(f *testing.F) {
	for _, v := range []string{
		`"Boss <boss@bank.example>" <attacker@evil.example> x`,
		"(boss@bank.example) attacker@evil.example;;",
		`"boss@bank.example" <attacker@evil.example`,
		"(x <a, boss@bank.example) attacker@evil.example x",
		`"a\" <boss@bank.example>" <attacker@evil.example> x`,
		`Ada <ada@example.com, "Bo <bo@example.com>" bo@example.org`,
		`"x"@evil.example (boss@bank.example) y`,
	} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v string) {
		list, strict := ParseAddressList(v)
		if strict {
			return
		}
		squash := func(s string) string { return strings.Join(strings.Fields(s), "") }
		open := squash(outsideQuotes(v))
		for _, a := range list {
			if a.Email == "" {
				continue
			}
			domain := squash(a.Email[max(strings.LastIndexByte(a.Email, '@'), 0):])
			unquoted := strings.Contains(open, domain) && strings.Contains(squash(v), `"`+domain)
			if !strings.Contains(open, squash(a.Email)) && !unquoted {
				t.Fatalf("%q shows %q, written inside a quoted string or a comment", v, a.Email)
			}
		}
	})
}

func FuzzDecodeHeader(f *testing.F) {
	// The last two are earlier fuzz failures, kept as seeds.
	for _, s := range []string{"=?UTF-8?Q?a?=", "=?x?B?////?= =?x?B?AA==?=", "\xff\xfe", "=?utf-8?q?=0A?=", ". .00000000000", ";0=\x9b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := DecodeHeader(s)
		if !utf8.ValidString(out) || strings.ContainsAny(out, "\r\n") {
			t.Fatalf("DecodeHeader(%q) = %q", s, out)
		}
		addrs, _ := ParseAddressList(s)
		for _, a := range addrs {
			if !utf8.ValidString(a.Name) {
				t.Fatalf("name %q", a.Name)
			}
		}
		_, params := ParseMediaType(s)
		for k, v := range params {
			if k != "boundary" && !utf8.ValidString(v) {
				t.Fatalf("param %q", v)
			}
		}
		if n := SafeBaseName(s); strings.ContainsAny(n, "/\\") || strings.HasPrefix(n, ".") || len(n) > maxNameBytes {
			t.Fatalf("SafeBaseName(%q) = %q", s, n)
		}
	})
}

func FuzzHTMLToText(f *testing.F) {
	for _, s := range []string{
		"<p>a</p>", "<style>.x{display:none}</style><div class=x>h</div>",
		"<a href='https://a.example'>b.example.com</a>", "<blockquote><pre>x\ny</pre></blockquote>",
		"<ol><li><ul><li>x</ul></ol>", "<font color=#fff><td bgcolor=white>x",
		"\x84", // an earlier fuzz failure: invalid UTF-8
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h := HTMLToText([]byte(s))
		if !utf8.ValidString(h.Text) {
			t.Fatalf("invalid text %q", h.Text)
		}
		if hasInvisible(h.Text) {
			// Joiners between non-ASCII letters are allowed through.
			for _, r := range h.Text {
				if isInvisible(r) && r != 0x200C && r != 0x200D {
					t.Fatalf("invisible %U survived in %q", r, h.Text)
				}
			}
		}
		for _, sp := range FindSpans(h.Text) {
			if sp.End > len(h.Text) {
				t.Fatalf("span %+v", sp)
			}
		}
	})
}
