// Package mime parses email: RFC 5322 headers and addresses, MIME
// structure (RFC 2045–2047), RFC 2231 parameters, every part's charset
// and transfer encoding, and the choice and conversion of the body a
// person reads (docs/architecture.md §4.10, §7.2).
//
// It reads two sources — a Gmail payload tree and raw RFC 5322 bytes —
// through one internal representation, so the two cannot disagree. It
// never touches the network and never panics on input: every step is
// lenient, keeping what it can read of a malformed message.
//
// Mail content is data (§4.1). This package removes what a reader would
// not see and counts it; it does not judge or act on what remains.
package mime

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
)

// Body sources.
const (
	SourcePlain = "text/plain"
	SourceHTML  = "text/html"
	SourceBoth  = "text/plain+text/html"
)

// Message is a parsed email.
type Message struct {
	// Headers are the top-level headers in order, undecoded.
	Headers []Header

	Subject string
	From    []Address
	Sender  []Address
	To      []Address
	Cc      []Address
	Bcc     []Address
	ReplyTo []Address
	// LenientHeaders names the address headers the standard parser
	// refused, whose addresses came from the lenient fallback.
	LenientHeaders []string

	Date       time.Time // zero when the Date header could not be read
	DateHeader string

	MessageID       string // RFC 5322 Message-ID, with brackets
	InReplyTo       []string
	References      []string
	ListUnsubscribe string
	// Unsubscribe is ListUnsubscribe read as URIs, with whether the
	// sender declares one-click unsubscribe.
	Unsubscribe Unsubscribe

	// HeaderHidden counts invisible characters removed from the subject,
	// display names, addresses and attachments' types (Attachment.Hidden).
	HeaderHidden int

	// ContentType is the top-level media type.
	ContentType string

	Body        Body
	Attachments []Attachment
	// NeedsFetch lists the parts whose content is behind an attachment
	// id and is needed to read the message: body parts, and at most one
	// calendar invitation (calendarFetch). Fetch each with
	// messages.attachments.get and parse again with the bytes keyed by
	// part id.
	NeedsFetch []PartRef
}

// Body is the text a person reads, chosen per §7.2.
type Body struct {
	Text string
	// Source is SourcePlain, SourceHTML, SourceBoth, or "" when the
	// message has no readable body.
	Source  string
	PartIDs []string
	// PlaceholderSkipped is set when a plain part only pointed at the
	// HTML version and the HTML was read instead.
	PlaceholderSkipped bool
	// Hidden is what was dropped because a reader would not see it.
	Hidden []HiddenCount
	Links  []Link
	// Spans are quoted text and signatures in Text.
	Spans []Span
	// UnknownCharsets are labels no table knew; those parts were read
	// as UTF-8 or windows-1252.
	UnknownCharsets []string
	// Missing lists body parts not decoded because their content was
	// not supplied (see Message.NeedsFetch).
	Missing []string
}

// HiddenChars is the total dropped as hidden.
func (b Body) HiddenChars() int { return HiddenTotal(b.Hidden) }

// Mismatches returns the links whose text names a different host.
func (b Body) Mismatches() []Link {
	var out []Link
	for _, l := range b.Links {
		if l.Mismatch {
			out = append(out, l)
		}
	}
	return out
}

// Attachment is a part a person would save rather than read, including
// inline images and calendar invitations.
type Attachment struct {
	PartID string
	// Filename is the declared name reduced to a safe base name, or a
	// generic name when none was declared.
	Filename string
	// DeclaredName is the decoded name as the sender wrote it, with
	// invisible characters shown as \u{…} escapes.
	DeclaredName string
	// Renamed is set when Filename differs from what was declared.
	Renamed     bool
	NameMissing bool
	MimeType    string
	Size        int
	// AttachmentID fetches the content, when Gmail stored it apart.
	AttachmentID string
	ContentID    string
	Inline       bool
	// CalendarMethod is set for text/calendar: REQUEST, CANCEL, REPLY…,
	// or "UNKNOWN" when it could not be read.
	CalendarMethod string
	// Invitation is what a calendar part says about its event, when its
	// content was read; nil otherwise.
	Invitation *Invitation
	// Alternative is set when the part is one version of a body, inside
	// a multipart/alternative: an invitation's calendar version beside
	// its text and HTML, for one.
	Alternative bool
	// ContentMissing is set when Gmail gave neither the part's content
	// nor an attachment id to fetch it by, though the part has parts of
	// its own or a size. An attached message Gmail serves as its parts
	// would read this way (§18 row 83); an empty part does not.
	ContentMissing bool
	// Hidden counts invisible characters removed from MimeType and
	// CalendarMethod, which are the sender's Content-Type values.
	Hidden int
}

