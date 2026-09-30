package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A call that fails as a JSON-RPC error is reported by how far it got:
// before the answer, nothing was written; after it, the write may have
// happened, and that is never called nothing.
func TestAskFailuresByStage(t *testing.T) {
	for _, tc := range []struct {
		stage int32
		want  string
	}{
		{stageWaiting, "[blocked] send_draft was not confirmed by the person"},
		{stageWriting, "[ambiguous_outcome] the person confirmed send_draft, and the call ended before its result, so the write may have started"},
		{stageWritten, "[ambiguous_outcome] the person confirmed send_draft, and it was written (verdict: written)"},
	} {
		next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
			setStage(ctx, tc.stage)
			return nil, errors.New("reply lost")
		}
		res, err := AskFailures()(next)(context.Background(), "tools/call",
			&mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "send_draft"}})
		if err != nil {
			t.Fatalf("stage %d: %v", tc.stage, err)
		}
		r := res.(*mcp.CallToolResult)
		text := r.Content[0].(*mcp.TextContent).Text
		if !r.IsError || !strings.HasPrefix(text, tc.want) || strings.Contains(text, "reply lost") {
			t.Errorf("stage %d: %s", tc.stage, text)
		}
		if tc.stage != stageWaiting && strings.Contains(text, "Nothing was written") {
			t.Errorf("stage %d says nothing was written: %s", tc.stage, text)
		}
	}
	// A call that never asked keeps its own error.
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return nil, errors.New("plain") }
	if _, err := AskFailures()(next)(context.Background(), "tools/call", &mcp.CallToolRequest{}); err == nil {
		t.Error("an error of a call that asked nothing was replaced")
	}
}

// A round that carries an accept is past "nothing was written" before
// its handler reads anything, and is written only once its answer is
// spent on the question.
func TestAnAcceptedRoundIsWritingFromTheStart(t *testing.T) {
	a := newAsking(slog.New(slog.DiscardHandler))
	in := probeIn{ID: "1"}
	p := &person{in: in}
	state := a.sign(askState{Tool: "x", Args: p.args(), Nonce: "n", Expires: time.Now().Add(time.Minute).Unix()})
	var during int32
	h := wrap(func(ctx context.Context, _ probeIn) (probeOut, error) {
		during = stageOf(ctx)
		return probeOut{}, nil
	}, -1, "x", a, false)
	stage := &atomic.Int32{}
	ctx := context.WithValue(context.Background(), stageKey{}, stage)
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "x", RequestState: state,
		InputResponses: mcp.InputResponseMap{askKey: &mcp.ElicitResult{Action: "accept"}}}}
	if _, _, err := h(ctx, req, in); err != nil {
		t.Fatal(err)
	}
	if during != stageWriting || stage.Load() != stageWriting {
		t.Errorf("stage %d while handling and %d after a handler that never asked; want %d for both", during, stage.Load(), stageWriting)
	}
}
