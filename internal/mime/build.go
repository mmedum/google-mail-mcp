package mime

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"mime/quotedprintable"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	netmail "net/mail"
)

// MaxRawBytes is the largest message drafts.create and drafts.update
// accept: the discovery document's mediaUpload.maxSize, 35 MB (§2.6).
const MaxRawBytes = 36700160

// Line lengths of RFC 5322 §2.1.1: a header line should stay within 78
// characters and must stay within 998. An encoded-word is at most 75
// (RFC 2047 §2).
const (
	foldAt       = 78
	maxLine      = 998
	maxWordChars = 75
)

// Outgoing is a message this server writes (§7.4). Addresses and text
// are as the caller gave them; Build does the encoding.
type Outgoing struct {
	// From is the sender, or nil to let Gmail fill in the default.
	From        *Address
	To, Cc, Bcc []Address
	Subject     string
	// Text is the plain-text body. It is sent exactly as given, with its
	// line endings written as CRLF, which RFC 5322 requires.
	Text string
	// HTML, when set, is sent beside Text as multipart/alternative.
	HTML        string
	Attachments []OutAttachment
	// MessageID is the Message-ID header, with its brackets. Required:
	// it is what a send is settled by (§4.3).
	MessageID string
	// InReplyTo and References thread a reply (§4.5).
	InReplyTo  string
	References []string
	// Date is the Date header; zero means now.
	Date time.Time
}

// OutAttachment is a file sent with a message.
type OutAttachment struct {
	// Filename is the name the recipient sees.
	Filename string
	// MediaType is its Content-Type; "" is application/octet-stream.
	// message/rfc822 is an attached message, which goes as written
	// (messageBody); every other type goes as base64.
	MediaType string
	Content   []byte
}

// ErrControl is text that would break a header: a line break or another
// control character in a subject or a display name.
var ErrControl = errors.New("a header value may not contain line breaks or control characters")

// ErrAttachedLines is an attached message that cannot go as it is: it
// has a line over 998 octets or a NUL. RFC 2046 §5.2.1 lets an attached
// message go only as 7bit, 8bit or binary, never encoded, and only
// binary carries such a line. SMTP carries binary only between servers
// that both offer it (RFC 3030), so this package does not write it.
var ErrAttachedLines = errors.New("an attached message has a line over 998 octets or a NUL octet, which only the binary encoding carries")

