package mime

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
)

// Edit is a change to an existing message (§4.4): only what it names is
// changed, and every other header and part is written back as it was
// read, byte for byte.
type Edit struct {
	// Headers replace the top-level headers they name. A header's old
	// lines are removed and the new value, when not empty, written in
	// their place. Values are encoded already: FormatText,
	// FormatAddresses.
	Headers []SetHeader
	// Text and HTML replace the body's plain and HTML parts. When the
	// message has both, both must be given, so the two cannot disagree,
	// unless the HTML is the version HTMLFromText makes of the text:
	// HTMLMode says what Text alone does to that.
	Text, HTML *string
	HTMLMode   HTMLMode
	// Remove drops attachments by part id, as the unedited message
	// numbers them.
	Remove []string
	// Add appends attachments.
	Add []OutAttachment
}

// HTMLMode is what new plain text, given without HTML, does to the
// HTML version HTMLFromText makes of a message's text (§7.4).
type HTMLMode int

const (
	// KeepShape makes the HTML version again from the new text when the
	// message has one, and keeps a message without one plain, unless it
	// has no text yet.
	KeepShape HTMLMode = iota
	// PlainOnly drops the HTML version, leaving the plain text alone.
	PlainOnly
	// WithHTML gives the message the HTML version, whether it had one.
	WithHTML
)

// SetHeader is one header to write.
type SetHeader struct {
	Name, Value string
}

// Edit errors, each the caller's to fix.
var (
	// ErrBothBodies is a change to one of a message's two versions of its
	// text, which would leave the other saying something else.
	ErrBothBodies = errors.New("the message has a plain-text and an HTML version; give both so they agree")
	// ErrNoPlainBody is HTML for a message with no text at all.
	ErrNoPlainBody = errors.New("the message has no body yet; give the plain text too")
	// ErrHTMLOnly is plain text for a message whose only text is HTML.
	ErrHTMLOnly = errors.New("the message's only text is HTML; give the HTML too")
	// ErrDropsHTML is PlainOnly on a message whose HTML was written
	// another way, which dropping it would lose.
	ErrDropsHTML = errors.New("the message's HTML was not made from its text; dropping it would lose what it says")
	// ErrTextLayout is a plain and HTML version that are not one
	// multipart/alternative pair, which PlainOnly cannot fold into one.
	ErrTextLayout = errors.New("the message's plain and HTML versions are not one pair")
	// ErrNotAttachment is a part id that names no attachment.
	ErrNotAttachment = errors.New("no attachment has that part id")
	// ErrTooManyParts is a message with more parts than this package
	// reads, which an edit could not write back whole.
	ErrTooManyParts = errors.New("the message has more MIME parts than this server edits")
)

// EditRaw applies e to raw RFC 5322 bytes, and reports whether it
// changed the HTML version, as given or as e.HTMLMode made it.
func EditRaw(raw []byte, e Edit) (out []byte, htmlChanged bool, err error) {
	count := 0
	root := readEntity(raw, "", 0, &count)
	if count >= maxParts {
		// readEntity stopped reading parts at the cap, and a multipart
		// rewritten from what was read would drop the rest silently.
		return nil, false, ErrTooManyParts
	}

	// Every part is found by its original id before anything moves.
	var removals []*entity
	for _, id := range e.Remove {
		n, parent := root.find(id)
		if n == nil || parent == nil || n.isMultipart() || n.isBody() {
			return nil, false, fmt.Errorf("%w: %q", ErrNotAttachment, id)
		}
		removals = append(removals, n)
	}
	plain, html := root.bodies()

	for _, h := range e.Headers {
		if isContentHeader(h.Name) {
			return nil, false, fmt.Errorf("header %s describes the content and is not set by name", h.Name)
		}
		if HasControl(h.Value) {
			return nil, false, fmt.Errorf("header %s: %w", h.Name, ErrControl)
		}
		root.headers = setHeader(root.headers, h.Name, h.Value)
	}
	for _, n := range removals {
		if !root.remove(n) {
			return nil, false, fmt.Errorf("part %q is the only part of its multipart, so removing it would leave an empty one", n.partID)
		}
	}
	htm, drop, err := e.htmlFor(plain, html)
	if err != nil {
		return nil, false, err
	}
	if drop {
		root, err = dropHTML(root, plain, html, *e.Text)
	} else {
		root, err = replaceBody(root, plain, html, e.Text, htm)
	}
	if err != nil {
		return nil, false, err
	}
	if len(e.Add) > 0 {
		var atts []*entity
		for _, a := range e.Add {
			att, err := attachmentEntity(a)
			if err != nil {
				return nil, false, err
			}
			atts = append(atts, att)
		}
		root = appendAttachments(root, atts)
	}
	var b bytes.Buffer
	b.Grow(root.size())
	root.write(&b)
	return b.Bytes(), drop || htm != nil, nil
}

