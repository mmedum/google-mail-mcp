// Package gmail holds the Gmail v1 wire types this server reads and
// writes, written by hand from the discovery document (§4.11).
//
// Field names and JSON tags are the discovery document's. A field the
// document types as a string with format int64 or uint64 (historyId,
// internalDate, a vacation's start and end) is a JSON string on the
// wire, so it is a Go string here, with a helper that parses it where a
// caller needs the number. int32 and uint32 fields are JSON numbers.
//
// Only the fields a tool uses are modeled; docs/architecture.md §8b
// records a verdict for every field the document publishes, and
// `scripts/gates api-fields` holds the two together.
package gmail

import "strconv"

// Message is one email. get_message, get_thread (through Thread),
// search_messages (id and threadId from a listing, the rest from
// messages.get format=metadata) and get_draft (through Draft).
type Message struct {
	ID           string   `json:"id,omitempty"`
	ThreadID     string   `json:"threadId,omitempty"`
	LabelIDs     []string `json:"labelIds,omitempty"`
	Snippet      string   `json:"snippet,omitempty"`
	HistoryID    string   `json:"historyId,omitempty"`    // uint64 as a string
	InternalDate string   `json:"internalDate,omitempty"` // int64 ms since the epoch, as a string
	SizeEstimate int32    `json:"sizeEstimate,omitempty"`
	// Payload is the parsed MIME tree, present for format=full and,
	// with headers only, format=metadata.
	Payload *MessagePart `json:"payload,omitempty"`
	// Raw is base64url RFC 5322, present only for format=raw, and what
	// a draft or send writes.
	Raw string `json:"raw,omitempty"`
	// ClassificationLabelValues is Workspace classification: read and
	// shown, never written (§8b).
	ClassificationLabelValues []ClassificationLabelValue `json:"classificationLabelValues,omitempty"`
}

// InternalDateMillis parses InternalDate. ok is false when it is
// absent or not a number.
func (m *Message) InternalDateMillis() (ms int64, ok bool) {
	return parseInt(m.InternalDate)
}

// ClassificationLabelValue is one Workspace classification label on a
// message. Shown by get_message; never written.
type ClassificationLabelValue struct {
	LabelID string                          `json:"labelId,omitempty"`
	Fields  []ClassificationLabelFieldValue `json:"fields,omitempty"`
}

// ClassificationLabelFieldValue is one field of a classification label.
type ClassificationLabelFieldValue struct {
	FieldID   string `json:"fieldId,omitempty"`
	Selection string `json:"selection,omitempty"`
}

// MessagePart is one node of a message's MIME tree. internal/mime walks
// it for get_message and get_thread.
type MessagePart struct {
	PartID   string              `json:"partId,omitempty"`
	MimeType string              `json:"mimeType,omitempty"`
	Filename string              `json:"filename,omitempty"`
	Headers  []MessagePartHeader `json:"headers,omitempty"`
	Body     *MessagePartBody    `json:"body,omitempty"`
	Parts    []MessagePart       `json:"parts,omitempty"`
}

// MessagePartHeader is one header, name and value as they appear in the
// message: an RFC 2047 encoded-word is still encoded.
type MessagePartHeader struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// MessagePartBody is a part's content, either inline in Data (base64url
// of the part after its transfer encoding is removed, still in its own
// charset) or behind AttachmentID, fetched with messages.attachments.get
// — which answers with this same type.
type MessagePartBody struct {
	AttachmentID string `json:"attachmentId,omitempty"`
	Size         int32  `json:"size,omitempty"`
	Data         string `json:"data,omitempty"`
}

// Thread is a conversation. search_threads (listing) and get_thread.
type Thread struct {
	ID        string    `json:"id,omitempty"`
	Snippet   string    `json:"snippet,omitempty"`
	HistoryID string    `json:"historyId,omitempty"` // uint64 as a string
	Messages  []Message `json:"messages,omitempty"`
}

// Draft is an unsent message. list_drafts and get_draft.
type Draft struct {
	ID      string   `json:"id,omitempty"`
	Message *Message `json:"message,omitempty"`
}

// Label is a system or user label. list_labels (labels.list, which
// carries no counts) and labels.get (which does).
type Label struct {
	ID                    string      `json:"id,omitempty"`
	Name                  string      `json:"name,omitempty"`
	Type                  string      `json:"type,omitempty"` // "system" or "user"
	MessageListVisibility string      `json:"messageListVisibility,omitempty"`
	LabelListVisibility   string      `json:"labelListVisibility,omitempty"`
	MessagesTotal         int32       `json:"messagesTotal,omitempty"`
	MessagesUnread        int32       `json:"messagesUnread,omitempty"`
	ThreadsTotal          int32       `json:"threadsTotal,omitempty"`
	ThreadsUnread         int32       `json:"threadsUnread,omitempty"`
	Color                 *LabelColor `json:"color,omitempty"`
}

