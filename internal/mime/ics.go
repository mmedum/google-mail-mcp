package mime

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	netmail "net/mail"
)

// Invitation is what a calendar part says about its event (RFC 5545):
// the identifiers a calendar server finds the event by, and its times.
// Every string but Method is the sender's, cleaned of control and
// invisible characters.
type Invitation struct {
	// Method is the calendar's METHOD when it is one RFC 5546 §1.4
	// defines, else "".
	Method string
	// UID is the event's UID; "" when it is missing or over maxICSID,
	// since a cut UID finds nothing.
	UID string
	// Sequence is the event's revision; 0 when it gives none (RFC 5545
	// §3.8.7.4: "When a calendar component is created, its sequence
	// number is 0").
	Sequence int
	// RecurrenceID is set when the event is one occurrence of a
	// repeating event, written as Start is.
	RecurrenceID string
	Summary      string
	// Organizer is the address of a mailto: ORGANIZER, else "".
	Organizer string
	// Start and End are RFC 3339 when the time is UTC or its TZID is an
	// IANA zone, YYYY-MM-DD for a date, and otherwise as written.
	Start, End string
	// TimeZone is the TZID the start names, as written.
	TimeZone string
	// AllDay is set when the start is a date.
	AllDay bool
	// Events counts the calendar's events. The fields describe the first
	// one that is not one occurrence's change, or else the first.
	Events int
}

// Limits a hostile calendar cannot exceed. A part longer than
// maxICSBytes is read for its METHOD only, and never fetched.
const (
	maxICSBytes = 1 << 20
	maxICSDepth = 8
	maxICSText  = 256  // runes of a summary; more is cut with an ellipsis
	maxICSID    = 1024 // bytes of a UID
	maxICSZone  = 128  // bytes of a TZID
	maxICSTime  = 64   // bytes of a time value
)

// itipMethods are the methods RFC 5546 §1.4 defines.
var itipMethods = []string{"PUBLISH", "REQUEST", "REPLY", "ADD", "CANCEL", "REFRESH", "COUNTER", "DECLINECOUNTER"}

// icsProp is one content line: its name in upper case, the two
// parameters read here, and its value as written.
type icsProp struct {
	name        string
	tzid, vtype string
	value       string
}

// parseInvitation reads a calendar part. It returns the invitation, nil
// when the part holds no VCALENDAR or is over maxICSBytes, and the
// calendar's METHOD as written, which an over-long part still gives
// when it comes first. charset is the part's label; RFC 5545 §3.1.4
// makes UTF-8 the default.
func parseInvitation(data []byte, charset string) (*Invitation, string) {
	whole := len(data) <= maxICSBytes
	if !whole {
		data = data[:maxICSBytes]
	}
	if charset == "" {
		charset = "utf-8"
	}
	// Unfolded before decoding: a fold may split a multi-octet
	// character, which RFC 5545 §3.1 asks a reader to restore.
	text, _ := decodeCharset(unfoldICS(data), charset)

	var r icsReader
	for line := range strings.SplitSeq(text, "\n") {
		if p, ok := parseICSLine(strings.TrimSuffix(line, "\r")); ok {
			r.read(p)
		}
	}
	if r.cur != nil {
		r.endEvent()
	}
	if !r.calendar || !whole {
		return nil, r.method
	}
	return r.invitation(), r.method
}

// icsReader walks a calendar's content lines and keeps what an
// Invitation needs.
type icsReader struct {
	stack []string
	// deep counts components nested past maxICSDepth, which are skipped.
	deep                 int
	calendar, haveMethod bool
	method               string
	events               int
	// cur is the event being read, at stack[curAt]; first and master are
	// the first event and the first that is not for one occurrence.
	cur, first, master map[string]icsProp
	curAt              int
}

