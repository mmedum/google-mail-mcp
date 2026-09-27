package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// liveCoverDump is a surface of eight tools; get_message has two options,
// the rest none, except search_messages with one.
func liveCoverDump(t *testing.T) *schemaDump {
	t.Helper()
	tools := []string{
		`{"name":"get_message","inputSchema":{"type":"object","properties":{"id":{},"format":{}}}}`,
		`{"name":"search_messages","inputSchema":{"type":"object","properties":{"q":{}}}}`,
	}
	for _, name := range []string{"get_profile", "list_labels", "search_threads", "get_thread", "list_drafts", "get_draft"} {
		tools = append(tools, fmt.Sprintf(`{"name":%q,"inputSchema":{"type":"object"}}`, name))
	}
	d, err := parseDump([]byte(`{"tools":[`+strings.Join(tools, ",")+`]}`), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const liveCoverDriverSource = `package main

type step struct {
	name, tool, pending string
	args func() map[string]any
}

var steps = []step{
	{tool: "get_profile"},
	{tool: "list_labels"},
	{tool: "search_threads"},
	{tool: "get_thread"},
	{tool: "list_drafts"},
	{tool: "get_draft", pending: "phase 0: step not yet written"},
	{tool: "get_message", args: func() map[string]any { return map[string]any{"id": "x"} }},
	{tool: "search_messages", args: func() map[string]any { return map[string]any{"q": "label:run"} }},
}
`

func runLiveCover(t *testing.T, waivers string, source string) (string, error) {
	t.Helper()
	root := writeTree(t, map[string]string{
		"driver/main.go": source,
		"waivers.tsv":    "# tool or tool.option\tverdict\treason\n" + waivers,
	})
	var out sink
	err := liveCoverCheck(&out, liveCoverDump(t), filepath.Join(root, "driver"), filepath.Join(root, "waivers.tsv"))
	return out.String(), err
}

const liveCoverCleanWaivers = "get_draft\tundriven\tphase 0: the step is not yet written\n" +
	"get_message.format\tundrivable\tneeds a message format the driver cannot insert\n"

func TestLiveCoverPassesWithEveryGapWaived(t *testing.T) {
	out, err := runLiveCover(t, liveCoverCleanWaivers, liveCoverDriverSource)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "live cover ok (2 of 3 options driven across 8 tools; 1 waived, 1 rows undriven") {
		t.Errorf("the gate does not say what it read: %s", out)
	}
}

func TestLiveCoverRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		waivers string
		source  string
		want    string
	}{
		{"an undriven option", "get_draft\tundriven\tphase 0: the step is not yet written\n", liveCoverDriverSource,
			"get_message.format is an option no step sends"},
		{"a pending step's tool", "get_message.format\tundrivable\tneeds a message format the driver cannot insert\n",
			liveCoverDriverSource, "get_draft is a tool no step in"},
		{"a waiver for an option now driven", liveCoverCleanWaivers + "get_message.id\tundriven\tnot yet, not yet, not yet\n",
			liveCoverDriverSource, "get_message.id is waived as undriven and a step sends it"},
		{"a waiver for a tool now called", liveCoverCleanWaivers + "get_profile\tundriven\tnot yet, not yet, not yet\n",
			liveCoverDriverSource, "get_profile is waived as undriven and a step calls it"},
		{"a waiver for a tool that is gone", liveCoverCleanWaivers + "send_message\tundrivable\tno such tool any more\n",
			liveCoverDriverSource, "send_message names a tool that does not exist"},
		{"a waiver for an option that is gone", liveCoverCleanWaivers + "get_message.raw\tundrivable\tno such option any more\n",
			liveCoverDriverSource, "get_message.raw names an option"},
		{"an unknown verdict", liveCoverCleanWaivers + "get_thread\tlater\tsomebody will get to it\n",
			liveCoverDriverSource, `verdict "later"`},
		{"no reason", "get_draft\tundriven\t\nget_message.format\tundrivable\tneeds a message format the driver cannot insert\n",
			liveCoverDriverSource, "no reason worth the name"},
		{"past the ceiling", liveCoverCleanWaivers +
			"search_messages.q\tundriven\tparked work number one\n" +
			"list_labels\tundriven\tparked work number two\n" +
			"list_drafts\tundriven\tparked work number three\n",
			strings.NewReplacer(`{tool: "list_labels"},`, "", `{tool: "list_drafts"},`, "",
				`return map[string]any{"q": "label:run"}`, `return nil`).Replace(liveCoverDriverSource),
			"4 rows are waived as undriven and the ceiling is 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runLiveCover(t, tc.waivers, tc.source)
			if err == nil {
				t.Fatalf("passed:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not say %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestLiveCoverFloorOnTools(t *testing.T) {
	d := liveCoverDump(t)
	d.Tools = d.Tools[:7]
	root := writeTree(t, map[string]string{"driver/main.go": liveCoverDriverSource, "waivers.tsv": ""})
	err := liveCoverCheck(&sink{}, d, filepath.Join(root, "driver"), filepath.Join(root, "waivers.tsv"))
	if err == nil || !strings.Contains(err.Error(), "want at least 8") {
		t.Errorf("a short surface passed: %v", err)
	}
}

// The real driver's source parses and names every phase 0 read tool.
// TestLiveCoverReadsTheRealDriver runs the gate over the real driver:
// every tool the phase 0 steps call must count as driven, which fails if
// the gate stops finding the step table.
func TestLiveCoverReadsTheRealDriver(t *testing.T) {
	root := filepath.Join("..", "..")
	out, _ := runLiveCoverOn(t, filepath.Join(root, liveCoverDriver))
	for _, tool := range []string{"get_profile", "search_threads", "get_draft"} {
		if strings.Contains(out, tool+" is a tool no step in") {
			t.Errorf("the real driver's step for %s was not found:\n%s", tool, out)
		}
	}
}

func runLiveCoverOn(t *testing.T, driver string) (string, error) {
	t.Helper()
	root := writeTree(t, map[string]string{"waivers.tsv": ""})
	var out sink
	err := liveCoverCheck(&out, liveCoverDump(t), driver, filepath.Join(root, "waivers.tsv"))
	return out.String(), err
}
