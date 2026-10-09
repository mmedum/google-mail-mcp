package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/v2/internal/config"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/server"
	"github.com/mmedum/google-mail-mcp/v2/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/v2/internal/tools"
)

// minTools is the floor on how many tools this test drives: the whole
// phase 1 surface, so a test that drives nothing cannot pass for one
// that drives everything (standard, preamble). Raise it with the surface.
var minTools = 12

// The rule (§9.2): a log line says when a call happened, what it was
// and how it ended. It never carries an address, a subject, a body, a
// snippet, a file name, a label name, a query, a token, or a whole id.
const (
	canaryText    = "CANARY-mail-text"
	canaryEmail   = "canary.person@example.com"
	canaryQuery   = "CANARY-search-term"
	canarySubject = "CANARY-subject"
	canaryLabel   = "CANARY-label-name"
	canaryFile    = "CANARY-attachment.pdf"
	canaryReason  = "CANARY-refusal-detail"
	canaryToken   = "CANARY-access-token"
	canaryID      = "18c2f0a1b2c3d4e5"
	canaryURL     = "canary-unsubscribe.example"
)

var forbidden = map[string]string{
	canaryText:      "message text",
	canaryEmail:     "an email address",
	"canary.person": "the local part of an address",
	canaryQuery:     "a search query",
	canarySubject:   "a subject",
	canaryLabel:     "a label name",
	canaryFile:      "an attachment name",
	canaryReason:    "Google's refusal text, which can quote the request",
	canaryToken:     "the access token",
	canaryID:        "a whole message id (only six characters may be logged)",
	canaryURL:       "an address a header names",
}

// canaryGmail answers every path with canary-laden mail, and every
// third request with a refusal quoting a canary, so both the success
// and the error paths are exercised.
func canaryGmail(t *testing.T) *httptest.Server {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n.Add(1)%3 == 0 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprintf(w, `{"error":{"code":403,"message":%q}}`, canaryReason+" "+canaryEmail)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%[1]q,"threadId":%[1]q,"historyId":"1","emailAddress":%[2]q,
			"snippet":%[3]q,"labelIds":["INBOX"],"messages":[{"id":%[1]q,"threadId":%[1]q}],
			"threads":[{"id":%[1]q,"snippet":%[3]q}],"labels":[{"id":"Label_1","name":%[4]q}],
			"payload":{"mimeType":"text/plain","filename":%[5]q,"headers":[{"name":"Subject","value":%[6]q},
			{"name":"From","value":%[2]q},{"name":"List-Unsubscribe","value":"<https://%[7]s/u>, <mailto:%[2]s>"},
			{"name":"List-Unsubscribe-Post","value":"List-Unsubscribe=One-Click"}],"body":{"size":4,"data":"Q0FOQVJZ"}}}`,
			canaryID, canaryEmail, canaryText, canaryLabel, canaryFile, canarySubject, canaryURL)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// argsFor fills every property of a tool's input schema with a canary
// of the right type, so the caller's own words are traceable through
// the logs as well as Google's.
func argsFor(tool *mcp.Tool) map[string]any {
	raw, _ := json.Marshal(tool.InputSchema)
	var schema struct {
		Properties map[string]struct {
			Type  any `json:"type"`
			Items *struct {
				Type any `json:"type"`
			} `json:"items"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(raw, &schema)
	args := map[string]any{}
	for name, p := range schema.Properties {
		// Fields that are validated before any request leaves get valid
		// values, so the call reaches Google and the paths where a payload
		// could be logged are the ones exercised.
		if v, ok := validArgs[name]; ok {
			args[name] = v
			continue
		}
		switch fmt.Sprint(p.Type) {
		case "boolean":
			args[name] = false
		case "integer", "number":
			args[name] = 1
		case "array":
			args[name] = []any{canaryFor(name)}
		case "object":
			args[name] = map[string]any{}
		default:
			args[name] = canaryFor(name)
		}
	}
	return args
}

