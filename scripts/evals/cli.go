//go:build evals

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The model runs in `claude -p`, signed in as whoever runs the harness,
// with this server as its only tools.
//
// The built-in tools are turned off at the source, never listed away:
// --tools "" disables every one, and --allowed-tools names the MCP ones
// that remain. A denylist fails open — a sibling's first version listed
// six built-ins and the model used two it had not listed to read the
// maintainer's notes mid-task. --setting-sources "" drops the
// maintainer's settings, hooks and permission rules, --strict-mcp-config
// keeps their other MCP servers out, and the run starts in an empty
// directory, so no CLAUDE.md is read. A tool call that is not this
// server's fails the trial: the fence is checked, not assumed.

// serverName is the name the MCP config gives this server; the CLI
// prefixes its tools with mcp__<name>__.
const serverName = "gmail"

// claudeCommand is the program run; a test replaces it.
var claudeCommand = "claude"

// cliOptions are what the command line sets for every trial.
type cliOptions struct {
	model, effort string
	budget        float64
}

// claudeArgs is the command line, kept in one function so the fence
// can be read in one place.
func claudeArgs(prompt, cfgPath string, o cliOptions) []string {
	return []string{
		"-p", prompt,
		"--output-format", "stream-json", "--verbose",
		"--mcp-config", cfgPath,
		"--strict-mcp-config",
		"--allowed-tools", "mcp__" + serverName + "__*",
		"--tools", "",
		"--setting-sources", "",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--model", o.model,
		"--effort", o.effort,
		"--max-budget-usd", fmt.Sprintf("%.2f", o.budget),
	}
}

// Run is what one trial produced, read from the CLI's event stream.
type Run struct {
	Transcript
	// Subtype is the result event's: success, or why the run ended.
	Subtype string
	Turns   int
	CostUSD float64
	// Connected is set when the CLI reported this server connected.
	Connected bool
	// fence is the first tool call that was not this server's.
	fence error
	// pending maps a tool_use id to its call, for the result's error bit.
	pending map[string]int
	// Refused counts calls whose result was an error, and Refusals keeps
	// the first line of each, for -v: a refusal by the CLI's own
	// permission check never reached the server.
	Refused  int
	Refusals []string
}

// askClaude runs one trial against the world's server.
func askClaude(ctx context.Context, o cliOptions, prompt, url string) (Run, error) {
	dir, err := os.MkdirTemp("", "evals-*")
	if err != nil {
		return Run{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	cfg, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		serverName: map[string]any{"type": "http", "url": url}}})
	if err != nil {
		return Run{}, err
	}
	cfgPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return Run{}, err
	}
	cmd := exec.CommandContext(ctx, claudeCommand, claudeArgs(prompt, cfgPath, o)...) //nolint:gosec // a fixed program
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Run{}, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Run{}, fmt.Errorf("start %s: %w", claudeCommand, err)
	}
	run := Run{pending: map[string]int{}}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		run.read(sc.Bytes())
	}
	if err := cmd.Wait(); err != nil && run.Subtype == "" {
		return run, fmt.Errorf("%s: %w: %s", claudeCommand, err, clip(stderr.String(), 300))
	}
	if run.fence != nil {
		return run, run.fence
	}
	if !run.Connected {
		return run, errors.New("the CLI never reported this server connected")
	}
	return run, nil
}

// event is one line of the CLI's stream-json output, the fields read.
type event struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	Result     string  `json:"result"`
	NumTurns   int     `json:"num_turns"`
	TotalCost  float64 `json:"total_cost_usd"`
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	Message struct {
		Content []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     map[string]any  `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
}

// read folds one stream-json line into the run. A line that is not an
// event is the CLI's own chatter.
func (r *Run) read(line []byte) {
	var ev event
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "system":
		for _, s := range ev.MCPServers {
			if s.Name == serverName && s.Status == "connected" {
				r.Connected = true
			}
		}
	case "assistant":
		for _, c := range ev.Message.Content {
			if c.Type != "tool_use" {
				continue
			}
			name, ours := strings.CutPrefix(c.Name, "mcp__"+serverName+"__")
			if !ours {
				if r.fence == nil {
					r.fence = fmt.Errorf("the model called %s, which is not this server's: the fence did not hold", c.Name)
				}
				continue
			}
			r.pending[c.ID] = len(r.Calls)
			r.Calls = append(r.Calls, Call{Tool: name, Args: c.Input})
		}
	case "user":
		for _, c := range ev.Message.Content {
			if _, ok := r.pending[c.ToolUseID]; ok && c.Type == "tool_result" {
				delete(r.pending, c.ToolUseID)
				if c.IsError {
					r.Refused++
					r.Refusals = append(r.Refusals, clip(resultText(c.Content), 200))
				}
			}
		}
	case "result":
		r.Subtype, r.Final, r.Turns, r.CostUSD = ev.Subtype, strings.TrimSpace(ev.Result), ev.NumTurns, ev.TotalCost
	}
}

// resultText reads a tool result's content, which the CLI writes as a
// string or as an array of content blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, " ")
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
