//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/internal/app"
	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/credentials"
	"github.com/mmedum/google-mail-mcp/internal/mime"
)

// gmailBase is where the driver's own calls go. They never go through
// the server: the driver writes the fixture the server is then held to.
const gmailBase = "https://gmail.googleapis.com/gmail/v1/users/me/"

// restMailbox is the mailbox over Gmail's REST API, signed in as the
// profile the server will run as.
type restMailbox struct {
	http *http.Client
	base string
}

// openMailbox signs in as profile the way the binary does, through the
// profile's stored token, and never through a login of its own.
func openMailbox(ctx context.Context, profile string) (*restMailbox, error) {
	env := func(k string) string {
		if k == "GMAIL_PROFILE" {
			return profile
		}
		return os.Getenv(k)
	}
	cfg, err := config.Load(nil, env)
	if err != nil {
		return nil, err
	}
	p, err := app.OpenProfile(cfg, credentials.OSKeyring(), env, func(string) {})
	if err != nil {
		return nil, err
	}
	ts, _, err := p.TokenSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("profile %s has no usable token; run google-mail-mcp login: %w", profile, err)
	}
	hc := oauth2.NewClient(ctx, ts)
	hc.Timeout = time.Minute
	return &restMailbox{http: hc, base: gmailBase}, nil
}

// call sends one request and decodes a successful reply into out.
func (m *restMailbox) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	_, data, err := m.callRaw(ctx, method, path, q, body)
	if err != nil || out == nil || len(data) == 0 {
		return err
	}
	return json.Unmarshal(data, out)
}

// callRaw sends one request and returns the reply's body whatever the
// status, so a probe can read Google's reason for a refusal.
func (m *restMailbox) callRaw(ctx context.Context, method, path string, q url.Values, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := m.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		// The error names the URL, which carries only ids the run made;
		// the transcript redacts those anyway.
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, data, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return resp.StatusCode, data, nil
}

func (m *restMailbox) CreateLabel(ctx context.Context, name string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := m.call(ctx, http.MethodPost, "labels", nil,
		map[string]string{"name": name, "labelListVisibility": "labelShow", "messageListVisibility": "show"}, &out)
	return out.ID, err
}

func (m *restMailbox) Insert(ctx context.Context, labelID string, raw []byte) (string, string, error) {
	var out struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	err := m.call(ctx, http.MethodPost, "messages", url.Values{"internalDateSource": {"receivedTime"}},
		map[string]any{"raw": base64.URLEncoding.EncodeToString(raw), "labelIds": []string{labelID}}, &out)
	return out.ID, out.ThreadID, err
}

func (m *restMailbox) Trash(ctx context.Context, messageID string) error {
	err := m.call(ctx, http.MethodPost, "messages/"+url.PathEscape(messageID)+"/trash", nil, nil, nil)
	return err
}

func (m *restMailbox) DeleteLabel(ctx context.Context, labelID string) error {
	err := m.call(ctx, http.MethodDelete, "labels/"+url.PathEscape(labelID), nil, nil, nil)
	return err
}

func (m *restMailbox) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := m.call(ctx, http.MethodPost, "drafts", nil,
		map[string]any{"message": map[string]string{"raw": base64.URLEncoding.EncodeToString(raw)}}, &out)
	return out.ID, err
}

func (m *restMailbox) DeleteDraft(ctx context.Context, draftID string) error {
	err := m.call(ctx, http.MethodDelete, "drafts/"+url.PathEscape(draftID), nil, nil, nil)
	return err
}

