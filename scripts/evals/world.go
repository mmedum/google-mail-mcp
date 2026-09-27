//go:build evals

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/internal/server"
	"github.com/mmedum/google-mail-mcp/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/internal/tools"
)

// World is one task's mailbox and the server in front of it. Every task
// gets its own, so nothing one task wrote can satisfy another.
type World struct {
	Fake  *gmailtest.Server
	Facts Facts
	// Model is the session the model's calls go through.
	Model *mcp.ClientSession
	// Instructions are the server's, which a client puts before the
	// model as it would with any MCP server.
	Instructions string
	// drafts are the draft ids the mailbox started with.
	drafts []string
	close  func()
}

// newWorld builds the mailbox, reads its facts and connects the real
// server to it over an in-memory transport, so the model sees the
// descriptions, schemas and refusals that ship.
func newWorld(ctx context.Context, t Task) (*World, error) {
	fake := gmailtest.New()
	client := gapi.New(gapi.Options{
		BaseURL:     fake.URL(),
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "evals"}),
		Sleep:       func(context.Context, time.Duration) error { return nil },
	})
	cfg := config.Config{EnableSend: t.Send}
	srv := server.New(server.Deps{Deps: tools.Deps{Config: cfg, Client: client}, Version: "evals"})
	h, err := testutil.ConnectTo(ctx, srv)
	if err != nil {
		fake.Close()
		return nil, err
	}
	w := &World{Fake: fake, Model: h.Client, drafts: fake.DraftIDs(), close: func() { h.Close(); fake.Close() }}
	if init := h.Client.InitializeResult(); init != nil {
		w.Instructions = init.Instructions
	}
	if w.Facts, err = facts(fake, t); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

// Close shuts the session and the mailbox down.
func (w *World) Close() { w.close() }

// facts reads what the scorers compare with. A planted instruction that
// is not in the mailbox would make its task score nothing, so that is
// checked here too.
func facts(fake *gmailtest.Server, t Task) (Facts, error) {
	budget := fake.Scenario(gmailtest.ScenarioDraftReply)
	f := Facts{
		"budget_thread": budget.ThreadID,
		"budget_draft":  budget.DraftID,
		"newsletter":    fake.Scenario(gmailtest.ScenarioNewsletter).MessageIDs[0],
	}
	if inj := t.Injection; inj != nil {
		raw, _ := fake.Raw(fake.Scenario(inj.Scenario).MessageIDs[0])
		if !strings.Contains(string(raw), inj.Marker) {
			return nil, fmt.Errorf("%s: the planted instruction is not in the %s message", t.Name, inj.Scenario)
		}
	}
	for k, v := range f {
		if strings.TrimSpace(v) == "" {
			// An empty fact matches everything, so it would pass a scorer.
			return nil, fmt.Errorf("the fact %q is empty", k)
		}
	}
	return f, nil
}

// newDrafts are the drafts made during the run.
func (w *World) newDrafts() []string {
	return slices.DeleteFunc(w.Fake.DraftIDs(), func(id string) bool { return slices.Contains(w.drafts, id) })
}

// sends counts drafts.send calls the fake answered.
func (w *World) sends() int { return len(w.Fake.CallsOf("gmail.users.drafts.send")) }

// offered is the tool list as the model receives it.
func (w *World) offered(ctx context.Context) ([]toolDef, error) {
	var out []toolDef
	for t, err := range w.Model.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, err
		}
		out = append(out, toolDef{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if len(out) > 0 {
		out[len(out)-1].CacheControl = &cacheControl{Type: "ephemeral"}
	}
	return out, nil
}

// call runs one tool call and returns what a client shows the model:
// the text half. A refusal comes back with isError, as it would in a
// client, because recovering from one is part of what a task measures.
func (w *World) call(ctx context.Context, name string, input json.RawMessage) (map[string]any, string, bool) {
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, "the arguments are not a JSON object: " + err.Error(), true
		}
	}
	res, err := w.Model.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return args, err.Error(), true
	}
	return args, testutil.Text(res), res.IsError
}