// Build writes an outgoing message as RFC 5322 bytes: headers encoded
// per RFC 2047, filenames per RFC 2231, text as quoted-printable UTF-8,
// attachments as base64 and an attached message as written, every line
// ending CRLF. What it writes, Parse reads back to the same fields
// (§4.10); the fuzz tests hold that.
func Build(o Outgoing) ([]byte, error) {
	if !ValidMessageID(o.MessageID) {
		return nil, fmt.Errorf("message id %q is not <local@domain>", o.MessageID)
	}
	var hs []rawHeader
	add := func(name, value string) {
		if value != "" {
			hs = append(hs, rawHeader{name: name, text: fold(name + ": " + value)})
		}
	}
	if o.From != nil {
		v, err := FormatAddresses([]Address{*o.From})
		if err != nil {
			return nil, fmt.Errorf("from: %w", err)
		}
		add("From", v)
	}
	for _, f := range []struct {
		name string
		list []Address
	}{{"To", o.To}, {"Cc", o.Cc}, {"Bcc", o.Bcc}} {
		v, err := FormatAddresses(f.list)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", strings.ToLower(f.name), err)
		}
		add(f.name, v)
	}
	subject, err := FormatText(o.Subject)
	if err != nil {
		return nil, fmt.Errorf("subject: %w", err)
	}
	add("Subject", subject)
	date := o.Date
	if date.IsZero() {
		date = time.Now()
	}
	add("Date", date.Format(time.RFC1123Z))
	add("Message-ID", o.MessageID)
	if o.InReplyTo != "" {
		if !ValidMessageID(o.InReplyTo) {
			return nil, fmt.Errorf("in-reply-to %q is not <local@domain>", o.InReplyTo)
		}
		add("In-Reply-To", o.InReplyTo)
	}
	for _, r := range o.References {
		if !ValidMessageID(r) {
			return nil, fmt.Errorf("references: %q is not <local@domain>", r)
		}
	}
	add("References", strings.Join(o.References, " "))
	add("MIME-Version", "1.0")

	root := bodyEntity(o.Text, o.HTML)
	if len(o.Attachments) > 0 {
		kids := []*entity{root}
		for _, a := range o.Attachments {
			att, err := attachmentEntity(a)
			if err != nil {
				return nil, err
			}
			kids = append(kids, att)
		}
		root = multipartEntity("mixed", kids...)
	}
	root.headers = append(hs, root.headers...)
	var b bytes.Buffer
	b.Grow(root.size())
	root.write(&b)
	if err := checkLines(b.Bytes()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// NewMessageID generates a Message-ID at the sender's domain, with its
// brackets.
func NewMessageID(sender string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	_, domain, _ := strings.Cut(sender, "@")
	domain = strings.ToLower(strings.TrimSpace(domain))
	if !dotAtom(domain) {
		domain = "google-mail-mcp.invalid"
	}
	return "<" + hex.EncodeToString(b[:]) + "@" + domain + ">"
}

// FormatText encodes free text for a header: as written when it is
// printable ASCII, otherwise as RFC 2047 encoded-words. Text holding
// "=?" is encoded too, so a reader cannot take it for a word.
func FormatText(s string) (string, error) {
	if HasControl(s) {
		return "", ErrControl
	}
	if !needsEncoding(s) {
		return s, nil
	}
	return encodeWords(s), nil
}

// FormatAddresses writes an address list: "Name <a@b>" or a bare
// address, names encoded or quoted as needed.
func FormatAddresses(as []Address) (string, error) {
	out := make([]string, 0, len(as))
	for _, a := range as {
		s, err := formatAddress(a)
		if err != nil {
			return "", err
		}
		out = append(out, s)
	}
	return strings.Join(out, ", "), nil
}

func formatAddress(a Address) (string, error) {
	if !validEmail(a.Email) {
		return "", fmt.Errorf("%q is not an email address this server can write: it must be ASCII local@domain", a.Email)
	}
	if HasControl(a.Name) {
		return "", ErrControl
	}
	switch {
	case a.Name == "":
		return a.Email, nil
	case needsEncoding(a.Name):
		return encodeWords(a.Name) + " <" + a.Email + ">", nil
	case isPhrase(a.Name):
		return a.Name + " <" + a.Email + ">", nil
	default:
		return quotedString(a.Name) + " <" + a.Email + ">", nil
	}
}

// validEmail accepts what net/mail parses as exactly one bare ASCII
// address. Internationalized addresses are refused rather than written
// raw into a header that must be ASCII.
func validEmail(s string) bool {
	if s == "" || strings.ContainsAny(s, " <>\"(),;:\\[]") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	a, err := netmail.ParseAddress(s)
	return err == nil && a.Address == s
}

// isPhrase reports a display name made only of atoms and single spaces,
// which needs no quoting and which net/mail reads back unchanged. A
// character beyond ASCII is atext, as RFC 6532 and net/mail read it; the
// header writer encodes such a name before it asks. "=?" would be read
// as the start of an encoded-word.
func isPhrase(s string) bool {
	if s != strings.TrimSpace(s) || strings.Contains(s, "  ") || strings.Contains(s, "=?") {
		return false
	}
	for _, r := range s {
		if r != ' ' && !isAtext(r) {
			return false
		}
	}
	return true
}

func isAtext(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r >= utf8.RuneSelf:
		return true
	}
	return strings.ContainsRune("!#$%&'*+-/=?^_`{|}~", r)
}

// HasControl reports a character that would break a header or a line:
// C0 and C1 controls and DEL, tab included.
func HasControl(s string) bool { return strings.IndexFunc(s, isControl) >= 0 }

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }

func needsEncoding(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7e {
			return true
		}
	}
	// A run with no space to fold at is encoded, since encoded-words can
	// be split where the run cannot, and a line must stay under 998.
	for w := range strings.FieldsSeq(s) {
		if len(w) > maxWordChars {
			return true
		}
	}
	return strings.Contains(s, "=?")
}

