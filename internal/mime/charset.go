package mime

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
)

// aliases maps charset labels seen in mail that neither index knows to
// one they do.
var aliases = map[string]string{
	"x-sjis":          "shift_jis",
	"sjis":            "shift_jis",
	"cp932":           "shift_jis",
	"ms932":           "shift_jis",
	"x-gbk":           "gbk",
	"cp936":           "gbk",
	"cp949":           "euc-kr",
	"ks_c_5601-1987":  "euc-kr",
	"x-euc-jp":        "euc-jp",
	"cp1250":          "windows-1250",
	"cp1251":          "windows-1251",
	"cp1252":          "windows-1252",
	"cp1253":          "windows-1253",
	"cp1254":          "windows-1254",
	"cp1255":          "windows-1255",
	"cp1256":          "windows-1256",
	"cp1257":          "windows-1257",
	"cp1258":          "windows-1258",
	"latin1":          "iso-8859-1",
	"latin-1":         "iso-8859-1",
	"iso8859-1":       "iso-8859-1",
	"iso_8859-1":      "iso-8859-1",
	"iso-8859-8-i":    "iso-8859-8",
	"x-mac-roman":     "macintosh",
	"unicode-1-1-utf": "utf-8",
}

// normCharset lowercases a label and strips quotes, an RFC 2231
// language suffix ("utf-8*en") and whitespace.
func normCharset(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.Trim(name, `"'`)
	if i := strings.IndexByte(name, '*'); i >= 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

func isUTF8Label(name string) bool {
	switch name {
	case "utf-8", "utf8", "unicode-1-1-utf-8", "x-unicode20utf8":
		return true
	}
	return false
}

func isASCIILabel(name string) bool {
	switch name {
	case "", "us-ascii", "ascii", "ansi_x3.4-1968", "us", "iso646-us", "default":
		return true
	}
	return false
}

// lookupCharset returns the decoder for a label, or nil when the label
// is unknown. UTF-8 and ASCII labels return nil too; the caller handles
// them before asking.
func lookupCharset(name string) encoding.Encoding {
	if a, ok := aliases[name]; ok {
		name = a
	}
	// The web index reads iso-8859-1 and us-ascii as windows-1252, as
	// browsers do. That is kept: mail labeled latin-1 that uses bytes
	// 0x80–0x9f was written in windows-1252.
	if e, err := htmlindex.Get(name); err == nil {
		return e
	}
	if e, err := ianaindex.MIME.Encoding(name); err == nil && e != nil {
		return e
	}
	return nil
}

// decodeCharset turns bytes in a named charset into UTF-8. known is
// false when the label was not recognized and a fallback was used:
// UTF-8 if the bytes are valid UTF-8, windows-1252 otherwise, which
// reads every byte and is what mislabeled western mail usually is.
//
// Invalid sequences become U+FFFD; the result is always valid UTF-8.
func decodeCharset(b []byte, label string) (s string, known bool) {
	name := normCharset(label)
	switch {
	case isUTF8Label(name):
		return strings.ToValidUTF8(string(b), "\uFFFD"), true
	case isASCIILabel(name):
		if utf8.Valid(b) {
			return string(b), true
		}
		// Undeclared or ASCII-labeled 8-bit text: the label is wrong
		// either way, and windows-1252 reads every byte.
		return decodeWith(charmap.Windows1252, b), name != ""
	}
	if e := lookupCharset(name); e != nil {
		return decodeWith(e, b), true
	}
	if utf8.Valid(b) {
		return string(b), false
	}
	return decodeWith(charmap.Windows1252, b), false
}

func decodeWith(e encoding.Encoding, b []byte) string {
	out, err := e.NewDecoder().Bytes(b)
	if err != nil {
		// A decoder that refuses the input still leaves the reader with
		// something: the bytes as windows-1252, which cannot fail.
		out, _ = charmap.Windows1252.NewDecoder().Bytes(b)
	}
	return strings.ToValidUTF8(string(out), "\uFFFD")
}
