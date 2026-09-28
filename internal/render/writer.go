package render

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/internal/mime"
	"github.com/mmedum/google-mail-mcp/internal/model"
)

// This file is the only place text becomes the server's voice.
//
// Outside a block the writer accepts a phrase and parts, nothing else.
// A phrase is a constant: an untyped string constant converts to it
// implicitly, a string variable does not compile. A part is made only by
// the constructors below, and each takes something a sender cannot
// shape — an int, a time, an id checked against Gmail's id shape, a
// label the account owns, or one of a fixed set of values. Text a sender
// wrote has one way out: block. So the next field that carries mail
// into a note is a compile error, not a review finding.
// TestServerVoiceIsTyped holds the rest: no file but this one converts
// to phrase or builds a part, or writes to the buffer directly.

// phrase is text the server wrote.
type phrase string

// part is one value in a statement of the server's.
type part struct{ s string }

// fragment is a piece rendered apart, to measure before it is used.
type fragment struct {
	s string
	n int
}

func (f fragment) len() int { return f.n }

// writer accumulates a rendering and remembers the untrusted text it
// wrapped, so a token that happens to occur in it can be replaced.
type writer struct {
	b         strings.Builder
	n         int // runes in b
	tok       string
	untrusted []string
	loc       *time.Location
}

func (w *writer) write(s string) {
	w.b.WriteString(s)
	w.n += utf8.RuneCountInString(s)
}

// text is what was written.
func (w *writer) text() string { return w.b.String() }

// len is the characters written so far.
func (w *writer) len() int { return w.n }

// say writes one line in the server's voice. Every verb in format is %s,
// filled by args in order.
func (w *writer) say(format phrase, args ...part) {
	w.write(fill(format, args...).s + "\n")
}

// fill makes a part of a line: a phrase with its %s verbs filled by
// args. What it returns is built only from a constant and parts.
func fill(format phrase, args ...part) part {
	if len(args) == 0 {
		return part{string(format)}
	}
	a := make([]any, len(args))
	for i, p := range args {
		a[i] = p.s
	}
	return part{fmt.Sprintf(string(format), a...)}
}

// blank writes an empty line.
func (w *writer) blank() { w.write("\n") }

// sub renders into a separate buffer with the same token, so a caller
// can measure a piece before committing it. The piece's untrusted text
// joins w's, whether or not the piece is used.
func (w *writer) sub(fn func(sub *writer)) fragment {
	s := &writer{tok: w.tok, loc: w.loc}
	fn(s)
	w.untrusted = append(w.untrusted, s.untrusted...)
	return fragment{s: s.b.String(), n: s.n}
}

// add writes a piece rendered with sub.
func (w *writer) add(f fragment) {
	w.b.WriteString(f.s)
	w.n += f.n
}

func (w *writer) leaks() bool {
	if w.tok == "" {
		return true
	}
	for _, u := range w.untrusted {
		if strings.Contains(u, w.tok) {
			return true
		}
	}
	return false
}

// block writes untrusted content inside a delimited block. what names
// the field; from and id name the origin, cleaned so they cannot break
// the opening line.
func (w *writer) block(what phrase, from, id, content string) {
	w.untrusted = append(w.untrusted, content, from)
	open := "<<<untrusted-mail " + w.tok + ": " + string(what)
	if from != "" {
		open += " from " + originText(from)
	}
	if id != "" {
		open += " in " + gmailID(id).s
	}
	w.write(open + ">>>\n")
	w.write(strings.TrimRight(content, "\n") + "\n")
	w.write("<<<end-untrusted-mail " + w.tok + ">>>\n")
}

// originText makes an address safe for the opening line: no spaces,
// brackets or line breaks, at most 100 characters.
func originText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, r == ' ', r == '<', r == '>', r == '"':
			return '_'
		}
		return r
	}, s)
	s, _ = mime.StripInvisible(s)
	if utf8.RuneCountInString(s) > 100 {
		s = string([]rune(s)[:100]) + "…"
	}
	return s
}

// num is a count or offset.
func num(n int) part { return part{strconv.Itoa(n)} }

// plural is a count with its noun.
func plural(n int, one, many phrase) part {
	if n == 1 {
		return part{strconv.Itoa(n) + " " + string(one)}
	}
	return part{strconv.Itoa(n) + " " + string(many)}
}

// when is Gmail's date, on Google's clock, in the caller's zone.
func (w *writer) when(t time.Time) part {
	if t.IsZero() {
		return part{"unknown date"}
	}
	return part{t.In(w.loc).Format("2006-01-02 15:04 MST")}
}