// PartRef names a part whose content must be fetched.
type PartRef struct {
	PartID       string
	AttachmentID string
	MimeType     string
	Size         int
}

// ParsePayload parses a Gmail payload tree (format=full, or the headers
// of format=metadata). fetched supplies, keyed by part id, the bytes of
// parts Gmail stored behind an attachment id; Gmail's attachment ids
// are not stable across reads, part ids are.
func ParsePayload(p *gmail.MessagePart, fetched map[string][]byte) (*Message, error) {
	if p == nil {
		return nil, errors.New("message has no payload")
	}
	count := 0
	return analyze(fromPayload(p, fetched, 0, &count)), nil
}

// ParseRaw parses RFC 5322 bytes. It never fails: a malformed message
// yields what could be read of it.
func ParseRaw(raw []byte) *Message {
	count := 0
	return analyze(fromRaw(raw, "", 0, &count))
}

// ParseRawBase64URL parses Gmail's format=raw field.
func ParseRawBase64URL(s string) (*Message, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("message has no raw content")
	}
	return ParseRaw(decodeBase64URL(s)), nil
}

// Header returns the first header of that name, decoded.
func (m *Message) Header(name string) string {
	return DecodeHeader(headerGet(m.Headers, name))
}

func analyze(root *node) *Message {
	m := &Message{Headers: root.headers, ContentType: root.mediaType}
	m.decodeHeaders()

	var candidates, calendars []*node
	var walk func(n *node, depth int, alternative bool)
	walk = func(n *node, depth int, alternative bool) {
		if depth > maxNesting {
			return
		}
		if n.isMultipart() {
			for _, ch := range n.children {
				walk(ch, depth+1, n.mediaType == "multipart/alternative")
			}
			return
		}
		switch {
		case n.isBodyLeaf():
			candidates = append(candidates, n)
		default:
			if n.isCalendar() && n.attachmentID != "" {
				calendars = append(calendars, n)
			}
			a := n.attachment()
			a.Alternative = alternative
			m.HeaderHidden += a.Hidden
			m.Attachments = append(m.Attachments, a)
		}
	}
	walk(root, 0, false)
	if ref, ok := calendarFetch(calendars); ok {
		m.NeedsFetch = append(m.NeedsFetch, ref)
	}

	for _, c := range candidates {
		if !c.hasData && (c.attachmentID != "" || c.size > 0) {
			m.NeedsFetch = append(m.NeedsFetch, PartRef{PartID: c.partID, AttachmentID: c.attachmentID, MimeType: c.mediaType, Size: c.size})
		}
	}
	m.Body = buildBody(root)
	return m
}

func (m *Message) decodeHeaders() {
	strip := func(s string) string {
		out, n := StripInvisible(s)
		m.HeaderHidden += n
		return out
	}
	m.Subject = strip(DecodeHeader(headerGet(m.Headers, "Subject")))
	addrs := func(name string) []Address {
		v := headerGet(m.Headers, name)
		list, strict := ParseAddressList(v)
		if !strict {
			m.LenientHeaders = append(m.LenientHeaders, name)
		}
		for i := range list {
			list[i].Name = strip(cleanHeader(list[i].Name))
			list[i].Email = strip(list[i].Email)
		}
		return list
	}
	m.From = addrs("From")
	m.Sender = addrs("Sender")
	m.To = addrs("To")
	m.Cc = addrs("Cc")
	m.Bcc = addrs("Bcc")
	m.ReplyTo = addrs("Reply-To")
	m.DateHeader = cleanHeader(headerGet(m.Headers, "Date"))
	m.Date, _ = ParseDate(m.DateHeader)
	if ids := parseMsgIDs(headerGet(m.Headers, "Message-ID")); len(ids) > 0 {
		m.MessageID = ids[0]
	}
	m.InReplyTo = parseMsgIDs(headerGet(m.Headers, "In-Reply-To"))
	m.References = parseMsgIDs(headerGet(m.Headers, "References"))
	m.ListUnsubscribe = cleanHeader(headerGet(m.Headers, "List-Unsubscribe"))
	m.Unsubscribe = parseUnsubscribe(m.Headers)
}

