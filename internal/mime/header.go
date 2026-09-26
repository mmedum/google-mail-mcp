package mime

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	netmail "net/mail"
)

// Header is one header as it appears in the message: name as written,
// value unfolded but otherwise undecoded.
type Header struct {
	Name  string
	Value string
}

// headerGet returns the first value of a header, matched without case.
func headerGet(hs []Header, name string) string {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// splitHeaders separates an entity's header block from its body. It
// accepts CRLF and bare LF, unfolds continuation lines and skips lines
// with no colon, so a malformed block yields what can be read of it.
func splitHeaders(b []byte) (hs []Header, body []byte) {
	end, bodyStart := len(b), len(b)
	if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
		end, bodyStart = i, i+4
	}
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 && i < end {
		end, bodyStart = i, i+2
	}
	// A message that opens with a blank line has no headers.
	if bytes.HasPrefix(b, []byte("\r\n")) {
		return nil, b[2:]
	}
	if bytes.HasPrefix(b, []byte("\n")) {
		return nil, b[1:]
	}
	for line := range strings.SplitSeq(string(b[:end]), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(hs) > 0 {
			hs[len(hs)-1].Value += line
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		hs = append(hs, Header{Name: name, Value: value})
	}
	for i := range hs {
		hs[i].Value = strings.TrimSpace(hs[i].Value)
	}
	return hs, b[bodyStart:]
}

var encodedWord = regexp.MustCompile(`=\?([^?\s]+)\?([BbQq])\?([^?\s]*)\?=`)

// DecodeHeader decodes RFC 2047 encoded-words in any charset the
// charset tables know, leaving a word it cannot decode as written.
//
// Adjacent encoded-words in the same charset are joined as bytes before
// decoding, because senders split a multi-byte character across two
// words often enough to matter. Unencoded 8-bit text that is not UTF-8
// is read as windows-1252. Line breaks and other control characters are
// replaced by spaces, so a decoded value cannot start a new line.
func DecodeHeader(v string) string {
	matches := encodedWord.FindAllStringSubmatchIndex(v, -1)
	if len(matches) == 0 {
		return cleanHeader(literal(v))
	}
	var out strings.Builder
	var pend []byte
	pendCharset := ""
	flush := func() {
		if pend != nil {
			s, _ := decodeCharset(pend, pendCharset)
			out.WriteString(s)
			pend, pendCharset = nil, ""
		}
	}
	last := 0
	for _, m := range matches {
		gap := v[last:m[0]]
		charset := v[m[2]:m[3]]
		raw, ok := decodeWord(v[m[4]:m[5]], v[m[6]:m[7]])
		if !ok {
			flush()
			out.WriteString(literal(gap))
			out.WriteString(literal(v[m[0]:m[1]]))
			last = m[1]
			continue
		}
		// Whitespace between two encoded-words is not part of the text
		// (RFC 2047 §6.2); anything else is.
		if pend != nil && strings.TrimSpace(gap) == "" {
			if normCharset(charset) != normCharset(pendCharset) {
				flush()
			}
		} else {
			flush()
			out.WriteString(literal(gap))
		}
		if pend == nil {
			pendCharset = charset
			pend = []byte{}
		}
		pend = append(pend, raw...)
		last = m[1]
	}
	flush()
	out.WriteString(literal(v[last:]))
	return cleanHeader(out.String())
}

func decodeWord(enc, text string) ([]byte, bool) {
	switch enc {
	case "B", "b":
		return decodeBase64Lenient(text)
	default:
		var b []byte
		for i := 0; i < len(text); i++ {
			c := text[i]
			switch {
			case c == '_':
				b = append(b, ' ')
			case c == '=' && i+2 < len(text) && isHex(text[i+1]) && isHex(text[i+2]):
				b = append(b, unhex(text[i+1])<<4|unhex(text[i+2]))
				i += 2
			default:
				b = append(b, c)
			}
		}
		return b, true
	}
}

func decodeBase64Lenient(s string) ([]byte, bool) {
	s = strings.TrimRight(s, "=")
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, true
	}
	return nil, false
}

// literal makes unencoded header text valid UTF-8.
func literal(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	out, _ := decodeCharset([]byte(s), "")
	return out
}

// cleanHeader replaces control characters with spaces and trims.
func cleanHeader(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' || r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return ' '
		}
		return r
	}, s))
}

// Address is a parsed mailbox. Email is empty when the header named
// someone the parser could not find an address for; the name is kept
// so nothing a sender wrote vanishes.
type Address struct {
	Name  string
	Email string
}

