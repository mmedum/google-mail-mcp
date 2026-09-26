package gmailtest

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/internal/gmail"
)

// systemLabels are the system label ids the fake knows. Google calls
// its own list "not exhaustive" (§2.12).
var systemLabels = []string{
	"INBOX", "SENT", "DRAFT", "SPAM", "TRASH", "UNREAD", "STARRED", "IMPORTANT",
	"CHAT", "CATEGORY_PERSONAL", "CATEGORY_SOCIAL", "CATEGORY_PROMOTIONS",
	"CATEGORY_UPDATES", "CATEGORY_FORUMS",
}

// Person is a generated correspondent. Header is the display name as it
// goes on the wire, RFC 2047-encoded where the fixture exercises that.
type Person struct {
	Name   string
	Email  string
	Header string
}

func (p Person) addr() string {
	name := p.Header
	if name == "" {
		name = p.Name
	}
	if name == "" {
		return p.Email
	}
	if !strings.HasPrefix(name, "=?") && strings.ContainsAny(name, `,.;:"()<>@`) {
		name = `"` + name + `"`
	}
	return name + " <" + p.Email + ">"
}

type message struct {
	id, threadID string
	labels       []string
	internalDate time.Time
	raw          []byte
	payload      gmail.MessagePart
	historyID    uint64
	snippet      string
	rfc822ID     string

	// Decoded fields search reads.
	from, to, subject, text string
	hasAttachment           bool
}

// spec describes a message to add.
type spec struct {
	from       Person
	to, cc     []Person
	subject    string // decoded, for search
	subjectHdr string // as written; "" means subject
	at         time.Time
	zone       *time.Location
	labels     []string
	body       *Part
	text       string // what search and the snippet read
	thread     string // existing thread id, or "" for a new thread
	inReplyTo  *message
	extra      []gmail.MessagePartHeader
}

func (s *Server) add(sp spec) *message {
	id := s.nextID()
	thread := sp.thread
	if thread == "" {
		thread = id
	}
	zone := sp.zone
	if zone == nil {
		zone = time.UTC
	}
	rfcID := "<fixture." + id + "@mail.example.com>"
	var hs []gmail.MessagePartHeader
	add := func(n, v string) {
		if v != "" {
			hs = append(hs, gmail.MessagePartHeader{Name: n, Value: v})
		}
	}
	add("From", sp.from.addr())
	add("To", joinAddrs(sp.to))
	add("Cc", joinAddrs(sp.cc))
	subj := sp.subjectHdr
	if subj == "" {
		subj = sp.subject
	}
	add("Subject", subj)
	add("Date", sp.at.In(zone).Format(time.RFC1123Z))
	add("Message-ID", rfcID)
	if p := sp.inReplyTo; p != nil {
		refs := headerValue(p.payload.Headers, "References")
		add("In-Reply-To", p.rfc822ID)
		add("References", strings.TrimSpace(refs+" "+p.rfc822ID))
	}
	hs = append(hs, sp.extra...)
	add("MIME-Version", "1.0")

	var raw bytes.Buffer
	sp.body.writeRaw(&raw, hs)
	m := &message{
		id: id, threadID: thread, labels: sp.labels, internalDate: sp.at,
		raw: raw.Bytes(), rfc822ID: rfcID,
		from:    strings.ToLower(sp.from.Name + " " + sp.from.Email),
		to:      strings.ToLower(peopleText(sp.to) + " " + peopleText(sp.cc)),
		subject: strings.ToLower(sp.subject),
		text:    strings.ToLower(sp.text),
		snippet: snippet(sp.text),
	}
	m.payload = sp.body.payload("", hs, func(partID string) string {
		if partID == "" {
			partID = "root"
		}
		return "att-" + id + "-" + partID
	}, s.store)
	m.hasAttachment = hasFilename(sp.body)
	s.messages[id] = m
	s.threads[thread] = append(s.threads[thread], id)
	s.historyID++
	m.historyID = s.historyID
	s.history = append(s.history, historyRecord{id: s.historyID, kind: kindAdded, message: id, labels: slices.Clone(sp.labels)})
	return m
}

func hasFilename(p *Part) bool {
	if p.Filename != "" {
		return true
	}
	for _, c := range p.Children {
		if hasFilename(c) {
			return true
		}
	}
	return false
}