// encodeWords writes s as B encoded-words of at most 75 characters,
// split between characters and separated by a space, which a reader
// drops between two words (RFC 2047 §6.2).
func encodeWords(s string) string {
	const open, close = "=?utf-8?b?", "?="
	// Base64 of n bytes takes 4*ceil(n/3) characters.
	maxBytes := (maxWordChars - len(open) - len(close)) / 4 * 3
	var words []string
	for s != "" {
		n := 0
		for n < len(s) {
			_, size := utf8.DecodeRuneInString(s[n:])
			if n+size > maxBytes {
				break
			}
			n += size
		}
		words = append(words, open+base64.StdEncoding.EncodeToString([]byte(s[:n]))+close)
		s = s[n:]
	}
	return strings.Join(words, " ")
}

// fold breaks a header line at spaces so no line exceeds 78 characters
// where a space allows it. Folding inserts CRLF before a space, and
// unfolding removes only the CRLF, so the value reads back unchanged.
func fold(line string) string {
	if len(line) <= foldAt {
		return line
	}
	var b strings.Builder
	start := 0
	for len(line)-start > foldAt {
		// The last space that ends a line of at most foldAt, after the
		// line's first character (a continuation starts with its space).
		cut := strings.LastIndexByte(line[start+1:start+foldAt+1], ' ')
		if cut < 0 {
			next := strings.IndexByte(line[start+1:], ' ')
			if next < 0 {
				break
			}
			cut = next
		}
		cut += start + 1
		b.WriteString(line[start:cut] + "\r\n")
		start = cut
	}
	b.WriteString(line[start:])
	return b.String()
}

// checkLines refuses a message with a line over 998 characters, which
// RFC 5322 forbids and a relay may break.
func checkLines(b []byte) error {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			i = len(b)
		}
		if n := len(bytes.TrimRight(b[:i], "\r")); n > maxLine {
			return fmt.Errorf("a line of %d characters is over the %d RFC 5322 allows", n, maxLine)
		}
		b = b[min(i+1, len(b)):]
	}
	return nil
}

// msgIDChar is a character a Message-ID's local part or domain may hold
// here: printable ASCII other than brackets, "@" and space.
func msgIDChar(r rune) bool { return r > 0x20 && r < 0x7f && !strings.ContainsRune("<>@ ", r) }

// ValidMessageID accepts "<local@domain>" with ASCII parts.
func ValidMessageID(s string) bool {
	inner, ok := strings.CutPrefix(s, "<")
	if !ok {
		return false
	}
	inner, ok = strings.CutSuffix(inner, ">")
	if !ok {
		return false
	}
	local, domain, ok := strings.Cut(inner, "@")
	if !ok || local == "" || domain == "" || len(s) > maxLine/2 {
		return false
	}
	bad := func(r rune) bool { return !msgIDChar(r) }
	return strings.IndexFunc(local, bad) < 0 && strings.IndexFunc(domain, bad) < 0
}

func dotAtom(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if r != '.' && !isAtext(r) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- parts

// textEntity is a text part: UTF-8, quoted-printable, line endings made
// CRLF. Quoted-printable is used for every text part, ASCII or not, so
// no body line can ever match a boundary (see newBoundary).
func textEntity(subtype, text string) *entity {
	text = normalizeNewlines(text)
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(text))
	_ = w.Close()
	return &entity{
		mediaType: "text/" + subtype,
		headers: []rawHeader{
			{name: "Content-Type", text: "Content-Type: text/" + subtype + "; charset=\"utf-8\""},
			{name: "Content-Transfer-Encoding", text: "Content-Transfer-Encoding: quoted-printable"},
		},
		body: b.Bytes(),
	}
}

// HTMLFromText is the HTML version of a plain-text body: the same words,
// escaped, a paragraph per block of lines between blank lines, and a
// line break for each line break inside one, each line as LineHTML
// writes it.
// It is "" for text with nothing to show. Gmail's web composer opens a
// draft with no HTML part as plain text and wraps it at about 70
// columns when the person sends it; an HTML part keeps each paragraph
// whole (§7.4, §18 row 73). EditRaw recognizes what this writes by
// reading it back (madeText), so any markup it ever writes must stay
// among what madeText accepts.
func HTMLFromText(text string) string {
	var b strings.Builder
	var para []string
	// The empty line added at the end closes the last paragraph.
	for l := range strings.SplitSeq(normalizeNewlines(text)+"\n", "\n") {
		if l = strings.TrimRight(l, " \t"); l != "" {
			para = append(para, LineHTML(l))
		} else if len(para) > 0 {
			b.WriteString("<p>" + strings.Join(para, "<br>\n") + "</p>\n")
			para = nil
		}
	}
	return b.String()
}