func yesNo(b bool) part {
	if b {
		return part{"yes"}
	}
	return part{"no"}
}

// idShape is every id and page token Gmail issues: hex message and
// thread ids, "r"-prefixed draft ids, Label_n and system label ids,
// decimal history ids and page tokens.
var idShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// gmailID is an id Gmail assigned. Anything not shaped like one — which
// Gmail never returns — is shown as a placeholder.
func gmailID(s string) part {
	if !idShape.MatchString(s) {
		return part{"(unreadable id)"}
	}
	return part{s}
}

// maxListed caps a list of ids in the server's voice, so a long thread
// or page cannot overrun the budget with its "not shown" list.
const maxListed = 30

// idList is ids joined with commas, at most maxListed of them.
func idList(ids []string) part {
	shown := make([]string, 0, min(len(ids), maxListed))
	for _, id := range ids[:min(len(ids), maxListed)] {
		shown = append(shown, gmailID(id).s)
	}
	s := strings.Join(shown, ", ")
	if len(ids) > maxListed {
		s += fmt.Sprintf(" and %d more", len(ids)-maxListed)
	}
	return part{s}
}

// labelName is a label's name. Label names appear outside the blocks
// because they are the account's own: the person or an app they
// authorized created them, and a sender can neither create a label nor
// name one — a filter only applies a label the account already has.
// Control and invisible characters are removed so a name is one line.
func labelName(s string) part { return part{oneLine(s, 225)} }

