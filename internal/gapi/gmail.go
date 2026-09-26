package gapi

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/mmedum/google-mail-mcp/internal/gmail"
)

// Format is how much of a message Google returns (messages.get "format").
type Format string

// The four formats the discovery document enumerates.
const (
	FormatMinimal  Format = "minimal"
	FormatMetadata Format = "metadata"
	FormatFull     Format = "full"
	FormatRaw      Format = "raw"
)

// ListOptions narrow a message, thread or draft listing.
type ListOptions struct {
	// Q is a Gmail search, passed through verbatim (architecture §6.3).
	Q string
	// LabelIDs must all be present on a result.
	LabelIDs []string
	// IncludeSpamTrash includes SPAM and TRASH.
	IncludeSpamTrash bool
	// Max is the page size; zero leaves Google's default.
	Max int
	// PageToken continues a previous listing.
	PageToken string
}

func (o ListOptions) query() url.Values {
	q := url.Values{}
	if o.Q != "" {
		q.Set("q", o.Q)
	}
	for _, id := range o.LabelIDs {
		q.Add("labelIds", id)
	}
	if o.IncludeSpamTrash {
		q.Set("includeSpamTrash", "true")
	}
	if o.Max > 0 {
		q.Set("maxResults", strconv.Itoa(o.Max))
	}
	if o.PageToken != "" {
		q.Set("pageToken", o.PageToken)
	}
	return q
}

// formatQuery asks for a format, and for the named headers when the
// format is metadata.
func formatQuery(f Format, headers []string) url.Values {
	q := url.Values{}
	if f != "" {
		q.Set("format", string(f))
	}
	if f == FormatMetadata {
		for _, h := range headers {
			q.Add("metadataHeaders", h)
		}
	}
	return q
}

// Profile reads the signed-in account's address, totals and current
// history id.
func (c *Client) Profile(ctx context.Context) (*gmail.Profile, error) {
	var out gmail.Profile
	err := c.Do(ctx, Call{ID: "gmail.users.getProfile", Method: http.MethodGet, Path: "profile"}, &out)
	return &out, err
}

// ListMessages lists message ids matching o. Each entry carries only its
// id and thread id; the caller reads what it shows.
func (c *Client) ListMessages(ctx context.Context, o ListOptions) (*gmail.ListMessagesResponse, error) {
	var out gmail.ListMessagesResponse
	err := c.Do(ctx, Call{ID: "gmail.users.messages.list", Method: http.MethodGet, Path: "messages", Query: o.query()}, &out)
	return &out, err
}

// GetMessage reads one message in format f. headers applies to
// FormatMetadata only.
func (c *Client) GetMessage(ctx context.Context, id string, f Format, headers ...string) (*gmail.Message, error) {
	var out gmail.Message
	err := c.Do(ctx, Call{ID: "gmail.users.messages.get", Method: http.MethodGet, Path: "messages/{}",
		Args: []string{id}, Query: formatQuery(f, headers)}, &out)
	return &out, err
}

// GetAttachment reads a part stored behind an attachment id: an
// attachment, or a body part Google chose not to inline.
func (c *Client) GetAttachment(ctx context.Context, messageID, attachmentID string) (*gmail.MessagePartBody, error) {
	var out gmail.MessagePartBody
	err := c.Do(ctx, Call{ID: "gmail.users.messages.attachments.get", Method: http.MethodGet,
		Path: "messages/{}/attachments/{}", Args: []string{messageID, attachmentID}}, &out)
	return &out, err
}

// ListThreads lists thread ids matching o, each with Gmail's snippet.
func (c *Client) ListThreads(ctx context.Context, o ListOptions) (*gmail.ListThreadsResponse, error) {
	var out gmail.ListThreadsResponse
	err := c.Do(ctx, Call{ID: "gmail.users.threads.list", Method: http.MethodGet, Path: "threads", Query: o.query()}, &out)
	return &out, err
}

// GetThread reads a thread and its messages in format f.
func (c *Client) GetThread(ctx context.Context, id string, f Format, headers ...string) (*gmail.Thread, error) {
	var out gmail.Thread
	err := c.Do(ctx, Call{ID: "gmail.users.threads.get", Method: http.MethodGet, Path: "threads/{}",
		Args: []string{id}, Query: formatQuery(f, headers)}, &out)
	return &out, err
}

