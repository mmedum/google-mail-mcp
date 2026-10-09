package render

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
)

// piece is a run of rendered body text and where it came from.
type piece struct {
	text     string
	srcStart int // byte offset in Body.Text
	srcEnd   int
	marker   bool // a collapse marker, not body text
}

// collapsed says what a body view folded away.
type collapsed struct {
	quotes, quoteLines int
	sigs, sigLines     int
}

// bodyPieces lays out a body from a byte offset, folding trailing
// quotes and signatures into one-line markers unless show is set. An
// inline quote — one the reply answers between its lines — stays.
func bodyPieces(b mime.Body, start int, show bool) ([]piece, collapsed) {
	var c collapsed
	text := b.Text
	var spans []mime.Span
	if !show {
		for _, s := range b.Spans {
			if s.Kind == mime.SpanSignature || s.Trailing {
				spans = append(spans, s)
			}
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	var out []piece
	pos := start
	for _, s := range spans {
		if s.End <= pos {
			continue
		}
		if s.Start > pos {
			out = append(out, piece{text: text[pos:s.Start], srcStart: pos, srcEnd: s.Start})
		}
		lines := s.Lines
		if s.Start < pos {
			lines = strings.Count(text[pos:s.End], "\n") + 1
		}
		var m string
		if s.Kind == mime.SpanQuote {
			c.quotes++
			c.quoteLines += lines
			m = "[… " + plural(lines, "line", "lines").s + " of quoted text collapsed …]"
		} else {
			c.sigs++
			c.sigLines += lines
			m = "[… " + plural(lines, "line", "lines").s + " of signature collapsed …]"
		}
		out = append(out, piece{text: m, srcStart: max(s.Start, pos), srcEnd: s.End, marker: true})
		pos = s.End
	}
	if pos < len(text) {
		out = append(out, piece{text: text[pos:], srcStart: pos, srcEnd: len(text)})
	}
	return out, c
}

// cutBody fits pieces into limit characters. It cuts inside a text
// piece at a paragraph boundary, else a line, else a space, and returns
// the byte offset in Body.Text to continue from (0 when all fit).
func cutBody(ps []piece, limit int) (string, int) {
	var b strings.Builder
	used := 0
	for _, p := range ps {
		n := utf8.RuneCountInString(p.text)
		if used+n <= limit {
			b.WriteString(p.text)
			used += n
			continue
		}
		if p.marker {
			return b.String(), p.srcStart
		}
		room := limit - used
		cut := cutPoint(p.text, room)
		if cut == 0 && b.Len() == 0 {
			// No boundary fits and nothing is shown yet: cut mid-text
			// rather than show nothing.
			cut = byteIndexOfRune(p.text, max(room, 1))
		}
		b.WriteString(p.text[:cut])
		return b.String(), p.srcStart + cut
	}
	return b.String(), 0
}

// cutPoint is the byte index of the best boundary within room runes: a
// paragraph break, else a line break, else a space — preferring any of
// them in the second half of the window over a coarser one far back.
func cutPoint(s string, room int) int {
	if room <= 0 {
		return 0
	}
	window := s[:byteIndexOfRune(s, room)]
	for _, floor := range []int{len(window) / 2, 1} {
		for _, sep := range []string{"\n\n", "\n", " "} {
			if i := strings.LastIndex(window, sep); i >= floor {
				return i + len(sep)
			}
		}
	}
	return 0
}

func byteIndexOfRune(s string, n int) int {
	i := 0
	for n > 0 && i < len(s) {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n--
	}
	return i
}

func runeOffset(s string, byteOff int) int {
	return utf8.RuneCountInString(s[:min(byteOff, len(s))])
}

// headerBlock is the untrusted header text of a message.
func headerBlock(m model.Message, all bool) string {
	var b strings.Builder
	add := func(name string, v model.Untrusted) {
		if v != "" {
			b.WriteString(name + ": " + string(v) + "\n")
		}
	}
	if all {
		for _, h := range m.Headers {
			add(h.Name, model.Untrusted(mime.DecodeHeader(h.Value)))
		}
	} else {
		add("From", addrList(m.From))
		add("To", addrList(m.To))
		add("Cc", addrList(m.Cc))
		add("Bcc", addrList(m.Bcc))
		add("Reply-To", addrList(m.ReplyTo))
		add("Date", m.DateHeader)
		add("Subject", m.Subject)
		add("Message-ID", m.RFC822MessageID)
		add("In-Reply-To", joinUntrusted(m.InReplyTo, " "))
		add("List-Unsubscribe", m.ListUnsubscribe)
	}
	for i, a := range m.Attachments {
		b.WriteString("Attachment: " + attachmentLine(a) + "\n")
		if a.Invitation != nil {
			b.WriteString("  Invitation: " + invitationLine(*a.Invitation, m.Attachments[:i]) + "\n")
		}
	}
	return b.String()
}

// invitationLine is what a calendar part says about its event, or which
// earlier part says the same: a message often carries one invitation
// twice, inline and as invite.ics.
func invitationLine(inv mime.Invitation, before []mime.Attachment) string {
	for _, p := range before {
		if p.Invitation != nil && *p.Invitation == inv {
			return `the same as part_id "` + p.PartID + `"`
		}
	}
	var out []string
	if inv.Summary != "" {
		out = append(out, `"`+inv.Summary+`"`)
	}
	if when := inv.Start; when != "" {
		if inv.End != "" {
			when += " to " + inv.End
		}
		if inv.AllDay {
			when = "all day " + when
		}
		if inv.TimeZone != "" {
			when += " (" + inv.TimeZone + ")"
		}
		out = append(out, when)
	}
	if inv.RecurrenceID != "" {
		out = append(out, "one occurrence, originally at "+inv.RecurrenceID)
	}
	if inv.Organizer != "" {
		out = append(out, "organizer "+inv.Organizer)
	}
	if inv.UID != "" {
		out = append(out, "uid "+inv.UID)
	}
	out = append(out, fmt.Sprintf("sequence %d", inv.Sequence))
	if inv.Events == 1 {
		out = append(out, "1 event")
	} else {
		out = append(out, fmt.Sprintf("%d events", inv.Events))
	}
	return strings.Join(out, " · ")
}

// capHeaders keeps a header block to at most limit characters, cut at a
// line, and returns how many characters it left out. A message can carry
// headers larger than any budget (§17a); the body still gets its share.
func capHeaders(text string, limit int) (string, int) {
	total := utf8.RuneCountInString(text)
	if total <= limit {
		return text, 0
	}
	kept := text[:byteIndexOfRune(text, limit)]
	if i := strings.LastIndexByte(kept, '\n'); i > 0 {
		kept = kept[:i+1]
	}
	return kept, total - utf8.RuneCountInString(kept)
}

func addrList(as []mime.Address) model.Untrusted {
	return joinUntrusted(model.UntrustedAddresses(as), ", ")
}

func joinUntrusted(us []model.Untrusted, sep string) model.Untrusted {
	ss := make([]string, len(us))
	for i, u := range us {
		ss[i] = string(u)
	}
	return model.Untrusted(strings.Join(ss, sep))
}

func attachmentLine(a mime.Attachment) string {
	var details []string
	if a.MimeType != "" {
		details = append(details, a.MimeType)
	}
	details = append(details, sizeText(a.Size))
	if a.Inline {
		details = append(details, "inline")
	}
	if a.CalendarMethod != "" {
		details = append(details, "invitation method "+a.CalendarMethod)
	}
	if a.Renamed {
		details = append(details, `declared name "`+a.DeclaredName+`"`)
	}
	details = append(details, `part_id "`+a.PartID+`"`)
	return a.Filename + " (" + strings.Join(details, ", ") + ")"
}

func sizeText(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// messageLine is the server's own line about a message: ids, Gmail's
// date and labels, none of which the sender controls.
// A thread's message line also gives its position, pos of n.
func (w *writer) messageLine(m model.Message, pos, n int) {
	if n > 0 {
		w.say("── %s of %s · message %s · thread %s · %s · labels: %s%s", num(pos), num(n),
			gmailID(m.ID), gmailID(m.ThreadID), w.when(m.Date), labelList(m.Labels), unsubscribeWays(m.Unsubscribe))
		return
	}
	w.say("message %s · thread %s · %s · labels: %s%s", gmailID(m.ID), gmailID(m.ThreadID), w.when(m.Date),
		labelList(m.Labels), unsubscribeWays(m.Unsubscribe))
}

// draftLine heads one draft in a thread's drafts section.
func (w *writer) draftLine(m model.Message, pos, n int) {
	w.say("── draft %s of %s · message %s · thread %s · %s · labels: %s", num(pos), num(n),
		gmailID(m.ID), gmailID(m.ThreadID), w.when(m.Date), labelList(m.Labels))
}

// notes states, outside the blocks, what the server found and did. What
// it found in the sender's words — a link's hosts, a charset's label —
// goes in a block of its own after the note that counts it.
func (w *writer) notes(m model.Message, c collapsed) {
	origin := m.Sender().Email
	if n := m.Body.HiddenChars(); n > 0 {
		w.say("note: %s a reader would not see were removed from the body (%s).",
			plural(n, "character", "characters"), hiddenList(m.Body.Hidden))
	}
	if m.HeaderHidden > 0 {
		w.say("note: %s were removed from the subject, names and addresses.",
			plural(m.HeaderHidden, "invisible character", "invisible characters"))
	}
	if links := m.Body.Mismatches(); len(links) > 0 {
		w.say("note: %s whose text names a different site from the one it points to; the block below pairs each.",
			plural(len(links), "link", "links"))
		var b strings.Builder
		for _, l := range links {
			b.WriteString("text names " + l.TextHost + ", link points to " + l.Host + "\n")
		}
		w.block("links whose text names another site", origin, m.ID, b.String())
	}
	if m.Body.PlaceholderSkipped {
		w.say("note: the plain-text part only pointed to the HTML version, so the HTML was read.")
	}
	if m.Body.Source == mime.SourceHTML || m.Body.Source == mime.SourceBoth {
		w.say("note: the body was converted from HTML; no image or link was fetched.")
	}
	for _, a := range m.Attachments {
		if a.Renamed {
			w.say("note: an attachment's declared name was unsafe as a file name; the header block shows it renamed.")
		}
	}
	if len(m.LenientHeaders) > 0 {
		w.say("note: malformed address headers were read leniently: %s.", headerNames(m.LenientHeaders))
	}
	if cs := m.Body.UnknownCharsets; len(cs) > 0 {
		w.say("note: unknown charsets were read as UTF-8 or windows-1252 in %s; the block below names them.",
			plural(len(cs), "body part", "body parts"))
		w.block("unknown charset labels", origin, m.ID, strings.Join(cs, "\n"))
	}
	if len(m.Body.Missing) > 0 {
		w.say("note: body parts not fetched: %s.", partIDs(m.Body.Missing))
	}
	switch {
	case c.quotes > 0 && c.sigs > 0:
		w.say("collapsed: %s of quoted text and a signature (%s); show_quoted=true shows them.",
			plural(c.quoteLines, "line", "lines"), plural(c.sigLines, "line", "lines"))
	case c.quotes > 0:
		w.say("collapsed: %s of quoted text; show_quoted=true shows them.", plural(c.quoteLines, "line", "lines"))
	case c.sigs > 0:
		w.say("collapsed: a signature (%s); show_quoted=true shows them.", plural(c.sigLines, "line", "lines"))
	}
}

// minBody is the least body a read shows, even when its frame alone
// fills the budget.
const minBody = 200

// messageBody writes one message's header and body blocks, starting the
// body at a rune offset and ending the whole by the writer position end.
// It returns the rune offset to continue from, or 0.
//
// The frame — the header block, notes and cut line — is rendered first
// with the widest cut line it could need, and the body gets exactly what
// is left.
func (w *writer) messageBody(m model.Message, o Options, startRune, end int) int {
	headers, cut := capHeaders(headerBlock(m, o.AllHeaders), o.budget()/2)
	w.block("headers", m.Sender().Email, m.ID, headers)
	if cut > 0 {
		w.say("cut: %s of the header block over half the budget were left out; the structured result lists every address.",
			plural(cut, "character", "characters"))
	}
	if !m.Complete {
		w.say("(headers only: this read did not include the body)")
		return 0
	}
	if m.Body.Text == "" {
		w.say("(no readable body)")
		w.notes(m, collapsed{})
		return 0
	}
	total := utf8.RuneCountInString(m.Body.Text)
	startRune = min(startRune, total)
	ps, c := bodyPieces(m.Body, byteIndexOfRune(m.Body.Text, startRune), o.ShowQuoted)
	frame := w.sub(func(s *writer) { s.bodyFrame(m, c, startRune, total, "", total) })
	text, next := cutBody(ps, max(end-w.len()-frame.len(), minBody))
	at := 0
	if next > 0 {
		at = runeOffset(m.Body.Text, next)
	}
	w.bodyFrame(m, c, startRune, total, text, at)
	return at
}

// bodyFrame writes a body block and what the server says about it; at
// is where a cut body continues, or 0.
func (w *writer) bodyFrame(m model.Message, c collapsed, startRune, total int, text string, at int) {
	if startRune > 0 {
		w.say("(body from character %s of %s)", num(startRune), num(total))
	}
	w.block("body", m.Sender().Email, m.ID, text)
	w.notes(m, c)
	if at > 0 {
		w.say("cut: the body continues at character %s of %s; offset=%s reads on.", num(at), num(total), num(at))
	}
}

// Message renders one message under the budget, starting its body at
// o.Offset.
func Message(m model.Message, o Options) Result {
	return single(o, m, func(w *writer) { w.messageLine(m, 0, 0) })
}

// Draft renders one draft: both ids, then the message as get_message
// renders it. A draft's text is treated as untrusted too: it may have
// been shaped by someone other than the person reading it (§4.2).
func Draft(d model.Draft, o Options) Result {
	m := d.Message
	return single(o, m, func(w *writer) {
		w.say("draft %s · message %s · thread %s · %s", gmailID(d.ID), gmailID(m.ID), gmailID(m.ThreadID), w.when(m.Date))
	})
}

// single renders one message under a head line.
func single(o Options, m model.Message, head func(w *writer)) Result {
	return render(o, func(w *writer) Result {
		res := Result{Budget: o.budget()}
		w.say("budget: %s characters", num(res.Budget))
		head(w)
		res.NextOffset = w.messageBody(m, o, o.Offset, res.Budget)
		res.Truncated = res.NextOffset > 0
		return res
	})
}
