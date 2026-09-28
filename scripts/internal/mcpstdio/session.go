// Package mcpstdio is a small MCP client over a child process's stdio,
// for the programs under scripts/ that drive the built server: the live
// driver and the evals. It is a client, not a library — it speaks
// exactly the frames those two need and nothing else — but it is one
// client, because two hand-written JSON-RPC loops are two places for the
// handshake to drift.
package mcpstdio

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// CallTimeout bounds one tool call against a real account.
const CallTimeout = 2 * time.Minute

// Session is one stdio conversation with the server.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  *bufio.Scanner
	nextID int
	// onCall sees every tool invocation, and options is what the server
	// published at initialize. Together they are what lets a run say how
	// much of the surface it actually drove — measured where the calls
	// go out, so a call cannot be made without being counted.
	onCall  func(tool string, args map[string]any)
	options map[string][]string
	// onElicit answers the server's questions to the person; when set,
	// Initialize declares form elicitation.
	onElicit func(message string) (action string, confirm bool)

	mu     sync.Mutex
	stderr []string
}

// Start launches the server. env adds to the process environment, which
// is how the local directory reaches it: an MCP client passes command,
// args and env, and nothing else.
func Start(binary string, env ...string) (*Session, error) {
	cmd := exec.Command(binary)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}
	s := &Session{cmd: cmd, stdin: stdin, lines: bufio.NewScanner(stdout)}
	s.lines.Buffer(make([]byte, 0, 64*1024), 16<<20)
	go s.drainStderr(stderr)
	return s, nil
}

func (s *Session) drainStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		s.mu.Lock()
		s.stderr = append(s.stderr, strings.TrimRight(scanner.Text(), "\r"))
		s.mu.Unlock()
	}
}

// StderrTail returns the last n log lines the server wrote.
func (s *Session) StderrTail(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.stderr) <= n {
		return append([]string(nil), s.stderr...)
	}
	return append([]string(nil), s.stderr[len(s.stderr)-n:]...)
}

// Close shuts the conversation down and waits for the server to exit.
func (s *Session) Close() {
	_ = s.stdin.Close()
	_ = s.cmd.Wait()
}

// request sends one frame and reads until its reply arrives. Frames the
// server sends on its own account (logs, notifications) are skipped.
func (s *Session) request(method string, params any) (map[string]any, error) {
	s.nextID++
	id := s.nextID
	frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if _, err := s.stdin.Write(append(raw, '\n')); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	deadline := time.Now().Add(CallTimeout)
	for s.lines.Scan() {
		line := strings.TrimSpace(s.lines.Text())
		if line == "" {
			continue
		}
		var reply map[string]any
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			return nil, fmt.Errorf("stdout carried a line that is not JSON-RPC: %q", line)
		}
		if method, ok := reply["method"].(string); ok {
			// A request of the server's own, made while it serves ours:
			// an elicitation to answer, or one this client does not take.
			if rid, hasID := reply["id"]; hasID {
				if err := s.answer(rid, method, reply["params"]); err != nil {
					return nil, err
				}
			}
			continue
		}
		if got, ok := reply["id"].(float64); ok && int(got) == id {
			if e, ok := reply["error"]; ok {
				return nil, &RPCError{Method: method, Detail: message(e)}
			}
			return reply, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: timed out", method)
		}
	}
	if err := s.lines.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s: the server closed the connection; stderr:\n%s",
		method, strings.Join(s.StderrTail(20), "\n"))
}

// answer replies to one request the server sent: elicitation/create
// through onElicit, anything else with method-not-found.
func (s *Session) answer(id any, method string, params any) error {
	frame := map[string]any{"jsonrpc": "2.0", "id": id}
	if method == "elicitation/create" && s.onElicit != nil {
		p, _ := params.(map[string]any)
		message, _ := p["message"].(string)
		action, confirm := s.onElicit(message)
		result := map[string]any{"action": action}
		if action == "accept" {
			result["content"] = map[string]any{"confirm": confirm}
		}
		frame["result"] = result
	} else {
		frame["error"] = map[string]any{"code": -32601, "message": "this client does not take " + method}
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(raw, '\n'))
	return err
}

func (s *Session) notify(method string) error {
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(raw, '\n'))
	return err
}

