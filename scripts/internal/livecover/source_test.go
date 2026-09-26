package livecover

import (
	"os"
	"path/filepath"
	"testing"
)

const driverSource = `package driver

type step struct {
	name    string
	tool    string
	pending string
	args    func(e *env) map[string]any
}

type env struct{ ids []string }

var steps = []step{
	{name: "profile", tool: "get_profile"},
	{name: "one message", tool: "get_message", args: func(e *env) map[string]any {
		args := map[string]any{"id": e.ids[0]}
		args["format"] = "full"
		return args
	}},
	{name: "not yet", tool: "get_draft", pending: "phase 0: step not yet written",
		args: func(*env) map[string]any { return map[string]any{"id": "x"} }},
	{name: "unknown tool", tool: "no_such_tool", args: func(*env) map[string]any {
		return map[string]any{"x": 1}
	}},
}

func helper(e *env) {
	query := map[string]any{"q": "label:run"}
	query["max_results"] = 5
	call(e, "search_messages", query)
	say("search_threads is mentioned but not called")
}

func call(*env, string, map[string]any) {}
func say(string)                        {}
`

func TestFromSourceReadsStepsAndHelpers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "driver.go"), []byte(driverSource), 0o600); err != nil {
		t.Fatal(err)
	}
	known := map[string][]string{
		"get_profile":     {},
		"get_message":     {"format", "id"},
		"get_draft":       {"id"},
		"search_messages": {"max_results", "q"},
		"search_threads":  {"q"},
	}
	sent, err := FromSource(dir, known)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sent["get_profile"]; !ok {
		t.Error("a step calling a tool with no options was not counted as calling it")
	}
	if !sent["get_message"]["id"] || !sent["get_message"]["format"] {
		t.Errorf("get_message options %v, want id and format (the second assigned later)", sent["get_message"])
	}
	if _, ok := sent["get_draft"]; ok {
		t.Error("a pending step was counted as driving its tool")
	}
	if _, ok := sent["no_such_tool"]; ok {
		t.Error("a tool the server does not publish was recorded")
	}
	if !sent["search_messages"]["q"] || !sent["search_messages"]["max_results"] {
		t.Errorf("search_messages options %v, want q and max_results", sent["search_messages"])
	}
	if _, ok := sent["search_threads"]; ok {
		t.Error("a tool named in a string with no arguments beside it was counted as called")
	}
}

func TestFromSourceRefusesAnEmptyDirectory(t *testing.T) {
	if _, err := FromSource(t.TempDir(), nil); err == nil {
		t.Error("a directory with no Go source was accepted")
	}
	if _, err := FromSource(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Error("a missing directory was accepted")
	}
}
