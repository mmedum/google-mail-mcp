//go:build evals

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// The Messages API client is hand-written for the reason internal/gapi
// is (CLAUDE.md rule 10): the official Go SDK would add eleven modules to
// the module graph of a binary shipped with an SBOM, a license allow-list
// and govulncheck on every commit, to make one shape of request from
// tooling that never ships. The shape is four fields and a loop.
//
// The API key is read from the environment only. It is never printed,
// written to a file, or taken as a flag, where it would land in a shell
// history.

const (
	messagesURL  = "https://api.anthropic.com/v1/messages"
	anthropicVer = "2023-06-01"
	defaultModel = "claude-opus-5"
	maxTokens    = 16000
	// attempts is how many times one request is tried when the API is
	// rate limited or overloaded.
	attempts = 4
)

// claudeClient calls the Messages API.
type claudeClient struct {
	url   string
	key   string
	model string
	http  *http.Client
	// sleep waits between attempts; a test replaces it.
	sleep func(context.Context, time.Duration) error
}

func newClaude(model string) (*claudeClient, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY is not set; the evals need one and nothing else here does")
	}
	return &claudeClient{url: messagesURL, key: key, model: model, http: &http.Client{Timeout: 10 * time.Minute}, sleep: sleepCtx}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// toolDef is one tool as the Messages API takes it: exactly what
// tools/list gives, so the model reads the server's own descriptions.
// An eval that reworded them would score the rewording.
type toolDef struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

// message is one turn. Content is raw so an assistant turn goes back
// exactly as it arrived: thinking blocks must be echoed unchanged.
type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type request struct {
	Model string `json:"model"`
	// CacheControl at the top level caches up to the request's last
	// block, so each turn reads the conversation so far from the cache.
	// The last tool carries a breakpoint of its own, a read point for the
	// static prefix that every task and turn shares.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
	MaxTokens    int           `json:"max_tokens"`
	System       string        `json:"system,omitempty"`
	Messages     []message     `json:"messages"`
	Tools        []toolDef     `json:"tools,omitempty"`
	Thinking     *thinking     `json:"thinking,omitempty"`
}

type thinking struct {
	Type string `json:"type"`
}

type response struct {
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
	Usage      usage           `json:"usage"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// usage is what a run costs. The cache counters show whether the tool
// block, resent every turn, was read from the cache.
type usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

func (u *usage) add(v usage) {
	u.InputTokens += v.InputTokens
	u.OutputTokens += v.OutputTokens
	u.CacheCreationTokens += v.CacheCreationTokens
	u.CacheReadTokens += v.CacheReadTokens
}

// block is one content block, read for the loop; the array itself goes
// back untouched.
type block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// retryable is a status worth waiting out: a rate limit, or a server
// error, 529 overloaded among them. Anything else is returned, since an
// eval that retried a bad request would score the retry.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// send makes one request, waiting out a rate limit or an overload with
// backoff, and honoring retry-after when the API gives it.
func (c *claudeClient) send(ctx context.Context, req request) (*response, error) {
	req.Model = c.model
	req.MaxTokens = maxTokens
	req.Thinking = &thinking{Type: "adaptive"}
	req.CacheControl = &cacheControl{Type: "ephemeral"}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	wait := 5 * time.Second
	var last error
	for attempt := range attempts {
		if attempt > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
			wait *= 2
		}
		out, status, after, err := c.post(ctx, body)
		switch {
		case err == nil:
			return out, nil
		case !retryable(status):
			return nil, err
		}
		last = err
		if after > 0 {
			wait = after
		}
	}
	return nil, fmt.Errorf("%w (after %d attempts)", last, attempts)
}

func (c *claudeClient) post(ctx context.Context, body []byte) (*response, int, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVer)
	httpReq.Header.Set("x-api-key", c.key)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A dropped connection is worth another attempt, like a 5xx.
		return nil, http.StatusServiceUnavailable, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	after := time.Duration(0)
	if s, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil && s > 0 {
		after = time.Duration(s) * time.Second
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, after, err
	}
	var out response
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, resp.StatusCode, after, fmt.Errorf("messages: %d, and the answer is not JSON", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != nil {
			return nil, resp.StatusCode, after, fmt.Errorf("messages: %d %s: %s", resp.StatusCode, out.Error.Type, out.Error.Message)
		}
		return nil, resp.StatusCode, after, fmt.Errorf("messages: %d", resp.StatusCode)
	}
	return &out, resp.StatusCode, after, nil
}

// blocks decodes the content array.
func (r *response) blocks() ([]block, error) {
	var out []block
	err := json.Unmarshal(r.Content, &out)
	return out, err
}

// userTurn builds a user turn from content blocks. Every tool result of
// one assistant turn goes back in one user turn: splitting them teaches
// the model to stop calling tools in parallel.
func userTurn(content []map[string]any) (message, error) {
	b, err := json.Marshal(content)
	return message{Role: "user", Content: b}, err
}