// LineHTML is one line of plain text as HTML: escaped, a tab as four
// spaces, runs of spaces kept, which HTML would collapse, and each web
// address a link whose text is the address itself (nextLink). A line
// holding a bidi control gets no link: an override anywhere before an
// address can draw its text as another address than its target.
func LineHTML(l string) string {
	l = strings.ReplaceAll(l, "\t", "    ")
	var b strings.Builder
	prev := ' ' // a space that starts the line is kept too
	if strings.ContainsFunc(l, func(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) }) {
		writeText(&b, l, prev)
		return b.String()
	}
	for l != "" {
		start, end := nextLink(l)
		writeText(&b, l[:start], prev)
		if start == len(l) {
			break
		}
		u := html.EscapeString(l[start:end])
		b.WriteString(`<a href="` + u + `">` + u + `</a>`)
		prev, l = 'a', l[end:]
	}
	return b.String()
}

// writeText writes s escaped, with a space that follows another space as
// a no-break space; prev is the character before s.
func writeText(b *strings.Builder, s string, prev rune) {
	for _, r := range html.EscapeString(s) { // escaping adds no spaces
		if r == ' ' && prev == ' ' {
			b.WriteString("&nbsp;")
		} else {
			b.WriteRune(r)
		}
		prev = r
	}
}

// linkStart finds where a web address may begin, as GitHub's autolinks
// allow: at the start of a line, after a space, or after one of ( [ < "
// ' * _ ~. Only http and https are linked (§18 row 75).
var linkStart = regexp.MustCompile(`(?i)(^|[\s\p{Zs}(\[<"'*_~])(https?://)[\pL\pN]`)

// nextLink returns the first web address in l as l[start:end], or
// start == end == len(l) when there is none. An address runs to a space,
// a control or bidi formatting character, '<', '>' or '"', and its end
// is trimmed as trimLink says.
func nextLink(l string) (start, end int) {
	m := linkStart.FindStringSubmatchIndex(l)
	if m == nil {
		return len(l), len(l)
	}
	start, end = m[4], len(l)
	if n := strings.IndexFunc(l[start:], endsLink); n >= 0 {
		end = start + n
	}
	return start, start + trimLink(l[start:end])
}

// endsLink reports whether r ends a web address: RFC 3986 Appendix C
// leaves quotes, angle brackets and space outside one, and a character a
// reader cannot see, a bidi control among them, could make a link read
// as another (§4.1.2).
func endsLink(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`<>"`, r) || isInvisible(r)
}

// trimLink returns the length of u without what GitHub's autolinks leave
// out at its end: trailing punctuation, a closing parenthesis or bracket
// with no opening one, and an entity or a semicolon. It counts the
// brackets once, so a long run of them costs one pass.
func trimLink(u string) int {
	opened := map[byte]int{')': strings.Count(u, "("), ']': strings.Count(u, "[")}
	closed := map[byte]int{')': strings.Count(u, ")"), ']': strings.Count(u, "]")}
	n := len(u)
	for {
		switch last := u[n-1]; {
		case strings.IndexByte("?!.,:*_~'", last) >= 0:
			n--
		case (last == ')' || last == ']') && closed[last] > opened[last]:
			closed[last]--
			n--
		case last == ';':
			n = entityStart(u[:n])
		default:
			return n
		}
	}
}

// entityStart returns where an entity such as &amp; ends u, or where its
// final semicolon is when no entity does.
func entityStart(u string) int {
	i := len(u) - 1 // the semicolon
	for i > 0 && isASCIIAlnum(u[i-1]) {
		i--
	}
	if i < len(u)-1 && i > 0 && u[i-1] == '&' {
		return i - 1
	}
	return len(u) - 1
}

func isASCIIAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// bodyEntity is the plain part, or plain and HTML as alternatives.
func bodyEntity(text, html string) *entity {
	plain := textEntity("plain", text)
	if html == "" {
		return plain
	}
	return multipartEntity("alternative", plain, textEntity("html", html))
}

func multipartEntity(subtype string, children ...*entity) *entity {
	boundary := newBoundary()
	// An attached message goes as written, so a line of it could be the
	// boundary; one that holds it gets another.
	for holds(children, "--"+boundary) {
		boundary = newBoundary()
	}
	return &entity{
		mediaType: "multipart/" + subtype,
		boundary:  boundary,
		headers: []rawHeader{{name: "Content-Type",
			text: "Content-Type: multipart/" + subtype + "; boundary=\"" + boundary + "\""}},
		children: children,
		dirty:    true,
	}
}

// holds reports whether any of es writes delim in a header or a body.
func holds(es []*entity, delim string) bool {
	for _, e := range es {
		for _, h := range e.headers {
			if strings.Contains(h.text, delim) {
				return true
			}
		}
		if e.dirty {
			if holds(e.children, delim) {
				return true
			}
		} else if bytes.Contains(e.body, []byte(delim)) {
			return true
		}
	}
	return false
}

// attachmentEntity is a file: base64 in lines of 76, named in both
// Content-Disposition (RFC 2231, the standard) and Content-Type (RFC
// 2047 in quotes, which older clients read instead). An attached message
// is named the same way and goes as messageBody writes it.
func attachmentEntity(a OutAttachment) (*entity, error) {
	name := a.Filename
	if !ValidFilename(name) {
		return nil, fmt.Errorf("attachment name %q is not a base name", name)
	}
	mt := a.MediaType
	if mt == "" {
		mt = "application/octet-stream"
	}
	if !mediaTypeShape(mt) {
		return nil, fmt.Errorf("media type %q is not type/subtype", mt)
	}
	nameParam := `"` + name + `"`
	if !quotable(name) {
		nameParam = `"` + encodeWords(name) + `"`
	}
	cte, body := "base64", []byte(nil)
	if strings.EqualFold(mt, "message/rfc822") {
		var err error
		if cte, body, err = messageBody(a.Content); err != nil {
			return nil, err
		}
	} else {
		body = base64Lines(a.Content)
	}
	return &entity{
		mediaType:   mt,
		disposition: "attachment",
		named:       true,
		headers: []rawHeader{
			{name: "Content-Type", text: fold("Content-Type: " + mt + "; name=" + nameParam)},
			{name: "Content-Disposition", text: fold("Content-Disposition: attachment; " + filenameParam(name))},
			{name: "Content-Transfer-Encoding", text: "Content-Transfer-Encoding: " + cte},
		},
		body: body,
	}, nil
}

// messageBody is an attached message as RFC 2046 §5.2.1 lets it go: as
// written, with its line endings made CRLF, and declared 7bit when every
// octet is ASCII, else 8bit. A message with a line over 998 octets or a
// NUL is ErrAttachedLines (RFC 2045 §2.7, §2.8).
func messageBody(b []byte) (cte string, body []byte, err error) {
	body = withCRLF(b)
	for line := range bytes.SplitSeq(body, []byte("\r\n")) {
		if len(line) > maxLine || bytes.IndexByte(line, 0) >= 0 {
			return "", nil, ErrAttachedLines
		}
	}
	for _, c := range body {
		if c > 0x7f {
			return "8bit", body, nil
		}
	}
	return "7bit", body, nil
}

// withCRLF writes every line ending as CRLF, which RFC 5322 requires:
// a bare LF or a bare CR becomes one.
func withCRLF(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/64)
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '\r':
			out = append(out, '\r', '\n')
			if i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
		case '\n':
			out = append(out, '\r', '\n')
		default:
			out = append(out, b[i])
		}
	}
	return out
}

