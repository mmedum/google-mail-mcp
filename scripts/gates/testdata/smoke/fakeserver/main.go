// Command fakeserver is a stdio MCP server the smoke gate's tests drive.
// SMOKE_FAKE_MODE makes it misbehave in one named way at a time.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// The fake's surface: one tool of each kind internal/tools registers,
// annotated the way its Kind annotates it.
var (
	readTool = map[string]any{"name": "get_profile", "inputSchema": map[string]any{"type": "object"},
		"annotations": map[string]any{"readOnlyHint": true, "idempotentHint": true, "openWorldHint": false}}
	writeTool = map[string]any{"name": "create_draft", "inputSchema": map[string]any{"type": "object"},
		"annotations": map[string]any{"destructiveHint": false, "openWorldHint": false}}
	sendTool = map[string]any{"name": "send_draft", "inputSchema": map[string]any{"type": "object"},
		"annotations": map[string]any{"destructiveHint": false, "openWorldHint": true},
		"_meta":       map[string]any{"anthropic/requiresUserInteraction": true}}
	destructiveTool = map[string]any{"name": "delete_message", "inputSchema": map[string]any{"type": "object"},
		"annotations": map[string]any{"destructiveHint": true, "idempotentHint": true, "openWorldHint": false},
		"_meta":       map[string]any{"anthropic/requiresUserInteraction": true}}
)

// registered is what the fake lists under the environment's settings,
// with mode's one misbehavior applied.
func registered(mode string) []any {
	readOnly := os.Getenv("GMAIL_READ_ONLY") == "true"
	send := os.Getenv("GMAIL_ENABLE_SEND") == "true"
	destructive := os.Getenv("GMAIL_ENABLE_DESTRUCTIVE") == "true"
	tools := []any{readTool}
	if !readOnly || mode == "leaky-readonly" {
		tools = append(tools, writeTool)
	}
	if (send && !readOnly) || mode == "leaky-send" {
		if mode == "no-meta" {
			tools = append(tools, map[string]any{"name": "send_draft", "inputSchema": sendTool["inputSchema"],
				"annotations": sendTool["annotations"]})
		} else {
			tools = append(tools, sendTool)
		}
	}
	if destructive && !readOnly && mode != "missing-destructive" {
		tools = append(tools, destructiveTool)
	}
	if mode == "undumped" {
		tools = append(tools, map[string]any{"name": "secret_tool",
			"annotations": map[string]any{"readOnlyHint": true}})
	}
	return tools
}

func main() {
	mode := os.Getenv("SMOKE_FAKE_MODE")
	if len(os.Args) > 1 && os.Args[1] == "--dump-schemas" {
		out(map[string]any{"server": "fake", "sdk_version": "v0",
			"tools":     []any{readTool, writeTool, sendTool, destructiveTool},
			"resources": []any{}, "resource_templates": []any{}})
		return
	}
	if mode == "crash" {
		os.Exit(3)
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if mode == "downgrade" {
				p.ProtocolVersion = "2024-11-05"
			}
			result = map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{},
				"serverInfo": map[string]any{"name": "fake", "version": "0"}}
		case "tools/list":
			result = map[string]any{"tools": registered(mode)}
		case "resources/list":
			// ttlMs beside the list, as the real SDK sends it.
			result = map[string]any{"resources": []any{}, "ttlMs": 0}
		case "resources/templates/list":
			result = map[string]any{"resourceTemplates": []any{}}
		case "server/discover":
			if mode == "legacy" {
				out(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "error": map[string]any{"code": -32601, "message": "no"}})
				continue
			}
			result = map[string]any{"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18"}}
		case "tools/call":
			if mode == "stray" {
				fmt.Println("debug: calling a tool")
			}
			text := "[auth] not signed in; run google-mail-mcp login"
			if mode == "noclass" {
				text = "not signed in"
			}
			if mode == "wrongcode" {
				out(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "error": map[string]any{"code": -32000, "message": "closed"}})
				continue
			}
			result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": text}}}
		default:
			out(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "error": map[string]any{"code": -32601, "message": "no"}})
			continue
		}
		out(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}
	if mode == "exit1" {
		os.Exit(1)
	}
}

func out(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}