func (r *icsReader) read(p icsProp) {
	atTop := len(r.stack) == 1 && r.stack[0] == "VCALENDAR"
	var comp string
	if p.name == "BEGIN" || p.name == "END" {
		comp = strings.ToUpper(strings.TrimSpace(p.value))
	}
	switch {
	case p.name == "BEGIN" && (r.deep > 0 || len(r.stack) >= maxICSDepth):
		r.deep++
	case p.name == "BEGIN":
		switch {
		case comp == "VCALENDAR" && len(r.stack) == 0:
			r.calendar = true
		case comp == "VEVENT" && atTop:
			r.events++
			r.cur, r.curAt = map[string]icsProp{}, len(r.stack)
		}
		r.stack = append(r.stack, comp)
	case p.name == "END" && r.deep > 0:
		r.deep--
	case p.name == "END":
		r.end(comp)
	case r.deep > 0:
	case atTop && p.name == "METHOD" && !r.haveMethod:
		r.method, r.haveMethod = strings.TrimSpace(p.value), true
	case r.cur != nil && len(r.stack)-1 == r.curAt && isEventProp(p.name):
		if _, seen := r.cur[p.name]; !seen {
			r.cur[p.name] = p
		}
	}
}

// end closes the innermost open component named comp and anything left
// open inside it. An END naming nothing open is ignored.
func (r *icsReader) end(comp string) {
	for i := len(r.stack) - 1; i >= 0; i-- {
		if r.stack[i] != comp {
			continue
		}
		if r.cur != nil && i <= r.curAt {
			r.endEvent()
		}
		r.stack = r.stack[:i]
		return
	}
}

func (r *icsReader) endEvent() {
	if r.first == nil {
		r.first = r.cur
	}
	if _, ok := r.cur["RECURRENCE-ID"]; !ok && r.master == nil {
		r.master = r.cur
	}
	r.cur = nil
}

// invitation describes the master event, else the first.
func (r *icsReader) invitation() *Invitation {
	inv := &Invitation{Events: r.events}
	if v := strings.ToUpper(r.method); slices.Contains(itipMethods, v) {
		inv.Method = v
	}
	e := r.master
	if e == nil {
		e = r.first
	}
	if e == nil {
		return inv
	}
	if uid := icsText(unescapeICS(e["UID"].value)); len(uid) <= maxICSID {
		inv.UID = uid
	}
	if n, err := strconv.Atoi(strings.TrimSpace(e["SEQUENCE"].value)); err == nil && n >= 0 {
		inv.Sequence = n
	}
	inv.Summary = cutRunes(icsText(unescapeICS(e["SUMMARY"].value)), maxICSText)
	inv.Organizer = icsOrganizer(e["ORGANIZER"].value)
	inv.Start, inv.AllDay = icsTime(e["DTSTART"])
	inv.End, _ = icsTime(e["DTEND"])
	inv.RecurrenceID, _ = icsTime(e["RECURRENCE-ID"])
	if z := e["DTSTART"].tzid; len(z) <= maxICSZone {
		inv.TimeZone = icsText(z)
	}
	return inv
}

func isEventProp(name string) bool {
	switch name {
	case "UID", "SEQUENCE", "RECURRENCE-ID", "SUMMARY", "ORGANIZER", "DTSTART", "DTEND":
		return true
	}
	return false
}

