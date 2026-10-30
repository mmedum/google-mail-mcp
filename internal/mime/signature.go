package mime

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// A send-as signature goes below a draft's body as Gmail's web composer
// puts one there (§7.4, §18 row 98): in the HTML version, as Gmail holds
// it, inside the markup the composer writes around a signature, and in
// the plain text as that HTML reads. A signature that ends the message
// starts with the delimiter line "-- " (RFC 3676 §4.3); one above a
// quote has none, as Gmail writes it there, since a reader that drops
// what follows the delimiter would drop the quote too. EditRaw carries a
// signature over to a new body by finding that markup, and finds its
// text in the plain part by reading the HTML again.

// sigShape is the markup around a signature: lead, then open, which
// starts the div whose close ends the block, the signature, and close.
type sigShape struct {
	lead, open, close string
}

var (
	// sigEnding ends a message, after the delimiter.
	sigEnding = sigShape{
		lead:  `<div><br clear="all"></div><div><br></div><span class="gmail_signature_prefix">-- </span><br>`,
		open:  `<div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature">`,
		close: `</div>`,
	}
	// sigAbove sits above a quote, with no delimiter.
	sigAbove = sigShape{
		lead:  `<div><br clear="all"></div>`,
		open:  `<div><div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature">`,
		close: `</div></div>`,
	}
)

// shape is the markup for a signature, ending the message or not.
func shape(ending bool) sigShape {
	if ending {
		return sigEnding
	}
	return sigAbove
}

// OutBody is a message's plain text, its HTML version, and the images
// the HTML shows from parts of their own.
type OutBody struct {
	Text, HTML string
	Inline     []InlineImage
}

// InlineImage is an image an HTML version shows from a part of its own,
// by the part's Content-ID (RFC 2392).
type InlineImage struct {
	// ContentID is the part's Content-ID, without its angle brackets.
	ContentID string
	MediaType string
	Content   []byte
}

// Bodies is a message's plain text and HTML version (§7.4): text, the
// signature sig below it as Gmail's composer puts one, and quote below
// that. The HTML is h with the signature, or with h nil the version
// HTMLFromText makes of the text and the quote with the signature
// between them, "" when there is nothing to show. A signature ends the
// message unless a quote follows it. A quote goes only with the made
// version. An image the signature carries inside its HTML goes as a part
// of its own (inlineImages). A signature does not go below one already
// there: h that holds a block the composer marks as one is
// ErrSignedHTML, and text that holds the signature's text, as whole
// lines, is ErrSignedText. A signature of one line, such as a name a body
// may sign off with, is not looked for.
func Bodies(text string, h *string, sig, quote string) (OutBody, error) {
	if sig != "" {
		switch t := sigText(sig); {
		case h != nil && strings.Contains(*h, `data-smartmail="gmail_signature"`):
			return OutBody{}, ErrSignedHTML
		case lineCount(t) > 1 && holdsLines(text, t):
			return OutBody{}, ErrSignedText
		}
	}
	ending := quote == ""
	b := OutBody{Text: below(below(text, sigLines(sig, ending)), quote)}
	sig, b.Inline = inlineImages(sig)
	if h == nil {
		b.HTML = signHTML(HTMLFromText(text), sig, ending) + HTMLFromText(quote)
	} else {
		b.HTML = signHTML(*h, sig, ending)
	}
	return b, nil
}

// dataImage is an img whose src is the image itself, as a base64 data
// URL of any image type, with or without parameters (RFC 2397). The src
// follows a space, so a data-src is not one.
var dataImage = regexp.MustCompile(`(?i)(<img\b[^>]*?\ssrc\s*=\s*)(["'])data:(image/[a-z0-9.+-]+)(?:;[^;,"']*)*;base64,([A-Za-z0-9+/=\s]*)(["'])`)

// inlineImages moves each image sig carries as a base64 data URL into a
// part of its own, its src becoming cid:, as Gmail's composer sends one.
// Gmail does the same to such an image when a draft is saved, and drops
// the plain-text version as it does (§18 row 99), so the server does it
// first. The Content-ID is made from the image, so the same image gets
// the same one. Anything else stays as written.
func inlineImages(sig string) (string, []InlineImage) {
	var imgs []InlineImage
	seen := map[string]bool{}
	out := dataImage.ReplaceAllStringFunc(sig, func(m string) string {
		g := dataImage.FindStringSubmatch(m)
		content, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(g[4]), ""))
		if g[2] != g[5] || err != nil || len(content) == 0 {
			return m
		}
		sum := sha256.Sum256(content)
		id := "sig." + hex.EncodeToString(sum[:8]) + "@signature"
		if !seen[id] {
			seen[id] = true
			mt := strings.ToLower(g[3])
			if mt == "image/jpg" {
				mt = "image/jpeg"
			}
			imgs = append(imgs, InlineImage{ContentID: id, MediaType: mt, Content: content})
		}
		return g[1] + g[2] + "cid:" + id + g[5]
	})
	return out, imgs
}

// cidRef is a reference from HTML to a part by its Content-ID.
var cidRef = regexp.MustCompile(`(?i)\bcid:([^"'\s>]+)`)