// relabel changes a message's labels and records the change in history.
func (s *Server) relabel(id string, addLabels, removeLabels []string) {
	m := s.messages[id]
	var added, removed []string
	for _, l := range addLabels {
		if !slices.Contains(m.labels, l) {
			m.labels = append(m.labels, l)
			added = append(added, l)
		}
	}
	for _, l := range removeLabels {
		if i := slices.Index(m.labels, l); i >= 0 {
			m.labels = slices.Delete(m.labels, i, i+1)
			removed = append(removed, l)
		}
	}
	if len(added) > 0 {
		s.historyID++
		s.history = append(s.history, historyRecord{id: s.historyID, kind: kindLabelAdded, message: id, labels: added})
	}
	if len(removed) > 0 {
		s.historyID++
		s.history = append(s.history, historyRecord{id: s.historyID, kind: kindLabelRemoved, message: id, labels: removed})
	}
	m.historyID = s.historyID
}

func joinAddrs(ps []Person) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.addr())
	}
	return strings.Join(out, ", ")
}

func peopleText(ps []Person) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name+" "+p.Email)
	}
	return strings.Join(out, " ")
}

func headerValue(hs []gmail.MessagePartHeader, name string) string {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// snippet is what Gmail shows: the start of the text, whitespace folded
// and HTML-escaped, which is how Gmail's own snippets arrive.
func snippet(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(t) > 140 {
		r := []rune(t)
		t = string(r[:140])
	}
	return html.EscapeString(t)
}

// render returns the message in a format. metadataHeaders filters the
// headers of format=metadata; empty means all.
func (s *Server) render(m *message, format string, metadataHeaders []string) gmail.Message {
	out := gmail.Message{
		ID:           m.id,
		ThreadID:     m.threadID,
		LabelIDs:     slices.Clone(m.labels),
		Snippet:      m.snippet,
		HistoryID:    strconv.FormatUint(m.historyID, 10),
		InternalDate: strconv.FormatInt(m.internalDate.UnixMilli(), 10),
		SizeEstimate: int32(len(m.raw)), //nolint:gosec // fixture sizes are small
	}
	switch format {
	case "raw":
		out.Raw = base64.URLEncoding.EncodeToString(m.raw)
	case "metadata":
		p := gmail.MessagePart{PartID: "", MimeType: m.payload.MimeType, Body: &gmail.MessagePartBody{}}
		for _, h := range m.payload.Headers {
			if len(metadataHeaders) == 0 || slices.ContainsFunc(metadataHeaders, func(n string) bool { return strings.EqualFold(n, h.Name) }) {
				p.Headers = append(p.Headers, h)
			}
		}
		out.Payload = &p
	case "full":
		p := clonePart(m.payload)
		out.Payload = &p
	}
	return out
}

func clonePart(p gmail.MessagePart) gmail.MessagePart {
	c := p
	c.Headers = slices.Clone(p.Headers)
	if p.Body != nil {
		b := *p.Body
		c.Body = &b
	}
	c.Parts = nil
	for _, ch := range p.Parts {
		c.Parts = append(c.Parts, clonePart(ch))
	}
	return c
}

type historyKind int

const (
	kindAdded historyKind = iota
	kindLabelAdded
	kindLabelRemoved
)

var historyTypeNames = map[string]historyKind{
	"messageAdded": kindAdded, "labelAdded": kindLabelAdded, "labelRemoved": kindLabelRemoved,
}

type historyRecord struct {
	id      uint64
	kind    historyKind
	message string
	labels  []string
}

func (s *Server) historyWire(r historyRecord) gmail.History {
	m := s.messages[r.message]
	ref := gmail.Message{ID: m.id, ThreadID: m.threadID, LabelIDs: slices.Clone(m.labels)}
	h := gmail.History{ID: strconv.FormatUint(r.id, 10), Messages: []gmail.Message{{ID: m.id, ThreadID: m.threadID}}}
	switch r.kind {
	case kindAdded:
		h.MessagesAdded = []gmail.HistoryMessageAdded{{Message: &ref}}
	case kindLabelAdded:
		h.LabelsAdded = []gmail.HistoryLabelAdded{{Message: &ref, LabelIDs: slices.Clone(r.labels)}}
	case kindLabelRemoved:
		h.LabelsRemoved = []gmail.HistoryLabelRemoved{{Message: &ref, LabelIDs: slices.Clone(r.labels)}}
	}
	return h
}

// Scenario names a generated situation and the ids it produced.
type Scenario struct {
	Name     string
	ThreadID string
	// MessageIDs are the scenario's messages, oldest first.
	MessageIDs []string
	DraftID    string
}

// Scenario returns a named scenario. It panics on an unknown name, which
// is a mistake in the test.
func (s *Server) Scenario(name string) Scenario {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.scenarios[name]
	if !ok {
		panic(fmt.Sprintf("gmailtest: no scenario %q", name))
	}
	sc.MessageIDs = slices.Clone(sc.MessageIDs)
	return sc
}

// Scenarios returns every scenario name.
func (s *Server) Scenarios() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.scenarios))
	for n := range s.scenarios {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
