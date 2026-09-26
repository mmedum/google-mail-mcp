// Package model is the server's view of a mailbox: messages, threads,
// drafts, labels, the profile and changes, built from the wire types of
// internal/gmail and the parse of internal/mime, with label ids resolved
// to names (§3.9).
//
// Everything a sender wrote stays data here. The model decodes and
// cleans; it never interprets.
package model

import (
	"errors"
	"html"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/mime"
)

// Untrusted is text a sender wrote: a subject, a snippet, a header
// value, an address. It is data (§4.1), and the type keeps it apart from
// the server's own strings: render has no way to write one outside a
// block, and the tools put one only in a field named untrusted_*.
type Untrusted string

// UntrustedAddresses are addresses as their sender wrote them.
func UntrustedAddresses(as []mime.Address) []Untrusted {
	out := make([]Untrusted, 0, len(as))
	for _, a := range as {
		out = append(out, Untrusted(a.String()))
	}
	return out
}

func untrustedList(ss []string) []Untrusted {
	if ss == nil {
		return nil
	}
	out := make([]Untrusted, len(ss))
	for i, s := range ss {
		out[i] = Untrusted(s)
	}
	return out
}

// LabelRef is a label on a message, by id and name.
type LabelRef struct {
	ID   string
	Name string
}

// LabelIndex resolves label ids to names. Build one per call from
// labels.list: a label renamed between two calls must not resolve to its
// old name (§6.2).
type LabelIndex struct {
	byID map[string]gmail.Label
}

// NewLabelIndex indexes a label list.
func NewLabelIndex(labels []gmail.Label) LabelIndex {
	x := LabelIndex{byID: make(map[string]gmail.Label, len(labels))}
	for _, l := range labels {
		x.byID[l.ID] = l
	}
	return x
}

// Name returns a label's name. A system label's name is its id; an id
// the index does not hold is returned as it is, so an unknown label is
// shown rather than dropped.
func (x LabelIndex) Name(id string) string {
	if l, ok := x.byID[id]; ok && l.Name != "" {
		return l.Name
	}
	return id
}

// Refs resolves ids in order.
func (x LabelIndex) Refs(ids []string) []LabelRef {
	out := make([]LabelRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, LabelRef{ID: id, Name: x.Name(id)})
	}
	return out
}

// Label is a label with its counts when labels.get supplied them.
type Label struct {
	ID                    string
	Name                  string
	Type                  string // "system" or "user"
	MessageListVisibility string
	LabelListVisibility   string
	TextColor             string
	BackgroundColor       string
	// HasCounts is false for a label from labels.list, which carries no
	// counts; zero then means unknown, not empty.
	HasCounts      bool
	MessagesTotal  int
	MessagesUnread int
	ThreadsTotal   int
	ThreadsUnread  int
}

// NewLabel converts a wire label. withCounts says whether it came from
// labels.get.
func NewLabel(l gmail.Label, withCounts bool) Label {
	out := Label{
		ID: l.ID, Name: l.Name, Type: l.Type,
		MessageListVisibility: l.MessageListVisibility, LabelListVisibility: l.LabelListVisibility,
		HasCounts:     withCounts,
		MessagesTotal: int(l.MessagesTotal), MessagesUnread: int(l.MessagesUnread),
		ThreadsTotal: int(l.ThreadsTotal), ThreadsUnread: int(l.ThreadsUnread),
	}
	if out.Name == "" {
		out.Name = l.ID
	}
	if l.Color != nil {
		out.TextColor, out.BackgroundColor = l.Color.TextColor, l.Color.BackgroundColor
	}
	return out
}