// setHeader replaces every line of name with one line of value, where
// the first of them stood, or before the content headers when there
// was none. An empty value removes the header.
func setHeader(hs []rawHeader, name, value string) []rawHeader {
	at := -1
	out := make([]rawHeader, 0, len(hs)+1)
	for _, h := range hs {
		if strings.EqualFold(h.name, name) {
			if at < 0 {
				at = len(out)
			}
			continue
		}
		out = append(out, h)
	}
	if value == "" {
		return out
	}
	if at < 0 {
		at = len(out)
		for i, h := range out {
			if isContentHeader(h.name) {
				at = i
				break
			}
		}
	}
	line := rawHeader{name: name, text: fold(name + ": " + value)}
	return append(out[:at], append([]rawHeader{line}, out[at:]...)...)
}

// remove takes n out of the tree. A multipart left with no parts is
// refused.
func (e *entity) remove(n *entity) bool { return e.splice(n, nil) }

// replace puts with in old's place. It reports whether old was found
// below e.
func (e *entity) replace(old, with *entity) bool { return e.splice(old, with) }

// splice replaces old below e with with, or removes it when with is nil,
// and marks every multipart above it for rewriting.
func (e *entity) splice(old, with *entity) bool {
	for i, c := range e.children {
		if c == old {
			switch {
			case with != nil:
				e.children[i] = with
			case len(e.children) == 1:
				return false
			default:
				e.children = append(e.children[:i], e.children[i+1:]...)
			}
			e.dirty = true
			return true
		}
		if c.splice(old, with) {
			e.dirty = true
			return true
		}
	}
	return false
}

// swap replaces old with a new entity anywhere in the tree rooted at
// root and returns the root. Replacing the root itself keeps its message
// headers: only the content headers are the new entity's.
func swap(root, old, with *entity) *entity {
	if root != old {
		root.replace(old, with)
		return root
	}
	var kept []rawHeader
	for _, h := range root.headers {
		if !isContentHeader(h.name) {
			kept = append(kept, h)
		}
	}
	kept = append(kept, rawHeader{name: "MIME-Version", text: "MIME-Version: 1.0"})
	with.headers = append(kept, with.headers...)
	return with
}

// htmlFor is the HTML an edit writes beside new text: the caller's, or
// the version made from the text as e.HTMLMode says. drop is set when a
// made version goes and the text stays alone. HTML written another way
// is left to replaceBody, which refuses text alone beside it.
func (e Edit) htmlFor(plain, html *entity) (htm *string, drop bool, err error) {
	if e.Text == nil || e.HTML != nil {
		return e.HTML, false, nil
	}
	if html != nil && (plain == nil || !isMade(plain, html)) {
		if e.HTMLMode == PlainOnly {
			return nil, false, ErrDropsHTML
		}
		return nil, false, nil
	}
	if e.HTMLMode == PlainOnly || e.HTMLMode == KeepShape && html == nil && hasText(plain) {
		return nil, html != nil, nil
	}
	if h := HTMLFromText(*e.Text); h != "" {
		return &h, false, nil
	}
	return nil, html != nil, nil
}

// hasText reports whether a plain part says anything: a draft with no
// text yet has no shape to keep, and takes the HTML version.
func hasText(plain *entity) bool {
	return plain != nil && strings.TrimSpace(plainText(plain.decoded())) != ""
}

// isMade reports whether html says nothing the plain text does not: it
// holds only paragraphs, line breaks and links whose text is their
// target, and it reads back as the text. Every HTML version this server
// makes is such HTML, whatever its line rules were when it was made.
func isMade(plain, html *entity) bool {
	text, ok := madeText(htmlText(html.decoded()))
	return ok && text == madeNormal(plainText(plain.decoded()))
}

// madeTag is the markup HTMLFromText writes.
var madeTag = regexp.MustCompile(`<a href="([^"<>]*)">([^"<>]*)</a>|<br>|</?p>`)

