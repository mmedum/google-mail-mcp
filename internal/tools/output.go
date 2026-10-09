package tools

import (
	"time"

	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
)

// Every field whose value came from mail rather than from Gmail's own
// bookkeeping is a model.Untrusted named untrusted_* (architecture
// §4.1): a client that shows only the structured half still sees which
// strings a stranger wrote. TestUntrustedFieldsAreNamed holds the pair
// both ways over every tool's output.

// LabelRef is a label on a message, by id and name.
type LabelRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Attachment describes an attachment without its content.
type Attachment struct {
	// PartID names the attachment to download_attachment. Gmail's own
	// structure, not the sender's words.
	PartID            string          `json:"part_id"`
	UntrustedFilename model.Untrusted `json:"untrusted_filename"`
	UntrustedMimeType model.Untrusted `json:"untrusted_mime_type"`
	Size              int             `json:"size"`
	AttachmentID      string          `json:"attachment_id,omitempty"`
	Inline            bool            `json:"inline,omitempty"`
	// UntrustedCalendarMethod is an invitation's METHOD as its sender
	// wrote it: REQUEST, CANCEL, REPLY, or anything else.
	UntrustedCalendarMethod model.Untrusted `json:"untrusted_calendar_method,omitempty"`
	Invitation              *Invitation     `json:"invitation,omitempty" jsonschema:"what a calendar invitation says about its event, read from this part; absent when its content was not read"`
}

// Invitation is what a calendar part says about its event (RFC 5545).
// Method is from a fixed set; every other string is the sender's.
type Invitation struct {
	Method                string          `json:"method,omitempty" jsonschema:"the calendar's METHOD when it is one RFC 5546 defines: PUBLISH, REQUEST, REPLY, ADD, CANCEL, REFRESH, COUNTER or DECLINECOUNTER"`
	UntrustedUID          model.Untrusted `json:"untrusted_uid,omitempty" jsonschema:"the event's iCalendar UID, which a calendar server finds the event by (the Google Calendar API's iCalUID)"`
	Sequence              int             `json:"sequence" jsonschema:"the event's revision (SEQUENCE); 0 when the calendar gives none"`
	UntrustedRecurrenceID model.Untrusted `json:"untrusted_recurrence_id,omitempty" jsonschema:"set when the invitation is for one occurrence of a repeating event: that occurrence's original start, written as untrusted_start is"`
	UntrustedSummary      model.Untrusted `json:"untrusted_summary,omitempty" jsonschema:"the event's title"`
	UntrustedOrganizer    model.Untrusted `json:"untrusted_organizer,omitempty" jsonschema:"the organizer's address, when the calendar gives a mailto address"`
	UntrustedStart        model.Untrusted `json:"untrusted_start,omitempty" jsonschema:"RFC 3339 when the time is in UTC or its time zone is an IANA zone; YYYY-MM-DD for an all-day event; otherwise the time as written, in untrusted_time_zone"`
	UntrustedEnd          model.Untrusted `json:"untrusted_end,omitempty" jsonschema:"written as untrusted_start is; absent when the calendar gives a duration instead"`
	UntrustedTimeZone     model.Untrusted `json:"untrusted_time_zone,omitempty" jsonschema:"the time zone the start names (its TZID), as written"`
	AllDay                bool            `json:"all_day" jsonschema:"true when the start is a date with no time of day"`
	Events                int             `json:"events" jsonschema:"how many events the calendar holds; the fields describe the first that is not one occurrence's change, or else the first"`
}

// MessageMeta is one message's headers and what the server noticed.
type MessageMeta struct {
	ID                       string            `json:"id"`
	ThreadID                 string            `json:"thread_id"`
	UntrustedRFC822MessageID model.Untrusted   `json:"untrusted_rfc822_message_id,omitempty"`
	Date                     time.Time         `json:"date"`
	Labels                   []LabelRef        `json:"labels"`
	UntrustedFrom            []model.Untrusted `json:"untrusted_from"`
	UntrustedTo              []model.Untrusted `json:"untrusted_to,omitempty"`
	UntrustedCc              []model.Untrusted `json:"untrusted_cc,omitempty"`
	UntrustedSubject         model.Untrusted   `json:"untrusted_subject"`
	UntrustedSnippet         model.Untrusted   `json:"untrusted_snippet,omitempty"`
	HasAttachments           bool              `json:"has_attachments"`
	Attachments              []Attachment      `json:"attachments,omitempty"`
	// HiddenCharsRemoved counts text a reader of the mail would not have
	// seen, removed before it reached you (§4.1).
	HiddenCharsRemoved int `json:"hidden_chars_removed,omitempty"`
	// LinkMismatches counts links whose visible text names a different
	// host from the one they point to.
	LinkMismatches int `json:"link_mismatches,omitempty"`
	// Unsubscribe is the message's List-Unsubscribe header, read.
	Unsubscribe *Unsubscribe `json:"unsubscribe,omitempty" jsonschema:"how the sender's List-Unsubscribe header offers to unsubscribe; absent when it offers no web or mail address. The server never visits or writes to these"`
}