// declaredName is the part's name: Content-Disposition filename, then
// Content-Type name, then Gmail's own filename field.
func (n *node) declaredName() string {
	for _, v := range []string{n.dispParams["filename"], n.params["name"], n.gmailName} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func (n *node) isBodyLeaf() bool {
	if n.mediaType != "text/plain" && n.mediaType != "text/html" {
		return false
	}
	return n.disposition != "attachment" && n.declaredName() == ""
}

func (n *node) attachment() Attachment {
	declared := n.declaredName()
	a := Attachment{
		PartID:       n.partID,
		Size:         n.size,
		AttachmentID: n.attachmentID,
		DeclaredName: escapeInvisible(declared),
		ContentID:    strings.Trim(strings.TrimSpace(headerGet(n.headers, "Content-ID")), "<>"),
	}
	if n.hasData && a.Size == 0 {
		a.Size = len(n.data)
	}
	a.ContentMissing = !n.hasData && n.attachmentID == "" && (len(n.children) > 0 || n.size > 0)
	a.Inline = n.disposition == "inline" || (n.disposition == "" && a.ContentID != "")
	a.MimeType, a.Hidden = StripInvisible(n.mediaType)
	a.Filename = SafeBaseName(declared)
	switch {
	case declared == "":
		a.NameMissing = true
		a.Filename = fallbackName(n.mediaType)
	case a.Filename == "":
		a.Renamed = true
		a.Filename = fallbackName(n.mediaType)
	default:
		a.Renamed = a.Filename != declared
	}
	if n.isCalendar() {
		var method string
		if n.hasData {
			a.Invitation, method = parseInvitation(n.data, n.params["charset"])
		}
		var hidden int
		a.CalendarMethod, hidden = StripInvisible(n.calendarMethod(method))
		a.Hidden += hidden
		if a.NameMissing {
			a.Filename = "invite.ics"
		}
	}
	return a
}

func (n *node) isCalendar() bool {
	return n.mediaType == "text/calendar" || n.mediaType == "application/ics"
}

// calendarFetch picks the one calendar part, of those Gmail stored
// behind an attachment id, whose content is worth a fetch: an
// invitation's details, and often its method, are only there (§7.2). It
// prefers a part whose Content-Type does not state the method, as an
// invite.ics attachment often does not, and then the first. Nothing is
// fetched once one of them has its content, or for a part over
// maxICSBytes.
func calendarFetch(parts []*node) (PartRef, bool) {
	var pick *node
	for _, n := range parts {
		if n.hasData {
			return PartRef{}, false
		}
		if n.size > maxICSBytes {
			continue
		}
		if pick == nil || (pick.params["method"] != "" && n.params["method"] == "") {
			pick = n
		}
	}
	if pick == nil {
		return PartRef{}, false
	}
	return PartRef{PartID: pick.partID, AttachmentID: pick.attachmentID, MimeType: pick.mediaType, Size: pick.size}, true
}

// calendarMethod is the part's method: its Content-Type's, else the
// METHOD its content gives, else "UNKNOWN".
func (n *node) calendarMethod(content string) string {
	if v := strings.TrimSpace(n.params["method"]); v != "" {
		return strings.ToUpper(v)
	}
	if content != "" && len(content) <= 64 && nameEnd(content, 0) == len(content) {
		return strings.ToUpper(content)
	}
	return "UNKNOWN"
}

func escapeInvisible(s string) string {
	if !hasInvisible(s) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if isInvisible(r) {
			b.WriteString(`\u{` + strings.ToUpper(strconv.FormatInt(int64(r), 16)) + `}`)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
