package mime

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
)

// placeholders are what a plain part says when it only points at the
// HTML version (§3.7). Matched on short plain parts only.
var placeholders = regexp.MustCompile(`(?i)(view (this|the) (e-?mail|message|newsletter)? ?(in|with) (your|a) (web )?browser|` +
	`(html|web) version|` +
	`does ?n[o']t support html|not support html|` +
	`this (is an? )?(html|mime|multi-?part) (formatted )?(message|e-?mail)|` +
	`enable html|html-?capable|` +
	`to view this (e-?mail|message))`)

// maxPlaceholderChars is the size under which a plain part can be a
// placeholder; a real body that mentions a web version is longer.
const maxPlaceholderChars = 400

func isPlaceholder(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	return utf8.RuneCountInString(t) <= maxPlaceholderChars && placeholders.MatchString(t)
}

type piece struct {
	n    *node
	text string // decoded, for plain parts with data
}

// selectBody chooses the parts a person reads: within
// multipart/alternative, the plain part unless it is absent or a
// placeholder, then HTML; elsewhere, every body part in order.
func selectBody(n *node, depth int, skipped *bool) []piece {
	if depth > maxNesting {
		return nil
	}
	if !n.isMultipart() {
		if !n.isBodyLeaf() {
			return nil
		}
		p := piece{n: n}
		if n.mediaType == "text/plain" && n.hasData {
			p.text = plainText(n)
		}
		return []piece{p}
	}
	if n.mediaType != "multipart/alternative" {
		var out []piece
		for _, ch := range n.children {
			out = append(out, selectBody(ch, depth+1, skipped)...)
		}
		return out
	}
	type option struct {
		ps    []piece
		score int
	}
	var opts []option
	for _, ch := range n.children {
		ps := selectBody(ch, depth+1, skipped)
		if len(ps) == 0 {
			continue
		}
		opts = append(opts, option{ps: ps, score: score(ps)})
	}
	best := -1
	for i, o := range opts {
		if best < 0 || o.score > opts[best].score {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	if opts[best].score == scoreHTML {
		for _, o := range opts {
			if o.score == scorePlaceholder {
				*skipped = true
			}
		}
	}
	return opts[best].ps
}

const (
	scorePlaceholder = 1
	scoreHTML        = 2
	scorePlain       = 3
)

func score(ps []piece) int {
	allPlain := true
	for _, p := range ps {
		if p.n.mediaType != "text/plain" {
			allPlain = false
		}
	}
	if !allPlain {
		return scoreHTML
	}
	for _, p := range ps {
		// Content not yet fetched cannot be judged; take it as a body.
		if !p.n.hasData || !isPlaceholder(p.text) {
			return scorePlain
		}
	}
	return scorePlaceholder
}

func buildBody(root *node) Body {
	var b Body
	skipped := false
	pieces := selectBody(root, 0, &skipped)
	b.PlaceholderSkipped = skipped
	hidden := map[string]int{}
	var texts []string
	plain, htm := false, false
	for _, p := range pieces {
		if !p.n.hasData {
			// A part with no data, no size and no attachment id is empty
			// (or, under format=metadata, not asked for): not missing.
			if p.n.attachmentID != "" || p.n.size > 0 {
				b.PartIDs = append(b.PartIDs, p.n.partID)
				b.Missing = append(b.Missing, p.n.partID)
			}
			continue
		}
		b.PartIDs = append(b.PartIDs, p.n.partID)
		if unknown := p.n.unknownCharset(); unknown != "" {
			b.UnknownCharsets = append(b.UnknownCharsets, unknown)
		}
		if p.n.mediaType == "text/html" {
			htm = true
		} else {
			plain = true
		}
		t, links := bodyText(p.n, p.text, hidden)
		b.Links = append(b.Links, links...)
		if t = strings.TrimSpace(t); t != "" {
			texts = append(texts, t)
		}
	}
	switch {
	case plain && htm:
		b.Source = SourceBoth
	case htm:
		b.Source = SourceHTML
	case plain:
		b.Source = SourcePlain
	}
	b.Text = strings.Join(texts, "\n\n")
	b.Hidden = sortedHidden(hidden)
	b.Spans = FindSpans(b.Text)
	sort.Strings(b.UnknownCharsets)
	return b
}

// Ways read_attachment reads an attachment (ReadAs).
const (
	// ReadText is plain text, CSV, Markdown or JSON, read as written.
	ReadText = "text"
	// ReadHTML is HTML, converted to text as a body is.
	ReadHTML = "html"
	// ReadCalendar is an iCalendar file, read as written.
	ReadCalendar = "calendar"
	// ReadMessage is an attached message, parsed as a message.
	ReadMessage = "message"
)

// ReadAs is how an attachment of this media type is read as text, one
// of the Read* ways, or "" when it is not.
func ReadAs(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/plain", "text/csv", "text/markdown", "application/json":
		return ReadText
	case "text/html":
		return ReadHTML
	case "text/calendar", "application/ics":
		return ReadCalendar
	case "message/rfc822":
		return ReadMessage
	}
	return ""
}

// PartText reads the content of one part of a payload as a body part is
// read (§4.1): its charset decoded and the characters a reader would not
// see removed and counted. Plain text also has format=flowed undone and
// its quotes and signature found; HTML is converted, its hidden text
// dropped and its links read. Any other type keeps every line as
// written. data is the part's content, from the payload or fetched.
func PartText(p *gmail.MessagePart, data []byte) Body {
	n := payloadNode(p)
	n.data, n.hasData = data, true

	b := Body{PartIDs: []string{p.PartID}}
	if cs := n.unknownCharset(); cs != "" {
		b.UnknownCharsets = []string{cs}
	}
	hidden := map[string]int{}
	switch n.mediaType {
	case "text/html":
		t, links := bodyText(n, "", hidden)
		b.Text, b.Links, b.Source = strings.TrimSpace(t), links, SourceHTML
	case "text/plain":
		t, _ := bodyText(n, plainText(n), hidden)
		b.Text, b.Source = strings.TrimSpace(t), SourcePlain
	default:
		s, _ := decodeCharset(n.data, n.params["charset"])
		t, k := StripInvisible(normalizeNewlines(s))
		hidden[HiddenInvisible] += k
		b.Text = strings.TrimRight(t, "\n")
	}
	b.Hidden = sortedHidden(hidden)
	if b.Source != "" {
		b.Spans = FindSpans(b.Text)
	}
	return b
}

// bodyText is a text/plain or text/html part's text as a body part is
// read (§4.1): HTML converted, its hidden text dropped and its links
// read; plain text, already decoded by plainText, with invisible
// characters removed. What was removed is counted into hidden.
func bodyText(n *node, plain string, hidden map[string]int) (string, []Link) {
	if n.mediaType == "text/html" {
		h := HTMLToText([]byte(htmlText(n)))
		for _, c := range h.Hidden {
			hidden[c.Reason] += c.Chars
		}
		return h.Text, h.Links
	}
	t, k := StripInvisible(plain)
	hidden[HiddenInvisible] += k
	return t, nil
}

// plainText decodes a text/plain part: charset, line endings, and
// format=flowed (RFC 3676), whose soft line breaks would otherwise read
// as hard-wrapped paragraphs.
func plainText(n *node) string {
	s, _ := decodeCharset(n.data, n.params["charset"])
	s = normalizeNewlines(s)
	if strings.EqualFold(n.params["format"], "flowed") {
		s = unflow(s, strings.EqualFold(n.params["delsp"], "yes"))
	}
	return trimLineEnds(s)
}

// trimLineEnds drops trailing whitespace from every line except the
// signature delimiter "-- ", whose space is what makes it one.
func trimLineEnds(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "-- " {
			lines[i] = strings.TrimRight(l, " \t")
		}
	}
	return strings.Join(lines, "\n")
}

