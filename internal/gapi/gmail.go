package gapi

import (
	"context"
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