// unfoldICS removes each line break followed by a space or tab, with
// that one character (RFC 5545 §3.1). A bare LF counts as a line break.
func unfoldICS(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' && i+1 < len(b) && (b[i+1] == ' ' || b[i+1] == '\t') {
			if n := len(out); n > 0 && out[n-1] == '\r' {
				out = out[:n-1]
			}
			i++
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// parseICSLine reads `name *(";" param) ":" value` (RFC 5545 §3.1). A
// parameter value may be quoted, and then holds ";", ":" and "," as
// text. It keeps TZID and VALUE, each its first value; false is a line
// that does not parse.
func parseICSLine(line string) (icsProp, bool) {
	i := nameEnd(line, 0)
	if i == 0 {
		return icsProp{}, false
	}
	p := icsProp{name: strings.ToUpper(line[:i])}
	for i < len(line) && line[i] == ';' {
		j := nameEnd(line, i+1)
		if j == i+1 || j >= len(line) || line[j] != '=' {
			return icsProp{}, false
		}
		name := strings.ToUpper(line[i+1 : j])
		i = j + 1
		for k := 0; ; k++ {
			var v string
			if i < len(line) && line[i] == '"' {
				end := strings.IndexByte(line[i+1:], '"')
				if end < 0 {
					return icsProp{}, false
				}
				v, i = line[i+1:i+1+end], i+end+2
			} else {
				j := i
				for j < len(line) && !strings.ContainsRune(`;:,"`, rune(line[j])) {
					j++
				}
				v, i = line[i:j], j
			}
			if k == 0 && name == "TZID" && p.tzid == "" {
				p.tzid = v
			}
			if k == 0 && name == "VALUE" && p.vtype == "" {
				p.vtype = v
			}
			if i >= len(line) || line[i] != ',' {
				break
			}
			i++
		}
	}
	if i >= len(line) || line[i] != ':' {
		return icsProp{}, false
	}
	p.value = line[i+1:]
	return p, true
}

// nameEnd is where a name of letters, digits and dashes starting at i
// ends.
func nameEnd(s string, i int) int {
	for i < len(s) {
		c := s[i]
		if c != '-' && (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			break
		}
		i++
	}
	return i
}

// unescapeICS reverses RFC 5545 §3.3.11's escapes: \\ \; \, and \n or
// \N. Any other backslash is kept as written.
func unescapeICS(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			case '\\', ';', ',':
				b.WriteByte(s[i+1])
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// icsText is a value as one line: control characters become spaces and
// invisible ones are shown as \u{…} escapes.
func icsText(s string) string { return escapeInvisible(cleanHeader(s)) }

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// icsOrganizer is the address of a mailto: ORGANIZER (RFC 5545
// §3.8.4.3), or "" for anything else.
func icsOrganizer(v string) string {
	const scheme = "mailto:"
	v = strings.TrimSpace(v)
	if len(v) <= len(scheme) || !strings.EqualFold(v[:len(scheme)], scheme) {
		return ""
	}
	addr, _, _ := strings.Cut(v[len(scheme):], "?")
	if u, err := url.PathUnescape(addr); err == nil {
		addr = u
	}
	a, err := netmail.ParseAddress(addr)
	if err != nil || a.Name != "" || len(a.Address) > 254 || hasInvisible(a.Address) || a.Address != cleanHeader(a.Address) {
		return ""
	}
	return a.Address
}

// icsTime writes a DTSTART, DTEND or RECURRENCE-ID value (RFC 5545
// §3.3.4, §3.3.5): a date as YYYY-MM-DD, a UTC time or one whose TZID is
// an IANA zone as RFC 3339, and anything else as written. date reports
// a date.
func icsTime(p icsProp) (text string, date bool) {
	v := strings.TrimSpace(p.value)
	if v == "" || len(v) > maxICSTime {
		return "", false
	}
	if strings.EqualFold(p.vtype, "DATE") || (len(v) == 8 && !strings.ContainsFunc(v, notDigit)) {
		if t, err := time.Parse("20060102", v); err == nil {
			return t.Format(time.DateOnly), true
		}
		return icsText(v), strings.EqualFold(p.vtype, "DATE")
	}
	if strings.HasSuffix(v, "Z") {
		if t, err := time.Parse("20060102T150405Z", v); err == nil {
			return t.Format(time.RFC3339), false
		}
	} else if loc := ianaZone(p.tzid); loc != nil {
		if t, err := time.ParseInLocation("20060102T150405", v, loc); err == nil {
			return t.Format(time.RFC3339), false
		}
	}
	return icsText(v), false
}

func notDigit(r rune) bool { return r < '0' || r > '9' }

// ianaZone loads a TZID that names an IANA zone, or returns nil. "Local"
// is the server's own zone, not one a sender can mean.
func ianaZone(name string) *time.Location {
	if name == "" || name == "Local" || len(name) > maxICSZone || !printableASCII(name) {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return loc
}
