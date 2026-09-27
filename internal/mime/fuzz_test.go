package mime

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/internal/gmail"
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