// Unsubscribe is what a List-Unsubscribe header offers (RFC 2369), each
// list in the header's order, which is the sender's order of preference.
type Unsubscribe struct {
	UntrustedURLs   []model.Untrusted `json:"untrusted_urls,omitempty" jsonschema:"the http and https addresses, most preferred first"`
	UntrustedMailto []model.Untrusted `json:"untrusted_mailto,omitempty" jsonschema:"the mailto addresses, with any subject or body they ask for, most preferred first"`
	OneClick        bool              `json:"one_click" jsonschema:"true when the sender declares one-click unsubscribe (RFC 8058) for an https address; the sender declares it, and no signature over it is checked"`
}

// Rendered is what every read returns besides its rows: how the text was
// bounded and what continues it.
type Rendered struct {
	// UntrustedText is the readable rendering, with mail content inside
	// blocks delimited by Boundary.
	UntrustedText model.Untrusted `json:"untrusted_text"`
	Boundary      string          `json:"boundary"`
	Budget        int             `json:"budget_chars" jsonschema:"the budget, in characters, of the text in content and untrusted_text; the structured rows are not counted against it"`
	Truncated     bool            `json:"truncated"`
	NextCursor    int             `json:"next_cursor,omitempty"`
	NextOffset    int             `json:"next_offset,omitempty"`
	Omitted       []string        `json:"omitted_ids,omitempty" jsonschema:"ids the text leaves out for the budget, named in the text with how to read them"`
}

// Render implements Renderer for every read that embeds Rendered.
func (r Rendered) Render() string { return string(r.UntrustedText) }

// Page is a listing's continuation.
type Page struct {
	NextPageToken string `json:"next_page_token,omitempty"`
	// ResultSizeEstimate is Gmail's own estimate, passed on as-is. A live
	// run saw 201 for a search that matched 3 (§18 row 33): never a count.
	ResultSizeEstimate int `json:"result_size_estimate" jsonschema:"Gmail's estimate of the total, often far off; not a count"`
	// Complete is false while a next page exists, even when this page
	// came back empty: Gmail can return an empty page with more behind it.
	Complete bool `json:"complete"`
}

// Searched is the query a search ran.
type Searched struct {
	Q      string     `json:"q"`
	After  *time.Time `json:"after,omitempty"`
	Before *time.Time `json:"before,omitempty"`
}

// Cost is the quota a call spent (§4.9).
type Cost struct {
	Units int `json:"units"`
}

// mapSlice maps a slice, never returning nil: an empty list is [] in the
// output, not null.
func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func labelRefs(ls []model.LabelRef) []LabelRef {
	return mapSlice(ls, func(l model.LabelRef) LabelRef { return LabelRef{ID: l.ID, Name: l.Name} })
}

func messageMeta(m model.Message) MessageMeta {
	meta := MessageMeta{
		ID: m.ID, ThreadID: m.ThreadID, UntrustedRFC822MessageID: m.RFC822MessageID, Date: m.Date,
		Labels: labelRefs(m.Labels), UntrustedFrom: model.UntrustedAddresses(m.From), UntrustedTo: model.UntrustedAddresses(m.To),
		UntrustedCc: model.UntrustedAddresses(m.Cc), UntrustedSubject: m.Subject, UntrustedSnippet: m.Snippet,
		HasAttachments:     m.HasAttachments,
		HiddenCharsRemoved: m.Body.HiddenChars() + m.HeaderHidden + m.SnippetHidden,
		LinkMismatches:     len(m.Body.Mismatches()),
		Unsubscribe:        unsubscribe(m.Unsubscribe),
	}
	for _, a := range m.Attachments {
		meta.Attachments = append(meta.Attachments, attachment(a))
	}
	return meta
}

func attachment(a mime.Attachment) Attachment {
	return Attachment{
		PartID: a.PartID, UntrustedFilename: model.Untrusted(a.Filename), UntrustedMimeType: model.Untrusted(a.MimeType),
		Size: a.Size, AttachmentID: a.AttachmentID, Inline: a.Inline,
		UntrustedCalendarMethod: model.Untrusted(a.CalendarMethod), Invitation: invitation(a.Invitation),
	}
}

func invitation(i *mime.Invitation) *Invitation {
	if i == nil {
		return nil
	}
	return &Invitation{
		Method: i.Method, UntrustedUID: model.Untrusted(i.UID), Sequence: i.Sequence,
		UntrustedRecurrenceID: model.Untrusted(i.RecurrenceID), UntrustedSummary: model.Untrusted(i.Summary),
		UntrustedOrganizer: model.Untrusted(i.Organizer), UntrustedStart: model.Untrusted(i.Start),
		UntrustedEnd: model.Untrusted(i.End), UntrustedTimeZone: model.Untrusted(i.TimeZone),
		AllDay: i.AllDay, Events: i.Events,
	}
}

func unsubscribe(u model.Unsubscribe) *Unsubscribe {
	if u.Empty() {
		return nil
	}
	return &Unsubscribe{UntrustedURLs: u.URLs, UntrustedMailto: u.Mailto, OneClick: u.OneClick}
}

func page(token string, estimate int) Page {
	return Page{NextPageToken: token, ResultSizeEstimate: estimate, Complete: token == ""}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
