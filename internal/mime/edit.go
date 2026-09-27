package mime

import (
	"bytes"
	"errors"
	"fmt"
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
	// message has both, both must be given, so the two cannot disagree.
	Text, HTML *string
	// Remove drops attachments by part id, as the unedited message
	// numbers them.
	Remove []string
	// Add appends attachments.
	Add []OutAttachment
}

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
	// ErrNotAttachment is a part id that names no attachment.
	ErrNotAttachment = errors.New("no attachment has that part id")
	// ErrTooManyParts is a message with more parts than this package
	// reads, which an edit could not write back whole.
	ErrTooManyParts = errors.New("the message has more MIME parts than this server edits")
)

// EditRaw applies e to raw RFC 5322 bytes.
func EditRaw(raw []byte, e Edit) ([]byte, error) {
	count := 0
	root := readEntity(raw, "", 0, &count)
	if count >= maxParts {
		// readEntity stopped reading parts at the cap, and a multipart
		// rewritten from what was read would drop the rest silently.
		return nil, ErrTooManyParts
	}

	// Every part is found by its original id before anything moves.
	var removals []*entity
	for _, id := range e.Remove {
		n, parent := root.find(id)
		if n == nil || parent == nil || n.isMultipart() || n.isBody() {
			return nil, fmt.Errorf("%w: %q", ErrNotAttachment, id)
		}
		removals = append(removals, n)
	}
	plain, html := root.bodies()

	for _, h := range e.Headers {
		if isContentHeader(h.Name) {
			return nil, fmt.Errorf("header %s describes the content and is not set by name", h.Name)
		}
		if HasControl(h.Value) {
			return nil, fmt.Errorf("header %s: %w", h.Name, ErrControl)
		}
		root.headers = setHeader(root.headers, h.Name, h.Value)
	}
	for _, n := range removals {
		if !root.remove(n) {
			return nil, fmt.Errorf("part %q is the only part of its multipart, so removing it would leave an empty one", n.partID)
		}
	}
	var err error
	if root, err = replaceBody(root, plain, html, e.Text, e.HTML); err != nil {
		return nil, err
	}
	if len(e.Add) > 0 {
		var atts []*entity
		for _, a := range e.Add {
			att, err := attachmentEntity(a)
			if err != nil {
				return nil, err
			}
			atts = append(atts, att)
		}
		root = appendAttachments(root, atts)
	}
	var b bytes.Buffer
	b.Grow(root.size())
	root.write(&b)
	return b.Bytes(), nil
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
