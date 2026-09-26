package gmailtest

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	stdmime "mime"
	stdmultipart "mime/multipart"
	"mime/quotedprintable"
	"slices"
	"strings"

	"github.com/mmedum/google-mail-mcp/internal/gmail"
)

// The fake reads the drafts it is sent with the standard library, not
// with internal/mime: a builder bug that the server's own parser
// mirrored would otherwise pass every test that goes through the fake.

var wordDecoder = &stdmime.WordDecoder{}

// parsedRaw is a received message as the fake stores it.
type parsedRaw struct {
	headers []gmail.MessagePartHeader
	body    *Part
	// text is what search and the snippet read: the first plain part.
	text string
}

// parseRaw reads RFC 5322 bytes into the fake's Part tree.
func parseRaw(raw []byte) (parsedRaw, error) {
	hs, body, err := splitMessage(raw)
	if err != nil {
		return parsedRaw{}, err
	}
	var top, content []gmail.MessagePartHeader
	for _, h := range hs {
		if strings.HasPrefix(strings.ToLower(h.Name), "content-") {
			content = append(content, h)
			continue
		}
		top = append(top, h)
	}
	p, err := parsePart(content, body, 0)
	if err != nil {
		return parsedRaw{}, err
	}
	out := parsedRaw{headers: top, body: p}
	out.text = firstText(p)
	return out, nil
}

// splitMessage separates headers, unfolded, from the body.
func splitMessage(raw []byte) ([]gmail.MessagePartHeader, []byte, error) {
	i := bytes.Index(raw, []byte("\r\n\r\n"))
	sep := 4
	if j := bytes.Index(raw, []byte("\n\n")); i < 0 || (j >= 0 && j < i) {
		i, sep = j, 2
	}
	if i < 0 {
		return nil, nil, errors.New("no blank line after the headers")
	}
	var hs []gmail.MessagePartHeader
	for line := range strings.SplitSeq(string(raw[:i]), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(hs) == 0 {
				return nil, nil, errors.New("a continuation line before any header")
			}
			hs[len(hs)-1].Value += line
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, nil, errors.New("a header line without a colon")
		}
		hs = append(hs, gmail.MessagePartHeader{Name: name, Value: strings.TrimSpace(value)})
	}
	return hs, raw[i+sep:], nil
}

func parsePart(hs []gmail.MessagePartHeader, body []byte, depth int) (*Part, error) {
	if depth > 10 {
		return nil, errors.New("nested too deep")
	}
	p := &Part{
		ContentType: headerValue(hs, "Content-Type"),
		Disposition: headerValue(hs, "Content-Disposition"),
		CTE:         headerValue(hs, "Content-Transfer-Encoding"),
		ContentID:   strings.Trim(headerValue(hs, "Content-ID"), "<>"),
	}
	mt, params, err := stdmime.ParseMediaType(p.ContentType)
	if p.ContentType == "" {
		mt, err = "text/plain", nil
	}
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(mt, "multipart/") {
		p.Boundary = params["boundary"]
		mr := stdmultipart.NewReader(bytes.NewReader(body), p.Boundary)
		for {
			np, err := mr.NextRawPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			b, err := io.ReadAll(np)
			if err != nil {
				return nil, err
			}
			var chs []gmail.MessagePartHeader
			for _, k := range slices.Sorted(maps.Keys(np.Header)) {
				for _, v := range np.Header[k] {
					chs = append(chs, gmail.MessagePartHeader{Name: k, Value: v})
				}
			}
			c, err := parsePart(chs, b, depth+1)
			if err != nil {
				return nil, err
			}
			p.Children = append(p.Children, c)
		}
		if len(p.Children) == 0 {
			return nil, errors.New("a multipart with no parts")
		}
		return p, nil
	}
	if p.Content, err = decodeBody(body, p.CTE); err != nil {
		return nil, err
	}
	name := params["name"]
	if _, dparams, err := stdmime.ParseMediaType(p.Disposition); err == nil && dparams["filename"] != "" {
		name = dparams["filename"]
	}
	if decoded, err := wordDecoder.DecodeHeader(name); err == nil {
		name = decoded
	}
	p.Filename = name
	return p, nil
}

func decodeBody(b []byte, cte string) ([]byte, error) {
	switch strings.ToLower(cte) {
	case "base64":
		clean := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' {
				return -1
			}
			return r
		}, string(b))
		return base64.StdEncoding.DecodeString(clean)
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
	}
	return b, nil
}

func firstText(p *Part) string {
	if len(p.Children) == 0 {
		if p.mimeType() == "text/plain" && p.Filename == "" {
			return string(p.Content)
		}
		return ""
	}
	for _, c := range p.Children {
		if t := firstText(c); t != "" {
			return t
		}
	}
	return ""
}

// decodedHeader is a header value with its encoded-words decoded.
func decodedHeader(hs []gmail.MessagePartHeader, name string) string {
	v := headerValue(hs, name)
	if d, err := wordDecoder.DecodeHeader(v); err == nil {
		return d
	}
	return v
}
