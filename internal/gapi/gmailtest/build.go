package gmailtest

import (
	"bytes"
	"encoding/base64"
	"mime/quotedprintable"
	"strconv"
	"strings"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
)

// Part is one MIME entity the fake serializes. It is written by this
// package's own builder, not by internal/mime, so that a bug in the
// parser cannot be mirrored by the fixture it is tested against.
type Part struct {
	// ContentType is the full header value, parameters included and
	// already encoded as the fixture wants them on the wire.
	ContentType string
	// Disposition is the Content-Disposition value, or "".
	Disposition string
	// CTE is the transfer encoding: "", "7bit", "8bit",
	// "quoted-printable" or "base64".
	CTE string
	// ContentID is written as Content-ID when set.
	ContentID string
	// Content is the part's bytes after the transfer encoding is removed,
	// in whatever charset ContentType names.
	Content []byte
	// Filename is the decoded name, as Gmail reports it in a payload's
	// filename field. Setting it makes the part an attachment in the
	// payload (content behind an attachment id), as Gmail does.
	Filename string
	// Backed puts the content behind an attachment id even without a
	// filename, which is how Gmail stores a large body part (§3.7).
	Backed bool
	// Expanded serves a message/rfc822 part, whose Content is the
	// attached message, as its own parts: one child holding the attached
	// message's headers and tree, and no data or attachment id for the
	// part itself. Gmail served attached messages whole in the live run
	// of 2026-10-09 (§18 row 83); the fake can serve one this way, so the
	// server's refusal of a shape not seen is tested.
	Expanded bool
	// Boundary separates Children when this is a multipart.
	Boundary string
	Children []*Part
}

func (p *Part) mimeType() string {
	mt, _, _ := strings.Cut(p.ContentType, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	if mt == "" {
		return "text/plain"
	}
	return mt
}

func (p *Part) headers() []gmail.MessagePartHeader {
	var hs []gmail.MessagePartHeader
	add := func(n, v string) {
		if v != "" {
			hs = append(hs, gmail.MessagePartHeader{Name: n, Value: v})
		}
	}
	add("Content-Type", p.ContentType)
	add("Content-Transfer-Encoding", p.CTE)
	add("Content-Disposition", p.Disposition)
	if p.ContentID != "" {
		add("Content-ID", "<"+p.ContentID+">")
	}
	return hs
}

// writeRaw serializes the part: headers, a blank line, and the body with
// its transfer encoding applied. Lines end in CRLF.
func (p *Part) writeRaw(b *bytes.Buffer, top []gmail.MessagePartHeader) {
	for _, h := range append(append([]gmail.MessagePartHeader(nil), top...), p.headers()...) {
		b.WriteString(h.Name + ": " + h.Value + "\r\n")
	}
	b.WriteString("\r\n")
	if len(p.Children) > 0 {
		for _, c := range p.Children {
			b.WriteString("--" + p.Boundary + "\r\n")
			c.writeRaw(b, nil)
			b.WriteString("\r\n")
		}
		b.WriteString("--" + p.Boundary + "--\r\n")
		return
	}
	switch strings.ToLower(p.CTE) {
	case "base64":
		enc := base64.StdEncoding.EncodeToString(p.Content)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc)
	case "quoted-printable":
		w := quotedprintable.NewWriter(b)
		_, _ = w.Write(p.Content)
		_ = w.Close()
	default:
		b.Write(bytes.ReplaceAll(bytes.ReplaceAll(p.Content, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n")))
	}
}

// payload builds the Gmail view of the part: part ids "", "0", "0.1";
// data inline as base64url, or behind an attachment id for attachments
// and backed parts, whose bytes go to store.
func (p *Part) payload(partID string, top []gmail.MessagePartHeader, attID func(partID string) string, store map[string][]byte) gmail.MessagePart {
	mp := gmail.MessagePart{
		PartID:   partID,
		MimeType: p.mimeType(),
		Filename: p.Filename,
		Body:     &gmail.MessagePartBody{},
	}
	// Gmail reports header values unfolded.
	for _, h := range append(append([]gmail.MessagePartHeader(nil), top...), p.headers()...) {
		h.Value = strings.ReplaceAll(h.Value, "\r\n", "")
		mp.Headers = append(mp.Headers, h)
	}
	if len(p.Children) > 0 {
		for i, c := range p.Children {
			id := strconv.Itoa(i)
			if partID != "" {
				id = partID + "." + id
			}
			mp.Parts = append(mp.Parts, c.payload(id, nil, attID, store))
		}
		return mp
	}
	mp.Body.Size = int32(len(p.Content)) //nolint:gosec // fixture sizes are small
	if p.Expanded {
		if inner, err := parseRaw(p.Content); err == nil {
			id := "0"
			if partID != "" {
				id = partID + ".0"
			}
			mp.Parts = []gmail.MessagePart{inner.body.payload(id, inner.headers, attID, store)}
			return mp
		}
	}
	if p.Filename != "" || p.Backed {
		id := attID(partID)
		mp.Body.AttachmentID = id
		store[id] = p.Content
		return mp
	}
	mp.Body.Data = base64.URLEncoding.EncodeToString(p.Content)
	return mp
}

// Helpers the scenarios build parts with.

func textPart(charset, cte string, content []byte) *Part {
	return &Part{ContentType: "text/plain; charset=" + charset, CTE: cte, Content: content}
}

func htmlPart(cte string, content []byte) *Part {
	return &Part{ContentType: "text/html; charset=utf-8", CTE: cte, Content: content}
}

func multipart(kind, boundary string, children ...*Part) *Part {
	return &Part{ContentType: "multipart/" + kind + "; boundary=\"" + boundary + "\"", Boundary: boundary, Children: children}
}

// File is an attachment named filename, written as most mail clients
// write one: the name in both Content-Type and Content-Disposition,
// base64. Tests build the parts of AddPartsMessage with it.
func File(contentType, filename string, content []byte) *Part {
	return attachment(contentType+`; name="`+filename+`"`, `attachment; filename="`+filename+`"`, filename, content)
}

// attachment is a named attachment. contentType and disposition carry
// the name in whatever encoding the fixture exercises; filename is the
// decoded name Gmail would report.
func attachment(contentType, disposition, filename string, content []byte) *Part {
	return &Part{ContentType: contentType, Disposition: disposition, CTE: "base64", Content: content, Filename: filename}
}