// ListLabels lists every label, without message counts.
func (c *Client) ListLabels(ctx context.Context) (*gmail.ListLabelsResponse, error) {
	var out gmail.ListLabelsResponse
	err := c.Do(ctx, Call{ID: "gmail.users.labels.list", Method: http.MethodGet, Path: "labels"}, &out)
	return &out, err
}

// GetLabel reads one label with its message and thread counts, which
// the listing leaves out.
func (c *Client) GetLabel(ctx context.Context, id string) (*gmail.Label, error) {
	var out gmail.Label
	err := c.Do(ctx, Call{ID: "gmail.users.labels.get", Method: http.MethodGet, Path: "labels/{}", Args: []string{id}}, &out)
	return &out, err
}

// ListDrafts lists draft ids matching o; LabelIDs does not apply.
func (c *Client) ListDrafts(ctx context.Context, o ListOptions) (*gmail.ListDraftsResponse, error) {
	var out gmail.ListDraftsResponse
	o.LabelIDs = nil
	err := c.Do(ctx, Call{ID: "gmail.users.drafts.list", Method: http.MethodGet, Path: "drafts", Query: o.query()}, &out)
	return &out, err
}

// GetDraft reads one draft and the message inside it.
func (c *Client) GetDraft(ctx context.Context, id string, f Format) (*gmail.Draft, error) {
	var out gmail.Draft
	err := c.Do(ctx, Call{ID: "gmail.users.drafts.get", Method: http.MethodGet, Path: "drafts/{}",
		Args: []string{id}, Query: formatQuery(f, nil)}, &out)
	return &out, err
}

// HistoryOptions narrow a history listing.
type HistoryOptions struct {
	// StartHistoryID is where the listing starts, exclusive. Required.
	StartHistoryID string
	// LabelID keeps only records about messages carrying this label.
	LabelID string
	// Types keeps only these record types: messageAdded,
	// messageDeleted, labelAdded, labelRemoved. Empty keeps all.
	Types []string
	// Max is the page size; zero leaves Google's default of 100.
	Max int
	// PageToken continues a previous listing.
	PageToken string
}

// ListHistory lists the mailbox's change records after a history id. A
// start older than Gmail keeps is answered 404, which is an expired
// cursor rather than a missing mailbox (§2.8); the caller tells them
// apart, since only it knows it asked for history.
func (c *Client) ListHistory(ctx context.Context, o HistoryOptions) (*gmail.ListHistoryResponse, error) {
	q := url.Values{"startHistoryId": {o.StartHistoryID}}
	if o.LabelID != "" {
		q.Set("labelId", o.LabelID)
	}
	for _, t := range o.Types {
		q.Add("historyTypes", t)
	}
	if o.Max > 0 {
		q.Set("maxResults", strconv.Itoa(o.Max))
	}
	if o.PageToken != "" {
		q.Set("pageToken", o.PageToken)
	}
	var out gmail.ListHistoryResponse
	err := c.Do(ctx, Call{ID: "gmail.users.history.list", Method: http.MethodGet, Path: "history", Query: q}, &out)
	return &out, err
}

// Vacation reads the vacation responder.
func (c *Client) Vacation(ctx context.Context) (*gmail.VacationSettings, error) {
	var out gmail.VacationSettings
	err := c.Do(ctx, Call{ID: "gmail.users.settings.getVacation", Method: http.MethodGet, Path: "settings/vacation"}, &out)
	return &out, err
}

// AutoForwarding reads whether all incoming mail is forwarded, and where.
func (c *Client) AutoForwarding(ctx context.Context) (*gmail.AutoForwarding, error) {
	var out gmail.AutoForwarding
	err := c.Do(ctx, Call{ID: "gmail.users.settings.getAutoForwarding", Method: http.MethodGet,
		Path: "settings/autoForwarding"}, &out)
	return &out, err
}

// ForwardingAddresses lists the addresses mail may be forwarded to.
func (c *Client) ForwardingAddresses(ctx context.Context) (*gmail.ListForwardingAddressesResponse, error) {
	var out gmail.ListForwardingAddressesResponse
	err := c.Do(ctx, Call{ID: "gmail.users.settings.forwardingAddresses.list", Method: http.MethodGet,
		Path: "settings/forwardingAddresses"}, &out)
	return &out, err
}

