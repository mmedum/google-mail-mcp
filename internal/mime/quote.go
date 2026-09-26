package mime

import (
	"regexp"
	"strings"
)

// SpanKind says what a span of body text is.
type SpanKind string

// Span kinds.
const (
	SpanQuote     SpanKind = "quote"
	SpanSignature SpanKind = "signature"
)

// Span is a region of Body.Text a reader can have collapsed: quoted text
// from an earlier message, or a signature block. Start and End are byte
// offsets into the text; Lines is how many lines it covers.
//
// Trailing is set when nothing but other spans and blank lines follows
// it — the quoted history below a reply, as opposed to a quote a reply
// answers inline. A renderer collapses trailing quotes and signatures
// and leaves inline quotes where the reply refers to them.
type Span struct {
	Kind     SpanKind
	Start    int
	End      int
	Lines    int
	Trailing bool
}

var (
	attribution = []*regexp.Regexp{
		regexp.MustCompile(`(?i)^\s*on\s.+\bwrote:\s*$`),
		regexp.MustCompile(`(?i)^.+<[^>]+@[^>]+>\s+(?:wrote|writes):\s*$`),
		regexp.MustCompile(`(?i)^\s*le\s.+\s(?:a\s)?écrit\s*:\s*$`),
		regexp.MustCompile(`(?i)^\s*am\s.+\sschrieb\s.*:\s*$`),
		regexp.MustCompile(`(?i)^\s*el\s.+\sescribió\s*:\s*$`),
		regexp.MustCompile(`(?i)^\s*il\s.+\sha scritto\s*:\s*$`),
		regexp.MustCompile(`(?i)^\s*op\s.+\sschreef\s.*:\s*$`),
		regexp.MustCompile(`(?i)^\s*den\s.+\sskrev\s.*:\s*$`),
	}
	attributionStart = regexp.MustCompile(`(?i)^\s*on\s.+`)
	attributionEnd   = regexp.MustCompile(`(?i)\bwrote:\s*$`)
	originalMessage  = regexp.MustCompile(`(?i)^\s*-{2,}\s*original message\s*-{2,}\s*$`)
	outlookRule      = regexp.MustCompile(`^\s*_{20,}\s*$`)
	outlookFrom      = regexp.MustCompile(`(?i)^\s*\*?from:\*?\s+\S`)
	outlookSent      = regexp.MustCompile(`(?i)^\s*\*?(?:sent|date):\*?\s+\S`)
	outlookTo        = regexp.MustCompile(`(?i)^\s*\*?(?:to|subject):\*?\s+`)
)

type line struct {
	start, end int // end excludes the newline
	text       string
}

func splitLines(s string) []line {
	var out []line
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			out = append(out, line{start: start, end: i, text: strings.TrimRight(s[start:i], "\r")})
			start = i + 1
		}
	}
	return out
}

func isQuoted(l string) bool {
	return strings.HasPrefix(strings.TrimLeft(l, " \t"), ">")
}

func isBlank(l string) bool { return strings.TrimSpace(l) == "" }

// maxSignatureLines bounds how far a "-- " delimiter reaches; beyond it
// the dashes were probably not a signature delimiter.
const maxSignatureLines = 30

// FindSpans locates quoted text and signature blocks in plain text.
//
// Quoted text is a run of ">" lines, together with the attribution line
// before it ("On … wrote:", in several languages, wrapped over two lines
// or not), or everything from an Outlook-style header block ("From: …"
// then "Sent:" or "Date:") or an "Original Message" rule to the end. A
// signature starts at a "-- " line and runs to the next quote or the
// end. Nothing is reported when collapsing it would leave no text: a
// message that is all quote is the message.
func FindSpans(text string) []Span {
	lines := splitLines(text)
	quoted := make([]bool, len(lines))
	markHistory(lines, quoted)
	markQuoteRuns(lines, quoted)
	kinds := make([]SpanKind, len(lines))
	for i := range lines {
		if quoted[i] {
			kinds[i] = SpanQuote
		}
	}
	markSignature(lines, quoted, kinds)
	spans := collectSpans(lines, kinds)
	if len(spans) == 0 {
		return nil
	}
	covered := linesInSpans(lines, spans)
	markTrailing(lines, spans, covered)
	// Refuse to report spans that would collapse the whole message.
	for i, l := range lines {
		if !isBlank(l.text) && !covered[i] {
			return spans
		}
	}
	return nil
}

// markHistory marks everything from an Outlook header block or an
// "Original Message" rule to the end.
func markHistory(lines []line, quoted []bool) {
	first := 0
	for first < len(lines) && isBlank(lines[first].text) {
		first++
	}
	for i := first + 1; i < len(lines); i++ {
		l := lines[i]
		if originalMessage.MatchString(l.text) || outlookHeaderAt(lines, i) {
			for j := i; j < len(lines); j++ {
				quoted[j] = true
			}
			return
		}
	}
}

