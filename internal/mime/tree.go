package mime

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"github.com/mmedum/google-mail-mcp/internal/gmail"
)

// node is one MIME entity, from either source: a Gmail payload or raw
// bytes. Everything downstream reads nodes, so the two sources cannot
// disagree about what a message says.
type node struct {
	partID      string
	headers     []Header
	mediaType   string
	params      map[string]string
	disposition string
	dispParams  map[string]string
	// gmailName is the payload's own filename field, which Google has
	// already decoded; used when the headers yield no name.
	gmailName    string
	data         []byte
	hasData      bool
	attachmentID string
	size         int
	children     []*node
}

// Limits a hostile message cannot exceed.
const (
	maxNesting = 32
	maxParts   = 1000
)

func (n *node) setContentHeaders() {
	ct := headerGet(n.headers, "Content-Type")
	mt, params := ParseMediaType(ct)
	if n.mediaType == "" {
		n.mediaType = mt
	}
	n.params = params
	if n.mediaType == "" || !strings.Contains(n.mediaType, "/") {
		n.mediaType = "text/plain"
	}
	n.disposition, n.dispParams = ParseMediaType(headerGet(n.headers, "Content-Disposition"))
}

func (n *node) isMultipart() bool { return strings.HasPrefix(n.mediaType, "multipart/") }

// fromPayload builds nodes from a Gmail payload. Body data there is
// base64url of the part with its transfer encoding already removed.
// fetched supplies, by part id, the bytes of parts Gmail stored behind
// an attachment id.
func fromPayload(p *gmail.MessagePart, fetched map[string][]byte, depth int, count *int) *node {
	*count++
	n := &node{partID: p.PartID, gmailName: p.Filename}
	for _, h := range p.Headers {
		n.headers = append(n.headers, Header{Name: h.Name, Value: h.Value})
	}
	n.mediaType = strings.ToLower(strings.TrimSpace(p.MimeType))
	n.setContentHeaders()
	if p.Body != nil {
		n.size = int(p.Body.Size)
		n.attachmentID = p.Body.AttachmentID
		if p.Body.Data != "" {
			n.data, n.hasData = decodeBase64URL(p.Body.Data), true
		}
	}
	if b, ok := fetched[p.PartID]; ok && !n.hasData {
		n.data, n.hasData = b, true
	}
	if depth >= maxNesting {
		return n
	}
	for i := range p.Parts {
		if *count >= maxParts {
			break
		}
		n.children = append(n.children, fromPayload(&p.Parts[i], fetched, depth+1, count))
	}
	return n
}

// DecodeBase64URL decodes Gmail's base64url, padded or not, and falls
// back to the standard alphabet. It fails only when neither alphabet
// reads the input.
func DecodeBase64URL(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, ok := decodeBase64Lenient(s); ok {
		return b, nil
	}
	return nil, errors.New("not base64url")
}

// decodeBase64URL is DecodeBase64URL with what cannot be decoded dropped.
func decodeBase64URL(s string) []byte {
	b, _ := DecodeBase64URL(s)
	return b
}

// fromRaw builds nodes from RFC 5322 bytes, removing each leaf's
// transfer encoding.
func fromRaw(b []byte, partID string, depth int, count *int) *node {
	*count++
	hs, body := splitHeaders(b)
	n := &node{partID: partID, headers: hs}
	n.setContentHeaders()
	boundary := n.params["boundary"]
	if n.isMultipart() && boundary != "" && depth < maxNesting {
		for i, pb := range splitMultipart(body, boundary) {
			if *count >= maxParts {
				break
			}
			id := strconv.Itoa(i)
			if partID != "" {
				id = partID + "." + id
			}
			n.children = append(n.children, fromRaw(pb, id, depth+1, count))
		}
		return n
	}
	if n.isMultipart() {
		// A multipart with no boundary cannot be split; read it as text.
		n.mediaType = "text/plain"
	}
	n.data = decodeTransfer(body, headerGet(hs, "Content-Transfer-Encoding"))
	n.hasData = true
	n.size = len(n.data)
	return n
}

// splitMultipart returns the bodies between boundary delimiters. The
// preamble and epilogue are dropped; a missing close delimiter ends the
// last part at the end of the input.
func splitMultipart(body []byte, boundary string) [][]byte {
	delim := []byte("--" + boundary)
	var parts [][]byte
	start := -1
	pos := 0
	for pos <= len(body) {
		lineEnd := len(body)
		if i := bytes.IndexByte(body[pos:], '\n'); i >= 0 {
			lineEnd = pos + i
		}
		ln := bytes.TrimRight(body[pos:lineEnd], "\r \t")
		if rest, ok := bytes.CutPrefix(ln, delim); ok && (len(rest) == 0 || bytes.Equal(rest, []byte("--"))) {
			if start >= 0 {
				end := pos
				if end > start && body[end-1] == '\n' {
					end--
					if end > start && body[end-1] == '\r' {
						end--
					}
				}
				parts = append(parts, body[start:end])
				if len(parts) >= maxParts {
					return parts
				}
			}
			if len(rest) == 2 {
				return parts
			}
			start = min(lineEnd+1, len(body))
		}
		if lineEnd >= len(body) {
			break
		}
		pos = lineEnd + 1
	}
	if start >= 0 {
		parts = append(parts, body[start:])
	}
	return parts
}

// decodeTransfer removes a Content-Transfer-Encoding. Unknown encodings
// and 7bit, 8bit and binary are identity.
func decodeTransfer(b []byte, cte string) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		return decodeBase64Body(b)
	case "quoted-printable":
		return decodeQP(b)
	}
	return b
}

// decodeBase64Body decodes a base64 body, ignoring whitespace and
// keeping what decoded before any corruption.
func decodeBase64Body(b []byte) []byte {
	clean := make([]byte, 0, len(b))
	for _, c := range b {
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		clean = append(clean, c)
	}
	clean = bytes.TrimRight(clean, "=")
	out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
	n, err := base64.RawStdEncoding.Decode(out, clean)
	if err != nil {
		if n2, err2 := base64.RawURLEncoding.Decode(out, clean); err2 == nil {
			return out[:n2]
		}
	}
	return out[:n]
}

// decodeQP decodes quoted-printable. It never fails: soft line breaks
// ("=" at the end of a line) join lines, "=XX" becomes a byte, anything
// else is kept as written, and trailing whitespace on a line — which
// RFC 2045 says a decoder must drop — is dropped.
func decodeQP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	lines := bytes.Split(b, []byte("\n"))
	for li, ln := range lines {
		ln = bytes.TrimRight(ln, "\r")
		ln = bytes.TrimRight(ln, " \t")
		soft := false
		if bytes.HasSuffix(ln, []byte("=")) {
			soft = true
			ln = ln[:len(ln)-1]
		}
		for i := 0; i < len(ln); i++ {
			if ln[i] == '=' && i+2 < len(ln) && isHex(ln[i+1]) && isHex(ln[i+2]) {
				out = append(out, unhex(ln[i+1])<<4|unhex(ln[i+2]))
				i += 2
				continue
			}
			out = append(out, ln[i])
		}
		if !soft && li < len(lines)-1 {
			out = append(out, '\n')
		}
	}
	return out
}