// Imap reads IMAP access.
func (c *Client) Imap(ctx context.Context) (*gmail.ImapSettings, error) {
	var out gmail.ImapSettings
	err := c.Do(ctx, Call{ID: "gmail.users.settings.getImap", Method: http.MethodGet, Path: "settings/imap"}, &out)
	return &out, err
}

// Pop reads POP access.
func (c *Client) Pop(ctx context.Context) (*gmail.PopSettings, error) {
	var out gmail.PopSettings
	err := c.Do(ctx, Call{ID: "gmail.users.settings.getPop", Method: http.MethodGet, Path: "settings/pop"}, &out)
	return &out, err
}

// Language reads the display language.
func (c *Client) Language(ctx context.Context) (*gmail.LanguageSettings, error) {
	var out gmail.LanguageSettings
	err := c.Do(ctx, Call{ID: "gmail.users.settings.getLanguage", Method: http.MethodGet, Path: "settings/language"}, &out)
	return &out, err
}

// SendAs lists the addresses the account may send as, its own primary
// address included.
func (c *Client) SendAs(ctx context.Context) (*gmail.ListSendAsResponse, error) {
	var out gmail.ListSendAsResponse
	err := c.Do(ctx, Call{ID: "gmail.users.settings.sendAs.list", Method: http.MethodGet, Path: "settings/sendAs"}, &out)
	return &out, err
}

// Filters lists the account's filters.
func (c *Client) Filters(ctx context.Context) (*gmail.ListFiltersResponse, error) {
	var out gmail.ListFiltersResponse
	err := c.Do(ctx, Call{ID: "gmail.users.settings.filters.list", Method: http.MethodGet, Path: "settings/filters"}, &out)
	return &out, err
}

// DownloadAttachment streams a part stored behind an attachment id into
// w, decoded, and returns how many bytes it wrote. The part is never
// held whole in memory (§7.3). w is truncated through reset before each
// attempt, so a retried read never appends to a partial one.
func (c *Client) DownloadAttachment(ctx context.Context, messageID, attachmentID string, reset func() (io.Writer, error)) (int64, error) {
	var n int64
	err := c.Stream(ctx, Call{ID: "gmail.users.messages.attachments.get", Method: http.MethodGet,
		Path: "messages/{}/attachments/{}", Args: []string{messageID, attachmentID}},
		func(body io.Reader) error {
			w, err := reset()
			if err != nil {
				return err
			}
			n, err = streamData(body, w)
			return err
		})
	return n, err
}

// UploadThreshold is the message size above which a draft is sent as a
// multipart upload rather than in the JSON raw field. Google recommends
// the simple path only up to 5 MB (§7.4).
const UploadThreshold = 5 << 20

// Uploads reports whether a message of n bytes goes as an upload.
func Uploads(n int) bool { return n > UploadThreshold }

// draftCall is a create or update of a draft: the message's thread, and
// its bytes as base64url in the JSON body, or beside it as an upload.
func draftCall(call Call, threadID string, raw []byte) Call {
	m := &gmail.Message{ThreadID: threadID}
	if Uploads(len(raw)) {
		call.Media = raw
	} else {
		m.Raw = base64.URLEncoding.EncodeToString(raw)
	}
	call.Body = gmail.Draft{Message: m}
	return call
}

// CreateDraft saves a new draft of raw RFC 5322 bytes, in threadID when
// it is set. Never repeated after an ambiguous failure: a second
// attempt whose first landed is a second draft.
func (c *Client) CreateDraft(ctx context.Context, threadID string, raw []byte) (*gmail.Draft, error) {
	var out gmail.Draft
	err := c.Do(ctx, draftCall(Call{ID: "gmail.users.drafts.create", Method: http.MethodPost, Path: "drafts"}, threadID, raw), &out)
	return &out, err
}

// UpdateDraft replaces a draft's message whole: drafts.update is a PUT
// with no ETag (§2.4), so the caller checks its witness first (§4.4).
func (c *Client) UpdateDraft(ctx context.Context, id, threadID string, raw []byte) (*gmail.Draft, error) {
	var out gmail.Draft
	err := c.Do(ctx, draftCall(Call{ID: "gmail.users.drafts.update", Method: http.MethodPut, Path: "drafts/{}",
		Args: []string{id}}, threadID, raw), &out)
	return &out, err
}