// NewLabels converts a label list, system labels first, then user
// labels by name.
func NewLabels(ls []gmail.Label, withCounts bool) []Label {
	out := make([]Label, 0, len(ls))
	for _, l := range ls {
		out = append(out, NewLabel(l, withCounts))
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := out[i].Type == gmail.LabelTypeSystem, out[j].Type == gmail.LabelTypeSystem
		if si != sj {
			return si
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Profile is the account.
type Profile struct {
	Email         string
	MessagesTotal int
	ThreadsTotal  int
	HistoryID     string
}

// NewProfile converts the wire profile.
func NewProfile(p gmail.Profile) Profile {
	return Profile{Email: p.EmailAddress, MessagesTotal: int(p.MessagesTotal), ThreadsTotal: int(p.ThreadsTotal), HistoryID: p.HistoryID}
}

// Message is one email as the server shows it.
type Message struct {
	ID        string
	ThreadID  string
	Labels    []LabelRef
	HistoryID string
	// Date is Gmail's internalDate: when Gmail received the message, on
	// Google's clock. The sender's Date header is DateHeader, and can say
	// anything.
	Date         time.Time
	DateHeader   Untrusted
	SizeEstimate int

	// Snippet is Gmail's snippet, unescaped, with invisible characters
	// removed; SnippetHidden counts them.
	Snippet       Untrusted
	SnippetHidden int

	Subject         Untrusted
	From            []mime.Address
	To              []mime.Address
	Cc              []mime.Address
	Bcc             []mime.Address
	ReplyTo         []mime.Address
	RFC822MessageID Untrusted
	InReplyTo       []Untrusted
	References      []Untrusted
	ListUnsubscribe Untrusted
	// Headers are every top-level header, undecoded, for headers: all.
	Headers []mime.Header
	// HeaderHidden counts invisible characters removed from the subject,
	// names and addresses; LenientHeaders names address headers read leniently.
	HeaderHidden   int
	LenientHeaders []string

	// Complete is set when the message was read with its body (format
	// full or raw). A metadata read has headers only.
	Complete    bool
	Body        mime.Body
	Attachments []mime.Attachment
	// HasAttachments is known from the parts on a complete read; on a
	// metadata read it is inferred from a multipart/mixed Content-Type,
	// which is what a message with attachments almost always is.
	HasAttachments bool
	// NeedsFetch lists parts to fetch before the body is whole.
	NeedsFetch []mime.PartRef

	Classification []gmail.ClassificationLabelValue
}

// HasLabel reports whether the message carries a label id.
func (m Message) HasLabel(id string) bool {
	return slices.ContainsFunc(m.Labels, func(l LabelRef) bool { return l.ID == id })
}

// Sender is the first From address, or a zero Address.
func (m Message) Sender() mime.Address {
	if len(m.From) == 0 {
		return mime.Address{}
	}
	return m.From[0]
}

// NewMessage converts a wire message read in any format. fetched holds,
// keyed by part id, the bytes of parts the previous conversion listed in
// NeedsFetch.
func NewMessage(g *gmail.Message, labels LabelIndex, fetched map[string][]byte) (Message, error) {
	if g == nil {
		return Message{}, errors.New("no message")
	}
	m := Message{
		ID: g.ID, ThreadID: g.ThreadID, HistoryID: g.HistoryID,
		Labels:         labels.Refs(g.LabelIDs),
		SizeEstimate:   int(g.SizeEstimate),
		Classification: g.ClassificationLabelValues,
	}
	snippet, hidden := mime.StripInvisible(html.UnescapeString(g.Snippet))
	m.Snippet, m.SnippetHidden = Untrusted(snippet), hidden
	if ms, ok := g.InternalDateMillis(); ok {
		m.Date = time.UnixMilli(ms).UTC()
	}

	var parsed *mime.Message
	var err error
	switch {
	case g.Raw != "":
		parsed, err = mime.ParseRawBase64URL(g.Raw)
		m.Complete = true
	case g.Payload != nil:
		parsed, err = mime.ParsePayload(g.Payload, fetched)
		m.Complete = isFull(g.Payload)
	default:
		return m, nil // format=minimal
	}
	if err != nil {
		return m, err
	}
	m.Subject = Untrusted(parsed.Subject)
	m.From, m.To, m.Cc, m.Bcc, m.ReplyTo = parsed.From, parsed.To, parsed.Cc, parsed.Bcc, parsed.ReplyTo
	m.RFC822MessageID = Untrusted(parsed.MessageID)
	m.InReplyTo, m.References = untrustedList(parsed.InReplyTo), untrustedList(parsed.References)
	m.ListUnsubscribe = Untrusted(parsed.ListUnsubscribe)
	m.Headers = parsed.Headers
	m.DateHeader = Untrusted(parsed.DateHeader)
	m.HeaderHidden, m.LenientHeaders = parsed.HeaderHidden, parsed.LenientHeaders
	if m.Date.IsZero() {
		m.Date = parsed.Date
	}
	if m.Complete {
		m.Body = parsed.Body
		m.Attachments = parsed.Attachments
		m.NeedsFetch = parsed.NeedsFetch
		for _, a := range parsed.Attachments {
			if !a.Inline {
				m.HasAttachments = true
			}
		}
	} else {
		m.HasAttachments = parsed.ContentType == "multipart/mixed"
	}
	return m, nil
}

// isFull tells a format=full payload from a format=metadata one: only
// full carries parts or body content.
func isFull(p *gmail.MessagePart) bool {
	if len(p.Parts) > 0 {
		return true
	}
	return p.Body != nil && (p.Body.Data != "" || p.Body.AttachmentID != "" || p.Body.Size > 0)
}

// Thread is a conversation, messages oldest first as Gmail returns them.
type Thread struct {
	ID        string
	HistoryID string
	Snippet   Untrusted
	Messages  []Message
}

// NewThread converts a wire thread. fetched is keyed by message id, then
// part id.
func NewThread(g *gmail.Thread, labels LabelIndex, fetched map[string]map[string][]byte) (Thread, error) {
	if g == nil {
		return Thread{}, errors.New("no thread")
	}
	t := Thread{ID: g.ID, HistoryID: g.HistoryID}
	snippet, _ := mime.StripInvisible(html.UnescapeString(g.Snippet))
	t.Snippet = Untrusted(snippet)
	for i := range g.Messages {
		m, err := NewMessage(&g.Messages[i], labels, fetched[g.Messages[i].ID])
		if err != nil {
			return t, err
		}
		t.Messages = append(t.Messages, m)
	}
	return t, nil
}

// Subject is the first message's subject.
func (t Thread) Subject() Untrusted {
	if len(t.Messages) == 0 {
		return ""
	}
	return t.Messages[0].Subject
}

// Latest is the newest message, or a zero Message.
func (t Thread) Latest() Message {
	if len(t.Messages) == 0 {
		return Message{}
	}
	return t.Messages[len(t.Messages)-1]
}

// Participants are the distinct senders and recipients, in order of
// first appearance.
func (t Thread) Participants() []mime.Address {
	var out []mime.Address
	seen := map[string]bool{}
	for _, m := range t.Messages {
		for _, list := range [][]mime.Address{m.From, m.To, m.Cc} {
			for _, a := range list {
				k := strings.ToLower(a.Email)
				if k == "" {
					k = "name:" + a.Name
				}
				if !seen[k] {
					seen[k] = true
					out = append(out, a)
				}
			}
		}
	}
	return out
}

// Labels is the union of the messages' labels, in first-seen order.
func (t Thread) Labels() []LabelRef {
	var out []LabelRef
	for _, m := range t.Messages {
		for _, l := range m.Labels {
			if !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	return out
}

// HasAttachments reports whether any message has one.
func (t Thread) HasAttachments() bool {
	return slices.ContainsFunc(t.Messages, func(m Message) bool { return m.HasAttachments })
}

// Unread counts messages carrying UNREAD.
func (t Thread) Unread() int {
	n := 0
	for _, m := range t.Messages {
		if m.HasLabel("UNREAD") {
			n++
		}
	}
	return n
}

// Draft is an unsent message. A draft has two ids, the draft's and its
// message's; every rendering names both (§6.1).
type Draft struct {
	ID      string
	Message Message
}

// NewDraft converts a wire draft.
func NewDraft(g *gmail.Draft, labels LabelIndex, fetched map[string][]byte) (Draft, error) {
	if g == nil || g.Message == nil {
		return Draft{}, errors.New("no draft message")
	}
	m, err := NewMessage(g.Message, labels, fetched)
	return Draft{ID: g.ID, Message: m}, err
}

// ChangeKind is what a history record says happened.
type ChangeKind string

// Change kinds.
const (
	ChangeAdded         ChangeKind = "added"
	ChangeDeleted       ChangeKind = "deleted"
	ChangeLabelsAdded   ChangeKind = "labels_added"
	ChangeLabelsRemoved ChangeKind = "labels_removed"
)

// Change is one message-level change from history.list.
type Change struct {
	HistoryID string
	Kind      ChangeKind
	MessageID string
	ThreadID  string
	// Labels are the labels added or removed, or the message's labels
	// when it was added.
	Labels []LabelRef
}

// NewChanges flattens history records into changes, in record order.
func NewChanges(hs []gmail.History, labels LabelIndex) []Change {
	var out []Change
	add := func(h gmail.History, k ChangeKind, m *gmail.Message, ids []string) {
		if m == nil {
			return
		}
		out = append(out, Change{HistoryID: h.ID, Kind: k, MessageID: m.ID, ThreadID: m.ThreadID, Labels: labels.Refs(ids)})
	}
	for _, h := range hs {
		for _, r := range h.MessagesAdded {
			if r.Message != nil {
				add(h, ChangeAdded, r.Message, r.Message.LabelIDs)
			}
		}
		for _, r := range h.MessagesDeleted {
			add(h, ChangeDeleted, r.Message, nil)
		}
		for _, r := range h.LabelsAdded {
			add(h, ChangeLabelsAdded, r.Message, r.LabelIDs)
		}
		for _, r := range h.LabelsRemoved {
			add(h, ChangeLabelsRemoved, r.Message, r.LabelIDs)
		}
	}
	return out
}