// Label types.
const (
	LabelTypeSystem = "system"
	LabelTypeUser   = "user"
)

// LabelColor is a user label's color. list_labels shows it.
type LabelColor struct {
	TextColor       string `json:"textColor,omitempty"`
	BackgroundColor string `json:"backgroundColor,omitempty"`
}

// Profile is the account. get_profile.
type Profile struct {
	EmailAddress  string `json:"emailAddress,omitempty"`
	MessagesTotal int32  `json:"messagesTotal,omitempty"`
	ThreadsTotal  int32  `json:"threadsTotal,omitempty"`
	HistoryID     string `json:"historyId,omitempty"` // uint64 as a string
}

// ListMessagesResponse is one page of messages.list. search_messages.
// Messages carry only id and threadId.
type ListMessagesResponse struct {
	Messages           []Message `json:"messages,omitempty"`
	NextPageToken      string    `json:"nextPageToken,omitempty"`
	ResultSizeEstimate uint32    `json:"resultSizeEstimate,omitempty"`
}

// ListThreadsResponse is one page of threads.list. search_threads.
// Threads carry id, snippet and historyId.
type ListThreadsResponse struct {
	Threads            []Thread `json:"threads,omitempty"`
	NextPageToken      string   `json:"nextPageToken,omitempty"`
	ResultSizeEstimate uint32   `json:"resultSizeEstimate,omitempty"`
}

// ListDraftsResponse is one page of drafts.list. list_drafts. Each
// draft carries its id and its message's id and threadId.
type ListDraftsResponse struct {
	Drafts             []Draft `json:"drafts,omitempty"`
	NextPageToken      string  `json:"nextPageToken,omitempty"`
	ResultSizeEstimate uint32  `json:"resultSizeEstimate,omitempty"`
}

// ListLabelsResponse is labels.list. list_labels and every label-name
// resolution.
type ListLabelsResponse struct {
	Labels []Label `json:"labels,omitempty"`
}

// ListHistoryResponse is one page of history.list. list_changes.
// HistoryID is the mailbox's current history id.
type ListHistoryResponse struct {
	History       []History `json:"history,omitempty"`
	NextPageToken string    `json:"nextPageToken,omitempty"`
	HistoryID     string    `json:"historyId,omitempty"` // uint64 as a string
}

// History is one change record. list_changes.
type History struct {
	ID              string                  `json:"id,omitempty"` // uint64 as a string
	Messages        []Message               `json:"messages,omitempty"`
	MessagesAdded   []HistoryMessageAdded   `json:"messagesAdded,omitempty"`
	MessagesDeleted []HistoryMessageDeleted `json:"messagesDeleted,omitempty"`
	LabelsAdded     []HistoryLabelAdded     `json:"labelsAdded,omitempty"`
	LabelsRemoved   []HistoryLabelRemoved   `json:"labelsRemoved,omitempty"`
}

// HistoryMessageAdded records a message added to the mailbox.
type HistoryMessageAdded struct {
	Message *Message `json:"message,omitempty"`
}

// HistoryMessageDeleted records a message deleted permanently.
type HistoryMessageDeleted struct {
	Message *Message `json:"message,omitempty"`
}

// HistoryLabelAdded records labels applied to a message.
type HistoryLabelAdded struct {
	Message  *Message `json:"message,omitempty"`
	LabelIDs []string `json:"labelIds,omitempty"`
}

// HistoryLabelRemoved records labels removed from a message.
type HistoryLabelRemoved struct {
	Message  *Message `json:"message,omitempty"`
	LabelIDs []string `json:"labelIds,omitempty"`
}

// ModifyMessageRequest is messages.modify's body. modify_labels (phase 2).
// The classification fields are left out: changing classification is an
// administrator's decision (§8b).
type ModifyMessageRequest struct {
	AddLabelIDs    []string `json:"addLabelIds,omitempty"`
	RemoveLabelIDs []string `json:"removeLabelIds,omitempty"`
}

// ModifyThreadRequest is threads.modify's body. modify_labels on a
// thread id (phase 2).
type ModifyThreadRequest struct {
	AddLabelIDs    []string `json:"addLabelIds,omitempty"`
	RemoveLabelIDs []string `json:"removeLabelIds,omitempty"`
}

// BatchModifyMessagesRequest is messages.batchModify's body.
// modify_labels on many message ids (phase 2). At most 100 ids (§4.7).
type BatchModifyMessagesRequest struct {
	IDs            []string `json:"ids,omitempty"`
	AddLabelIDs    []string `json:"addLabelIds,omitempty"`
	RemoveLabelIDs []string `json:"removeLabelIds,omitempty"`
}

