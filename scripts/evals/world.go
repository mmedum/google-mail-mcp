//go:build evals

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/v2/internal/config"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/server"
	"github.com/mmedum/google-mail-mcp/v2/internal/tools"
)

// World is one task's mailbox and the server in front of it. Every task
// gets its own, so nothing one task wrote can satisfy another.
type World struct {
	Fake  *gmailtest.Server
	Facts Facts
	// URL is where the server answers MCP over streamable HTTP, on the
	// loopback interface, for the claude CLI to connect to.
	URL string
	// drafts are the draft ids the mailbox started with.
	drafts []string
	close  func()
}

// newWorld builds the mailbox, reads its facts and serves the real
// server in front of it. The model sees the descriptions, schemas,
// instructions and refusals that ship; only the transport differs from
// the binary's stdio, so no credential is needed and the mailbox stays
// in this process, where the end-state scorers read it.
func newWorld(t Task) (*World, error) {
	fake := gmailtest.New()
	f, err := facts(fake, t)
	if err != nil {
		fake.Close()
		return nil, err
	}
	client := gapi.New(gapi.Options{
		BaseURL:     fake.URL(),
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "evals"}),
		Sleep:       func(context.Context, time.Duration) error { return nil },
	})
	fake.SettingsScope = t.Settings
	cfg := config.Config{EnableSend: t.Send, EnableSettings: t.Settings}
	srv := server.New(server.Deps{Deps: tools.Deps{Config: cfg, Client: client}, Version: "evals"})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	return &World{Fake: fake, Facts: f, URL: ts.URL, drafts: fake.DraftIDs(),
		close: func() { ts.Close(); fake.Close() }}, nil
}

// Close shuts the server and the mailbox down.
func (w *World) Close() { w.close() }

// facts reads what the scorers compare with. A planted instruction that
// is not in the mailbox would make its task score nothing, so that is
// checked here too.
func facts(fake *gmailtest.Server, t Task) (Facts, error) {
	budget := fake.Scenario(gmailtest.ScenarioDraftReply)
	f := Facts{
		"budget_thread": budget.ThreadID,
		"budget_draft":  budget.DraftID,
		// The scenario records the draft's message last.
		"budget_draft_message": budget.MessageIDs[len(budget.MessageIDs)-1],
		"newsletter":           fake.Scenario(gmailtest.ScenarioNewsletter).MessageIDs[0],
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

// offered connects to the world as the CLI does, over HTTP, and returns
// the tool names and the instructions a model would be given.
func (w *World) offered(ctx context.Context) ([]string, string, error) {
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "evals", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: w.URL}, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = cs.Close() }()
	var names []string
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, "", err
		}
		names = append(names, t.Name)
	}
	instructions := ""
	if init := cs.InitializeResult(); init != nil {
		instructions = init.Instructions
	}
	return names, instructions, nil
}
