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
	"time"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/internal/app"
	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/credentials"
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

// call sends one request and decodes the reply into out. The status is
// returned for the probes, which judge it rather than fail on it.
func (m *restMailbox) call(ctx context.Context, method, path string, q url.Values, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	u := m.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		// The error names the URL, which carries only ids the run made;
		// the transcript redacts those anyway.
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out != nil && len(data) > 0 {
		return resp.StatusCode, json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

func (m *restMailbox) CreateLabel(ctx context.Context, name string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	_, err := m.call(ctx, http.MethodPost, "labels", nil,
		map[string]string{"name": name, "labelListVisibility": "labelShow", "messageListVisibility": "show"}, &out)
	return out.ID, err
}

func (m *restMailbox) Insert(ctx context.Context, labelID string, raw []byte) (string, string, error) {
	var out struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	_, err := m.call(ctx, http.MethodPost, "messages", url.Values{"internalDateSource": {"receivedTime"}},
		map[string]any{"raw": base64.URLEncoding.EncodeToString(raw), "labelIds": []string{labelID}}, &out)
	return out.ID, out.ThreadID, err
}

func (m *restMailbox) Trash(ctx context.Context, messageID string) error {
	_, err := m.call(ctx, http.MethodPost, "messages/"+url.PathEscape(messageID)+"/trash", nil, nil, nil)
	return err
}

func (m *restMailbox) DeleteLabel(ctx context.Context, labelID string) error {
	_, err := m.call(ctx, http.MethodDelete, "labels/"+url.PathEscape(labelID), nil, nil, nil)
	return err
}

func (m *restMailbox) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	_, err := m.call(ctx, http.MethodPost, "drafts", nil,
		map[string]any{"message": map[string]string{"raw": base64.URLEncoding.EncodeToString(raw)}}, &out)
	return out.ID, err
}

func (m *restMailbox) DeleteDraft(ctx context.Context, draftID string) error {
	_, err := m.call(ctx, http.MethodDelete, "drafts/"+url.PathEscape(draftID), nil, nil, nil)
	return err
}

// Probe makes one call for a spike and returns only its status. It reads
// nothing back: a probe asks how Gmail answers, never what it holds.
func (m *restMailbox) Probe(ctx context.Context, method, path string, q url.Values) int {
	status, _ := m.call(ctx, method, path, q, nil, nil)
	return status
}

func (m *restMailbox) InternalDate(ctx context.Context, messageID string) (int64, error) {
	var out struct {
		InternalDate string `json:"internalDate"`
	}
	if _, err := m.call(ctx, http.MethodGet, "messages/"+url.PathEscape(messageID),
		url.Values{"format": {"minimal"}}, nil, &out); err != nil {
		return 0, err
	}
	return strconv.ParseInt(out.InternalDate, 10, 64)
}