// DeleteDraft deletes a draft permanently; it does not go to the trash.
func (c *Client) DeleteDraft(ctx context.Context, id string) error {
	return c.Do(ctx, Call{ID: "gmail.users.drafts.delete", Method: http.MethodDelete, Path: "drafts/{}",
		Args: []string{id}}, nil)
}

// labelsRepeat is why a label change may be sent twice: applying a label
// already applied, or removing one already gone, changes nothing (§11).
const labelsRepeat = "a label added or removed twice ends in the same labels"

// ModifyMessage adds and removes labels on one message and returns the
// message with its labels after.
func (c *Client) ModifyMessage(ctx context.Context, id string, req gmail.ModifyMessageRequest) (*gmail.Message, error) {
	var out gmail.Message
	err := c.Do(ctx, Call{ID: "gmail.users.messages.modify", Method: http.MethodPost, Path: "messages/{}/modify",
		Args: []string{id}, Body: req, Repeatable: labelsRepeat}, &out)
	return &out, err
}

// ModifyThread adds and removes labels on every message of a thread.
func (c *Client) ModifyThread(ctx context.Context, id string, req gmail.ModifyThreadRequest) (*gmail.Thread, error) {
	var out gmail.Thread
	err := c.Do(ctx, Call{ID: "gmail.users.threads.modify", Method: http.MethodPost, Path: "threads/{}/modify",
		Args: []string{id}, Body: req, Repeatable: labelsRepeat}, &out)
	return &out, err
}

// trashRepeat is why a trash or untrash may be sent twice.
const trashRepeat = "trashing what is in the trash, or restoring what is not, leaves it where it is"

// TrashMessage moves a message to the trash, where Gmail keeps it for 30
// days (§4.6).
func (c *Client) TrashMessage(ctx context.Context, id string) (*gmail.Message, error) {
	var out gmail.Message
	err := c.Do(ctx, Call{ID: "gmail.users.messages.trash", Method: http.MethodPost, Path: "messages/{}/trash",
		Args: []string{id}, Repeatable: trashRepeat}, &out)
	return &out, err
}

// UntrashMessage takes a message out of the trash.
func (c *Client) UntrashMessage(ctx context.Context, id string) (*gmail.Message, error) {
	var out gmail.Message
	err := c.Do(ctx, Call{ID: "gmail.users.messages.untrash", Method: http.MethodPost, Path: "messages/{}/untrash",
		Args: []string{id}, Repeatable: trashRepeat}, &out)
	return &out, err
}

// TrashThread moves every message of a thread to the trash.
func (c *Client) TrashThread(ctx context.Context, id string) (*gmail.Thread, error) {
	var out gmail.Thread
	err := c.Do(ctx, Call{ID: "gmail.users.threads.trash", Method: http.MethodPost, Path: "threads/{}/trash",
		Args: []string{id}, Repeatable: trashRepeat}, &out)
	return &out, err
}

// UntrashThread takes every message of a thread out of the trash.
func (c *Client) UntrashThread(ctx context.Context, id string) (*gmail.Thread, error) {
	var out gmail.Thread
	err := c.Do(ctx, Call{ID: "gmail.users.threads.untrash", Method: http.MethodPost, Path: "threads/{}/untrash",
		Args: []string{id}, Repeatable: trashRepeat}, &out)
	return &out, err
}

// CreateLabel creates a user label. Not repeated after an ambiguous
// failure: a second attempt whose first landed would be refused as a
// taken name and read as one.
func (c *Client) CreateLabel(ctx context.Context, l gmail.Label) (*gmail.Label, error) {
	var out gmail.Label
	err := c.Do(ctx, Call{ID: "gmail.users.labels.create", Method: http.MethodPost, Path: "labels", Body: l}, &out)
	return &out, err
}

// PatchLabel changes the fields of a label that l sets, and no others
// (§4.4: labels are patched, never replaced).
func (c *Client) PatchLabel(ctx context.Context, id string, l gmail.Label) (*gmail.Label, error) {
	var out gmail.Label
	err := c.Do(ctx, Call{ID: "gmail.users.labels.patch", Method: http.MethodPatch, Path: "labels/{}",
		Args: []string{id}, Body: l}, &out)
	return &out, err
}
