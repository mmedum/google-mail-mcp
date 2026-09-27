package testutil

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type in struct {
	Name string `json:"name"`
}

type out struct {
	Greeting string `json:"greeting"`
}

func TestHarness(t *testing.T) {
	h := Connect(t, func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "greet"}, func(_ context.Context, _ *mcp.CallToolRequest, i in) (*mcp.CallToolResult, out, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi " + i.Name}}}, out{Greeting: "hi " + i.Name}, nil
		})
	})
	if tools := h.Tools(t); len(tools) != 1 || tools[0].Name != "greet" {
		t.Fatalf("tools = %v", tools)
	}
	var got out
	res := h.CallInto(t, "greet", map[string]any{"name": "a"}, &got)
	if got.Greeting != "hi a" || Text(res) != "hi a" {
		t.Errorf("got %+v, text %q", got, Text(res))
	}
	if Text(&mcp.CallToolResult{}) != "" {
		t.Error("empty result has text")
	}
	h.Close()
	h.Close()
}
