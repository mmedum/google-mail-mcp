package mime

import (
	"strings"
	"testing"
)

func TestParseMediaType(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		mt    string
		key   string
		value string
	}{
		{"simple", `text/plain; charset=UTF-8`, "text/plain", "charset", "UTF-8"},
		{"quoted", `text/plain; charset="iso-8859-1"`, "text/plain", "charset", "iso-8859-1"},
		{"case", `Text/HTML; CharSet=utf-8`, "text/html", "charset", "utf-8"},
		{"escaped quote", `attachment; filename="a \"b\".txt"`, "attachment", "filename", `a "b".txt`},
		{"unquoted with spaces", `attachment; filename=annual report.pdf`, "attachment", "filename", "annual report.pdf"},
		{"unclosed quote", `attachment; filename="open.pdf`, "attachment", "filename", "open.pdf"},
		{"bare token ignored", `attachment; weird; filename=a.txt`, "attachment", "filename", "a.txt"},
		{"rfc2231 utf-8", `attachment; filename*=UTF-8''%E5%A0%B1%E5%91%8A.pdf`, "attachment", "filename", "報告.pdf"},
		{"rfc2231 latin-1 with language", `attachment; filename*=iso-8859-1'fr'r%E9sum%E9.pdf`, "attachment", "filename", "résumé.pdf"},
		{"rfc2231 no charset quotes", `attachment; filename*=caf%C3%A9.txt`, "attachment", "filename", "café.txt"},
		{"rfc2231 continuations", `attachment; filename*0="long "; filename*1="name.txt"`, "attachment", "filename", "long name.txt"},
		{"rfc2231 extended continuations out of order",
			`attachment; filename*1*=%E8%A1%A8.xlsx; filename*0*=UTF-8''%E5%B9%B4%E5%BA%A6`, "attachment", "filename", "年度表.xlsx"},
		{"rfc2231 mixed continuation", `attachment; filename*0*=utf-8''%C3%A9t%C3%A9; filename*1=".txt"`, "attachment", "filename", "été.txt"},
		{"rfc2231 multibyte split across segments",
			`attachment; filename*0*=UTF-8''%E6%97; filename*1*=%A5.txt`, "attachment", "filename", "日.txt"},
		{"extended beats plain", `attachment; filename="fallback.txt"; filename*=UTF-8''real.txt`, "attachment", "filename", "real.txt"},
		{"rfc2047 in quoted name", `application/pdf; name="=?UTF-8?B?0J7RgtGH0ZHRgi5wZGY=?="`, "application/pdf", "name", "Отчёт.pdf"},
		{"rfc2047 not applied to boundary", `multipart/mixed; boundary="=?x?="`, "multipart/mixed", "boundary", "=?x?="},
		{"bad percent kept", `attachment; filename*=UTF-8''100%zz.txt`, "attachment", "filename", "100%zz.txt"},
		{"bad continuation index ignored", `attachment; filename*x=a; filename=b`, "attachment", "filename", "b"},
		{"empty", ``, "", "charset", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mt, params := ParseMediaType(c.in)
			if mt != c.mt || params[c.key] != c.value {
				t.Fatalf("ParseMediaType(%q) = %q %q=%q, want %q %q", c.in, mt, c.key, params[c.key], c.mt, c.value)
			}
		})
	}
}

func TestSafeBaseName(t *testing.T) {
	long := strings.Repeat("ä", 150) + ".pdf"
	cases := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\x\evil.exe`, "evil.exe"},
		{".bashrc", "bashrc"},
		{"...", ""},
		{"", ""},
		{"a/b/", ""},
		{"trailing dots. . ", "trailing dots"},
		{"invoice\u202Etxt.exe", "invoicetxt.exe"},
		{"zero\u200Bwidth.txt", "zerowidth.txt"},
		{"ctl\x00\x1fchars.txt", "ctlchars.txt"},
		{`what?<>|"*.txt`, "what______.txt"},
		{"CON.txt", "_CON.txt"},
		{"nul", "_nul"},
		{"报告.docx", "报告.docx"},
	}
	for _, c := range cases {
		if got := SafeBaseName(c.in); got != c.want {
			t.Errorf("SafeBaseName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	got := SafeBaseName(long)
	if len(got) > maxNameBytes || !strings.HasSuffix(got, ".pdf") || !strings.HasPrefix(got, "ää") {
		t.Errorf("long name: %d bytes %q", len(got), got)
	}
	weirdExt := strings.Repeat("a", 190) + "." + strings.Repeat("b", 30)
	if got := SafeBaseName(weirdExt); len(got) > maxNameBytes {
		t.Errorf("long extension: %d", len(got))
	}
}

func TestStripInvisible(t *testing.T) {
	cases := []struct {
		in, want string
		n        int
	}{
		{"plain", "plain", 0},
		{"a\u200Bb\u200Cc\u200Dd\u200Ee\u200Ff", "abcdef", 5},
		{"x\u2060y\uFEFFz", "xyz", 2},
		{"\u202Eevil\u202C", "evil", 2},
		{"\u2066iso\u2069", "iso", 2},
		{"tag\U000E0041\U000E0042", "tag", 2},
		{"soft\u00ADhyphen", "softhyphen", 1},
		// Joiners inside an emoji sequence or a non-Latin word stay.
		{"👩\u200D💻", "👩\u200D💻", 0},
		{"می\u200Cخواهم", "می\u200Cخواهم", 0},
		{"a\u200D\u200Bé", "aé", 2},
	}
	for _, c := range cases {
		got, n := StripInvisible(c.in)
		if got != c.want || n != c.n {
			t.Errorf("StripInvisible(%q) = %q %d, want %q %d", c.in, got, n, c.want, c.n)
		}
	}
	if got := escapeInvisible("a\u202Eb"); got != `a\u{202E}b` {
		t.Errorf("escapeInvisible = %q", got)
	}
}

func TestFallbackName(t *testing.T) {
	if fallbackName("application/pdf") != "attachment.pdf" || fallbackName("x/unknown") != "attachment.bin" {
		t.Fatal("fallback names")
	}
}
