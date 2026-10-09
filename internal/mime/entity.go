package mime

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
)

// entity is one MIME entity as it goes on the wire, for writing: the
// build side and the edit side both produce entities and one writer
// serializes them.
//
// An entity read from existing bytes keeps those bytes. Until something
// inside it changes it is written back exactly as it was read, so an
// edit leaves every part it does not touch as Gmail stored it (§4.4).
type entity struct {
	partID string
	// headers are the header lines, each whole with its folding kept.
	headers []rawHeader
	// body is the bytes after the blank line, as read or as encoded.
	body []byte

	mediaType   string
	boundary    string
	disposition string
	named       bool // declares a file name
	children    []*entity
	// dirty is set when the entity must be written from its parts rather
	// than from body: a child was replaced, added or removed.
	dirty bool
}

// rawHeader is one header line with its continuation lines. name is ""
// for a line that has no colon, kept so nothing read is dropped.
type rawHeader struct {
	name string
	text string // "Name: value", folded lines joined by CRLF
}

func (e *entity) isMultipart() bool { return strings.HasPrefix(e.mediaType, "multipart/") }

// isBody reports a text part a person reads rather than saves, as
// analyze decides it.
func (e *entity) isBody() bool {
	return (e.mediaType == "text/plain" || e.mediaType == "text/html") && e.disposition != "attachment" && !e.named
}

// splitRawHeaders separates a header block from the body, keeping each
// header's lines as written. It accepts CRLF and bare LF.
func splitRawHeaders(b []byte) ([]rawHeader, []byte) {
	end, bodyStart := len(b), len(b)
	if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
		end, bodyStart = i, i+4
	}
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 && i < end {
		end, bodyStart = i, i+2
	}
	if bytes.HasPrefix(b, []byte("\r\n")) {
		return nil, b[2:]
	}
	if bytes.HasPrefix(b, []byte("\n")) {
		return nil, b[1:]
	}
	var hs []rawHeader
	for line := range strings.SplitSeq(string(b[:end]), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(hs) > 0 {
			hs[len(hs)-1].text += "\r\n" + line
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t") {
			name = ""
		}
		hs = append(hs, rawHeader{name: name, text: line})
	}
	return hs, b[bodyStart:]
}

// value is a header's value unfolded, as splitHeaders reads it.
func (h rawHeader) value() string {
	_, v, _ := strings.Cut(strings.ReplaceAll(h.text, "\r\n", ""), ":")
	return strings.TrimSpace(v)
}

func rawGet(hs []rawHeader, name string) string {
	for _, h := range hs {
		if strings.EqualFold(h.name, name) {
			return h.value()
		}
	}
	return ""
}

// readEntity reads raw bytes into entities, with Gmail's part ids: ""
// for the message, "0", "1" for its parts, "0.1" below them. It is the
// numbering fromRaw and a Gmail payload use, so a part id get_draft
// lists names the same part here.
func readEntity(b []byte, partID string, depth int, count *int) *entity {
	*count++
	hs, body := splitRawHeaders(b)
	e := &entity{partID: partID, headers: hs, body: body}
	mt, params := ParseMediaType(rawGet(hs, "Content-Type"))
	if mt == "" || !strings.Contains(mt, "/") {
		mt = "text/plain"
	}
	e.mediaType = mt
	disp, dparams := ParseMediaType(rawGet(hs, "Content-Disposition"))
	e.disposition = disp
	e.named = strings.TrimSpace(dparams["filename"]) != "" || strings.TrimSpace(params["name"]) != ""
	if !e.isMultipart() {
		return e
	}
	e.boundary = params["boundary"]
	if e.boundary == "" || depth >= maxNesting {
		// Unsplittable: kept as it is and never edited inside. fromRaw
		// reads one as text, and so is it classed here, so the two agree
		// on which parts are bodies.
		e.mediaType = "text/plain"
		return e
	}
	for i, pb := range splitMultipart(body, e.boundary) {
		if *count >= maxParts {
			break
		}
		id := strconv.Itoa(i)
		if partID != "" {
			id = partID + "." + id
		}
		e.children = append(e.children, readEntity(pb, id, depth+1, count))
	}
	return e
}

// write serializes e. An entity that was read and not changed is its
// own bytes again.
func (e *entity) write(b *bytes.Buffer) {
	for _, h := range e.headers {
		b.WriteString(h.text + "\r\n")
	}
	b.WriteString("\r\n")
	if !e.dirty {
		b.Write(e.body)
		return
	}
	for _, c := range e.children {
		b.WriteString("--" + e.boundary + "\r\n")
		c.write(b)
		b.WriteString("\r\n")
	}
	b.WriteString("--" + e.boundary + "--\r\n")
}

// size is about how many bytes write produces, to size its buffer once.
func (e *entity) size() int {
	n := len(e.body) + 2
	for _, h := range e.headers {
		n += len(h.text) + 2
	}
	if e.dirty {
		for _, c := range e.children {
			n += c.size() + len(e.boundary) + 6
		}
		n += len(e.boundary) + 6
	}
	return n
}

// find returns the entity with a part id, and its parent.
func (e *entity) find(partID string) (found, parent *entity) {
	if e.partID == partID {
		return e, nil
	}
	for _, c := range e.children {
		if c.partID == partID {
			return c, e
		}
		if f, p := c.find(partID); f != nil {
			return f, p
		}
	}
	return nil, nil
}

// bodies returns the first plain and the first HTML body part, looking
// through multiparts but not into attachments.
func (e *entity) bodies() (plain, html *entity) {
	var walk func(n *entity)
	walk = func(n *entity) {
		if n.isMultipart() {
			for _, c := range n.children {
				walk(c)
			}
			return
		}
		if !n.isBody() {
			return
		}
		if n.mediaType == "text/plain" && plain == nil {
			plain = n
		}
		if n.mediaType == "text/html" && html == nil {
			html = n
		}
	}
	walk(e)
	return plain, html
}

// isContentHeader names the headers that describe an entity's own
// content, which a replacement brings its own of.
func isContentHeader(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "content-") || n == "mime-version"
}

// newBoundary draws a multipart boundary. It starts with "=_", which
// quoted-printable and base64 can never produce, so no part this package
// encodes can contain it. An attached message goes as written, and
// multipartEntity draws again for one that holds the boundary drawn.
// Tests replace it to get fixed bytes.
var newBoundary = func() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "=_" + hex.EncodeToString(b[:])
}