// String renders an address for reading: `Name <email>`, or either half
// alone.
func (a Address) String() string {
	switch {
	case a.Name == "":
		return a.Email
	case a.Email == "":
		return a.Name
	default:
		return a.Name + " <" + a.Email + ">"
	}
}

var (
	angleAddr = regexp.MustCompile(`<([^<>\s]+@[^<>\s]+)>`)
	bareAddr  = regexp.MustCompile(`[^\s<>",;()\[\]]+@[^\s<>",;()\[\]]+`)
)

var addrParser = netmail.AddressParser{WordDecoder: wordDecoder}

// ParseAddressList parses an address header. strict is false when the
// standard parser refused the value and the lenient splitter produced
// the result; it never fails and never panics.
func ParseAddressList(v string) (addrs []Address, strict bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, true
	}
	if list, err := addrParser.ParseList(v); err == nil {
		for _, a := range list {
			addrs = append(addrs, Address{Name: decodeName(a.Name), Email: a.Address})
		}
		return addrs, true
	}
	for _, piece := range splitAddressList(v) {
		if a, ok := lenientAddress(piece); ok {
			addrs = append(addrs, a)
		}
	}
	return addrs, false
}

// decodeName decodes what the standard parser left encoded — a word
// inside a quoted string, which RFC 2047 forbids and senders write.
func decodeName(n string) string {
	return DecodeHeader(n)
}

// splitAddressList splits on commas outside quotes, angle brackets and
// comments, and drops group syntax.
func splitAddressList(v string) []string {
	var parts []string
	var cur strings.Builder
	inQuote, angle, comment := false, 0, 0
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case inQuote:
			if c == '\\' && i+1 < len(v) {
				cur.WriteByte(c)
				i++
				c = v[i]
			} else if c == '"' {
				inQuote = false
			}
		case c == '"':
			inQuote = true
		case c == '<':
			angle++
		case c == '>' && angle > 0:
			angle--
		case c == '(':
			comment++
		case c == ')' && comment > 0:
			comment--
		case (c == ',' || c == ';') && angle > 0 && !strings.Contains(v[i:], ">"):
			// An angle bracket that never closes does not swallow the
			// rest of the list.
			angle = 0
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		case (c == ',' || c == ';') && angle == 0 && comment == 0:
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		case c == ':' && angle == 0 && comment == 0:
			// "undisclosed-recipients:" — the group name is not a mailbox.
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	return append(parts, cur.String())
}

func lenientAddress(piece string) (Address, bool) {
	piece = strings.TrimSpace(piece)
	if piece == "" {
		return Address{}, false
	}
	if a, err := addrParser.Parse(piece); err == nil {
		return Address{Name: decodeName(a.Name), Email: a.Address}, true
	}
	if m := angleAddr.FindStringSubmatchIndex(piece); m != nil {
		name := strings.TrimSpace(piece[:m[0]] + " " + piece[m[1]:])
		return Address{Name: unquote(DecodeHeader(name)), Email: piece[m[2]:m[3]]}, true
	}
	if m := bareAddr.FindStringIndex(piece); m != nil {
		name := strings.TrimSpace(piece[:m[0]] + " " + piece[m[1]:])
		name = strings.Trim(name, "()<> ")
		return Address{Name: unquote(DecodeHeader(name)), Email: piece[m[0]:m[1]]}, true
	}
	return Address{Name: unquote(DecodeHeader(piece))}, true
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(s, `\"`, `"`)), " ")
}

var dateLayouts = []string{
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04 -0700",
	"Mon, 2 Jan 06 15:04:05 -0700",
	"Mon Jan 2 15:04:05 2006",
	"Mon, 2 Jan 2006 15:04:05",
	time.RFC3339,
}

var dateComment = regexp.MustCompile(`\s*\([^)]*\)\s*$`)

// ParseDate parses a Date header, leniently. ok is false when nothing
// could be read, and the caller should fall back to internalDate.
func ParseDate(v string) (t time.Time, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if t, err := netmail.ParseDate(v); err == nil {
		return t, true
	}
	v = dateComment.ReplaceAllString(v, "")
	v = strings.Join(strings.Fields(v), " ")
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

var msgIDToken = regexp.MustCompile(`<[^<>\s]+>`)

// parseMsgIDs returns the <id> tokens of a Message-ID, In-Reply-To or
// References header, in order. A value with no brackets is returned
// whole, since some senders omit them.
func parseMsgIDs(v string) []string {
	ids := msgIDToken.FindAllString(v, -1)
	if len(ids) == 0 {
		if f := strings.Fields(v); len(f) == 1 {
			return f
		}
	}
	return ids
}