var validArgs = map[string]any{
	"time_zone": "UTC", "after": "2026-01-01", "before": "2026-02-01",
	"budget_chars": 0, "cursor": 0, "offset": 0,
	// The canary message's one part has no part id, and carries the
	// canary file name, so download_attachment and download_attachments
	// write it.
	"part_id": "", "part_ids": []any{""}, "history_id": "1", "kinds": []any{"added"},
}

func canaryFor(name string) string {
	switch {
	case strings.Contains(name, "query") || name == "q":
		return canaryQuery
	case strings.Contains(name, "to") || strings.Contains(name, "cc") || strings.Contains(name, "from") ||
		strings.Contains(name, "recipient") || strings.Contains(name, "address") || strings.Contains(name, "email"):
		return canaryEmail
	case strings.Contains(name, "subject"):
		return canarySubject
	case strings.Contains(name, "label"):
		return canaryLabel
	case strings.Contains(name, "file") || strings.Contains(name, "name"):
		return canaryFile
	case name == "id" || strings.HasSuffix(name, "_id") || strings.HasSuffix(name, "_ids"):
		return canaryID
	}
	return canaryText
}

// TestLogsNeverCarryThePayload drives every registered tool at debug
// and reads back everything the server logged.
func TestLogsNeverCarryThePayload(t *testing.T) {
	var logs bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	gmail := canaryGmail(t)
	cfg := tools.FullSurface(config.Config{LocalDir: t.TempDir(), APIBase: gmail.URL})
	client := gapi.New(gapi.Options{
		BaseURL: gmail.URL, Logger: lg, MaxRetries: -1,
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: canaryToken}),
	})
	srv := server.New(server.Deps{Deps: tools.Deps{Config: cfg, Client: client, Logger: lg}, Version: "test"})
	h := testutil.ConnectServer(t, srv)

	surface := h.Tools(t)
	if len(surface) < minTools {
		t.Fatalf("drove %d tools, below the floor of %d", len(surface), minTools)
	}
	for _, tool := range surface {
		// A refusal is as interesting as a success: the error paths are
		// where a payload reaches a log, so no result is checked.
		if _, err := h.Client.CallTool(context.Background(), &mcp.CallToolParams{
			Name: tool.Name, Arguments: argsFor(tool),
		}); err != nil {
			t.Fatalf("call %s: %v", tool.Name, err)
		}
	}
	// A call to a tool that does not exist, named with a canary, must
	// not echo the name either.
	_, _ = h.Client.CallTool(context.Background(), &mcp.CallToolParams{Name: canaryText, Arguments: map[string]any{}})

	// The questions put to the person carry mail, and so do the calls
	// that answer them: every tool again, from a client that can be
	// asked and accepts, with its guards set so the call reaches the
	// question, on both ways a question goes out (§4.13).
	var asked atomic.Int64
	for _, protocol := range []string{"2025-11-25", "2026-07-28"} {
		ah, err := testutil.ConnectClient(context.Background(), srv, &mcp.ClientOptions{
			ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				asked.Add(1)
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
			},
		}, protocol)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(ah.Close)
		for _, tool := range surface {
			args := argsFor(tool)
			for _, guard := range []string{"confirm", "trash", "enable"} {
				if _, ok := args[guard]; ok {
					args[guard] = true
				}
			}
			// Every third request to the canary server fails, so a call is
			// made three times to meet it at each point of that cycle.
			for range 3 {
				_, _ = ah.Client.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.Name, Arguments: args})
			}
		}
	}
	if asked.Load() == 0 {
		t.Fatal("no call reached a question; this test would pass on a server that asks nothing")
	}

	out := logs.String()
	if strings.TrimSpace(out) == "" {
		t.Fatal("nothing was logged; this test would pass on a server that logs nothing")
	}
	for canary, what := range forbidden {
		if strings.Contains(out, canary) {
			t.Errorf("the logs carry %s:\n%s", what, linesWith(out, canary))
		}
	}
}

func linesWith(out, s string) string {
	var hits []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, s) {
			hits = append(hits, line)
		}
	}
	return strings.Join(hits, "\n")
}
