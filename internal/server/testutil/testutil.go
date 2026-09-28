// Package testutil connects an in-memory MCP client to a server, so a
// test drives a tool through the SDK the way a client does: schema
// validation, handler, and the reply's two halves, with no binary and
// no stdio.
package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Harness is a connected client and server session.
type Harness struct {
	Client  *mcp.ClientSession
	closeFn func()
}

// Close shuts both sessions down. Safe to call more than once.
func (h *Harness) Close() {
	if h.closeFn != nil {
		h.closeFn()
		h.closeFn = nil
	}
}

// Connect builds a bare server, applies register to it, connects a
// client and closes both when the test ends.
func Connect(t *testing.T, register func(*mcp.Server)) *Harness {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "google-mail-mcp-test", Version: "test"}, nil)
	register(server)
	return ConnectServer(t, server)
}

// ConnectServer connects a client to a server the caller built — the
// real one from internal/server, say — and closes both when the test
// ends.
func ConnectServer(t *testing.T, server *mcp.Server) *Harness {
	t.Helper()
	h, err := ConnectTo(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// ConnectTo is ConnectServer for a caller with no *testing.T.
func ConnectTo(ctx context.Context, server *mcp.Server) (*Harness, error) {
	return ConnectClient(ctx, server, nil, "")
}

// ConnectClient is ConnectTo with the client's options — an elicitation
// handler, say — and the protocol version it asks for, the SDK's newest
// when empty.
func ConnectClient(ctx context.Context, server *mcp.Server, opts *mcp.ClientOptions, protocol string) (*Harness, error) {
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		return nil, fmt.Errorf("server connect: %w", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, opts)
	cs, err := client.Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		_ = ss.Close()
		return nil, fmt.Errorf("client connect: %w", err)
	}
	return &Harness{Client: cs, closeFn: func() {
		_ = cs.Close()
		_ = ss.Close()
	}}, nil
}

// Call calls one tool. A transport error fails the test; a tool error
// is a result, returned for the caller to assert on.
func (h *Harness) Call(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := h.Client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	return res
}

// CallInto is Call plus decoding structuredContent into into, when the
// result is not an error.
func (h *Harness) CallInto(t *testing.T, name string, args map[string]any, into any) *mcp.CallToolResult {
	t.Helper()
	res := h.Call(t, name, args)
	if into != nil && !res.IsError && res.StructuredContent != nil {
		DecodeStructured(t, res.StructuredContent, into)
	}
	return res
}

// Tools lists every registered tool.
func (h *Harness) Tools(t *testing.T) []*mcp.Tool {
	t.Helper()
	var out []*mcp.Tool
	for tool, err := range h.Client.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		out = append(out, tool)
	}
	return out
}

// DecodeStructured re-decodes a result's structuredContent, which the
// client holds as a map, into a typed value.
func DecodeStructured(t *testing.T, v, into any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("decode structuredContent into %T: %v (raw: %s)", into, err, b)
	}
}

// Text returns a result's first text block: the rendering, or for an
// error the "[class] message".
func Text(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
