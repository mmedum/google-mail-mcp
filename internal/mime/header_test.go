package mime

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func enc(t *testing.T, e encoding.Encoding, s string) []byte {
	t.Helper()
	b, err := e.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("encode %q: %v", s, err)
	}
	return b
}

func bword(charset string, b []byte) string {
	return "=?" + charset + "?B?" + base64.StdEncoding.EncodeToString(b) + "?="
}

func TestDecodeHeader(t *testing.T) {
	utf := []byte("日本語のテキスト")
	cases := []struct {
		name, in, want string
	}{
		{"plain ascii", "Weekly report", "Weekly report"},
		{"utf-8 Q", "=?UTF-8?Q?Caf=C3=A9_menu?=", "Café menu"},
		{"utf-8 lowercase q", "=?utf-8?q?Caf=C3=A9?=", "Café"},
		{"utf-8 B", bword("UTF-8", []byte("Grüße aus Köln")), "Grüße aus Köln"},
		{"iso-8859-1 Q", "=?ISO-8859-1?Q?R=E9sum=E9?=", "Résumé"},
		{"iso-8859-15 euro", "=?ISO-8859-15?Q?Prix_=A4?=", "Prix €"},
		{"windows-1252 quotes", "=?windows-1252?Q?=93quoted=94?=", "“quoted”"},
		{"windows-1251", bword("windows-1251", enc(t, charmap.Windows1251, "Привет мир")), "Привет мир"},
		{"koi8-r", bword("KOI8-R", enc(t, charmap.KOI8R, "Отчёт")), "Отчёт"},
		{"shift_jis", bword("Shift_JIS", enc(t, japanese.ShiftJIS, "会議の案内")), "会議の案内"},
		{"gb2312", bword("GB2312", enc(t, simplifiedchinese.HZGB2312, "")) + "x", "x"},
		{"gbk", bword("GB2312", enc(t, simplifiedchinese.GBK, "季度报告")), "季度报告"},
		{"iso-2022-jp", bword("ISO-2022-JP", enc(t, japanese.ISO2022JP, "お知らせ")), "お知らせ"},
		{"iso-8859-7 greek", bword("ISO-8859-7", enc(t, charmap.ISO8859_7, "Καλημέρα")), "Καλημέρα"},
		{"words joined across whitespace", "=?UTF-8?Q?one?= =?UTF-8?Q?two?=", "onetwo"},
		{"text between words kept", "=?UTF-8?Q?one?= and =?UTF-8?Q?two?=", "one and two"},
		{"leading and trailing text", "Re: =?UTF-8?Q?Caf=C3=A9?= (2)", "Re: Café (2)"},
		{"multibyte split across words", bword("UTF-8", utf[:4]) + " " + bword("UTF-8", utf[4:]), "日本語のテキスト"},
		{"different charsets adjacent", "=?ISO-8859-1?Q?=E9?= =?UTF-8?Q?=C3=A9?=", "éé"},
		{"bad base64 kept literal", "=?UTF-8?B?!!!?=", "=?UTF-8?B?!!!?="},
		{"unpadded base64", "=?UTF-8?B?Q2Fmw6k?=", "Café"},
		{"language suffix", "=?UTF-8*en?Q?Hello?=", "Hello"},
		{"unknown charset falls back", "=?x-made-up?Q?plain?=", "plain"},
		{"newline injection flattened", "=?UTF-8?Q?line=0ABcc:_x?=", "line Bcc: x"},
		{"raw latin-1 bytes", "Caf\xe9", "Café"},
		{"raw utf-8 kept", "Café", "Café"},
		{"tabs and controls", "a\tb\x01c", "a b c"},
		{"first C1 control", "a\u0080b", "a b"},
		{"last C1 control", "a\u009fb", "a b"},
		{"no-break space kept", "a\u00a0b", "a\u00a0b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DecodeHeader(c.in); got != c.want {
				t.Fatalf("DecodeHeader(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseAddressList(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   []Address
		strict bool
	}{
		{"empty", "", nil, true},
		{"bare", "ada@example.com", []Address{{Email: "ada@example.com"}}, true},
		{"named", "Ada Quill <ada@example.com>", []Address{{"Ada Quill", "ada@example.com"}}, true},
		{"quoted comma", `"Quill, Ada" <ada@example.com>, bruno@example.org`,
			[]Address{{"Quill, Ada", "ada@example.com"}, {"", "bruno@example.org"}}, true},
		{"encoded name", "=?UTF-8?Q?Zo=C3=AB_=C3=85ngstr=C3=B6m?= <zoe@example.org>",
			[]Address{{"Zoë Ångström", "zoe@example.org"}}, true},
		{"encoded inside quotes", `"=?UTF-8?B?5bGx55SwIOiKseWtkA==?=" <hanako@example.com>`,
			[]Address{{"山田 花子", "hanako@example.com"}}, true},
		{"iso-8859-1 name", "=?ISO-8859-1?Q?S=F8ren?= <soren@example.com>",
			[]Address{{"Søren", "soren@example.com"}}, true},
		{"group", "undisclosed-recipients:;", nil, true},
		{"unquoted dot in name", "A. Quill <ada@example.com>, B <b@example.org>",
			[]Address{{"A. Quill", "ada@example.com"}, {"B", "b@example.org"}}, true},
		{"unclosed bracket", "Ada <ada@example.com, bruno@example.org",
			[]Address{{"Ada", "ada@example.com"}, {"", "bruno@example.org"}}, false},
		{"trailing garbage after address", "Ada <ada@example.com> (Ops) extra",
			[]Address{{"Ada (Ops) extra", "ada@example.com"}}, false},
		{"garbage kept as name", "not an address, ada@example.com",
			[]Address{{"not an address", ""}, {"", "ada@example.com"}}, false},
		{"semicolon separated", "ada@example.com; bruno@example.org",
			[]Address{{"", "ada@example.com"}, {"", "bruno@example.org"}}, false},
		{"lenient list keeps every angle address", "Ada <ada@example.com>, Bo <bo@example.com>, Cy <cy@example.com",
			[]Address{{"Ada", "ada@example.com"}, {"Bo", "bo@example.com"}, {"Cy", "cy@example.com"}}, false},
		{"lenient list with a comment", "Ada (ops) <ada@example.com>, Bo <bo@example.com>, Cy <cy@example.com",
			[]Address{{"Ada", "ada@example.com"}, {"Bo", "bo@example.com"}, {"Cy", "cy@example.com"}}, false},
		{"lenient quoted name unquoted", `"Ada" <ada@example.com, bo@example.org`,
			[]Address{{"Ada", "ada@example.com"}, {"", "bo@example.org"}}, false},
		{"bidi in name stripped by message", "Ada <ada@example.com>", []Address{{"Ada", "ada@example.com"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, strict := ParseAddressList(c.in)
			if strict != c.strict {
				t.Errorf("strict = %v, want %v", strict, c.strict)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d] = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestAddressString(t *testing.T) {
	for _, c := range []struct {
		a    Address
		want string
	}{
		{Address{"Ada", "ada@example.com"}, "Ada <ada@example.com>"},
		{Address{Email: "ada@example.com"}, "ada@example.com"},
		{Address{Name: "Ada"}, "Ada"},
	} {
		if got := c.a.String(); got != c.want {
			t.Errorf("%+v = %q", c.a, got)
		}
	}
}

func TestParseDate(t *testing.T) {
	want := time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)
	for _, in := range []string{
		"Mon, 2 Mar 2026 09:30:00 +0000",
		"Mon, 2 Mar 2026 09:30:00 +0000 (UTC)",
		"Mon,  2 Mar 2026 09:30:00 GMT",
		"2 Mar 2026 09:30:00 +0000",
		"2026-03-02T09:30:00Z",
		"Mon Mar 2 09:30:00 2026",
	} {
		got, ok := ParseDate(in)
		if !ok || !got.Equal(want) {
			t.Errorf("ParseDate(%q) = %v %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "yesterday"} {
		if _, ok := ParseDate(in); ok {
			t.Errorf("ParseDate(%q) parsed", in)
		}
	}
}

func TestSplitHeaders(t *testing.T) {
	hs, body := splitHeaders([]byte("Subject: a\r\n  folded\r\nX-Bad line\r\nFrom: x@example.com\r\n\r\nbody\r\n"))
	if len(hs) != 2 || hs[0].Value != "a  folded" || string(body) != "body\r\n" {
		t.Fatalf("got %+v %q", hs, body)
	}
	hs, body = splitHeaders([]byte("Subject: lf only\n\nbody"))
	if len(hs) != 1 || string(body) != "body" {
		t.Fatalf("lf: %+v %q", hs, body)
	}
	hs, body = splitHeaders([]byte("\r\nno headers"))
	if hs != nil || string(body) != "no headers" {
		t.Fatalf("blank first: %+v %q", hs, body)
	}
	hs, body = splitHeaders([]byte("\nno headers"))
	if hs != nil || string(body) != "no headers" {
		t.Fatalf("blank first lf: %+v %q", hs, body)
	}
	hs, body = splitHeaders([]byte(" folded first\r\nSubject: x\r\n\r\nbody"))
	if len(hs) != 1 || hs[0].Value != "x" || string(body) != "body" {
		t.Fatalf("continuation with nothing to continue: %+v %q", hs, body)
	}
	hs, body = splitHeaders([]byte("Subject: only"))
	if len(hs) != 1 || len(body) != 0 {
		t.Fatalf("headers only: %+v %q", hs, body)
	}
}

func TestParseMsgIDs(t *testing.T) {
	if got := parseMsgIDs("<a@x.example.com> <b@x.example.com>\r\n <c@x.example.com>"); len(got) != 3 || got[2] != "<c@x.example.com>" {
		t.Fatalf("got %v", got)
	}
	if got := parseMsgIDs("bare@x.example.com"); len(got) != 1 || got[0] != "bare@x.example.com" {
		t.Fatalf("bare: %v", got)
	}
	if got := parseMsgIDs("two words"); got != nil {
		t.Fatalf("words: %v", got)
	}
}

func TestDecodeCharset(t *testing.T) {
	cases := []struct {
		name, label string
		in          []byte
		want        string
		known       bool
	}{
		{"utf-8 invalid replaced", "utf-8", []byte("a\xffb"), "a\uFFFDb", true},
		{"ascii label with 8-bit", "us-ascii", []byte("caf\xe9"), "café", true},
		{"empty label valid utf-8", "", []byte("café"), "café", true},
		{"empty label 8-bit", "", []byte("caf\xe9"), "café", false},
		{"alias cp1252", "cp1252", []byte{0x80}, "€", true},
		{"quoted label", `"ISO-8859-2"`, []byte{0xb3}, "ł", true},
		{"unknown label utf-8 bytes", "x-unknown", []byte("ok"), "ok", false},
		{"unknown label 8-bit", "x-unknown", []byte{0xe9}, "é", false},
		{"iana only label", "IBM437", []byte{0x82}, "é", true},
	}
	for _, c := range cases {
		got, known := decodeCharset(c.in, c.label)
		if got != c.want || known != c.known {
			t.Errorf("%s: got %q %v, want %q %v", c.name, got, known, c.want, c.known)
		}
	}
	if !strings.Contains(decodeWith(japanese.ShiftJIS, []byte{0x82, 0xa0}), "あ") {
		t.Error("shift_jis")
	}
}

func TestAddressInvisiblesStripped(t *testing.T) {
	m := ParseRaw(crlf("From: Ada <a\u200Bd\u202Ea@example.com>\nTo: b\u2066ob@example.org\nSubject: s\n\nbody\n"))
	if len(m.From) != 1 || m.From[0].Email != "ada@example.com" {
		t.Fatalf("from = %+v", m.From)
	}
	if len(m.To) != 1 || m.To[0].Email != "bob@example.org" {
		t.Fatalf("to = %+v", m.To)
	}
	if m.HeaderHidden != 3 {
		t.Fatalf("HeaderHidden = %d, want 3", m.HeaderHidden)
	}
}