// madeText reads HTML made from plain text back as that text, or reports
// that it holds other markup or a link whose text is not its target.
func madeText(h string) (string, bool) {
	// The line breaks HTMLFromText writes after its tags show nothing; one
	// anywhere else shows as a space, so it stays and must match.
	h = strings.NewReplacer("<br>\n", "<br>", "</p>\n", "</p>").Replace(normalizeNewlines(h))
	var b strings.Builder
	last := 0
	for _, m := range madeTag.FindAllStringSubmatchIndex(h, -1) {
		b.WriteString(h[last:m[0]])
		switch tag := h[m[0]:m[1]]; {
		case tag == "<br>":
			b.WriteString("\n")
		case tag == "</p>":
			b.WriteString("\n\n")
		case tag == "<p>":
		case h[m[2]:m[3]] != h[m[4]:m[5]]:
			return "", false
		default:
			b.WriteString(h[m[4]:m[5]])
		}
		last = m[1]
	}
	b.WriteString(h[last:])
	if strings.ContainsAny(b.String(), "<>") {
		return "", false
	}
	return madeNormal(html.UnescapeString(b.String())), true
}

// madeNormal is text as the HTML version shows it: a no-break space and a
// tab as spaces, no space ending a line, and one blank line between
// paragraphs, none around them.
func madeNormal(s string) string {
	s = strings.NewReplacer("\u00a0", " ", "\t", "    ").Replace(normalizeNewlines(s))
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		l = strings.TrimRight(l, " ")
		if l == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, l)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// dropHTML leaves text as the message's only version of its text: the
// multipart/alternative holding the plain and HTML pair becomes the
// plain part.
func dropHTML(root, plain, html *entity, text string) (*entity, error) {
	_, pair := root.find(html.partID)
	if pair == nil || pair.mediaType != "multipart/alternative" || len(pair.children) != 2 ||
		!slices.Contains(pair.children, plain) {
		return nil, ErrTextLayout
	}
	return swap(root, pair, textEntity("plain", text)), nil
}

// decoded is a body part as the read tree holds it, its transfer
// encoding undone, for plainText and htmlText.
func (e *entity) decoded() *node {
	_, params := ParseMediaType(rawGet(e.headers, "Content-Type"))
	return &node{mediaType: e.mediaType, params: params, hasData: true,
		data: decodeTransfer(e.body, rawGet(e.headers, "Content-Transfer-Encoding"))}
}

// replaceBody applies new text and HTML to the body parts found before
// any change. A part that is replaced is rebuilt; its siblings are kept.
func replaceBody(root, plain, html *entity, text, htm *string) (*entity, error) {
	switch {
	case text == nil && htm == nil:
		return root, nil
	case text != nil && htm != nil:
		switch {
		case plain != nil && html != nil:
			root = swap(root, plain, textEntity("plain", *text))
			return swap(root, html, textEntity("html", *htm)), nil
		case plain != nil:
			return swap(root, plain, bodyEntity(*text, *htm)), nil
		case html != nil:
			return swap(root, html, bodyEntity(*text, *htm)), nil
		}
		return insertBody(root, bodyEntity(*text, *htm)), nil
	case text != nil:
		switch {
		case plain != nil && html != nil:
			return nil, ErrBothBodies
		case plain != nil:
			return swap(root, plain, textEntity("plain", *text)), nil
		case html != nil:
			return nil, ErrHTMLOnly
		}
		return insertBody(root, textEntity("plain", *text)), nil
	default:
		switch {
		case plain != nil && html != nil:
			return nil, ErrBothBodies
		case html != nil:
			return swap(root, html, textEntity("html", *htm)), nil
		case plain != nil:
			// The plain part stays as it was, now beside the HTML.
			alt := multipartEntity("alternative", plain, textEntity("html", *htm))
			if plain == root {
				alt.children[0] = contentOnly(plain)
			}
			return swap(root, plain, alt), nil
		}
		return nil, ErrNoPlainBody
	}
}

// contentOnly is a copy of e with only its content headers, for moving
// the message's own entity down into a part.
func contentOnly(e *entity) *entity {
	c := *e
	c.headers = nil
	for _, h := range e.headers {
		if isContentHeader(h.name) && !strings.EqualFold(h.name, "MIME-Version") {
			c.headers = append(c.headers, h)
		}
	}
	return &c
}

// insertBody adds a body to a message that has none: first in a
// multipart/mixed, or beside what the message was.
func insertBody(root, body *entity) *entity {
	if root.mediaType == "multipart/mixed" {
		root.children = append([]*entity{body}, root.children...)
		root.dirty = true
		return root
	}
	return swap(root, root, multipartEntity("mixed", body, contentOnly(root)))
}

// appendAttachments adds files to a multipart/mixed message, or makes
// the message one with what it was as the first part.
func appendAttachments(root *entity, atts []*entity) *entity {
	if root.mediaType == "multipart/mixed" {
		root.children = append(root.children, atts...)
		root.dirty = true
		return root
	}
	return swap(root, root, multipartEntity("mixed", append([]*entity{contentOnly(root)}, atts...)...))
}