// Probe makes one call for a spike. It reads back only how Gmail
// answered: the status, Google's reason and message on a refusal, and the
// id of what it created. A probe asks how Gmail answers, never what the
// mailbox holds.
func (m *restMailbox) Probe(ctx context.Context, method, path string, q url.Values, body any) probeResult {
	var out struct {
		ID    string `json:"id"`
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	status, data, _ := m.callRaw(ctx, method, path, q, body)
	_ = json.Unmarshal(data, &out)
	r := probeResult{status: status, id: out.ID, message: out.Error.Message}
	if len(out.Error.Errors) > 0 {
		r.reason = out.Error.Errors[0].Reason
	}
	return r
}

// HistoryID reads the mailbox's current history id.
func (m *restMailbox) HistoryID(ctx context.Context) (string, error) {
	var out struct {
		HistoryID string `json:"historyId"`
	}
	err := m.call(ctx, http.MethodGet, "profile", nil, nil, &out)
	return out.HistoryID, err
}

func (m *restMailbox) InternalDate(ctx context.Context, messageID string) (int64, error) {
	var out struct {
		InternalDate string `json:"internalDate"`
	}
	if err := m.call(ctx, http.MethodGet, "messages/"+url.PathEscape(messageID),
		url.Values{"format": {"minimal"}}, nil, &out); err != nil {
		return 0, err
	}
	return strconv.ParseInt(out.InternalDate, 10, 64)
}

func (m *restMailbox) UpdateDraft(ctx context.Context, draftID string, raw []byte) (string, error) {
	var out struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	err := m.call(ctx, http.MethodPut, "drafts/"+url.PathEscape(draftID), nil,
		map[string]any{"message": map[string]string{"raw": base64.URLEncoding.EncodeToString(raw)}}, &out)
	return out.Message.ID, err
}

func (m *restMailbox) DraftMessageID(ctx context.Context, draftID string) (string, error) {
	var out struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	err := m.call(ctx, http.MethodGet, "drafts/"+url.PathEscape(draftID), url.Values{"format": {"minimal"}}, nil, &out)
	return out.Message.ID, err
}

func (m *restMailbox) Send(ctx context.Context, o mime.Outgoing, to, threadID string) (string, string, error) {
	name := ""
	if len(o.To) > 0 {
		name = o.To[0].Name
	}
	o.To, o.Cc, o.Bcc = []mime.Address{{Name: name, Email: to}}, nil, nil
	raw, err := mime.Build(o)
	if err != nil {
		return "", "", err
	}
	var out struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	body := map[string]string{"raw": base64.URLEncoding.EncodeToString(raw)}
	if threadID != "" {
		body["threadId"] = threadID
	}
	err = m.call(ctx, http.MethodPost, "messages/send", nil, body, &out)
	return out.ID, out.ThreadID, err
}

func (m *restMailbox) Label(ctx context.Context, messageID, labelID string) error {
	return m.call(ctx, http.MethodPost, "messages/"+url.PathEscape(messageID)+"/modify", nil,
		map[string]any{"addLabelIds": []string{labelID}}, nil)
}

func (m *restMailbox) SentCopy(ctx context.Context, messageID string) (string, []byte, error) {
	var out struct {
		ThreadID string `json:"threadId"`
		Raw      string `json:"raw"`
	}
	if err := m.call(ctx, http.MethodGet, "messages/"+url.PathEscape(messageID), url.Values{"format": {"raw"}}, nil, &out); err != nil {
		return "", nil, err
	}
	raw, err := base64.URLEncoding.DecodeString(out.Raw)
	return out.ThreadID, raw, err
}

func (m *restMailbox) Header(ctx context.Context, messageID, name string) (string, error) {
	var out struct {
		Payload struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
		} `json:"payload"`
	}
	if err := m.call(ctx, http.MethodGet, "messages/"+url.PathEscape(messageID),
		url.Values{"format": {"metadata"}, "metadataHeaders": {name}}, nil, &out); err != nil {
		return "", err
	}
	for _, h := range out.Payload.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value, nil
		}
	}
	return "", nil
}

func (m *restMailbox) Account(ctx context.Context) (string, error) {
	var out struct {
		EmailAddress string `json:"emailAddress"`
	}
	err := m.call(ctx, http.MethodGet, "profile", nil, nil, &out)
	return out.EmailAddress, err
}