// cids is the Content-IDs h refers to.
func cids(h string) map[string]bool {
	out := map[string]bool{}
	for _, m := range cidRef.FindAllStringSubmatch(h, -1) {
		out[m[1]] = true
	}
	return out
}

// holdsLines reports whether text holds lines as whole lines anywhere,
// the spaces ending a line aside. A signature's lines come from
// HTMLToText, which reads a no-break space as a space.
func holdsLines(text, lines string) bool {
	norm := func(s string) string { return trimLineEnds(normalizeNewlines(s)) }
	t, l := norm(text), norm(lines)
	for at := 0; ; {
		i := strings.Index(t[at:], l)
		if i < 0 {
			return false
		}
		i += at
		if j := i + len(l); (i == 0 || t[i-1] == '\n') && (j == len(t) || t[j] == '\n') {
			return true
		}
		at = i + 1
	}
}

// lineCount is how many lines of s say something.
func lineCount(s string) int {
	n := 0
	for l := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

// SameSignature reports whether two signatures are one: the HTML Gmail
// holds and the one a draft carries read the same once its images are
// parts and its line endings one kind.
func SameSignature(a, b string) bool {
	norm := func(s string) string {
		s, _ = inlineImages(s)
		return normalizeNewlines(s)
	}
	return norm(a) == norm(b)
}

// below is text with more below it after a blank line; either alone is
// as given.
func below(text, more string) string {
	if text == "" || more == "" {
		return text + more
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + "\n" + more
}

// sigText is a signature's HTML read as a body is, without what a reader
// would not see and no space ending a line; "" when it shows no text.
func sigText(sig string) string {
	t := strings.Trim(trimLineEnds(HTMLToText([]byte(sig)).Text), "\n")
	if strings.TrimSpace(t) == "" {
		return ""
	}
	return t
}

// sigLines is what the plain text carries for a signature: its text,
// after the delimiter line when it ends the message. That line stands
// alone for a signature with no text; above a quote, one with no text
// carries nothing.
func sigLines(sig string, ending bool) string {
	if sig == "" {
		return ""
	}
	t := sigText(sig)
	if ending {
		return strings.TrimSuffix(signatureDelimiter+"\n"+t, "\n") + "\n"
	}
	if t == "" {
		return ""
	}
	return t + "\n"
}

// signatureDelimiter is the line a signature that ends a message starts
// with in plain text.
const signatureDelimiter = "-- "

// signHTML is h with a signature below it, wrapped as Gmail's composer
// wraps one that ends the message or sits above a quote: before h's
// closing body tag when it has one, and at its end otherwise.
func signHTML(h, sig string, ending bool) string {
	if sig == "" {
		return h
	}
	s := shape(ending)
	block := s.lead + s.open + sig + s.close
	if i := strings.LastIndex(asciiLower(h), "</body"); i >= 0 {
		return h[:i] + block + h[i:]
	}
	return h + block
}

// sigBlock is a signature block found in an HTML version: the
// signature's own HTML, whether it ends the message, and where the block
// starts and ends.
type sigBlock struct {
	html       string
	ending     bool
	start, end int
}

// findBlock finds the last signature block in h of either shape, as
// signHTML writes one. One that ends the message runs to the body's
// close or the end of h, whatever the signature's own markup; one above
// a quote ends at the close of the div its open starts, so the quote
// below stays outside, and is not found when the signature leaves that
// div unbalanced.
func findBlock(h string) (sigBlock, bool) {
	s, start := sigEnding, strings.LastIndex(h, sigEnding.lead+sigEnding.open)
	if above := strings.LastIndex(h, sigAbove.lead+sigAbove.open); above > start {
		s, start = sigAbove, above
	}
	if start < 0 {
		return sigBlock{}, false
	}
	end, ok := len(h), true
	if s == sigEnding {
		if i := strings.LastIndex(asciiLower(h[start:]), "</body"); i >= 0 {
			end = start + i
		}
		end = start + len(strings.TrimRight(h[start:end], " \t\r\n"))
	} else {
		end, ok = divEnd(h, start+len(s.lead))
	}
	if !ok || !strings.HasSuffix(h[start:end], s.close) || end-start < len(s.lead)+len(s.open)+len(s.close) {
		return sigBlock{}, false
	}
	return sigBlock{html: h[start+len(s.lead)+len(s.open) : end-len(s.close)], ending: s == sigEnding,
		start: start, end: end}, true
}

// divEnd is where the div that starts at from closes.
func divEnd(h string, from int) (int, bool) {
	z := html.NewTokenizer(strings.NewReader(h[from:]))
	depth, at := 0, from
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return 0, false
		}
		at += len(z.Raw())
		if name, _ := z.TagName(); string(name) != "div" {
			continue
		}
		switch tt {
		case html.StartTagToken:
			depth++
		case html.EndTagToken:
			depth--
		}
		if depth == 0 {
			return at, true
		}
	}
}

// asciiLower lowers A to Z alone, byte by byte, so every index into the
// result is an index into s, whatever else s holds.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