func htmlText(n *node) string {
	if cs := n.params["charset"]; cs != "" {
		s, _ := decodeCharset(n.data, cs)
		return s
	}
	_, name, _ := charset.DetermineEncoding(n.data, "text/html")
	s, _ := decodeCharset(n.data, name)
	return s
}

// unknownCharset returns the part's charset label when no table knows
// it. The label is the sender's: it is data, shown only inside a block.
func (n *node) unknownCharset() string {
	cs := normCharset(n.params["charset"])
	if cs == "" || isUTF8Label(cs) || isASCIILabel(cs) || lookupCharset(cs) != nil {
		return ""
	}
	return cs
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// unflow joins format=flowed soft line breaks. A line ending in a space
// continues on the next line of the same quote depth; space-stuffing is
// removed; the signature delimiter is never joined.
func unflow(s string, delsp bool) string {
	var out []string
	var cur strings.Builder
	curDepth, open := 0, false
	flush := func() {
		if open {
			out = append(out, strings.Repeat(">", curDepth)+prefixSpace(curDepth)+cur.String())
			cur.Reset()
			open = false
		}
	}
	for ln := range strings.SplitSeq(s, "\n") {
		depth := 0
		for depth < len(ln) && ln[depth] == '>' {
			depth++
		}
		body := ln[depth:]
		body = strings.TrimPrefix(body, " ") // space-stuffing
		if open && depth != curDepth {
			flush()
		}
		soft := strings.HasSuffix(body, " ") && body != "-- "
		if soft && delsp {
			body = body[:len(body)-1]
		}
		cur.WriteString(body)
		curDepth, open = depth, true
		if !soft {
			flush()
		}
	}
	flush()
	return strings.Join(out, "\n")
}

func prefixSpace(depth int) string {
	if depth > 0 {
		return " "
	}
	return ""
}