// ForwardCopy is a message as a forward attaches it (§7.4): its line
// endings made CRLF, and its Bcc and Resent-Bcc headers left out. The
// sender's own copy of a sent message keeps its Bcc, and a forward would
// show the blind recipients to everyone it reaches. bccRemoved reports
// whether one was left out; every other byte is as given.
func ForwardCopy(raw []byte) (out []byte, bccRemoved bool) {
	out = withCRLF(raw)
	hs, body := splitRawHeaders(out)
	var kept []rawHeader
	for _, h := range hs {
		// RFC 5322 §4.5 lets a reader take "Bcc :" for Bcc too.
		name, _, _ := strings.Cut(h.text, ":")
		if name = strings.TrimRight(name, " \t"); strings.EqualFold(name, "Bcc") || strings.EqualFold(name, "Resent-Bcc") {
			bccRemoved = true
			continue
		}
		kept = append(kept, h)
	}
	if !bccRemoved {
		return out, false
	}
	var b bytes.Buffer
	b.Grow(len(out))
	for _, h := range kept {
		b.WriteString(h.text + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes(), true
}

// base64Lines encodes b as base64 in lines of 76, straight into a buffer
// of the final size: an attachment can be most of 35 MB.
func base64Lines(b []byte) []byte {
	const perLine = 57 // input bytes per 76-character line
	n := base64.StdEncoding.EncodedLen(len(b))
	out := make([]byte, 0, n+n/76*2)
	for len(b) > 0 {
		chunk := b[:min(perLine, len(b))]
		b = b[len(chunk):]
		out = base64.StdEncoding.AppendEncode(out, chunk)
		if len(b) > 0 {
			out = append(out, '\r', '\n')
		}
	}
	return out
}

// ValidFilename accepts a name an attachment can carry: a base name with
// no separator, control character or surrounding space, and not "." or
// "..".
func ValidFilename(name string) bool {
	return name != "" && name != "." && name != ".." && name == strings.TrimSpace(name) && !HasControl(name) &&
		path.Base(name) == name && !strings.ContainsAny(name, `/\`)
}

// quotable is a name that can stand in a quoted-string as it is: short
// printable ASCII without quotes, backslashes or "=?".
func quotable(s string) bool {
	if len(s) > 60 || strings.Contains(s, "=?") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e || s[i] == '"' || s[i] == '\\' {
			return false
		}
	}
	return true
}

// filenameParam is filename="…" for a quotable name, otherwise RFC 2231
// filename*0*=utf-8”…; filename*1*=… in segments short enough to fold.
func filenameParam(name string) string {
	if quotable(name) {
		return `filename="` + name + `"`
	}
	var enc strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isAttrChar(c) {
			enc.WriteByte(c)
			continue
		}
		fmt.Fprintf(&enc, "%%%02X", c)
	}
	s := enc.String()
	var segs []string
	for i := 0; s != ""; i++ {
		n := min(len(s), 54)
		// Never split a %XX escape.
		if j := strings.LastIndexByte(s[max(n-2, 0):n], '%'); j >= 0 && n < len(s) {
			n = max(n-2, 0) + j
		}
		prefix := "filename*" + strconv.Itoa(i) + "*="
		if i == 0 {
			prefix += "utf-8''"
		}
		segs = append(segs, prefix+s[:n])
		s = s[n:]
	}
	return strings.Join(segs, "; ")
}

// isAttrChar is RFC 2231's attribute-char, which needs no escape.
func isAttrChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}

func mediaTypeShape(s string) bool {
	t, sub, ok := strings.Cut(s, "/")
	token := func(x string) bool {
		if x == "" {
			return false
		}
		for i := 0; i < len(x); i++ {
			if x[i] <= 0x20 || x[i] >= 0x7f || strings.IndexByte(`()<>@,;:\"/[]?=`, x[i]) >= 0 {
				return false
			}
		}
		return true
	}
	return ok && token(t) && token(sub)
}

// mediaTypes names a file's type by its extension. The table is fixed
// rather than the system's, so a draft is built the same on every
// machine.
var mediaTypes = map[string]string{
	".pdf": "application/pdf", ".zip": "application/zip", ".json": "application/json",
	".ics": "text/calendar", ".txt": "text/plain", ".csv": "text/csv", ".md": "text/markdown",
	".html": "text/html", ".htm": "text/html", ".xml": "application/xml",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".svg": "image/svg+xml", ".heic": "image/heic",
	".mp3": "audio/mpeg", ".mp4": "video/mp4", ".mov": "video/quicktime",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
}

// MediaTypeFor is the media type of a file name, or
// application/octet-stream.
func MediaTypeFor(name string) string {
	if t, ok := mediaTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}