// oneLine is text made one line: invisible characters removed, controls
// made spaces, cut at max runes.
func oneLine(s string, max int) string {
	s, _ = mime.StripInvisible(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

// quoted is text from the mailbox or from a call's arguments, shown in a
// question put to the person (§4.13), where no block can go: a client
// draws the question as plain text in a dialog. It is made one line;
// every double or typographic quote mark becomes a plain single one, so
// it cannot close the quote it sits in or seem to; and a URL scheme, a
// mailto:, a leading "www." and a bare domain followed by a path are
// broken so the client draws no link. It is cut at max runes. The quotes
// mark it as quoted material, never the server's own words.
func quoted(s string, max int) part {
	s = strings.Join(strings.Fields(oneLine(s, max)), " ")
	s = quoteMarks.Replace(s)
	s = linkShape.ReplaceAllString(s, "$1[:]//")
	s = mailtoShape.ReplaceAllString(s, "${1}[:]")
	s = wwwShape.ReplaceAllString(s, "${1}[.]")
	s = pathShape.ReplaceAllString(s, "${1}[.]${2}/")
	return part{`"` + s + `"`}
}

var (
	// quoteMarks folds every quotation mark a reader could take for the
	// question's own to a plain single quote.
	quoteMarks = strings.NewReplacer(`"`, "'", "\u2018", "'", "\u2019", "'", "\u201a", "'", "\u201b", "'",
		"\u201c", "'", "\u201d", "'", "\u201e", "'", "\u201f", "'", "\u2032", "'", "\u2033", "'",
		"\u00ab", "'", "\u00bb", "'", "\u2039", "'", "\u203a", "'", "\u301d", "'", "\u301e", "'",
		"\u301f", "'", "\uff02", "'", "\uff07", "'")
	// linkShape is a URL scheme followed by //, as a client links it.
	linkShape = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://`)
	// mailtoShape is a mail link without //.
	mailtoShape = regexp.MustCompile(`(?i)\b(mailto):`)
	// wwwShape is a host a client links without a scheme.
	wwwShape = regexp.MustCompile(`(?i)\b(www)\.`)
	// pathShape is a bare domain followed by a path, x.example/..., which
	// a client links too; its last dot is broken.
	pathShape = regexp.MustCompile(`(?i)\b([a-z0-9-]+(?:\.[a-z0-9-]+)*)\.([a-z]{2,63})/`)
)

// labelList is a message's or thread's labels by name.
func labelList(ls []model.LabelRef) part {
	if len(ls) == 0 {
		return part{"none"}
	}
	names := make([]string, len(ls))
	for i, l := range ls {
		names[i] = labelName(l.Name).s
	}
	return part{strings.Join(names, ", ")}
}

// account is the signed-in address, from users.getProfile: the
// account's own, never a sender's.
func account(s string) part { return labelName(s) }

// setting is a value of the account's own configuration: a forwarding
// or send-as address, a display name, a filter's criteria. Like a label
// name, a sender cannot write one — only the person, or an app they
// authorized to change settings — so it stands outside the blocks,
// cleaned the same way. Free text a setting sends as mail (a vacation
// reply, a signature) is not a setting in this sense and goes in a
// block.
func setting(s string) part { return labelName(s) }

// oneOf is a value Google draws from a fixed set — a verification
// status, a disposition — shown as itself when it is one of known, and
// as "other" when Google answers something new.
func oneOf(s string, known ...phrase) part {
	for _, k := range known {
		if s == string(k) {
			return part{s}
		}
	}
	return part{"other"}
}

// sha is a SHA-256 in hex, computed by this server.
var shaShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

func sha(s string) part {
	if !shaShape.MatchString(s) {
		return part{"(unreadable hash)"}
	}
	return part{s}
}

// hiddenReasons are the reasons mime drops hidden text, all of them the
// server's own words.
var hiddenReasons = map[string]bool{
	mime.HiddenDisplayNone: true, mime.HiddenVisibility: true, mime.HiddenAttribute: true,
	mime.HiddenFontSize: true, mime.HiddenOpacity: true, mime.HiddenZeroBox: true,
	mime.HiddenSameColor: true, mime.HiddenTransparent: true, mime.HiddenStyleSheet: true,
	mime.HiddenComment: true, mime.HiddenInvisible: true,
}

// hiddenList is "reason count; reason count".
func hiddenList(hs []mime.HiddenCount) part {
	parts := make([]string, 0, len(hs))
	for _, h := range hs {
		reason := "other"
		if hiddenReasons[h.Reason] {
			reason = h.Reason
		}
		parts = append(parts, reason+" "+strconv.Itoa(h.Chars))
	}
	return part{strings.Join(parts, "; ")}
}

// addressHeaders are the headers mime reads addresses from.
var addressHeaders = map[string]bool{"From": true, "Sender": true, "To": true, "Cc": true, "Bcc": true, "Reply-To": true}

// headerNames names address headers; any other name, which mime never
// reports, is shown as a placeholder.
func headerNames(ns []string) part {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = "another header"
		if addressHeaders[n] {
			out[i] = n
		}
	}
	return part{strings.Join(out, ", ")}
}

// partIDShape is a MIME part id: dotted decimals.
var partIDShape = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,6}){0,20}$`)

// partIDs names MIME parts by id.
func partIDs(ids []string) part {
	out := make([]string, len(ids))
	for i, id := range ids {
		switch {
		case id == "":
			out[i] = "top-level"
		case partIDShape.MatchString(id):
			out[i] = id
		default:
			out[i] = "(unreadable part id)"
		}
	}
	return part{strings.Join(out, ", ")}
}

// colorShape is a label color as Gmail's palette writes it.
var colorShape = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func color(s string) part {
	if !colorShape.MatchString(s) {
		return part{"-"}
	}
	return part{s}
}

func labelType(s string) part {
	if s == "system" {
		return part{"system"}
	}
	return part{"user"}
}

// size is a byte count, as the attachment lines write it.
func size(n int64) part { return part{sizeText(int(n))} }

// fixed is a value drawn from a set this server defines — an outcome,
// a verb, a field name — shown as itself when it is one of known and as
// "other" otherwise, so no caller can pass other text through it.
func fixed(s string, known map[string]bool) part {
	if known[s] {
		return part{s}
	}
	return part{"other"}
}

// fixedList is values of a fixed set, comma-joined.
func fixedList(ss []string, known map[string]bool) part {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fixed(s, known).s
	}
	return part{strings.Join(out, ", ")}
}

// classShape is an error class of §6.5: lowercase words and underscores.
var classShape = regexp.MustCompile(`^[a-z_]{1,32}$`)

// failure is one item's error, "[class] message". The message is this
// server's own: it names ids and says what to do, and a text Google
// returned in it has its addresses masked by the client (gapi). No
// message a write fails with quotes mail, since a write reads only
// labels. It is cleaned to one line and capped.
func failure(class, message string) part {
	c := "unavailable"
	if classShape.MatchString(class) {
		c = class
	}
	return part{"[" + c + "] " + oneLine(message, 400)}
}

// recipientFields are the headers a recipient is addressed in.
var recipientFields = map[string]bool{"to": true, "cc": true, "bcc": true}

// recipientRef names a recipient by field and position, as "cc[1]":
// the name send_draft's guard gives it, which carries nothing a sender
// wrote.
func recipientRef(field string, i int) part {
	return part{fixed(field, recipientFields).s + "[" + strconv.Itoa(i) + "]"}
}

// partList is parts joined with commas.
func partList(ps []part) part {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.s
	}
	return part{strings.Join(out, ", ")}
}