// RPCError is the server answering with an error object rather than a
// result. It is worth telling apart from a transport failure: a tool
// call reports a refusal inside its result, but a resource read reports
// one as a JSON-RPC error, and a driver that could not tell the two
// apart would count every expected refusal as a broken connection.
type RPCError struct {
	Method string
	Detail string
}

func (e *RPCError) Error() string { return e.Method + ": " + e.Detail }

// message pulls the human half out of a JSON-RPC error object, falling
// back to the whole thing when it is not shaped as expected.
func message(e any) string {
	if m, ok := e.(map[string]any); ok {
		if text, ok := m["message"].(string); ok && text != "" {
			return text
		}
	}
	return fmt.Sprint(e)
}

// Initialize completes the handshake and lists the tool surface.
// name is what the server sees as the client.
func (s *Session) Initialize(name string) (protocol string, tools []string, err error) {
	capabilities := map[string]any{}
	if s.onElicit != nil {
		capabilities["elicitation"] = map[string]any{"form": map[string]any{}}
	}
	reply, err := s.request("initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    capabilities,
		"clientInfo":      map[string]any{"name": name, "version": "0"},
	})
	if err != nil {
		return "", nil, err
	}
	result, _ := reply["result"].(map[string]any)
	protocol, _ = result["protocolVersion"].(string)
	if err := s.notify("notifications/initialized"); err != nil {
		return "", nil, err
	}

	listed, err := s.request("tools/list", nil)
	if err != nil {
		return "", nil, err
	}
	result, _ = listed["result"].(map[string]any)
	raw, _ := result["tools"].([]any)
	s.options = map[string][]string{}
	for _, t := range raw {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, ok := m["name"].(string)
		if !ok {
			continue
		}
		tools = append(tools, name)
		schema, _ := m["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		names := make([]string, 0, len(props))
		for option := range props {
			names = append(names, option)
		}
		sort.Strings(names)
		s.options[name] = names
	}
	return protocol, tools, nil
}

// OnCall registers a watcher for every tool call this session makes.
// It is here rather than in the caller because here is the one place
// every call passes through: a count kept beside the call sites is a
// count that misses the next call site.
func (s *Session) OnCall(f func(tool string, args map[string]any)) { s.onCall = f }

// OnElicit makes the session a client that can ask the person: it
// declares form elicitation at Initialize, so it is set before, and f
// answers each question the server puts, with an action and, on accept,
// whether the box was ticked. The question's form is always one boolean
// named confirm (docs/architecture.md §4.13).
func (s *Session) OnElicit(f func(message string) (action string, confirm bool)) { s.onElicit = f }

// Options is the tool surface the server published at initialize: each
// registered tool and the option names its schema declares.
func (s *Session) Options() map[string][]string { return s.options }

// CallTool runs one tool and returns its text and whether it refused.
func (s *Session) CallTool(name string, args map[string]any) (text string, isError bool, err error) {
	text, _, isError, err = s.CallToolStructured(name, args)
	return text, isError, err
}

// CallToolStructured is CallTool that also returns the result's
// structuredContent, nil when it has none.
func (s *Session) CallToolStructured(name string, args map[string]any) (text string, structured map[string]any, isError bool, err error) {
	if s.onCall != nil {
		s.onCall(name, args)
	}
	reply, err := s.request("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", nil, false, err
	}
	result, _ := reply["result"].(map[string]any)
	structured, _ = result["structuredContent"].(map[string]any)
	isError, _ = result["isError"].(bool)
	var b strings.Builder
	content, _ := result["content"].([]any)
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String(), structured, isError, nil
}

// ReadResource reads one resource and returns its text and media type.
// A refusal comes back as an *RPCError, which the caller judges; any
// other error is the connection.
func (s *Session) ReadResource(uri string) (text, mime string, err error) {
	reply, err := s.request("resources/read", map[string]any{"uri": uri})
	if err != nil {
		return "", "", err
	}
	result, _ := reply["result"].(map[string]any)
	contents, _ := result["contents"].([]any)
	var b strings.Builder
	for _, c := range contents {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := m["text"].(string); ok {
			b.WriteString(t)
		}
		if mt, ok := m["mimeType"].(string); ok && mime == "" {
			mime = mt
		}
	}
	return b.String(), mime, nil
}

// Encode renders arguments for the transcript heading.
func Encode(args map[string]any) string {
	// Without HTML escaping: an rfc822:<id> argument otherwise prints as
	// \u003c…\u003e, which a live transcript showed is unreadable.
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(b.String(), "\n")
}
