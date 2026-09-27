package mime

import (
	"io"
	stdmime "mime"
	"sort"
	"strconv"
	"strings"
)

// wordDecoder is the standard library's RFC 2047 decoder, given every
// charset the tables know. The address parser uses it; DecodeHeader is
// this package's own, more lenient decoder.
var wordDecoder = &stdmime.WordDecoder{
	CharsetReader: func(label string, in io.Reader) (io.Reader, error) {
		b, err := io.ReadAll(in)
		if err != nil {
			return nil, err
		}
		s, _ := decodeCharset(b, label)
		return strings.NewReader(s), nil
	},
}

// ParseMediaType splits a Content-Type or Content-Disposition value into
// its lowercased type and its parameters, with lowercased names.
//
// It is lenient where the standard library is strict, because a
// refused header loses every parameter at once — the charset with the
// filename. Unquoted values may hold spaces, an unclosed quote runs to
// the end, and RFC 2231 is handled in full: continuations (name*0,
// name*1), extended values (name*=charset'lang'%XX) in any charset, and
// both together. An extended or continued value wins over a plain one
// of the same name. A plain name or filename carrying RFC 2047
// encoded-words, which RFC 2047 forbids and mailers write, is decoded.
func ParseMediaType(v string) (mediatype string, params map[string]string) {
	head, rest, _ := strings.Cut(v, ";")
	mediatype = strings.ToLower(strings.TrimSpace(head))
	params = map[string]string{}

	type seg struct {
		n   int
		ext bool
		val string
	}
	plain := map[string]string{}
	extended := map[string]string{}
	conts := map[string][]seg{}

	for _, kv := range splitParams(rest) {
		key, val := kv[0], kv[1]
		base, star, hasStar := strings.Cut(key, "*")
		if !hasStar {
			plain[key] = val
			continue
		}
		if star == "" {
			// name*=charset'lang'value
			extended[base] = decode2231(val)
			continue
		}
		numStr, ext := strings.CutSuffix(star, "*")
		n, err := strconv.Atoi(numStr)
		if err != nil || n < 0 || n > 999 {
			continue
		}
		conts[base] = append(conts[base], seg{n: n, ext: ext, val: val})
	}

	for k, v := range plain {
		switch {
		case k == "boundary":
			// Matched byte for byte against the body; never rewritten.
		case (k == "name" || k == "filename") && strings.Contains(v, "=?"):
			v = DecodeHeader(v)
		default:
			v = literal(v)
		}
		params[k] = v
	}
	for k, segs := range conts {
		sort.SliceStable(segs, func(i, j int) bool { return segs[i].n < segs[j].n })
		var raw []byte
		charset := ""
		for i, s := range segs {
			val := s.val
			if s.ext {
				if i == 0 {
					var ok bool
					if charset, val, ok = cutCharset(val); !ok {
						val = s.val
					}
				}
				raw = append(raw, percentDecode(val)...)
			} else {
				raw = append(raw, val...)
			}
		}
		params[k], _ = decodeCharset(raw, charset)
	}
	for k, v := range extended {
		params[k] = v
	}
	return mediatype, params
}

// splitParams tokenizes `; key=value; key="quoted"` leniently.
func splitParams(s string) [][2]string {
	var out [][2]string
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ';' || s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
			i++
		}
		start := i
		for i < len(s) && s[i] != '=' && s[i] != ';' {
			i++
		}
		key := strings.ToLower(strings.TrimSpace(s[start:i]))
		if i >= len(s) || s[i] == ';' {
			continue // a bare token with no value
		}
		i++ // '='
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		var val string
		val, i = readParamValue(s, i)
		if key != "" {
			out = append(out, [2]string{key, val})
		}
	}
	return out
}

// readParamValue reads a quoted or bare value starting at i and returns
// it with the index of the ';' (or end) that follows.
func readParamValue(s string, i int) (string, int) {
	if i >= len(s) || s[i] != '"' {
		start := i
		for i < len(s) && s[i] != ';' {
			i++
		}
		return strings.TrimSpace(s[start:i]), i
	}
	var val strings.Builder
	i++
	for i < len(s) && s[i] != '"' {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		val.WriteByte(s[i])
		i++
	}
	i++ // the closing quote, if there was one
	for i < len(s) && s[i] != ';' {
		i++
	}
	return val.String(), i
}

// decode2231 decodes charset'lang'%XX. A value missing the quotes is
// percent-decoded as UTF-8.
func decode2231(v string) string {
	charset, rest, ok := cutCharset(v)
	if !ok {
		rest = v
	}
	s, _ := decodeCharset(percentDecode(rest), charset)
	return s
}

func cutCharset(v string) (charset, rest string, ok bool) {
	charset, afterCharset, ok := strings.Cut(v, "'")
	if !ok {
		return "", v, false
	}
	_, rest, ok = strings.Cut(afterCharset, "'")
	if !ok {
		return "", v, false
	}
	return charset, rest, true
}

// percentDecode decodes %XX, leaving a malformed escape as written.
func percentDecode(s string) []byte {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return b
}

func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
