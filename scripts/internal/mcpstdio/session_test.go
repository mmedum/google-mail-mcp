package mcpstdio

import (
	"bufio"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// The test binary doubles as a fake server: started with fakeServerEnv
// set, it answers the frames a driver sends and exits on EOF.
const fakeServerEnv = "MCPSTDIO_FAKE_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) == "1" {
		fakeServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeServer() {
	in := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	declared := false
	for in.Scan() {
		var req map[string]any
		if json.Unmarshal(in.Bytes(), &req) != nil {
			continue
		}
		id, hasID := req["id"]
		if !hasID {
			continue // a notification
		}
		var result any
		switch req["method"] {
		case "initialize":
			params, _ := req["params"].(map[string]any)
			caps, _ := params["capabilities"].(map[string]any)
			_, declared = caps["elicitation"]
			// A log notification first, which the client must skip.
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/message"})
			result = map[string]any{"protocolVersion": "2025-11-25"}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "get_profile", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "get_message", "inputSchema": map[string]any{"type": "object",
					"properties": map[string]any{"id": map[string]any{}, "format": map[string]any{}}}},
			}}
		case "tools/call":
			params, _ := req["params"].(map[string]any)
			if params["name"] == "ask" {
				result = ask(in, enc, declared)
				break
			}
			result = map[string]any{"isError": params["name"] == "get_message",
				"content": []any{map[string]any{"type": "text", "text": "called " + params["name"].(string)}}}
		default:
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": -32601, "message": "no such method"}})
			continue
		}
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
}

// ask puts a question to the client, as the SDK does inside a call on
// protocols before 2026-07-28, and says what came back.
func ask(in *bufio.Scanner, enc *json.Encoder, declared bool) any {
	text := "no elicitation declared"
	if declared {
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": "q1", "method": "elicitation/create",
			"params": map[string]any{"message": "Delete it?"}})
		in.Scan()
		var reply struct {
			ID     string `json:"id"`
			Result struct {
				Action  string         `json:"action"`
				Content map[string]any `json:"content"`
			} `json:"result"`
		}
		_ = json.Unmarshal(in.Bytes(), &reply)
		text = reply.ID + " " + reply.Result.Action
		if reply.Result.Content["confirm"] == true {
			text += " ticked"
		}
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

func startFake(t *testing.T) *Session {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(exe, fakeServerEnv+"=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestHandshakeListsOptionsAndCallsAreWatched(t *testing.T) {
	s := startFake(t)
	protocol, tools, err := s.Initialize("test")
	if err != nil {
		t.Fatal(err)
	}
	if protocol != "2025-11-25" {
		t.Errorf("protocol %q", protocol)
	}
	if !slices.Equal(tools, []string{"get_profile", "get_message"}) {
		t.Errorf("tools %v", tools)
	}
	if got := s.Options()["get_message"]; !slices.Equal(got, []string{"format", "id"}) {
		t.Errorf("options %v, want sorted [format id]", got)
	}

	var watched []string
	s.OnCall(func(tool string, _ map[string]any) { watched = append(watched, tool) })
	text, isError, err := s.CallTool("get_profile", nil)
	if err != nil || isError || text != "called get_profile" {
		t.Errorf("get_profile = %q, %v, %v", text, isError, err)
	}
	if _, isError, _ := s.CallTool("get_message", map[string]any{"id": "x"}); !isError {
		t.Error("a refusal was not reported as one")
	}
	if !slices.Equal(watched, []string{"get_profile", "get_message"}) {
		t.Errorf("the watcher saw %v", watched)
	}
}

func TestAnErrorObjectIsAnRPCError(t *testing.T) {
	s := startFake(t)
	if _, _, err := s.Initialize("test"); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.ReadResource("gmail://labels")
	var rpc *RPCError
	if err == nil || !strings.Contains(err.Error(), "no such method") {
		t.Fatalf("err = %v", err)
	}
	if !asRPC(err, &rpc) {
		t.Errorf("%T is not an *RPCError", err)
	}
}

func asRPC(err error, target **RPCError) bool {
	e, ok := err.(*RPCError) //nolint:errorlint // returned unwrapped by request
	if ok {
		*target = e
	}
	return ok
}

// A session that answers questions declares so, and answers the one the
// server puts in the middle of a call before the call's own reply.
func TestElicitationIsDeclaredAndAnswered(t *testing.T) {
	s := startFake(t)
	var asked []string
	s.OnElicit(func(message string) (string, bool) {
		asked = append(asked, message)
		return "accept", true
	})
	if _, _, err := s.Initialize("test"); err != nil {
		t.Fatal(err)
	}
	text, _, err := s.CallTool("ask", nil)
	if err != nil || text != "q1 accept ticked" || !slices.Equal(asked, []string{"Delete it?"}) {
		t.Errorf("ask = %q, %v; asked %v", text, err, asked)
	}

	quiet := startFake(t)
	if _, _, err := quiet.Initialize("test"); err != nil {
		t.Fatal(err)
	}
	if text, _, _ := quiet.CallTool("ask", nil); text != "no elicitation declared" {
		t.Errorf("without OnElicit: %q", text)
	}
}

func TestEncode(t *testing.T) {
	if got := Encode(map[string]any{"id": "x"}); got != `{"id":"x"}` {
		t.Errorf("Encode = %s", got)
	}
}