// VacationSettings is the vacation responder. get_settings (phase 1).
type VacationSettings struct {
	EnableAutoReply       bool   `json:"enableAutoReply,omitempty"`
	ResponseSubject       string `json:"responseSubject,omitempty"`
	ResponseBodyPlainText string `json:"responseBodyPlainText,omitempty"`
	ResponseBodyHTML      string `json:"responseBodyHtml,omitempty"`
	RestrictToContacts    bool   `json:"restrictToContacts,omitempty"`
	RestrictToDomain      bool   `json:"restrictToDomain,omitempty"`
	StartTime             string `json:"startTime,omitempty"` // int64 ms, as a string
	EndTime               string `json:"endTime,omitempty"`   // int64 ms, as a string
}

// AutoForwarding is the account's automatic forwarding. get_settings
// shows it; changing it is written off (§4.1).
type AutoForwarding struct {
	Enabled      bool   `json:"enabled,omitempty"`
	EmailAddress string `json:"emailAddress,omitempty"`
	Disposition  string `json:"disposition,omitempty"`
}

// ForwardingAddress is an address mail may be forwarded to.
// get_settings.
type ForwardingAddress struct {
	ForwardingEmail    string `json:"forwardingEmail,omitempty"`
	VerificationStatus string `json:"verificationStatus,omitempty"`
}

// ListForwardingAddressesResponse is forwardingAddresses.list.
type ListForwardingAddressesResponse struct {
	ForwardingAddresses []ForwardingAddress `json:"forwardingAddresses,omitempty"`
}

// ImapSettings is IMAP access. get_settings.
type ImapSettings struct {
	Enabled         bool   `json:"enabled,omitempty"`
	AutoExpunge     bool   `json:"autoExpunge,omitempty"`
	ExpungeBehavior string `json:"expungeBehavior,omitempty"`
	MaxFolderSize   int32  `json:"maxFolderSize,omitempty"`
}

// PopSettings is POP access. get_settings.
type PopSettings struct {
	AccessWindow string `json:"accessWindow,omitempty"`
	Disposition  string `json:"disposition,omitempty"`
}

// LanguageSettings is the display language. get_settings.
type LanguageSettings struct {
	DisplayLanguage string `json:"displayLanguage,omitempty"`
}

// SendAs is an address the account may send as. get_settings, and the
// account's own addresses a reply-all drops (§4.5). smtpMsa is left
// out: it carries a password field and nothing here needs it.
type SendAs struct {
	SendAsEmail        string `json:"sendAsEmail,omitempty"`
	DisplayName        string `json:"displayName,omitempty"`
	ReplyToAddress     string `json:"replyToAddress,omitempty"`
	Signature          string `json:"signature,omitempty"`
	IsPrimary          bool   `json:"isPrimary,omitempty"`
	IsDefault          bool   `json:"isDefault,omitempty"`
	TreatAsAlias       bool   `json:"treatAsAlias,omitempty"`
	VerificationStatus string `json:"verificationStatus,omitempty"`
}

// ListSendAsResponse is sendAs.list.
type ListSendAsResponse struct {
	SendAs []SendAs `json:"sendAs,omitempty"`
}

// Filter is a mail filter. list_filters (phase 1) shows it and flags a
// forward action.
type Filter struct {
	ID       string          `json:"id,omitempty"`
	Criteria *FilterCriteria `json:"criteria,omitempty"`
	Action   *FilterAction   `json:"action,omitempty"`
}

// FilterCriteria is what a filter matches.
type FilterCriteria struct {
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
	Subject        string `json:"subject,omitempty"`
	Query          string `json:"query,omitempty"`
	NegatedQuery   string `json:"negatedQuery,omitempty"`
	HasAttachment  bool   `json:"hasAttachment,omitempty"`
	ExcludeChats   bool   `json:"excludeChats,omitempty"`
	Size           int32  `json:"size,omitempty"`
	SizeComparison string `json:"sizeComparison,omitempty"`
}

// FilterAction is what a filter does. Forward is read and flagged,
// never written (§8b).
type FilterAction struct {
	AddLabelIDs    []string `json:"addLabelIds,omitempty"`
	RemoveLabelIDs []string `json:"removeLabelIds,omitempty"`
	Forward        string   `json:"forward,omitempty"`
}

// ListFiltersResponse is filters.list. The array's name is singular in
// the discovery document.
type ListFiltersResponse struct {
	Filter []Filter `json:"filter,omitempty"`
}

// ParseHistoryID parses a uint64 history id as the wire carries it.
func ParseHistoryID(s string) (uint64, bool) {
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

func parseInt(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}