// markQuoteRuns marks ">" runs — blank lines inside a run join it — and
// the attribution just above each.
func markQuoteRuns(lines []line, quoted []bool) {
	for i := 0; i < len(lines); i++ {
		if !isQuoted(lines[i].text) {
			continue
		}
		j := runEnd(lines, i)
		for k := i; k < j; k++ {
			quoted[k] = true
		}
		if a := attributionStartAt(lines, i); a >= 0 {
			for k := a; k < i; k++ {
				quoted[k] = true
			}
		}
		i = j
	}
}

// runEnd returns the index after the ">" run starting at i.
func runEnd(lines []line, i int) int {
	j := i
	for j < len(lines) {
		if isQuoted(lines[j].text) {
			j++
			continue
		}
		k := j
		for k < len(lines) && isBlank(lines[k].text) {
			k++
		}
		if k < len(lines) && k > j && isQuoted(lines[k].text) {
			j = k
			continue
		}
		break
	}
	return j
}

// attributionStartAt returns the first line of an attribution above a
// quote starting at i — across one blank line, on one line or wrapped
// over two — or -1.
func attributionStartAt(lines []line, i int) int {
	a := i - 1
	if a >= 0 && isBlank(lines[a].text) {
		a--
	}
	switch {
	case a >= 0 && isAttribution(lines[a].text):
		return a
	case a >= 1 && attributionStart.MatchString(lines[a-1].text) && attributionEnd.MatchString(lines[a].text):
		return a - 1
	}
	return -1
}

// markSignature marks the last "-- " outside a quote, to the next quote
// or the end.
func markSignature(lines []line, quoted []bool, kinds []SpanKind) {
	for i := len(lines) - 1; i >= 0; i-- {
		t := lines[i].text
		if quoted[i] || (t != "-- " && t != "--") || !hasContentBefore(lines, i) {
			continue
		}
		end := i + 1
		for end < len(lines) && !quoted[end] {
			end++
		}
		if end-i-1 <= maxSignatureLines {
			for k := i; k < end; k++ {
				kinds[k] = SpanSignature
			}
		}
		return
	}
}

// collectSpans turns marked lines into spans, leaving trailing blank
// lines to what follows.
func collectSpans(lines []line, kinds []SpanKind) []Span {
	var spans []Span
	for i := 0; i < len(lines); {
		if kinds[i] == "" {
			i++
			continue
		}
		j := i
		for j < len(lines) && kinds[j] == kinds[i] {
			j++
		}
		last := j - 1
		for last > i && isBlank(lines[last].text) {
			last--
		}
		spans = append(spans, Span{Kind: kinds[i], Start: lines[i].start, End: lines[last].end, Lines: last - i + 1})
		i = j
	}
	return spans
}

// linesInSpans reports, per line, whether the line starts inside a
// span. Spans are sorted and do not overlap, so one pass does it.
func linesInSpans(lines []line, spans []Span) []bool {
	in := make([]bool, len(lines))
	p := 0
	for i, l := range lines {
		for p < len(spans) && spans[p].End < l.start {
			p++
		}
		in[i] = p < len(spans) && l.start >= spans[p].Start
	}
	return in
}

// markTrailing sets Trailing on spans followed only by blank lines and
// other spans. It walks from the end once, carrying whether a line of
// reply text has been seen.
func markTrailing(lines []line, spans []Span, covered []bool) {
	text := false
	li := len(lines) - 1
	for si := len(spans) - 1; si >= 0; si-- {
		for ; li >= 0 && lines[li].start >= spans[si].End; li-- {
			if !covered[li] && !isBlank(lines[li].text) {
				text = true
			}
		}
		spans[si].Trailing = !text
	}
}

func isAttribution(l string) bool {
	for _, re := range attribution {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

func hasContentBefore(lines []line, i int) bool {
	for k := range i {
		if !isBlank(lines[k].text) {
			return true
		}
	}
	return false
}

// outlookHeaderAt reports whether an Outlook reply header starts at
// line i: an optional rule of underscores, "From:", then "Sent:" or
// "Date:" and "To:" or "Subject:" within the next five lines.
func outlookHeaderAt(lines []line, i int) bool {
	from := i
	if outlookRule.MatchString(lines[i].text) {
		from = i + 1
		for from < len(lines) && isBlank(lines[from].text) {
			from++
		}
	}
	if from >= len(lines) || !outlookFrom.MatchString(lines[from].text) {
		return false
	}
	sent, to := false, false
	for k := from + 1; k < len(lines) && k <= from+5; k++ {
		sent = sent || outlookSent.MatchString(lines[k].text)
		to = to || outlookTo.MatchString(lines[k].text)
	}
	return sent && to
}
